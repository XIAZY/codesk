//go:build !windows

package syncer

import "os"

func commitReplacement(stagedPath, targetPath string) error {
	return os.Rename(stagedPath, targetPath)
}
