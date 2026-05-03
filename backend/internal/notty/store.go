package notty

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/reearth/ygo/crdt"
)

var ErrNotFound = errors.New("not found")
var ErrDocumentDiffTooLarge = errors.New("document diff too large")

var mentionPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([a-z0-9][a-z0-9_-]{0,31})`)

const (
	agentRunLogPreviewLines  = 5
	agentRunLogLineLimit     = 240
	agentRunErrorLimit       = 1000
	maxDiffInputBytesPerSide = 2 * 1024 * 1024
	maxDiffLinesPerSide      = 20000
	maxDiffLineProduct       = 2000000
	maxDiffResponseBytes     = 1024 * 1024
)

type Store struct {
	mu       sync.RWMutex
	state    WorkspaceState
	db       *sql.DB
	dataFile string

	documentLocks         map[string]*sync.RWMutex
	dirtyDocuments        map[string]struct{}
	deletedDocuments      map[string]struct{}
	pendingDocumentEvents []documentUpdateRecord
	dirtyAgentEvents      bool
}

type documentUpdateRecord struct {
	DocumentID string
	Update     []byte
	ActorID    string
	ActorType  string
	CreatedAt  time.Time
}

type ApplyCRDTUpdateResult struct {
	Document *Document
	Applied  bool
}

type principalRef struct {
	UserID string
	Handle string
	Name   string
	Kind   string
}

func NewStore(dataFile string) (*Store, error) {
	store := &Store{}
	if isPostgresDSN(dataFile) {
		db, err := sql.Open("pgx", dataFile)
		if err != nil {
			return nil, err
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := initPostgresSchema(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		store.db = db
	} else {
		store.dataFile = dataFile
	}
	if err := store.load(); err != nil {
		if store.db != nil {
			_ = store.db.Close()
		}
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		s.state = seedWorkspace()
		if err := s.loadNormalizedPostgresLocked(); err != nil {
			return err
		}
		s.ensureMaps()
		needsPersist := false
		if s.refreshAgentSystemPromptsLocked() {
			needsPersist = true
		}
		s.refreshThreadParticipantsLocked()
		if s.reconcileThreadMentionEventsLocked() {
			needsPersist = true
		}
		if needsPersist {
			return s.persistLocked()
		}
		return nil
	}

	if _, err := os.Stat(s.dataFile); errors.Is(err, os.ErrNotExist) {
		s.state = seedWorkspace()
		return s.persistLocked()
	}

	bytes, err := os.ReadFile(s.dataFile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(bytes, &s.state); err != nil {
		return err
	}
	s.ensureMaps()
	for _, document := range s.state.Documents {
		doc, err := decodeCRDTState(document.CRDTState, document.ClientIDSeed)
		if err != nil {
			return err
		}
		document.Doc = doc
		document.SyncDerivedFields()
	}
	needsPersist := false
	if s.refreshAgentSystemPromptsLocked() {
		needsPersist = true
	}
	s.refreshThreadParticipantsLocked()
	if s.reconcileThreadMentionEventsLocked() {
		needsPersist = true
	}
	if needsPersist {
		return s.persistLocked()
	}
	return nil
}

func (s *Store) ensureMaps() {
	if s.state.Documents == nil {
		s.state.Documents = map[string]*Document{}
	}
	if s.state.Users == nil {
		s.state.Users = map[string]*User{}
	}
	if s.state.Agents == nil {
		s.state.Agents = map[string]*Agent{}
	}
	if s.state.AgentRuns == nil {
		s.state.AgentRuns = map[string]*AgentRun{}
	}
	if s.state.Threads == nil {
		s.state.Threads = map[string]*Thread{}
	}
	if s.state.AgentEvents == nil {
		s.state.AgentEvents = map[string]*AgentEvent{}
	}
	if s.state.AgentDocumentViews == nil {
		s.state.AgentDocumentViews = map[string]*AgentDocumentView{}
	}
	if s.state.DocumentCheckpoints == nil {
		s.state.DocumentCheckpoints = map[string]*DocumentCheckpoint{}
	}
	if s.state.Presences == nil {
		s.state.Presences = map[string]*Presence{}
	}
	if s.state.Activities == nil {
		s.state.Activities = []*ActivityEvent{}
	}
	if s.documentLocks == nil {
		s.documentLocks = map[string]*sync.RWMutex{}
	}
	if s.dirtyDocuments == nil {
		s.dirtyDocuments = map[string]struct{}{}
	}
	if s.deletedDocuments == nil {
		s.deletedDocuments = map[string]struct{}{}
	}
	if s.pendingDocumentEvents == nil {
		s.pendingDocumentEvents = []documentUpdateRecord{}
	}
	if len(s.state.Users) == 0 {
		now := time.Now().UTC()
		s.state.Users["user_owner"] = &User{
			ID:        "user_owner",
			Handle:    "owner",
			Name:      "Workspace Owner",
			Role:      "Coordinates the shared workspace",
			Kind:      "human",
			Status:    "active",
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
}

func (s *Store) documentLockLocked(documentID string) *sync.RWMutex {
	if s.documentLocks == nil {
		s.documentLocks = map[string]*sync.RWMutex{}
	}
	lock := s.documentLocks[documentID]
	if lock == nil {
		lock = &sync.RWMutex{}
		s.documentLocks[documentID] = lock
	}
	return lock
}

func seedWorkspace() WorkspaceState {
	now := time.Now().UTC()
	return WorkspaceState{
		WorkspaceID: "ws_notty",
		Name:        "notty",
		Documents:   map[string]*Document{},
		Users: map[string]*User{
			"user_owner": {
				ID:        "user_owner",
				Handle:    "owner",
				Name:      "Workspace Owner",
				Role:      "Coordinates the shared workspace",
				Kind:      "human",
				Status:    "active",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
		Agents:              map[string]*Agent{},
		AgentRuns:           map[string]*AgentRun{},
		Threads:             map[string]*Thread{},
		AgentEvents:         map[string]*AgentEvent{},
		AgentDocumentViews:  map[string]*AgentDocumentView{},
		DocumentCheckpoints: map[string]*DocumentCheckpoint{},
		Presences:           map[string]*Presence{},
		Activities:          []*ActivityEvent{},
		UpdatedAt:           now,
	}
}

func newSeedDocument(id string, clientID uint64, path string, title string, content string, now time.Time) *Document {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, content, nil)
	}, "seed")
	document := &Document{
		ID:           id,
		Path:         path,
		Title:        title,
		UpdatedAt:    now,
		ClientIDSeed: clientID,
		Doc:          doc,
	}
	document.SyncDerivedFields()
	return document
}

func decodeCRDTState(encoded string, clientIDSeed uint64) (*crdt.Doc, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientIDSeed)))
	if encoded == "" {
		return doc, nil
	}
	update, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if err := crdt.ApplyUpdateV1(doc, update, "restore"); err != nil {
		return nil, err
	}
	return doc, nil
}

func (s *Store) Snapshot() WorkspaceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

func (s *Store) GetDocument(id string) (*Document, error) {
	s.mu.Lock()
	document, ok := s.state.Documents[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.RLock()
	s.mu.Unlock()
	defer documentLock.RUnlock()
	if s.db != nil {
		return cloneDocument(document), nil
	}
	return cloneDocumentWithCRDTState(document), nil
}

func (s *Store) ListDocuments() []*Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SortedDocuments(s.state)
}

func (s *Store) GetDocumentByPath(path string) (*Document, error) {
	normalized, err := normalizeDocumentPath(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, document := range s.state.Documents {
		if document.Path == normalized {
			documentLock := s.documentLockLocked(document.ID)
			documentLock.RLock()
			s.mu.Unlock()
			defer documentLock.RUnlock()
			if s.db != nil {
				return cloneDocument(document), nil
			}
			return cloneDocumentWithCRDTState(document), nil
		}
	}
	s.mu.Unlock()
	return nil, ErrNotFound
}

func (s *Store) GetDocumentMetadataByPath(path string) (*DocumentMetadata, error) {
	normalized, err := normalizeDocumentPath(path)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, document := range s.state.Documents {
		if document.Path == normalized {
			return documentMetadata(document), nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) HasDocument(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.state.Documents[id]
	return ok
}

func (s *Store) GetLiveDocument(id string) (*Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	document, ok := s.state.Documents[id]
	if !ok {
		return nil, ErrNotFound
	}
	return document, nil
}

func (s *Store) EncodeDocumentUpdate(documentID string, stateVector []byte) (*DocumentMetadata, []byte, error) {
	s.mu.Lock()
	document, ok := s.state.Documents[documentID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.RLock()
	s.mu.Unlock()
	defer documentLock.RUnlock()
	var decoded crdt.StateVector
	var err error
	if len(stateVector) > 0 {
		decoded, err = crdt.DecodeStateVectorV1(stateVector)
		if err != nil {
			return nil, nil, err
		}
	}
	doc := document.Doc
	metadata := documentMetadata(document)
	if s.db != nil && document.Doc == nil {
		doc, err = s.restoreDocumentDocPostgresLocked(document)
		if err != nil {
			return nil, nil, err
		}
		metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	}
	if doc == nil {
		return nil, nil, errors.New("document CRDT state is unavailable")
	}
	update := crdt.EncodeStateAsUpdateV1(doc, decoded)
	return metadata, update, nil
}

func (s *Store) EncodeDocumentSyncUpdates(documentID string, stateVector []byte) (*DocumentMetadata, [][]byte, error) {
	document, updates, optimized, err := s.encodeDocumentCheckpointSyncUpdates(documentID, stateVector)
	if err != nil {
		return nil, nil, err
	}
	if optimized {
		return document, updates, nil
	}
	document, update, err := s.EncodeDocumentUpdate(documentID, stateVector)
	if err != nil {
		return nil, nil, err
	}
	return document, [][]byte{update}, nil
}

func (s *Store) encodeDocumentCheckpointSyncUpdates(documentID string, stateVector []byte) (*DocumentMetadata, [][]byte, bool, error) {
	if s.db == nil {
		return nil, nil, false, nil
	}
	var decoded crdt.StateVector
	if len(stateVector) > 0 {
		var err error
		decoded, err = crdt.DecodeStateVectorV1(stateVector)
		if err != nil {
			return nil, nil, false, err
		}
	}

	s.mu.Lock()
	document, ok := s.state.Documents[documentID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, false, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	workspaceID := s.state.WorkspaceID
	headUpdateID := document.UpdateID
	metadata := documentMetadata(document)
	if len(stateVector) > 0 && document.StateVector != "" && (s.db == nil || document.Doc != nil) {
		currentStateVector, err := base64.StdEncoding.DecodeString(document.StateVector)
		if err != nil {
			s.mu.Unlock()
			documentLock.Unlock()
			return nil, nil, false, err
		}
		if bytes.Equal(stateVector, currentStateVector) {
			s.mu.Unlock()
			documentLock.Unlock()
			return metadata, nil, true, nil
		}
	}
	if document.Doc != nil {
		update := crdt.EncodeStateAsUpdateV1(document.Doc, decoded)
		s.mu.Unlock()
		documentLock.Unlock()
		return metadata, [][]byte{update}, true, nil
	}
	s.mu.Unlock()
	defer documentLock.Unlock()

	updates, ok, err := loadDocumentBootstrapUpdatesPostgres(s.db, workspaceID, documentID, headUpdateID, stateVector)
	if err != nil {
		return nil, nil, false, err
	}
	if ok {
		if len(stateVector) > 0 {
			doc, err := s.restoreDocumentDocPostgresLocked(document)
			if err != nil {
				return nil, nil, false, err
			}
			metadata = documentMetadata(document)
			metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
		}
		return metadata, updates, true, nil
	}

	doc, err := s.restoreDocumentDocPostgresLocked(document)
	if err != nil {
		return nil, nil, false, err
	}
	metadata = documentMetadata(document)
	metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	update := crdt.EncodeStateAsUpdateV1(doc, decoded)
	return metadata, [][]byte{update}, true, nil
}

func (s *Store) GetThread(id string) (*Thread, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	thread, ok := s.state.Threads[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneThread(thread), nil
}

func (s *Store) ListThreadsForDocument(documentID string) ([]*Thread, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.state.Documents[documentID]; !ok {
		return nil, ErrNotFound
	}
	threads := make([]*Thread, 0)
	for _, thread := range s.state.Threads {
		if thread.DocumentID == documentID {
			threads = append(threads, cloneThread(thread))
		}
	}
	sort.Slice(threads, func(i, j int) bool {
		if threads[i].UpdatedAt.Equal(threads[j].UpdatedAt) {
			return threads[i].ID < threads[j].ID
		}
		return threads[i].UpdatedAt.After(threads[j].UpdatedAt)
	})
	return threads, nil
}

func (s *Store) ListAgentNotifications(agentID string, statuses ...string) ([]*AgentEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	allowed := map[string]struct{}{}
	for _, status := range statuses {
		if trimmed := strings.TrimSpace(status); trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}
	notifications := make([]*AgentEvent, 0)
	for _, event := range s.state.AgentEvents {
		if event == nil || event.AgentID != resolvedAgentID {
			continue
		}
		if s.eventBelongsToLogDocumentLocked(event) {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[event.Status]; !ok {
				continue
			}
		}
		notifications = append(notifications, cloneAgentEvent(event))
	}
	sort.Slice(notifications, func(i, j int) bool {
		if notifications[i].CreatedAt.Equal(notifications[j].CreatedAt) {
			return notifications[i].ID < notifications[j].ID
		}
		return notifications[i].CreatedAt.Before(notifications[j].CreatedAt)
	})
	return notifications, nil
}

func (s *Store) ListAgentInbox(agentID string, box string, statuses ...string) ([]*AgentEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	box = normalizeInboxBox(box)
	allowed := map[string]struct{}{}
	for _, status := range statuses {
		if trimmed := strings.TrimSpace(status); trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}
	notifications := make([]*AgentEvent, 0)
	seen := map[string]struct{}{}
	for _, event := range s.state.AgentEvents {
		if event == nil || event.AgentID != resolvedAgentID {
			continue
		}
		if s.eventBelongsToLogDocumentLocked(event) {
			continue
		}
		if normalizeInboxBox(event.Box) != box {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[event.Status]; !ok {
				continue
			}
		}
		cloned := cloneAgentEvent(event)
		notifications = append(notifications, cloned)
		seen[inboxDedupKey(cloned)] = struct{}{}
	}
	if includesPendingStatus(allowed) {
		for _, event := range s.computedDocumentInboxLocked(resolvedAgentID) {
			if event == nil || normalizeInboxBox(event.Box) != box {
				continue
			}
			key := inboxDedupKey(event)
			if _, ok := seen[key]; ok {
				continue
			}
			notifications = append(notifications, event)
			seen[key] = struct{}{}
		}
	}
	sort.Slice(notifications, func(i, j int) bool {
		if notifications[i].CreatedAt.Equal(notifications[j].CreatedAt) {
			return notifications[i].ID < notifications[j].ID
		}
		return notifications[i].CreatedAt.Before(notifications[j].CreatedAt)
	})
	return notifications, nil
}

func (s *Store) GetAgentNotification(id string) (*AgentEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	event, ok := s.state.AgentEvents[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAgentEvent(event), nil
}

func (s *Store) UpdateAgentNotification(id string, req UpdateAgentNotificationRequest, meta OperationMeta) (*AgentEvent, error) {
	return s.UpdateAgentEvent(id, UpdateAgentEventRequest{
		Status: strings.TrimSpace(req.Status),
	}, meta)
}

func (s *Store) GetAgentInboxItem(id string) (*AgentEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	event, ok := s.state.AgentEvents[id]
	if !ok {
		if synthetic, ok := s.syntheticDocumentInboxItemLocked(id); ok {
			return synthetic, nil
		}
		return nil, ErrNotFound
	}
	return cloneAgentEvent(event), nil
}

func (s *Store) UpdateAgentInboxItem(id string, req UpdateAgentNotificationRequest, meta OperationMeta) (*AgentEvent, error) {
	if spec, ok := parseSyntheticDocumentInboxID(id); ok {
		status := strings.TrimSpace(req.Status)
		if status == "" {
			status = "completed"
		}
		view, err := s.MarkDocumentViewed(spec.AgentID, spec.DocumentID, MarkDocumentViewedRequest{})
		if err != nil {
			return nil, err
		}
		event := &AgentEvent{
			ID:           id,
			AgentID:      spec.AgentID,
			Type:         "document.updated",
			Box:          normalizeInboxBox(spec.Box),
			Status:       status,
			DocumentID:   spec.DocumentID,
			ToUpdateID:   view.UpdateID,
			Summary:      "Document update marked viewed",
			CreatedAt:    view.ViewedAt,
			UpdatedAt:    view.ViewedAt,
			CompletedAt:  view.ViewedAt,
			AvailableAt:  view.ViewedAt,
			FromUpdateID: view.UpdateID,
		}
		return event, nil
	}
	return s.UpdateAgentEvent(id, UpdateAgentEventRequest{
		Status: strings.TrimSpace(req.Status),
	}, meta)
}

func (s *Store) MarkDocumentViewed(agentID, documentID string, req MarkDocumentViewedRequest) (*AgentDocumentView, error) {
	s.mu.Lock()

	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	document, ok := s.state.Documents[strings.TrimSpace(documentID)]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	updateID := req.UpdateID
	if updateID <= 0 || updateID > document.UpdateID {
		updateID = document.UpdateID
	}
	stateVector := document.StateVector
	if updateID != document.UpdateID {
		stateVector = ""
	}
	now := time.Now().UTC()
	view := &AgentDocumentView{
		AgentID:     resolvedAgentID,
		DocumentID:  document.ID,
		UpdateID:    updateID,
		StateVector: stateVector,
		ViewedAt:    now,
	}
	s.state.AgentDocumentViews[agentDocumentViewKey(resolvedAgentID, document.ID)] = view
	s.state.UpdatedAt = now
	if s.db != nil {
		workspaceID := s.state.WorkspaceID
		cloned := cloneAgentDocumentView(view)
		s.mu.Unlock()
		if err := upsertAgentDocumentViewPostgres(s.db, workspaceID, cloned); err != nil {
			return nil, err
		}
		return cloned, nil
	}
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	cloned := cloneAgentDocumentView(view)
	s.mu.Unlock()
	return cloned, nil
}

func (s *Store) DiffDocument(agentID, documentID, fromSpec, toSpec string) (*DocumentDiff, error) {
	s.mu.RLock()

	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	document, ok := s.state.Documents[strings.TrimSpace(documentID)]
	if !ok {
		s.mu.RUnlock()
		return nil, ErrNotFound
	}
	fromUpdateID, err := s.resolveDocumentVersionLocked(resolvedAgentID, document, fromSpec, "last-viewed")
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	toUpdateID, err := s.resolveDocumentVersionLocked(resolvedAgentID, document, toSpec, "head")
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	if fromUpdateID > toUpdateID {
		s.mu.RUnlock()
		return nil, fmt.Errorf("from version %d is newer than to version %d", fromUpdateID, toUpdateID)
	}
	if fromUpdateID == toUpdateID {
		s.mu.RUnlock()
		return emptyDocumentDiff(document.ID, fromUpdateID, toUpdateID), nil
	}
	if s.db == nil {
		defer s.mu.RUnlock()
		fromContent, err := s.documentContentAtUpdateLocked(document, fromUpdateID)
		if err != nil {
			return nil, err
		}
		toContent, err := s.documentContentAtUpdateLocked(document, toUpdateID)
		if err != nil {
			return nil, err
		}
		return buildDocumentDiff(document.ID, fromUpdateID, toUpdateID, fromContent, toContent)
	}
	documentSnapshot := cloneDocument(document)
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()

	fromContent, err := documentContentAtUpdatePostgres(db, workspaceID, documentSnapshot, fromUpdateID)
	if err != nil {
		return nil, err
	}
	toContent, err := documentContentAtUpdatePostgres(db, workspaceID, documentSnapshot, toUpdateID)
	if err != nil {
		return nil, err
	}
	return buildDocumentDiff(document.ID, fromUpdateID, toUpdateID, fromContent, toContent)
}

type syntheticDocumentInboxSpec struct {
	Box        string
	AgentID    string
	DocumentID string
}

func normalizeInboxBox(box string) string {
	switch strings.TrimSpace(strings.ToLower(strings.ReplaceAll(box, "-", "_"))) {
	case "general":
		return "general"
	default:
		return "for_me"
	}
}

func includesPendingStatus(allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	_, ok := allowed["pending"]
	return ok
}

func syntheticDocumentInboxID(box, agentID, documentID string) string {
	return "docinbox:" + normalizeInboxBox(box) + ":" + agentID + ":" + documentID
}

func parseSyntheticDocumentInboxID(id string) (syntheticDocumentInboxSpec, bool) {
	parts := strings.SplitN(strings.TrimSpace(id), ":", 4)
	if len(parts) != 4 || parts[0] != "docinbox" || parts[2] == "" || parts[3] == "" {
		return syntheticDocumentInboxSpec{}, false
	}
	return syntheticDocumentInboxSpec{Box: normalizeInboxBox(parts[1]), AgentID: parts[2], DocumentID: parts[3]}, true
}

func inboxDedupKey(event *AgentEvent) string {
	if event == nil {
		return ""
	}
	if event.DocumentID != "" && strings.HasPrefix(event.Type, "document.") {
		return normalizeInboxBox(event.Box) + ":document:" + event.DocumentID
	}
	return event.ID
}

func (s *Store) eventBelongsToLogDocumentLocked(event *AgentEvent) bool {
	if event == nil || event.DocumentID == "" {
		return false
	}
	if !strings.HasPrefix(event.Type, "document.") {
		return false
	}
	document := s.state.Documents[event.DocumentID]
	return document != nil && isLogDocumentPath(document.Path)
}

func (s *Store) computedDocumentInboxLocked(agentID string) []*AgentEvent {
	items := make([]*AgentEvent, 0)
	for _, document := range s.state.Documents {
		if document == nil || document.UpdateID <= 0 {
			continue
		}
		if isLogDocumentPath(document.Path) {
			continue
		}
		if s.documentInboxHandledLocked(agentID, document) {
			continue
		}
		view := s.state.AgentDocumentViews[agentDocumentViewKey(agentID, document.ID)]
		fromUpdateID := int64(0)
		if view != nil {
			fromUpdateID = view.UpdateID
		}
		if fromUpdateID >= document.UpdateID {
			continue
		}
		box, eventType, anchorStart, anchorEnd := s.documentInboxClassificationLocked(agentID, document)
		items = append(items, &AgentEvent{
			ID:           syntheticDocumentInboxID(box, agentID, document.ID),
			AgentID:      agentID,
			AgentHandle:  s.agentHandleByIDLocked(agentID),
			Type:         eventType,
			Box:          box,
			Status:       "pending",
			DocumentID:   document.ID,
			AnchorStart:  anchorStart,
			AnchorEnd:    anchorEnd,
			FromUpdateID: fromUpdateID,
			ToUpdateID:   document.UpdateID,
			Summary:      fmt.Sprintf("%s changed from version %d to %d", document.Path, fromUpdateID, document.UpdateID),
			Prompt:       fmt.Sprintf("Review %s with notty-agent-tool diff-document --document-id %s --from %d --to %d. Act only if you have useful feedback or edits.", document.Path, document.ID, fromUpdateID, document.UpdateID),
			AvailableAt:  document.UpdatedAt,
			CreatedAt:    document.UpdatedAt,
			UpdatedAt:    document.UpdatedAt,
		})
	}
	return items
}

func (s *Store) documentInboxHandledLocked(agentID string, document *Document) bool {
	for _, event := range s.state.AgentEvents {
		if event == nil || event.AgentID != agentID || event.DocumentID != document.ID || !strings.HasPrefix(event.Type, "document.") {
			continue
		}
		if event.Status != "completed" && event.Status != "dismissed" {
			continue
		}
		if event.ToUpdateID >= document.UpdateID {
			return true
		}
		if event.ToUpdateID == 0 && !event.UpdatedAt.Before(document.UpdatedAt) {
			return true
		}
	}
	return false
}

func (s *Store) syntheticDocumentInboxItemLocked(id string) (*AgentEvent, bool) {
	spec, ok := parseSyntheticDocumentInboxID(id)
	if !ok {
		return nil, false
	}
	items := s.computedDocumentInboxLocked(spec.AgentID)
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return nil, false
}

func (s *Store) documentInboxClassificationLocked(agentID string, document *Document) (box string, eventType string, anchorStart int, anchorEnd int) {
	for _, thread := range s.state.Threads {
		if thread == nil || thread.DocumentID != document.ID || thread.Status != "open" {
			continue
		}
		if containsText(thread.ParticipantIDs, agentID) {
			return "for_me", "document.updated", thread.Anchor.Start, thread.Anchor.End
		}
	}
	return "general", "document.updated", 0, 0
}

func (s *Store) agentHandleByIDLocked(agentID string) string {
	if agent := s.state.Agents[agentID]; agent != nil {
		return agent.Handle
	}
	return ""
}

func agentDocumentViewKey(agentID, documentID string) string {
	return agentID + ":" + documentID
}

func documentCheckpointKey(documentID string, updateID int64) string {
	return documentID + ":" + strconv.FormatInt(updateID, 10)
}

func cloneAgentDocumentView(view *AgentDocumentView) *AgentDocumentView {
	if view == nil {
		return nil
	}
	clone := *view
	return &clone
}

func (s *Store) resolveDocumentVersionLocked(agentID string, document *Document, spec string, defaultSpec string) (int64, error) {
	value := strings.TrimSpace(strings.ToLower(spec))
	if value == "" {
		value = strings.TrimSpace(strings.ToLower(defaultSpec))
	}
	switch value {
	case "head", "current", "latest":
		return document.UpdateID, nil
	case "last-viewed", "last_viewed", "viewed":
		if view := s.state.AgentDocumentViews[agentDocumentViewKey(agentID, document.ID)]; view != nil {
			return view.UpdateID, nil
		}
		return 0, nil
	}
	value = strings.TrimPrefix(value, "update:")
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid document version %q", spec)
	}
	if parsed > document.UpdateID {
		return 0, fmt.Errorf("document version %d is newer than head %d", parsed, document.UpdateID)
	}
	return parsed, nil
}

func (s *Store) documentContentAtUpdateLocked(document *Document, updateID int64) (string, error) {
	if document == nil {
		return "", ErrNotFound
	}
	if updateID <= 0 {
		return "", nil
	}
	if s.db != nil {
		return s.documentContentAtUpdatePostgresLocked(document, updateID)
	}
	if updateID >= document.UpdateID {
		return document.Content, nil
	}
	checkpoint := s.bestDocumentCheckpointLocked(document.ID, updateID)
	if checkpoint == nil {
		return "", fmt.Errorf("no checkpoint available for %s at version %d", document.ID, updateID)
	}
	doc, err := decodeCRDTState(checkpoint.CRDTState, document.ClientIDSeed)
	if err != nil {
		return "", err
	}
	if checkpoint.UpdateID != updateID {
		return "", fmt.Errorf("no update log available after checkpoint %d for %s at version %d", checkpoint.UpdateID, document.ID, updateID)
	}
	return doc.GetText("content").ToString(), nil
}

func (s *Store) bestDocumentCheckpointLocked(documentID string, updateID int64) *DocumentCheckpoint {
	var best *DocumentCheckpoint
	for _, checkpoint := range s.state.DocumentCheckpoints {
		if checkpoint == nil || checkpoint.DocumentID != documentID || checkpoint.UpdateID > updateID {
			continue
		}
		if best == nil || checkpoint.UpdateID > best.UpdateID {
			best = checkpoint
		}
	}
	return best
}

func diffTextByLine(before, after string) []DocumentDiffHunk {
	a := splitLinesKeepEnd(before)
	b := splitLinesKeepEnd(after)
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	hunks := make([]DocumentDiffHunk, 0)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			hunks = appendMergedHunk(hunks, "equal", a[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			hunks = appendMergedHunk(hunks, "delete", a[i])
			i++
		default:
			hunks = appendMergedHunk(hunks, "insert", b[j])
			j++
		}
	}
	for i < len(a) {
		hunks = appendMergedHunk(hunks, "delete", a[i])
		i++
	}
	for j < len(b) {
		hunks = appendMergedHunk(hunks, "insert", b[j])
		j++
	}
	return hunks
}

func emptyDocumentDiff(documentID string, fromUpdateID int64, toUpdateID int64) *DocumentDiff {
	return &DocumentDiff{
		DocumentID:   documentID,
		FromUpdateID: fromUpdateID,
		ToUpdateID:   toUpdateID,
		Hunks:        []DocumentDiffHunk{},
	}
}

func buildDocumentDiff(documentID string, fromUpdateID int64, toUpdateID int64, fromContent string, toContent string) (*DocumentDiff, error) {
	if fromContent == toContent {
		return emptyDocumentDiff(documentID, fromUpdateID, toUpdateID), nil
	}
	if err := validateDocumentDiffSize(fromContent, toContent); err != nil {
		return nil, err
	}
	if len(fromContent)+len(toContent) > maxDiffResponseBytes {
		return nil, fmt.Errorf("%w: response content is at least %d bytes, limit is %d bytes", ErrDocumentDiffTooLarge, len(fromContent)+len(toContent), maxDiffResponseBytes)
	}
	hunks := diffTextByLine(fromContent, toContent)
	unified := renderUnifiedDiff(hunks)
	if responseBytes := len(fromContent) + len(toContent) + len(unified) + documentDiffHunkTextBytes(hunks); responseBytes > maxDiffResponseBytes {
		return nil, fmt.Errorf("%w: response is %d bytes, limit is %d bytes", ErrDocumentDiffTooLarge, responseBytes, maxDiffResponseBytes)
	}
	return &DocumentDiff{
		DocumentID:   documentID,
		FromUpdateID: fromUpdateID,
		ToUpdateID:   toUpdateID,
		FromContent:  fromContent,
		ToContent:    toContent,
		Unified:      unified,
		Hunks:        hunks,
	}, nil
}

func validateDocumentDiffSize(before, after string) error {
	if len(before) > maxDiffInputBytesPerSide {
		return fmt.Errorf("%w: from content is %d bytes, limit is %d bytes", ErrDocumentDiffTooLarge, len(before), maxDiffInputBytesPerSide)
	}
	if len(after) > maxDiffInputBytesPerSide {
		return fmt.Errorf("%w: to content is %d bytes, limit is %d bytes", ErrDocumentDiffTooLarge, len(after), maxDiffInputBytesPerSide)
	}
	beforeLines := countDiffLines(before)
	afterLines := countDiffLines(after)
	if beforeLines > maxDiffLinesPerSide {
		return fmt.Errorf("%w: from content has %d lines, limit is %d lines", ErrDocumentDiffTooLarge, beforeLines, maxDiffLinesPerSide)
	}
	if afterLines > maxDiffLinesPerSide {
		return fmt.Errorf("%w: to content has %d lines, limit is %d lines", ErrDocumentDiffTooLarge, afterLines, maxDiffLinesPerSide)
	}
	lineProduct := beforeLines * afterLines
	if lineProduct > maxDiffLineProduct {
		return fmt.Errorf("%w: line product is %d, limit is %d", ErrDocumentDiffTooLarge, lineProduct, maxDiffLineProduct)
	}
	return nil
}

func documentDiffHunkTextBytes(hunks []DocumentDiffHunk) int {
	total := 0
	for _, hunk := range hunks {
		total += len(hunk.Text)
	}
	return total
}

func countDiffLines(value string) int {
	if value == "" {
		return 0
	}
	lines := strings.Count(value, "\n")
	if !strings.HasSuffix(value, "\n") {
		lines++
	}
	return lines
}

func splitLinesKeepEnd(value string) []string {
	if value == "" {
		return nil
	}
	lines := strings.SplitAfter(value, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func appendMergedHunk(hunks []DocumentDiffHunk, op, text string) []DocumentDiffHunk {
	if len(hunks) > 0 && hunks[len(hunks)-1].Op == op {
		hunks[len(hunks)-1].Text += text
		return hunks
	}
	return append(hunks, DocumentDiffHunk{Op: op, Text: text})
}

func renderUnifiedDiff(hunks []DocumentDiffHunk) string {
	var builder strings.Builder
	for _, hunk := range hunks {
		prefix := " "
		switch hunk.Op {
		case "insert":
			prefix = "+"
		case "delete":
			prefix = "-"
		}
		for _, line := range splitLinesKeepEnd(hunk.Text) {
			builder.WriteString(prefix)
			builder.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				builder.WriteString("\n")
			}
		}
	}
	return builder.String()
}

func (s *Store) CreateDocument(req CreateDocumentRequest, meta OperationMeta) (*Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := normalizeDocumentPath(req.Path)
	if err != nil {
		return nil, err
	}
	if s.documentExistsAtPathLocked(path, "") {
		return nil, fmt.Errorf("document already exists at %s", path)
	}

	now := time.Now().UTC()
	clientIDSeed := s.nextClientIDSeedLocked()
	id := "doc_" + uuid.NewString()
	document := newSeedDocument(id, clientIDSeed, path, titleFromPath(path), req.Content, now)
	s.state.Documents[id] = document
	_ = s.documentLockLocked(document.ID)
	s.markDocumentDirtyLocked(document.ID)
	s.appendFullDocumentUpdateLocked(document, meta, now)
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "document.created",
		DocumentID: document.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s created %s", meta.ActorID, document.Path),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistDocumentMutationLocked(); err != nil {
		return nil, err
	}
	created := cloneDocument(document)
	if s.db != nil {
		document.Doc = nil
		document.Content = ""
		document.CRDTState = ""
	}
	return created, nil
}

func (s *Store) MoveDocument(id, nextPath string, meta OperationMeta) (*Document, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.Documents[id]
	if !ok {
		return nil, "", ErrNotFound
	}
	path, err := normalizeDocumentPath(nextPath)
	if err != nil {
		return nil, "", err
	}
	if s.documentExistsAtPathLocked(path, id) {
		return nil, "", fmt.Errorf("document already exists at %s", path)
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	defer documentLock.Unlock()
	oldPath := document.Path
	document.Path = path
	document.Title = titleFromPath(path)
	document.UpdatedAt = time.Now().UTC()
	s.markDocumentDirtyLocked(document.ID)
	s.state.UpdatedAt = document.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "document.moved",
		DocumentID: document.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s moved %s to %s", meta.ActorID, oldPath, document.Path),
		OccurredAt: document.UpdatedAt,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, "", err
	}
	return cloneDocument(document), oldPath, nil
}

func (s *Store) DeleteDocument(id string, meta OperationMeta) (*Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.Documents[id]
	if !ok {
		return nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	defer documentLock.Unlock()
	s.markDocumentDeletedLocked(id)
	delete(s.state.Documents, id)
	for threadID, thread := range s.state.Threads {
		if thread.DocumentID == id {
			delete(s.state.Threads, threadID)
		}
	}
	for eventID, event := range s.state.AgentEvents {
		if event.DocumentID == id {
			delete(s.state.AgentEvents, eventID)
			continue
		}
		if event.ThreadID != "" {
			if _, ok := s.state.Threads[event.ThreadID]; !ok {
				delete(s.state.AgentEvents, eventID)
			}
		}
	}
	for actorID, presence := range s.state.Presences {
		if presence.DocumentID == id || presence.FilePath == document.Path {
			delete(s.state.Presences, actorID)
		}
	}
	now := time.Now().UTC()
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "document.deleted",
		DocumentID: id,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s deleted %s", meta.ActorID, document.Path),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistDocumentMutationLocked(); err != nil {
		return nil, err
	}
	return cloneDocument(document), nil
}

func (s *Store) CreateUser(req CreateUserRequest, meta OperationMeta) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, err := buildUser(req.Name, req.Handle, req.Role)
	if err != nil {
		return nil, err
	}
	if err := s.ensureHandleAvailableLocked(user.Handle, "", ""); err != nil {
		return nil, err
	}
	user.ID = "user_" + uuid.NewString()
	user.CreatedAt = time.Now().UTC()
	user.UpdatedAt = user.CreatedAt
	s.state.Users[user.ID] = user
	s.refreshThreadParticipantsLocked()
	s.state.UpdatedAt = user.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "user.created",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s created user @%s", meta.ActorID, user.Handle),
		OccurredAt: user.UpdatedAt,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneUser(user), nil
}

func (s *Store) UpdateUser(id string, req UpdateUserRequest, meta OperationMeta) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.state.Users[id]
	if !ok {
		return nil, ErrNotFound
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		user.Name = name
	}
	if role := strings.TrimSpace(req.Role); role != "" {
		user.Role = role
	}
	if req.Handle != "" {
		handle, err := normalizeHandle(req.Handle)
		if err != nil {
			return nil, err
		}
		if err := s.ensureHandleAvailableLocked(handle, id, ""); err != nil {
			return nil, err
		}
		user.Handle = handle
	}
	user.UpdatedAt = time.Now().UTC()
	s.refreshThreadParticipantsLocked()
	s.state.UpdatedAt = user.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "user.updated",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s updated user @%s", meta.ActorID, user.Handle),
		OccurredAt: user.UpdatedAt,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneUser(user), nil
}

func (s *Store) DeleteUser(id string, meta OperationMeta) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.state.Users[id]
	if !ok {
		return nil, ErrNotFound
	}
	if len(s.state.Users) == 1 {
		return nil, errors.New("workspace must keep at least one human user")
	}
	delete(s.state.Users, id)
	s.refreshThreadParticipantsLocked()
	now := time.Now().UTC()
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "user.deleted",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s deleted user @%s", meta.ActorID, user.Handle),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneUser(user), nil
}

func (s *Store) CreateAgent(req CreateAgentRequest, meta OperationMeta) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, err := buildAgent(req.Handle, req.Name, req.Role, req.Kind)
	if err != nil {
		return nil, err
	}
	if err := s.ensureHandleAvailableLocked(agent.Handle, "", ""); err != nil {
		return nil, err
	}
	agent.ID = "agent_" + uuid.NewString()
	agent.WorkspaceRoot = "agents/" + agent.ID
	agent.UpdatedAt = time.Now().UTC()
	s.state.Agents[agent.ID] = agent
	for _, document := range s.state.Documents {
		if document == nil {
			continue
		}
		s.state.AgentDocumentViews[agentDocumentViewKey(agent.ID, document.ID)] = &AgentDocumentView{
			AgentID:     agent.ID,
			DocumentID:  document.ID,
			UpdateID:    document.UpdateID,
			StateVector: document.StateVector,
			ViewedAt:    agent.UpdatedAt,
		}
	}
	s.refreshThreadParticipantsLocked()
	s.state.UpdatedAt = agent.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.created",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s created agent @%s", meta.ActorID, agent.Handle),
		OccurredAt: agent.UpdatedAt,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAgent(agent), nil
}

func (s *Store) UpdateAgent(id string, req UpdateAgentRequest, meta OperationMeta) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.state.Agents[id]
	if !ok {
		return nil, ErrNotFound
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		agent.Name = name
	}
	if role := strings.TrimSpace(req.Role); role != "" {
		agent.Role = role
	}
	if req.Handle != "" {
		handle, err := normalizeHandle(req.Handle)
		if err != nil {
			return nil, err
		}
		if err := s.ensureHandleAvailableLocked(handle, "", id); err != nil {
			return nil, err
		}
		agent.Handle = handle
	}
	agent.SystemPrompt = sharedAgentSystemPrompt(agent)
	agent.UpdatedAt = time.Now().UTC()
	s.refreshThreadParticipantsLocked()
	s.state.UpdatedAt = agent.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.updated",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s updated agent @%s", meta.ActorID, agent.Handle),
		OccurredAt: agent.UpdatedAt,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAgent(agent), nil
}

func (s *Store) DeleteAgent(id string, meta OperationMeta) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.state.Agents[id]
	if !ok {
		return nil, ErrNotFound
	}
	if agent.CurrentRunID != "" {
		if run, ok := s.state.AgentRuns[agent.CurrentRunID]; ok && !isTerminalRunStatus(run.Status) {
			return nil, errors.New("stop the active run before deleting this agent")
		}
	}
	delete(s.state.Agents, id)
	for runID, run := range s.state.AgentRuns {
		if run.AgentID == id {
			delete(s.state.AgentRuns, runID)
		}
	}
	for eventID, event := range s.state.AgentEvents {
		if event.AgentID == id {
			delete(s.state.AgentEvents, eventID)
		}
	}
	for _, thread := range s.state.Threads {
		thread.ParticipantIDs = removeString(thread.ParticipantIDs, id)
	}
	s.refreshThreadParticipantsLocked()
	now := time.Now().UTC()
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.deleted",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s deleted agent @%s", meta.ActorID, agent.Handle),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAgent(agent), nil
}

func (s *Store) StartAgentRun(req StartAgentRunRequest, meta OperationMeta) (*Agent, *AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, nil, errors.New("prompt is required")
	}

	agentID := strings.TrimSpace(req.AgentID)
	agent, ok := s.state.Agents[agentID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	if agent.CurrentRunID != "" {
		if active, ok := s.state.AgentRuns[agent.CurrentRunID]; ok && !isTerminalRunStatus(active.Status) {
			return nil, nil, fmt.Errorf("agent %s already has an active run", agent.Name)
		}
	}
	now := time.Now().UTC()
	run := &AgentRun{
		ID:              "run_" + uuid.NewString(),
		AgentID:         agentID,
		AgentHandle:     agent.Handle,
		AgentName:       agent.Name,
		AgentKind:       agent.Kind,
		SystemPrompt:    sharedAgentSystemPrompt(agent),
		SessionID:       agent.SessionID,
		WorkspaceID:     s.state.WorkspaceID,
		WorkspaceRoot:   agent.WorkspaceRoot,
		WorkingDir:      ".",
		Prompt:          prompt,
		Status:          "queued",
		DesiredStatus:   "running",
		LogTail:         []string{},
		AssignedTaskRef: strings.TrimSpace(req.AssignedTaskRef),
		UpdatedAt:       now,
	}
	s.state.AgentRuns[run.ID] = run
	agent.Status = "queued"
	agent.CurrentRunID = run.ID
	agent.CurrentTask = summarizePrompt(prompt)
	agent.CurrentActivity = "Queued in daemon"
	agent.UpdatedAt = now
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.run.created",
		ActorID:    agent.ID,
		ActorType:  "agent",
		Summary:    fmt.Sprintf("%s queued %s run", meta.ActorID, agent.Name),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, nil, err
	}
	return cloneAgent(agent), cloneAgentRun(run), nil
}

func (s *Store) UpdateAgentRun(id string, req UpdateAgentRunRequest, meta OperationMeta) (*AgentRun, *Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.state.AgentRuns[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	agent, ok := s.state.Agents[run.AgentID]
	if !ok {
		return nil, nil, ErrNotFound
	}

	now := time.Now().UTC()
	if req.Status != "" {
		run.Status = strings.TrimSpace(req.Status)
	}
	if req.DesiredStatus != "" {
		run.DesiredStatus = strings.TrimSpace(req.DesiredStatus)
	}
	if req.SessionID != "" {
		run.SessionID = strings.TrimSpace(req.SessionID)
	}
	if req.ProcessID != 0 {
		run.ProcessID = req.ProcessID
	}
	if req.LastHeartbeatAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, req.LastHeartbeatAt); err == nil {
			run.LastHeartbeatAt = parsed
		}
	}
	if req.LastMessage != "" {
		run.LastMessage = req.LastMessage
	}
	if req.LogTail != nil {
		run.LogTail = append([]string(nil), req.LogTail...)
	}
	if req.Error != "" || (req.Status == "running" || req.Status == "completed") {
		run.Error = req.Error
	}
	if req.ExitCode != nil {
		run.ExitCode = *req.ExitCode
		run.CompletedAt = now
	}
	if run.Status == "running" && run.LaunchTime.IsZero() {
		run.LaunchTime = now
	}
	if run.Status == "completed" || run.Status == "failed" || run.Status == "stopped" {
		if run.CompletedAt.IsZero() {
			run.CompletedAt = now
		}
		if run.Status != "running" && run.DesiredStatus == "running" {
			run.DesiredStatus = "stopped"
		}
	}
	run.UpdatedAt = now

	agent.Status = run.Status
	agent.CurrentRunID = run.ID
	agent.CurrentTask = summarizePrompt(run.Prompt)
	agent.CurrentActivity = describeAgentRunStatus(run)
	agent.UpdatedAt = now
	if run.SessionID != "" {
		agent.SessionID = run.SessionID
	}
	if !run.LastHeartbeatAt.IsZero() {
		agent.LastHeartbeatAt = run.LastHeartbeatAt
	}
	if !run.CompletedAt.IsZero() {
		agent.LastRunCompleted = run.CompletedAt
	}

	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.run.updated",
		ActorID:    agent.ID,
		ActorType:  "agent",
		Summary:    fmt.Sprintf("%s is %s", agent.Name, run.Status),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, nil, err
	}
	return cloneAgentRun(run), cloneAgent(agent), nil
}

func (s *Store) StopAgentRun(id string, meta OperationMeta) (*AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.state.AgentRuns[id]
	if !ok {
		return nil, ErrNotFound
	}
	run.DesiredStatus = "stopped"
	run.UpdatedAt = time.Now().UTC()
	if agent := s.state.Agents[run.AgentID]; agent != nil {
		agent.Status = "stopping"
		agent.CurrentActivity = "Waiting for daemon stop"
		agent.UpdatedAt = run.UpdatedAt
	}
	s.state.UpdatedAt = run.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.run.stop_requested",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s requested stop for %s", meta.ActorID, run.AgentName),
		OccurredAt: run.UpdatedAt,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAgentRun(run), nil
}

func (s *Store) UpdateAgentSession(id string, req UpdateAgentSessionRequest, meta OperationMeta) (*Agent, error) {
	s.mu.Lock()

	agent, ok := s.state.Agents[strings.TrimSpace(id)]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	if status := strings.TrimSpace(req.Status); status != "" {
		switch status {
		case "idle", "working", "disconnected":
			agent.Status = status
		default:
			s.mu.Unlock()
			return nil, fmt.Errorf("unsupported agent status %q", status)
		}
	}
	if threadID := strings.TrimSpace(req.CodexThreadID); threadID != "" {
		agent.CodexThreadID = threadID
		agent.SessionID = threadID
	}
	if turnID := strings.TrimSpace(req.CurrentTurnID); turnID != "" || agent.Status != "working" {
		agent.CurrentTurnID = turnID
		agent.CurrentRunID = turnID
	}
	if activity := strings.TrimSpace(req.CurrentActivity); activity != "" {
		agent.CurrentActivity = activity
	} else {
		switch agent.Status {
		case "idle":
			agent.CurrentActivity = "Idle"
		case "working":
			agent.CurrentActivity = "Working"
		case "disconnected":
			agent.CurrentActivity = "Disconnected"
		}
	}
	if heartbeat := strings.TrimSpace(req.LastHeartbeatAt); heartbeat != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, heartbeat); err == nil {
			agent.LastHeartbeatAt = parsed
		}
	} else {
		agent.LastHeartbeatAt = now
	}
	agent.UpdatedAt = now
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.session.updated",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s session is %s", agent.Name, agent.Status),
		OccurredAt: now,
		Provenance: meta,
	})
	if s.db != nil {
		workspaceID := s.state.WorkspaceID
		updated := cloneAgent(agent)
		s.mu.Unlock()
		if err := upsertAgentPostgres(s.db, workspaceID, updated); err != nil {
			return nil, err
		}
		return updated, nil
	}
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	updated := cloneAgent(agent)
	s.mu.Unlock()
	return updated, nil
}

func (s *Store) ApplyCRDTUpdate(documentID string, update []byte, meta OperationMeta) (*Document, error) {
	result, err := s.ApplyCRDTUpdateWithResult(documentID, update, meta)
	if err != nil {
		return nil, err
	}
	return result.Document, nil
}

func (s *Store) ApplyCRDTUpdateWithResult(documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.Documents[documentID]
	if !ok {
		return nil, ErrNotFound
	}
	if s.db != nil {
		now := time.Now().UTC()
		s.markDocumentDirtyLocked(document.ID)
		s.appendIncrementalDocumentUpdateLocked(document.ID, update, meta, now)
		document.Doc = nil
		document.Content = ""
		document.CRDTState = ""
		document.StateVector = ""
		document.UpdatedAt = now
		s.state.UpdatedAt = now
		if err := s.persistDocumentMutationLocked(); err != nil {
			return nil, err
		}
		return &ApplyCRDTUpdateResult{
			Document: cloneDocument(document),
			Applied:  true,
		}, nil
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	defer documentLock.Unlock()
	beforeSnapshot := crdt.CaptureSnapshot(document.Doc)
	if err := crdt.ApplyUpdateV1(document.Doc, update, meta); err != nil {
		return nil, err
	}
	afterSnapshot := crdt.CaptureSnapshot(document.Doc)
	if crdt.EqualSnapshots(beforeSnapshot, afterSnapshot) {
		return &ApplyCRDTUpdateResult{
			Document: cloneDocument(document),
			Applied:  false,
		}, nil
	}
	afterStateVector := crdt.EncodeStateVectorV1(document.Doc)
	now := time.Now().UTC()
	document.StateVector = base64.StdEncoding.EncodeToString(afterStateVector)
	if s.db == nil {
		document.Content = document.Doc.GetText("content").ToString()
	}
	s.markDocumentDirtyLocked(document.ID)
	s.appendIncrementalDocumentUpdateLocked(document.ID, update, meta, now)
	document.UpdatedAt = now
	s.state.UpdatedAt = document.UpdatedAt

	if err := s.persistDocumentMutationLocked(); err != nil {
		return nil, err
	}
	return &ApplyCRDTUpdateResult{
		Document: cloneDocument(document),
		Applied:  true,
	}, nil
}

func (s *Store) ReplaceDocumentText(documentID string, nextText string, meta OperationMeta) (*Document, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.Documents[documentID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	defer documentLock.Unlock()
	if s.db != nil && document.Doc == nil {
		doc, err := s.restoreDocumentDocPostgresLocked(document)
		if err != nil {
			return nil, nil, err
		}
		document.Doc = doc
		document.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	}
	currentText := document.Doc.GetText("content").ToString()
	text := document.Doc.GetText("content")

	var update []byte
	unsubscribe := document.Doc.OnUpdate(func(nextUpdate []byte, origin any) {
		if origin == meta {
			update = append([]byte(nil), nextUpdate...)
		}
	})
	document.Doc.Transact(func(txn *crdt.Transaction) {
		if len(currentText) > 0 {
			text.Delete(txn, 0, len(currentText))
		}
		if nextText != "" {
			text.Insert(txn, 0, nextText, nil)
		}
	}, meta)
	unsubscribe()

	if s.db != nil {
		document.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(document.Doc))
	} else {
		document.SyncProjectionFields()
	}
	s.markDocumentDirtyLocked(document.ID)
	s.appendIncrementalDocumentUpdateLocked(document.ID, update, meta, time.Now().UTC())
	notify := !isLogDocumentPath(document.Path)
	document.UpdatedAt = time.Now().UTC()
	if notify {
		s.enqueueDocumentThreadEventsLocked(document, meta)
	}
	s.state.UpdatedAt = document.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "document.updated",
		DocumentID: document.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s replaced %s", meta.ActorID, document.Path),
		OccurredAt: document.UpdatedAt,
		Provenance: meta,
	})

	if err := s.persistDocumentMutationLocked(); err != nil {
		return nil, nil, err
	}
	return cloneDocumentWithCRDTState(document), update, nil
}

func (s *Store) CreateThread(req CreateThreadRequest, meta OperationMeta) (*Thread, *ThreadMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.Documents[req.DocumentID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, nil, errors.New("thread body is required")
	}
	author, err := s.resolvePrincipalLocked(meta.ActorID, meta.ActorType)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	anchor, err := buildThreadAnchorFromRequest(document, req)
	if err != nil {
		return nil, nil, err
	}
	thread := &Thread{
		ID:              "thread_" + uuid.NewString(),
		DocumentID:      document.ID,
		Title:           firstNonEmptyString(strings.TrimSpace(req.Title), inferThreadTitleFromRequest(document, req)),
		Status:          "open",
		Anchor:          anchor,
		CreatedByID:     author.ID,
		CreatedByType:   author.Type,
		CreatedByHandle: author.Handle,
		CreatedByName:   author.Name,
		ParticipantIDs:  []string{author.ID},
		Messages:        []*ThreadMessage{},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	message := &ThreadMessage{
		ID:           "threadmsg_" + uuid.NewString(),
		ThreadID:     thread.ID,
		AuthorID:     author.ID,
		AuthorType:   author.Type,
		AuthorHandle: author.Handle,
		AuthorName:   author.Name,
		Body:         body,
		Kind:         "comment",
		CreatedAt:    now,
	}
	thread.Messages = append(thread.Messages, message)
	s.mergeThreadParticipantsLocked(thread, author.ID)
	s.mergeThreadParticipantsLocked(thread, s.extractMentionPrincipalIDsLocked(body)...)
	s.state.Threads[thread.ID] = thread
	s.refreshThreadParticipantsLocked()
	s.enqueueThreadMentionEventsLocked(thread, message, meta)
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "thread.created",
		DocumentID: document.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s started a thread on %s", meta.ActorID, document.Path),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, nil, err
	}
	return cloneThread(thread), cloneThreadMessage(message), nil
}

func (s *Store) ReplyThread(id string, req ReplyThreadRequest, meta OperationMeta) (*Thread, *ThreadMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	thread, ok := s.state.Threads[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, nil, errors.New("thread reply is required")
	}
	author, err := s.resolvePrincipalLocked(meta.ActorID, meta.ActorType)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "comment"
	}
	message := &ThreadMessage{
		ID:           "threadmsg_" + uuid.NewString(),
		ThreadID:     thread.ID,
		AuthorID:     author.ID,
		AuthorType:   author.Type,
		AuthorHandle: author.Handle,
		AuthorName:   author.Name,
		Body:         body,
		Kind:         kind,
		CreatedAt:    now,
	}
	thread.Messages = append(thread.Messages, message)
	thread.UpdatedAt = now
	mentionedIDs := s.extractMentionPrincipalIDsLocked(body)
	s.mergeThreadParticipantsLocked(thread, author.ID)
	s.mergeThreadParticipantsLocked(thread, mentionedIDs...)
	s.refreshThreadParticipantsLocked()
	s.enqueueThreadMentionEventsLocked(thread, message, meta)
	s.enqueueThreadReplyEventsLocked(thread, message, meta, mentionedIDs...)
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "thread.replied",
		DocumentID: thread.DocumentID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s replied in thread %s", meta.ActorID, thread.Title),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, nil, err
	}
	return cloneThread(thread), cloneThreadMessage(message), nil
}

func (s *Store) ClaimAgentEvent(req ClaimAgentEventRequest) (*AgentEvent, error) {
	s.mu.Lock()

	if s.db != nil {
		workspaceID := s.state.WorkspaceID
		agentID, agentHandle, err := s.resolveAgentIdentityLocked(req.AgentID)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		claimedBy := stringsOr(req.ClaimedBy, "daemon")
		s.mu.Unlock()
		event, err := claimAgentEventPostgres(s.db, workspaceID, agentID, agentHandle, claimedBy)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.state.AgentEvents[event.ID] = cloneAgentEvent(event)
		s.mu.Unlock()
		return event, nil
	}
	defer s.mu.Unlock()

	agentID, agentHandle, err := s.resolveAgentIdentityLocked(req.AgentID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var next *AgentEvent
	for _, event := range s.state.AgentEvents {
		if event.AgentID != agentID {
			continue
		}
		if s.eventBelongsToLogDocumentLocked(event) {
			continue
		}
		if event.AvailableAt.After(now) {
			continue
		}
		if event.Status == "completed" || event.Status == "dismissed" {
			continue
		}
		if event.Status == "processing" && now.Sub(event.ClaimedAt) < 30*time.Second {
			continue
		}
		if next == nil || event.CreatedAt.Before(next.CreatedAt) {
			next = event
		}
	}
	if next == nil {
		return nil, ErrNotFound
	}
	next.Status = "processing"
	next.AgentHandle = agentHandle
	next.ClaimedBy = strings.TrimSpace(req.ClaimedBy)
	if next.ClaimedBy == "" {
		next.ClaimedBy = "daemon"
	}
	next.ClaimedAt = now
	next.AttemptCount++
	next.UpdatedAt = now
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAgentEvent(next), nil
}

func (s *Store) UpdateAgentEvent(id string, req UpdateAgentEventRequest, meta OperationMeta) (*AgentEvent, error) {
	s.mu.Lock()

	if s.db != nil {
		workspaceID := s.state.WorkspaceID
		if _, ok := s.state.AgentEvents[id]; !ok {
			s.mu.Unlock()
			return nil, ErrNotFound
		}
		s.mu.Unlock()
		updated, err := updateAgentEventPostgres(s.db, workspaceID, id, req)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.state.AgentEvents[id] = cloneAgentEvent(updated)
		s.state.UpdatedAt = updated.UpdatedAt
		s.appendActivityLocked(&ActivityEvent{
			Type:       "agent.event.updated",
			DocumentID: updated.DocumentID,
			ActorID:    meta.ActorID,
			ActorType:  meta.ActorType,
			Summary:    fmt.Sprintf("%s marked %s %s", meta.ActorID, updated.Type, updated.Status),
			OccurredAt: updated.UpdatedAt,
			Provenance: meta,
		})
		s.mu.Unlock()
		return updated, nil
	}
	defer s.mu.Unlock()

	event, ok := s.state.AgentEvents[id]
	if !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	if status := strings.TrimSpace(req.Status); status != "" {
		event.Status = status
	}
	if threadID := strings.TrimSpace(req.ThreadID); threadID != "" {
		event.ThreadID = threadID
	}
	if runID := strings.TrimSpace(req.RunID); runID != "" {
		event.RunID = runID
	}
	if lastError := strings.TrimSpace(req.LastError); lastError != "" {
		event.LastError = lastError
	}
	if event.Status == "completed" {
		event.CompletedAt = now
	}
	if event.Status == "pending" && event.AvailableAt.Before(now) {
		event.AvailableAt = now.Add(5 * time.Second)
	}
	event.UpdatedAt = now
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "agent.event.updated",
		DocumentID: event.DocumentID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s marked %s %s", meta.ActorID, event.Type, event.Status),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAgentEvent(event), nil
}

func (s *Store) UpsertPresence(req UpsertPresenceRequest) (*Presence, error) {
	s.mu.Lock()

	now := time.Now().UTC()
	presence := &Presence{
		ActorID:    req.ActorID,
		ActorType:  req.ActorType,
		DocumentID: req.DocumentID,
		FilePath:   req.FilePath,
		Mode:       req.Mode,
		Selection:  append([]int(nil), req.Selection...),
		Activity:   req.Activity,
		UpdatedAt:  now,
	}
	s.state.Presences[req.ActorID] = presence
	s.state.UpdatedAt = now
	if s.db != nil {
		workspaceID := s.state.WorkspaceID
		clone := *presence
		clone.Selection = append([]int(nil), presence.Selection...)
		s.mu.Unlock()
		if err := upsertPresencePostgres(s.db, workspaceID, &clone); err != nil {
			return nil, err
		}
		return &clone, nil
	} else {
		if err := s.persistLocked(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	clone := *presence
	clone.Selection = append([]int(nil), presence.Selection...)
	s.mu.Unlock()
	return &clone, nil
}

func (s *Store) persistLocked() error {
	s.ensureMaps()
	if s.db != nil {
		return s.persistPostgresLocked()
	}
	if err := os.MkdirAll(filepath.Dir(s.dataFile), 0o755); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.dataFile, bytes, 0o644)
}

func (s *Store) persistDocumentMutationLocked() error {
	s.ensureMaps()
	if s.db != nil {
		return s.persistDocumentMutationPostgresLocked()
	}
	return s.persistLocked()
}

func (s *Store) nextClientIDSeedLocked() uint64 {
	var next uint64 = 1001
	for _, document := range s.state.Documents {
		if document.ClientIDSeed >= next {
			next = document.ClientIDSeed + 1
		}
	}
	return next
}

func (s *Store) documentExistsAtPathLocked(path string, exceptID string) bool {
	for id, document := range s.state.Documents {
		if id == exceptID {
			continue
		}
		if document.Path == path {
			return true
		}
	}
	return false
}

func (s *Store) appendActivityLocked(event *ActivityEvent) {
	s.state.Activities = append([]*ActivityEvent{event}, s.state.Activities...)
	if len(s.state.Activities) > 100 {
		s.state.Activities = s.state.Activities[:100]
	}
}

func cloneState(state WorkspaceState) WorkspaceState {
	copyState := state
	copyState.Documents = map[string]*Document{}
	for key, doc := range state.Documents {
		copyState.Documents[key] = cloneDocument(doc)
	}
	copyState.Users = map[string]*User{}
	for key, user := range state.Users {
		copyState.Users[key] = cloneUser(user)
	}
	copyState.Agents = map[string]*Agent{}
	for key, agent := range state.Agents {
		copyState.Agents[key] = cloneAgent(agent)
	}
	copyState.AgentRuns = map[string]*AgentRun{}
	for key, run := range state.AgentRuns {
		copyState.AgentRuns[key] = cloneAgentRun(run)
	}
	copyState.Threads = map[string]*Thread{}
	for key, thread := range state.Threads {
		copyState.Threads[key] = cloneThread(thread)
	}
	copyState.AgentEvents = map[string]*AgentEvent{}
	for key, event := range state.AgentEvents {
		copyState.AgentEvents[key] = cloneAgentEvent(event)
	}
	copyState.AgentDocumentViews = map[string]*AgentDocumentView{}
	for key, view := range state.AgentDocumentViews {
		copyState.AgentDocumentViews[key] = cloneAgentDocumentView(view)
	}
	copyState.DocumentCheckpoints = map[string]*DocumentCheckpoint{}
	for key, checkpoint := range state.DocumentCheckpoints {
		if checkpoint == nil {
			continue
		}
		clone := *checkpoint
		copyState.DocumentCheckpoints[key] = &clone
	}
	copyState.Presences = map[string]*Presence{}
	for key, presence := range state.Presences {
		clone := *presence
		clone.Selection = append([]int(nil), presence.Selection...)
		copyState.Presences[key] = &clone
	}
	copyState.Activities = make([]*ActivityEvent, len(state.Activities))
	for index, activity := range state.Activities {
		clone := *activity
		copyState.Activities[index] = &clone
	}
	return copyState
}

func (s *Store) markDocumentDirtyLocked(documentID string) {
	if documentID == "" {
		return
	}
	if s.dirtyDocuments == nil {
		s.dirtyDocuments = map[string]struct{}{}
	}
	delete(s.deletedDocuments, documentID)
	s.dirtyDocuments[documentID] = struct{}{}
}

func (s *Store) markDocumentDeletedLocked(documentID string) {
	if documentID == "" {
		return
	}
	if s.deletedDocuments == nil {
		s.deletedDocuments = map[string]struct{}{}
	}
	delete(s.dirtyDocuments, documentID)
	s.deletedDocuments[documentID] = struct{}{}
}

func (s *Store) appendIncrementalDocumentUpdateLocked(documentID string, update []byte, meta OperationMeta, now time.Time) {
	if len(update) == 0 || documentID == "" {
		return
	}
	if s.db == nil {
		if document := s.state.Documents[documentID]; document != nil {
			document.UpdateID++
			document.SyncDerivedFields()
			s.recordDocumentCheckpointLocked(document, now)
		}
	}
	s.pendingDocumentEvents = append(s.pendingDocumentEvents, documentUpdateRecord{
		DocumentID: documentID,
		Update:     append([]byte(nil), update...),
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		CreatedAt:  now,
	})
}

func (s *Store) appendFullDocumentUpdateLocked(document *Document, meta OperationMeta, now time.Time) {
	if document == nil || document.Doc == nil {
		return
	}
	s.appendIncrementalDocumentUpdateLocked(document.ID, document.Doc.EncodeStateAsUpdate(), meta, now)
}

func (s *Store) recordDocumentCheckpointLocked(document *Document, now time.Time) {
	if document == nil || document.UpdateID <= 0 {
		return
	}
	if s.state.DocumentCheckpoints == nil {
		s.state.DocumentCheckpoints = map[string]*DocumentCheckpoint{}
	}
	s.state.DocumentCheckpoints[documentCheckpointKey(document.ID, document.UpdateID)] = &DocumentCheckpoint{
		DocumentID:  document.ID,
		UpdateID:    document.UpdateID,
		CRDTState:   document.CRDTState,
		StateVector: document.StateVector,
		CreatedAt:   now,
	}
}

func SortedAgentRuns(state WorkspaceState) []*AgentRun {
	return sortedAgentRunsWithCloner(state, cloneAgentRun)
}

func SortedWorkspaceAgentRuns(state WorkspaceState) []*AgentRun {
	return sortedAgentRunsWithCloner(state, cloneAgentRunForWorkspace)
}

func SortedSyncAgentRuns(state WorkspaceState) []*AgentRun {
	return sortedAgentRunsWithCloner(state, cloneAgentRunForSync)
}

func sortedAgentRunsWithCloner(state WorkspaceState, clone func(*AgentRun) *AgentRun) []*AgentRun {
	runs := make([]*AgentRun, 0, len(state.AgentRuns))
	for _, run := range state.AgentRuns {
		runs = append(runs, clone(run))
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].UpdatedAt.Equal(runs[j].UpdatedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].UpdatedAt.After(runs[j].UpdatedAt)
	})
	return runs
}

func SortedUsers(state WorkspaceState) []*User {
	users := make([]*User, 0, len(state.Users))
	for _, user := range state.Users {
		users = append(users, cloneUser(user))
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Handle < users[j].Handle })
	return users
}

func SortedAgents(state WorkspaceState) []*Agent {
	agents := make([]*Agent, 0, len(state.Agents))
	for _, agent := range state.Agents {
		agents = append(agents, cloneAgent(agent))
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents
}

func SortedDocuments(state WorkspaceState) []*Document {
	docs := make([]*Document, 0, len(state.Documents))
	for _, doc := range state.Documents {
		docs = append(docs, cloneDocument(doc))
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs
}

func SortedSyncDocuments(state WorkspaceState) []*DocumentMetadata {
	docs := make([]*DocumentMetadata, 0, len(state.Documents))
	for _, doc := range state.Documents {
		docs = append(docs, documentMetadata(doc))
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs
}

func isPostgresDSN(value string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://")
}

func initPostgresSchema(db *sql.DB) error {
	return initPostgresSchemaTables(db)
}

func normalizeDocumentPath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("path is required")
	}
	cleaned := filepath.ToSlash(filepath.Clean(trimmed))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", errors.New("path must stay within workspace")
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", errors.New("path must be relative")
	}
	return cleaned, nil
}

func isLogDocumentPath(path string) bool {
	return strings.EqualFold(filepath.Ext(strings.TrimSpace(path)), ".log")
}

func titleFromPath(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.TrimSpace(name)
	if name == "" {
		return "Untitled"
	}
	parts := strings.Fields(name)
	for index, part := range parts {
		runes := []rune(strings.ToLower(part))
		runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		parts[index] = string(runes)
	}
	return strings.Join(parts, " ")
}

func buildUser(name, handle, role string) (*User, error) {
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return nil, errors.New("user name is required")
	}
	normalizedHandle, err := normalizeHandle(handle)
	if err != nil {
		return nil, err
	}
	trimmedRole := strings.TrimSpace(role)
	if trimmedRole == "" {
		trimmedRole = "Collaborates in the shared workspace"
	}
	return &User{
		Handle: normalizedHandle,
		Name:   trimmedName,
		Role:   trimmedRole,
		Kind:   "human",
		Status: "active",
	}, nil
}

func buildAgent(handle, name, role, kind string) (*Agent, error) {
	trimmedKind := strings.TrimSpace(strings.ToLower(kind))
	if trimmedKind == "" {
		trimmedKind = "codex"
	}
	if trimmedKind != "codex" {
		return nil, fmt.Errorf("unsupported agent kind %q", trimmedKind)
	}
	normalizedHandle, err := normalizeHandle(handle)
	if err != nil {
		return nil, err
	}
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return nil, errors.New("agent name is required")
	}
	trimmedRole := strings.TrimSpace(role)
	if trimmedRole == "" {
		trimmedRole = "Execute tasks inside the mounted workspace"
	}
	agent := &Agent{
		Handle: normalizedHandle,
		Name:   trimmedName,
		Role:   trimmedRole,
		Kind:   trimmedKind,
		Status: "idle",
	}
	agent.SystemPrompt = sharedAgentSystemPrompt(agent)
	return agent, nil
}

func sharedAgentSystemPrompt(agent *Agent) string {
	if agent == nil {
		return ""
	}
	role := strings.TrimSpace(agent.Role)
	if role == "" {
		role = "Execute tasks inside the mounted workspace"
	}
	name := strings.TrimSpace(agent.Name)
	handle := strings.TrimSpace(agent.Handle)
	kind := strings.TrimSpace(agent.Kind)
	if kind == "" {
		kind = "codex"
	}
	return strings.TrimSpace(fmt.Sprintf(`You are %s (@%s), a persistent %s agent collaborator inside notty.
