package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"notty/internal/rootmanifest"
	crdt "notty/internal/ycrdt"
)

type ProjectionMode string

const (
	ProjectionPrimary   ProjectionMode = "primary"
	ProjectionAgentCopy ProjectionMode = "agent-copy"
)

type RootManifestProjector struct {
	State        *WorkspaceStateDB
	FS           *WorkspaceFS
	RootStreamID string
	ActorID      string
	ActorType    string
	Mode         ProjectionMode
	Capabilities ScanCapabilities
	NewID        func(kind string, relPath string) string
	Now          func() time.Time
	Queue        func(streamID string)
}

func (p RootManifestProjector) CaptureLocal(ctx context.Context, rootDoc *crdt.Doc) ([]StreamMutation, error) {
	if p.State == nil {
		return nil, errors.New("state db is required")
	}
	if p.FS == nil {
		return nil, errors.New("workspace fs is required")
	}
	if strings.TrimSpace(p.RootStreamID) == "" {
		return nil, errors.New("root stream id is required")
	}
	manifest, err := rootmanifest.Read(rootDoc)
	if err != nil {
		return nil, err
	}
	projection := rootmanifest.Resolve(manifest)
	tracker, err := p.State.LoadManifestProjection(ctx)
	if err != nil {
		return nil, err
	}
	scanState, err := p.State.GetScanState(ctx)
	if err != nil {
		return nil, err
	}
	caps := scanState.Capabilities()
	if p.Capabilities != (ScanCapabilities{}) {
		caps = p.Capabilities
	}
	hints, err := p.State.DrainScanHints(ctx, ScanHintDrainLimit)
	if err != nil {
		return nil, err
	}
	// TODO: Use fsnotify-backed scan hints for steady-state local changes so
	// no-hint reconciles do not full-scan on every tick. Keep periodic full
	// scans as the correctness fallback for missed events, overflow, and
	// daemon downtime.
	scan, err := p.FS.Scan(ctx, ScanOptions{
		Hints:        hints,
		StatOnly:     true,
		Capabilities: caps,
		UseDirCache:  caps.DirectoryMTimeReliable,
		CursorPath:   scanState.CursorPath,
		Budget:       DefaultRootScanBudget(),
	})
	if err != nil {
		return nil, err
	}
	fullScanComplete := !scan.Incomplete && (hasFullHint(hints) || len(hints) == 0)

	intents := []rootmanifest.Intent{}
	pendingCreates := []PendingContentCreate{}
	matchedPaths := map[string]string{}
	claimedPaths := map[string]struct{}{}
	dirMoves := map[string]string{}
	tombstonedByDirectoryDelete := map[string]struct{}{}
	fileKeyIndex := buildScanFileKeyIndex(scan, caps)

	for _, entry := range sortedManifestEntriesForCapture(manifest) {
		if entry.ID == rootmanifest.RootEntryID || entry.Tombstone != nil {
			continue
		}
		if _, tombstoned := tombstonedByDirectoryDelete[entry.ID]; tombstoned {
			continue
		}
		materialized := projection.EntryPath[entry.ID]
		if materialized == "" {
			continue
		}
		row, tracked := tracker[entry.ID]
		if stat, ok := scanStatForEntry(scan, materialized, entry.Kind); ok && stat.Exists {
			if entry.Kind == rootmanifest.EntryKindFile && !tracked {
				p.queue(entry.ContentStreamID)
				if p.canClaimUntrackedManifestFile(ctx, entry.ContentStreamID, materialized, stat, caps) {
					matchedPaths[entry.ID] = materialized
					claimedPaths[materialized] = struct{}{}
					_ = p.upsertManifestProjection(ctx, entry, projection, stat, false)
				}
				continue
			}
			matchedPaths[entry.ID] = materialized
			claimedPaths[materialized] = struct{}{}
			if entry.Kind == rootmanifest.EntryKindFile && (!tracked || !SameStatTuple(row.Stat, stat, caps)) {
				p.queue(entry.ContentStreamID)
			}
			_ = p.upsertManifestProjection(ctx, entry, projection, stat, false)
			continue
		}
		if caps.FileKeyReliable && tracked && row.Stat.FileKey != "" {
			if movedPath := fileKeyIndex[row.Stat.FileKey]; movedPath != "" {
				if _, claimed := claimedPaths[movedPath]; !claimed {
					if stat, ok := scanStatForEntry(scan, movedPath, entry.Kind); ok {
						loc := locForProjectedPath(manifest, tracker, movedPath, entry.ID)
						if loc != nil {
							intents = append(intents, rootmanifest.Intent{Type: "loc", EntryID: entry.ID, Loc: loc})
							matchedPaths[entry.ID] = movedPath
							claimedPaths[movedPath] = struct{}{}
							if entry.Kind == rootmanifest.EntryKindDir {
								dirMoves[materialized] = movedPath
							}
							_ = p.upsertManifestProjectionPath(ctx, entry, movedPath, movedPath, stat, false)
							continue
						}
					}
				}
			}
		}
		if tracked && entry.Kind == rootmanifest.EntryKindFile && row.LastCleanHash != "" {
			if movedPath, stat := p.detectCleanHashMove(ctx, row, scan, claimedPaths); movedPath != "" {
				loc := locForProjectedPath(manifest, tracker, movedPath, entry.ID)
				if loc != nil {
					intents = append(intents, rootmanifest.Intent{Type: "loc", EntryID: entry.ID, Loc: loc})
					matchedPaths[entry.ID] = movedPath
					claimedPaths[movedPath] = struct{}{}
					_ = p.upsertManifestProjectionPath(ctx, entry, movedPath, movedPath, stat, false)
					continue
				}
			}
		}
		if remapped := remapByDirMove(materialized, dirMoves); remapped != "" {
			if stat, ok := scanStatForEntry(scan, remapped, entry.Kind); ok && stat.Exists {
				matchedPaths[entry.ID] = remapped
				claimedPaths[remapped] = struct{}{}
				_ = p.upsertManifestProjectionPath(ctx, entry, remapped, remapped, stat, false)
				continue
			}
		}
		if tracked && entry.Kind == rootmanifest.EntryKindDir {
			if p.shouldRetryMissingDirectory(ctx, row, scan, fullScanComplete) {
				_, _ = p.State.InsertFSJob(ctx, FSJob{
					JobKey:     "root:mkdir:" + entry.ID + ":" + hashKey(materialized),
					Kind:       "mkdir",
					EntryID:    entry.ID,
					TargetPath: materialized,
				})
				continue
			}
			for _, intent := range p.tombstoneDirectoryTreeIntents(manifest, entry.ID) {
				intents = append(intents, intent)
				tombstonedByDirectoryDelete[intent.EntryID] = struct{}{}
			}
			continue
		}
		if tracked && entry.Kind == rootmanifest.EntryKindFile && row.LastCleanHash == "" && !row.PendingCreate {
			p.queue(entry.ContentStreamID)
			continue
		}
		if tracked && entry.Kind == rootmanifest.EntryKindFile && p.contentStreamNeedsProjection(ctx, entry.ContentStreamID, materialized) {
			p.queue(entry.ContentStreamID)
			continue
		}
		if tracked && entry.Kind == rootmanifest.EntryKindFile && p.isAgentCopyProjection() {
			p.queue(entry.ContentStreamID)
			continue
		}
		if !tracked {
			if entry.Kind == rootmanifest.EntryKindFile {
				p.queue(entry.ContentStreamID)
			}
			continue
		}
		if entry.Kind == rootmanifest.EntryKindFile && p.hasLocalContentOutbox(ctx, entry.ContentStreamID) {
			p.queue(entry.ContentStreamID)
			continue
		}
		intents = append(intents, rootmanifest.Intent{
			Type:    "tombstone",
			EntryID: entry.ID,
			Tombstone: &rootmanifest.Tombstone{
				ActorID:   firstNonEmptyText(p.ActorID, "daemon"),
				ActorType: firstNonEmptyText(p.ActorType, "daemon"),
				At:        p.now().Format(time.RFC3339Nano),
			},
		})
	}

	dirIDsByPath := liveDirIDsByDesiredPath(manifest, tracker)
	manifestCreateBlockPaths := p.manifestCreateBlockPaths(ctx, manifest, projection, tracker)
	for _, rel := range sortedScanDirPaths(scan) {
		if rel == "" || isIgnoredStatePath(rel) {
			continue
		}
		if _, claimed := claimedPaths[rel]; claimed || trackerPathExists(tracker, rel) {
			continue
		}
		entry, created := p.ensureDirectoryIntent(manifest, &intents, dirIDsByPath, rel)
		if created {
			stat := scan.Dirs[rel]
			_ = p.upsertManifestProjectionPath(ctx, entry, rel, rel, stat, true)
		}
	}
	contentProjectionPaths, err := p.State.LoadContentProjectionPaths(ctx)
	if err != nil {
		return nil, err
	}
	for _, rel := range sortedScanFilePaths(scan) {
		if _, claimed := claimedPaths[rel]; claimed || trackerPathExists(tracker, rel) || contentProjectionPathBlocksLocalCreate(contentProjectionPaths, tracker, rel) || manifestCreateBlockPaths[rel] {
			continue
		}
		parentID := rootmanifest.RootEntryID
		parentPath := path.Dir(rel)
		if parentPath != "." && parentPath != "" {
			parent, _ := p.ensureDirectoryIntent(manifest, &intents, dirIDsByPath, parentPath)
			parentID = parent.ID
		}
		entryID := pendingEntryIDForPath(tracker, rel)
		if entryID == "" {
			entryID = p.newID("file", rel)
		}
		entry := rootmanifest.Entry{
			ID:              entryID,
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(parentID, path.Base(rel)),
			ContentStreamID: entryID,
			CreatedBy:       firstNonEmptyText(p.ActorID, "daemon"),
			UpdatedBy:       firstNonEmptyText(p.ActorID, "daemon"),
			CreatedAt:       p.now().Format(time.RFC3339Nano),
			UpdatedAt:       p.now().Format(time.RFC3339Nano),
		}
		intents = append(intents, rootmanifest.Intent{Type: "create-file", Entry: entry})
		stat := scan.Files[rel].Stat
		_ = p.upsertManifestProjectionPath(ctx, entry, rel, rel, stat, true)
		pendingCreates = append(pendingCreates, PendingContentCreate{
			EntryID:          entry.ID,
			ContentStreamID:  entry.ContentStreamID,
			MaterializedPath: rel,
			ObservedStat:     stat,
		})
	}

	if scan.Incomplete {
		_ = p.State.SaveScanCursor(ctx, scan.CursorPath, true)
	} else {
		_ = p.State.SaveScanCursor(ctx, "", false)
		if hasFullHint(hints) || len(hints) == 0 {
			_ = p.State.MarkFullScanComplete(ctx)
		}
	}
	if len(intents) == 0 {
		return nil, nil
	}
	update, err := rootmanifest.ApplyIntents(rootDoc, intents)
	if err != nil {
		return nil, err
	}
	mutationKey := rootMutationKey(intents)
	for _, create := range pendingCreates {
		create.RootMutationKey = mutationKey
		if err := p.State.UpsertPendingContentCreate(ctx, create, caps); err != nil {
			return nil, err
		}
	}
	return []StreamMutation{{
		StreamID:    p.RootStreamID,
		KindHint:    "root",
		MutationKey: mutationKey,
		UpdateBytes: update,
		ActorID:     firstNonEmptyText(p.ActorID, "daemon"),
		ActorType:   firstNonEmptyText(p.ActorType, "daemon"),
		Reason:      "root-local-namespace",
	}}, nil
}

