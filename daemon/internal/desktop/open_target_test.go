package desktop

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateOpenTarget(t *testing.T) {
	logs := filepath.Join(t.TempDir(), "Logs", "Codesk")
	tests := []struct {
		name        string
		target      string
		wantDir     bool
		wantFailure bool
	}{
		{name: "logs directory", target: logs, wantDir: true},
		{name: "workspace URL", target: "https://app.getcodesk.com/workspaces/example"},
		{name: "connect callback query", target: "https://app.getcodesk.com/desktop/connect?callback=http%3A%2F%2F127.0.0.1%3A43123%2Fnonce"},
		{name: "localhost test origin", target: "http://127.0.0.1:3000/desktop/connect?callback=value"},
		{name: "other directory", target: filepath.Dir(logs), wantFailure: true},
		{name: "file URL", target: "file:///tmp/Codesk", wantFailure: true},
		{name: "credentials", target: "https://token@example.com/path", wantFailure: true},
		{name: "fragment", target: "https://example.com/path#fragment", wantFailure: true},
		{name: "relative", target: "/desktop/connect", wantFailure: true},
		{name: "control", target: "https://example.com/path\nother", wantFailure: true},
		{name: "oversized", target: "https://example.com/" + strings.Repeat("a", maximumOpenTargetBytes), wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isDirectory, err := validateOpenTarget(test.target, logs)
			if (err != nil) != test.wantFailure {
				t.Fatalf("validateOpenTarget() error = %v, wantFailure = %t", err, test.wantFailure)
			}
			if err == nil && isDirectory != test.wantDir {
				t.Fatalf("validateOpenTarget() directory = %t, want %t", isDirectory, test.wantDir)
			}
		})
	}
}

func TestValidateOpenTargetRejectsInvalidLogsDirectory(t *testing.T) {
	for _, logs := range []string{"relative/logs", filepath.Join(t.TempDir(), "Logs") + "\x00suffix"} {
		if _, err := validateOpenTarget("https://example.com", logs); err == nil {
			t.Fatalf("validateOpenTarget() unexpectedly accepted logs directory %q", logs)
		}
	}
}
