package notty

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	crdt "notty/internal/ycrdt"
)

type reverseWindowTestFixture struct {
	database          *Database
	store             *Store
	contentDocumentID string
	entryID           string
	desiredPath       string
	daemonAID         string
	daemonBID         string
}

func newReverseWindowTestFixture(t *testing.T) reverseWindowTestFixture {
	t.Helper()
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	contentDocumentID := mustCreateTestDocument(t, store, "docs/a.md", "initial content\n")
	fixture := reverseWindowTestFixture{
		database:          database,
		store:             store,
		contentDocumentID: contentDocumentID,
		entryID:           contentDocumentID,
		desiredPath:       "docs/a.md",
		daemonAID:         seedStoreDaemonRuntime(t, store, ""),
		daemonBID:         seedStoreDaemonRuntime(t, store, ""),
	}
	fixture.upsertRootEntry(t, store, fixture.entryID, fixture.contentDocumentID, fixture.desiredPath)
	return fixture
}

func (f reverseWindowTestFixture) openRequest(daemonID, operationID string, expectedGeneration int64) OpenReverseWindowRequest {
	return OpenReverseWindowRequest{
		OriginDaemonID:           daemonID,
		OriginScope:              "primary",
		OperationID:              operationID,
		ExpectedWindowGeneration: expectedGeneration,
		EntryID:                  f.entryID,
		ContentDocumentID:        f.contentDocumentID,
		ExpectedDesiredPath:      f.desiredPath,
	}
}

func (f reverseWindowTestFixture) contentStateVector(t *testing.T) []byte {
	t.Helper()
	document, err := f.store.GetDocument(f.contentDocumentID)
	if err != nil {
		t.Fatalf("get content document: %v", err)
	}
	vector, err := base64.StdEncoding.DecodeString(document.StateVector)
	if err != nil {
		t.Fatalf("decode content state vector: %v", err)
	}
	return vector
}

func (f reverseWindowTestFixture) consumeRequestForTest(t *testing.T, daemonID, tombstoneOperationID, restoreOperationID string, generation int64) ConsumeReverseWindowRequest {
	t.Helper()
	req := ConsumeReverseWindowRequest{
		OriginDaemonID:       daemonID,
		OriginScope:          "primary",
		TombstoneOperationID: tombstoneOperationID,
		RestoreOperationID:   restoreOperationID,
		WindowGeneration:     generation,
		ContentStateVector:   f.contentStateVector(t),
	}
	return req
}

func (f reverseWindowTestFixture) upsertRootEntry(t *testing.T, store *Store, entryID, contentDocumentID, path string) {
	t.Helper()
	rootDocument, err := store.GetDocument(store.RootDocumentID())
	if err != nil {
		t.Fatalf("get root document: %v", err)
	}
	doc, err := store.restoreDocumentDocPostgresLocked(rootDocument)
	if err != nil {
		t.Fatalf("restore root document: %v", err)
	}
	defer doc.Close()
	update, err := upsertRootCommandEntryForTest(doc, entryID, contentDocumentID, path)
	if err != nil {
		t.Fatalf("build root entry: %v", err)
	}
	if _, err := store.ApplyCRDTUpdate(store.RootDocumentID(), update, OperationMeta{
		ActorID:   f.daemonAID,
		ActorType: "daemon",
		Source:    "reverse-window-test",
	}); err != nil {
		t.Fatalf("apply root entry: %v", err)
	}
}

func upsertRootCommandEntryForTest(doc *crdt.Doc, entryID, contentDocumentID, path string) ([]byte, error) {
	root := doc.GetMap(rootMapName)
	return doc.Update(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if !ok {
			entries, err = root.SetMap(txn, rootEntriesMapName)
			if err != nil {
				return err
			}
		}
		entry, ok, err := entries.GetMap(txn, entryID)
		if err != nil {
			return err
		}
		if !ok {
			entry, err = entries.SetMap(txn, entryID)
			if err != nil {
				return err
			}
		}
		if err := entry.SetString(txn, "kind", rootEntryKindFile); err != nil {
			return err
		}
		if err := entry.SetString(txn, "contentDocumentId", contentDocumentID); err != nil {
			return err
		}
		if err := entry.SetString(txn, "loc", `{"name":"`+path+`"}`); err != nil {
			return err
		}
		return entry.SetString(txn, "deleted", "false")
	}, "reverse-window-test-root-entry")
}

