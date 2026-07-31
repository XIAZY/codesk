package syncer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestDocumentCacheMaterializesCachedStateWithoutBackendFetch(t *testing.T) {
	root := t.TempDir()

	cache, err := newTestDocumentCache(t, root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	cachedDoc := newDocWithText(t, "alpha")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 7, cachedDoc); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}

	cache, err = newTestDocumentCache(t, root)
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
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	cache, err := newTestDocumentCache(t, root)
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

	reopened, err := newTestDocumentCache(t, root)
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
	cache, err := newTestDocumentCache(t, root)
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
	if err := cache.Close(); err != nil {
		t.Fatalf("close cache: %v", err)
	}

	reopened, err := newTestDocumentCache(t, root)
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
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	base, ok, err := cache.loadProjectedBaseRowByDocumentID("doc_1")
	if err != nil {
		t.Fatalf("load projected base row: %v", err)
	}
	if !ok {
		t.Fatal("expected projected_bases row")
	}
	if base.ProjectedSeq != baseSeq || base.ContentText != "base" || base.ContentLen != len("base") || len(base.StateUpdate) == 0 {
		t.Fatalf("projected_bases row = %#v, want explicit seq/content/state", base)
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
	cache, err := newTestDocumentCache(t, t.TempDir())
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

	currentDoc, _, _, err := cache.loadBaseDoc("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load current doc: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, currentDoc, "base\nlocal\nremote\n", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "docs/spec.md", remoteUpdate); err != nil {
		t.Fatalf("append remote update: %v", err)
	}
	entry.mu.Lock()
	_, remoteSeq, err := cache.applyPendingRemoteUpdatesLocked(entry, "doc_1", currentDoc)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply remote update: %v", err)
	}
	if remoteSeq <= firstSeq {
		t.Fatalf("remote seq %d must be after local seq %d", remoteSeq, firstSeq)
	}

	entry.mu.Lock()
	boundarySeq, err := cache.applyOutboxUpdateLocked(entry, "doc_1", "docs/spec.md", record)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply duplicate outbox after later boundary: %v", err)
	}
	if boundarySeq != remoteSeq {
		t.Fatalf("duplicate after later boundary returned seq %d, want boundary %d", boundarySeq, remoteSeq)
	}
}

func TestDocumentCacheCreatesSnapshotsAtConfiguredUpdateInterval(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 2)
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "v0")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	remoteDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(remoteDoc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base to remote doc: %v", err)
	}

	appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "v1", "remote1")
	seqs := loadDocumentSnapshotSeqs(t, cache, "doc_1")
	if !reflect.DeepEqual(seqs, []int64{1}) {
		t.Fatalf("snapshot seqs after one remote update = %#v, want only initial snapshot", seqs)
	}
	appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "v2", "remote2")
	seqs = loadDocumentSnapshotSeqs(t, cache, "doc_1")
	if !reflect.DeepEqual(seqs, []int64{1, 3}) {
		t.Fatalf("snapshot seqs after threshold = %#v, want initial plus threshold snapshot", seqs)
	}
}

func TestDocumentCacheLoadDocAtSeqUsesNearestSnapshotTailReplay(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1000)
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "v0")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	remoteDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(remoteDoc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base to remote doc: %v", err)
	}
	var appliedSeq int64
	for i := 1; i <= 3; i++ {
		appliedSeq = appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", fmt.Sprintf("v%d", i), fmt.Sprintf("remote%d", i))
	}

	var calls []struct {
		from int64
		to   int64
		rows int
	}
	withDocumentReplayHook(t, func(documentID string, fromSeq, toSeq int64, rows int) {
		if documentID == "doc_1" {
			calls = append(calls, struct {
				from int64
				to   int64
				rows int
			}{from: fromSeq, to: toSeq, rows: rows})
		}
	})
	doc, state, known, err := cache.loadDocAtSeq("doc_1", appliedSeq)
	if err != nil {
		t.Fatalf("load doc: %v", err)
	}
	defer doc.Close()
	if !known || len(state) == 0 {
		t.Fatalf("expected loaded doc to be known with state, known=%v state=%d", known, len(state))
	}
	if got := doc.GetText("content").ToString(); got != "v3" {
		t.Fatalf("loaded content = %q, want v3", got)
	}
	if len(calls) != 1 || calls[0].from != 1 || calls[0].to != appliedSeq || calls[0].rows != 3 {
		t.Fatalf("replay calls = %#v, want one tail replay from snapshot seq 1 with 3 rows", calls)
	}
}

