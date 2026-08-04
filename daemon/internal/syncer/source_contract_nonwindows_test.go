//go:build !windows

package syncer

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	sourceContractFilename          = "source_contract_nonwindows_test.go"
	sourceContractExpectedTestCount = 11
)

func TestSourceContract_InventoryIsNonWindowsOnlyAndComplete(t *testing.T) {
	source := sourceContractReadTestFile(t, sourceContractFilename)
	if !sourceContractHasWindowsExclusion(source) {
		t.Fatalf("%s must remain compile-time excluded from Windows", sourceContractFilename)
	}
	if sourceContractHasWindowsExclusion(bytes.TrimPrefix(source, []byte("//go:build !windows\n\n"))) {
		t.Fatal("removed !windows build constraint mutation passed")
	}

	file, err := parser.ParseFile(token.NewFileSet(), sourceContractFilename, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", sourceContractFilename, err)
	}
	var contractTests []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") {
			continue
		}
		if !strings.HasPrefix(function.Name.Name, "TestSourceContract_") {
			t.Fatalf("source contract %s must use the TestSourceContract_ prefix", function.Name.Name)
		}
		contractTests = append(contractTests, function.Name.Name)
	}
	sort.Strings(contractTests)
	if len(contractTests) != sourceContractExpectedTestCount {
		t.Fatalf("source contract inventory = %d tests (%v), want %d", len(contractTests), contractTests, sourceContractExpectedTestCount)
	}

	// Keep source access out of every Windows-eligible test file. A newly added
	// source fitness function must move into this !windows file rather than
	// silently running against an empty source tree in the native binary.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read syncer package: %v", err)
	}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == sourceContractFilename || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		violations = append(violations, sourceContractBoundaryViolations(name, parsed)...)
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("Windows-eligible tests contain package-source contracts; move them to %s: %v", sourceContractFilename, violations)
	}

	// This also rejects a source-less Linux false green before the negative
	// contracts below get a chance to scan an empty directory.
	productionFiles := sourceContractProductionFiles(t)
	if !sourceContractContainsString(productionFiles, "service.go") {
		t.Fatalf("syncer production source inventory is missing service.go: %v", productionFiles)
	}

	for name, mutation := range map[string]string{
		"prefixed test moved into Windows inventory": "package syncer\nfunc TestSourceContract_Leak() {}\n",
		"production source literal outside boundary": "package syncer\nvar source = \"service.go\"\n",
		"Go parser import outside boundary":          "package syncer\nimport _ \"go/parser\"\n",
	} {
		mutated, err := parser.ParseFile(token.NewFileSet(), "mutated_test.go", mutation, 0)
		if err != nil {
			t.Fatalf("parse %s mutation: %v", name, err)
		}
		if got := sourceContractBoundaryViolations("mutated_test.go", mutated); len(got) == 0 {
			t.Fatalf("%s mutation passed", name)
		}
	}
}

func TestSourceContract_AgentSessionSupervisorDoesNotUseCodexWireMethods(t *testing.T) {
	text := string(sourceContractReadProductionFile(t, "agent_sessions.go"))
	for _, forbidden := range []string{
		"CodexThreadID",
		"codexThreadId",
		"thread/start",
		"thread/resume",
		"turn/start",
		"turn/steer",
		"turn/interrupt",
		"turn/started",
		"codexAppServer",
		"appServerEvent",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("agent session supervisor should not contain Codex wire term %q", forbidden)
		}
	}
}

