//go:build windows

package desktop

import (
	"notty/daemon/internal/winlock"
)

type windowsInstanceLock struct {
	file *winlock.FileLock
}

func NewWindowsInstanceLock(path string) InstanceLock {
	return &windowsInstanceLock{file: winlock.New(path)}
}

func (l *windowsInstanceLock) Acquire() (bool, error) {
	return l.file.Acquire()
}

func (l *windowsInstanceLock) Release() error {
	return l.file.Release()
}