func TestDocumentCacheProjectedBaseUsesMaterializedRowWithoutReconstruction(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if err := cache.storeProjectedBase("doc_1", "base", baseDoc.EncodeStateAsUpdate(), row.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if _, err := cache.db.Exec(`delete from crdt_updates where document_id = ?`, "doc_1"); err != nil {
		t.Fatalf("delete folded updates: %v", err)
	}

	fallbackCalls := 0
	withProjectedBaseFallbackHook(t, func(documentID string, projectedSeq int64) {
		fallbackCalls++
	})
	replayCalls := 0
	withDocumentReplayHook(t, func(documentID string, fromSeq, toSeq int64, rows int) {
		replayCalls++
	})
	decodeCalls := 0
	withDocumentSnapshotDecodeHook(t, func(documentID string, seq int64) {
		decodeCalls++
	})
	hashValidationCalls := 0
	withDocumentSnapshotHashValidationHook(t, func(documentID string, seq int64) {
		hashValidationCalls++
	})
	content, state, known, err := cache.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || content != "base" || len(state) == 0 {
		t.Fatalf("projected base = known %v content %q state %d, want materialized row hit", known, content, len(state))
	}
	if fallbackCalls != 0 {
		t.Fatalf("projected_bases hit should not call loadDocAtSeq fallback, got %d fallback calls", fallbackCalls)
	}
	if replayCalls != 0 {
		t.Fatalf("projected_bases hit should not replay crdt rows, got %d replay calls", replayCalls)
	}
	if decodeCalls != 0 {
		t.Fatalf("projected_bases hit should not decode snapshot state, got %d decode calls", decodeCalls)
	}
	if hashValidationCalls != 0 {
		t.Fatalf("projected_bases hit should not rehash snapshot blobs, got %d hash validation calls", hashValidationCalls)
	}
}

func TestDocumentCacheStoreProjectedBaseDoesNotBypassSnapshotInterval(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1000)
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	projectedSeq := appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "remote", "remote")
	if err := cache.storeProjectedBase("doc_1", "remote", remoteDoc.EncodeStateAsUpdate(), projectedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if seqs := loadDocumentSnapshotSeqs(t, cache, "doc_1"); !reflect.DeepEqual(seqs, []int64{1}) {
		t.Fatalf("projected-base store bypassed snapshot interval, snapshot seqs=%#v", seqs)
	}
	content, _, known, err := cache.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || content != "remote" {
		t.Fatalf("projected base = known %v content %q, want fallback reconstruction", known, content)
	}
	if seqs := loadDocumentSnapshotSeqs(t, cache, "doc_1"); !reflect.DeepEqual(seqs, []int64{1}) {
		t.Fatalf("projected-base fallback bypassed snapshot interval, snapshot seqs=%#v", seqs)
	}
}

func TestDocumentCacheProjectedBaseFallsBackAndBackfillsStaleRow(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1)
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	projectedSeq := appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "remote", "remote")
	if err := cache.storeProjectedBase("doc_1", "remote", remoteDoc.EncodeStateAsUpdate(), projectedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if _, err := cache.db.Exec(`update projected_bases set content_len = ? where document_id = ?`, 999, "doc_1"); err != nil {
		t.Fatalf("stale projected base metadata: %v", err)
	}

	var fallbackCalls int
	withProjectedBaseFallbackHook(t, func(documentID string, projectedSeq int64) {
		if documentID == "doc_1" {
			fallbackCalls++
		}
	})
	content, state, known, err := cache.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || content != "remote" || len(state) == 0 {
		t.Fatalf("projected base = known %v content %q state %d, want fallback replay", known, content, len(state))
	}
	if fallbackCalls != 1 {
		t.Fatalf("expected one projected-base fallback, got %d", fallbackCalls)
	}
	base, ok, err := cache.loadProjectedBaseRowByDocumentID("doc_1")
	if err != nil {
		t.Fatalf("reload backfilled projected base: %v", err)
	}
	if !ok || !base.metadataValid() || base.ContentText != "remote" || base.ProjectedSeq != projectedSeq {
		t.Fatalf("expected backfilled valid projected base at projected seq, ok=%v base=%#v", ok, base)
	}
}

