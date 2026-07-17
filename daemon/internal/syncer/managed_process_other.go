//go:build !windows

package syncer

import "os/exec"

func applyManagedBackgroundProcessPolicy(command *exec.Cmd) {
	_ = command
}
