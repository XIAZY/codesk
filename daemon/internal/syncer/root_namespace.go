package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	crdt "notty/internal/ycrdt"
)

const (
	rootMapName        = "root"
	rootEntriesMapName = "entriesById"
	rootEntryKindFile  = "file"
	rootDeletedTrue    = "true"
	rootDeletedFalse   = "false"
)

type rootEntry struct {
	EntryID           string
	Kind              string
	ContentDocumentID string
	ParentID          string
	Name              string
	Deleted           bool
}

type rootProjectionEntry struct {
	EntryID           string
	Kind              string
	ContentDocumentID string
	DesiredPath       string
	MaterializedPath  string
	Active            bool
	ProjectedSeq      int64
}

type rootIntent struct {
	Kind              string
	EntryID           string
	ContentDocumentID string
	SourcePath        string
	TargetPath        string
}

type rootProjectionPlan struct {
	Previous map[string]rootProjectionEntry
	Next     []rootProjectionEntry
	Removed  []rootProjectionEntry
	Upserts  []rootProjectionEntry
}

type rootLoc struct {
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

func (e rootEntry) desiredPath() string {
	name := normalizeRootPath(e.Name)
	parent := normalizeRootPath(e.ParentID)
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return normalizeRootPath(parent + "/" + name)
}

func rootEntryIDForDocument(documentID string) string {
	return strings.TrimSpace(documentID)
}

func rootEntryPathLoc(path string) string {
	encoded, _ := json.Marshal(rootLoc{Name: normalizeRootPath(path)})
	return string(encoded)
}

func normalizeRootPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." {
		return ""
	}
	return clean
}

func decodeRootEntryMap(txn *crdt.Transaction, entryID string, entryMap *crdt.YMap) (rootEntry, bool, error) {
	entry := rootEntry{EntryID: strings.TrimSpace(entryID)}
	if entry.EntryID == "" || entryMap == nil {
		return rootEntry{}, false, nil
	}
	if value, ok, err := entryMap.GetString(txn, "kind"); err != nil {
		return rootEntry{}, false, err
	} else if ok {
		entry.Kind = strings.TrimSpace(value)
	}
	if value, ok, err := entryMap.GetString(txn, "contentDocumentId"); err != nil {
		return rootEntry{}, false, err
	} else if ok {
		entry.ContentDocumentID = strings.TrimSpace(value)
	}
	if value, ok, err := entryMap.GetString(txn, "loc"); err != nil {
		return rootEntry{}, false, err
	} else if ok {
		var loc rootLoc
		if err := json.Unmarshal([]byte(value), &loc); err == nil {
			entry.ParentID = normalizeRootPath(loc.ParentID)
			entry.Name = normalizeRootPath(loc.Name)
		}
	}
	if value, ok, err := entryMap.GetString(txn, "deleted"); err != nil {
		return rootEntry{}, false, err
	} else if ok {
		entry.Deleted = strings.EqualFold(strings.TrimSpace(value), rootDeletedTrue)
	}
	if entry.Kind == "" {
		entry.Kind = rootEntryKindFile
	}
	if entry.Kind != rootEntryKindFile || entry.ContentDocumentID == "" {
		return rootEntry{}, false, nil
	}
	return entry, true, nil
}

