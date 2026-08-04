package syncer

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestManagedBackgroundCommandsPreserveArgumentsStreamsExitAndContext(t *testing.T) {
	executable := fakeProcessCommand(t, fakeProcessManagedChildIO)
	arguments := []string{"one", "two words", ""}
	factories := map[string]func(string, ...string) *exec.Cmd{
		"runtime": managedBackgroundCommand,
		"context": func(name string, args ...string) *exec.Cmd {
			return managedBackgroundCommandContext(context.Background(), name, args...)
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			command := factory(executable, arguments...)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
				t.Fatalf("managed child error = %v, want exit code 23", err)
			}
			if got, want := stdout.String(), "stdout:one|two words|"; got != want {
				t.Fatalf("managed child stdout = %q, want %q", got, want)
			}
			if got, want := stderr.String(), "stderr:one|two words|"; got != want {
				t.Fatalf("managed child stderr = %q, want %q", got, want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := managedBackgroundCommandContext(ctx, executable, "canceled").Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("managed context command error = %v, want context cancellation", err)
	}
}
