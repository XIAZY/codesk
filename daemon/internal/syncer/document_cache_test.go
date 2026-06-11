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

func TestDocumentCacheFoldsPendingRemoteUpdatesOnceAfterReopen(t *testing.T) {
	root := t.TempDir()
	cache, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}

	remoteDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(remoteDoc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base to remote doc: %v", err)
	}
	updates := map[string][]byte{}
	unsubscribe := remoteDoc.OnUpdate(func(update []byte, origin any) {
		key, _ := origin.(string)
		if key == "remote1" || key == "remote2" {
			updates[key] = append([]byte(nil), update...)
		}
	})
	updateDocText(t, remoteDoc, "base\nremote one", "remote1")
	updateDocText(t, remoteDoc, "base\nremote one\nremote two", "remote2")
	unsubscribe()
	for _, key := range []string{"remote1", "remote2"} {
		if len(updates[key]) == 0 {
			t.Fatalf("expected captured update %s", key)
		}
		appended, err := cache.appendPendingRemoteUpdate("doc_1", "docs/spec.md", updates[key])
		if err != nil {
			t.Fatalf("append pending update %s: %v", key, err)
		}
		if !appended {
			t.Fatalf("expected pending update %s to append", key)
		}
	}
	if err := cache.db.Close(); err != nil {
		t.Fatalf("close cache: %v", err)
	}

	reopened, err := newDocumentCache(root)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	count, err := reopened.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count after reopen: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected two durable pending updates after reopen, got %d", count)
	}

	doc, _, _, err := reopened.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load base doc: %v", err)
	}
	entry := reopened.entryFor("doc_1")
	entry.mu.Lock()
	applied, projectedSeq, err := reopened.applyPendingRemoteUpdatesLocked(entry, "doc_1", doc)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply pending updates: %v", err)
	}
	if applied != 2 {
		t.Fatalf("expected two applied pending updates, got %d", applied)
	}
	if projectedSeq == 0 {
		t.Fatal("expected apply pending updates to return folded projected seq")
	}
	firstProjectedSeq := projectedSeq

	count, err = reopened.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count after apply: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected pending inbox to be empty after apply, got %d", count)
	}
	gotDoc, _, _, err := reopened.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("reload folded doc: %v", err)
	}
	if got := gotDoc.GetText("content").ToString(); got != "base\nremote one\nremote two" {
		t.Fatalf("unexpected folded content: %q", got)
	}

	rows, err := reopened.db.Query(`select update_sha256 from crdt_updates where document_id = ? and source = 'remote' order by seq`, "doc_1")
	if err != nil {
		t.Fatalf("load folded remote hashes: %v", err)
	}
	defer rows.Close()
	var foldedHashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("scan folded remote hash: %v", err)
		}
		foldedHashes = append(foldedHashes, hash)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate folded remote hashes: %v", err)
	}
	wantHashes := []string{sha256Hex(updates["remote1"]), sha256Hex(updates["remote2"])}
	if !reflect.DeepEqual(foldedHashes, wantHashes) {
		t.Fatalf("folded remote hashes = %#v, want %#v", foldedHashes, wantHashes)
	}

	doc, _, _, err = reopened.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("reload doc before no-op apply: %v", err)
	}
	entry.mu.Lock()
	applied, projectedSeq, err = reopened.applyPendingRemoteUpdatesLocked(entry, "doc_1", doc)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("second apply pending updates: %v", err)
	}
	if applied != 0 {
		t.Fatalf("expected second apply to be no-op, got %d", applied)
	}
	if projectedSeq == 0 {
		t.Fatal("expected no-op apply to return current applied seq")
	}
	if projectedSeq != firstProjectedSeq {
		t.Fatalf("second apply returned projected seq %d, want unchanged %d", projectedSeq, firstProjectedSeq)
	}
	var foldedCount int
	if err := reopened.db.QueryRow(`select count(*) from crdt_updates where document_id = ?`, "doc_1").Scan(&foldedCount); err != nil {
		t.Fatalf("count folded updates: %v", err)
	}
	if foldedCount != 3 {
		t.Fatalf("second apply must not duplicate folded updates, got %d rows", foldedCount)
	}
}

func TestWorkspaceStoreStoreProjectedBaseUsesExplicitSeq(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	baseRow, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load base row: %v", err)
	}
	baseSeq := baseRow.AppliedSeq
	baseState := baseDoc.EncodeStateAsUpdate()

	remoteDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(remoteDoc, baseState, "base"); err != nil {
		t.Fatalf("apply base to remote doc: %v", err)
	}
	var remoteUpdate []byte
	unsubscribe := remoteDoc.OnUpdate(func(update []byte, origin any) {
		if origin == "remote" {
			remoteUpdate = append([]byte(nil), update...)
		}
	})
	updateDocText(t, remoteDoc, "base\nremote", "remote")
	unsubscribe()
	if len(remoteUpdate) == 0 {
		t.Fatal("expected remote update")
	}
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "docs/spec.md", remoteUpdate); err != nil {
		t.Fatalf("append remote update: %v", err)
	}
	doc, _, _, err := cache.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load base doc: %v", err)
	}
	entry := cache.entryFor("doc_1")
	entry.mu.Lock()
	applied, remoteSeq, err := cache.applyPendingRemoteUpdatesLocked(entry, "doc_1", doc)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply remote update: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected one remote update, got %d", applied)
	}
	if remoteSeq <= baseSeq {
		t.Fatalf("expected remote seq %d to be after base seq %d", remoteSeq, baseSeq)
	}

	if err := cache.storeProjectedBase("doc_1", "base", baseState, baseSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.AppliedSeq != remoteSeq {
		t.Fatalf("store projected base changed applied seq to %d, want %d", row.AppliedSeq, remoteSeq)
	}
	if row.ProjectedSeq != baseSeq {
		t.Fatalf("projected seq = %d, want explicit base seq %d", row.ProjectedSeq, baseSeq)
	}
	projected, _, known, err := cache.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projected != "base" {
		t.Fatalf("projected base = known %v content %q, want base", known, projected)
	}
}

func TestWorkspaceStoreApplyDuplicateOutboxUpdateReturnsExistingSeq(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	baseRow, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load base row: %v", err)
	}
	update := updateFromBaseDoc(t, baseDoc, "base\nlocal\n", "local")
	record := &outboxUpdateRecord{Update: update, ActorID: "agent_1", ActorType: "agent"}

	entry := cache.entryFor("doc_1")
	entry.mu.Lock()
	firstSeq, err := cache.applyOutboxUpdateLocked(entry, "doc_1", "docs/spec.md", record)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply first outbox update: %v", err)
	}
	if firstSeq <= baseRow.AppliedSeq {
		t.Fatalf("first local seq %d must be after base seq %d", firstSeq, baseRow.AppliedSeq)
	}

	entry.mu.Lock()
	duplicateSeq, err := cache.applyOutboxUpdateLocked(entry, "doc_1", "docs/spec.md", record)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply duplicate outbox update: %v", err)
	}
	if duplicateSeq != firstSeq {
		t.Fatalf("duplicate local update seq = %d, want existing seq %d", duplicateSeq, firstSeq)
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.AppliedSeq != firstSeq {
		t.Fatalf("applied seq = %d, want %d after duplicate", row.AppliedSeq, firstSeq)
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
	want := []string{"content_outbox", "crdt_updates", "documents", "incoming_updates", "root_projection_entries", "thread_outbox"}
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
