//go:build windows

package desktopsetup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	maximumWindowsPathCharacters = 32768
	processStopTimeout           = 10 * time.Second
)

type matchingProcess struct {
	pid  uint32
	path string
}

func stopProcessesAtPaths(ctx context.Context, targetPaths ...string) error {
	targets := make([]string, 0, len(targetPaths))
	for _, path := range targetPaths {
		if err := validateWindowsPath(path); err != nil {
			return err
		}
		targets = append(targets, normalizeWindowsProcessPath(path))
	}

	for pass := 0; pass < 4; pass++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("desktop setup: stop processes: %w", err)
		}
		matches, err := findProcessesAtPaths(targets)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return nil
		}
		for _, match := range matches {
			if err := terminateExactProcess(ctx, match); err != nil {
				return err
			}
		}
	}
	return errors.New("desktop setup: matching process restarted during shutdown")
}

func findProcessesAtPaths(targets []string) ([]matchingProcess, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	targetBases := make(map[string]struct{}, len(targets))
	for _, path := range targets {
		targetBases[strings.ToLower(filepath.Base(path))] = struct{}{}
	}

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("desktop setup: enumerate processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, fmt.Errorf("desktop setup: enumerate first process: %w", err)
	}

	currentPID := windows.GetCurrentProcessId()
	var matches []matchingProcess
	for {
		if entry.ProcessID != 0 && entry.ProcessID != currentPID {
			path, queryErr := queryProcessImagePath(entry.ProcessID, windows.PROCESS_QUERY_LIMITED_INFORMATION)
			if queryErr != nil {
				base := strings.ToLower(windows.UTF16ToString(entry.ExeFile[:]))
				if _, relevant := targetBases[base]; relevant && !processGone(queryErr) {
					return nil, fmt.Errorf("desktop setup: inspect possible matching process %d: %w", entry.ProcessID, queryErr)
				}
			} else {
				normalized := normalizeWindowsProcessPath(path)
				for _, target := range targets {
					if strings.EqualFold(normalized, target) {
						matches = append(matches, matchingProcess{pid: entry.ProcessID, path: normalized})
						break
					}
				}
			}
		}

		err := windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("desktop setup: enumerate next process: %w", err)
		}
	}
	return matches, nil
}

func terminateExactProcess(ctx context.Context, match matchingProcess) error {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_TERMINATE | windows.SYNCHRONIZE)
	handle, err := windows.OpenProcess(access, false, match.pid)
	if err != nil {
		if processGone(err) {
			return nil
		}
		return fmt.Errorf("desktop setup: open matching process %d: %w", match.pid, err)
	}
	defer windows.CloseHandle(handle)

	path, err := queryProcessImagePathHandle(handle)
	if err != nil {
		if processGone(err) {
			return nil
		}
		return fmt.Errorf("desktop setup: recheck matching process %d: %w", match.pid, err)
	}
	if !strings.EqualFold(normalizeWindowsProcessPath(path), match.path) {
		return nil
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return fmt.Errorf("desktop setup: terminate matching process %d: %w", match.pid, err)
	}

	deadline := time.Now().Add(processStopTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("desktop setup: wait for process %d: %w", match.pid, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("desktop setup: process %d did not exit", match.pid)
		}
		wait := remaining
		if wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		result, err := windows.WaitForSingleObject(handle, uint32(wait/time.Millisecond))
		if err != nil {
			return fmt.Errorf("desktop setup: wait for process %d: %w", match.pid, err)
		}
		switch result {
		case windows.WAIT_OBJECT_0:
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			return fmt.Errorf("desktop setup: unexpected wait result %#x for process %d", result, match.pid)
		}
	}
}

func queryProcessImagePath(pid uint32, access uint32) (string, error) {
	handle, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	return queryProcessImagePathHandle(handle)
}

func queryProcessImagePathHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, maximumWindowsPathCharacters)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	if size == 0 || int(size) >= len(buffer) {
		return "", errors.New("invalid process image path")
	}
	return windows.UTF16ToString(buffer[:size]), nil
}

func normalizeWindowsProcessPath(path string) string {
	for _, prefix := range []string{`\\?\`, `\??\`} {
		if strings.HasPrefix(path, prefix) {
			path = path[len(prefix):]
			break
		}
	}
	return filepath.Clean(path)
}

func processGone(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_FOUND)
}

func startDetached(executable string, arguments ...string) error {
	return startDetachedWithEnvironment(executable, nil, arguments...)
}

func startDetachedWithEnvironment(executable string, environment []string, arguments ...string) error {
	return startDetachedWithInheritedHandles(executable, environment, nil, arguments...)
}

func startDetachedWithInheritedHandles(
	executable string,
	environment []string,
	inheritedHandles []syscall.Handle,
	arguments ...string,
) error {
	if err := validateWindowsPath(executable); err != nil {
		return err
	}
	command := exec.Command(executable, arguments...)
	if environment != nil {
		command.Env = environment
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:                 true,
		NoInheritHandles:           len(inheritedHandles) == 0,
		AdditionalInheritedHandles: inheritedHandles,
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("desktop setup: start detached process: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("desktop setup: release detached process: %w", err)
	}
	return nil
}