func (p RootManifestProjector) detectCleanHashMove(ctx context.Context, row ManifestProjectionRow, scan WorkspaceScan, claimed map[string]struct{}) (string, FileStat) {
	if row.LastCleanHash == "" || row.Stat.SizeBytes < 0 {
		return "", FileStat{}
	}
	candidates := []string{}
	for rel, snap := range scan.Files {
		if _, ok := claimed[rel]; ok {
			continue
		}
		if snap.Stat.SizeBytes == row.Stat.SizeBytes {
			candidates = append(candidates, rel)
		}
	}
	if len(candidates) != 1 {
		return "", FileStat{}
	}
	rel := candidates[0]
	snap := scan.Files[rel]
	if snap.Stat.SizeBytes > DefaultMoveDetectionMaxHashBytes {
		return "", FileStat{}
	}
	read, ok, err := p.FS.ReadBytesStable(ctx, rel, StableReadOptions{
		ExpectedStat: &snap.Stat,
		Capabilities: p.Capabilities,
		MaxBytes:     DefaultMoveDetectionMaxHashBytes,
	})
	if err != nil || !ok {
		return "", FileStat{}
	}
	if contentSHA256(read.Bytes) != row.LastCleanHash {
		return "", FileStat{}
	}
	return rel, read.FinalStat
}