func setRootFileEntry(txn *crdt.Transaction, entriesMap *crdt.YMap, entry rootEntry) error {
	if txn == nil || entriesMap == nil {
		return errors.New("root entry update requires a transaction")
	}
	entry.EntryID = strings.TrimSpace(entry.EntryID)
	entry.ContentDocumentID = strings.TrimSpace(entry.ContentDocumentID)
	if entry.EntryID == "" || entry.ContentDocumentID == "" {
		return errors.New("root entry requires entry and content document IDs")
	}
	entryMap, ok, err := entriesMap.GetMap(txn, entry.EntryID)
	if err != nil {
		return err
	}
	if !ok {
		entryMap, err = entriesMap.SetMap(txn, entry.EntryID)
		if err != nil {
			return err
		}
	}
	if err := entryMap.SetString(txn, "kind", rootEntryKindFile); err != nil {
		return err
	}
	if err := entryMap.SetString(txn, "contentDocumentId", entry.ContentDocumentID); err != nil {
		return err
	}
	path := entry.desiredPath()
	if path == "" {
		path = normalizeRootPath(entry.Name)
	}
	if err := entryMap.SetString(txn, "loc", rootEntryPathLoc(path)); err != nil {
		return err
	}
	if entry.Deleted {
		return entryMap.SetString(txn, "deleted", rootDeletedTrue)
	}
	return entryMap.SetString(txn, "deleted", rootDeletedFalse)
}

func (s *workspaceRuntime) flushRootOutbox(ctx context.Context) error {
	if s == nil || s.docCache == nil || strings.TrimSpace(s.rootDocumentID) == "" {
		return nil
	}
	cache := s.docCache
	entry, unlock := cache.lockEntry(s.rootDocumentID)
	outboxes, err := cache.loadOutboxUpdatesLocked(entry, s.rootDocumentID)
	unlock()
	if err != nil || len(outboxes) == 0 {
		return err
	}
	return s.flushOutboxUpdates(ctx, cache, s.rootDocumentID, rootDocumentPath, nil, outboxes)
}

func (s *workspaceRuntime) upsertRootFileEntry(ctx context.Context, contentDocumentID, path, actorID, actorType string) error {
	contentDocumentID = strings.TrimSpace(contentDocumentID)
	if contentDocumentID == "" {
		return nil
	}
	path, err := normalizeVisibleRootPath(path)
	if err != nil {
		return err
	}
	return s.mutateRootDoc(ctx, actorID, actorType, func(doc *crdt.Doc) ([]byte, error) {
		return UpsertRootFile(doc, contentDocumentID, path, rootMutationActor{ID: actorID, Kind: actorType})
	})
}

func (s *workspaceRuntime) tombstoneRootFileEntry(ctx context.Context, contentDocumentID, actorID, actorType string) error {
	contentDocumentID = strings.TrimSpace(contentDocumentID)
	if contentDocumentID == "" {
		return nil
	}
	return s.mutateRootDoc(ctx, actorID, actorType, func(doc *crdt.Doc) ([]byte, error) {
		return TombstoneRootFile(doc, contentDocumentID, rootMutationActor{ID: actorID, Kind: actorType})
	})
}

func (s *workspaceRuntime) mutateRootDoc(ctx context.Context, actorID, actorType string, mutate func(*crdt.Doc) ([]byte, error)) error {
	if s == nil || s.docCache == nil || strings.TrimSpace(s.rootDocumentID) == "" || mutate == nil {
		return nil
	}
	if err := s.flushRootOutbox(ctx); err != nil {
		return err
	}
	cache := s.docCache
	entry, unlock := cache.lockEntry(s.rootDocumentID)
	pendingOutbox, err := cache.loadOutboxUpdatesLocked(entry, s.rootDocumentID)
	if err != nil {
		unlock()
		return err
	}
	if len(pendingOutbox) > 0 {
		unlock()
		s.markDocumentDirty(s.rootDocumentID)
		return errors.New("root outbox is still waiting for websocket acknowledgement")
	}
	doc, _, _, err := cache.loadBaseDocLocked(entry, s.rootDocumentID, rootDocumentPath)
	if err != nil {
		unlock()
		return err
	}
	defer doc.Close()
	update, err := mutate(doc)
	if err != nil {
		unlock()
		return err
	}
	if len(update) == 0 {
		unlock()
		return nil
	}
	record := outboxUpdateRecord{
		Update:     update,
		SourcePath: rootDocumentPath,
		ActorID:    firstNonEmptyText(actorID, s.actorID()),
		ActorType:  firstNonEmptyText(actorType, s.actorKind()),
		CreatedAt:  time.Now().UTC(),
	}
	if err := cache.storeOutboxUpdateLocked(entry, s.rootDocumentID, rootDocumentPath, record); err != nil {
		unlock()
		return err
	}
	unlock()
	return s.flushOutboxUpdates(ctx, cache, s.rootDocumentID, rootDocumentPath, nil, []outboxUpdateRecord{record})
}

