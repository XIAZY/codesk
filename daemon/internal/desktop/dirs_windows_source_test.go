package desktop

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsDefaultDirsUsesNativeAccountRoot(t *testing.T) {
	source, err := os.ReadFile("dirs_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		"windows.KnownFolderPath",
		"windows.FOLDERID_LocalAppData",
		"windows.KF_FLAG_CREATE",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Windows desktop root resolver is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`os.`,
		`syscall.`,
		`GetEnvironmentVariable`,
		`"LOCALAPPDATA"`,
		`"APPDATA"`,
		`"USERPROFILE"`,
		`"HOME"`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Windows desktop root resolver trusts mutable environment source %q", forbidden)
		}
	}
}
