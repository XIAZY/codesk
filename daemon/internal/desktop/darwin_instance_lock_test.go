//go:build darwin

package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinInstanceLockExcludesSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Locks", "desktop.lock")
	first, err := NewDarwinInstanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDarwinInstanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := first.Acquire()
	if err != nil || !acquired {
		t.Fatalf("first Acquire() = %t, %v", acquired, err)
	}
	defer first.Release()
	acquired, err = second.Acquire()
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	if acquired {
		t.Fatal("second Acquire() unexpectedly acquired the held lock")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	acquired, err = second.Acquire()
	if err != nil || !acquired {
		t.Fatalf("second Acquire() after release = %t, %v", acquired, err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinInstanceLockRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real.lock")
	if err := os.WriteFile(realPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "desktop.lock")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	lock, err := NewDarwinInstanceLock(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if acquired, err := lock.Acquire(); err == nil || acquired {
		t.Fatalf("Acquire() = %t, %v; want symlink rejection", acquired, err)
	}
}