func TestSourceContract_ManagedBackgroundProcessConstructorInventory(t *testing.T) {
	directExecCalls := map[string]map[string]int{}
	managedCalls := map[string]map[string]int{}
	for _, name := range sourceContractProductionFiles(t) {
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
					incrementSourceContractCallInventory(managedCalls, name, function.Name)
				}
			case *ast.SelectorExpr:
				identifier, ok := function.X.(*ast.Ident)
				if ok && execAliases[identifier.Name] && (function.Sel.Name == "Command" || function.Sel.Name == "CommandContext") {
					incrementSourceContractCallInventory(directExecCalls, name, function.Sel.Name)
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

func TestSourceContract_WindowsManagedBackgroundProcessPolicy(t *testing.T) {
	text := string(sourceContractReadProductionFile(t, "managed_process_windows.go"))
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

func TestSourceContract_RootProjectionPlannerStaysPureStructural(t *testing.T) {
	source := string(sourceContractReadProductionFile(t, "root_namespace.go"))
	start := strings.Index(source, "func (RootProjectionPlanner) Plan")
	end := strings.Index(source, "func buildRootProjectionPlan")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate planner body")
	}
	planner := source[start:end]
	for _, forbidden := range []string{"WorkspaceFS", ".Exec(", ".Query(", ".QueryRow(", "os.", "http."} {
		if strings.Contains(planner, forbidden) {
			t.Fatalf("root projection planner must stay pure; found %q in:\n%s", forbidden, planner)
		}
	}
	if strings.Contains(source, "RootTree") || strings.Contains(source, "DecodeRootTree") {
		t.Fatal("obsolete RootTree naming should not be reintroduced")
	}
}

func TestSourceContract_ServiceHasNoBroadAgentWakeFallback(t *testing.T) {
	source := string(sourceContractReadProductionFile(t, "service.go"))
	for _, forbidden := range []string{"wake" + "AllAgentWorkers", "should" + "WakeAgentWorkersForEvent"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("service.go still contains broad agent wake fallback %q", forbidden)
		}
	}
}

func TestSourceContract_PublishSnapshotStoresBeforeEpochIncrement(t *testing.T) {
	// Reversal admits an uncanceled REST ahead of a pending snapshot:
	// coordinator captures the new epoch, swaps nil, passes the admission
	// fence, and starts REST while the snapshot is still being stored.
	source := sourceContractReadProductionFile(t, "service.go")
	fnSig := []byte("func (s *Service) publishSnapshot(")
	fnStart := bytes.Index(source, fnSig)
	if fnStart < 0 {
		t.Fatal("publishSnapshot not found in service.go")
	}
	body := source[fnStart:]
	braceStart := bytes.IndexByte(body, '{')
	depth := 1
	pos := braceStart + 1
	for pos < len(body) && depth > 0 {
		switch body[pos] {
		case '{':
			depth++
		case '}':
			depth--
		}
		pos++
	}
	fnBody := body[:pos]
	storeIdx := bytes.Index(fnBody, []byte("pendingSnapshot.Store"))
	addIdx := bytes.Index(fnBody, []byte("snapshotEpoch.Add"))
	if storeIdx < 0 || addIdx < 0 {
		t.Fatal("pendingSnapshot.Store and snapshotEpoch.Add not found in publishSnapshot")
	}
	if addIdx < storeIdx {
		t.Fatal("snapshotEpoch.Add must appear after pendingSnapshot.Store in publishSnapshot")
	}
}

func TestSourceContract_ProductionReconcileTrackedDocumentOnlyRuntimeLoop(t *testing.T) {
	matches := map[string]int{}
	for _, path := range sourceContractProductionFiles(t) {
		count := strings.Count(string(sourceContractReadProductionFile(t, path)), "reconcileTrackedDocument(")
		if count > 0 {
			matches[path] = count
		}
	}
	if matches["service.go"] != 2 || len(matches) != 1 {
		t.Fatalf("reconcileTrackedDocument must stay owned by the runtime loop; production matches: %#v", matches)
	}
}

func TestSourceContract_ProductionDocumentSyncUsesWorkspaceMuxSocketOnly(t *testing.T) {
	forbidden := []string{
		"/ws/documents/",
		"managedDocumentSync",
		"documentSyncs",
	}
	matches := sourceContractProductionTokenMatches(t, forbidden)
	if len(matches) != 0 {
		t.Fatalf("daemon document sync must use the workspace mux socket only; production matches: %#v", matches)
	}
}

func TestSourceContract_ProductionDocumentNamespaceUsesRootProjectionOnly(t *testing.T) {
	forbidden := []string{
		"workspace.Documents",
		"workspaceDocuments",
		"ensureRootEntriesForVisibleDocuments",
		"desiredDocumentPaths",
	}
	matches := sourceContractProductionTokenMatches(t, forbidden)
	if len(matches) != 0 {
		t.Fatalf("daemon document namespace must come from root projection only; production matches: %#v", matches)
	}
}

func TestSourceContract_ProductionProjectionDoesNotReplayHistoryForProjectionSeq(t *testing.T) {
	forbidden := "find" + "Projected" + "Seq"
	matches := map[string]int{}
	for _, path := range sourceContractProductionFiles(t) {
		if count := strings.Count(string(sourceContractReadProductionFile(t, path)), forbidden); count > 0 {
			matches[path] = count
		}
	}
	if len(matches) != 0 {
		t.Fatalf("projection must carry explicit seq instead of replaying history; production matches: %#v", matches)
	}
}

func sourceContractProductionFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read syncer package: %v", err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		t.Fatal("syncer source contract found no production Go files")
	}
	sort.Strings(files)
	return files
}

func sourceContractHasWindowsExclusion(source []byte) bool {
	return bytes.HasPrefix(source, []byte("//go:build !windows\n\n"))
}

func sourceContractBoundaryViolations(filename string, file *ast.File) []string {
	var violations []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "TestSourceContract_") {
			violations = append(violations, filename+": source-contract test is Windows-eligible: "+function.Name.Name)
		}
	}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			violations = append(violations, filename+": malformed import "+spec.Path.Value)
			continue
		}
		if path == "go/ast" || path == "go/parser" || path == "go/token" {
			violations = append(violations, filename+": imports "+path)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		if strings.HasSuffix(value, ".go") || strings.HasSuffix(value, "_test.go") {
			violations = append(violations, filename+": references Go source "+value)
		}
		return true
	})
	return violations
}

func sourceContractReadProductionFile(t *testing.T, name string) []byte {
	t.Helper()
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		t.Fatalf("invalid production source filename %q", name)
	}
	return sourceContractReadTestFile(t, name)
}

func sourceContractReadTestFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(data) == 0 {
		t.Fatalf("source file %s is empty", name)
	}
	return data
}

func sourceContractProductionTokenMatches(t *testing.T, tokens []string) map[string][]string {
	t.Helper()
	matches := map[string][]string{}
	for _, path := range sourceContractProductionFiles(t) {
		text := string(sourceContractReadProductionFile(t, path))
		for _, token := range tokens {
			if strings.Contains(text, token) {
				matches[path] = append(matches[path], token)
			}
		}
	}
	return matches
}

func incrementSourceContractCallInventory(inventory map[string]map[string]int, filename, function string) {
	if inventory[filename] == nil {
		inventory[filename] = map[string]int{}
	}
	inventory[filename][function]++
}

func sourceContractContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
