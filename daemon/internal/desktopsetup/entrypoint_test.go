package desktopsetup

import (
	"errors"
	"strings"
	"testing"
)

func TestRunMainReportsFailuresWithNonzeroExit(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		setupErr  error
		wantRun   bool
	}{
		{
			name:      "invalid arguments",
			arguments: []string{"unexpected"},
		},
		{
			name:     "setup failure",
			setupErr: errors.New("payload verification failed"),
			wantRun:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var ran bool
			var messages []shownMessage
			exitCode := RunMain(
				test.arguments,
				"1.2.3",
				"amd64",
				func(options Options) error {
					ran = true
					return test.setupErr
				},
				func(kind MessageKind, message string) {
					messages = append(messages, shownMessage{kind: kind, message: message})
				},
			)
			if exitCode != ExitFailure {
				t.Fatalf("RunMain() exit code = %d, want %d", exitCode, ExitFailure)
			}
			if ran != test.wantRun {
				t.Fatalf("setup ran = %t, want %t", ran, test.wantRun)
			}
			if len(messages) != 1 || messages[0].kind != ErrorMessage {
				t.Fatalf("messages = %#v, want one error", messages)
			}
			if !strings.HasPrefix(messages[0].message, "Codesk setup could not complete.\n\n") {
				t.Fatalf("error message = %q", messages[0].message)
			}
		})
	}
}

func TestRunMainQuietFailureIsNonzeroWithoutDialog(t *testing.T) {
	for _, arguments := range [][]string{
		{"--quiet", "unexpected"},
		{"--QUIET=true", "unexpected"},
	} {
		exitCode := RunMain(
			arguments,
			"1.2.3",
			"amd64",
			func(Options) error { return errors.New("must not run") },
			func(MessageKind, string) { t.Fatal("quiet failure displayed a dialog") },
		)
		if exitCode != ExitFailure {
			t.Fatalf("RunMain(%q) exit code = %d, want %d", arguments, exitCode, ExitFailure)
		}
	}
}

func TestRunMainSuccessMessagesAndExitCode(t *testing.T) {
	tests := []struct {
		name        string
		arguments   []string
		wantMessage string
	}{
		{name: "install", wantMessage: "Codesk was installed successfully."},
		{name: "quiet install", arguments: []string{"--quiet"}},
		{name: "uninstall", arguments: []string{"--uninstall"}, wantMessage: "Codesk was uninstalled successfully."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var messages []shownMessage
			exitCode := RunMain(
				test.arguments,
				"1.2.3",
				"arm64",
				func(options Options) error {
					if options.Version != "1.2.3" || options.Arch != "arm64" {
						t.Fatalf("setup options = %#v", options)
					}
					return nil
				},
				func(kind MessageKind, message string) {
					messages = append(messages, shownMessage{kind: kind, message: message})
				},
			)
			if exitCode != ExitSuccess {
				t.Fatalf("RunMain() exit code = %d, want %d", exitCode, ExitSuccess)
			}
			if test.wantMessage == "" {
				if len(messages) != 0 {
					t.Fatalf("messages = %#v, want none", messages)
				}
				return
			}
			if len(messages) != 1 || messages[0] != (shownMessage{kind: InformationMessage, message: test.wantMessage}) {
				t.Fatalf("messages = %#v", messages)
			}
		})
	}
}

type shownMessage struct {
	kind    MessageKind
	message string
}
