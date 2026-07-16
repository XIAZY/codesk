//go:build windows

package desktopsetup

import (
	"errors"

	"notty/daemon/internal/winlock"
)

type setupLock struct {
	file *winlock.FileLock
}

func acquireSetupLock(path string) (*setupLock, error) {
	file := winlock.New(path)
	acquired, err := file.Acquire()
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errors.New("desktop setup: another Codesk setup is already running")
	}
	return &setupLock{file: file}, nil
}

func (l *setupLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Release()
}
