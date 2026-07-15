package desktop

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultDirsWindows(t *testing.T) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		t.Skip("LOCALAPPDATA not set")
	}

	dirs, err := DefaultDirs()
	if err != nil {
		t.Fatalf("DefaultDirs() error = %v", err)
	}

	want := local + `\Codesk`
	if dirs.Data != want {
		t.Errorf("Data = %q, want %q", dirs.Data, want)
	}
	if dirs.Logs != want+`\Logs` {
		t.Errorf("Logs = %q, want %q", dirs.Logs, want+`\Logs`)
	}
	if dirs.Cache != want+`\Cache` {
		t.Errorf("Cache = %q, want %q", dirs.Cache, want+`\Cache`)
	}

	for _, cli := range []string{".notty", `\notty`} {
		if strings.Contains(dirs.Data, cli) {
			t.Errorf("Data %q contains CLI path %q", dirs.Data, cli)
		}
	}
}

func TestDefaultDirsWindowsMissingLocalAppData(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	_, err := DefaultDirs()
	if err == nil {
		t.Error("DefaultDirs() should fail when LOCALAPPDATA is empty")
	}
}