func (f reverseWindowTestFixture) rootEntry(t *testing.T) rootFileCommandEntry {
	t.Helper()
	fresh, err := NewWorkspaceStore(f.database, f.store.WorkspaceID(), f.store.WorkspaceName())
	if err != nil {
		t.Fatalf("reload workspace store: %v", err)
	}
	rootDocument, err := fresh.GetDocument(fresh.RootDocumentID())
	if err != nil {
		t.Fatalf("get reloaded root: %v", err)
	}
	doc, err := fresh.restoreDocumentDocPostgresLocked(rootDocument)
	if err != nil {
		t.Fatalf("restore reloaded root: %v", err)
	}
	defer doc.Close()
	entry, err := rootFileEntryForCommand(doc, f.entryID, f.contentDocumentID, f.desiredPath)
	if err != nil {
		t.Fatalf("read root entry: %v", err)
	}
	return entry
}

type reverseWindowSnapshot struct {
	generation         int64
	tombstoneOperation string
	fingerprint        string
	tombstoneUpdateID  int64
	openedAt           time.Time
	reverseUntil       time.Time
	restoreOperation   string
	restoreUpdateID    int64
	consumedAt         time.Time
	rootHeadUpdateID   int64
	rootDeleted        bool
}

func (f reverseWindowTestFixture) snapshot(t *testing.T) reverseWindowSnapshot {
	t.Helper()
	var snapshot reverseWindowSnapshot
	var restoreOperation sql.NullString
	var restoreUpdateID sql.NullInt64
	var consumedAt sql.NullTime
	if err := f.database.DB.QueryRow(`SELECT
		COALESCE(window_generation, 0), tombstone_operation_id::text,
		tombstone_request_fingerprint, COALESCE(tombstone_update_id, 0),
		opened_at, reverse_until, restore_operation_id::text,
		restore_update_id, consumed_at
	FROM document_reverse_windows
	WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		f.store.WorkspaceID(), f.contentDocumentID,
	).Scan(
		&snapshot.generation,
		&snapshot.tombstoneOperation,
		&snapshot.fingerprint,
		&snapshot.tombstoneUpdateID,
		&snapshot.openedAt,
		&snapshot.reverseUntil,
		&restoreOperation,
		&restoreUpdateID,
		&consumedAt,
	); err != nil {
		t.Fatalf("read reverse window: %v", err)
	}
	snapshot.restoreOperation = restoreOperation.String
	snapshot.restoreUpdateID = restoreUpdateID.Int64
	if consumedAt.Valid {
		snapshot.consumedAt = consumedAt.Time
	}
	if err := f.database.DB.QueryRow(`SELECT update_id FROM document_heads
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		f.store.WorkspaceID(), f.store.RootDocumentID(),
	).Scan(&snapshot.rootHeadUpdateID); err != nil {
		t.Fatalf("read root head: %v", err)
	}
	snapshot.rootDeleted = f.rootEntry(t).Deleted
	return snapshot
}

