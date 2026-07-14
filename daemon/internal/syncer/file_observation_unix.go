//go:build !windows

package syncer

import "os"

func openFileObservation(path string) (*os.File, error) {
	return os.Open(path)
}