func (s *workspaceRuntime) reconcileRootNamespace(ctx context.Context) error {
	if s == nil || s.docCache == nil || s.rootDocumentID == "" {
		return nil
	}
	if err := s.flushRootOutbox(ctx); err != nil {
		s.markDocumentDirty(s.rootDocumentID)
		return err
	}

	cache := s.docCache
	entry, unlock := cache.lockEntry(s.rootDocumentID)
	doc, _, _, err := cache.loadBaseDocLocked(entry, s.rootDocumentID, rootDocumentPath)
	if err != nil {
		unlock()
		return err
	}
	pending, err := cache.pendingRemoteUpdateCountLocked(entry, s.rootDocumentID)
	if err != nil {
		doc.Close()
		unlock()
		return err
	}
	if pending > 0 {
		if _, err := applyPendingRemoteUpdatesLocked(cache, entry, s.rootDocumentID, doc); err != nil {
			doc.Close()
			unlock()
			return err
		}
	}
	tree, err := DecodeRootTree(doc)
	doc.Close()
	if err != nil {
		unlock()
		return err
	}
	projectedSeq, err := cache.documentAppliedSeq(s.rootDocumentID)
	unlock()
	if err != nil {
		return err
	}

	previous, err := cache.loadRootProjectionEntries(s.rootDocumentID)
	if err != nil {
		return err
	}
	plan := PlanRootProjection(previous, tree, projectedSeq)
	if err := s.applyRootProjection(ctx, plan); err != nil {
		return err
	}
	if err := cache.storeRootProjectionEntries(s.rootDocumentID, plan.Next); err != nil {
		return err
	}
	if err := s.setDesiredDocumentsFromRootProjection(plan.Next); err != nil {
		return err
	}
	return cache.storeProjectedSeq(s.rootDocumentID, projectedSeq)
}

func (s *workspaceRuntime) updateDesiredDocumentsFromRootProjection() error {
	if s == nil || s.docCache == nil || s.rootDocumentID == "" {
		if s != nil && s.documentSocket != nil {
			s.documentSocket.SetDesiredDocuments(nil)
		}
		return nil
	}
	projection, err := s.docCache.loadRootProjectionEntries(s.rootDocumentID)
	if err != nil {
		return err
	}
	entries := make([]rootProjectionEntry, 0, len(projection))
	for _, entry := range projection {
		entries = append(entries, entry)
	}
	return s.setDesiredDocumentsFromRootProjection(entries)
}

func (s *workspaceRuntime) setDesiredDocumentsFromRootProjection(entries []rootProjectionEntry) error {
	if s == nil || s.documentSocket == nil {
		return nil
	}
	desired := []*document(nil)
	if s.rootDocumentID != "" {
		desired = append(desired, &document{ID: s.rootDocumentID, Path: rootDocumentPath})
	}
	for _, entry := range entries {
		if !entry.Active || strings.TrimSpace(entry.ContentDocumentID) == "" || strings.TrimSpace(entry.MaterializedPath) == "" {
			continue
		}
		desired = append(desired, &document{
			ID:   entry.ContentDocumentID,
			Path: entry.MaterializedPath,
		})
	}
	sort.Slice(desired, func(i, j int) bool {
		if desired[i].ID != desired[j].ID {
			return desired[i].ID < desired[j].ID
		}
		return desired[i].Path < desired[j].Path
	})
	s.documentSocket.SetDesiredDocuments(desired)
	return nil
}

