//go:build windows

package syncer

import "golang.org/x/sys/windows"

func commitReplacement(stagedPath, targetPath string) error {
	stagedPtr, err := windows.UTF16PtrFromString(stagedPath)
	if err != nil {
		return err
	}
	targetPtr, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		stagedPtr,
		targetPtr,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
