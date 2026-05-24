package notty

import (
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
)

func (s *Store) ReadRootManifestStream() (string, RootManifest, StreamHead, error) {
	rootStreamID, err := s.BootstrapWorkspaceStreams()
	if err != nil {
		return "", RootManifest{}, StreamHead{}, err
	}
	doc, head, err := s.RestoreStreamDoc(rootStreamID)
	if err != nil {
		return "", RootManifest{}, StreamHead{}, err
	}
	manifest, err := ReadRootManifest(doc)
	if err != nil {
		return "", RootManifest{}, StreamHead{}, err
	}
	return rootStreamID, manifest, head, nil
}

func (s *Store) ListStreamDocuments() ([]*Document, error) {
	_, manifest, _, err := s.ReadRootManifestStream()
	if err != nil {
		return nil, err
	}
	projection := ResolveMaterializedPaths(manifest)
	documents := make([]*Document, 0, len(manifest.EntriesByID))
	for _, entry := range sortedRootEntries(manifest) {
		if entry.Kind != RootEntryKindFile || entry.Tombstone != nil {
			continue
		}
		materializedPath := projection.EntryPath[entry.ID]
		if materializedPath == "" {
			continue
		}
		documents = append(documents, s.streamDocumentFromEntry(entry, materializedPath, projection.DesiredPath[entry.ID]))
	}
	sort.Slice(documents, func(i, j int) bool {
		if documents[i].Path != documents[j].Path {
			return documents[i].Path < documents[j].Path
		}
		return documents[i].ID < documents[j].ID
	})
	return documents, nil
}

func (s *Store) ListStreamDocumentMetadata() ([]*DocumentMetadata, error) {
	documents, err := s.ListStreamDocuments()
	if err != nil {
		return nil, err
	}
	metadata := make([]*DocumentMetadata, 0, len(documents))
	for _, document := range documents {
		metadata = append(metadata, documentMetadata(document))
	}
	return metadata, nil
}

