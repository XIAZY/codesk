//go:build windows

package syncer

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestIsCrossDeviceErrorWindows(t *testing.T) {
	if !isCrossDeviceError(fmt.Errorf("rename: %w", windows.ERROR_NOT_SAME_DEVICE)) {
		t.Fatal("wrapped ERROR_NOT_SAME_DEVICE should be classified as cross-device")
	}
	if isCrossDeviceError(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("ERROR_ACCESS_DENIED should not be classified as cross-device")
	}
}
