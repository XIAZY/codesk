//go:build darwin

package desktop

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const darwinWatchdogReadyByte = byte('C')

// RunDarwinProcessWatchdog handles the private watchdog invocation used by the
// signed desktop executable. Call it before constructing Cocoa application
// state. The watchdog is placed outside the app process group and kills that
// group after any normal or abnormal app exit.
func RunDarwinProcessWatchdog(arguments []string) (bool, error) {
	invocation, handled, err := parseDarwinWatchdogInvocation(arguments)
	if err != nil || !handled {
		return handled, err
	}
	return true, runDarwinProcessWatchdog(invocation)
}

// ContainDarwinProcessTree makes the app its own process-group leader and
// starts a separately grouped signed watchdog before providers may spawn.
func ContainDarwinProcessTree(executable string) error {
	if !filepath.IsAbs(executable) || executable != filepath.Clean(executable) {
		return errors.New("desktop: process containment executable must be absolute and clean")
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return fmt.Errorf("desktop: inspect process containment executable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("desktop: process containment executable must be a real executable file")
	}

	pid := unix.Getpid()
	if unix.Getpgrp() != pid {
		if err := unix.Setpgid(0, 0); err != nil {
			return fmt.Errorf("desktop: create application process group: %w", err)
		}
	}
	if group := unix.Getpgrp(); group != pid {
		return fmt.Errorf("desktop: application process group is %d, want %d", group, pid)
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("desktop: create process watchdog readiness pipe: %w", err)
	}
	defer readyRead.Close()
	command := exec.Command(executable, darwinWatchdogArgument, strconv.Itoa(pid), strconv.Itoa(pid), "3")
	command.ExtraFiles = []*os.File{readyWrite}
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		readyWrite.Close()
		return fmt.Errorf("desktop: start process watchdog: %w", err)
	}
	if err := readyWrite.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("desktop: close process watchdog pipe: %w", err)
	}
	if err := readyRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("desktop: set process watchdog deadline: %w", err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(readyRead, ready[:]); err != nil || ready[0] != darwinWatchdogReadyByte {
		_ = command.Process.Kill()
		_ = command.Wait()
		if err == nil {
			err = errors.New("invalid readiness byte")
		}
		return fmt.Errorf("desktop: process watchdog did not become ready: %w", err)
	}
	go func() {
		_ = command.Wait()
		// Any watchdog exit while this process is still alive means abnormal-exit
		// containment is gone. Fail closed by terminating the app-owned group;
		// normal parent exit destroys this goroutine before it can run.
		_ = killDarwinProcessGroup(pid)
	}()
	return nil
}

func runDarwinProcessWatchdog(invocation darwinWatchdogInvocation) error {
	ready := os.NewFile(uintptr(invocation.readyFD), "codesk-watchdog-ready")
	if ready == nil {
		return errors.New("desktop: process watchdog readiness descriptor is unavailable")
	}
	readyClosed := false
	defer func() {
		if !readyClosed {
			_ = ready.Close()
		}
	}()
	parentGroup, err := unix.Getpgid(invocation.parentPID)
	if errors.Is(err, unix.ESRCH) {
		return killDarwinProcessGroup(invocation.groupID)
	}
	if err != nil {
		return fmt.Errorf("desktop: inspect watchdog parent: %w", err)
	}
	parentExited, err := darwinWatchdogParentExited(invocation, os.Getppid(), parentGroup)
	if err != nil {
		return err
	}
	if parentExited {
		return killDarwinProcessGroup(invocation.groupID)
	}

	queue, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("desktop: create process watchdog queue: %w", err)
	}
	defer unix.Close(queue)
	unix.CloseOnExec(queue)
	change := unix.Kevent_t{
		Ident:  uint64(invocation.parentPID),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	if _, err := unix.Kevent(queue, []unix.Kevent_t{change}, nil, nil); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return killDarwinProcessGroup(invocation.groupID)
		}
		return fmt.Errorf("desktop: register process watchdog: %w", err)
	}
	if _, err := ready.Write([]byte{darwinWatchdogReadyByte}); err != nil {
		return fmt.Errorf("desktop: signal process watchdog readiness: %w", err)
	}
	if err := ready.Close(); err != nil {
		return fmt.Errorf("desktop: close process watchdog readiness: %w", err)
	}
	readyClosed = true

	events := make([]unix.Kevent_t, 1)
	for {
		count, err := unix.Kevent(queue, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("desktop: wait for application exit: %w", err)
		}
		if count == 1 {
			break
		}
	}
	return killDarwinProcessGroup(invocation.groupID)
}

func killDarwinProcessGroup(groupID int) error {
	if err := unix.Kill(-groupID, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("desktop: terminate application process group: %w", err)
	}
	return nil
}
