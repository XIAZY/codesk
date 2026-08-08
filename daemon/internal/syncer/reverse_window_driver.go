package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type reverseWindowFinalization string

const (
	reverseWindowFinalizationExpired     reverseWindowFinalization = "expired"
	reverseWindowFinalizationPathClaimed reverseWindowFinalization = "path_claimed"
)

type rootLocalDeleteReconcileResult struct {
	LocalCreates []localCreateCandidate
	NextWake     time.Time
}

type daemonOpenReverseWindowRequest struct {
	OperationID              string `json:"operationId"`
	ExpectedWindowGeneration int64  `json:"expectedWindowGeneration"`
	EntryID                  string `json:"entryId"`
	ContentDocumentID        string `json:"contentDocumentId"`
	ExpectedDesiredPath      string `json:"expectedDesiredPath"`
}

type daemonOpenReverseWindowResult struct {
	Outcome                 string    `json:"outcome"`
	WindowGeneration        int64     `json:"windowGeneration"`
	CurrentWindowGeneration int64     `json:"currentWindowGeneration"`
	OpenedAt                time.Time `json:"openedAt"`
	ReverseUntil            time.Time `json:"reverseUntil"`
	TombstoneUpdateID       int64     `json:"tombstoneUpdateId"`
}

type daemonConsumeReverseWindowRequest struct {
	TombstoneOperationID string `json:"tombstoneOperationId"`
	RestoreOperationID   string `json:"restoreOperationId"`
	WindowGeneration     int64  `json:"windowGeneration"`
	ContentStateVector   []byte `json:"contentStateVector"`
}

type daemonConsumeReverseWindowResult struct {
	Outcome          string    `json:"outcome"`
	WindowGeneration int64     `json:"windowGeneration"`
	ConsumedAt       time.Time `json:"consumedAt"`
	RestoreUpdateID  int64     `json:"restoreUpdateId"`
}

type reverseWindowPathObservation struct {
	kind         workspacePathOccupantKind
	identity     string
	contentSHA   string
	contentBytes []byte
}

func (r *workspaceRuntime) prepareRootReverseProjection(ctx context.Context, plan rootProjectionPlan) (bool, error) {
	if r == nil || r.docCache == nil || strings.TrimSpace(r.rootDocumentID) == "" {
		return false, nil
	}
	intents, err := r.docCache.loadRootLocalDeleteIntents(r.rootDocumentID)
	if err != nil {
		return false, err
	}
	for _, intent := range intents {
		if intent.Phase != rootLocalDeletePhaseProjectionPending {
			continue
		}
		projected, ok := exactActiveRootProjection(plan.Next, intent)
		if !ok {
			continue
		}
		observation, err := r.observeReverseWindowPath(intent)
		if err != nil {
			return false, err
		}
		trackedByOther := r.reverseWindowPathTrackedByOtherDocument(intent)
		if observation.kind == workspacePathRegularFile && !trackedByOther &&
			(observation.identity != intent.ObservedFileIdentity || observation.contentSHA != intent.ObservedContentSHA256) {
			current, deferred, err := r.reconcileRootReverseProjectionContent(ctx, intent, observation)
			if err != nil {
				return false, err
			}
			if deferred {
				return true, nil
			}
			observation = current
			intent.ObservedFileIdentity = current.identity
			intent.ObservedContentSHA256 = current.contentSHA
		}
		if projected.MaterializedPath != intent.MaterializedPath {
			deferred, err := r.resolveRootReverseProjectionConflict(ctx, intent, projected, observation)
			if err != nil {
				return false, err
			}
			if deferred {
				return true, nil
			}
			continue
		}
		if trackedByOther {
			r.markDocumentDirty(r.rootDocumentID)
			return true, nil
		}
		if observation.kind != workspacePathRegularFile {
			if intent.ObservedContentSHA256 == "" {
				if err := r.scheduleRootReverseProjectionMaterialization(ctx, intent); err != nil {
					return false, err
				}
				r.markDocumentDirty(r.rootDocumentID)
				return true, nil
			}
			if err := r.docCache.replaceRootLocalDeleteIntentAfterProjectionAbsence(intent, uuid.NewString(), time.Now().UTC()); err != nil {
				return false, err
			}
			r.markDocumentDirty(rootLocalDeleteReconcileWake)
			r.markDocumentDirty(r.rootDocumentID)
			return true, nil
		}
	}
	return false, nil
}