func (s *workspaceRuntime) currentRootDesiredPaths() (map[string]struct{}, error) {
	paths := map[string]struct{}{}
	if s == nil || s.docCache == nil || s.rootDocumentID == "" {
		return paths, nil
	}
	projection, err := s.docCache.loadRootProjectionEntries(s.rootDocumentID)
	if err != nil {
		return nil, err
	}
	for _, entry := range projection {
		if !entry.Active || strings.TrimSpace(entry.MaterializedPath) == "" {
			continue
		}
		path, err := normalizeVisibleRootPath(entry.MaterializedPath)
		if err != nil {
			continue
		}
		paths[path] = struct{}{}
	}
	return paths, nil
}

func (s *workspaceRuntime) documentsFromRootProjection() ([]*document, error) {
	if s == nil || s.docCache == nil || s.rootDocumentID == "" {
		return nil, nil
	}
	projection, err := s.docCache.loadRootProjectionEntries(s.rootDocumentID)
	if err != nil {
		return nil, err
	}
	documents := make([]*document, 0, len(projection))
	for _, entry := range projection {
		if !entry.Active || strings.TrimSpace(entry.ContentDocumentID) == "" || strings.TrimSpace(entry.MaterializedPath) == "" {
			continue
		}
		documents = append(documents, &document{ID: entry.ContentDocumentID, Path: entry.MaterializedPath})
	}
	sort.Slice(documents, func(i, j int) bool {
		if documents[i].Path != documents[j].Path {
			return documents[i].Path < documents[j].Path
		}
		return documents[i].ID < documents[j].ID
	})
	return documents, nil
}

func (s *workspaceRuntime) projectRootProjectionEntries(ctx context.Context, previous map[string]rootProjectionEntry, next []rootProjectionEntry) error {
	return s.applyRootProjection(ctx, buildRootProjectionPlan(previous, next))
}

func (s *workspaceRuntime) applyRootProjection(ctx context.Context, plan rootProjectionPlan) error {
	if s == nil || s.replica == nil {
		return nil
	}
	for _, projected := range plan.Removed {
		if err := s.projectRootRemovedEntry(projected); err != nil {
			return err
		}
	}
	for _, projected := range plan.Upserts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.replica.ensureTracked(ctx, &document{ID: projected.ContentDocumentID, Path: projected.MaterializedPath}); err != nil {
			return err
		}
		s.markDocumentDirty(projected.ContentDocumentID)
	}
	return nil
}

func PlanRootProjection(previous map[string]rootProjectionEntry, tree RootTree, projectedSeq int64) rootProjectionPlan {
	return buildRootProjectionPlan(previous, allocateRootProjectionEntries(tree.Entries, projectedSeq))
}

func buildRootProjectionPlan(previous map[string]rootProjectionEntry, next []rootProjectionEntry) rootProjectionPlan {
	activeByEntry := map[string]rootProjectionEntry{}
	activeByContent := map[string]rootProjectionEntry{}
	upserts := []rootProjectionEntry{}
	for _, entry := range next {
		if !entry.Active || entry.ContentDocumentID == "" || entry.MaterializedPath == "" {
			continue
		}
		activeByEntry[entry.EntryID] = entry
		activeByContent[entry.ContentDocumentID] = entry
		upserts = append(upserts, entry)
	}
	removed := []rootProjectionEntry{}
	for entryID, projected := range previous {
		if !projected.Active {
			continue
		}
		if _, ok := activeByEntry[entryID]; ok {
			continue
		}
		if replacement, ok := activeByContent[projected.ContentDocumentID]; ok && replacement.Active {
			continue
		}
		removed = append(removed, projected)
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].EntryID < removed[j].EntryID })
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].EntryID < upserts[j].EntryID })
	return rootProjectionPlan{
		Previous: previous,
		Next:     next,
		Removed:  removed,
		Upserts:  upserts,
	}
}