func (f reverseWindowTestFixture) rootUpdateCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.database.DB.QueryRow(`SELECT COUNT(*) FROM document_updates
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		f.store.WorkspaceID(), f.store.RootDocumentID(),
	).Scan(&count); err != nil {
		t.Fatalf("count root updates: %v", err)
	}
	return count
}

func assertReverseWindowSnapshotEqual(t *testing.T, got, want reverseWindowSnapshot) {
	t.Helper()
	if got != want {
		t.Fatalf("reverse-window state mutated\n got: %#v\nwant: %#v", got, want)
	}
}

func TestReverseWindowG1RejectsSupersededReplayForever(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	ctx := context.Background()
	a1 := uuid.NewString()
	b1 := uuid.NewString()
	ra1 := uuid.NewString()
	rb1 := uuid.NewString()

	openedA, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, a1, 0))
	if err != nil || openedA.Outcome != OpenReverseWindowAcceptedNew || openedA.WindowGeneration != 1 {
		t.Fatalf("open A1: result=%#v err=%v", openedA, err)
	}
	if _, err := fixture.store.ConsumeReverseWindow(ctx, fixture.consumeRequestForTest(t, fixture.daemonAID, a1, ra1, 1)); err != nil {
		t.Fatalf("restore A1: %v", err)
	}
	openedB, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonBID, b1, 1))
	if err != nil || openedB.Outcome != OpenReverseWindowAcceptedNew || openedB.WindowGeneration != 2 {
		t.Fatalf("open B1: result=%#v err=%v", openedB, err)
	}

	delayed, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, a1, 0))
	if err != nil || delayed.Outcome != OpenReverseWindowGenerationConflict || delayed.CurrentWindowGeneration != 2 {
		t.Fatalf("deliver delayed A1: result=%#v err=%v", delayed, err)
	}
	if !fixture.rootEntry(t).Deleted {
		t.Fatal("delayed A1 changed B1's tombstoned root")
	}
	if _, err := fixture.store.ConsumeReverseWindow(ctx, fixture.consumeRequestForTest(t, fixture.daemonBID, b1, rb1, 2)); err != nil {
		t.Fatalf("restore B1: %v", err)
	}
	before := fixture.snapshot(t)
	delayed, err = fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, a1, 0))
	if err != nil || delayed.Outcome != OpenReverseWindowGenerationConflict || delayed.CurrentWindowGeneration != 2 {
		t.Fatalf("redeliver delayed A1: result=%#v err=%v", delayed, err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
}

func TestReverseWindowG2G3ExactReplayPrecedesCASAndFingerprintMismatch(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	ctx := context.Background()
	operationID := uuid.NewString()
	req := fixture.openRequest(fixture.daemonAID, operationID, 0)
	first, err := fixture.store.OpenOrReplaceReverseWindow(ctx, req)
	if err != nil || first.Outcome != OpenReverseWindowAcceptedNew {
		t.Fatalf("first open: result=%#v err=%v", first, err)
	}
	before := fixture.snapshot(t)

	// Path separators normalize before the server-derived fingerprint is built.
	retryReq := req
	retryReq.ExpectedDesiredPath = `docs\a.md`
	retry, err := fixture.store.OpenOrReplaceReverseWindow(ctx, retryReq)
	if err != nil || retry.Outcome != OpenReverseWindowExactReplayStoredResult {
		t.Fatalf("exact normalized retry: result=%#v err=%v", retry, err)
	}
	if retry.WindowGeneration != first.WindowGeneration || !retry.OpenedAt.Equal(first.OpenedAt) || !retry.ReverseUntil.Equal(first.ReverseUntil) {
		t.Fatalf("exact retry changed stored result: first=%#v retry=%#v", first, retry)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)

	mismatchReq := req
	mismatchReq.ExpectedWindowGeneration = 1
	mismatch, err := fixture.store.OpenOrReplaceReverseWindow(ctx, mismatchReq)
	if err != nil || mismatch.Outcome != OpenReverseWindowOperationMismatch {
		t.Fatalf("same-op changed-generation request: result=%#v err=%v", mismatch, err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)

	mismatchReq = req
	mismatchReq.ExpectedDesiredPath = "docs/other.md"
	mismatch, err = fixture.store.OpenOrReplaceReverseWindow(ctx, mismatchReq)
	if err != nil || mismatch.Outcome != OpenReverseWindowOperationMismatch {
		t.Fatalf("same-op changed-path request: result=%#v err=%v", mismatch, err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
}

func TestReverseWindowG4TwoIndependentPostgresConnectionsAcceptOneGeneration(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	fixture.database.DB.SetMaxOpenConns(1)

	secondSQL, err := sql.Open("pgx", fixture.database.URL)
	if err != nil {
		t.Fatalf("open second Postgres pool: %v", err)
	}
	secondSQL.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = secondSQL.Close() })
	if err := secondSQL.Ping(); err != nil {
		t.Fatalf("ping second Postgres pool: %v", err)
	}
	var firstPID, secondPID int
	if err := fixture.database.DB.QueryRow(`SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatalf("read first backend pid: %v", err)
	}
	if err := secondSQL.QueryRow(`SELECT pg_backend_pid()`).Scan(&secondPID); err != nil {
		t.Fatalf("read second backend pid: %v", err)
	}
	if firstPID == secondPID {
		t.Fatalf("race requires independent PostgreSQL connections, both used pid %d", firstPID)
	}
	secondStore, err := NewWorkspaceStore(&Database{DB: secondSQL, URL: fixture.database.URL}, fixture.store.WorkspaceID(), fixture.store.WorkspaceName())
	if err != nil {
		t.Fatalf("open second workspace store: %v", err)
	}

	beforeUpdates := fixture.rootUpdateCount(t)
	requests := []struct {
		store *Store
		req   OpenReverseWindowRequest
	}{
		{store: fixture.store, req: fixture.openRequest(fixture.daemonAID, uuid.NewString(), 0)},
		{store: secondStore, req: fixture.openRequest(fixture.daemonBID, uuid.NewString(), 0)},
	}
	type openResult struct {
		result OpenReverseWindowResult
		err    error
	}
	results := make(chan openResult, len(requests))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := request.store.OpenOrReplaceReverseWindow(context.Background(), request.req)
			results <- openResult{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	accepted := 0
	conflicted := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("contended open: %v", got.err)
		}
		switch got.result.Outcome {
		case OpenReverseWindowAcceptedNew:
			accepted++
			if got.result.WindowGeneration != 1 {
				t.Fatalf("accepted generation = %d, want 1", got.result.WindowGeneration)
			}
		case OpenReverseWindowGenerationConflict:
			conflicted++
			if got.result.CurrentWindowGeneration != 1 {
				t.Fatalf("conflict generation = %d, want 1", got.result.CurrentWindowGeneration)
			}
		default:
			t.Fatalf("unexpected contended outcome %q", got.result.Outcome)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("contended outcomes accepted=%d conflicted=%d, want 1/1", accepted, conflicted)
	}
	if got := fixture.rootUpdateCount(t) - beforeUpdates; got != 1 {
		t.Fatalf("contended opens appended %d root updates, want exactly 1", got)
	}
	if snapshot := fixture.snapshot(t); snapshot.generation != 1 || !snapshot.rootDeleted {
		t.Fatalf("contended final state = %#v, want generation-1 tombstone", snapshot)
	}
}