func (r *workspaceRuntime) reconcileRootReverseProjectionContent(
	ctx context.Context,
	intent rootLocalDeleteIntent,
	observation reverseWindowPathObservation,
) (reverseWindowPathObservation, bool, error) {
	r.prepareTrackedReverseWindowContent(intent)
	if err := r.ensureTrackedRootReverseProjection(ctx, intent); err != nil {
		return reverseWindowPathObservation{}, false, err
	}
	if err := r.reconcileDocumentIDs(ctx, []string{intent.ContentDocumentID}); err != nil {
		return reverseWindowPathObservation{}, false, err
	}
	frontier := r.docCache.localStateVector(intent.ContentDocumentID)
	if len(frontier) == 0 {
		return reverseWindowPathObservation{}, false, errors.New("post-restore content merge produced an empty state vector")
	}
	if err := r.awaitBackendStateVector(ctx, intent.ContentDocumentID, frontier); err != nil {
		return reverseWindowPathObservation{}, false, err
	}
	current, err := r.observeReverseWindowPath(intent)
	if err != nil {
		return reverseWindowPathObservation{}, false, err
	}
	if current.kind != workspacePathRegularFile || current.identity != observation.identity || current.contentSHA != observation.contentSHA {
		r.markDocumentDirty(r.rootDocumentID)
		return current, true, nil
	}
	if err := r.docCache.updateRootLocalDeleteProjectionObservation(intent, current.identity, current.contentSHA, time.Now().UTC()); err != nil {
		return reverseWindowPathObservation{}, false, err
	}
	return current, false, nil
}

func (r *workspaceRuntime) resolveRootReverseProjectionConflict(
	ctx context.Context,
	intent rootLocalDeleteIntent,
	projected rootProjectionEntry,
	sourceObservation reverseWindowPathObservation,
) (bool, error) {
	targetPath, err := normalizeVisibleRootPath(projected.MaterializedPath)
	if err != nil {
		return false, err
	}
	targetIntent := intent
	targetIntent.MaterializedPath = targetPath
	targetAbsolutePath, err := r.reverseWindowAbsolutePath(targetIntent)
	if err != nil {
		return false, err
	}
	sourceAbsolutePath, err := r.reverseWindowAbsolutePath(intent)
	if err != nil {
		return false, err
	}
	targetObservation, err := r.observeReverseWindowPath(targetIntent)
	if err != nil {
		return false, err
	}

	r.replica.mu.Lock()
	trackedByID := r.replica.projectedByID[intent.ContentDocumentID]
	sourceOwner := r.replica.projectedByPath[sourceAbsolutePath]
	targetOwner := r.replica.projectedByPath[targetAbsolutePath]
	r.replica.mu.Unlock()
	if targetOwner != nil && targetOwner.DocumentID != intent.ContentDocumentID {
		r.deferRootReverseProjectionConflict()
		return true, nil
	}
	if targetObservation.kind == workspacePathRegularFile {
		if !observationMatchesReverseProjectionProof(targetObservation, intent) {
			r.deferRootReverseProjectionConflict()
			return true, nil
		}
		if trackedByID != nil && filepath.Clean(trackedByID.Path) != filepath.Clean(targetAbsolutePath) {
			trackedByID.untrack()
		}
		if err := r.ensureTrackedRootReverseProjection(ctx, targetIntent); err != nil {
			return false, err
		}
		if err := r.docCache.rekeyRootLocalDeleteProjectionPath(
			intent,
			targetPath,
			targetObservation.identity,
			targetObservation.contentSHA,
			time.Now().UTC(),
		); err != nil {
			return false, err
		}
		return false, nil
	}
	if targetObservation.kind != workspacePathAbsent {
		r.deferRootReverseProjectionConflict()
		return true, nil
	}

	sourceOwnedByOther := sourceOwner != nil && sourceOwner.DocumentID != intent.ContentDocumentID
	if !sourceOwnedByOther && observationMatchesReverseProjectionProof(sourceObservation, intent) {
		fs, err := requireWorkspaceFS(r.replica.fs, r.replica.rootDir)
		if err != nil {
			return false, err
		}
		if err := fs.MoveIfNoTarget(sourceAbsolutePath, targetAbsolutePath); err != nil {
			if errors.Is(err, ErrPathCollision) {
				r.deferRootReverseProjectionConflict()
				return true, nil
			}
			return false, err
		}
		if err := r.replica.addWatchDir(filepath.Dir(targetAbsolutePath)); err != nil {
			return false, err
		}
		if trackedByID != nil {
			trackedByID.DocumentPath = targetPath
			trackedByID.setFilesystemPath(targetAbsolutePath)
		} else if err := r.ensureTrackedRootReverseProjection(ctx, targetIntent); err != nil {
			return false, err
		}
		targetObservation, err = r.observeReverseWindowPath(targetIntent)
		if err != nil {
			return false, err
		}
		if !observationMatchesReverseProjectionProof(targetObservation, intent) {
			return false, errors.New("reverse-window conflict relocation changed the proven restored bytes")
		}
		if err := r.docCache.rekeyRootLocalDeleteProjectionPath(
			intent,
			targetPath,
			targetObservation.identity,
			targetObservation.contentSHA,
			time.Now().UTC(),
		); err != nil {
			return false, err
		}
		return false, nil
	}

	if err := r.docCache.rekeyRootLocalDeleteProjectionPath(intent, targetPath, "", "", time.Now().UTC()); err != nil {
		return false, err
	}
	if trackedByID != nil && trackedByID != sourceOwner {
		trackedByID.untrack()
	}
	if err := r.scheduleRootReverseProjectionMaterialization(ctx, targetIntent); err != nil {
		return false, err
	}
	r.markDocumentDirty(r.rootDocumentID)
	return true, nil
}

