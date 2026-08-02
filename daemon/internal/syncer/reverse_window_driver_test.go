package syncer

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
)

func TestReverseWindowDriverOrdersOpenContentProofConsumeProjection(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)

	result, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	if len(result.LocalCreates) != 0 {
		t.Fatalf("unexpected local creates after open: %#v", result.LocalCreates)
	}
	assertReverseWindowPhase(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID, rootLocalDeletePhaseWindowOpen)

	const restored = "restored under the original content identity\n"
	if err := os.WriteFile(fixture.path, []byte(restored), 0o644); err != nil {
		t.Fatalf("write retained create: %v", err)
	}
	candidate := localCreateCandidate{
		Root:      fixture.root,
		Path:      fixture.path,
		ActorID:   fixture.cfg.AgentID,
		ActorType: "daemon",
	}
	result, err = fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), []localCreateCandidate{candidate})
	if err != nil {
		t.Fatalf("correlate retained create: %v", err)
	}
	if len(result.LocalCreates) != 0 {
		t.Fatalf("retained create escaped into ordinary create arbitration: %#v", result.LocalCreates)
	}
	assertReverseWindowPhase(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID, rootLocalDeletePhaseContentSyncing)

	waitedForFrontier := 0
	fixture.runtime.waitForBackendStateVector = func(_ context.Context, documentID string, frontier []byte) error {
		waitedForFrontier++
		if documentID != fixture.tracked.DocumentID {
			t.Fatalf("frontier document = %q, want %q", documentID, fixture.tracked.DocumentID)
		}
		if len(frontier) == 0 {
			t.Fatal("content proof attempted with an empty frontier")
		}
		return nil
	}
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("synchronize retained content: %v", err)
	}
	if waitedForFrontier != 1 {
		t.Fatalf("backend frontier waits = %d, want 1", waitedForFrontier)
	}
	assertReverseWindowPhase(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID, rootLocalDeletePhaseRestorePending)
	fixture.server.assertContents(t, map[string]string{fixture.relative: restored})

	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("consume reverse window: %v", err)
	}
	assertReverseWindowPhase(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID, rootLocalDeletePhaseProjectionPending)
	if got := fixture.server.reverseRequestLog(); len(got) != 2 || got[0] != "open" || got[1] != "consume" {
		t.Fatalf("reverse-window request order = %#v, want [open consume]", got)
	}
}

func TestReverseWindowDriverGenerationConflictAllocatesFreshImmutableOperation(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	fixture.server.setReverseGeneration(7)

	before := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("handle generation conflict: %v", err)
	}
	after := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	if after.Phase != rootLocalDeletePhaseTombstonePending || after.ExpectedWindowGeneration != 7 {
		t.Fatalf("replacement intent = phase %q expected generation %d, want pending/7", after.Phase, after.ExpectedWindowGeneration)
	}
	if after.TombstoneOperationID == before.TombstoneOperationID {
		t.Fatal("generation conflict rewrote the expected generation on the existing operation")
	}

	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("accept fresh generation-bound operation: %v", err)
	}
	accepted := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	if accepted.Phase != rootLocalDeletePhaseWindowOpen || accepted.WindowGeneration != 8 {
		t.Fatalf("accepted replacement = phase %q generation %d, want window_open/8", accepted.Phase, accepted.WindowGeneration)
	}
}

func TestReverseWindowDriverSupersededGenerationArchivesRestoredBytesWithoutProjection(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	const restored = "restored bytes rejected by a superseding generation\n"
	if err := os.WriteFile(fixture.path, []byte(restored), 0o644); err != nil {
		t.Fatalf("write restored bytes: %v", err)
	}
	candidate := localCreateCandidate{
		Root:      fixture.root,
		Path:      fixture.path,
		ActorID:   fixture.cfg.AgentID,
		ActorType: "daemon",
	}
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), []localCreateCandidate{candidate}); err != nil {
		t.Fatalf("correlate restored bytes: %v", err)
	}
	fixture.runtime.waitForBackendStateVector = func(context.Context, string, []byte) error { return nil }
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("prove restored content: %v", err)
	}
	assertReverseWindowPhase(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID, rootLocalDeletePhaseRestorePending)
	fixture.server.setReverseConsumeStatus(http.StatusConflict)

	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("finalize superseded restore: %v", err)
	}

	if _, ok, err := fixture.runtime.docCache.loadRootLocalDeleteIntent(fixture.server.rootDocumentID, fixture.tracked.DocumentID); err != nil {
		t.Fatalf("load superseded reverse workflow: %v", err)
	} else if ok {
		t.Fatal("superseded generation reached or retained projection-pending state")
	}
	assertWorkspaceFileMissing(t, fixture.root, fixture.relative)
	assertRecoveredContent(t, fixture.root, fixture.tracked.DocumentID, restored)
	if got := fixture.server.reverseRequestLog(); len(got) != 2 || got[0] != "open" || got[1] != "consume" {
		t.Fatalf("superseded request order = %#v, want [open consume]", got)
	}
}

