//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"notty/daemon/internal/desktopacceptance"
)

const (
	eventObjectShow        = 0x8002
	objIDWindow            = 0
	winEventOutOfContext   = 0x0000
	winEventSkipOwnProcess = 0x0002
	pmNoRemove             = 0x0000
	wmQuit                 = 0x0012
)

var (
	user32Observer        = windows.NewLazySystemDLL("user32.dll")
	procSetWinEventHook   = user32Observer.NewProc("SetWinEventHook")
	procUnhookWinEvent    = user32Observer.NewProc("UnhookWinEvent")
	procGetMessage        = user32Observer.NewProc("GetMessageW")
	procPeekMessage       = user32Observer.NewProc("PeekMessageW")
	procPostThreadMessage = user32Observer.NewProc("PostThreadMessageW")
)

type nativeMessage struct {
	hwnd    windows.HWND
	message uint32
	_       uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pointX  int32
	pointY  int32
	private uint32
}

type windowsObserver struct {
	mu             sync.Mutex
	events         []desktopacceptance.SurfaceEvent
	threadID       uint32
	done           chan struct{}
	stop           sync.Once
	captureErrOnce sync.Once
	err            error
	callback       uintptr
	scope          windowObserverScope
}

type windowObserverScope struct {
	RootPIDs         map[int]struct{}
	ExactExecutables []string
	ExecutableRoots  []string
}

func startWindowObserver(scope windowObserverScope) (*windowsObserver, error) {
	observer := &windowsObserver{done: make(chan struct{}), scope: scope}
	ready := make(chan error, 1)
	go observer.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return observer, nil
}

func (o *windowsObserver) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(o.done)
	o.threadID = windows.GetCurrentThreadId()
	var queueMessage nativeMessage
	procPeekMessage.Call(uintptr(unsafe.Pointer(&queueMessage)), 0, 0, 0, pmNoRemove)
	o.callback = syscall.NewCallback(func(
		_ uintptr,
		_ uint32,
		hwnd windows.HWND,
		idObject int32,
		_ int32,
		_ uint32,
		_ uint32,
	) uintptr {
		if hwnd != 0 && idObject == objIDWindow {
			// EVENT_OBJECT_SHOW is the evidence. Rechecking current visibility can
			// discard the exact short-lived console flash this hook must retain.
			o.capture(hwnd, time.Now().UTC(), windowClass(hwnd))
		}
		return 0
	})
	hook, _, callErr := procSetWinEventHook.Call(
		eventObjectShow,
		eventObjectShow,
		0,
		o.callback,
		0,
		0,
		winEventOutOfContext|winEventSkipOwnProcess,
	)
	if hook == 0 {
		ready <- fmt.Errorf("SetWinEventHook: %w", callErr)
		return
	}
	ready <- nil
	defer func() {
		result, _, err := procUnhookWinEvent.Call(hook)
		if result == 0 {
			o.mu.Lock()
			o.err = errors.Join(o.err, fmt.Errorf("UnhookWinEvent: %w", err))
			o.mu.Unlock()
		}
	}()
	for {
		var message nativeMessage
		result, _, err := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) == -1 {
			o.mu.Lock()
			o.err = errors.Join(o.err, fmt.Errorf("GetMessageW: %w", err))
			o.mu.Unlock()
			return
		}
		if result == 0 {
			return
		}
	}
}