func (p RootManifestProjector) PlanApplyMerged(ctx context.Context, rootDoc *crdt.Doc, rootStateID int64) error {
	if p.State == nil {
		return errors.New("state db is required")
	}
	manifest, err := rootmanifest.Read(rootDoc)
	if err != nil {
		return err
	}
	projection := rootmanifest.Resolve(manifest)
	tracker, err := p.State.LoadManifestProjection(ctx)
	if err != nil {
		return err
	}
	moveJobs := []FSJob{}
	for _, entry := range sortedManifestEntries(manifest) {
		if entry.ID == rootmanifest.RootEntryID {
			continue
		}
		materialized := projection.EntryPath[entry.ID]
		desired := projection.DesiredPath[entry.ID]
		previous, hadPrevious := tracker[entry.ID]
		if entry.Tombstone != nil {
			if hadPrevious && previous.Kind == rootmanifest.EntryKindFile && !previous.Tombstoned {
				if p.hasLocalContentOutboxAfter(ctx, previous.ContentStreamID, entry.Tombstone.At) {
					_ = p.State.InsertScanHint(ctx, ScanHintPath, previous.MaterializedPath, "remote-delete-local-content-detach")
					p.queue(p.RootStreamID)
				} else if p.localFileMatchesCleanHash(ctx, previous) {
					_, _ = p.State.InsertFSJob(ctx, FSJob{
						JobKey:       "root:delete:" + entry.ID + ":" + hashKey(previous.MaterializedPath),
						Kind:         "delete-clean-entry",
						EntryID:      entry.ID,
						StreamID:     previous.ContentStreamID,
						TargetPath:   previous.MaterializedPath,
						ExpectedHash: previous.LastCleanHash,
					})
				} else {
					_ = p.State.InsertScanHint(ctx, ScanHintPath, previous.MaterializedPath, "remote-delete-dirty-detach")
					p.queue(p.RootStreamID)
				}
			}
			previous.Tombstoned = true
			previous.RootProjectedStateID = rootStateID
			if previous.EntryID != "" {
				_ = p.State.UpsertManifestProjection(ctx, previous)
			}
			continue
		}
		if materialized == "" {
			continue
		}
		materializedPath := materialized
		movePending := false
		if hadPrevious && !previous.Tombstoned && previous.MaterializedPath != "" && previous.MaterializedPath != materialized {
			materializedPath = previous.MaterializedPath
			movePending = true
		}
		stat, _ := p.FS.Stat(ctx, materializedPath)
		row := ManifestProjectionRow{
			EntryID:              entry.ID,
			Kind:                 entry.Kind,
			ContentStreamID:      entry.ContentStreamID,
			DesiredPath:          desired,
			MaterializedPath:     materializedPath,
			Stat:                 stat,
			RootProjectedStateID: rootStateID,
			Tombstoned:           false,
		}
		if hadPrevious {
			row.LastCleanHash = previous.LastCleanHash
			if movePending {
				moveJobs = append(moveJobs, FSJob{
					JobKey:     "root:move:" + entry.ID + ":" + hashKey(previous.MaterializedPath+"->"+materialized),
					Kind:       "move-entry",
					EntryID:    entry.ID,
					StreamID:   entry.ContentStreamID,
					SourcePath: previous.MaterializedPath,
					TargetPath: materialized,
				})
			}
		}
		if err := p.State.UpsertManifestProjection(ctx, row); err != nil {
			return err
		}
		if entry.Kind == rootmanifest.EntryKindFile {
			if err := p.State.EnsureLocalStream(ctx, entry.ContentStreamID, "content"); err != nil {
				return err
			}
			if _, err := p.State.GetContentProjection(ctx, entry.ContentStreamID); err != nil {
				return err
			}
			contentRow := ContentProjectionRow{
				StreamID:         entry.ContentStreamID,
				EntryID:          entry.ID,
				MaterializedPath: materializedPath,
			}
			if existing, err := p.State.GetContentProjection(ctx, entry.ContentStreamID); err != nil {
				return err
			} else if existing != nil {
				contentRow.ProjectedStateID = existing.ProjectedStateID
				contentRow.ProjectedHash = existing.ProjectedHash
				contentRow.Stat = existing.Stat
				contentRow.Dirty = existing.Dirty
			}
			if err := p.State.UpsertContentProjection(ctx, contentRow); err != nil {
				return err
			}
			p.queue(entry.ContentStreamID)
		}
		if entry.Kind == rootmanifest.EntryKindDir {
			_, _ = p.State.InsertFSJob(ctx, FSJob{
				JobKey:     "root:mkdir:" + entry.ID + ":" + hashKey(materialized),
				Kind:       "mkdir",
				EntryID:    entry.ID,
				TargetPath: materialized,
			})
		}
	}
	for _, job := range orderRootMoveJobs(moveJobs) {
		if _, err := p.State.InsertFSJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

func (p RootManifestProjector) queue(streamID string) {
	if p.Queue != nil && strings.TrimSpace(streamID) != "" {
		p.Queue(streamID)
	}
}

func (p RootManifestProjector) localFileMatchesCleanHash(ctx context.Context, row ManifestProjectionRow) bool {
	if p.FS == nil || row.LastCleanHash == "" || row.MaterializedPath == "" {
		return false
	}
	stat, err := p.FS.Stat(ctx, row.MaterializedPath)
	if err != nil || !stat.Exists || stat.Kind != FileKindFile || stat.SizeBytes > MaxSinglePendingCreateBytes {
		return false
	}
	read, ok, err := p.FS.ReadBytesStable(ctx, row.MaterializedPath, StableReadOptions{
		ExpectedStat: &stat,
		Capabilities: p.Capabilities,
		MaxBytes:     MaxSinglePendingCreateBytes,
	})
	if err != nil || !ok {
		return false
	}
	return contentSHA256(read.Bytes) == row.LastCleanHash
}

func (p RootManifestProjector) localFileMatchesLatestStreamState(ctx context.Context, streamID string, rel string, stat FileStat, caps ScanCapabilities) bool {
	if p.State == nil || p.FS == nil || strings.TrimSpace(streamID) == "" || rel == "" {
		return false
	}
	if !stat.Exists || stat.Kind != FileKindFile || stat.SizeBytes > MaxSinglePendingCreateBytes {
		return false
	}
	doc, stream, err := p.State.LoadLatestStreamDoc(ctx, streamID, "content")
	if err != nil {
		return false
	}
	defer doc.Close()
	if !stream.LatestStateID.Valid {
		return false
	}
	expectedHash := contentSHA256([]byte(doc.GetText("content").ToString()))
	read, ok, err := p.FS.ReadBytesStable(ctx, rel, StableReadOptions{
		ExpectedStat: &stat,
		Capabilities: caps,
		MaxBytes:     MaxSinglePendingCreateBytes,
	})
	if err != nil || !ok {
		return false
	}
	return contentSHA256(read.Bytes) == expectedHash
}

func (p RootManifestProjector) canClaimUntrackedManifestFile(ctx context.Context, streamID string, rel string, stat FileStat, caps ScanCapabilities) bool {
	if p.localFileMatchesLatestStreamState(ctx, streamID, rel, stat, caps) {
		return true
	}
	projection, err := p.State.GetContentProjection(ctx, streamID)
	if err != nil || projection == nil || projection.Dirty || !projection.ProjectedStateID.Valid {
		return false
	}
	return normalizeStateRelPath(projection.MaterializedPath) == normalizeStateRelPath(rel)
}

func (p RootManifestProjector) hasLocalContentOutboxAfter(ctx context.Context, streamID string, after string) bool {
	if p.State == nil {
		return false
	}
	ok, err := p.State.HasOutboxCreatedAfter(ctx, streamID, after)
	return err == nil && ok
}

func (p RootManifestProjector) hasLocalContentOutbox(ctx context.Context, streamID string) bool {
	if p.State == nil {
		return false
	}
	count, err := p.State.PendingOutboxCount(ctx, streamID)
	return err == nil && count > 0
}

func (p RootManifestProjector) shouldRetryMissingDirectory(ctx context.Context, row ManifestProjectionRow, scan WorkspaceScan, fullScanComplete bool) bool {
	if scan.Incomplete || row.PendingCreate || !row.Stat.StatValid || row.RootProjectedStateID == 0 {
		return true
	}
	if !fullScanComplete {
		_, explicitlyMissing := scan.Missing[normalizeStateRelPath(row.MaterializedPath)]
		if !explicitlyMissing {
			return true
		}
	}
	if p.State == nil || strings.TrimSpace(row.EntryID) == "" {
		return false
	}
	ok, err := p.State.HasBlockingFSJobForEntry(ctx, row.EntryID)
	return err == nil && ok
}

func (p RootManifestProjector) tombstoneDirectoryTreeIntents(manifest rootmanifest.Manifest, dirID string) []rootmanifest.Intent {
	tombstone := rootmanifest.Tombstone{
		ActorID:   firstNonEmptyText(p.ActorID, "daemon"),
		ActorType: firstNonEmptyText(p.ActorType, "daemon"),
		At:        p.now().Format(time.RFC3339Nano),
	}
	intents := []rootmanifest.Intent{}
	for _, entry := range sortedManifestEntriesForCapture(manifest) {
		if entry.ID == rootmanifest.RootEntryID || entry.Tombstone != nil {
			continue
		}
		if entry.ID != dirID && !manifestEntryDescendsFrom(manifest, entry.ID, dirID) {
			continue
		}
		entryTombstone := tombstone
		intents = append(intents, rootmanifest.Intent{
			Type:      "tombstone",
			EntryID:   entry.ID,
			Tombstone: &entryTombstone,
		})
	}
	return intents
}

func manifestEntryDescendsFrom(manifest rootmanifest.Manifest, entryID string, ancestorID string) bool {
	seen := map[string]struct{}{}
	entry, ok := manifest.EntriesByID[entryID]
	for ok && entry.Loc != nil {
		parentID := strings.TrimSpace(entry.Loc.ParentID)
		if parentID == ancestorID {
			return true
		}
		if parentID == "" || parentID == rootmanifest.RootEntryID {
			return false
		}
		if _, loop := seen[parentID]; loop {
			return false
		}
		seen[parentID] = struct{}{}
		entry, ok = manifest.EntriesByID[parentID]
	}
	return false
}

func (p RootManifestProjector) isAgentCopyProjection() bool {
	return p.Mode == ProjectionAgentCopy
}

func (p RootManifestProjector) contentStreamNeedsProjection(ctx context.Context, streamID string, materialized string) bool {
	if p.State == nil || strings.TrimSpace(streamID) == "" {
		return false
	}
	if blocking, err := p.State.HasBlockingFSJob(ctx, streamID); err == nil && blocking {
		return true
	}
	materialized = normalizeStateRelPath(materialized)
	if projection, err := p.State.GetContentProjection(ctx, streamID); err == nil && projection != nil {
		if normalizeStateRelPath(projection.MaterializedPath) == "" && materialized != "" {
			projection.MaterializedPath = materialized
			_ = p.State.UpsertContentProjection(ctx, *projection)
			return true
		}
	}
	stream, err := p.State.GetStream(ctx, streamID)
	if err != nil {
		return false
	}
	return stream.LatestStateID.Valid && (!stream.ProjectedStateID.Valid || stream.ProjectedStateID.Int64 != stream.LatestStateID.Int64)
}

func (p RootManifestProjector) manifestCreateBlockPaths(ctx context.Context, manifest rootmanifest.Manifest, projection rootmanifest.Projection, tracker map[string]ManifestProjectionRow) map[string]bool {
	blocked := map[string]bool{}
	if p.State == nil {
		return blocked
	}
	for _, entry := range manifest.EntriesByID {
		if entry.Kind != rootmanifest.EntryKindFile || entry.Tombstone != nil {
			continue
		}
		rel := normalizeStateRelPath(projection.EntryPath[entry.ID])
		if rel == "" {
			continue
		}
		if row, ok := tracker[entry.ID]; ok && !row.Tombstoned {
			blocked[rel] = true
			continue
		}
		if contentProjection, err := p.State.GetContentProjection(ctx, entry.ContentStreamID); err == nil && contentProjection != nil {
			blocked[rel] = true
			continue
		}
		if p.hasLocalContentOutbox(ctx, entry.ContentStreamID) {
			blocked[rel] = true
			continue
		}
		if stream, err := p.State.GetStream(ctx, entry.ContentStreamID); err == nil && stream.LatestStateID.Valid {
			blocked[rel] = true
			continue
		}
	}
	return blocked
}

func orderRootMoveJobs(jobs []FSJob) []FSJob {
	if len(jobs) < 2 {
		return jobs
	}
	remaining := append([]FSJob(nil), jobs...)
	ordered := make([]FSJob, 0, len(jobs))
	for len(remaining) > 0 {
		next := -1
		for i, job := range remaining {
			target := normalizeStateRelPath(job.TargetPath)
			blocked := false
			for j, other := range remaining {
				if i == j {
					continue
				}
				if target != "" && target == normalizeStateRelPath(other.SourcePath) {
					blocked = true
					break
				}
			}
			if !blocked {
				next = i
				break
			}
		}
		if next == -1 {
			cycle := remaining[0]
			tempPath := tempMovePath(cycle)
			ordered = append(ordered, FSJob{
				JobKey:     "root:move-temp:" + cycle.EntryID + ":" + hashKey(cycle.SourcePath+"->"+tempPath),
				Kind:       "move-entry-temp",
				EntryID:    cycle.EntryID,
				StreamID:   cycle.StreamID,
				SourcePath: cycle.SourcePath,
				TargetPath: tempPath,
			})
			remaining[0].SourcePath = tempPath
			remaining[0].JobKey = "root:move:" + cycle.EntryID + ":" + hashKey(tempPath+"->"+cycle.TargetPath)
			continue
		}
		ordered = append(ordered, remaining[next])
		remaining = append(remaining[:next], remaining[next+1:]...)
	}
	return ordered
}

func tempMovePath(job FSJob) string {
	stem := firstNonEmptyText(job.EntryID, hashKey(job.SourcePath+"->"+job.TargetPath))
	base := path.Base(normalizeStateRelPath(job.SourcePath))
	if base == "." || base == "/" || base == "" {
		base = "entry"
	}
	return path.Join(".notty", "tmp", "moves", stem+"-"+hashKey(job.SourcePath+"->"+job.TargetPath), base)
}

func DefaultRootScanBudget() ScanBudget {
	return ScanBudget{MaxPaths: 10_000, MaxDirs: 2_000, MaxDuration: 2 * time.Second}
}

const DefaultMoveDetectionMaxHashBytes int64 = 16 << 20

func (p RootManifestProjector) upsertManifestProjection(ctx context.Context, entry rootmanifest.Entry, projection rootmanifest.Projection, stat FileStat, pending bool) error {
	return p.upsertManifestProjectionPath(ctx, entry, projection.DesiredPath[entry.ID], projection.EntryPath[entry.ID], stat, pending)
}

func (p RootManifestProjector) upsertManifestProjectionPath(ctx context.Context, entry rootmanifest.Entry, desired string, materialized string, stat FileStat, pending bool) error {
	return p.State.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              entry.ID,
		Kind:                 entry.Kind,
		ContentStreamID:      entry.ContentStreamID,
		DesiredPath:          desired,
		MaterializedPath:     materialized,
		Stat:                 stat,
		RootProjectedStateID: 0,
		PendingCreate:        pending,
	})
}