Your role in this workspace is: %s.
You work from your own dedicated workspace copy, and the backend's canonical workspace is the source of truth.
Your file changes sync to other peers through the shared workspace promptly, so be careful with file operations.
Prefer direct edits to existing files when possible. Avoid delete-and-recreate or broad filesystem churn unless that exact operation is clearly intended.
You may be notified by direct thread mentions, document edits, thread messages, or an inbox check.
Plain @handle text inside markdown documents is regular document text, not a notification; use document threads when you want to mention a collaborator.
Your inbox has two classes: for-me items are specific to you and should be reviewed first; general items are workspace activity and may not require action unless you have specific opinions, questions, or useful edits.
Document update inbox items are deduplicated; use the diff-document tool to compare your last viewed CRDT update version with the current head, and mark documents viewed after review.
Use notty-agent-tool list-inbox --box for-me and notty-agent-tool list-inbox --box general to inspect notification center items. Use get-inbox-item, complete-inbox-item, dismiss-inbox-item, diff-document, and mark-document-viewed when needed.
Use notty-agent-tool list-documents, get-document-by-path, get-thread, and list-threads-for-document to gather context before acting.
Create document threads with simple anchors: notty-agent-tool create-thread --path <file> --line <line> --body "..." or add --quote "exact text" for a precise anchor. Use --document for document-level threads.
You do not need to reply by default. If there is nothing worth replying to, and you have no disagreement, question, or constructive feedback, you may stay silent and only make useful workspace changes.
If you decide to communicate, you must use the provided thread tools instead of writing conversational replies into documents.
If you have comments about a specific part of a document, reply in the existing thread anchored there or create a new thread anchored to that document range.
If you want help or input from other collaborators, mention them in the thread with their @handle.
Respect other collaborators because this is a shared workspace.
If you have doubts or are uncertain about a change, it is often better to ask for others' input in a thread before making the change.
It is important to consult others' opinions before making edits, and preferably have everyone aligned in a thread before making substantial changes.
Whenever possible, reuse an existing thread instead of opening a new one if the existing thread is already well aligned with the topic.
If you are directly mentioned in a thread, you must reply with the thread tools.
Keep edits bounded, relevant to your role, and grounded in the current document and thread context.`, name, handle, kind, role))
}

func (s *Store) refreshAgentSystemPromptsLocked() bool {
	changed := false
	for _, agent := range s.state.Agents {
		next := sharedAgentSystemPrompt(agent)
		if agent.SystemPrompt != next {
			agent.SystemPrompt = next
			changed = true
		}
	}
	return changed
}

func normalizeHandle(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return "", errors.New("handle is required")
	}
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return "", errors.New("handle may only use lowercase letters, numbers, hyphen, or underscore")
	}
	if len(trimmed) < 2 || len(trimmed) > 32 {
		return "", errors.New("handle must be between 2 and 32 characters")
	}
	return trimmed, nil
}

func normalizeWorkspaceRelativePath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." {
		return ".", nil
	}
	return normalizeDocumentPath(trimmed)
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "completed", "failed", "stopped":
		return true
	default:
		return false
	}
}

func cloneAgent(agent *Agent) *Agent {
	if agent == nil {
		return nil
	}
	clone := *agent
	return &clone
}

func cloneUser(user *User) *User {
	if user == nil {
		return nil
	}
	clone := *user
	return &clone
}

func (s *Store) ensureHandleAvailableLocked(handle, exceptUserID, exceptAgentID string) error {
	for id, user := range s.state.Users {
		if id == exceptUserID {
			continue
		}
		if user.Handle == handle {
			return fmt.Errorf("handle @%s is already in use", handle)
		}
	}
	for id, agent := range s.state.Agents {
		if id == exceptAgentID {
			continue
		}
		if agent.Handle == handle {
			return fmt.Errorf("handle @%s is already in use", handle)
		}
	}
	return nil
}

func (s *Store) principalByHandleLocked(handle string) (*principalRef, bool) {
	for _, user := range s.state.Users {
		if user.Handle == handle {
			return &principalRef{UserID: user.ID, Handle: user.Handle, Name: user.Name, Kind: user.Kind}, true
		}
	}
	for _, agent := range s.state.Agents {
		if agent.Handle == handle {
			return &principalRef{UserID: agent.ID, Handle: agent.Handle, Name: agent.Name, Kind: "agent"}, true
		}
	}
	return nil, false
}

func (s *Store) documentHasThreadsLocked(documentID string) bool {
	if documentID == "" {
		return false
	}
	for _, thread := range s.state.Threads {
		if thread != nil && thread.DocumentID == documentID {
			return true
		}
	}
	return false
}

func cloneAgentRun(run *AgentRun) *AgentRun {
	if run == nil {
		return nil
	}
	clone := *run
	clone.LogTail = append([]string(nil), run.LogTail...)
	return &clone
}

func cloneAgentRunForWorkspace(run *AgentRun) *AgentRun {
	clone := cloneAgentRun(run)
	slimAgentRunPayload(clone)
	return clone
}

func cloneAgentRunForSync(run *AgentRun) *AgentRun {
	clone := cloneAgentRun(run)
	if isTerminalRunStatus(clone.Status) {
		slimAgentRunPayload(clone)
	}
	return clone
}

func slimAgentRunPayload(run *AgentRun) {
	if run == nil {
		return
	}
	run.SystemPrompt = ""
	run.Prompt = summarizePrompt(run.Prompt)
	run.LogTail = summarizeLogTail(run.LogTail)
	run.Error = truncateString(run.Error, agentRunErrorLimit)
}

func summarizePrompt(prompt string) string {
	trimmed := strings.Join(strings.Fields(strings.TrimSpace(prompt)), " ")
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > 72 {
		return trimmed[:72] + "..."
	}
	return trimmed
}

func summarizeLogTail(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	start := len(lines) - agentRunLogPreviewLines
	if start < 0 {
		start = 0
	}
	summary := make([]string, 0, len(lines)-start)
	for _, line := range lines[start:] {
		summary = append(summary, truncateString(line, agentRunLogLineLimit))
	}
	return summary
}

func truncateString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func describeAgentRunStatus(run *AgentRun) string {
	switch run.Status {
	case "queued":
		return "Queued in daemon"
	case "running":
		if run.LastMessage != "" {
			return run.LastMessage
		}
		return "Running Codex"
	case "completed":
		if run.LastMessage != "" {
			return run.LastMessage
		}
		return "Completed"
	case "failed":
		if run.Error != "" {
			return run.Error
		}
		return "Failed"
	case "stopping":
		return "Stopping"
	case "stopped":
		return "Stopped"
	default:
		return run.Status
	}
}

type principalIdentity struct {
	ID     string
	Type   string
	Handle string
	Name   string
}

func SortedThreads(state WorkspaceState) []*Thread {
	threads := make([]*Thread, 0, len(state.Threads))
	for _, thread := range state.Threads {
		threads = append(threads, cloneThread(thread))
	}
	sort.Slice(threads, func(i, j int) bool {
		if threads[i].UpdatedAt.Equal(threads[j].UpdatedAt) {
			return threads[i].ID < threads[j].ID
		}
		return threads[i].UpdatedAt.After(threads[j].UpdatedAt)
	})
	return threads
}

func SortedAgentEvents(state WorkspaceState) []*AgentEvent {
	events := make([]*AgentEvent, 0, len(state.AgentEvents))
	for _, event := range state.AgentEvents {
		if event != nil && event.DocumentID != "" && strings.HasPrefix(event.Type, "document.") {
			if document := state.Documents[event.DocumentID]; document != nil && isLogDocumentPath(document.Path) {
				continue
			}
		}
		events = append(events, cloneAgentEvent(event))
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
	return events
}

func cloneThread(thread *Thread) *Thread {
	if thread == nil {
		return nil
	}
	clone := *thread
	clone.ParticipantIDs = append([]string(nil), thread.ParticipantIDs...)
	clone.ParticipantHandles = append([]string(nil), thread.ParticipantHandles...)
	clone.Messages = make([]*ThreadMessage, len(thread.Messages))
	for index, message := range thread.Messages {
		clone.Messages[index] = cloneThreadMessage(message)
	}
	return &clone
}

func cloneThreadMessage(message *ThreadMessage) *ThreadMessage {
	if message == nil {
		return nil
	}
	clone := *message
	return &clone
}

func cloneAgentEvent(event *AgentEvent) *AgentEvent {
	if event == nil {
		return nil
	}
	clone := *event
	return &clone
}

func (s *Store) refreshThreadParticipantsLocked() {
	for _, thread := range s.state.Threads {
		handles := make([]string, 0, len(thread.ParticipantIDs))
		for _, participantID := range thread.ParticipantIDs {
			if user, ok := s.state.Users[participantID]; ok {
				handles = append(handles, user.Handle)
				continue
			}
			if agent, ok := s.state.Agents[participantID]; ok {
				handles = append(handles, agent.Handle)
			}
		}
		sort.Strings(handles)
		thread.ParticipantHandles = handles
	}
}

func (s *Store) mergeThreadParticipantsLocked(thread *Thread, participantIDs ...string) {
	if thread == nil {
		return
	}
	for _, participantID := range participantIDs {
		if participantID == "" {
			continue
		}
		if !containsText(thread.ParticipantIDs, participantID) {
			thread.ParticipantIDs = append(thread.ParticipantIDs, participantID)
		}
	}
	sort.Strings(thread.ParticipantIDs)
}

func buildThreadAnchorFromRequest(document *Document, req CreateThreadRequest) (ThreadAnchor, error) {
	relativeStart := strings.TrimSpace(req.RelativeStart)
	relativeEnd := strings.TrimSpace(req.RelativeEnd)
	if (relativeStart == "") != (relativeEnd == "") {
		return ThreadAnchor{}, errors.New("relativeStart and relativeEnd must be provided together")
	}
	start := maxInt(0, req.Start)
	end := maxInt(start, req.End)
	line := maxInt(1, req.Line)
	excerpt := truncateText(strings.TrimSpace(req.Excerpt), 140)
	if relativeStart == "" && relativeEnd == "" {
		if start == 0 && end == 0 && req.Line == 0 && excerpt == "" {
			return ThreadAnchor{
				DocumentID: document.ID,
				Kind:       "document",
				Line:       1,
			}, nil
		}
		return ThreadAnchor{}, errors.New("text-range threads require relativeStart and relativeEnd")
	}
	anchor := ThreadAnchor{
		DocumentID: document.ID,
		Kind:       "text-range",
		Start:      start,
		End:        end,
		Line:       line,
		Excerpt:    excerpt,
	}
	anchor.RelativeStart = relativeStart
	anchor.RelativeEnd = relativeEnd
	return anchor, nil
}

func inferThreadTitleFromRequest(document *Document, req CreateThreadRequest) string {
	if excerpt := strings.TrimSpace(req.Excerpt); excerpt != "" {
		return truncateText(excerpt, 72)
	}
	if document != nil {
		return fmt.Sprintf("Discussion on %s", document.Title)
	}
	return "Discussion"
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func removeString(items []string, target string) []string {
	filtered := items[:0]
	for _, item := range items {
		if item != target {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Store) resolvePrincipalLocked(actorID, actorType string) (*principalIdentity, error) {
	trimmedID := strings.TrimSpace(actorID)
	switch strings.TrimSpace(actorType) {
	case "human":
		for _, user := range s.state.Users {
			if user.ID == trimmedID || user.Handle == trimmedID {
				return &principalIdentity{ID: user.ID, Type: "human", Handle: user.Handle, Name: user.Name}, nil
			}
		}
	case "agent":
		for _, agent := range s.state.Agents {
			if agent.ID == trimmedID || agent.Handle == trimmedID {
				return &principalIdentity{ID: agent.ID, Type: "agent", Handle: agent.Handle, Name: agent.Name}, nil
			}
		}
	}
	for _, user := range s.state.Users {
		if user.ID == trimmedID || user.Handle == trimmedID {
			return &principalIdentity{ID: user.ID, Type: "human", Handle: user.Handle, Name: user.Name}, nil
		}
	}
	for _, agent := range s.state.Agents {
		if agent.ID == trimmedID || agent.Handle == trimmedID {
			return &principalIdentity{ID: agent.ID, Type: "agent", Handle: agent.Handle, Name: agent.Name}, nil
		}
	}
	return nil, fmt.Errorf("unknown principal %q", actorID)
}

func (s *Store) resolveAgentIdentityLocked(agentRef string) (string, string, error) {
	trimmed := strings.TrimSpace(agentRef)
	for _, agent := range s.state.Agents {
		if agent.ID == trimmed || agent.Handle == trimmed {
			return agent.ID, agent.Handle, nil
		}
	}
	return "", "", ErrNotFound
}

func (s *Store) extractMentionPrincipalIDsLocked(content string) []string {
	matches := mentionPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		handle := content[match[4]:match[5]]
		principal, ok := s.principalByHandleLocked(handle)
		if !ok {
			continue
		}
		if !containsText(ids, principal.UserID) {
			ids = append(ids, principal.UserID)
		}
	}
	return ids
}

func (s *Store) enqueueDocumentThreadEventsLocked(document *Document, meta OperationMeta) {
	if document == nil {
		return
	}
	if isLogDocumentPath(document.Path) {
		return
	}
	for _, thread := range s.state.Threads {
		if thread.DocumentID != document.ID || thread.Status != "open" {
			continue
		}
		for _, participantID := range thread.ParticipantIDs {
			agent, ok := s.state.Agents[participantID]
			if !ok {
				continue
			}
			dedupKey := fmt.Sprintf("document-edited:%s:%s:%d", document.ID, participantID, document.UpdatedAt.UnixNano())
			s.enqueueAgentNotificationLocked(agent.ID, agent.Handle, "document.edited", dedupKey, meta, "", func(event *AgentEvent, now time.Time) {
				event.DocumentID = document.ID
				event.ThreadID = thread.ID
				event.AnchorStart = thread.Anchor.Start
				event.AnchorEnd = thread.Anchor.End
				event.Summary = fmt.Sprintf("%s changed near thread %s", document.Path, thread.Title)
				event.Prompt = fmt.Sprintf("Document %s changed near thread %q. Review the latest edits and continue the discussion if needed.", document.Path, thread.Title)
			})
		}
	}
}

func (s *Store) enqueueThreadMentionEventsLocked(thread *Thread, message *ThreadMessage, meta OperationMeta) {
	if thread == nil || message == nil {
		return
	}
	mentionedIDs := s.extractMentionPrincipalIDsLocked(message.Body)
	for _, principalID := range mentionedIDs {
		agent, ok := s.state.Agents[principalID]
		if !ok {
			continue
		}
		dedupKey := fmt.Sprintf("thread-mentioned:%s:%s", message.ID, agent.ID)
		s.enqueueAgentNotificationLocked(agent.ID, agent.Handle, "thread.mentioned", dedupKey, meta, message.AuthorID, func(event *AgentEvent, now time.Time) {
			event.DocumentID = thread.DocumentID
			event.ThreadID = thread.ID
			event.ThreadMessageID = message.ID
			event.AnchorStart = thread.Anchor.Start
			event.AnchorEnd = thread.Anchor.End
			event.Summary = fmt.Sprintf("@%s was mentioned in thread %s", agent.Handle, thread.Title)
			event.Prompt = fmt.Sprintf("You were mentioned by @%s in thread %q: %s", message.AuthorHandle, thread.Title, truncateText(message.Body, 240))
		})
	}
}

func (s *Store) reconcileThreadMentionEventsLocked() bool {
	before := len(s.state.AgentEvents)
	for _, thread := range s.state.Threads {
		if thread == nil {
			continue
		}
		for _, message := range thread.Messages {
			if message == nil {
				continue
			}
			s.enqueueThreadMentionEventsLocked(thread, message, OperationMeta{
				ActorID:   message.AuthorID,
				ActorType: message.AuthorType,
				Source:    "reconcile",
			})
		}
	}
	return len(s.state.AgentEvents) != before
}

func (s *Store) enqueueThreadReplyEventsLocked(thread *Thread, message *ThreadMessage, meta OperationMeta, skipAgentIDs ...string) {
	if thread == nil || message == nil {
		return
	}
	for _, participantID := range thread.ParticipantIDs {
		if containsText(skipAgentIDs, participantID) {
			continue
		}
		agent, ok := s.state.Agents[participantID]
		if !ok {
			continue
		}
		dedupKey := fmt.Sprintf("thread-replied:%s:%s", message.ID, agent.ID)
		s.enqueueAgentNotificationLocked(agent.ID, agent.Handle, "thread.replied", dedupKey, meta, message.AuthorID, func(event *AgentEvent, now time.Time) {
			event.DocumentID = thread.DocumentID
			event.ThreadID = thread.ID
			event.ThreadMessageID = message.ID
			event.AnchorStart = thread.Anchor.Start
			event.AnchorEnd = thread.Anchor.End
			event.Summary = fmt.Sprintf("New reply in thread %s", thread.Title)
			event.Prompt = fmt.Sprintf("A new reply was added in thread %q by @%s: %s", thread.Title, message.AuthorHandle, truncateText(message.Body, 240))
		})
	}
}

func (s *Store) enqueueAgentNotificationLocked(agentID, agentHandle, eventType, dedupKey string, meta OperationMeta, fallbackActorID string, apply func(event *AgentEvent, now time.Time)) {
	if !s.shouldNotifyAgentLocked(agentID, meta, fallbackActorID) {
		return
	}
	s.ensureAgentEventLocked(agentID, agentHandle, eventType, dedupKey, apply)
}

func (s *Store) shouldNotifyAgentLocked(agentID string, meta OperationMeta, fallbackActorID string) bool {
	originID := strings.TrimSpace(fallbackActorID)
	if actor, err := s.resolvePrincipalLocked(meta.ActorID, meta.ActorType); err == nil {
		originID = actor.ID
	}
	if originID == "" {
		return true
	}
	return originID != agentID
}

func (s *Store) ensureAgentEventLocked(agentID, agentHandle, eventType, dedupKey string, apply func(event *AgentEvent, now time.Time)) {
	for _, current := range s.state.AgentEvents {
		if current.DedupKey == dedupKey {
			return
		}
	}
	now := time.Now().UTC()
	event := &AgentEvent{
		ID:          "aevt_" + uuid.NewString(),
		AgentID:     agentID,
		AgentHandle: agentHandle,
		Type:        eventType,
		Box:         "for_me",
		Status:      "pending",
		DedupKey:    dedupKey,
		CreatedAt:   now,
		UpdatedAt:   now,
		AvailableAt: now,
	}
	if apply != nil {
		apply(event, now)
	}
	s.state.AgentEvents[event.ID] = event
	s.dirtyAgentEvents = true
}

func containsText(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