func TestReverseWindowDriverMismatchedConsumeGenerationCannotAdvanceProjection(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	intent := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	const restored = "bytes bound to the accepted generation\n"
	if err := os.WriteFile(fixture.path, []byte(restored), 0o644); err != nil {
		t.Fatalf("write generation-bound restored bytes: %v", err)
	}
	observation, err := fixture.runtime.observeReverseWindowPath(intent)
	if err != nil {
		t.Fatalf("observe generation-bound restored bytes: %v", err)
	}
	intent.Phase = rootLocalDeletePhaseRestorePending
	intent.RestoreOperationID = uuid.NewString()
	intent.RequiredContentStateVector = []byte{1}
	intent.ObservedFileIdentity = observation.identity
	intent.ObservedContentSHA256 = observation.contentSHA
	intent.UpdatedAt = time.Now().UTC()
	if err := fixture.runtime.docCache.storeRootLocalDeleteIntent(intent); err != nil {
		t.Fatalf("store generation-bound restore: %v", err)
	}
	fixture.server.setReverseConsumeGeneration(intent.WindowGeneration + 1)

	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err == nil {
		t.Fatal("mismatched consume generation unexpectedly advanced")
	}

	got := loadReverseWindowIntent(t, fixture.runtime.docCache, intent.RootDocumentID, intent.ContentDocumentID)
	if got.Phase != rootLocalDeletePhaseRestorePending || got.WindowGeneration != intent.WindowGeneration || got.RestoreOperationID != intent.RestoreOperationID {
		t.Fatalf("mismatched consume mutated the durable restore identity: %#v", got)
	}
	assertWorkspaceFileContent(t, fixture.root, fixture.relative, restored)
}

func TestReverseWindowDriverReappearanceBeforeFirstOpenCancelsUnsentOperation(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	before := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	const reappeared = "present again before the first Open attempt\n"
	mutatedAtSendBoundary := false
	fixture.runtime.reverseWindowDecisionHook = func() {
		if mutatedAtSendBoundary {
			return
		}
		mutatedAtSendBoundary = true
		if err := os.WriteFile(fixture.path, []byte(reappeared), 0o644); err != nil {
			t.Fatalf("write send-boundary reappearance: %v", err)
		}
	}
	candidate := localCreateCandidate{
		Root:      fixture.root,
		Path:      fixture.path,
		ActorID:   fixture.cfg.AgentID,
		ActorType: "daemon",
	}

	result, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), []localCreateCandidate{candidate})
	if err != nil {
		t.Fatalf("cancel unsent reverse-window operation: %v", err)
	}
	if len(result.LocalCreates) != 0 {
		t.Fatalf("same-document reappearance escaped to ordinary create arbitration: %#v", result.LocalCreates)
	}
	if got := fixture.server.reverseRequestLog(); len(got) != 0 {
		t.Fatalf("unsent tombstone still reached backend: %#v", got)
	}
	if !mutatedAtSendBoundary {
		t.Fatal("send-boundary reappearance hook did not run")
	}
	if _, ok, err := fixture.runtime.docCache.loadRootLocalDeleteIntent(fixture.server.rootDocumentID, fixture.tracked.DocumentID); err != nil {
		t.Fatalf("load canceled reverse-window intent: %v", err)
	} else if ok {
		t.Fatal("canceled unsent reverse-window intent is still durable")
	}
	assertWorkspaceFileContent(t, fixture.root, fixture.relative, reappeared)

	if err := os.Remove(fixture.path); err != nil {
		t.Fatalf("remove reappeared file for a subsequent delete: %v", err)
	}
	started, err := fixture.runtime.beginSemanticRootLocalDelete(fixture.tracked.DocumentID)
	if err != nil {
		t.Fatalf("begin subsequent semantic delete: %v", err)
	}
	if !started {
		t.Fatal("subsequent semantic delete did not start")
	}
	after := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	if after.TombstoneOperationID == before.TombstoneOperationID {
		t.Fatal("subsequent delete resurrected the canceled tombstone operation id")
	}
}

