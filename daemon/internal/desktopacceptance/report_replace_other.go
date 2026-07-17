//go:build !windows

package desktopacceptance

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
