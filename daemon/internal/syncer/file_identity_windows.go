//go:build windows

package syncer

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func fileIdentityForPath(path string) fileIdentity {
	_, identity, err := statFileWithIdentity(path)
	if err != nil {
		return fileIdentity{}
	}
	return identity
}

func statFileWithIdentity(path string) (os.FileInfo, fileIdentity, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fileIdentity{}, fmt.Errorf("stat file %q: invalid Windows handle", path)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fileIdentity{}, err
	}

	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		return nil, fileIdentity{}, err
	}
	return info, fileIdentityFromWindowsInfo(handleInfo), nil
}

func fileIdentityFromWindowsInfo(info windows.ByHandleFileInformation) fileIdentity {
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fileIdentity{dev: uint64(info.VolumeSerialNumber), ino: fileIndex, valid: fileIndex != 0}
}