func observationMatchesReverseProjectionProof(observation reverseWindowPathObservation, intent rootLocalDeleteIntent) bool {
	return observation.kind == workspacePathRegularFile &&
		intent.ObservedContentSHA256 != "" &&
		observation.identity == intent.ObservedFileIdentity &&
		observation.contentSHA == intent.ObservedContentSHA256
}

func (r *workspaceRuntime) ensureTrackedRootReverseProjection(ctx context.Context, intent rootLocalDeleteIntent) error {
	absolutePath, err := r.reverseWindowAbsolutePath(intent)
	if err != nil {
		return err
	}
	r.replica.mu.Lock()
	trackedByID := r.replica.projectedByID[intent.ContentDocumentID]
	trackedAtPath := r.replica.projectedByPath[absolutePath]
	r.replica.mu.Unlock()
	if trackedAtPath != nil && trackedAtPath.DocumentID != intent.ContentDocumentID {
		return fmt.Errorf("restored projection path %q is tracked by document %s", intent.MaterializedPath, trackedAtPath.DocumentID)
	}
	if trackedByID != nil && filepath.Clean(trackedByID.Path) != filepath.Clean(absolutePath) {
		trackedByID.untrack()
	}
	return r.replica.ensureTracked(ctx, &document{ID: intent.ContentDocumentID, Path: intent.MaterializedPath})
}

func (r *workspaceRuntime) scheduleRootReverseProjectionMaterialization(ctx context.Context, intent rootLocalDeleteIntent) error {
	if err := r.ensureTrackedRootReverseProjection(ctx, intent); err != nil {
		return err
	}
	r.replica.mu.Lock()
	tracked := r.replica.projectedByID[intent.ContentDocumentID]
	r.replica.mu.Unlock()
	if tracked == nil {
		return errors.New("restored projection materialization did not establish tracking")
	}
	baseContent, baseState, baseKnown, err := tracked.loadProjectedBase()
	if err != nil {
		return err
	}
	if baseKnown {
		projectedSeq, err := r.docCache.documentAppliedSeq(intent.ContentDocumentID)
		if err != nil {
			return err
		}
		fs, err := tracked.workspaceFS()
		if err != nil {
			return err
		}
		clean, err := applyProjectedContentWithWrite(
			tracked,
			baseContent,
			baseState,
			projectedSeq,
			func(path string, _ projectedContentHash, content []byte) error {
				return fs.WriteIfUnchanged(path, projectedHashBytes(nil), content)
			},
		)
		if err != nil {
			return err
		}
		if !clean {
			r.deferRootReverseProjectionConflict()
			return nil
		}
	} else {
		tracked.setProjectedSnapshot(projectedContentHash{}, false)
		r.markDocumentDirty(intent.ContentDocumentID)
	}
	tracked.clearLocalDirty()
	tracked.clearLocalDeleted()
	tracked.clearRemoteDeleted()
	return nil
}

func (r *workspaceRuntime) deferRootReverseProjectionConflict() {
	r.markDocumentDirty(localPathChangeReconcileWake)
	r.markDocumentDirty(r.rootDocumentID)
}

