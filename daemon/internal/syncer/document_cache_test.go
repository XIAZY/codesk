package syncer

import (
	"context"
	"os"
	"testing"

	"github.com/reearth/ygo/crdt"
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
	if materialized.Content != "alpha" {
		t.Fatalf("unexpected cached content: %q", materialized.Content)
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
		firstText.Insert(txn, firstText.Len(), " local", nil)
	}, "first")
	if got := second.Doc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("second materialized doc observed first doc mutation: %q", got)
	}
}

func TestDocumentCacheReportsUnknownContentWithoutCacheOrBootstrapState(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	materialized, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("materialize uncached doc: %v", err)
	}
	if materialized.ContentKnown {
		t.Fatal("expected uncached metadata-only document content to be unknown")
	}
	if got := materialized.Doc.GetText("content").ToString(); got != "" {
		t.Fatalf("expected unknown document to start empty locally, got %q", got)
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
	if !next.ContentKnown || next.Content != "after websocket sync" {
		t.Fatalf("expected websocket-populated cache to be known, known=%v content=%q", next.ContentKnown, next.Content)
	}
}

func TestDocumentCacheMaybeStoreDocCheckpointsInsteadOfEveryUpdate(t *testing.T) {
	root := t.TempDir()
	cache, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	doc := newDocWithText(t, "alpha")
	if err := cache.maybeStoreDoc("doc_1", "docs/spec.md", 1, doc); err != nil {
		t.Fatalf("initial cache store: %v", err)
	}

	for i := 0; i < documentCachePersistEveryUpdates-1; i++ {
		appendText(t, doc, "x")
		if err := cache.maybeStoreDoc("doc_1", "docs/spec.md", int64(i+2), doc); err != nil {
			t.Fatalf("skip cache store %d: %v", i, err)
		}
	}
	reopened, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("reopen cache before checkpoint: %v", err)
	}
	beforeCheckpoint, err := reopened.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("materialize before checkpoint: %v", err)
	}
	if beforeCheckpoint.Content != "alpha" {
		t.Fatalf("expected disk cache to remain at last checkpoint, got %q", beforeCheckpoint.Content)
	}

	appendText(t, doc, "x")
	if err := cache.maybeStoreDoc("doc_1", "docs/spec.md", documentCachePersistEveryUpdates+1, doc); err != nil {
		t.Fatalf("checkpoint cache store: %v", err)
	}
	reopened, err = newDocumentCache(root)
	if err != nil {
		t.Fatalf("reopen cache after checkpoint: %v", err)
	}
	afterCheckpoint, err := reopened.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("materialize after checkpoint: %v", err)
	}
	if afterCheckpoint.Content != doc.GetText("content").ToString() {
		t.Fatalf("expected disk cache checkpoint to catch up, got %q", afterCheckpoint.Content)
	}
}

func appendText(t *testing.T, doc *crdt.Doc, value string) {
	t.Helper()
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), value, nil)
	}, "test")
}