func TestReverseWindowProjectionKeepsTombstonedContentOnSocket(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	if err := fixture.runtime.docCache.storeRootProjectionEntries(fixture.server.rootDocumentID, []rootProjectionEntry{{
		EntryID:           rootEntryIDForDocument(fixture.tracked.DocumentID),
		Kind:              rootEntryKindFile,
		ContentDocumentID: fixture.tracked.DocumentID,
		DesiredPath:       fixture.relative,
		Active:            false,
		ProjectedSeq:      2,
	}}); err != nil {
		t.Fatalf("store tombstoned root projection: %v", err)
	}
	if err := fixture.runtime.updateDesiredDocumentsFromRootProjection(); err != nil {
		t.Fatalf("update desired documents: %v", err)
	}

	desired := fixture.runtime.documentSocket.Document(fixture.tracked.DocumentID)
	if desired == nil || desired.Path != fixture.relative {
		t.Fatalf("hidden reverse-window document = %#v, want same document/path", desired)
	}
}

func TestReverseWindowTombstoneProjectionRetainsOriginCorrelation(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	const reappeared = "reappeared before tombstone projection\n"
	if err := os.WriteFile(fixture.path, []byte(reappeared), 0o644); err != nil {
		t.Fatalf("write reappeared origin file: %v", err)
	}

	if err := fixture.runtime.projectRootRemovedEntry(rootProjectionEntry{
		EntryID:           rootEntryIDForDocument(fixture.tracked.DocumentID),
		ContentDocumentID: fixture.tracked.DocumentID,
		DesiredPath:       fixture.relative,
		MaterializedPath:  fixture.relative,
		Active:            true,
		LocalDeleteIntent: true,
	}); err != nil {
		t.Fatalf("project semantic tombstone: %v", err)
	}

	assertWorkspaceFileContent(t, fixture.root, fixture.relative, reappeared)
	assertRuntimeTrackedAtPath(t, fixture.runtime, fixture.tracked.DocumentID, fixture.path)
	assertReverseWindowPhase(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID, rootLocalDeletePhaseWindowOpen)
}

func TestReverseWindowProjectionClearsOnlyAfterExactActiveBinding(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	intent := forceProjectionPendingIntent(t, fixture)
	const occupant = "post-proof bytes remain authoritative\n"
	if err := os.WriteFile(fixture.path, []byte(occupant), 0o644); err != nil {
		t.Fatalf("write projection occupant: %v", err)
	}
	observation, err := fixture.runtime.observeReverseWindowPath(intent)
	if err != nil {
		t.Fatalf("observe projection occupant: %v", err)
	}
	intent.ObservedFileIdentity = observation.identity
	intent.ObservedContentSHA256 = observation.contentSHA
	if err := fixture.runtime.docCache.storeRootLocalDeleteIntent(intent); err != nil {
		t.Fatalf("store projection proof observation: %v", err)
	}

	if err := fixture.runtime.reconcileRootNamespace(fixture.ctx); err != nil {
		t.Fatalf("reconcile restored root projection: %v", err)
	}

	assertWorkspaceFileContent(t, fixture.root, fixture.relative, occupant)
	if _, ok, err := fixture.runtime.docCache.loadRootLocalDeleteIntent(intent.RootDocumentID, intent.ContentDocumentID); err != nil {
		t.Fatalf("load projection-complete intent: %v", err)
	} else if ok {
		t.Fatal("exact active in-memory and SQLite projection did not clear the workflow")
	}
}