func (p RootManifestProjector) ensureDirectoryIntent(manifest rootmanifest.Manifest, intents *[]rootmanifest.Intent, dirs map[string]string, rel string) (rootmanifest.Entry, bool) {
	rel = normalizeStateRelPath(rel)
	if rel == "" || rel == "." {
		return manifest.EntriesByID[rootmanifest.RootEntryID], false
	}
	if id := dirs[rel]; id != "" {
		if entry, ok := manifest.EntriesByID[id]; ok {
			return entry, false
		}
		return rootmanifest.Entry{ID: id, Kind: rootmanifest.EntryKindDir}, false
	}
	parentID := rootmanifest.RootEntryID
	parentPath := path.Dir(rel)
	if parentPath != "." && parentPath != "" {
		parent, _ := p.ensureDirectoryIntent(manifest, intents, dirs, parentPath)
		parentID = parent.ID
	}
	entryID := p.newID("dir", rel)
	entry := rootmanifest.Entry{
		ID:        entryID,
		Kind:      rootmanifest.EntryKindDir,
		Loc:       rootmanifest.NewLocation(parentID, path.Base(rel)),
		CreatedBy: firstNonEmptyText(p.ActorID, "daemon"),
		UpdatedBy: firstNonEmptyText(p.ActorID, "daemon"),
		CreatedAt: p.now().Format(time.RFC3339Nano),
		UpdatedAt: p.now().Format(time.RFC3339Nano),
	}
	*intents = append(*intents, rootmanifest.Intent{Type: "create-dir", Entry: entry})
	dirs[rel] = entry.ID
	manifest.EntriesByID[entry.ID] = entry
	return entry, true
}