func (r *workspaceRuntime) rootReverseProjectionClearProofs(entries []rootProjectionEntry) ([]rootProjectionClearProof, error) {
	if r == nil || r.docCache == nil || r.replica == nil || strings.TrimSpace(r.rootDocumentID) == "" {
		return nil, nil
	}
	intents, err := r.docCache.loadRootLocalDeleteIntents(r.rootDocumentID)
	if err != nil {
		return nil, err
	}
	var proofs []rootProjectionClearProof
	for _, intent := range intents {
		if intent.Phase != rootLocalDeletePhaseProjectionPending {
			continue
		}
		projected, ok := exactActiveRootProjection(entries, intent)
		if !ok || projected.MaterializedPath != intent.MaterializedPath {
			continue
		}
		absolutePath, err := r.reverseWindowAbsolutePath(intent)
		if err != nil {
			return nil, err
		}
		r.replica.mu.Lock()
		trackedByID := r.replica.projectedByID[intent.ContentDocumentID]
		trackedByPath := r.replica.projectedByPath[absolutePath]
		r.replica.mu.Unlock()
		if trackedByID == nil || trackedByID != trackedByPath || filepath.Clean(trackedByID.Path) != filepath.Clean(absolutePath) {
			continue
		}
		observation, err := r.observeReverseWindowPath(intent)
		if err != nil {
			return nil, err
		}
		if observation.kind != workspacePathRegularFile ||
			observation.identity != intent.ObservedFileIdentity ||
			observation.contentSHA != intent.ObservedContentSHA256 {
			continue
		}
		proofs = append(proofs, rootProjectionClearProof{
			RootDocumentID:       intent.RootDocumentID,
			ContentDocumentID:    intent.ContentDocumentID,
			EntryID:              intent.EntryID,
			DesiredPath:          intent.DesiredPath,
			MaterializedPath:     intent.MaterializedPath,
			TombstoneOperationID: intent.TombstoneOperationID,
			RestoreOperationID:   intent.RestoreOperationID,
			WindowGeneration:     intent.WindowGeneration,
		})
	}
	return proofs, nil
}

func exactActiveRootProjection(entries []rootProjectionEntry, intent rootLocalDeleteIntent) (rootProjectionEntry, bool) {
	for _, entry := range entries {
		if entry.Active &&
			entry.EntryID == intent.EntryID &&
			entry.ContentDocumentID == intent.ContentDocumentID &&
			entry.DesiredPath == intent.DesiredPath {
			return entry, true
		}
	}
	return rootProjectionEntry{}, false
}

func (r *workspaceRuntime) beginSemanticRootLocalDelete(contentDocumentID string) (bool, error) {
	if r == nil || r.docCache == nil || r.replica == nil || strings.TrimSpace(r.rootDocumentID) == "" {
		return false, nil
	}
	contentDocumentID = strings.TrimSpace(contentDocumentID)
	if contentDocumentID == "" {
		return false, nil
	}
	if existing, ok, err := r.docCache.loadRootLocalDeleteIntent(r.rootDocumentID, contentDocumentID); err != nil {
		return false, err
	} else if ok {
		return existing.Phase != rootLocalDeletePhaseLegacyFloor, nil
	}
	projection, err := r.docCache.loadRootProjectionEntries(r.rootDocumentID)
	if err != nil {
		return false, err
	}
	var projected rootProjectionEntry
	for _, candidate := range projection {
		if candidate.Active && candidate.ContentDocumentID == contentDocumentID {
			projected = candidate
			break
		}
	}
	if projected.EntryID == "" || projected.DesiredPath == "" || projected.MaterializedPath == "" {
		return false, errors.New("semantic root delete requires an active projected namespace identity")
	}
	intent := rootLocalDeleteIntent{
		RootDocumentID:           r.rootDocumentID,
		ContentDocumentID:        contentDocumentID,
		EntryID:                  projected.EntryID,
		DesiredPath:              projected.DesiredPath,
		MaterializedPath:         projected.MaterializedPath,
		TombstoneOperationID:     uuid.NewString(),
		ExpectedWindowGeneration: 0,
		Phase:                    rootLocalDeletePhaseTombstonePending,
		CreatedAt:                time.Now().UTC(),
		UpdatedAt:                time.Now().UTC(),
	}
	if err := r.docCache.beginRootLocalDeleteIntent(intent); err != nil {
		return false, err
	}
	r.markDocumentDirty(rootLocalDeleteReconcileWake)
	return true, nil
}

