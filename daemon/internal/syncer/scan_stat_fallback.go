//go:build !linux && !openbsd && !dragonfly && !aix && !solaris && !darwin && !freebsd && !netbsd

package syncer

import "os"

func fileKeyFromInfo(info os.FileInfo) (string, bool) {
	return "", false
}

func ctimeNSFromInfo(info os.FileInfo) int64 {
	return 0
}