func TestDocumentCacheProjectedBaseBackfillDoesNotMoveProjectionBackward(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	if err := cache.storeProjectedBase("doc_1", "base", baseDoc.EncodeStateAsUpdate(), baseSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	remoteDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(remoteDoc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base to remote doc: %v", err)
	}
	remoteSeq := appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "base\nremote", "remote")
	if remoteSeq <= baseSeq {
		t.Fatalf("remote seq = %d, want after base seq %d", remoteSeq, baseSeq)
	}
	if _, err := cache.db.Exec(`delete from projected_bases where document_id = ?`, "doc_1"); err != nil {
		t.Fatalf("delete projected base: %v", err)
	}

	var advanced bool
	withProjectedBaseFallbackHook(t, func(documentID string, projectedSeq int64) {
		if documentID != "doc_1" {
			return
		}
		if projectedSeq != baseSeq {
			t.Fatalf("fallback seq = %d, want old base seq %d", projectedSeq, baseSeq)
		}
		if err := cache.storeProjectedSeq(documentID, remoteSeq); err != nil {
			t.Fatalf("advance projected seq before backfill: %v", err)
		}
		advanced = true
	})
	content, state, known, err := cache.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !advanced {
		t.Fatal("expected projected-base fallback hook to advance projection")
	}
	if !known || content != "base" || len(state) == 0 {
		t.Fatalf("projected base = known %v content %q state %d, want reconstructed old base returned to caller", known, content, len(state))
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.ProjectedSeq != remoteSeq {
		t.Fatalf("fallback backfill moved projected seq to %d, want newer seq %d", row.ProjectedSeq, remoteSeq)
	}
	if base, ok, err := cache.loadProjectedBaseRowByDocumentID("doc_1"); err != nil || ok {
		t.Fatalf("guarded backfill must not install old projected base after cursor advanced, ok=%v base=%#v err=%v", ok, base, err)
	}
}

func TestDocumentCacheProjectedBaseFallbackBackfillFailureIsNonFatal(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if err := cache.storeProjectedBase("doc_1", "base", baseDoc.EncodeStateAsUpdate(), row.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if _, err := cache.db.Exec(`delete from projected_bases where document_id = ?`, "doc_1"); err != nil {
		t.Fatalf("delete projected base: %v", err)
	}
	storeErr := errors.New("projected base backfill failed")
	withProjectedBaseStoreHook(t, func(documentID string, projectedSeq int64) error {
		return storeErr
	})
	content, state, known, err := cache.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || content != "base" || len(state) == 0 {
		t.Fatalf("projected base = known %v content %q state %d, want fallback success", known, content, len(state))
	}
	if _, ok, err := cache.loadProjectedBaseRowByDocumentID("doc_1"); err != nil || ok {
		t.Fatalf("backfill failure should leave projected_bases missing, ok=%v err=%v", ok, err)
	}
}

func TestDocumentCacheStoreProjectedSeqClearsProjectedBaseRow(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if err := cache.storeProjectedBase("doc_1", "base", baseDoc.EncodeStateAsUpdate(), row.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if err := cache.storeProjectedSeq("doc_1", row.AppliedSeq); err != nil {
		t.Fatalf("store projected seq: %v", err)
	}
	if _, ok, err := cache.loadProjectedBaseRowByDocumentID("doc_1"); err != nil || ok {
		t.Fatalf("storeProjectedSeq should clear projected_bases row, ok=%v err=%v", ok, err)
	}
}

func TestDocumentCacheLoadDocAtSeqDeletesInvalidSnapshotStateAndFallsBack(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1)
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	appliedSeq := appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "remote", "remote")
	badState := []byte{1, 2, 3}
	if _, err := cache.db.Exec(`update document_snapshots set state_update = ?, state_sha256 = ? where document_id = ? and seq = ?`, badState, sha256Hex(badState), "doc_1", appliedSeq); err != nil {
		t.Fatalf("corrupt snapshot state: %v", err)
	}

	var decoded []int64
	withDocumentSnapshotDecodeHook(t, func(documentID string, seq int64) {
		if documentID == "doc_1" {
			decoded = append(decoded, seq)
		}
	})
	doc, state, known, err := cache.loadDocAtSeq("doc_1", appliedSeq)
	if err != nil {
		t.Fatalf("load doc: %v", err)
	}
	defer doc.Close()
	if !known || len(state) == 0 || doc.GetText("content").ToString() != "remote" {
		t.Fatalf("loaded doc = known %v state %d content %q", known, len(state), doc.GetText("content").ToString())
	}
	if len(decoded) < 2 || decoded[0] != appliedSeq {
		t.Fatalf("expected first decode to try corrupt exact snapshot before fallback, decoded=%#v", decoded)
	}
	snapshot, ok, err := cache.loadDocumentSnapshotAt("doc_1", appliedSeq)
	if err != nil {
		t.Fatalf("load repaired snapshot: %v", err)
	}
	if !ok || !snapshot.hashesValid() || snapshot.ContentText != "remote" {
		t.Fatalf("expected repaired valid snapshot at applied seq, ok=%v snapshot=%#v", ok, snapshot)
	}
}

func TestDocumentCacheSnapshotFailureDoesNotFailRemoteFoldOrLoad(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1)
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	storeErr := errors.New("snapshot insert failed")
	withDocumentSnapshotStoreHook(t, func(documentID string, seq int64) error {
		return storeErr
	})
	projectedSeq := appendAndApplyRemoteText(t, cache, remoteDoc, "doc_1", "docs/spec.md", "remote", "remote")

	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 0 {
		t.Fatalf("snapshot failure must not leave remote inbox pending, got %d", count)
	}
	doc, state, known, err := cache.loadDocAtSeq("doc_1", projectedSeq)
	if err != nil {
		t.Fatalf("load doc after snapshot failure: %v", err)
	}
	defer doc.Close()
	if !known || len(state) == 0 || doc.GetText("content").ToString() != "remote" {
		t.Fatalf("loaded doc after snapshot failure = known %v state %d content %q", known, len(state), doc.GetText("content").ToString())
	}
	if seqs := loadDocumentSnapshotSeqs(t, cache, "doc_1"); !reflect.DeepEqual(seqs, []int64{1}) {
		t.Fatalf("snapshot failure should leave only the pre-existing base snapshot, got %#v", seqs)
	}
}

func TestDocumentCacheProjectedBaseRowSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	cache, err := newTestDocumentCache(t, root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/spec.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	row, err := cache.ensureDocument("doc_1", "docs/spec.md")
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if err := cache.storeProjectedBase("doc_1", "base", baseDoc.EncodeStateAsUpdate(), row.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("close cache: %v", err)
	}

	reopened, err := newTestDocumentCache(t, root)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	fallbackCalls := 0
	withProjectedBaseFallbackHook(t, func(documentID string, projectedSeq int64) {
		fallbackCalls++
	})
	replayCalls := 0
	withDocumentReplayHook(t, func(documentID string, fromSeq, toSeq int64, rows int) {
		replayCalls++
	})
	content, _, known, err := reopened.loadProjectedBase("doc_1")
	if err != nil {
		t.Fatalf("load projected base after reopen: %v", err)
	}
	if !known || content != "base" {
		t.Fatalf("projected base after reopen = known %v content %q, want materialized row", known, content)
	}
	if fallbackCalls != 0 {
		t.Fatalf("reopen projected base row should not use fallback, got %d fallback calls", fallbackCalls)
	}
	if replayCalls != 0 {
		t.Fatalf("reopen projected base row should not replay crdt rows, got %d replay calls", replayCalls)
	}
}

