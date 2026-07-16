package desktop

import (
	"strings"
	"testing"

	"notty/daemon/internal/macosuser"
)

func TestDefaultDirsDarwin(t *testing.T) {
	home, err := macosuser.HomeDir()
	if err != nil {
		t.Skipf("account home lookup failed: %v", err)
	}
	t.Setenv("HOME", t.TempDir())

	dirs, err := DefaultDirs()
	if err != nil {
		t.Fatalf("DefaultDirs() error = %v", err)
	}

	if dirs.Data != home+"/Library/Application Support/Codesk" {
		t.Errorf("Data = %q, want %q", dirs.Data, home+"/Library/Application Support/Codesk")
	}
	if dirs.Logs != home+"/Library/Logs/Codesk" {
		t.Errorf("Logs = %q, want %q", dirs.Logs, home+"/Library/Logs/Codesk")
	}
	if dirs.Cache != home+"/Library/Caches/Codesk" {
		t.Errorf("Cache = %q, want %q", dirs.Cache, home+"/Library/Caches/Codesk")
	}

	for _, cli := range []string{".notty", "/notty"} {
		if strings.HasSuffix(dirs.Data, cli) {
			t.Errorf("Data %q ends with CLI path %q", dirs.Data, cli)
		}
	}
}
