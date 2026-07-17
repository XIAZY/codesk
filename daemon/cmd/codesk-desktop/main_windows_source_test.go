package main

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsCompositionUsesOneAccountRootForStateAndSingleton(t *testing.T) {
	source, err := os.ReadFile("main_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`dirs, err := desktop.DefaultDirs()`,
		`desktop.NewWindowsInstanceLock(filepath.Join(dirs.Data, "Locks", "desktop.lock"))`,
		`desktopstate.NewFileConfigurationStore(dirs.Data)`,
		`desktopstate.NewWindowsSecretStore(dirs.Data)`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Windows composition root is missing account-root consumer %q", required)
		}
	}
	if count := strings.Count(text, "desktop.DefaultDirs()"); count != 1 {
		t.Fatalf("Windows composition resolves desktop directories %d times, want exactly once", count)
	}
	for _, forbidden := range []string{"LOCALAPPDATA", "APPDATA", "USERPROFILE", "HOME"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Windows composition root directly references mutable environment source %q", forbidden)
		}
	}
}
