package syncer

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func TestRootLocalDeleteIntentG9ConflictReplacesImmutableOperationAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	cache, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	initial := rootLocalDeleteIntent{
		RootDocumentID:           "root-1",
		ContentDocumentID:        "doc-1",
		EntryID:                  "doc-1",
		DesiredPath:              "docs/a.md",
		MaterializedPath:         "docs/a.md",
		TombstoneOperationID:     "tombstone-old",
		ExpectedWindowGeneration: 0,
		Phase:                    rootLocalDeletePhaseTombstonePending,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if err := cache.beginRootLocalDeleteIntent(initial); err != nil {
		t.Fatalf("persist operation before send: %v", err)
	}
	if _, err := cache.recordRootLocalDeleteIntentAttempt(
		initial.RootDocumentID,
		initial.ContentDocumentID,
		initial.TombstoneOperationID,
		initial.ExpectedWindowGeneration,
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("record send attempt: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("close before response: %v", err)
	}

	reopened, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("reopen before response: %v", err)
	}
	got, ok, err := reopened.loadRootLocalDeleteIntent("root-1", "doc-1")
	if err != nil {
		t.Fatalf("load attempted operation: %v", err)
	}
	if !ok || got.TombstoneOperationID != "tombstone-old" || got.ExpectedWindowGeneration != 0 || got.Attempts != 1 {
		t.Fatalf("attempted operation after reopen = %#v, present=%v", got, ok)
	}

	// The conflict transition must terminalize the old operation and INSERT a
	// fresh row. This trigger makes an in-place identity rewrite causally RED.
	if _, err := reopened.db.Exec(`create trigger reject_root_intent_identity_rewrite
		before update of tombstone_operation_id, expected_window_generation
		on root_local_delete_intents
		begin
			select raise(abort, 'root intent identity is immutable');
	end`); err != nil {
		t.Fatalf("install immutable-operation trigger: %v", err)
	}
	if _, err := reopened.recordRootLocalDeleteIntentAttempt(
		"root-1", "doc-1", "tombstone-old", 0, now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("bookkeeping update under identity trigger: %v", err)
	}
	if _, err := reopened.db.Exec(`create trigger abort_root_intent_replacement_after_delete
		after delete on root_local_delete_intents
		begin
			select raise(rollback, 'simulated crash after delete');
		end`); err != nil {
		t.Fatalf("install replacement crash trigger: %v", err)
	}
	if err := reopened.replaceRootLocalDeleteIntentAfterGenerationConflict(
		"root-1",
		"doc-1",
		"tombstone-old",
		0,
		"tombstone-fresh",
		7,
		now.Add(3*time.Second),
	); err == nil {
		t.Fatal("replacement unexpectedly committed through simulated post-delete crash")
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close after interrupted conflict replacement: %v", err)
	}

	interrupted, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("reopen after interrupted conflict replacement: %v", err)
	}
	got, ok, err = interrupted.loadRootLocalDeleteIntent("root-1", "doc-1")
	if err != nil {
		t.Fatalf("load operation after interrupted replacement: %v", err)
	}
	if !ok || got.TombstoneOperationID != "tombstone-old" || got.ExpectedWindowGeneration != 0 || got.Attempts != 2 {
		t.Fatalf("interrupted replacement did not roll back to re-drivable old operation: %#v, present=%v", got, ok)
	}
	if _, err := interrupted.db.Exec(`drop trigger abort_root_intent_replacement_after_delete`); err != nil {
		t.Fatalf("remove replacement crash trigger: %v", err)
	}
	if err := interrupted.replaceRootLocalDeleteIntentAfterGenerationConflict(
		"root-1",
		"doc-1",
		"tombstone-old",
		0,
		"tombstone-fresh",
		7,
		now.Add(4*time.Second),
	); err != nil {
		t.Fatalf("replace conflicted operation: %v", err)
	}
	if err := interrupted.Close(); err != nil {
		t.Fatalf("close after conflict: %v", err)
	}

	finalCache, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("reopen after conflict: %v", err)
	}
	t.Cleanup(func() { _ = finalCache.Close() })
	got, ok, err = finalCache.loadRootLocalDeleteIntent("root-1", "doc-1")
	if err != nil {
		t.Fatalf("load replacement operation: %v", err)
	}
	if !ok {
		t.Fatal("replacement operation missing after reopen")
	}
	if got.TombstoneOperationID != "tombstone-fresh" || got.ExpectedWindowGeneration != 7 {
		t.Fatalf("replacement identity = (%q, %d), want fresh/7", got.TombstoneOperationID, got.ExpectedWindowGeneration)
	}
	if got.Phase != rootLocalDeletePhaseTombstonePending || got.Attempts != 0 || got.WindowGeneration != 0 {
		t.Fatalf("replacement retry state = %#v", got)
	}
	if got.EntryID != initial.EntryID || got.DesiredPath != initial.DesiredPath || got.MaterializedPath != initial.MaterializedPath {
		t.Fatalf("replacement lost namespace correlation: %#v", got)
	}
}