func TestReverseWindowG5RestoreBindsAcceptedPositiveGeneration(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	ctx := context.Background()
	tombstoneOperationID := uuid.NewString()
	restoreOperationID := uuid.NewString()
	opened, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, tombstoneOperationID, 0))
	if err != nil || opened.WindowGeneration != 1 {
		t.Fatalf("open window: result=%#v err=%v", opened, err)
	}
	before := fixture.snapshot(t)
	wrongGeneration := fixture.consumeRequestForTest(t, fixture.daemonAID, tombstoneOperationID, restoreOperationID, 2)
	if _, err := fixture.store.ConsumeReverseWindow(ctx, wrongGeneration); !errors.Is(err, ErrReverseWindowIdentityMismatch) {
		t.Fatalf("wrong-generation restore error = %v, want identity mismatch", err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)

	zeroGeneration := fixture.consumeRequestForTest(t, fixture.daemonAID, tombstoneOperationID, restoreOperationID, 0)
	if _, err := fixture.store.ConsumeReverseWindow(ctx, zeroGeneration); !errors.Is(err, ErrReverseWindowInvalidRequest) {
		t.Fatalf("zero-generation restore error = %v, want invalid request", err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)

	result, err := fixture.store.ConsumeReverseWindow(ctx, fixture.consumeRequestForTest(t, fixture.daemonAID, tombstoneOperationID, restoreOperationID, 1))
	if err != nil || result.Outcome != ConsumeReverseWindowAccepted {
		t.Fatalf("consume accepted generation: result=%#v err=%v", result, err)
	}
	if got := fixture.snapshot(t); got.generation != 1 || got.rootDeleted {
		t.Fatalf("restore changed generation or left root deleted: %#v", got)
	}
}

func TestReverseWindowG6ConsumedReplayRequiresFullIdentityBeforeDeadline(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	ctx := context.Background()
	tombstoneOperationID := uuid.NewString()
	restoreOperationID := uuid.NewString()
	if _, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, tombstoneOperationID, 0)); err != nil {
		t.Fatalf("open window: %v", err)
	}
	req := fixture.consumeRequestForTest(t, fixture.daemonAID, tombstoneOperationID, restoreOperationID, 1)
	first, err := fixture.store.ConsumeReverseWindow(ctx, req)
	if err != nil || first.Outcome != ConsumeReverseWindowAccepted {
		t.Fatalf("first consume: result=%#v err=%v", first, err)
	}
	if _, err := fixture.database.DB.Exec(`UPDATE document_reverse_windows
		SET reverse_until = opened_at + interval '1 microsecond'
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		fixture.store.WorkspaceID(), fixture.contentDocumentID,
	); err != nil {
		t.Fatalf("expire consumed window: %v", err)
	}
	before := fixture.snapshot(t)

	exactAfterDeadline := req
	exactAfterDeadline.ContentStateVector = nil
	replay, err := fixture.store.ConsumeReverseWindow(ctx, exactAfterDeadline)
	if err != nil || replay.Outcome != ConsumeReverseWindowExactReplayStoredResult {
		t.Fatalf("exact consumed replay after deadline: result=%#v err=%v", replay, err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)

	wrongRestore := req
	wrongRestore.RestoreOperationID = uuid.NewString()
	if _, err := fixture.store.ConsumeReverseWindow(ctx, wrongRestore); !errors.Is(err, ErrReverseWindowIdentityMismatch) {
		t.Fatalf("wrong restore-op error = %v, want identity mismatch", err)
	}
	wrongOrigin := req
	wrongOrigin.OriginDaemonID = fixture.daemonBID
	if _, err := fixture.store.ConsumeReverseWindow(ctx, wrongOrigin); !errors.Is(err, ErrReverseWindowIdentityMismatch) {
		t.Fatalf("wrong origin error = %v, want identity mismatch", err)
	}
	wrongGeneration := req
	wrongGeneration.WindowGeneration = 2
	if _, err := fixture.store.ConsumeReverseWindow(ctx, wrongGeneration); !errors.Is(err, ErrReverseWindowIdentityMismatch) {
		t.Fatalf("wrong generation error = %v, want identity mismatch", err)
	}
	notDistinct := req
	notDistinct.RestoreOperationID = tombstoneOperationID
	if _, err := fixture.store.ConsumeReverseWindow(ctx, notDistinct); !errors.Is(err, ErrReverseWindowInvalidRequest) {
		t.Fatalf("shared tombstone/restore op error = %v, want invalid request", err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
}

func TestReverseWindowG7RestoreAdmissionGuards(t *testing.T) {
	t.Run("origin", func(t *testing.T) {
		fixture := newReverseWindowTestFixture(t)
		operationID := uuid.NewString()
		if _, err := fixture.store.OpenOrReplaceReverseWindow(context.Background(), fixture.openRequest(fixture.daemonAID, operationID, 0)); err != nil {
			t.Fatalf("open window: %v", err)
		}
		before := fixture.snapshot(t)
		req := fixture.consumeRequestForTest(t, fixture.daemonBID, operationID, uuid.NewString(), 1)
		if _, err := fixture.store.ConsumeReverseWindow(context.Background(), req); !errors.Is(err, ErrReverseWindowIdentityMismatch) {
			t.Fatalf("wrong-origin restore error = %v, want identity mismatch", err)
		}
		assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
	})

	t.Run("missing frontier", func(t *testing.T) {
		fixture := newReverseWindowTestFixture(t)
		operationID := uuid.NewString()
		if _, err := fixture.store.OpenOrReplaceReverseWindow(context.Background(), fixture.openRequest(fixture.daemonAID, operationID, 0)); err != nil {
			t.Fatalf("open window: %v", err)
		}
		before := fixture.snapshot(t)
		req := fixture.consumeRequestForTest(t, fixture.daemonAID, operationID, uuid.NewString(), 1)
		req.ContentStateVector = nil
		if _, err := fixture.store.ConsumeReverseWindow(context.Background(), req); !errors.Is(err, ErrReverseWindowFrontierNotReached) {
			t.Fatalf("missing-frontier restore error = %v, want frontier rejection", err)
		}
		assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
	})

	t.Run("future frontier", func(t *testing.T) {
		fixture := newReverseWindowTestFixture(t)
		operationID := uuid.NewString()
		if _, err := fixture.store.OpenOrReplaceReverseWindow(context.Background(), fixture.openRequest(fixture.daemonAID, operationID, 0)); err != nil {
			t.Fatalf("open window: %v", err)
		}
		future := crdt.New(crdt.WithClientID(999999))
		futureText := future.GetText("content")
		future.Transact(func(txn *crdt.Transaction) {
			futureText.Insert(txn, 0, "future", nil)
		}, "future-frontier")
		futureVector := crdt.EncodeStateVectorV1(future)
		future.Close()
		before := fixture.snapshot(t)
		req := fixture.consumeRequestForTest(t, fixture.daemonAID, operationID, uuid.NewString(), 1)
		req.ContentStateVector = futureVector
		if _, err := fixture.store.ConsumeReverseWindow(context.Background(), req); !errors.Is(err, ErrReverseWindowFrontierNotReached) {
			t.Fatalf("future-frontier restore error = %v, want frontier rejection", err)
		}
		assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
	})

	t.Run("strict deadline", func(t *testing.T) {
		fixture := newReverseWindowTestFixture(t)
		operationID := uuid.NewString()
		if _, err := fixture.store.OpenOrReplaceReverseWindow(context.Background(), fixture.openRequest(fixture.daemonAID, operationID, 0)); err != nil {
			t.Fatalf("open window: %v", err)
		}
		if _, err := fixture.database.DB.Exec(`UPDATE document_reverse_windows
			SET reverse_until = opened_at + interval '1 microsecond'
			WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
			fixture.store.WorkspaceID(), fixture.contentDocumentID,
		); err != nil {
			t.Fatalf("expire window: %v", err)
		}
		before := fixture.snapshot(t)
		req := fixture.consumeRequestForTest(t, fixture.daemonAID, operationID, uuid.NewString(), 1)
		if _, err := fixture.store.ConsumeReverseWindow(context.Background(), req); !errors.Is(err, ErrReverseWindowExpired) {
			t.Fatalf("deadline restore error = %v, want expired", err)
		}
		assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
	})
}

