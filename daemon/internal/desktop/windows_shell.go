//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

type windowsShellOpener struct{}

func NewWindowsShellOpener() OpenURL {
	return windowsShellOpener{}
}

func (windowsShellOpener) Open(target string) error {
	if strings.TrimSpace(target) == "" || strings.ContainsRune(target, '\x00') {
		return errors.New("desktop: invalid shell target")
	}
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := windows.ShellExecute(0, nil, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("desktop: shell open failed: %w", err)
	}
	return nil
}
