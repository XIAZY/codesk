package syncer

import (
	"context"
	"os/exec"
)

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