func (p RootManifestProjector) newID(kind string, relPath string) string {
	if p.NewID != nil {
		if id := strings.TrimSpace(p.NewID(kind, relPath)); id != "" {
			return id
		}
	}
	prefix := "doc"
	if kind == "dir" {
		prefix = "dir"
	}
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func (p RootManifestProjector) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func rootMutationKey(intents []rootmanifest.Intent) string {
	parts := make([]string, 0, len(intents))
	for _, intent := range intents {
		parts = append(parts, rootMutationKeyPart(intent))
	}
	sort.Strings(parts)
	return "root:batch:" + hashKey(strings.Join(parts, "|"))
}

func rootMutationKeyPart(intent rootmanifest.Intent) string {
	entryID := strings.TrimSpace(intent.EntryID)
	if entryID == "" {
		entryID = strings.TrimSpace(intent.Entry.ID)
	}
	payload := struct {
		Type      string                  `json:"type"`
		EntryID   string                  `json:"entryId,omitempty"`
		Entry     *rootmanifest.Entry     `json:"entry,omitempty"`
		Loc       *rootmanifest.Location  `json:"loc,omitempty"`
		Tombstone *rootmanifest.Tombstone `json:"tombstone,omitempty"`
	}{
		Type:      strings.TrimSpace(intent.Type),
		EntryID:   entryID,
		Loc:       intent.Loc,
		Tombstone: intent.Tombstone,
	}
	if intent.Entry.ID != "" {
		entry := intent.Entry
		payload.Entry = &entry
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return payload.Type + ":" + entryID
	}
	return string(raw)
}

func hashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func sortedManifestEntries(manifest rootmanifest.Manifest) []rootmanifest.Entry {
	entries := make([]rootmanifest.Entry, 0, len(manifest.EntriesByID))
	for _, entry := range manifest.EntriesByID {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

func sortedManifestEntriesForCapture(manifest rootmanifest.Manifest) []rootmanifest.Entry {
	entries := sortedManifestEntries(manifest)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind == rootmanifest.EntryKindDir
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

func remapByDirMove(rel string, moves map[string]string) string {
	if len(moves) == 0 {
		return ""
	}
	bestOld := ""
	bestNew := ""
	for oldPrefix, newPrefix := range moves {
		if rel == oldPrefix || strings.HasPrefix(rel, oldPrefix+"/") {
			if len(oldPrefix) > len(bestOld) {
				bestOld = oldPrefix
				bestNew = newPrefix
			}
		}
	}
	if bestOld == "" {
		return ""
	}
	if rel == bestOld {
		return bestNew
	}
	return strings.TrimSuffix(bestNew, "/") + strings.TrimPrefix(rel, bestOld)
}

func sortedScanFilePaths(scan WorkspaceScan) []string {
	paths := make([]string, 0, len(scan.Files))
	for path := range scan.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sortedScanDirPaths(scan WorkspaceScan) []string {
	paths := make([]string, 0, len(scan.Dirs))
	for path := range scan.Dirs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func scanStatForEntry(scan WorkspaceScan, materialized string, kind string) (FileStat, bool) {
	if kind == rootmanifest.EntryKindDir {
		stat, ok := scan.Dirs[materialized]
		return stat, ok
	}
	snap, ok := scan.Files[materialized]
	return snap.Stat, ok
}

func buildScanFileKeyIndex(scan WorkspaceScan, caps ScanCapabilities) map[string]string {
	index := map[string]string{}
	if !caps.FileKeyReliable {
		return index
	}
	for rel, snap := range scan.Files {
		if snap.Stat.FileKey != "" {
			index[snap.Stat.FileKey] = rel
		}
	}
	for rel, stat := range scan.Dirs {
		if stat.FileKey != "" {
			index[stat.FileKey] = rel
		}
	}
	return index
}

func liveDirIDsByDesiredPath(manifest rootmanifest.Manifest, tracker map[string]ManifestProjectionRow) map[string]string {
	projection := rootmanifest.Resolve(manifest)
	result := map[string]string{"": rootmanifest.RootEntryID}
	for _, entry := range manifest.EntriesByID {
		if entry.Kind == rootmanifest.EntryKindDir && entry.Tombstone == nil {
			result[projection.DesiredPath[entry.ID]] = entry.ID
			result[projection.EntryPath[entry.ID]] = entry.ID
		}
	}
	for _, row := range tracker {
		if row.Kind == rootmanifest.EntryKindDir && !row.Tombstoned {
			result[row.DesiredPath] = row.EntryID
			result[row.MaterializedPath] = row.EntryID
		}
	}
	return result
}

func trackerPathExists(tracker map[string]ManifestProjectionRow, rel string) bool {
	for _, row := range tracker {
		if row.MaterializedPath == rel && !row.Tombstoned {
			return true
		}
	}
	return false
}

func contentProjectionPathBlocksLocalCreate(paths map[string]string, tracker map[string]ManifestProjectionRow, rel string) bool {
	if len(paths) == 0 {
		return false
	}
	entryID, ok := paths[normalizeStateRelPath(rel)]
	if !ok {
		return false
	}
	row, tracked := tracker[entryID]
	return !tracked || !row.Tombstoned
}

func pendingEntryIDForPath(tracker map[string]ManifestProjectionRow, rel string) string {
	for _, row := range tracker {
		if row.MaterializedPath == rel && row.PendingCreate {
			return row.EntryID
		}
	}
	return ""
}

func locForProjectedPath(manifest rootmanifest.Manifest, tracker map[string]ManifestProjectionRow, rel string, entryID string) *rootmanifest.Location {
	if pathReservedByOtherEntry(manifest, tracker, rel, entryID) {
		return nil
	}
	parentPath := path.Dir(rel)
	parentID := rootmanifest.RootEntryID
	if parentPath != "." && parentPath != "" {
		dirs := liveDirIDsByDesiredPath(manifest, tracker)
		parentID = dirs[parentPath]
		if parentID == "" {
			return nil
		}
	}
	return rootmanifest.NewLocation(parentID, path.Base(rel))
}

func pathReservedByOtherEntry(manifest rootmanifest.Manifest, tracker map[string]ManifestProjectionRow, rel string, entryID string) bool {
	rel = normalizeStateRelPath(rel)
	if rel == "" {
		return false
	}
	projection := rootmanifest.Resolve(manifest)
	for id, projectedPath := range projection.EntryPath {
		if id != entryID && normalizeStateRelPath(projectedPath) == rel {
			return true
		}
	}
	for id, row := range tracker {
		rowEntryID := firstNonEmptyText(row.EntryID, id)
		if rowEntryID != entryID && !row.Tombstoned && normalizeStateRelPath(row.MaterializedPath) == rel {
			return true
		}
	}
	return false
}
