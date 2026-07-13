//go:build windows

package syncer

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileRenameInfo mirrors FILE_RENAME_INFO for FileRenameInfoEx.
type fileRenameInfo struct {
	flags          uint32
	rootDirectory  windows.Handle
	fileNameLength uint32
	fileName       [1]uint16
}

func commitReplacement(stagedPath, targetPath string) error {
	stagedPtr, err := windows.UTF16PtrFromString(stagedPath)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		stagedPtr,
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	renameInfo, err := makeFileRenameInfo(targetPath)
	if err != nil {
		return err
	}
	return windows.SetFileInformationByHandle(
		handle,
		windows.FileRenameInfoEx,
		&renameInfo[0],
		uint32(len(renameInfo)),
	)
}

func makeFileRenameInfo(targetPath string) ([]byte, error) {
	targetName, err := windows.UTF16FromString(targetPath)
	if err != nil {
		return nil, err
	}
	var header fileRenameInfo
	// FileNameLength excludes the UTF-16 terminator, but the buffer must include it.
	bufferSize := int(unsafe.Offsetof(header.fileName)) + len(targetName)*2
	buffer := make([]byte, bufferSize)
	info := (*fileRenameInfo)(unsafe.Pointer(&buffer[0]))
	info.flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.fileNameLength = uint32((len(targetName) - 1) * 2)
	copy(unsafe.Slice(&info.fileName[0], len(targetName)), targetName)
	return buffer, nil
}