func (r *workspaceRuntime) reconcileRootLocalDeleteIntents(
	ctx context.Context,
	now time.Time,
	localCreates []localCreateCandidate,
) (rootLocalDeleteReconcileResult, error) {
	result := rootLocalDeleteReconcileResult{LocalCreates: append([]localCreateCandidate(nil), localCreates...)}
	if r == nil || r.docCache == nil || r.replica == nil || strings.TrimSpace(r.rootDocumentID) == "" {
		return result, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	intents, err := r.docCache.loadRootLocalDeleteIntents(r.rootDocumentID)
	if err != nil {
		return result, err
	}
	consumedCreatePaths := map[string]struct{}{}
	for _, intent := range intents {
		if intent.Phase == rootLocalDeletePhaseLegacyFloor {
			continue
		}
		absolutePath, err := r.reverseWindowAbsolutePath(intent)
		if err != nil {
			return result, err
		}
		for _, candidate := range localCreates {
			if filepath.Clean(candidate.Path) == filepath.Clean(absolutePath) {
				consumedCreatePaths[filepath.Clean(candidate.Path)] = struct{}{}
			}
		}
		if !r.supportsWorkspaceCapability(documentTombstoneReverseWindowV1) {
			if err := r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationExpired); err != nil {
				return result, err
			}
			continue
		}
		nextWake, err := r.reconcileRootLocalDeleteIntent(ctx, now, intent)
		if err != nil {
			return result, err
		}
		result.NextWake = earlierNonZeroTime(result.NextWake, nextWake)
	}
	result.LocalCreates = result.LocalCreates[:0]
	for _, candidate := range localCreates {
		if _, consumed := consumedCreatePaths[filepath.Clean(candidate.Path)]; !consumed {
			result.LocalCreates = append(result.LocalCreates, candidate)
		}
	}
	return result, nil
}

func (r *workspaceRuntime) reconcileRootLocalDeleteIntent(ctx context.Context, now time.Time, intent rootLocalDeleteIntent) (time.Time, error) {
	switch intent.Phase {
	case rootLocalDeletePhaseTombstonePending:
		return time.Time{}, r.reconcilePendingRootTombstone(ctx, now, intent)
	case rootLocalDeletePhaseWindowOpen:
		return r.reconcileOpenRootDeleteWindow(now, intent)
	case rootLocalDeletePhaseContentSyncing:
		return r.reconcileRootDeleteContent(ctx, now, intent)
	case rootLocalDeletePhaseRestorePending:
		return r.reconcilePendingRootRestore(ctx, now, intent)
	case rootLocalDeletePhaseProjectionPending:
		r.markDocumentDirty(r.rootDocumentID)
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported root local-delete phase %q", intent.Phase)
	}
}

func (r *workspaceRuntime) reconcilePendingRootTombstone(ctx context.Context, now time.Time, intent rootLocalDeleteIntent) error {
	if intent.Attempts == 0 {
		if r.reverseWindowDecisionHook != nil {
			r.reverseWindowDecisionHook()
		}
		observation, err := r.observeReverseWindowPath(intent)
		if err != nil {
			return err
		}
		if observation.kind == workspacePathRegularFile {
			if err := r.docCache.deleteRootLocalDeleteIntent(intent); err != nil {
				return err
			}
			r.prepareTrackedReverseWindowContent(intent)
			r.markDocumentDirty(intent.ContentDocumentID)
			return nil
		}
	}
	if _, err := r.docCache.recordRootLocalDeleteIntentAttempt(
		intent.RootDocumentID,
		intent.ContentDocumentID,
		intent.TombstoneOperationID,
		intent.ExpectedWindowGeneration,
		now,
	); err != nil {
		return err
	}
	var response daemonOpenReverseWindowResult
	err := r.postReverseWindowJSON(ctx, "/api/documents/reverse-window/open", daemonOpenReverseWindowRequest{
		OperationID:              intent.TombstoneOperationID,
		ExpectedWindowGeneration: intent.ExpectedWindowGeneration,
		EntryID:                  intent.EntryID,
		ContentDocumentID:        intent.ContentDocumentID,
		ExpectedDesiredPath:      intent.DesiredPath,
	}, &response)
	if err != nil {
		return err
	}
	switch response.Outcome {
	case "accepted_new", "exact_replay_stored_result":
		if response.WindowGeneration <= 0 || response.OpenedAt.IsZero() || !response.ReverseUntil.After(response.OpenedAt) {
			return errors.New("backend returned an invalid accepted reverse window")
		}
		if err := r.docCache.acceptRootLocalDeleteWindow(
			intent.RootDocumentID,
			intent.ContentDocumentID,
			intent.TombstoneOperationID,
			intent.ExpectedWindowGeneration,
			response.WindowGeneration,
			response.OpenedAt,
			response.ReverseUntil,
			now,
		); err != nil {
			return err
		}
		r.markDocumentDirty(r.rootDocumentID)
		r.markDocumentDirty(rootLocalDeleteReconcileWake)
		return nil
	case "window_generation_conflict":
		if response.CurrentWindowGeneration < 0 {
			return errors.New("backend returned a negative reverse-window generation")
		}
		if err := r.docCache.replaceRootLocalDeleteIntentAfterGenerationConflict(
			intent.RootDocumentID,
			intent.ContentDocumentID,
			intent.TombstoneOperationID,
			intent.ExpectedWindowGeneration,
			uuid.NewString(),
			response.CurrentWindowGeneration,
			now,
		); err != nil {
			return err
		}
		r.markDocumentDirty(rootLocalDeleteReconcileWake)
		return nil
	case "operation_mismatch":
		return r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationExpired)
	default:
		return fmt.Errorf("unsupported reverse-window open outcome %q", response.Outcome)
	}
}

