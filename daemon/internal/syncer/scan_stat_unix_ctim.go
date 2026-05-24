//go:build linux || openbsd || dragonfly || aix || solaris

package syncer

import (
	"fmt"
	"os"
	"syscall"
)

func fileKeyFromInfo(info os.FileInfo) (string, bool) {
	if info == nil {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), true
}

func ctimeNSFromInfo(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ctim.Nano()
}
