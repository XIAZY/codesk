package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentWithOverridesRemovesInheritedCaseInsensitiveDuplicates(t *testing.T) {
	base := []string{
		"PATH=keep",
		"codesk_accept_artifact=hostile-lowercase",
		"CODESK_ACCEPT_ARTIFACT=hostile-uppercase",
		"CODESK_ACCEPT_SHORTCUT=hostile-shortcut",
		"=C:=C:\\work",
	}
	got := environmentWithOverridesFrom(base, map[string]string{
		"CODESK_ACCEPT_SHORTCUT": "expected-shortcut",
		"CODESK_ACCEPT_ARTIFACT": "expected-artifact",
	})
	want := []string{
		"PATH=keep",
		"=C:=C:\\work",
		"CODESK_ACCEPT_ARTIFACT=expected-artifact",
		"CODESK_ACCEPT_SHORTCUT=expected-shortcut",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

func TestEnvironmentWithOverridesControlsChildProbePath(t *testing.T) {
	temporary := t.TempDir()
	trusted := filepath.Join(temporary, "trusted-probe")
	hostileLower := filepath.Join(temporary, "hostile-lower")
	hostileUpper := filepath.Join(temporary, "hostile-upper")
	base := append([]string(nil), os.Environ()...)
	base = append(base,
		"codesk_accept_artifact="+hostileLower,
		"CODESK_ACCEPT_ARTIFACT="+hostileUpper,
	)

	command := exec.Command(os.Args[0], "-test.run=^TestEnvironmentProbeHelperProcess$")
	command.Env = environmentWithOverridesFrom(base, map[string]string{
		"CODESK_ACCEPT_ARTIFACT":           trusted,
		"GO_WANT_ENVIRONMENT_PROBE_HELPER": "1",
	})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("probe helper failed: %v: %s", err, output)
	}
	if contents, err := os.ReadFile(trusted); err != nil || string(contents) != "trusted" {
		t.Fatalf("trusted probe output = %q, %v", contents, err)
	}
	for _, path := range []string{hostileLower, hostileUpper} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("hostile probe path was touched: %s err=%v", path, err)
		}
	}
}

func TestEnvironmentProbeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ENVIRONMENT_PROBE_HELPER") != "1" {
		return
	}
	const name = "CODESK_ACCEPT_ARTIFACT"
	var matches []string
	for _, entry := range os.Environ() {
		separator := strings.IndexByte(entry, '=')
		if separator > 0 && strings.EqualFold(entry[:separator], name) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 || !strings.HasPrefix(matches[0], name+"=") {
		t.Fatalf("probe environment contains non-canonical or duplicate values: %q", matches)
	}
	path := strings.TrimPrefix(matches[0], name+"=")
	if err := os.WriteFile(path, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
}
