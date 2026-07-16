// Package macosuser resolves macOS account properties without trusting the
// caller's process environment.
package macosuser

import (
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
)

// HomeDir returns the current uid's home from the operating-system account
// record. In particular, it never consults HOME.
func HomeDir() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("codesk macOS account: resolve current uid: %w", err)
	}
	account, err := user.LookupId(current.Uid)
	if err != nil {
		return "", fmt.Errorf("codesk macOS account: look up uid %s: %w", current.Uid, err)
	}
	if account.Uid != current.Uid {
		return "", errors.New("codesk macOS account: uid lookup returned a different account")
	}
	home := account.HomeDir
	if home == "" || home != strings.TrimSpace(home) || strings.ContainsRune(home, '\x00') ||
		!filepath.IsAbs(home) || home != filepath.Clean(home) {
		return "", errors.New("codesk macOS account: account home must be an absolute clean path")
	}
	return home, nil
}
