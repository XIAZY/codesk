//go:build windows

package winlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// FileLock uses Windows file-sharing exclusion. Unlike Local\ named kernel
// objects, the lock applies to every session that resolves the same per-user
// path.
type FileLock struct {
	path   string
	mu     sync.Mutex
	handle windows.Handle
}

func New(path string) *FileLock {
	return &FileLock{path: path}
}

func (l *FileLock) Acquire() (bool, error) {
	if l == nil {
		return false, errors.New("Windows file lock is nil")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.handle != 0 {
		return false, errors.New("Windows file lock is already acquired")
	}
	if err := validatePath(l.path); err != nil {
		return false, err
	}
	parent := filepath.Dir(l.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, fmt.Errorf("create Windows lock directory: %w", err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return false, fmt.Errorf("inspect Windows lock directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("Windows lock directory is not a real directory")
	}
	if info, err := os.Lstat(l.path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, errors.New("Windows lock path is not a real file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect Windows lock file: %w", err)
	}

	path, err := windows.UTF16PtrFromString(l.path)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire Windows file lock: %w", err)
	}
	l.handle = handle
	return true, nil
}

func (l *FileLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.handle == 0 {
		return nil
	}
	handle := l.handle
	l.handle = 0
	return windows.CloseHandle(handle)
}

func validatePath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsRune(path, '\x00') ||
		!filepath.IsAbs(path) || path != filepath.Clean(path) {
		return errors.New("invalid Windows file lock path")
	}
	return nil
}