func (s *Store) GetStreamDocumentMetadataByPath(documentPath string) (*DocumentMetadata, error) {
	normalized, err := normalizeDocumentPath(documentPath)
	if err != nil {
		return nil, err
	}
	documents, err := s.ListStreamDocuments()
	if err != nil {
		return nil, err
	}
	for _, document := range documents {
		if document.Path == normalized {
			return documentMetadata(document), nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) refreshStreamDocumentCacheLocked() error {
	documents, err := s.ListStreamDocuments()
	if err != nil {
		return err
	}
	next := make(map[string]*Document, len(documents))
	for _, document := range documents {
		next[document.ID] = cloneDocument(document)
	}
	s.state.Documents = next
	return nil
}

func (s *Store) RefreshStreamDocumentCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshStreamDocumentCacheLocked()
}

func (s *Store) GetStreamDocument(documentID string) (*Document, error) {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, ErrNotFound
	}
	documents, err := s.ListStreamDocuments()
	if err != nil {
		return nil, err
	}
	for _, document := range documents {
		if document.ID == documentID {
			return document, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) HasStreamDocument(documentID string) bool {
	_, err := s.GetStreamDocument(documentID)
	return err == nil
}

func (s *Store) CreateStreamDocument(req CreateDocumentRequest, meta OperationMeta) (*Document, error) {
	path, err := normalizeDocumentPath(req.Path)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	document := &Document{
		ID:                 "doc_" + uuid.NewString(),
		Path:               path,
		DesiredPath:        path,
		Title:              titleFromPath(path),
		NotificationPolicy: strings.TrimSpace(req.NotificationPolicy),
		UpdatedAt:          now,
		ClientIDSeed:       1001,
	}
	if err := s.MirrorDocumentCreateToStreams(document, req.Content, nil, meta); err != nil {
		return nil, err
	}
	created, err := s.GetStreamDocument(document.ID)
	if err != nil {
		return nil, err
	}
	if created.ClientIDSeed == 0 {
		created.ClientIDSeed = document.ClientIDSeed
	}
	s.mu.Lock()
	s.state.Documents[created.ID] = cloneDocument(created)
	s.state.UpdatedAt = created.UpdatedAt
	s.mu.Unlock()
	return created, nil
}

func (s *Store) MoveStreamDocument(documentID string, nextPath string, meta OperationMeta) (*Document, string, error) {
	current, err := s.GetStreamDocument(documentID)
	if err != nil {
		return nil, "", err
	}
	path, err := normalizeDocumentPath(nextPath)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	if err := s.MirrorDocumentMoveToRoot(documentID, path, now, meta); err != nil {
		return nil, "", err
	}
	moved, err := s.GetStreamDocument(documentID)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	s.state.Documents[moved.ID] = cloneDocument(moved)
	s.state.UpdatedAt = moved.UpdatedAt
	s.mu.Unlock()
	return moved, current.Path, nil
}

func (s *Store) DeleteStreamDocument(documentID string, meta OperationMeta) (*Document, error) {
	current, err := s.GetStreamDocument(documentID)
	if err != nil {
		return nil, err
	}
	if err := s.MirrorDocumentDeleteToRoot(documentID, time.Now().UTC(), meta); err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.state.Documents, documentID)
	s.state.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	return current, nil
}

func (s *Store) ApplyStreamDocumentUpdate(documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error) {
	if len(update) == 0 {
		return nil, errors.New("document update is required")
	}
	result, err := s.ApplyStreamUpdate(documentID, update, meta)
	if err != nil {
		return nil, err
	}
	document, docErr := s.GetStreamDocument(documentID)
	if docErr != nil {
		return nil, docErr
	}
	s.mu.Lock()
	s.state.Documents[document.ID] = cloneDocument(document)
	s.state.UpdatedAt = document.UpdatedAt
	s.mu.Unlock()
	return &ApplyCRDTUpdateResult{
		Document: document,
		Applied:  result.Applied,
	}, nil
}

func (s *Store) MirrorDocumentCreateToStreams(document *Document, content string, initialUpdate []byte, meta OperationMeta) error {
	if document == nil {
		return errors.New("document is required")
	}
	now := document.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}
	workspaceName := s.workspaceName
	if workspaceName == "" {
		workspaceName = workspaceID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rootStreamID, err := bootstrapWorkspaceStreamsTx(tx, workspaceID, workspaceName)
	if err != nil {
		return err
	}
	rootDoc, _, err := restoreStreamDocForUpdateTx(tx, workspaceID, rootStreamID)
	if err != nil {
		return err
	}
	defer rootDoc.Close()
	manifest, err := ReadRootManifest(rootDoc)
	if err != nil {
		return err
	}
	intents, err := rootCreateFileIntents(manifest, document.ID, document.Path, document.NotificationPolicy, now, meta)
	if err != nil {
		return err
	}
	rootUpdate, err := ApplyRootIntents(rootDoc, intents)
	if err != nil {
		return err
	}
	if _, err := applyStreamUpdateTx(tx, workspaceID, rootStreamID, rootUpdate, meta); err != nil {
		return err
	}
	if len(initialUpdate) == 0 {
		initialUpdate = contentStreamUpdate(content, document.ClientIDSeed)
	}
	if len(initialUpdate) > 0 {
		if _, err := applyStreamUpdateTx(tx, workspaceID, document.ID, initialUpdate, meta); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) MirrorDocumentMoveToRoot(documentID string, nextPath string, updatedAt time.Time, meta OperationMeta) error {
	rootStreamID, manifest, _, err := s.ReadRootManifestStream()
	if err != nil {
		return err
	}
	entry, ok := manifest.EntriesByID[documentID]
	if !ok || entry.Tombstone != nil {
		return ErrNotFound
	}
	intents, loc, err := rootLocationIntentsForPath(manifest, nextPath, updatedAt, meta)
	if err != nil {
		return err
	}
	updatedAtString := updatedAt.Format(time.RFC3339Nano)
	entry.UpdatedAt = updatedAtString
	entry.UpdatedBy = meta.ActorID
	intents = append(intents, RootIntent{
		Type:      "loc",
		EntryID:   documentID,
		Loc:       loc,
		UpdatedBy: meta.ActorID,
		UpdatedAt: updatedAtString,
	})
	rootDoc, _, err := s.RestoreStreamDoc(rootStreamID)
	if err != nil {
		return err
	}
	update, err := ApplyRootIntents(rootDoc, intents)
	if err != nil {
		return err
	}
	_, err = s.ApplyStreamUpdate(rootStreamID, update, meta)
	return err
}

func (s *Store) MirrorDocumentDeleteToRoot(documentID string, deletedAt time.Time, meta OperationMeta) error {
	rootStreamID, manifest, _, err := s.ReadRootManifestStream()
	if err != nil {
		return err
	}
	if _, ok := manifest.EntriesByID[documentID]; !ok {
		return ErrNotFound
	}
	rootDoc, _, err := s.RestoreStreamDoc(rootStreamID)
	if err != nil {
		return err
	}
	update, err := ApplyRootIntents(rootDoc, []RootIntent{{
		Type:      "tombstone",
		EntryID:   documentID,
		UpdatedBy: meta.ActorID,
		UpdatedAt: deletedAt.Format(time.RFC3339Nano),
		Tombstone: &RootTombstone{
			ActorID:   meta.ActorID,
			ActorType: meta.ActorType,
			At:        deletedAt.Format(time.RFC3339Nano),
		},
	}})
	if err != nil {
		return err
	}
	_, err = s.ApplyStreamUpdate(rootStreamID, update, meta)
	return err
}

func (s *Store) MirrorContentUpdateToStream(documentID string, update []byte, meta OperationMeta) error {
	if len(update) == 0 {
		return nil
	}
	if err := s.EnsureStream(documentID, StreamKindContent); err != nil {
		return err
	}
	_, err := s.ApplyStreamUpdate(documentID, update, meta)
	return err
}

func (s *Store) streamDocumentFromEntry(entry RootEntry, materializedPath string, desiredPath string) *Document {
	document := &Document{
		ID:                 entry.ID,
		Path:               materializedPath,
		DesiredPath:        desiredPath,
		Title:              titleFromPath(materializedPath),
		NotificationPolicy: rootNotificationPolicyForDocument(entry.NotificationPolicy),
		ClientIDSeed:       1001,
	}
	if updatedAt, ok := parseRootEntryTime(entry.UpdatedAt); ok {
		document.UpdatedAt = updatedAt
	} else if createdAt, ok := parseRootEntryTime(entry.CreatedAt); ok {
		document.UpdatedAt = createdAt
	}
	if head, err := s.GetStreamHead(entry.ContentStreamID); err == nil {
		document.UpdateID = head.UpdateID
		document.UpdatedAt = head.UpdatedAt
		if len(head.StateVector) > 0 {
			document.StateVector = base64.StdEncoding.EncodeToString(head.StateVector)
		}
	}
	return document
}

func rootNotificationPolicyForDocument(policy string) string {
	policy = strings.TrimSpace(policy)
	if policy == "" || policy == RootNotificationPolicyNormal {
		return ""
	}
	return policy
}

func rootCreateFileIntents(manifest RootManifest, documentID string, documentPath string, notificationPolicy string, now time.Time, meta OperationMeta) ([]RootIntent, error) {
	intents, loc, err := rootLocationIntentsForPath(manifest, documentPath, now, meta)
	if err != nil {
		return nil, err
	}
	stamp := now.Format(time.RFC3339Nano)
	intents = append(intents, RootIntent{
		Type: "create-file",
		Entry: RootEntry{
			ID:                 documentID,
			Kind:               RootEntryKindFile,
			Loc:                loc,
			ContentStreamID:    documentID,
			CreatedBy:          meta.ActorID,
			UpdatedBy:          meta.ActorID,
			CreatedAt:          stamp,
			UpdatedAt:          stamp,
			NotificationPolicy: strings.TrimSpace(notificationPolicy),
		},
	})
	return intents, nil
}

func rootLocationIntentsForPath(manifest RootManifest, documentPath string, now time.Time, meta OperationMeta) ([]RootIntent, *RootLocation, error) {
	normalized, err := normalizeDocumentPath(documentPath)
	if err != nil {
		return nil, nil, err
	}
	parts := strings.Split(normalized, "/")
	if len(parts) == 0 {
		return nil, nil, errors.New("path is required")
	}
	parentID := RootEntryID
	intents := []RootIntent{}
	working := cloneRootManifest(manifest)
	stamp := now.Format(time.RFC3339Nano)
	for _, segment := range parts[:len(parts)-1] {
		if existing, ok := findLiveChildDir(working, parentID, segment); ok {
			parentID = existing.ID
			continue
		}
		dirID := "dir_" + uuid.NewString()
		entry := RootEntry{
			ID:        dirID,
			Kind:      RootEntryKindDir,
			Loc:       NewRootLocation(parentID, segment),
			CreatedBy: meta.ActorID,
			UpdatedBy: meta.ActorID,
			CreatedAt: stamp,
			UpdatedAt: stamp,
		}
		intents = append(intents, RootIntent{Type: "create-dir", Entry: entry})
		working.EntriesByID[dirID] = entry
		parentID = dirID
	}
	return intents, NewRootLocation(parentID, parts[len(parts)-1]), nil
}

func findLiveChildDir(manifest RootManifest, parentID string, name string) (RootEntry, bool) {
	normName := NormalizeRootManifestName(name)
	matches := []RootEntry{}
	for _, entry := range manifest.EntriesByID {
		if entry.Kind != RootEntryKindDir || entry.Tombstone != nil || entry.Loc == nil {
			continue
		}
		if entry.Loc.ParentID == parentID && entry.Loc.NormName == normName {
			matches = append(matches, entry)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	if len(matches) == 0 {
		return RootEntry{}, false
	}
	return matches[0], true
}

func sortedRootEntries(manifest RootManifest) []RootEntry {
	entries := make([]RootEntry, 0, len(manifest.EntriesByID))
	for _, entry := range manifest.EntriesByID {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

func contentStreamUpdate(content string, clientIDSeed uint64) []byte {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientIDSeed)))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, content, nil)
	}, "seed")
	return doc.EncodeStateAsUpdate()
}

func parseRootEntryTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}
