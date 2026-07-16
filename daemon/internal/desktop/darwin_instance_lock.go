//go:build darwin

package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type darwinInstanceLock struct {
	path string

	mu   sync.Mutex
	file *os.File
}

func NewDarwinInstanceLock(path string) (InstanceLock, error) {
	if err := requireAbsolute("instance lock", path); err != nil {
		return nil, err
	}
	return &darwinInstanceLock{path: path}, nil
}

func (l *darwinInstanceLock) Acquire() (bool, error) {
	if l == nil || l.path == "" {
		return false, errors.New("desktop: instance lock is not initialized")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return true, nil
	}
	directory := filepath.Dir(l.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, fmt.Errorf("desktop: create instance lock directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return false, fmt.Errorf("desktop: inspect instance lock directory: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return false, errors.New("desktop: instance lock directory is not a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return false, fmt.Errorf("desktop: protect instance lock directory: %w", err)
	}

	fd, err := unix.Open(l.path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, fmt.Errorf("desktop: open instance lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), l.path)
	if file == nil {
		_ = unix.Close(fd)
		return false, errors.New("desktop: wrap instance lock descriptor")
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("desktop: inspect instance lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("desktop: instance lock is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		return false, fmt.Errorf("desktop: protect instance lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return false, nil
		}
		return false, fmt.Errorf("desktop: acquire instance lock: %w", err)
	}
	l.file = file
	closeFile = false
	return true, nil
}

func (l *darwinInstanceLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		unlockErr = fmt.Errorf("desktop: release instance lock: %w", unlockErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("desktop: close instance lock: %w", closeErr)
	}
	return errors.Join(unlockErr, closeErr)
}
