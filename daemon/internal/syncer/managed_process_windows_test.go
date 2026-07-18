//go:build windows

package syncer

import (
	"context"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedBackgroundCommandsSuppressWindowsConsole(t *testing.T) {
	commands := map[string]*exec.Cmd{
		"runtime": managedBackgroundCommand("managed-child.exe"),
		"context": managedBackgroundCommandContext(context.Background(), "managed-child.exe"),
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			attributes := command.SysProcAttr
			if attributes == nil {
				t.Fatal("managed child has no Windows process attributes")
			}
			if attributes.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
				t.Fatalf("managed child creation flags %#x do not include CREATE_NO_WINDOW", attributes.CreationFlags)
			}
			if !attributes.HideWindow {
				t.Fatal("managed child does not set HideWindow")
			}
			if attributes.NoInheritHandles {
				t.Fatal("managed child disables the standard handles used for stdin/stdout/stderr capture")
			}
		})
	}
}
