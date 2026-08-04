package syncer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const managedProcessKillErrorJoinWait = 2 * time.Second

// managedBackgroundCommand constructs a supervised runtime child whose
// lifetime is not tied to a reconcile context.
func managedBackgroundCommand(name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	applyManagedBackgroundProcessPolicy(command)
	return command
}

// managedBackgroundCommandContext constructs a bounded child such as a
// detection probe. Published runtime children must use managedBackgroundCommand
// and remain owned by RuntimeProcess.Stop instead of a construction context.
func managedBackgroundCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	applyManagedBackgroundProcessPolicy(command)
	return command
}

// killManagedProcessAndJoin resolves the race between graceful stdin closure,
// the Wait owner, and Process.Kill. In particular, Windows can report
// ERROR_ACCESS_DENIED when TerminateProcess lands after the child has exited.
// The syscall error is treated as benign only when the Wait-owned completion
// signal proves the child was joined within the bounded race window.
func killManagedProcessAndJoin(done <-chan struct{}, kill func() error) error {
	return killManagedProcessAndJoinWithin(done, kill, managedProcessKillErrorJoinWait)
}

func killManagedProcessAndJoinWithin(done <-chan struct{}, kill func() error, errorJoinWait time.Duration) error {
	select {
	case <-done:
		return nil
	default:
	}

	err := kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		<-done
		return nil
	}

	timer := time.NewTimer(errorJoinWait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return err
	}
}
