package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestAllocateRootProjectionEntriesUsesDeterministicConflictPaths(t *testing.T) {
	entries := map[string]rootEntry{
		"entry_b": {
			EntryID:           "entry_b",
			Kind:              rootEntryKindFile,
			ContentDocumentID: "doc_b",
			Name:              "docs/same.md",
		},
		"entry_a": {
			EntryID:           "entry_a",
			Kind:              rootEntryKindFile,
			ContentDocumentID: "doc_a",
			Name:              "docs/same.md",
		},
	}

	projected := allocateRootProjectionEntries(entries, 7)
	if len(projected) != 2 {
		t.Fatalf("projected entries = %#v", projected)
	}
	byDoc := map[string]rootProjectionEntry{}
	for _, entry := range projected {
		byDoc[entry.ContentDocumentID] = entry
	}
	if got := byDoc["doc_a"].MaterializedPath; got != "docs/same.md" {
		t.Fatalf("doc_a materialized path = %q", got)
	}
	if got := byDoc["doc_b"].MaterializedPath; got != "docs/same (doc_b).md" {
		t.Fatalf("doc_b conflict materialized path = %q", got)
	}
}

func TestRootLocalMoveSendsBeforeApplyingPendingIncomingRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "local.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
		t.Fatalf("write moved file: %v", err)
	}
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	rootID := "doc_root_test"
	seedRoot := crdt.New()
	seedMap := seedRoot.GetMap(rootMapName)
	seedUpdate, err := seedRoot.Update(func(txn *crdt.Transaction) error {
		entriesMap, err := seedMap.SetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		return setRootFileEntry(txn, entriesMap, rootEntry{
			EntryID:           rootEntryIDForDocument("doc_1"),
			ContentDocumentID: "doc_1",
			Name:              "docs/old.md",
		})
	}, "seed-root")
	if err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := cache.storeDoc(rootID, rootDocumentPath, 1, seedRoot); err != nil {
		t.Fatalf("store root: %v", err)
	}
	remoteRoot := crdt.New()
	if err := crdt.ApplyUpdateV1(remoteRoot, seedUpdate, "seed"); err != nil {
		t.Fatalf("apply remote seed: %v", err)
	}
	remoteMap := remoteRoot.GetMap(rootMapName)
	remoteUpdate, err := remoteRoot.Update(func(txn *crdt.Transaction) error {
		entriesMap, ok, err := remoteMap.GetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing root entries")
		}
		return setRootFileEntry(txn, entriesMap, rootEntry{
			EntryID:           rootEntryIDForDocument("doc_1"),
			ContentDocumentID: "doc_1",
			Name:              "docs/remote.md",
		})
	}, "remote-root")
	if err != nil {
		t.Fatalf("remote root update: %v", err)
	}
	if _, err := cache.appendPendingRemoteUpdate(rootID, rootDocumentPath, remoteUpdate); err != nil {
		t.Fatalf("append pending root: %v", err)
	}

	baseDoc := newDocWithText(t, "same")
	if err := cache.storeDoc("doc_1", "docs/old.md", 1, baseDoc); err != nil {
		t.Fatalf("store content doc: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/old.md",
		Path:          path,
		WorkspaceRoot: root,
		cache:         cache,
	}
	tracked.setProjectedContent("same")
	if err := tracked.storeProjectedBase("same", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalMoved()

	service := &workspaceRuntime{
		cfg:            Config{BackendURL: "http://backend.test", AgentID: "daemon_agent"},
		docCache:       cache,
		rootDocumentID: rootID,
	}
	var pendingAtSend int
	var sentPath string
	service.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		if documentID != rootID {
			t.Fatalf("unexpected document update: %s", documentID)
		}
		var countErr error
		pendingAtSend, countErr = cache.pendingRemoteUpdateCount(rootID)
		if countErr != nil {
			t.Fatalf("pending root count: %v", countErr)
		}
		serverRoot := crdt.New()
		if err := crdt.ApplyUpdateV1(serverRoot, seedUpdate, "seed"); err != nil {
			t.Fatalf("server seed apply: %v", err)
		}
		if err := crdt.ApplyUpdateV1(serverRoot, record.Update, "local-root"); err != nil {
			t.Fatalf("server local apply: %v", err)
		}
		entries, err := decodeRootEntries(serverRoot)
		if err != nil {
			t.Fatalf("decode sent root: %v", err)
		}
		sentPath = entries[rootEntryIDForDocument("doc_1")].desiredPath()
		return cache.clearOutboxUpdates(documentID)
	}

	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile local move: %v", err)
	}
	if pendingAtSend != 1 {
		t.Fatalf("pending root updates at send = %d, want 1", pendingAtSend)
	}
	if sentPath != "docs/local.md" {
		t.Fatalf("sent root path = %q, want docs/local.md", sentPath)
	}
	pending, err := cache.pendingRemoteUpdateCount(rootID)
	if err != nil {
		t.Fatalf("pending root count after move: %v", err)
	}
	if pending != 1 {
		t.Fatalf("local move should not apply pending incoming root, got %d pending", pending)
	}

	service.workspaceDocuments = []*document{{ID: "doc_1", Path: "docs/old.md", UpdateID: 1}}
	if err := service.reconcileDocumentIDs(context.Background(), []string{rootID}); err != nil {
		t.Fatalf("reconcile root: %v", err)
	}
	waitUntil(t, func() bool {
		pending, err := cache.pendingRemoteUpdateCount(rootID)
		return err == nil && pending == 0
	})
}
