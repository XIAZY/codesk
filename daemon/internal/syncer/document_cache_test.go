package syncer

import (
	"context"
	"reflect"
	"strings"
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
	assertSQLiteTableExists(t, cache, "crdt_updates")
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
		t.Fatal("missing CRDT update rows must not be treated as materialized document content")
	}
	if got := materialized.Doc.GetText("content").ToString(); got != "" {
		t.Fatalf("expected empty placeholder doc, got %q", got)
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
	updateDoc := newDocWithText(t, "remote")
	update := updateDoc.EncodeStateAsUpdate()
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

func TestDocumentCacheDoesNotCreateFileBackedState(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, newDocWithText(t, "cached")); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}

	doc, _, state, err := cache.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load cached state: %v", err)
	}
	if len(state) == 0 {
		t.Fatal("expected sqlite-backed state")
	}
	if got := doc.GetText("content").ToString(); got != "cached" {
		t.Fatalf("expected cached doc, got %q", got)
	}
}

func TestWorkspaceStoreSchemaUsesAgreedDurableTables(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	rows, err := cache.db.Query(`select name from sqlite_master where type = 'table' and name not like 'sqlite_%' order by name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, table)
	}
	want := []string{"content_outbox", "crdt_updates", "documents", "incoming_updates", "thread_outbox"}
	if !reflect.DeepEqual(tables, want) {
		t.Fatalf("sqlite schema tables = %#v, want %#v", tables, want)
	}
	var seqSQL string
	if err := cache.db.QueryRow(`select sql from sqlite_master where type = 'table' and name = 'crdt_updates'`).Scan(&seqSQL); err != nil {
		t.Fatalf("load crdt schema: %v", err)
	}
	if !strings.Contains(strings.ToLower(seqSQL), "seq integer primary key autoincrement") {
		t.Fatalf("crdt_updates must use autoincrement seq, schema=%s", seqSQL)
	}
	if strings.Contains(strings.ToLower(seqSQL), "backend_update_id") {
		t.Fatalf("crdt_updates must not persist backend_update_id, schema=%s", seqSQL)
	}
	var documentsSQL string
	if err := cache.db.QueryRow(`select sql from sqlite_master where type = 'table' and name = 'documents'`).Scan(&documentsSQL); err != nil {
		t.Fatalf("load documents schema: %v", err)
	}
	if strings.Contains(strings.ToLower(documentsSQL), "backend_update_id") {
		t.Fatalf("documents must not persist backend_update_id, schema=%s", documentsSQL)
	}
}

func assertSQLiteTableExists(t *testing.T, cache *documentCache, table string) {
	t.Helper()
	var name string
	if err := cache.db.QueryRow(`select name from sqlite_master where type = 'table' and name = ?`, table).Scan(&name); err != nil {
		t.Fatalf("expected sqlite table %s: %v", table, err)
	}
}