func (r *workspaceRuntime) reconcileOpenRootDeleteWindow(now time.Time, intent rootLocalDeleteIntent) (time.Time, error) {
	observation, err := r.observeReverseWindowPath(intent)
	if err != nil {
		return time.Time{}, err
	}
	if observation.kind == workspacePathRegularFile {
		claimed, err := r.rootPathClaimedByOtherActiveEntry(intent)
		if err != nil {
			return time.Time{}, err
		}
		if claimed || r.reverseWindowPathTrackedByOtherDocument(intent) {
			return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationPathClaimed)
		}
		restoreOperationID := uuid.NewString()
		if err := r.docCache.beginRootLocalDeleteRestore(
			intent.RootDocumentID,
			intent.ContentDocumentID,
			intent.TombstoneOperationID,
			restoreOperationID,
			intent.WindowGeneration,
			observation.identity,
			observation.contentSHA,
			now,
		); err != nil {
			return time.Time{}, err
		}
		r.prepareTrackedReverseWindowContent(intent)
		r.markDocumentDirty(rootLocalDeleteReconcileWake)
		return time.Time{}, nil
	}
	if !now.Before(intent.ReverseUntil) {
		return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationExpired)
	}
	return intent.ReverseUntil, nil
}

func (r *workspaceRuntime) reconcileRootDeleteContent(ctx context.Context, now time.Time, intent rootLocalDeleteIntent) (time.Time, error) {
	if !now.Before(intent.ReverseUntil) {
		return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationExpired)
	}
	claimed, err := r.rootPathClaimedByOtherActiveEntry(intent)
	if err != nil {
		return time.Time{}, err
	}
	if claimed || r.reverseWindowPathTrackedByOtherDocument(intent) {
		return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationPathClaimed)
	}
	observation, err := r.observeReverseWindowPath(intent)
	if err != nil {
		return time.Time{}, err
	}
	if observation.kind != workspacePathRegularFile {
		return intent.ReverseUntil, nil
	}
	if observation.identity != intent.ObservedFileIdentity || observation.contentSHA != intent.ObservedContentSHA256 {
		if err := r.docCache.updateRootLocalDeleteObservation(intent, observation.identity, observation.contentSHA, now); err != nil {
			return time.Time{}, err
		}
		intent.ObservedFileIdentity = observation.identity
		intent.ObservedContentSHA256 = observation.contentSHA
	}
	r.prepareTrackedReverseWindowContent(intent)
	if err := r.replica.ensureTracked(ctx, &document{ID: intent.ContentDocumentID, Path: intent.MaterializedPath}); err != nil {
		return time.Time{}, err
	}
	if err := r.reconcileDocumentIDs(ctx, []string{intent.ContentDocumentID}); err != nil {
		return time.Time{}, err
	}
	frontier := r.docCache.localStateVector(intent.ContentDocumentID)
	if len(frontier) == 0 {
		return time.Time{}, errors.New("reverse-window content sync produced an empty state vector")
	}
	if err := r.awaitBackendStateVector(ctx, intent.ContentDocumentID, frontier); err != nil {
		return time.Time{}, err
	}
	current, err := r.observeReverseWindowPath(intent)
	if err != nil {
		return time.Time{}, err
	}
	if current.kind != workspacePathRegularFile || current.identity != observation.identity || current.contentSHA != observation.contentSHA {
		if current.kind == workspacePathRegularFile {
			if err := r.docCache.updateRootLocalDeleteObservation(intent, current.identity, current.contentSHA, now); err != nil {
				return time.Time{}, err
			}
		}
		r.markDocumentDirty(rootLocalDeleteReconcileWake)
		return time.Time{}, nil
	}
	if err := r.docCache.markRootLocalDeleteRestorePending(
		intent.RootDocumentID,
		intent.ContentDocumentID,
		intent.TombstoneOperationID,
		intent.RestoreOperationID,
		intent.WindowGeneration,
		frontier,
		now,
	); err != nil {
		return time.Time{}, err
	}
	r.markDocumentDirty(rootLocalDeleteReconcileWake)
	return time.Time{}, nil
}

