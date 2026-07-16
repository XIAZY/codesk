//go:build windows

package winlock

import (
	"path/filepath"
	"testing"
)

func TestFileLockExcludesSamePathUntilRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "desktop.lock")
	first := New(path)
	acquired, err := first.Acquire()
	if err != nil || !acquired {
		t.Fatalf("first Acquire() = (%t, %v), want (true, nil)", acquired, err)
	}
	defer first.Release()

	second := New(path)
	acquired, err = second.Acquire()
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	if acquired {
		t.Fatal("second Acquire() unexpectedly acquired the same file")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("first Release() error = %v", err)
	}
	acquired, err = second.Acquire()
	if err != nil || !acquired {
		t.Fatalf("Acquire() after release = (%t, %v), want (true, nil)", acquired, err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second Release() error = %v", err)
	}
}
