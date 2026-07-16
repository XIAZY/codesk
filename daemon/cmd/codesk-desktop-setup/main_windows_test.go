//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"notty/daemon/internal/desktopsetup"
)

var (
	setupTestUser32         = windows.NewLazySystemDLL("user32.dll")
	setupTestFindWindowW    = setupTestUser32.NewProc("FindWindowW")
	setupTestPostMessageW   = setupTestUser32.NewProc("PostMessageW")
	setupTestWindowTitle, _ = windows.UTF16PtrFromString("Codesk Setup")
)

const setupTestWMClose = 0x0010

func TestSetupExecutableInvalidArgumentsExitWithFailure(t *testing.T) {
	buildContext, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()

	executable := filepath.Join(t.TempDir(), "CodeskSetup.exe")
	ldflags := "-buildid= -H=windowsgui -s -w -X main.setupVersion=process-test -X main.setupArch=" + runtime.GOARCH
	build := exec.CommandContext(
		buildContext,
		"go",
		"build",
		"-buildvcs=false",
		"-trimpath",
		"-ldflags",
		ldflags,
		"-o",
		executable,
		".",
	)
	if output, err := build.CombinedOutput(); err != nil {
		if errors.Is(buildContext.Err(), context.DeadlineExceeded) {
			t.Fatal("timed out building setup process-test executable")
		}
		t.Fatalf("build setup process-test executable: %v\n%s", err, output)
	}

	tests := []struct {
		name      string
		arguments []string
	}{
		{name: "quiet unknown flag", arguments: []string{"--quiet", "--not-a-setup-option"}},
		{name: "non-quiet unexpected positional argument", arguments: []string{"unexpected"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runContext, cancelRun := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelRun()

			command := exec.CommandContext(runContext, executable, test.arguments...)
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err := command.Start(); err != nil {
				t.Fatalf("start setup process for arguments %q: %v", test.arguments, err)
			}
			processID := uint32(command.Process.Pid)
			stopDialogCloser := make(chan struct{})
			dialogCloserDone := make(chan struct{})
			go func() {
				defer close(dialogCloserDone)
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopDialogCloser:
						return
					case <-ticker.C:
						closeSetupTestDialog(processID)
					}
				}
			}()
			err := command.Wait()
			close(stopDialogCloser)
			<-dialogCloserDone
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				t.Fatalf("setup process timed out for arguments %q", test.arguments)
			}
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) {
				t.Fatalf("setup process error for arguments %q = %v, want exit code %d\n%s", test.arguments, err, desktopsetup.ExitFailure, output.Bytes())
			}
			if exitCode := exitError.ExitCode(); exitCode != desktopsetup.ExitFailure {
				t.Fatalf("setup process exit code for arguments %q = %d, want %d\n%s", test.arguments, exitCode, desktopsetup.ExitFailure, output.Bytes())
			}
		})
	}
}

func closeSetupTestDialog(processID uint32) {
	window, _, _ := setupTestFindWindowW.Call(0, uintptr(unsafe.Pointer(setupTestWindowTitle)))
	if window == 0 {
		return
	}
	var windowProcessID uint32
	_, _ = windows.GetWindowThreadProcessId(windows.HWND(window), &windowProcessID)
	if windowProcessID == processID {
		_, _, _ = setupTestPostMessageW.Call(window, setupTestWMClose, 0, 0)
	}
}