func TestReverseWindowProjectionConflictMovesOriginalOwnedBytesToAllocatedPath(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	intent := forceProjectionPendingIntent(t, fixture)
	const restored = "base\n"
	if err := os.WriteFile(fixture.path, []byte(restored), 0o644); err != nil {
		t.Fatalf("write restored original bytes: %v", err)
	}
	observation, err := fixture.runtime.observeReverseWindowPath(intent)
	if err != nil {
		t.Fatalf("observe restored original bytes: %v", err)
	}
	intent.ObservedFileIdentity = observation.identity
	intent.ObservedContentSHA256 = observation.contentSHA
	if err := fixture.runtime.docCache.storeRootLocalDeleteIntent(intent); err != nil {
		t.Fatalf("store restored original proof: %v", err)
	}
	beforeIdentity := statFileIdentity(fixture.path)

	const claimantID = "00000000-0000-0000-0000-000000000000"
	seedProjectionConflict(t, fixture, claimantID, "claimant base\n")
	projected := plannedProjectionForDocument(t, fixture.runtime, intent.ContentDocumentID)
	if projected.MaterializedPath == intent.MaterializedPath {
		t.Fatalf("planner retained conflicted path %q", projected.MaterializedPath)
	}

	if err := fixture.runtime.reconcileRootNamespace(fixture.ctx); err != nil {
		t.Fatalf("reconcile original-owned projection conflict: %v", err)
	}

	assertWorkspaceFileMissing(t, fixture.root, intent.MaterializedPath)
	assertWorkspaceFileContent(t, fixture.root, projected.MaterializedPath, restored)
	afterIdentity := statFileIdentity(filepath.Join(fixture.root, filepath.FromSlash(projected.MaterializedPath)))
	if !sameFileIdentity(beforeIdentity, afterIdentity) {
		t.Fatalf("conflict relocation did not preserve file identity: before=%#v after=%#v", beforeIdentity, afterIdentity)
	}
	assertRuntimeTrackedAtPath(t, fixture.runtime, intent.ContentDocumentID, filepath.Join(fixture.root, filepath.FromSlash(projected.MaterializedPath)))
	if _, ok, err := fixture.runtime.docCache.loadRootLocalDeleteIntent(intent.RootDocumentID, intent.ContentDocumentID); err != nil {
		t.Fatalf("load conflict-complete reverse-window intent: %v", err)
	} else if ok {
		t.Fatal("conflict relocation did not clear the exact projection-pending workflow")
	}
}

func TestReverseWindowProjectionConflictPreservesNewerTrackedClaimant(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	intent := forceProjectionPendingIntent(t, fixture)

	const (
		claimantID    = "00000000-0000-0000-0000-000000000000"
		claimantBytes = "bytes already owned by the newer claimant\n"
	)
	seedProjectionConflict(t, fixture, claimantID, claimantBytes)
	fixture.tracked.untrack()
	claimant := &trackedFile{
		DocumentID:    claimantID,
		DocumentPath:  fixture.relative,
		Path:          fixture.path,
		WorkspaceRoot: fixture.root,
		FS:            fixture.runtime.replica.fs,
		Owner:         fixture.runtime.replica,
	}
	claimant.setProjectedContent(claimantBytes)
	fixture.runtime.replica.mu.Lock()
	fixture.runtime.replica.projectedByID[claimantID] = claimant
	fixture.runtime.replica.projectedByPath[fixture.path] = claimant
	fixture.runtime.replica.mu.Unlock()
	if err := os.WriteFile(fixture.path, []byte(claimantBytes), 0o644); err != nil {
		t.Fatalf("write newer claimant bytes: %v", err)
	}
	projected := plannedProjectionForDocument(t, fixture.runtime, intent.ContentDocumentID)
	if projected.MaterializedPath == intent.MaterializedPath {
		t.Fatalf("planner retained conflicted path %q", projected.MaterializedPath)
	}
	fixture.runtime.waitForBackendStateVector = func(context.Context, string, []byte) error { return nil }

	if err := fixture.runtime.reconcileRootNamespace(fixture.ctx); err != nil {
		t.Fatalf("prepare newer-claimant projection conflict: %v", err)
	}
	assertWorkspaceFileContent(t, fixture.root, intent.MaterializedPath, claimantBytes)
	assertRuntimeTrackedAtPath(t, fixture.runtime, claimantID, fixture.path)
	assertRuntimeTrackedAtPath(t, fixture.runtime, intent.ContentDocumentID, filepath.Join(fixture.root, filepath.FromSlash(projected.MaterializedPath)))

	if err := fixture.runtime.reconcileDocumentIDs(fixture.ctx, []string{intent.ContentDocumentID}); err != nil {
		t.Fatalf("materialize restored content at allocated path: %v", err)
	}
	if err := fixture.runtime.reconcileRootNamespace(fixture.ctx); err != nil {
		t.Fatalf("seal newer-claimant projection conflict: %v", err)
	}

	assertWorkspaceFileContent(t, fixture.root, intent.MaterializedPath, claimantBytes)
	assertWorkspaceFileContent(t, fixture.root, projected.MaterializedPath, "base\n")
	assertRuntimeTrackedAtPath(t, fixture.runtime, claimantID, fixture.path)
	assertRuntimeTrackedAtPath(t, fixture.runtime, intent.ContentDocumentID, filepath.Join(fixture.root, filepath.FromSlash(projected.MaterializedPath)))
	if _, ok, err := fixture.runtime.docCache.loadRootLocalDeleteIntent(intent.RootDocumentID, intent.ContentDocumentID); err != nil {
		t.Fatalf("load claimant-safe projection workflow: %v", err)
	} else if ok {
		t.Fatal("claimant-safe projection did not clear the exact workflow")
	}
}

