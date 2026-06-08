package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestRootTreeUpsertMoveAndTombstoneUseDocumentIDIdentity(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()

	if update, err := UpsertRootFile(doc, "doc_1", "docs/old.md", rootMutationActor{ID: "agent", Kind: "daemon"}); err != nil || len(update) == 0 {
		t.Fatalf("upsert old: update=%d err=%v", len(update), err)
	}
	tree, err := DecodeRootTree(doc)
	if err != nil {
		t.Fatalf("decode root tree: %v", err)
	}
	if got, ok := ResolveRootPath(tree, "docs/old.md"); !ok || got != "doc_1" {
		t.Fatalf("old path resolved to %q ok=%v", got, ok)
	}

	if update, err := UpsertRootFile(doc, "doc_1", "docs/new.md", rootMutationActor{ID: "agent", Kind: "daemon"}); err != nil || len(update) == 0 {
		t.Fatalf("move: update=%d err=%v", len(update), err)
	}
	tree, err = DecodeRootTree(doc)
	if err != nil {
		t.Fatalf("decode moved root tree: %v", err)
	}
	files := ListVisibleRootFiles(tree)
	if len(files) != 1 {
		t.Fatalf("visible files after move = %#v", files)
	}
	if files[0].ContentDocumentID != "doc_1" || files[0].DesiredPath != "docs/new.md" {
		t.Fatalf("visible file after move = %#v", files[0])
	}
	if _, ok := ResolveRootPath(tree, "docs/old.md"); ok {
		t.Fatal("old path should not resolve after moving same document id")
	}

	if update, err := TombstoneRootFile(doc, "doc_1", rootMutationActor{ID: "agent", Kind: "daemon"}); err != nil || len(update) == 0 {
		t.Fatalf("tombstone: update=%d err=%v", len(update), err)
	}
	tree, err = DecodeRootTree(doc)
	if err != nil {
		t.Fatalf("decode tombstoned root tree: %v", err)
	}
	if files := ListVisibleRootFiles(tree); len(files) != 0 {
		t.Fatalf("tombstoned entry should not be visible: %#v", files)
	}
}

func TestRootTreeDuplicateDesiredPathsStaySeparate(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()

	if _, err := UpsertRootFile(doc, "doc_a", "docs/same.md", rootMutationActor{}); err != nil {
		t.Fatalf("upsert doc_a: %v", err)
	}
	if _, err := UpsertRootFile(doc, "doc_b", "docs/same.md", rootMutationActor{}); err != nil {
		t.Fatalf("upsert doc_b: %v", err)
	}
	tree, err := DecodeRootTree(doc)
	if err != nil {
		t.Fatalf("decode root tree: %v", err)
	}
	files := ListVisibleRootFiles(tree)
	if len(files) != 2 {
		t.Fatalf("visible files = %#v", files)
	}
	if _, ok := ResolveRootPath(tree, "docs/same.md"); ok {
		t.Fatal("duplicate desired path should be ambiguous, not merged")
	}
	plan := PlanRootProjection(nil, tree, 11)
	byDoc := map[string]rootProjectionEntry{}
	for _, entry := range plan.Next {
		byDoc[entry.ContentDocumentID] = entry
	}
	if got := byDoc["doc_a"].MaterializedPath; got != "docs/same.md" {
		t.Fatalf("doc_a materialized path = %q", got)
	}
	if got := byDoc["doc_b"].MaterializedPath; got != "docs/same (doc_b).md" {
		t.Fatalf("doc_b materialized path = %q", got)
	}
}

func TestRootTreeRejectsInvalidVisiblePaths(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()

	for _, path := range []string{"", "../outside.md", "/abs.md", ".notty/root", "docs/.secret.md"} {
		if _, err := UpsertRootFile(doc, "doc_1", path, rootMutationActor{}); !errors.Is(err, errInvalidRootPath) {
			t.Fatalf("UpsertRootFile(%q) err=%v, want errInvalidRootPath", path, err)
		}
	}
	if _, err := UpsertRootFile(doc, "doc_1", "docs//ok.md", rootMutationActor{}); err != nil {
		t.Fatalf("valid normalized path rejected: %v", err)
	}
	tree, err := DecodeRootTree(doc)
	if err != nil {
		t.Fatalf("decode root tree: %v", err)
	}
	if got, ok := ResolveRootPath(tree, "docs/ok.md"); !ok || got != "doc_1" {
		t.Fatalf("normalized path resolved to %q ok=%v", got, ok)
	}
}

func TestPlanRootProjectionSeparatesCreateDeleteAndConflict(t *testing.T) {
	tree := RootTree{Entries: map[string]rootEntry{
		"doc_a": {
			EntryID:           "doc_a",
			Kind:              rootEntryKindFile,
			ContentDocumentID: "doc_a",
			Name:              "docs/same.md",
		},
		"doc_b": {
			EntryID:           "doc_b",
			Kind:              rootEntryKindFile,
			ContentDocumentID: "doc_b",
			Name:              "docs/same.md",
		},
		"doc_old": {
			EntryID:           "doc_old",
			Kind:              rootEntryKindFile,
			ContentDocumentID: "doc_old",
			Name:              "docs/old.md",
			Deleted:           true,
		},
	}}
	previous := map[string]rootProjectionEntry{
		"doc_old": {
			EntryID:           "doc_old",
			ContentDocumentID: "doc_old",
			DesiredPath:       "docs/old.md",
			MaterializedPath:  "docs/old.md",
			Active:            true,
		},
	}

	plan := PlanRootProjection(previous, tree, 42)
	if len(plan.Removed) != 1 || plan.Removed[0].ContentDocumentID != "doc_old" {
		t.Fatalf("removed projection entries = %#v", plan.Removed)
	}
	if len(plan.Upserts) != 2 {
		t.Fatalf("upsert projection entries = %#v", plan.Upserts)
	}
	byDoc := map[string]rootProjectionEntry{}
	for _, entry := range plan.Upserts {
		byDoc[entry.ContentDocumentID] = entry
		if entry.ProjectedSeq != 42 {
			t.Fatalf("projected seq = %d, want 42", entry.ProjectedSeq)
		}
	}
	if byDoc["doc_a"].MaterializedPath != "docs/same.md" || byDoc["doc_b"].MaterializedPath != "docs/same (doc_b).md" {
		t.Fatalf("conflict materialization = %#v", byDoc)
	}
}

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

	if err := service.reconcileDocumentIDs(context.Background(), []string{rootID}); err != nil {
		t.Fatalf("reconcile root: %v", err)
	}
	waitUntil(t, func() bool {
		pending, err := cache.pendingRemoteUpdateCount(rootID)
		return err == nil && pending == 0
	})
}