func TestReverseWindowRestoreUsesFrontierDominanceAndRejectsPathClaim(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	ctx := context.Background()
	oldVector := fixture.contentStateVector(t)
	firstOperationID := uuid.NewString()
	if _, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, firstOperationID, 0)); err != nil {
		t.Fatalf("open first window: %v", err)
	}
	if _, _, err := fixture.store.ReplaceDocumentText(fixture.contentDocumentID, "backend is ahead\n", OperationMeta{
		ActorID: fixture.daemonAID, ActorType: "daemon", Source: "reverse-window-test",
	}); err != nil {
		t.Fatalf("advance content frontier: %v", err)
	}
	dominatedReq := fixture.consumeRequestForTest(t, fixture.daemonAID, firstOperationID, uuid.NewString(), 1)
	dominatedReq.ContentStateVector = oldVector
	if result, err := fixture.store.ConsumeReverseWindow(ctx, dominatedReq); err != nil || result.Outcome != ConsumeReverseWindowAccepted {
		t.Fatalf("restore with dominated frontier: result=%#v err=%v", result, err)
	}

	secondOperationID := uuid.NewString()
	if result, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, secondOperationID, 1)); err != nil || result.WindowGeneration != 2 {
		t.Fatalf("open second window: result=%#v err=%v", result, err)
	}
	otherContentID := mustCreateTestDocument(t, fixture.store, "docs/other.md", "other\n")
	fixture.upsertRootEntry(t, fixture.store, otherContentID, otherContentID, fixture.desiredPath)
	before := fixture.snapshot(t)
	req := fixture.consumeRequestForTest(t, fixture.daemonAID, secondOperationID, uuid.NewString(), 2)
	if _, err := fixture.store.ConsumeReverseWindow(ctx, req); !errors.Is(err, ErrReverseWindowPathClaimed) {
		t.Fatalf("conflicting-path restore error = %v, want path claimed", err)
	}
	assertReverseWindowSnapshotEqual(t, fixture.snapshot(t), before)
}

