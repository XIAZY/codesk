package syncer

import (
	"context"
	"os"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestDocumentCacheMaterializesCachedStateWithoutBackendFetch(t *testing.T) {
	root := t.TempDir()

	cache, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	cachedDoc := newDocWithText(t, "alpha")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 7, cachedDoc); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}

	cache, err = newDocumentCache(root)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	materialized, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md", UpdateID: 8})
	if err != nil {
		t.Fatalf("materialize cached doc: %v", err)
	}
	if !materialized.ContentKnown {
		t.Fatal("expected cached document content to be known")
	}
	if got := materialized.Doc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("unexpected cached content: %q", got)
	}
	if _, err := os.Stat(cache.statePath("doc_1")); err != nil {
		t.Fatalf("expected cache state on disk: %v", err)
	}
}

func TestDocumentCacheMaterializesIndependentMutableDocs(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, newDocWithText(t, "alpha")); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}

	first, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("materialize first doc: %v", err)
	}
	second, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("materialize second doc: %v", err)
	}
	if first.Doc == second.Doc {
		t.Fatal("expected separate CRDT doc instances per materialization")
	}
	firstText := first.Doc.GetText("content")
	first.Doc.Transact(func(txn *crdt.Transaction) {
		firstText.Insert(txn, firstText.LenInTxn(txn), " local", nil)
	}, "first")
	if got := second.Doc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("second materialized doc observed first doc mutation: %q", got)
	}
}

func TestDocumentCacheReportsUnknownContentWithoutCacheState(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	materialized, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("materialize missing state: %v", err)
	}
	if materialized.ContentKnown {
		t.Fatal("missing state.bin must not be treated as materialized document content")
	}
	if got := materialized.Doc.GetText("content").ToString(); got != "" {
		t.Fatalf("expected empty placeholder doc, got %q", got)
	}
	if _, err := os.Stat(cache.statePath("doc_1")); !os.IsNotExist(err) {
		t.Fatalf("missing state must not initialize state.bin, stat err=%v", err)
	}

	text := materialized.Doc.GetText("content")
	materialized.Doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "after websocket sync", nil)
	}, "remote")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, materialized.Doc); err != nil {
		t.Fatalf("store synced doc: %v", err)
	}
	next, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md", UpdateID: 1})
	if err != nil {
		t.Fatalf("rematerialize synced doc: %v", err)
	}
	if got := next.Doc.GetText("content").ToString(); !next.ContentKnown || got != "after websocket sync" {
		t.Fatalf("expected websocket-populated cache to be known, known=%v content=%q", next.ContentKnown, got)
	}
}

func TestDocumentCacheDedupesPendingRemoteUpdatesAfterReopen(t *testing.T) {
	root := t.TempDir()
	cache, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	update := []byte{1, 2, 3, 4}
	appended, err := cache.appendPendingRemoteUpdate("doc_1", "docs/spec.md", update)
	if err != nil {
		t.Fatalf("append first pending update: %v", err)
	}
	if !appended {
		t.Fatal("expected first pending update to append")
	}

	reopened, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	appended, err = reopened.appendPendingRemoteUpdate("doc_1", "docs/spec.md", update)
	if err != nil {
		t.Fatalf("append duplicate pending update after reopen: %v", err)
	}
	if appended {
		t.Fatal("expected duplicate pending update after reopen to be ignored")
	}
	count, err := reopened.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one pending update after reopen, got %d", count)
	}
}

func TestDocumentCacheDropsCorruptCachedState(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, newDocWithText(t, "cached")); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}
	if err := os.WriteFile(cache.statePath("doc_1"), []byte("not a crdt update"), 0o644); err != nil {
		t.Fatalf("corrupt cached state: %v", err)
	}

	doc, _, state, err := cache.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load corrupt cached state: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("expected corrupt state to be dropped, got %d bytes", len(state))
	}
	if got := doc.GetText("content").ToString(); got != "" {
		t.Fatalf("expected empty doc after dropping corrupt cache, got %q", got)
	}
	if _, err := os.Stat(cache.statePath("doc_1")); !os.IsNotExist(err) {
		t.Fatalf("expected corrupt state file to be removed, stat err=%v", err)
	}
}
