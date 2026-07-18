//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const windowsRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

type windowsLoginItem struct {
	valueName string
	command   string
}

func NewWindowsLoginItem(valueName, executablePath string) (LoginItem, error) {
	if strings.TrimSpace(valueName) == "" || strings.ContainsAny(valueName, "\x00\\") {
		return nil, errors.New("desktop: invalid login item name")
	}
	if err := requireAbsolute("executable", executablePath); err != nil {
		return nil, err
	}
	if strings.ContainsAny(executablePath, "\x00\"") {
		return nil, errors.New("desktop: invalid login executable path")
	}
	return &windowsLoginItem{
		valueName: valueName,
		command:   `"` + filepath.Clean(executablePath) + `"`,
	}, nil
}

func (i *windowsLoginItem) Enable() error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("desktop: open login registry key: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(i.valueName, i.command); err != nil {
		return fmt.Errorf("desktop: enable login item: %w", err)
	}
	return nil
}

func (i *windowsLoginItem) Disable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("desktop: open login registry key: %w", err)
	}
	defer key.Close()
	if err := key.DeleteValue(i.valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("desktop: disable login item: %w", err)
	}
	return nil
}

func (i *windowsLoginItem) IsEnabled() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("desktop: open login registry key: %w", err)
	}
	defer key.Close()
	command, _, err := key.GetStringValue(i.valueName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("desktop: read login item: %w", err)
	}
	return loginItemRegistrationMatches(command, i.command), nil
}