func TestDocumentCacheDoesNotCreateFileBackedState(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
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

func TestStoreRootOutboxAndLocalDeleteIntentIsAtomic(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	const (
		rootDocumentID = "root_atomic_outbox"
		documentID     = "doc_atomic_outbox"
	)
	if _, err := cache.db.Exec(`create trigger fail_atomic_local_delete_intent
		before insert on root_local_delete_intents
		when new.root_document_id = 'root_atomic_outbox'
		begin
			select raise(abort, 'injected local-delete intent failure');
		end`); err != nil {
		t.Fatalf("create local-delete intent failure trigger: %v", err)
	}
	record := outboxUpdateRecord{Update: []byte("root tombstone update"), SourcePath: rootDocumentPath}
	if err := cache.storeOutboxUpdateWithRootLocalDeleteIntentLocked(
		nil,
		rootDocumentID,
		rootDocumentPath,
		record,
		documentID,
	); err == nil {
		t.Fatal("root outbox store unexpectedly succeeded through the intent failure trigger")
	}
	var outboxCount int
	if err := cache.db.QueryRow(`select count(*) from content_outbox where document_id = ?`, rootDocumentID).Scan(&outboxCount); err != nil {
		t.Fatalf("count rolled-back root outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("rolled-back root outbox count = %d, want 0", outboxCount)
	}
	assertRootLocalDeleteIntent(t, cache, rootDocumentID, documentID, false)

	if _, err := cache.db.Exec(`drop trigger fail_atomic_local_delete_intent`); err != nil {
		t.Fatalf("drop local-delete intent failure trigger: %v", err)
	}
	if err := cache.storeOutboxUpdateWithRootLocalDeleteIntentLocked(
		nil,
		rootDocumentID,
		rootDocumentPath,
		record,
		documentID,
	); err != nil {
		t.Fatalf("store atomic root outbox and local-delete intent: %v", err)
	}
	if err := cache.db.QueryRow(`select count(*) from content_outbox where document_id = ?`, rootDocumentID).Scan(&outboxCount); err != nil {
		t.Fatalf("count committed root outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("committed root outbox count = %d, want 1", outboxCount)
	}
	assertRootLocalDeleteIntent(t, cache, rootDocumentID, documentID, true)
}

func TestStoreRootProjectionEntriesClearsLocalDeleteIntentsAtomically(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	const (
		rootDocumentID = "root_atomic_projection"
		documentID     = "doc_atomic_projection"
	)
	previous := rootProjectionEntry{
		EntryID:           rootEntryIDForDocument(documentID),
		Kind:              rootEntryKindFile,
		ContentDocumentID: documentID,
		DesiredPath:       "docs/atomic.md",
		MaterializedPath:  "docs/atomic.md",
		Active:            true,
		ProjectedSeq:      1,
	}
	if err := cache.storeRootProjectionEntries(rootDocumentID, []rootProjectionEntry{previous}); err != nil {
		t.Fatalf("store previous root projection: %v", err)
	}
	if _, err := cache.db.Exec(`insert into root_local_delete_intents (
		root_document_id, content_document_id, created_at
	) values (?, ?, 1)`, rootDocumentID, documentID); err != nil {
		t.Fatalf("store local-delete intent: %v", err)
	}
	if _, err := cache.db.Exec(`create trigger fail_atomic_root_projection
		before insert on root_projection_entries
		when new.root_document_id = 'root_atomic_projection'
		begin
			select raise(abort, 'injected root projection store failure');
		end`); err != nil {
		t.Fatalf("create projection failure trigger: %v", err)
	}

	next := previous
	next.Active = false
	next.MaterializedPath = ""
	next.ProjectedSeq = 2
	if err := cache.storeRootProjectionEntries(rootDocumentID, []rootProjectionEntry{next}); err == nil {
		t.Fatal("root projection store unexpectedly succeeded through the failure trigger")
	}
	stored, err := cache.loadRootProjectionEntries(rootDocumentID)
	if err != nil {
		t.Fatalf("load projection after rolled-back store: %v", err)
	}
	got := stored[previous.EntryID]
	if !got.Active || got.ProjectedSeq != previous.ProjectedSeq || !got.LocalDeleteIntent {
		t.Fatalf("rolled-back projection = %#v, want previous active projection with durable intent", got)
	}
	if _, err := cache.db.Exec(`drop trigger fail_atomic_root_projection`); err != nil {
		t.Fatalf("drop projection failure trigger: %v", err)
	}
	if err := cache.storeRootProjectionEntries(rootDocumentID, []rootProjectionEntry{next}); err != nil {
		t.Fatalf("store successful root projection: %v", err)
	}
	stored, err = cache.loadRootProjectionEntries(rootDocumentID)
	if err != nil {
		t.Fatalf("load successful projection: %v", err)
	}
	got = stored[next.EntryID]
	if got.Active || got.ProjectedSeq != next.ProjectedSeq || got.LocalDeleteIntent {
		t.Fatalf("successful projection = %#v, want inactive projection with cleared intent", got)
	}
}

func TestWorkspaceStoreSchemaUsesAgreedDurableTables(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
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
	want := []string{"content_outbox", "crdt_updates", "document_snapshots", "documents", "incoming_updates", "local_namespace_intents", "projected_bases", "root_local_delete_intents", "root_projection_entries", "thread_outbox"}
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
	if strings.Contains(strings.ToLower(documentsSQL), "projected_text") {
		t.Fatalf("documents must not own projected base text metadata, schema=%s", documentsSQL)
	}
}

func assertSQLiteTableExists(t *testing.T, cache *documentCache, table string) {
	t.Helper()
	var name string
	if err := cache.db.QueryRow(`select name from sqlite_master where type = 'table' and name = ?`, table).Scan(&name); err != nil {
		t.Fatalf("expected sqlite table %s: %v", table, err)
	}
}

func withDocumentSnapshotEveryUpdates(t *testing.T, interval int64) {
	t.Helper()
	previous := documentSnapshotEveryUpdates
	documentSnapshotEveryUpdates = interval
	t.Cleanup(func() {
		documentSnapshotEveryUpdates = previous
	})
}

func withDocumentReplayHook(t *testing.T, hook func(documentID string, fromSeq, toSeq int64, rows int)) {
	t.Helper()
	previous := documentReplayHook
	documentReplayHook = hook
	t.Cleanup(func() {
		documentReplayHook = previous
	})
}

func withDocumentSnapshotStoreHook(t *testing.T, hook func(documentID string, seq int64) error) {
	t.Helper()
	previous := documentSnapshotStoreHook
	documentSnapshotStoreHook = hook
	t.Cleanup(func() {
		documentSnapshotStoreHook = previous
	})
}

func withDocumentSnapshotDecodeHook(t *testing.T, hook func(documentID string, seq int64)) {
	t.Helper()
	previous := documentSnapshotDecodeHook
	documentSnapshotDecodeHook = hook
	t.Cleanup(func() {
		documentSnapshotDecodeHook = previous
	})
}

func withDocumentSnapshotHashValidationHook(t *testing.T, hook func(documentID string, seq int64)) {
	t.Helper()
	previous := documentSnapshotHashValidationHook
	documentSnapshotHashValidationHook = hook
	t.Cleanup(func() {
		documentSnapshotHashValidationHook = previous
	})
}

func withProjectedBaseStoreHook(t *testing.T, hook func(documentID string, projectedSeq int64) error) {
	t.Helper()
	previous := projectedBaseStoreHook
	projectedBaseStoreHook = hook
	t.Cleanup(func() {
		projectedBaseStoreHook = previous
	})
}

func withProjectedBaseFallbackHook(t *testing.T, hook func(documentID string, projectedSeq int64)) {
	t.Helper()
	previous := projectedBaseFallbackHook
	projectedBaseFallbackHook = hook
	t.Cleanup(func() {
		projectedBaseFallbackHook = previous
	})
}

func appendAndApplyRemoteText(t *testing.T, cache *documentCache, remoteDoc *crdt.Doc, documentID, path, content string, origin any) int64 {
	t.Helper()
	update := captureDocTextUpdate(t, remoteDoc, content, origin)
	appended, err := cache.appendPendingRemoteUpdate(documentID, path, update)
	if err != nil {
		t.Fatalf("append pending remote update: %v", err)
	}
	if !appended {
		t.Fatal("expected pending remote update to append")
	}
	doc, _, _, err := cache.loadBaseDoc(documentID, path)
	if err != nil {
		t.Fatalf("load base doc: %v", err)
	}
	entry := cache.entryFor(documentID)
	entry.mu.Lock()
	applied, projectedSeq, err := cache.applyPendingRemoteUpdatesLocked(entry, documentID, doc)
	entry.mu.Unlock()
	doc.Close()
	if err != nil {
		t.Fatalf("apply pending remote update: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected one pending remote update to apply, got %d", applied)
	}
	return projectedSeq
}

func captureDocTextUpdate(t *testing.T, doc *crdt.Doc, content string, origin any) []byte {
	t.Helper()
	var update []byte
	unsubscribe := doc.OnUpdate(func(next []byte, observedOrigin any) {
		if observedOrigin == origin {
			update = append([]byte(nil), next...)
		}
	})
	updateDocText(t, doc, content, origin)
	unsubscribe()
	if len(update) == 0 {
		t.Fatal("expected CRDT update")
	}
	return update
}

func loadDocumentSnapshotSeqs(t *testing.T, cache *documentCache, documentID string) []int64 {
	t.Helper()
	rows, err := cache.db.Query(`select seq from document_snapshots where document_id = ? order by seq`, documentID)
	if err != nil {
		t.Fatalf("load snapshot seqs: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan snapshot seq: %v", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate snapshot seqs: %v", err)
	}
	return seqs
}

func assertDocumentSnapshotContent(t *testing.T, cache *documentCache, documentID string, seq int64, content string) {
	t.Helper()
	snapshot, ok, err := cache.loadDocumentSnapshotAt(documentID, seq)
	if err != nil {
		t.Fatalf("load snapshot %s/%d: %v", documentID, seq, err)
	}
	if !ok {
		t.Fatalf("expected snapshot %s/%d", documentID, seq)
	}
	if !snapshot.hashesValid() {
		t.Fatalf("snapshot %s/%d has invalid hashes: %#v", documentID, seq, snapshot)
	}
	if snapshot.ContentText != content {
		t.Fatalf("snapshot %s/%d content = %q, want %q", documentID, seq, snapshot.ContentText, content)
	}
}