func TestReverseWindowProjectionAbsentAfterRestoreStartsFreshTombstone(t *testing.T) {
	fixture := newDeleteCandidateRuntimeFixture(t)
	beginSemanticDeleteCandidate(t, fixture)
	if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(fixture.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatalf("open reverse window: %v", err)
	}
	intent := forceProjectionPendingIntent(t, fixture)

	if err := fixture.runtime.reconcileRootNamespace(fixture.ctx); err != nil {
		t.Fatalf("reconcile absent restored projection: %v", err)
	}

	assertWorkspaceFileMissing(t, fixture.root, fixture.relative)
	got := loadReverseWindowIntent(t, fixture.runtime.docCache, intent.RootDocumentID, intent.ContentDocumentID)
	if got.Phase != rootLocalDeletePhaseTombstonePending || got.ExpectedWindowGeneration != intent.WindowGeneration {
		t.Fatalf("absent restored projection = phase %q expected generation %d, want pending/%d", got.Phase, got.ExpectedWindowGeneration, intent.WindowGeneration)
	}
	if got.TombstoneOperationID == intent.TombstoneOperationID {
		t.Fatal("absent restored projection reused the consumed tombstone operation")
	}
}

func TestReverseWindowDriverExpiryWithOccupantArchivesBeforeFinalization(t *testing.T) {
	fixture := newReverseWindowDecisionFixture(t, rootLocalDeletePhaseWindowOpen)
	const occupant = "late bytes after the authoritative deadline\n"
	if err := os.WriteFile(fixture.path, []byte(occupant), 0o644); err != nil {
		t.Fatalf("write late occupant: %v", err)
	}

	if err := fixture.runtime.finalizeRootLocalDeleteIntent(
		fixture.intent,
		reverseWindowFinalizationExpired,
	); err != nil {
		t.Fatalf("finalize expired reverse window: %v", err)
	}

	assertWorkspaceFileMissing(t, fixture.root, fixture.intent.MaterializedPath)
	assertRecoveredContent(t, fixture.root, fixture.intent.ContentDocumentID, occupant)
	assertReverseWindowIntentMissing(t, fixture)
}