func TestRootLocalDeleteIntentG10AcceptedGenerationAndDistinctRestorePersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	now := time.Unix(1_700_000_100, 0).UTC()
	cache, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	intent := rootLocalDeleteIntent{
		RootDocumentID:           "root-1",
		ContentDocumentID:        "doc-1",
		EntryID:                  "doc-1",
		DesiredPath:              "docs/a.md",
		MaterializedPath:         "docs/a.md",
		TombstoneOperationID:     "tombstone-1",
		ExpectedWindowGeneration: 4,
		Phase:                    rootLocalDeletePhaseTombstonePending,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if err := cache.beginRootLocalDeleteIntent(intent); err != nil {
		t.Fatalf("begin tombstone: %v", err)
	}
	if _, err := cache.db.Exec(`create trigger reject_root_intent_accepted_generation_rewrite
		before update of window_generation on root_local_delete_intents
		when old.window_generation is not null
			and new.window_generation is not old.window_generation
		begin
			select raise(abort, 'accepted window generation is immutable');
		end`); err != nil {
		t.Fatalf("install accepted-generation trigger: %v", err)
	}
	if err := cache.acceptRootLocalDeleteWindow(
		"root-1", "doc-1", "tombstone-1", 4, 5,
		now.Add(time.Second), now.Add(5*time.Minute), now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("persist accepted generation: %v", err)
	}
	if _, err := cache.db.Exec(`update root_local_delete_intents
		set window_generation = 6
		where root_document_id = 'root-1' and content_document_id = 'doc-1'`); err == nil {
		t.Fatal("accepted generation was mutable after its NULL-to-value transition")
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("close after acceptance: %v", err)
	}

	reopened, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("reopen after acceptance: %v", err)
	}
	accepted, ok, err := reopened.loadRootLocalDeleteIntent("root-1", "doc-1")
	if err != nil {
		t.Fatalf("load accepted window: %v", err)
	}
	if !ok || accepted.Phase != rootLocalDeletePhaseWindowOpen || accepted.WindowGeneration != 5 {
		t.Fatalf("accepted window after reopen = %#v, present=%v", accepted, ok)
	}
	if err := reopened.beginRootLocalDeleteRestore(
		"root-1", "doc-1", "tombstone-1", "restore-1", 5,
		"identity-1", "content-hash-1", now.Add(3*time.Second),
	); err != nil {
		t.Fatalf("persist distinct restore operation: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close after restore allocation: %v", err)
	}

	finalCache, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("reopen after restore allocation: %v", err)
	}
	t.Cleanup(func() { _ = finalCache.Close() })
	got, ok, err := finalCache.loadRootLocalDeleteIntent("root-1", "doc-1")
	if err != nil {
		t.Fatalf("load restore operation: %v", err)
	}
	if !ok {
		t.Fatal("restore operation missing after reopen")
	}
	if got.Phase != rootLocalDeletePhaseContentSyncing || got.WindowGeneration != 5 {
		t.Fatalf("restore phase/generation after reopen = %#v", got)
	}
	if got.RestoreOperationID != "restore-1" || got.RestoreOperationID == got.TombstoneOperationID {
		t.Fatalf("restore identity is not distinct: tombstone=%q restore=%q", got.TombstoneOperationID, got.RestoreOperationID)
	}
	if got.ObservedFileIdentity != "identity-1" || got.ObservedContentSHA256 != "content-hash-1" {
		t.Fatalf("restore observation after reopen = %#v", got)
	}

	frontier := []byte{1, 2, 3}
	if err := finalCache.markRootLocalDeleteRestorePending(
		"root-1", "doc-1", "tombstone-1", "restore-1", 5, frontier, now.Add(4*time.Second),
	); err != nil {
		t.Fatalf("persist acknowledged restore frontier: %v", err)
	}
	if err := finalCache.Close(); err != nil {
		t.Fatalf("close after restore-ready: %v", err)
	}
	lastCache, err := newDocumentCache(path)
	if err != nil {
		t.Fatalf("reopen restore-ready row: %v", err)
	}
	t.Cleanup(func() { _ = lastCache.Close() })
	got, ok, err = lastCache.loadRootLocalDeleteIntent("root-1", "doc-1")
	if err != nil {
		t.Fatalf("load restore-ready row: %v", err)
	}
	if !ok || got.Phase != rootLocalDeletePhaseRestorePending || got.WindowGeneration != 5 || !bytes.Equal(got.RequiredContentStateVector, frontier) {
		t.Fatalf("restore-ready row after reopen = %#v, present=%v", got, ok)
	}
}