func (o *windowsObserver) capture(hwnd windows.HWND, observedAt time.Time, class string) {
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil || pid == 0 {
		if forbiddenSurfaceClass(class) {
			o.recordUnboundConsoleShow(0, observedAt, class, "native console SHOW did not retain a window process identity")
		}
		return
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		if forbiddenSurfaceClass(class) {
			o.recordUnboundConsoleShow(int(pid), observedAt, class, "native console SHOW process ended before exact identity binding")
		}
		return
	}
	defer windows.CloseHandle(handle)
	executable, startedAt := processDetailsHandle(handle)
	processes, err := enumerateProcesses()
	if err != nil {
		o.captureErrOnce.Do(func() {
			o.mu.Lock()
			o.err = errors.Join(o.err, fmt.Errorf("enumerate processes for native-surface correlation: %w", err))
			o.mu.Unlock()
		})
		return
	}
	for index := range processes {
		if processes[index].PID == int(pid) {
			processes[index].Executable = executable
			processes[index].StartedAt = startedAt
			break
		}
	}
	process, relevant := o.scope.correlate(int(pid), processes)
	if !relevant && o.scope.includesExecutable(executable) {
		process = desktopacceptance.Process{PID: int(pid), Executable: executable, StartedAt: startedAt}
		relevant = true
	}
	if !relevant {
		return
	}
	if process.Executable == "" || process.StartedAt == "" {
		o.captureErrOnce.Do(func() {
			o.mu.Lock()
			o.err = errors.Join(o.err, fmt.Errorf("native-surface pid %d lacked an exact executable/creation-time identity", pid))
			o.mu.Unlock()
		})
		return
	}
	executable = process.Executable
	o.mu.Lock()
	o.events = append(o.events, desktopacceptance.SurfaceEvent{
		At: observedAt, PID: int(pid), ParentPID: process.ParentPID,
		Executable: executable, StartedAt: process.StartedAt, Class: class,
		Forbidden: forbiddenSurfaceExecutable(executable) || forbiddenSurfaceClass(class),
	})
	o.mu.Unlock()
}

func (o *windowsObserver) recordUnboundConsoleShow(pid int, observedAt time.Time, class, detail string) {
	o.mu.Lock()
	o.events = append(o.events, desktopacceptance.SurfaceEvent{
		At: observedAt, PID: pid, Class: class, Forbidden: true,
	})
	o.mu.Unlock()
	o.captureErrOnce.Do(func() {
		o.mu.Lock()
		o.err = errors.Join(o.err, errors.New(detail))
		o.mu.Unlock()
	})
}

func forbiddenSurfaceExecutable(executable string) bool {
	name := strings.ToLower(filepath.Base(executable))
	return map[string]bool{
		"cmd.exe": true, "conhost.exe": true, "powershell.exe": true, "pwsh.exe": true,
		"node.exe": true, "codex.exe": true, "claude.exe": true,
	}[name]
}

func forbiddenSurfaceClass(class string) bool {
	return strings.EqualFold(class, "ConsoleWindowClass") || strings.Contains(strings.ToLower(class), "console")
}

func (s windowObserverScope) correlate(pid int, processes []desktopacceptance.Process) (desktopacceptance.Process, bool) {
	byPID := make(map[int]desktopacceptance.Process, len(processes))
	for _, process := range processes {
		byPID[process.PID] = process
	}
	observed, ok := byPID[pid]
	if !ok {
		return desktopacceptance.Process{}, false
	}
	seen := make(map[int]struct{})
	current := observed
	for current.PID > 0 {
		if _, ok := s.RootPIDs[current.PID]; ok || s.includesExecutable(current.Executable) {
			return observed, true
		}
		if _, ok := seen[current.PID]; ok {
			return desktopacceptance.Process{}, false
		}
		seen[current.PID] = struct{}{}
		parent, ok := byPID[current.ParentPID]
		if !ok {
			return desktopacceptance.Process{}, false
		}
		current = parent
	}
	return desktopacceptance.Process{}, false
}

func (s windowObserverScope) includesExecutable(executable string) bool {
	if executable == "" {
		return false
	}
	for _, expected := range s.ExactExecutables {
		if samePath(expected, executable) {
			return true
		}
	}
	for _, root := range s.ExecutableRoots {
		inside, err := pathWithin(root, executable)
		if err == nil && inside {
			return true
		}
	}
	return false
}

func (o *windowsObserver) Stop(ctx context.Context) ([]desktopacceptance.SurfaceEvent, error) {
	o.stop.Do(func() {
		result, _, err := procPostThreadMessage.Call(uintptr(o.threadID), wmQuit, 0, 0)
		if result == 0 {
			o.mu.Lock()
			o.err = errors.Join(o.err, fmt.Errorf("PostThreadMessageW: %w", err))
			o.mu.Unlock()
		}
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.done:
		o.mu.Lock()
		defer o.mu.Unlock()
		return append([]desktopacceptance.SurfaceEvent(nil), o.events...), o.err
	}
}

func windowClass(hwnd windows.HWND) string {
	buffer := make([]uint16, 256)
	length, err := windows.GetClassName(hwnd, &buffer[0], int32(len(buffer)))
	if err != nil || length <= 0 {
		return ""
	}
	return windows.UTF16ToString(buffer[:length])
}
