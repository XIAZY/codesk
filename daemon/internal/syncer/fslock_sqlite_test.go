package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSLockDBSerializesFilesystemOperations(t *testing.T) {
	root := t.TempDir()
	lock, err := OpenFSLockDB(root, "test-holder")
	if err != nil {
		t.Fatalf("open fs lock: %v", err)
	}
	defer lock.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- lock.WithFilesystemLock(context.Background(), "first", "a", "", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- lock.WithFilesystemLock(context.Background(), "second", "b", "", func() error {
			return nil
		})
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second lock completed before first released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first lock did not finish")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second lock did not finish after release")
	}
}

func TestWorkspaceFSUsesSQLiteFSLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	fs := NewWorkspaceFS(root)
	if fs.Locks == nil {
		t.Fatal("expected workspace fs to initialize fslock.sqlite")
	}
	if err := fs.WriteIfUnchanged(path, projectedContentHash{}, []byte("content")); err != nil {
		t.Fatalf("write with workspace fs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".notty", "fslock.sqlite")); err != nil {
		t.Fatalf("expected fslock.sqlite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".notty", "locks")); !os.IsNotExist(err) {
		t.Fatalf("legacy lock directory should not be used, stat err=%v", err)
	}
}
