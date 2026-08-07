package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceReplicaRealWatcherDeliversCreateWriteRenameDelete(t *testing.T) {
	root := t.TempDir()
	replica := newWorkspaceReplica(Config{}, root, "daemon_agent", "daemon", nil, nil)

	writePath := filepath.Join(root, "write.md")
	renameOldPath := filepath.Join(root, "rename-old.md")
	renameNewPath := filepath.Join(root, "rename-new.md")
	deletePath := filepath.Join(root, "delete.md")

	writeTracked := seedRealWatcherTrackedFile(t, replica, "doc_write", writePath, "before\n")
	seedRealWatcherTrackedFile(t, replica, "doc_rename", renameOldPath, "rename\n")
	seedRealWatcherTrackedFile(t, replica, "doc_delete", deletePath, "delete\n")

	// Full reconciliation is deliberately inert: only events delivered through
	// the real watcher pump and handler can populate the assertions below.
	replica.reconcile = func(context.Context) error { return nil }
	startRealWatcherReplica(t, replica)
	replica.watchMu.Lock()
	watcher := replica.watcher
	replica.watchMu.Unlock()
	_, usesFSNotify := watcher.(*fsnotifyWorkspaceWatcher)
	if !usesFSNotify {
		t.Fatalf("workspace watcher type = %T, want real fsnotify backend", watcher)
	}

	createPath := filepath.Join(root, "create.md")
	if err := os.WriteFile(createPath, []byte("create\n"), 0o644); err != nil {
		t.Fatalf("create file: %v", err)
	}
	waitForRealWatcherState(t, "create event", func() bool {
		replica.changes.mu.Lock()
		defer replica.changes.mu.Unlock()
		created, ok := replica.changes.creates[createPath]
		return ok && created.candidate.Path == createPath && created.identity.valid
	})

	if err := os.WriteFile(writePath, []byte("after\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	waitForRealWatcherState(t, "write event", func() bool {
		if !writeTracked.isLocalDirty() {
			return false
		}
		replica.changes.mu.Lock()
		defer replica.changes.mu.Unlock()
		_, ok := replica.changes.dirtyDocuments[writeTracked.DocumentID]
		return ok
	})

	if err := os.Rename(renameOldPath, renameNewPath); err != nil {
		t.Fatalf("rename tracked file: %v", err)
	}
	waitForRealWatcherState(t, "rename events", func() bool {
		replica.changes.mu.Lock()
		defer replica.changes.mu.Unlock()
		missing, missingOK := replica.changes.missing["doc_rename"]
		created, createdOK := replica.changes.creates[renameNewPath]
		return missingOK && missing.path == renameOldPath && createdOK &&
			created.candidate.Path == renameNewPath &&
			sameFileIdentity(missing.identity, created.identity)
	})

	if err := os.Remove(deletePath); err != nil {
		t.Fatalf("delete tracked file: %v", err)
	}
	waitForRealWatcherState(t, "delete event", func() bool {
		replica.changes.mu.Lock()
		defer replica.changes.mu.Unlock()
		missing, ok := replica.changes.missing["doc_delete"]
		return ok && missing.path == deletePath && missing.identity.valid
	})
}

func seedRealWatcherTrackedFile(
	t *testing.T,
	replica *workspaceReplica,
	documentID, path, content string,
) *trackedFile {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed tracked file %s: %v", path, err)
	}
	tracked := &trackedFile{
		DocumentID:    documentID,
		DocumentPath:  filepath.Base(path),
		Path:          path,
		WorkspaceRoot: replica.rootDir,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent(content)
	replica.projectedByPath[path] = tracked
	replica.projectedByID[documentID] = tracked
	replica.recordTrackedIdentity(path)
	return tracked
}

func startRealWatcherReplica(t *testing.T, replica *workspaceReplica) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- replica.run(ctx, ready) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop real workspace watcher: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("real workspace watcher did not stop")
		}
	})

	select {
	case err := <-ready:
		if err != nil {
			cancel()
			t.Fatalf("start real workspace watcher: %v", err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("real workspace watcher did not reach its ready barrier")
	}
}

func waitForRealWatcherState(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("real workspace watcher did not deliver %s", description)
		}
	}
}
