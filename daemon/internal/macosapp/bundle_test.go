package macosapp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"notty/daemon/internal/macosuser"
)

func TestResolveAndConfigureHelperPath(t *testing.T) {
	bundle := makeTestBundle(t)
	other := t.TempDir()
	hostileHome := t.TempDir()
	canonicalHome, err := macosuser.HomeDir()
	if err != nil {
		t.Skipf("operating-system account lookup is unavailable: %v", err)
	}
	for _, directory := range []string{filepath.Join(hostileHome, ".local", "bin"), filepath.Join(hostileHome, ".npm-global", "bin"), filepath.Join(hostileHome, ".notty", "bin")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", hostileHome)
	t.Setenv("PATH", strings.Join([]string{other, bundle.Helpers, "", other}, string(os.PathListSeparator)))

	resolved, err := Resolve(bundle.Executable)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved != bundle {
		t.Fatalf("Resolve() = %#v, want %#v", resolved, bundle)
	}
	if err := resolved.ConfigureHelperPath(); err != nil {
		t.Fatalf("ConfigureHelperPath() error = %v", err)
	}
	entries := filepath.SplitList(os.Getenv("PATH"))
	wantPrefix := []string{bundle.Helpers}
	for _, candidate := range []string{filepath.Join(canonicalHome, ".local", "bin"), filepath.Join(canonicalHome, ".npm-global", "bin")} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			wantPrefix = append(wantPrefix, candidate)
		}
	}
	if len(entries) < len(wantPrefix) {
		t.Fatalf("PATH entries = %q, want prefix %q", entries, wantPrefix)
	}
	for index, want := range wantPrefix {
		if entries[index] != want {
			t.Fatalf("PATH entry %d = %q, want %q (all %q)", index, entries[index], want, entries)
		}
	}
	for _, forbidden := range []string{other, filepath.Join(hostileHome, ".local", "bin"), filepath.Join(hostileHome, ".npm-global", "bin"), filepath.Join(hostileHome, ".notty", "bin")} {
		for _, entry := range entries {
			if entry == forbidden {
				t.Fatalf("PATH retained forbidden ambient or legacy entry %q: %q", forbidden, entries)
			}
		}
	}
}

func TestResolveRejectsWrongLayoutAndUnsafeEntries(t *testing.T) {
	t.Run("wrong executable name", func(t *testing.T) {
		bundle := makeTestBundle(t)
		wrong := filepath.Join(bundle.MacOS, "Other")
		writeExecutable(t, wrong)
		if _, err := Resolve(wrong); err == nil {
			t.Fatal("Resolve() unexpectedly accepted a wrong executable name")
		}
	})

	t.Run("missing helper", func(t *testing.T) {
		bundle := makeTestBundle(t)
		if err := os.Remove(bundle.AgentTool); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(bundle.Executable); err == nil {
			t.Fatal("Resolve() unexpectedly accepted a missing helper")
		}
	})

	t.Run("helper symlink", func(t *testing.T) {
		bundle := makeTestBundle(t)
		realHelper := filepath.Join(t.TempDir(), AgentToolName)
		writeExecutable(t, realHelper)
		if err := os.Remove(bundle.AgentTool); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realHelper, bundle.AgentTool); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(bundle.Executable); err == nil {
			t.Fatal("Resolve() unexpectedly accepted a symlinked helper")
		}
	})

	t.Run("non executable helper", func(t *testing.T) {
		bundle := makeTestBundle(t)
		if err := os.Chmod(bundle.AgentTool, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(bundle.Executable); err == nil {
			t.Fatal("Resolve() unexpectedly accepted a non-executable helper")
		}
	})

	t.Run("relative executable", func(t *testing.T) {
		if _, err := Resolve(filepath.Join(AppName, "Contents", "MacOS", ExecutableName)); err == nil {
			t.Fatal("Resolve() unexpectedly accepted a relative executable")
		}
	})
}

func TestBoundedToolPathRejectsUnsafeDirectory(t *testing.T) {
	separator := string(os.PathListSeparator)
	for _, directory := range []string{"relative", t.TempDir() + string(os.PathSeparator) + "..", t.TempDir() + separator + "extra"} {
		if _, err := boundedToolPath(directory, t.TempDir()); err == nil {
			t.Fatalf("boundedToolPath(%q) unexpectedly succeeded", directory)
		}
	}
}

func makeTestBundle(t *testing.T) Bundle {
	t.Helper()
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tempRoot, AppName)
	bundle := Bundle{
		Root:       root,
		Contents:   filepath.Join(root, "Contents"),
		MacOS:      filepath.Join(root, "Contents", "MacOS"),
		Helpers:    filepath.Join(root, "Contents", "Helpers"),
		Resources:  filepath.Join(root, "Contents", "Resources"),
		Executable: filepath.Join(root, "Contents", "MacOS", ExecutableName),
		AgentTool:  filepath.Join(root, "Contents", "Helpers", AgentToolName),
		InfoPlist:  filepath.Join(root, "Contents", "Info.plist"),
	}
	for _, directory := range []string{bundle.MacOS, bundle.Helpers, bundle.Resources} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, bundle.Executable)
	writeExecutable(t, bundle.AgentTool)
	if err := os.WriteFile(bundle.InfoPlist, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	content := []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		content = []byte("test executable")
	}
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
}