func (s *workspaceRuntime) projectRootRemovedEntry(projected rootProjectionEntry) error {
	if s == nil || s.replica == nil || projected.ContentDocumentID == "" {
		return nil
	}
	s.replica.mu.Lock()
	tracked := s.replica.projectedByID[projected.ContentDocumentID]
	s.replica.mu.Unlock()
	if tracked == nil {
		return nil
	}
	states, err := collectTrackedReconcileStates([]*trackedFile{tracked})
	if err != nil {
		return err
	}
	if len(states) > 0 {
		state := states[0]
		if state.fileExists {
			if state.baseKnown && !state.localDirty {
				if err := tracked.workspaceFS().DeleteIfUnchanged(tracked.Path, projectedHashString(state.baseContent)); err != nil {
					if errors.Is(err, ErrUnsafeDelete) {
						if _, archiveErr := tracked.workspaceFS().Archive(tracked.Path, safeDocumentCacheName(tracked.DocumentID)); archiveErr != nil {
							return archiveErr
						}
					} else {
						return err
					}
				}
			} else if _, err := tracked.workspaceFS().Archive(tracked.Path, safeDocumentCacheName(tracked.DocumentID)); err != nil {
				return err
			}
		}
	}
	tracked.untrack()
	s.markDocumentDirty(projected.ContentDocumentID)
	return nil
}

func allocateRootProjectionEntries(entries map[string]rootEntry, projectedSeq int64) []rootProjectionEntry {
	active := make([]rootEntry, 0, len(entries))
	for _, entry := range entries {
		path, err := normalizeVisibleRootPath(entry.desiredPath())
		if entry.Deleted || entry.ContentDocumentID == "" || err != nil {
			continue
		}
		entry.Name = path
		entry.ParentID = ""
		active = append(active, entry)
	}
	sort.Slice(active, func(i, j int) bool {
		leftPath := active[i].desiredPath()
		rightPath := active[j].desiredPath()
		if leftPath != rightPath {
			return leftPath < rightPath
		}
		if active[i].ContentDocumentID != active[j].ContentDocumentID {
			return active[i].ContentDocumentID < active[j].ContentDocumentID
		}
		return active[i].EntryID < active[j].EntryID
	})
	used := map[string]struct{}{}
	result := make([]rootProjectionEntry, 0, len(entries))
	for _, entry := range active {
		desired := entry.desiredPath()
		materialized := allocateMaterializedRootPath(desired, entry.ContentDocumentID, used)
		used[materialized] = struct{}{}
		result = append(result, rootProjectionEntry{
			EntryID:           entry.EntryID,
			Kind:              rootEntryKindFile,
			ContentDocumentID: entry.ContentDocumentID,
			DesiredPath:       desired,
			MaterializedPath:  materialized,
			Active:            true,
			ProjectedSeq:      projectedSeq,
		})
	}
	for _, entry := range entries {
		if !entry.Deleted {
			continue
		}
		result = append(result, rootProjectionEntry{
			EntryID:           entry.EntryID,
			Kind:              rootEntryKindFile,
			ContentDocumentID: entry.ContentDocumentID,
			DesiredPath:       entry.desiredPath(),
			Active:            false,
			ProjectedSeq:      projectedSeq,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EntryID < result[j].EntryID })
	return result
}

func allocateMaterializedRootPath(desiredPath, contentDocumentID string, used map[string]struct{}) string {
	desiredPath = normalizeRootPath(desiredPath)
	if desiredPath == "" {
		desiredPath = "untitled"
	}
	if _, exists := used[desiredPath]; !exists {
		return desiredPath
	}
	base := strings.TrimSuffix(desiredPath, filepath.Ext(desiredPath))
	ext := filepath.Ext(desiredPath)
	suffix := safeDocumentCacheName(contentDocumentID)
	if len(suffix) > 14 {
		suffix = suffix[:14]
	}
	for index := 1; ; index++ {
		candidate := normalizeRootPath(base + " (" + suffix + ")" + ext)
		if index > 1 {
			candidate = normalizeRootPath(base + " (" + suffix + "-" + strconv.Itoa(index) + ")" + ext)
		}
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}