func TestReverseWindowDriverReversalWithOccupantAndActiveClaimNeverClobbers(t *testing.T) {
	fixture := newReverseWindowDecisionFixture(t, rootLocalDeletePhaseRestorePending)
	const occupant = "bytes owned by the newer active path claimant\n"
	if err := os.WriteFile(fixture.path, []byte(occupant), 0o644); err != nil {
		t.Fatalf("write claimed occupant: %v", err)
	}
	seedReverseWindowRoot(t, fixture, true)

	claimed, err := fixture.runtime.rootPathClaimedByOtherActiveEntry(fixture.intent)
	if err != nil {
		t.Fatalf("classify active root claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected the newer active root entry to claim the desired path")
	}
	if err := fixture.runtime.finalizeRootLocalDeleteIntent(
		fixture.intent,
		reverseWindowFinalizationPathClaimed,
	); err != nil {
		t.Fatalf("resolve claimed reversal: %v", err)
	}

	assertWorkspaceFileContent(t, fixture.root, fixture.intent.MaterializedPath, occupant)
	assertReverseWindowIntentMissing(t, fixture)
	recoveredRoot := filepath.Join(
		fixture.root,
		".notty",
		"recovered",
		safeDocumentCacheName(fixture.intent.ContentDocumentID),
	)
	if _, err := os.Stat(recoveredRoot); err == nil {
		t.Fatalf("claimed occupant was incorrectly archived under %s", recoveredRoot)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat recovery directory: %v", err)
	}
}

func TestReverseWindowDriverDecisionTimeRevalidationDefeatsCachedAbsence(t *testing.T) {
	fixture := newReverseWindowDecisionFixture(t, rootLocalDeletePhaseWindowOpen)
	const occupant = "created after observation but before final mutation\n"
	observedThenMutated := false
	fixture.runtime.reverseWindowDecisionHook = func() {
		if observedThenMutated {
			return
		}
		observedThenMutated = true
		if err := os.WriteFile(fixture.path, []byte(occupant), 0o644); err != nil {
			t.Fatalf("write decision-race occupant: %v", err)
		}
	}

	if err := fixture.runtime.finalizeRootLocalDeleteIntent(
		fixture.intent,
		reverseWindowFinalizationExpired,
	); err != nil {
		t.Fatalf("finalize after cached absence: %v", err)
	}
	if !observedThenMutated {
		t.Fatal("decision hook did not mutate the path between observation and commit")
	}

	assertWorkspaceFileMissing(t, fixture.root, fixture.intent.MaterializedPath)
	assertRecoveredContent(t, fixture.root, fixture.intent.ContentDocumentID, occupant)
	assertReverseWindowIntentMissing(t, fixture)
}

func TestReverseWindowDriverCapabilityDisappearanceTerminalizesEveryLivePhaseByteSafely(t *testing.T) {
	for _, phase := range []rootLocalDeletePhase{
		rootLocalDeletePhaseTombstonePending,
		rootLocalDeletePhaseWindowOpen,
		rootLocalDeletePhaseContentSyncing,
		rootLocalDeletePhaseRestorePending,
		rootLocalDeletePhaseProjectionPending,
	} {
		t.Run(string(phase), func(t *testing.T) {
			fixture := newReverseWindowDecisionFixture(t, phase)
			fixture.runtime.replaceWorkspaceCapabilities([]string{documentTombstoneReverseWindowV1})
			fixture.runtime.replaceWorkspaceCapabilities(nil)
			occupant := "bytes retained after capability loss in " + string(phase) + "\n"
			if err := os.WriteFile(fixture.path, []byte(occupant), 0o644); err != nil {
				t.Fatalf("write capability-loss occupant: %v", err)
			}

			if _, err := fixture.runtime.reconcileRootLocalDeleteIntents(context.Background(), time.Now().UTC(), nil); err != nil {
				t.Fatalf("terminalize capability-loss workflow: %v", err)
			}

			assertWorkspaceFileMissing(t, fixture.root, fixture.intent.MaterializedPath)
			assertRecoveredContent(t, fixture.root, fixture.intent.ContentDocumentID, occupant)
			assertReverseWindowIntentMissing(t, fixture)
		})
	}
}

type reverseWindowDecisionFixture struct {
	root    string
	path    string
	runtime *workspaceRuntime
	intent  rootLocalDeleteIntent
}