func (r *workspaceRuntime) reconcilePendingRootRestore(ctx context.Context, now time.Time, intent rootLocalDeleteIntent) (time.Time, error) {
	claimed, err := r.rootPathClaimedByOtherActiveEntry(intent)
	if err != nil {
		return time.Time{}, err
	}
	if claimed || r.reverseWindowPathTrackedByOtherDocument(intent) {
		return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationPathClaimed)
	}
	observation, err := r.observeReverseWindowPath(intent)
	if err != nil {
		return time.Time{}, err
	}
	if observation.kind != workspacePathRegularFile {
		if !now.Before(intent.ReverseUntil) {
			return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationExpired)
		}
		return intent.ReverseUntil, nil
	}
	if observation.identity != intent.ObservedFileIdentity || observation.contentSHA != intent.ObservedContentSHA256 {
		if err := r.docCache.updateRootLocalDeleteObservation(intent, observation.identity, observation.contentSHA, now); err != nil {
			return time.Time{}, err
		}
		r.markDocumentDirty(rootLocalDeleteReconcileWake)
		return time.Time{}, nil
	}
	if err := r.docCache.recordRootLocalDeleteRestoreAttempt(intent, now); err != nil {
		return time.Time{}, err
	}
	var response daemonConsumeReverseWindowResult
	err = r.postReverseWindowJSON(ctx, "/api/documents/reverse-window/consume", daemonConsumeReverseWindowRequest{
		TombstoneOperationID: intent.TombstoneOperationID,
		RestoreOperationID:   intent.RestoreOperationID,
		WindowGeneration:     intent.WindowGeneration,
		ContentStateVector:   intent.RequiredContentStateVector,
	}, &response)
	if err != nil {
		var statusErr *backendStatusError
		if !errors.As(err, &statusErr) {
			return time.Time{}, err
		}
		switch statusErr.StatusCode {
		case http.StatusGone:
			return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationExpired)
		case http.StatusConflict:
			return time.Time{}, r.finalizeRootLocalDeleteIntent(intent, reverseWindowFinalizationPathClaimed)
		case http.StatusPreconditionFailed:
			if err := r.awaitBackendStateVector(ctx, intent.ContentDocumentID, intent.RequiredContentStateVector); err != nil {
				return time.Time{}, err
			}
			r.markDocumentDirty(rootLocalDeleteReconcileWake)
			return time.Time{}, nil
		default:
			return time.Time{}, err
		}
	}
	if response.Outcome != "accepted" && response.Outcome != "exact_replay_stored_result" {
		return time.Time{}, fmt.Errorf("unsupported reverse-window consume outcome %q", response.Outcome)
	}
	if response.WindowGeneration != intent.WindowGeneration {
		return time.Time{}, errors.New("backend returned a mismatched consumed reverse-window generation")
	}
	if err := r.docCache.markRootLocalDeleteProjectionPending(intent, now); err != nil {
		return time.Time{}, err
	}
	r.markDocumentDirty(r.rootDocumentID)
	r.markDocumentDirty(rootLocalDeleteReconcileWake)
	return time.Time{}, nil
}

func (r *workspaceRuntime) awaitBackendStateVector(ctx context.Context, documentID string, frontier []byte) error {
	if r.waitForBackendStateVector != nil {
		return r.waitForBackendStateVector(ctx, documentID, frontier)
	}
	if r.documentSocket == nil {
		return errors.New("reverse-window content proof requires a document socket")
	}
	return r.documentSocket.WaitForBackendStateVector(ctx, documentID, frontier)
}

func (r *workspaceRuntime) prepareTrackedReverseWindowContent(intent rootLocalDeleteIntent) {
	if r == nil || r.replica == nil {
		return
	}
	r.replica.mu.Lock()
	tracked := r.replica.projectedByID[intent.ContentDocumentID]
	r.replica.mu.Unlock()
	if tracked == nil {
		return
	}
	tracked.clearLocalDeleted()
	tracked.clearRemoteDeleted()
	tracked.markLocalDirty()
}

func (r *workspaceRuntime) reverseWindowPathTrackedByOtherDocument(intent rootLocalDeleteIntent) bool {
	if r == nil || r.replica == nil {
		return false
	}
	absolutePath, err := r.reverseWindowAbsolutePath(intent)
	if err != nil {
		return true
	}
	r.replica.mu.Lock()
	tracked := r.replica.projectedByPath[absolutePath]
	r.replica.mu.Unlock()
	return tracked != nil && tracked.DocumentID != intent.ContentDocumentID
}

