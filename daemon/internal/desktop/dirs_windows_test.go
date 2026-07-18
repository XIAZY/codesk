package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDefaultDirsWindows(t *testing.T) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Skipf("current-user local application data lookup failed: %v", err)
	}
	forged := make(map[string]string)
	for _, name := range []string{"LOCALAPPDATA", "APPDATA", "USERPROFILE", "HOME"} {
		forged[name] = filepath.Join(t.TempDir(), "forged", name)
		t.Setenv(name, forged[name])
	}

	dirs, err := DefaultDirs()
	if err != nil {
		t.Fatalf("DefaultDirs() error = %v", err)
	}

	want := filepath.Join(local, "Codesk")
	if dirs.Data != want {
		t.Errorf("Data = %q, want %q", dirs.Data, want)
	}
	if dirs.Logs != filepath.Join(want, "Logs") {
		t.Errorf("Logs = %q, want %q", dirs.Logs, filepath.Join(want, "Logs"))
	}
	if dirs.Cache != filepath.Join(want, "Cache") {
		t.Errorf("Cache = %q, want %q", dirs.Cache, filepath.Join(want, "Cache"))
	}
	for name, path := range forged {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("DefaultDirs() created forged %s root %q: %v", name, path, err)
		}
	}

	for _, cli := range []string{".notty", `\notty`} {
		if strings.Contains(dirs.Data, cli) {
			t.Errorf("Data %q contains CLI path %q", dirs.Data, cli)
		}
	}
}