func TestReverseWindowG8OnlyAcceptedNewTombstoneIncrementsGeneration(t *testing.T) {
	fixture := newReverseWindowTestFixture(t)
	ctx := context.Background()
	if generation, err := fixture.store.ReadDocumentReverseGeneration(ctx, fixture.contentDocumentID); err != nil || generation != 0 {
		t.Fatalf("initial generation = %d err=%v, want 0", generation, err)
	}
	firstOperationID := uuid.NewString()
	first, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonAID, firstOperationID, 0))
	if err != nil || first.WindowGeneration != 1 {
		t.Fatalf("first open: result=%#v err=%v", first, err)
	}
	if generation, err := fixture.store.ReadDocumentReverseGeneration(ctx, fixture.contentDocumentID); err != nil || generation != 1 {
		t.Fatalf("generation after read = %d err=%v, want 1", generation, err)
	}
	fixture.upsertRootEntry(t, fixture.store, uuid.NewString(), mustCreateTestDocument(t, fixture.store, "docs/sibling.md", "sibling\n"), "docs/sibling.md")
	if generation, err := fixture.store.ReadDocumentReverseGeneration(ctx, fixture.contentDocumentID); err != nil || generation != 1 {
		t.Fatalf("generation after ordinary root update = %d err=%v, want 1", generation, err)
	}
	if _, err := fixture.store.ConsumeReverseWindow(ctx, fixture.consumeRequestForTest(t, fixture.daemonAID, firstOperationID, uuid.NewString(), 1)); err != nil {
		t.Fatalf("restore first window: %v", err)
	}
	if generation, err := fixture.store.ReadDocumentReverseGeneration(ctx, fixture.contentDocumentID); err != nil || generation != 1 {
		t.Fatalf("generation after restore = %d err=%v, want 1", generation, err)
	}
	if _, err := fixture.database.DB.Exec(`UPDATE document_reverse_windows
		SET reverse_until = opened_at + interval '1 microsecond'
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		fixture.store.WorkspaceID(), fixture.contentDocumentID,
	); err != nil {
		t.Fatalf("expire first window: %v", err)
	}
	if generation, err := fixture.store.ReadDocumentReverseGeneration(ctx, fixture.contentDocumentID); err != nil || generation != 1 {
		t.Fatalf("generation after expiry = %d err=%v, want 1", generation, err)
	}
	second, err := fixture.store.OpenOrReplaceReverseWindow(ctx, fixture.openRequest(fixture.daemonBID, uuid.NewString(), 1))
	if err != nil || second.Outcome != OpenReverseWindowAcceptedNew || second.WindowGeneration != 2 {
		t.Fatalf("second open: result=%#v err=%v", second, err)
	}
	if generation, err := fixture.store.ReadDocumentReverseGeneration(ctx, fixture.contentDocumentID); err != nil || generation != 2 {
		t.Fatalf("generation after second accepted tombstone = %d err=%v, want 2", generation, err)
	}
}