func (r *workspaceRuntime) observeReverseWindowPath(intent rootLocalDeleteIntent) (reverseWindowPathObservation, error) {
	absolutePath, err := r.reverseWindowAbsolutePath(intent)
	if err != nil {
		return reverseWindowPathObservation{}, err
	}
	fs, err := requireWorkspaceFS(r.replica.fs, r.replica.rootDir)
	if err != nil {
		return reverseWindowPathObservation{}, err
	}
	cleanPath, err := fs.cleanPath(absolutePath)
	if err != nil {
		return reverseWindowPathObservation{}, err
	}
	unlock, err := fs.lockPaths(cleanPath)
	if err != nil {
		return reverseWindowPathObservation{}, err
	}
	defer unlock()
	occupant, err := classifyWorkspacePathOccupant(cleanPath)
	if err != nil {
		return reverseWindowPathObservation{}, err
	}
	observation := reverseWindowPathObservation{kind: occupant.Kind}
	if occupant.Kind != workspacePathRegularFile {
		return observation, nil
	}
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return reverseWindowPathObservation{}, err
	}
	identity := statFileIdentity(cleanPath)
	if identity.valid {
		observation.identity = fmt.Sprintf("%d:%d", identity.dev, identity.ino)
	}
	observation.contentBytes = content
	observation.contentSHA = sha256Hex(content)
	return observation, nil
}

func (r *workspaceRuntime) reverseWindowAbsolutePath(intent rootLocalDeleteIntent) (string, error) {
	path, err := normalizeVisibleRootPath(intent.MaterializedPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.replica.rootDir, filepath.FromSlash(path)), nil
}

func (r *workspaceRuntime) postReverseWindowJSON(ctx context.Context, path string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := r.newBackendRequest(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if actingAgentID := strings.TrimSpace(r.actingAgentID()); actingAgentID != "" {
		applyBackendAuth(req.Header, r.cfg, actingAgentID)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return &backendStatusError{
			Method:     req.Method,
			URL:        req.URL.String(),
			StatusCode: res.StatusCode,
			Body:       string(body),
		}
	}
	return json.NewDecoder(res.Body).Decode(response)
}

func earlierNonZeroTime(left, right time.Time) time.Time {
	if left.IsZero() || (!right.IsZero() && right.Before(left)) {
		return right
	}
	return left
}

func (r *workspaceRuntime) rootPathClaimedByOtherActiveEntry(intent rootLocalDeleteIntent) (bool, error) {
	if r == nil || r.docCache == nil {
		return false, nil
	}
	rootDocumentID := strings.TrimSpace(intent.RootDocumentID)
	if rootDocumentID == "" {
		rootDocumentID = strings.TrimSpace(r.rootDocumentID)
	}
	if rootDocumentID == "" {
		return false, nil
	}
	entry, unlock := r.docCache.lockEntry(rootDocumentID)
	defer unlock()
	doc, _, _, err := r.docCache.loadBaseDocLocked(entry, rootDocumentID, rootDocumentPath)
	if err != nil {
		return false, err
	}
	defer doc.Close()
	mirror, err := DecodeRootCRDTMirror(doc)
	if err != nil {
		return false, err
	}
	desiredPath := normalizeRootPath(intent.DesiredPath)
	for entryID, candidate := range mirror.Entries {
		if candidate.Deleted || strings.TrimSpace(entryID) == strings.TrimSpace(intent.EntryID) {
			continue
		}
		if normalizeRootPath(candidate.desiredPath()) == desiredPath {
			return true, nil
		}
	}
	return false, nil
}

func (r *workspaceRuntime) finalizeRootLocalDeleteIntent(intent rootLocalDeleteIntent, reason reverseWindowFinalization) error {
	if r == nil || r.docCache == nil || r.replica == nil {
		return nil
	}
	materializedPath, err := normalizeVisibleRootPath(intent.MaterializedPath)
	if err != nil {
		return err
	}
	absolutePath := filepath.Join(r.replica.rootDir, filepath.FromSlash(materializedPath))
	if _, err := classifyWorkspacePathOccupant(absolutePath); err != nil {
		return err
	}
	if r.reverseWindowDecisionHook != nil {
		r.reverseWindowDecisionHook()
	}

	preserveClaimedPath := false
	if reason == reverseWindowFinalizationPathClaimed {
		preserveClaimedPath, err = r.rootPathClaimedByOtherActiveEntry(intent)
		if err != nil {
			return err
		}
	}
	if !preserveClaimedPath {
		fs, err := requireWorkspaceFS(r.replica.fs, r.replica.rootDir)
		if err != nil {
			return err
		}
		if _, err := fs.archiveRegularFile(absolutePath, safeDocumentCacheName(intent.ContentDocumentID)); err != nil {
			return err
		}
	}
	if err := r.docCache.deleteRootLocalDeleteIntent(intent); err != nil {
		return err
	}
	if strings.TrimSpace(r.rootDocumentID) != "" {
		r.markDocumentDirty(r.rootDocumentID)
	}
	return nil
}

var errRootLocalDeleteIntentChanged = errors.New("root local-delete intent changed before finalization")
