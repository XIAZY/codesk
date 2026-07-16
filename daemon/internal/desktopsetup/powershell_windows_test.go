//go:build windows

package desktopsetup

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestSelfDeleteRemovesTheExternalSetupTreeRecursively(t *testing.T) {
	if !strings.Contains(selfDeleteScript, "Remove-Item -LiteralPath $env:CODESK_DELETE_PATH -Recurse -Force") {
		t.Fatal("self-delete script does not recursively remove the external setup tree")
	}
	for _, required := range []string{
		"CODESK_DELETE_PARENT_HANDLE",
		"SafeWaitHandle",
		"$waitHandle.WaitOne()",
	} {
		if !strings.Contains(selfDeleteScript, required) {
			t.Fatalf("self-delete script is missing exact-parent wait %q", required)
		}
	}
	if strings.Contains(selfDeleteScript, "Get-Process") || strings.Contains(selfDeleteScript, "PARENT_PID") {
		t.Fatal("self-delete script resolves a reusable parent PID")
	}
}

func TestSelfDeleteScriptWaitsOnInheritedExactHandle(t *testing.T) {
	security := &windows.SecurityAttributes{InheritHandle: 1}
	security.Length = uint32(unsafe.Sizeof(*security))
	waitEvent, err := windows.CreateEvent(security, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = windows.SetEvent(waitEvent)
		_ = windows.CloseHandle(waitEvent)
	})

	system, err := windows.KnownFolderPath(windows.FOLDERID_System, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatal(err)
	}
	powerShell := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	deletePath := filepath.Join(t.TempDir(), "Setup")
	if err := os.Mkdir(deletePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deletePath, "Uninstall Codesk.exe"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment, err := mergedEnvironment(map[string]string{
		"CODESK_DELETE_PARENT_HANDLE": strconv.FormatUint(uint64(waitEvent), 10),
		"CODESK_DELETE_PATH":          deletePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := startDetachedWithInheritedHandles(
		powerShell,
		environment,
		[]syscall.Handle{syscall.Handle(waitEvent)},
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", selfDeleteScript,
	); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(deletePath); err != nil {
		t.Fatalf("self-delete ran before the exact handle was signaled: %v", err)
	}
	if err := windows.SetEvent(waitEvent); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(deletePath); os.IsNotExist(err) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("self-delete did not run after the inherited exact handle was signaled")
}