func newReverseWindowDecisionFixture(t *testing.T, phase rootLocalDeletePhase) reverseWindowDecisionFixture {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
		AgentID:            "daemon_agent",
	}
	runtime, err := newTestWorkspaceRuntime(t, cfg, http.DefaultClient, root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	runtime.rootDocumentID = "root-reverse-test"

	desiredPath := "docs/reversible.md"
	absolutePath := filepath.Join(root, filepath.FromSlash(desiredPath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatalf("mkdir reversible path: %v", err)
	}
	now := time.Now().UTC()
	intent := rootLocalDeleteIntent{
		RootDocumentID:           runtime.rootDocumentID,
		ContentDocumentID:        "doc-reversible",
		EntryID:                  "entry-reversible",
		DesiredPath:              desiredPath,
		MaterializedPath:         desiredPath,
		TombstoneOperationID:     uuid.NewString(),
		ExpectedWindowGeneration: 0,
		WindowGeneration:         1,
		OpenedAt:                 now.Add(-10 * time.Minute),
		ReverseUntil:             now.Add(-5 * time.Minute),
		Phase:                    phase,
		CreatedAt:                now.Add(-10 * time.Minute),
		UpdatedAt:                now,
	}
	if phase == rootLocalDeletePhaseContentSyncing || phase == rootLocalDeletePhaseRestorePending || phase == rootLocalDeletePhaseProjectionPending {
		intent.RestoreOperationID = uuid.NewString()
		intent.RequiredContentStateVector = []byte{0}
	}
	if err := runtime.docCache.storeRootLocalDeleteIntent(intent); err != nil {
		t.Fatalf("store reverse-window intent: %v", err)
	}
	return reverseWindowDecisionFixture{root: root, path: absolutePath, runtime: runtime, intent: intent}
}

func seedReverseWindowRoot(t *testing.T, fixture reverseWindowDecisionFixture, withOtherActiveClaim bool) {
	t.Helper()
	doc := crdt.New()
	defer doc.Close()
	rootMap := doc.GetMap(rootMapName)
	if _, err := doc.Update(func(txn *crdt.Transaction) error {
		entries, err := rootMap.SetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if err := setRootFileEntry(txn, entries, rootEntry{
			EntryID:           fixture.intent.EntryID,
			ContentDocumentID: fixture.intent.ContentDocumentID,
			Name:              fixture.intent.DesiredPath,
			Deleted:           true,
		}); err != nil {
			return err
		}
		if !withOtherActiveClaim {
			return nil
		}
		return setRootFileEntry(txn, entries, rootEntry{
			EntryID:           "entry-newer-claim",
			ContentDocumentID: "doc-newer-claim",
			Name:              fixture.intent.DesiredPath,
		})
	}, "seed-reverse-window-root"); err != nil {
		t.Fatalf("seed reverse-window root: %v", err)
	}
	if err := fixture.runtime.docCache.storeDoc(fixture.intent.RootDocumentID, rootDocumentPath, 1, doc); err != nil {
		t.Fatalf("store reverse-window root: %v", err)
	}
}

func assertReverseWindowIntentMissing(t *testing.T, fixture reverseWindowDecisionFixture) {
	t.Helper()
	if _, ok, err := fixture.runtime.docCache.loadRootLocalDeleteIntent(
		fixture.intent.RootDocumentID,
		fixture.intent.ContentDocumentID,
	); err != nil {
		t.Fatalf("load finalized reverse-window intent: %v", err)
	} else if ok {
		t.Fatal("finalized reverse-window intent is still durable")
	}
}

func beginSemanticDeleteCandidate(t *testing.T, fixture *deleteCandidateRuntimeFixture) {
	t.Helper()
	fixture.server.setCapabilities(documentTombstoneReverseWindowV1)
	fixture.runtime.replaceWorkspaceCapabilities([]string{documentTombstoneReverseWindowV1})
	fixture.confirmDeleteCandidate(t)
	if err := fixture.runtime.reconcileDirtyDocuments(fixture.ctx); err != nil {
		t.Fatalf("persist semantic delete candidate: %v", err)
	}
	intent := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	if intent.Phase != rootLocalDeletePhaseTombstonePending || intent.TombstoneOperationID == "" {
		t.Fatalf("initial semantic delete intent = %#v", intent)
	}
}

func assertReverseWindowPhase(t *testing.T, cache *documentCache, rootDocumentID, contentDocumentID string, want rootLocalDeletePhase) {
	t.Helper()
	got := loadReverseWindowIntent(t, cache, rootDocumentID, contentDocumentID)
	if got.Phase != want {
		t.Fatalf("reverse-window phase = %q, want %q", got.Phase, want)
	}
}

func loadReverseWindowIntent(t *testing.T, cache *documentCache, rootDocumentID, contentDocumentID string) rootLocalDeleteIntent {
	t.Helper()
	intent, ok, err := cache.loadRootLocalDeleteIntent(rootDocumentID, contentDocumentID)
	if err != nil {
		t.Fatalf("load reverse-window intent: %v", err)
	}
	if !ok {
		t.Fatal("reverse-window intent is missing")
	}
	return intent
}

func forceProjectionPendingIntent(t *testing.T, fixture *deleteCandidateRuntimeFixture) rootLocalDeleteIntent {
	t.Helper()
	intent := loadReverseWindowIntent(t, fixture.runtime.docCache, fixture.server.rootDocumentID, fixture.tracked.DocumentID)
	intent.Phase = rootLocalDeletePhaseProjectionPending
	intent.RestoreOperationID = uuid.NewString()
	intent.RequiredContentStateVector = []byte{0}
	intent.ObservedFileIdentity = "test-identity"
	intent.ObservedContentSHA256 = "test-content-hash"
	intent.UpdatedAt = time.Now().UTC()
	if err := fixture.runtime.docCache.storeRootLocalDeleteIntent(intent); err != nil {
		t.Fatalf("force projection-pending intent: %v", err)
	}
	return intent
}

func seedProjectionConflict(t *testing.T, fixture *deleteCandidateRuntimeFixture, claimantID, claimantContent string) {
	t.Helper()
	claims, err := fixture.runtime.docCache.loadLocalNamespaceIntentsByStatus(localNamespaceIntentPending, localNamespaceIntentResolved)
	if err != nil {
		t.Fatalf("load original namespace claim: %v", err)
	}
	for _, claim := range claims {
		if claim.DocumentID != fixture.tracked.DocumentID {
			continue
		}
		if err := fixture.runtime.docCache.updateLocalNamespaceIntentStatus(claim.ID, localNamespaceIntentFailed); err != nil {
			t.Fatalf("retire original namespace claim: %v", err)
		}
	}
	claimantDoc := newDocWithText(t, claimantContent)
	defer claimantDoc.Close()
	if err := fixture.runtime.docCache.storeDoc(claimantID, fixture.relative, 1, claimantDoc); err != nil {
		t.Fatalf("store claimant content document: %v", err)
	}
	if err := fixture.runtime.mutateRootDoc(fixture.ctx, fixture.cfg.AgentID, "daemon", func(doc *crdt.Doc) ([]byte, error) {
		return UpsertRootFile(doc, claimantID, fixture.relative, rootMutationActor{ID: fixture.cfg.AgentID, Kind: "daemon"})
	}); err != nil {
		t.Fatalf("seed concurrent root claimant: %v", err)
	}
}

func plannedProjectionForDocument(t *testing.T, runtime *workspaceRuntime, documentID string) rootProjectionEntry {
	t.Helper()
	previous, err := runtime.docCache.loadRootProjectionEntries(runtime.rootDocumentID)
	if err != nil {
		t.Fatalf("load previous root projection: %v", err)
	}
	entry, unlock := runtime.docCache.lockEntry(runtime.rootDocumentID)
	doc, _, _, err := runtime.docCache.loadBaseDocLocked(entry, runtime.rootDocumentID, rootDocumentPath)
	if err != nil {
		unlock()
		t.Fatalf("load root document for projection plan: %v", err)
	}
	mirror, err := DecodeRootCRDTMirror(doc)
	doc.Close()
	unlock()
	if err != nil {
		t.Fatalf("decode root projection plan: %v", err)
	}
	claims, err := runtime.docCache.loadLocalNamespaceProjectionClaims()
	if err != nil {
		t.Fatalf("load root projection claims: %v", err)
	}
	plan := RootProjectionPlanner{}.Plan(RootProjectionPlannerInput{
		Previous:    previous,
		Mirror:      mirror,
		LocalClaims: claims,
	})
	for _, projected := range plan.Next {
		if projected.Active && projected.ContentDocumentID == documentID {
			return projected
		}
	}
	t.Fatalf("missing active projection for %s: %#v", documentID, plan.Next)
	return rootProjectionEntry{}
}
