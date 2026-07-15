//go:build !windows

package syncer

import (
	"os"
	"syscall"
)

func fileIdentityForPath(path string) fileIdentity {
	_, identity, err := statFileWithIdentity(path)
	if err != nil {
		return fileIdentity{}
	}
	return identity
}

func statFileWithIdentity(path string) (os.FileInfo, fileIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	return info, fileIdentityFromFileInfo(info), nil
}

func fileIdentityFromFileInfo(info os.FileInfo) fileIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileIdentity{}
	}
	return fileIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino), valid: stat.Ino != 0}
}
