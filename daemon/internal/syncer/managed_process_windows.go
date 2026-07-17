//go:build windows

package syncer

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func applyManagedBackgroundProcessPolicy(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	command.SysProcAttr.HideWindow = true
}
