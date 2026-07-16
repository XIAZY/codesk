//go:build !windows

package desktopsetup

import "os"

func movePathDurably(source, destination string, replace bool) error {
	if !replace {
		if _, err := os.Lstat(destination); err == nil {
			return os.ErrExist
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(source, destination)
}
