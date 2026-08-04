package syncer

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
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

func TestKillManagedProcessAndJoinDistinguishesExitRaceFromLiveKillFailure(t *testing.T) {
	t.Run("already joined skips kill", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		called := false
		if err := killManagedProcessAndJoinWithin(done, func() error {
			called = true
			return errors.New("must not run")
		}, time.Second); err != nil {
			t.Fatalf("already-joined stop returned error: %v", err)
		}
		if called {
			t.Fatal("already-joined stop called Kill")
		}
	})

	t.Run("kill error followed by joined exit is benign", func(t *testing.T) {
		done := make(chan struct{})
		killReturned := make(chan struct{})
		result := make(chan error, 1)
		wantErr := errors.New("TerminateProcess: access denied")
		go func() {
			result <- killManagedProcessAndJoinWithin(done, func() error {
				defer close(killReturned)
				return wantErr
			}, time.Second)
		}()
		<-killReturned
		select {
		case err := <-result:
			t.Fatalf("kill error returned before the Wait-owned join: %v", err)
		default:
		}
		close(done)
		if err := <-result; err != nil {
			t.Fatalf("joined exit race returned error: %v", err)
		}
	})

	t.Run("live kill failure is preserved", func(t *testing.T) {
		done := make(chan struct{})
		wantErr := errors.New("permission denied")
		err := killManagedProcessAndJoinWithin(done, func() error { return wantErr }, 10*time.Millisecond)
		if !errors.Is(err, wantErr) {
			t.Fatalf("live kill failure = %v, want %v", err, wantErr)
		}
	})
}
