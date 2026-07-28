package syncer

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestManagedBackgroundCommandsPreserveArgumentsStreamsExitAndContext(t *testing.T) {
	executable := fakeProcessCommand(t, fakeProcessManagedChildIO)
	arguments := []string{"one", "two words", ""}
	factories := map[string]func(string, ...string) *exec.Cmd{
		"runtime": managedBackgroundCommand,
		"context": func(name string, args ...string) *exec.Cmd {
			return managedBackgroundCommandContext(context.Background(), name, args...)
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			command := factory(executable, arguments...)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
				t.Fatalf("managed child error = %v, want exit code 23", err)
			}
			if got, want := stdout.String(), "stdout:one|two words|"; got != want {
				t.Fatalf("managed child stdout = %q, want %q", got, want)
			}
			if got, want := stderr.String(), "stderr:one|two words|"; got != want {
				t.Fatalf("managed child stderr = %q, want %q", got, want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := managedBackgroundCommandContext(ctx, executable, "canceled").Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("managed context command error = %v, want context cancellation", err)
	}
}

func TestManagedBackgroundProcessConstructorInventory(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read syncer package: %v", err)
	}

	directExecCalls := map[string]map[string]int{}
	managedCalls := map[string]map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		execAliases := map[string]bool{}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			if path != "os/exec" {
				continue
			}
			alias := "exec"
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "." {
				t.Fatalf("%s uses a dot import that would bypass the os/exec constructor inventory", name)
			}
			execAliases[alias] = true
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch function := call.Fun.(type) {
			case *ast.Ident:
				if function.Name == "managedBackgroundCommand" || function.Name == "managedBackgroundCommandContext" {
					incrementCallInventory(managedCalls, name, function.Name)
				}
			case *ast.SelectorExpr:
				identifier, ok := function.X.(*ast.Ident)
				if ok && execAliases[identifier.Name] && (function.Sel.Name == "Command" || function.Sel.Name == "CommandContext") {
					incrementCallInventory(directExecCalls, name, function.Sel.Name)
				}
			}
			return true
		})
	}

	wantDirect := map[string]map[string]int{
		"managed_process.go": {"Command": 1, "CommandContext": 1},
	}
	if !reflect.DeepEqual(directExecCalls, wantDirect) {
		t.Fatalf("production os/exec constructors must stay inside the managed process policy: got %#v, want %#v", directExecCalls, wantDirect)
	}
	wantManaged := map[string]map[string]int{
		"appserver.go":     {"managedBackgroundCommand": 1},
		"claude_driver.go": {"managedBackgroundCommand": 1, "managedBackgroundCommandContext": 2},
		"codex_driver.go":  {"managedBackgroundCommandContext": 2},
	}
	if !reflect.DeepEqual(managedCalls, wantManaged) {
		t.Fatalf("managed child inventory changed without an explicit policy review: got %#v, want %#v", managedCalls, wantManaged)
	}
}

func TestWindowsManagedBackgroundProcessPolicySource(t *testing.T) {
	source, err := os.ReadFile("managed_process_windows.go")
	if err != nil {
		t.Fatalf("read Windows managed-process policy: %v", err)
	}
	text := string(source)
	for _, required := range []string{
		"command.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW",
		"command.SysProcAttr.HideWindow = true",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Windows managed-process policy is missing %q", required)
		}
	}
	if strings.Contains(text, "NoInheritHandles") {
		t.Fatal("Windows managed-process policy disables the standard handles used for stdin/stdout/stderr capture")
	}
}

func incrementCallInventory(inventory map[string]map[string]int, filename, function string) {
	if inventory[filename] == nil {
		inventory[filename] = map[string]int{}
	}
	inventory[filename][function]++
}
