//go:build !windows

package syncer

import (
	"fmt"
	"syscall"
	"testing"
)

func TestIsCrossDeviceErrorUnix(t *testing.T) {
	if !isCrossDeviceError(fmt.Errorf("rename: %w", syscall.EXDEV)) {
		t.Fatal("wrapped EXDEV should be classified as cross-device")
	}
	if isCrossDeviceError(syscall.EPERM) {
		t.Fatal("EPERM should not be classified as cross-device")
	}
}
