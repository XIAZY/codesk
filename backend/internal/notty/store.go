package notty

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
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
	rootMapName              = "root"
	rootEntriesMapName       = "entriesById"
)

type Store struct {
	mu            sync.RWMutex
	state         WorkspaceState
	db            *sql.DB
	workspaceID   string
	workspaceName string

	documentLocks         map[string]*sync.RWMutex
	dirtyDocuments        map[string]struct{}
	pendingDocumentEvents []documentUpdateRecord
	pendingInboxChanges   []AgentInboxChangedEvent
	pendingAgentEvents    []*AgentEvent
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

const (
	defaultWorkspaceID = "00000000-0000-0000-0000-000000000001"
	defaultOwnerUserID = "00000000-0000-0000-0000-000000000002"
)

type principalRef struct {
	UserID string
	Handle string
	Name   string
	Kind   string
}

func NewWorkspaceStore(database *Database, workspaceID string, workspaceName string) (*Store, error) {
	if database == nil || database.DB == nil {
		return nil, errors.New("database is required")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, errors.New("workspace id is required")
	}
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		workspaceName = workspaceID
	}
	store := &Store{
		db:            database.DB,
		workspaceID:   workspaceID,
		workspaceName: workspaceName,
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Reload() error {
	return s.load()
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = seedWorkspaceFor(s.workspaceID, s.workspaceName)
	if err := s.loadWorkspaceMetadataPostgresLocked(); err != nil {
		return fmt.Errorf("load workspace metadata: %w", err)
	}
	if err := s.loadNormalizedPostgresLocked(); err != nil {
		return fmt.Errorf("load normalized workspace state: %w", err)
	}
	s.ensureMaps()
	needsPersist := false
	if changed, err := s.ensureRootDocumentLocked(); err != nil {
		return fmt.Errorf("ensure root document: %w", err)
	} else if changed {
		needsPersist = true
	}
	if s.refreshAgentSystemPromptsLocked() {
		needsPersist = true
	}
	s.refreshThreadParticipantsLocked()
	if needsPersist {
		if err := s.persistLocked(); err != nil {
			return fmt.Errorf("persist normalized workspace state: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureMaps() {
	if s.state.ContentDocuments == nil {
		s.state.ContentDocuments = map[string]*Document{}
	}
	if s.state.Users == nil {
		s.state.Users = map[string]*User{}
	}
	if s.state.Daemons == nil {
		s.state.Daemons = map[string]*Daemon{}
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
	if s.pendingDocumentEvents == nil {
		s.pendingDocumentEvents = []documentUpdateRecord{}
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

func cloneStringSet(values map[string]struct{}) map[string]struct{} {
	if values == nil {
		return nil
	}
	clone := make(map[string]struct{}, len(values))
	for value := range values {
		clone[value] = struct{}{}
	}
	return clone
}

func seedWorkspaceFor(workspaceID string, workspaceName string) WorkspaceState {
	now := time.Now().UTC()
	workspaceID = strings.TrimSpace(workspaceID)
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		workspaceName = workspaceID
	}
	return WorkspaceState{
		WorkspaceID:         workspaceID,
		Name:                workspaceName,
		ContentDocuments:    map[string]*Document{},
		Users:               map[string]*User{},
		Daemons:             map[string]*Daemon{},
		Agents:              map[string]*Agent{},
		AgentRuns:           map[string]*AgentRun{},
		Threads:             map[string]*Thread{},
		AgentDocumentViews:  map[string]*AgentDocumentView{},
		DocumentCheckpoints: map[string]*DocumentCheckpoint{},
		Presences:           map[string]*Presence{},
		Activities:          []*ActivityEvent{},
		UpdatedAt:           now,
	}
}

func newRootDocumentID() string {
	return uuid.NewString()
}

func normalizeCreateDocumentID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("invalid document id: %w", err)
	}
	return id, nil
}

func (s *Store) ensureRootDocumentLocked() (bool, error) {
	s.ensureMaps()
	rootID := strings.TrimSpace(s.state.RootDocumentID)
	if rootID == "" {
		return false, errors.New("workspace root document id is required")
	}
	s.state.RootDocumentID = rootID
	if existing := s.state.ContentDocuments[rootID]; existing != nil {
		changed := false
		if !existing.Hidden {
			existing.Hidden = true
			changed = true
		}
		if changed {
			existing.UpdatedAt = time.Now().UTC()
			s.markDocumentDirtyLocked(existing.ID)
		}
		return changed, nil
	}

	now := time.Now().UTC()
	clientIDSeed := s.nextClientIDSeedLocked()
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientIDSeed)))
	defer doc.Close()
	document := &Document{
		ID:           rootID,
		Hidden:       true,
		UpdatedAt:    now,
		ClientIDSeed: clientIDSeed,
		StateVector:  base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc)),
	}
	s.state.ContentDocuments[rootID] = document
	_ = s.documentLockLocked(rootID)
	s.markDocumentDirtyLocked(rootID)
	s.appendIncrementalDocumentUpdateLocked(rootID, doc.EncodeStateAsUpdate(), OperationMeta{
		ActorID:   "system",
		ActorType: "system",
		Source:    "root-document-bootstrap",
	}, now)
	s.state.UpdatedAt = now
	return true, nil
}

func newSeedDocument(id string, clientID uint64, content string, now time.Time) (*Document, []byte) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, content, nil)
	}, "seed")
	document := &Document{
		ID:           id,
		UpdatedAt:    now,
		ClientIDSeed: clientID,
	}
	document.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	return document, doc.EncodeStateAsUpdate()
}

func (s *Store) Snapshot() WorkspaceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

func (s *Store) GetDocument(id string) (*Document, error) {
	s.mu.Lock()
	document, ok := s.state.ContentDocuments[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.RLock()
	s.mu.Unlock()
	defer documentLock.RUnlock()
	return cloneDocument(document), nil
}

func (s *Store) HasDocument(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.state.ContentDocuments[id]
	return ok
}

func (s *Store) EncodeDocumentSyncUpdates(documentID string, stateVector []byte) (*DocumentMetadata, [][]byte, error) {
	s.mu.Lock()
	document, ok := s.state.ContentDocuments[documentID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.RLock()
	headUpdateID := document.UpdateID
	metadata := documentMetadata(document)
	s.mu.Unlock()
	defer documentLock.RUnlock()

	if headUpdateID <= 0 {
		return metadata, nil, nil
	}
	doc, err := s.restoreDocumentDocPostgresLocked(document)
	if err != nil {
		return nil, nil, err
	}
	metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	update, err := doc.EncodeStateAsUpdateV1(stateVector)
	if err != nil {
		return nil, nil, err
	}
	if len(update) == 0 {
		return metadata, nil, nil
	}
	return metadata, [][]byte{update}, nil
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
	if document := s.state.ContentDocuments[documentID]; document == nil || document.Hidden {
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
	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()

	trimmed := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if t := strings.TrimSpace(st); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	return listAgentEventsPostgres(s.db, workspaceID, resolvedAgentID, "", trimmed)
}

func (s *Store) DrainAgentInboxChanges() []AgentInboxChangedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingInboxChanges) == 0 {
		return nil
	}
	changes := append([]AgentInboxChangedEvent(nil), s.pendingInboxChanges...)
	s.pendingInboxChanges = nil
	return changes
}

func (s *Store) ListAgentInbox(agentID string, box string, statuses ...string) ([]*AgentEvent, error) {
	s.mu.RLock()
	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()

	box = normalizeInboxBox(box)
	trimmed := make([]string, 0, len(statuses))
	allowed := map[string]struct{}{}
	for _, st := range statuses {
		if t := strings.TrimSpace(st); t != "" {
			trimmed = append(trimmed, t)
			allowed[t] = struct{}{}
		}
	}
	notifications, err := listAgentEventsPostgres(s.db, workspaceID, resolvedAgentID, box, trimmed)
	if err != nil {
		return nil, err
	}
	if includesPendingStatus(allowed) {
		seen := map[string]struct{}{}
		for _, event := range notifications {
			seen[inboxDedupKey(event)] = struct{}{}
		}
		s.mu.RLock()
		synthetic := s.computedDocumentInboxLocked(resolvedAgentID)
		s.mu.RUnlock()
		for _, event := range synthetic {
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
		sort.Slice(notifications, func(i, j int) bool {
			if notifications[i].CreatedAt.Equal(notifications[j].CreatedAt) {
				return notifications[i].ID < notifications[j].ID
			}
			return notifications[i].CreatedAt.Before(notifications[j].CreatedAt)
		})
	}
	return notifications, nil
}

func (s *Store) GetAgentNotification(id string) (*AgentEvent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()
	return getAgentEventPostgres(s.db, workspaceID, id)
}

func (s *Store) UpdateAgentNotification(id string, req UpdateAgentNotificationRequest, meta OperationMeta) (*AgentEvent, error) {
	return s.UpdateAgentEvent(id, UpdateAgentEventRequest{
		Status: strings.TrimSpace(req.Status),
	}, meta)
}

func (s *Store) GetAgentInboxItem(id string) (*AgentEvent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()
	event, err := getAgentEventPostgres(s.db, workspaceID, id)
	if err == ErrNotFound {
		s.mu.RLock()
		synthetic, ok := s.syntheticDocumentInboxItemLocked(id)
		s.mu.RUnlock()
		if ok {
			return synthetic, nil
		}
		return nil, ErrNotFound
	}
	return event, err
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
	document, ok := s.state.ContentDocuments[strings.TrimSpace(documentID)]
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
	workspaceID := s.state.WorkspaceID
	cloned := cloneAgentDocumentView(view)
	s.mu.Unlock()
	if err := upsertAgentDocumentViewPostgres(s.db, workspaceID, cloned); err != nil {
		return nil, err
	}
	if err := completeDocumentInboxEventsPostgres(s.db, workspaceID, resolvedAgentID, document.ID, updateID, now); err != nil {
		return nil, err
	}
	return cloned, nil
}

func (s *Store) DiffDocument(agentID, documentID, fromSpec, toSpec string) (*DocumentDiff, error) {
	s.mu.RLock()

	resolvedAgentID, _, err := s.resolveAgentIdentityLocked(strings.TrimSpace(agentID))
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	document, ok := s.state.ContentDocuments[strings.TrimSpace(documentID)]
	if !ok || document.Hidden {
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

func (s *Store) computedDocumentInboxLocked(agentID string) []*AgentEvent {
	items := make([]*AgentEvent, 0)
	for _, document := range s.state.ContentDocuments {
		if document == nil || document.Hidden || document.UpdateID <= 0 {
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
		box, eventType := s.documentInboxClassificationLocked(agentID, document)
		items = append(items, &AgentEvent{
			ID:           syntheticDocumentInboxID(box, agentID, document.ID),
			AgentID:      agentID,
			AgentHandle:  s.agentHandleByIDLocked(agentID),
			Type:         eventType,
			Box:          box,
			Status:       "pending",
			DocumentID:   document.ID,
			FromUpdateID: fromUpdateID,
			ToUpdateID:   document.UpdateID,
			Summary:      fmt.Sprintf("%s changed from version %d to %d", documentLabel(document), fromUpdateID, document.UpdateID),
			Prompt:       fmt.Sprintf("Review %s with notty-agent-tool diff-document --document-id %s --from %d --to %d. Act only if you have useful feedback or edits.", documentLabel(document), document.ID, fromUpdateID, document.UpdateID),
			AvailableAt:  document.UpdatedAt,
			CreatedAt:    document.UpdatedAt,
			UpdatedAt:    document.UpdatedAt,
		})
	}
	return items
}

func (s *Store) documentInboxHandledLocked(agentID string, document *Document) bool {
	handled, err := documentInboxHandledPostgres(s.db, s.state.WorkspaceID, agentID, document.ID, document.UpdateID, document.UpdatedAt)
	if err != nil {
		return false
	}
	return handled
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

func (s *Store) documentInboxClassificationLocked(agentID string, document *Document) (box string, eventType string) {
	for _, thread := range s.state.Threads {
		if thread == nil || thread.DocumentID != document.ID || thread.Status != "open" {
			continue
		}
		if containsText(thread.ParticipantIDs, agentID) {
			return "for_me", "document.updated"
		}
	}
	return "general", "document.updated"
}

func shouldMarkDocumentViewedForEvent(event *AgentEvent) bool {
	if event == nil || event.AgentID == "" || event.DocumentID == "" || !strings.HasPrefix(event.Type, "document.") {
		return false
	}
	return event.Status == "completed" || event.Status == "dismissed"
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

	requestedID, err := normalizeCreateDocumentID(req.DocumentID)
	if err != nil {
		return nil, err
	}
	clientOperationID := strings.TrimSpace(req.ClientOperationID)
	if requestedID != "" {
		if existing := s.state.ContentDocuments[requestedID]; existing != nil {
			if existing.CreateClientOperationID != "" && clientOperationID != "" && existing.CreateClientOperationID == clientOperationID {
				return cloneDocument(existing), nil
			}
			if existing.CreateClientOperationID == "" && clientOperationID == "" {
				return cloneDocument(existing), nil
			}
			return nil, fmt.Errorf("document id %s already exists for a different create operation", requestedID)
		}
	}

	rollbackState := cloneState(s.state)
	rollbackDirtyDocuments := cloneStringSet(s.dirtyDocuments)
	rollbackPendingDocumentEvents := append([]documentUpdateRecord(nil), s.pendingDocumentEvents...)

	now := time.Now().UTC()
	clientIDSeed := s.nextClientIDSeedLocked()
	id := uuid.NewString()
	if requestedID != "" {
		id = requestedID
	}
	document, initialUpdate := newSeedDocument(id, clientIDSeed, "", now)
	document.CreateClientOperationID = clientOperationID
	s.state.ContentDocuments[id] = document
	_ = s.documentLockLocked(document.ID)
	s.markDocumentDirtyLocked(document.ID)
	s.appendIncrementalDocumentUpdateLocked(document.ID, initialUpdate, meta, now)
	s.state.UpdatedAt = now
	s.appendActivityLocked(&ActivityEvent{
		Type:       "document.created",
		DocumentID: document.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s created document %s", meta.ActorID, document.ID),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistDocumentMutationLocked(); err != nil {
		s.state = rollbackState
		s.dirtyDocuments = rollbackDirtyDocuments
		s.pendingDocumentEvents = rollbackPendingDocumentEvents
		delete(s.documentLocks, document.ID)
		return nil, err
	}
	created := cloneDocument(document)
	return created, nil
}

func (s *Store) CreateAgent(req CreateAgentRequest, meta OperationMeta) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	daemonID := strings.TrimSpace(req.DaemonID)
	if daemonID == "" {
		for id, daemon := range s.state.Daemons {
			if daemon != nil && daemon.Status == "active" {
				if daemonID != "" {
					daemonID = ""
					break
				}
				daemonID = id
			}
		}
	}
	if daemonID == "" {
		return nil, errors.New("daemon id is required")
	}
	daemon := s.state.Daemons[daemonID]
	if daemon == nil || daemon.Status != "active" || !daemon.DeletedAt.IsZero() {
		return nil, ErrNotFound
	}
	kind, err := normalizeAgentRuntimeKind(req.Kind)
	if err != nil {
		return nil, err
	}
	if err := validateDaemonRuntimeKind(daemon, kind); err != nil {
		return nil, err
	}
	agent, err := buildAgent(req.Handle, req.Name, req.Role, kind)
	if err != nil {
		return nil, err
	}
	if err := s.ensureHandleAvailableLocked(agent.Handle, "", ""); err != nil {
		return nil, err
	}
	agent.ID = uuid.NewString()
	agent.DaemonID = daemonID
	agent.WorkspaceRoot = "agents/" + agent.ID
	agent.UpdatedAt = time.Now().UTC()
	s.state.Agents[agent.ID] = agent
	for _, document := range s.state.ContentDocuments {
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
		ID:              uuid.NewString(),
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
	if sessionID := strings.TrimSpace(req.SessionID); sessionID != "" {
		agent.SessionID = sessionID
	}
	if turnID := strings.TrimSpace(req.CurrentTurnID); turnID != "" || agent.Status != "working" {
		agent.CurrentTurnID = turnID
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
	workspaceID := s.state.WorkspaceID
	updated := cloneAgent(agent)
	s.mu.Unlock()
	if err := upsertAgentPostgres(s.db, workspaceID, updated); err != nil {
		return nil, err
	}
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

	document, ok := s.state.ContentDocuments[documentID]
	if !ok {
		return nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	defer documentLock.Unlock()

	doc, err := s.restoreDocumentDocPostgresLocked(document)
	if err != nil {
		return nil, err
	}
	beforeState := doc.EncodeStateAsUpdate()
	if err := crdt.ApplyUpdateV1(doc, update, meta); err != nil {
		return nil, err
	}
	afterState := doc.EncodeStateAsUpdate()
	afterStateVector := crdt.EncodeStateVectorV1(doc)
	if bytes.Equal(beforeState, afterState) {
		return &ApplyCRDTUpdateResult{
			Document: cloneDocument(document),
			Applied:  false,
		}, nil
	}
	now := time.Now().UTC()
	s.markDocumentDirtyLocked(document.ID)
	s.appendIncrementalDocumentUpdateLocked(document.ID, update, meta, now)
	document.StateVector = base64.StdEncoding.EncodeToString(afterStateVector)
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

func (s *Store) ReplaceDocumentText(documentID string, nextText string, meta OperationMeta) (*Document, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.ContentDocuments[documentID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	documentLock := s.documentLockLocked(document.ID)
	documentLock.Lock()
	defer documentLock.Unlock()
	doc, err := s.restoreDocumentDocPostgresLocked(document)
	if err != nil {
		return nil, nil, err
	}
	currentText := doc.GetText("content").ToString()
	text := doc.GetText("content")
	currentLength := text.Len()

	var update []byte
	unsubscribe := doc.OnUpdate(func(nextUpdate []byte, origin any) {
		if origin == meta {
			update = append([]byte(nil), nextUpdate...)
		}
	})
	doc.Transact(func(txn *crdt.Transaction) {
		if len(currentText) > 0 {
			text.Delete(txn, 0, currentLength)
		}
		if nextText != "" {
			text.Insert(txn, 0, nextText, nil)
		}
	}, meta)
	unsubscribe()

	document.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	s.markDocumentDirtyLocked(document.ID)
	s.appendIncrementalDocumentUpdateLocked(document.ID, update, meta, time.Now().UTC())
	document.UpdatedAt = time.Now().UTC()
	s.state.UpdatedAt = document.UpdatedAt
	s.appendActivityLocked(&ActivityEvent{
		Type:       "document.updated",
		DocumentID: document.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s replaced %s", meta.ActorID, documentLabel(document)),
		OccurredAt: document.UpdatedAt,
		Provenance: meta,
	})

	if err := s.persistDocumentMutationLocked(); err != nil {
		return nil, nil, err
	}
	return cloneDocument(document), update, nil
}

func (s *Store) CreateThread(req CreateThreadRequest, meta OperationMeta) (*Thread, *ThreadMessage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, ok := s.state.ContentDocuments[req.DocumentID]
	if !ok || document.Hidden {
		return nil, nil, false, ErrNotFound
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, nil, false, errors.New("thread body is required")
	}
	author, err := s.resolvePrincipalLocked(meta.ActorID, meta.ActorType)
	if err != nil {
		return nil, nil, false, err
	}
	clientOperationID := strings.TrimSpace(req.ClientOperationID)
	if clientOperationID != "" {
		for _, thread := range s.state.Threads {
			if thread == nil {
				continue
			}
			if thread.ClientOperationID == clientOperationID && thread.CreatedByID == author.ID && thread.CreatedByType == author.Type {
				message := firstThreadMessage(thread)
				return cloneThread(thread), cloneThreadMessage(message), false, nil
			}
		}
	}
	now := time.Now().UTC()
	anchor, err := buildThreadAnchorFromRequest(req)
	if err != nil {
		return nil, nil, false, err
	}
	thread := &Thread{
		ID:                uuid.NewString(),
		DocumentID:        document.ID,
		ClientOperationID: clientOperationID,
		Title:             firstNonEmptyString(strings.TrimSpace(req.Title), inferThreadTitleFromRequest(document, req)),
		Status:            "open",
		Anchor:            anchor,
		CreatedByID:       author.ID,
		CreatedByType:     author.Type,
		CreatedByHandle:   author.Handle,
		CreatedByName:     author.Name,
		ParticipantIDs:    []string{author.ID},
		Messages:          []*ThreadMessage{},
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	message := &ThreadMessage{
		ID:           uuid.NewString(),
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
		Summary:    fmt.Sprintf("%s started a thread on %s", meta.ActorID, documentLabel(document)),
		OccurredAt: now,
		Provenance: meta,
	})
	if err := s.persistLocked(); err != nil {
		return nil, nil, false, err
	}
	return cloneThread(thread), cloneThreadMessage(message), true, nil
}

func firstThreadMessage(thread *Thread) *ThreadMessage {
	if thread == nil || len(thread.Messages) == 0 {
		return nil
	}
	return thread.Messages[0]
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
		ID:           uuid.NewString(),
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
	agentID, agentHandle, err := s.resolveAgentIdentityLocked(req.AgentID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	workspaceID := s.state.WorkspaceID
	targetAgent := s.state.Agents[agentID]
	if targetAgent == nil {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	claimedBy := strings.TrimSpace(req.ClaimedBy)
	targetDaemonID := strings.TrimSpace(targetAgent.DaemonID)
	switch claimedBy {
	case "", "daemon", "system":
		claimedBy = agentID
	case agentID:
	case targetDaemonID:
		if targetDaemonID == "" || s.state.Daemons[targetDaemonID] == nil {
			s.mu.Unlock()
			return nil, errors.New("claimed_by must be the target agent or its daemon")
		}
	default:
		s.mu.Unlock()
		return nil, errors.New("claimed_by must be the target agent or its daemon")
	}
	s.mu.Unlock()
	event, err := claimAgentEventPostgres(s.db, workspaceID, agentID, agentHandle, claimedBy)
	if err != nil {
		return nil, err
	}
	return event, nil
}

func (s *Store) UpdateAgentEvent(id string, req UpdateAgentEventRequest, meta OperationMeta) (*AgentEvent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()
	updated, err := updateAgentEventPostgres(s.db, workspaceID, id, req)
	if err != nil {
		return nil, err
	}
	if shouldMarkDocumentViewedForEvent(updated) {
		if _, err := s.MarkDocumentViewed(updated.AgentID, updated.DocumentID, MarkDocumentViewedRequest{UpdateID: updated.ToUpdateID}); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
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
	workspaceID := s.state.WorkspaceID
	clone := *presence
	clone.Selection = append([]int(nil), presence.Selection...)
	s.mu.Unlock()
	if err := upsertPresencePostgres(s.db, workspaceID, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func (s *Store) persistLocked() error {
	s.ensureMaps()
	return s.persistPostgresLocked()
}

func (s *Store) persistDocumentMutationLocked() error {
	s.ensureMaps()
	return s.persistDocumentMutationPostgresLocked()
}

func (s *Store) nextClientIDSeedLocked() uint64 {
	var next uint64 = 1001
	for _, document := range s.state.ContentDocuments {
		if document.ClientIDSeed >= next {
			next = document.ClientIDSeed + 1
		}
	}
	return next
}

func (s *Store) appendActivityLocked(event *ActivityEvent) {
	s.state.Activities = append([]*ActivityEvent{event}, s.state.Activities...)
	if len(s.state.Activities) > 100 {
		s.state.Activities = s.state.Activities[:100]
	}
}

func cloneState(state WorkspaceState) WorkspaceState {
	copyState := state
	copyState.ContentDocuments = map[string]*Document{}
	for key, doc := range state.ContentDocuments {
		copyState.ContentDocuments[key] = cloneDocument(doc)
	}
	copyState.Users = map[string]*User{}
	for key, user := range state.Users {
		copyState.Users[key] = cloneUser(user)
	}
	copyState.Daemons = map[string]*Daemon{}
	for key, daemon := range state.Daemons {
		copyState.Daemons[key] = cloneDaemon(daemon)
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
	s.dirtyDocuments[documentID] = struct{}{}
}

func (s *Store) appendIncrementalDocumentUpdateLocked(documentID string, update []byte, meta OperationMeta, now time.Time) {
	if len(update) == 0 || documentID == "" {
		return
	}
	s.pendingDocumentEvents = append(s.pendingDocumentEvents, documentUpdateRecord{
		DocumentID: documentID,
		Update:     append([]byte(nil), update...),
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		CreatedAt:  now,
	})
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

func SortedDaemons(state WorkspaceState) []*Daemon {
	daemons := make([]*Daemon, 0, len(state.Daemons))
	now := time.Now().UTC()
	for _, daemon := range state.Daemons {
		daemons = append(daemons, daemonWithLiveness(daemon, now))
	}
	sort.Slice(daemons, func(i, j int) bool {
		if daemons[i].CreatedAt.Equal(daemons[j].CreatedAt) {
			return daemons[i].ID < daemons[j].ID
		}
		return daemons[i].CreatedAt.Before(daemons[j].CreatedAt)
	})
	return daemons
}

func isPostgresDSN(value string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://")
}

func initPostgresSchema(db *sql.DB) error {
	if err := initPostgresSchemaTables(db); err != nil {
		return err
	}
	return initPostgresSchemaConstraints(db)
}

func uuidStringOrNil(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !isUUIDString(value) {
		return nil
	}
	return value
}

func isUUIDString(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

func actorUUIDOrNil(actorID, actorType string) any {
	actorID = strings.TrimSpace(actorID)
	switch strings.TrimSpace(actorType) {
	case "", "system":
		return nil
	default:
		return uuidStringOrNil(actorID)
	}
}

func stringFromNull(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
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

func documentLabel(document *Document) string {
	if document == nil || strings.TrimSpace(document.ID) == "" {
		return "document"
	}
	return "document " + document.ID
}

func buildAgent(handle, name, role, kind string) (*Agent, error) {
	trimmedKind, err := normalizeAgentRuntimeKind(kind)
	if err != nil {
		return nil, err
	}
	validHandle, err := validateHandle(handle)
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
		Handle: validHandle,
		Name:   trimmedName,
		Role:   trimmedRole,
		Kind:   trimmedKind,
		Status: "idle",
	}
	agent.SystemPrompt = sharedAgentSystemPrompt(agent)
	return agent, nil
}

func normalizeAgentRuntimeKind(kind string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(kind))
	if trimmed == "" {
		return "", errors.New("agent kind is required")
	}
	for i, r := range trimmed {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		if i > 0 && (r == '-' || r == '_') {
			continue
		}
		return "", fmt.Errorf("invalid agent kind %q", strings.TrimSpace(kind))
	}
	return trimmed, nil
}

func validateDaemonRuntimeKind(daemon *Daemon, kind string) error {
	if daemon == nil {
		return ErrNotFound
	}
	if len(daemon.Runtimes) == 0 {
		return fmt.Errorf("daemon %q has not reported runtime availability", daemon.ID)
	}
	for _, runtime := range daemon.Runtimes {
		runtimeKind, err := normalizeAgentRuntimeKind(runtime.Kind)
		if err != nil || runtimeKind != kind {
			continue
		}
		if runtime.Available {
			return nil
		}
		reason := strings.TrimSpace(runtime.Reason)
		if reason != "" {
			return fmt.Errorf("runtime %q is unavailable on daemon %q: %s", kind, daemon.ID, reason)
		}
		return fmt.Errorf("runtime %q is unavailable on daemon %q", kind, daemon.ID)
	}
	return fmt.Errorf("runtime %q is not reported by daemon %q", kind, daemon.ID)
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

func cloneDaemon(daemon *Daemon) *Daemon {
	if daemon == nil {
		return nil
	}
	clone := *daemon
	return &clone
}

const (
	daemonOnlineWindow = 30 * time.Second
	daemonStaleWindow  = 2 * time.Minute
)

func daemonWithLiveness(daemon *Daemon, now time.Time) *Daemon {
	clone := cloneDaemon(daemon)
	applyDaemonLiveness(clone, now)
	return clone
}

func applyDaemonLiveness(daemon *Daemon, now time.Time) {
	if daemon == nil {
		return
	}
	daemon.ConnectionStatus = "disconnected"
	daemon.LastSeenAgeSeconds = 0
	if daemon.Status != "active" || !daemon.DeletedAt.IsZero() || daemon.LastSeenAt.IsZero() {
		return
	}
	age := now.Sub(daemon.LastSeenAt)
	if age < 0 {
		age = 0
	}
	daemon.LastSeenAgeSeconds = int64(age / time.Second)
	switch {
	case age <= daemonOnlineWindow:
		daemon.ConnectionStatus = "online"
	case age <= daemonStaleWindow:
		daemon.ConnectionStatus = "stale"
	default:
		daemon.ConnectionStatus = "disconnected"
	}
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
			return errors.New("Handle is already taken.")
		}
	}
	for id, agent := range s.state.Agents {
		if id == exceptAgentID {
			continue
		}
		if agent.Handle == handle {
			return errors.New("Handle is already taken.")
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
	return truncateText(trimmed, 72)
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
	value = strings.ToValidUTF8(value, "\uFFFD")
	if limit <= 0 || runeCount(value) <= limit {
		return value
	}
	if limit <= 3 {
		return firstRunes(value, limit)
	}
	return firstRunes(value, limit-3) + "..."
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


func cloneThread(thread *Thread) *Thread {
	if thread == nil {
		return nil
	}
	clone := *thread
	clone.ParticipantIDs = cloneStringSlice(thread.ParticipantIDs)
	clone.ParticipantHandles = cloneStringSlice(thread.ParticipantHandles)
	clone.Messages = make([]*ThreadMessage, len(thread.Messages))
	for index, message := range thread.Messages {
		clone.Messages[index] = cloneThreadMessage(message)
	}
	return &clone
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
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

func buildThreadAnchorFromRequest(req CreateThreadRequest) (ThreadAnchor, error) {
	relativeStart := strings.TrimSpace(req.RelativeStart)
	relativeEnd := strings.TrimSpace(req.RelativeEnd)
	if (relativeStart == "") != (relativeEnd == "") {
		return ThreadAnchor{}, errors.New("relativeStart and relativeEnd must be provided together")
	}
	excerpt := truncateText(strings.TrimSpace(req.Excerpt), 140)
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		if relativeStart == "" {
			kind = "document"
		} else {
			kind = "text-range"
		}
	}
	if kind != "document" && kind != "text-range" {
		return ThreadAnchor{}, errors.New("thread anchor kind must be document or text-range")
	}
	if kind == "document" {
		if relativeStart != "" || relativeEnd != "" {
			return ThreadAnchor{}, errors.New("document threads cannot include relative anchors")
		}
		return ThreadAnchor{
			Kind:    "document",
			Excerpt: excerpt,
		}, nil
	}
	if relativeStart == "" && relativeEnd == "" {
		return ThreadAnchor{}, errors.New("text-range threads require relativeStart and relativeEnd")
	}
	anchor := ThreadAnchor{
		Kind:    "text-range",
		Excerpt: excerpt,
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
		return fmt.Sprintf("Discussion on %s", documentLabel(document))
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

func (s *Store) enqueueDocumentInboxEventsLocked(document *Document, meta OperationMeta) {
	if document == nil || document.Hidden || document.UpdateID <= 1 {
		return
	}
	for _, agent := range s.state.Agents {
		if agent == nil || !s.shouldNotifyAgentLocked(agent.ID, meta, "") {
			continue
		}
		box, threadID, threadTitle := s.documentInboxTargetLocked(agent.ID, document)
		view := s.state.AgentDocumentViews[agentDocumentViewKey(agent.ID, document.ID)]
		fromUpdateID := int64(0)
		if view != nil {
			fromUpdateID = view.UpdateID
		}
		if fromUpdateID >= document.UpdateID {
			continue
		}
		s.upsertDocumentInboxEventLocked(agent, document, box, threadID, threadTitle, fromUpdateID, document.UpdateID)
	}
}

func (s *Store) documentInboxTargetLocked(agentID string, document *Document) (box string, threadID string, threadTitle string) {
	for _, thread := range s.state.Threads {
		if thread == nil || document == nil || thread.DocumentID != document.ID || thread.Status != "open" {
			continue
		}
		if containsText(thread.ParticipantIDs, agentID) {
			return "for_me", thread.ID, thread.Title
		}
	}
	return "general", "", ""
}

func (s *Store) upsertDocumentInboxEventLocked(agent *Agent, document *Document, box string, threadID string, threadTitle string, fromUpdateID int64, toUpdateID int64) {
	if agent == nil || document == nil || toUpdateID <= 0 {
		return
	}
	box = normalizeInboxBox(box)
	now := time.Now().UTC()
	summary := fmt.Sprintf("%s changed from version %d to %d", documentLabel(document), fromUpdateID, toUpdateID)
	prompt := fmt.Sprintf("Review %s with notty-agent-tool diff-document --document-id %s --from %d --to %d. Act only if you have useful feedback or edits.", documentLabel(document), document.ID, fromUpdateID, toUpdateID)
	if threadID != "" {
		summary = fmt.Sprintf("%s changed near thread %s", documentLabel(document), firstNonEmptyString(threadTitle, threadID))
		prompt = fmt.Sprintf("Document %s changed near thread %q. Review the latest edits and continue the discussion if needed.", documentLabel(document), firstNonEmptyString(threadTitle, threadID))
	}
	event := &AgentEvent{
		ID:           uuid.NewString(),
		AgentID:      agent.ID,
		AgentHandle:  agent.Handle,
		Type:         "document.updated",
		Box:          box,
		Status:       "pending",
		DocumentID:   document.ID,
		ThreadID:     threadID,
		FromUpdateID: fromUpdateID,
		ToUpdateID:   toUpdateID,
		Summary:      summary,
		Prompt:       prompt,
		DedupKey:     fmt.Sprintf("document-updated:%s:%s:%s", box, document.ID, agent.ID),
		CreatedAt:    now,
		UpdatedAt:    now,
		AvailableAt:  now,
	}
	upserted, err := upsertDocumentInboxEventPostgres(s.db, s.state.WorkspaceID, event)
	if err != nil {
		return
	}
	s.recordAgentInboxChangedLocked(upserted)
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
			event.Summary = fmt.Sprintf("@%s was mentioned in thread %s", agent.Handle, thread.Title)
			event.Prompt = fmt.Sprintf("You were mentioned by @%s in thread %q: %s", message.AuthorHandle, thread.Title, truncateText(message.Body, 240))
		})
	}
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
	now := time.Now().UTC()
	event := &AgentEvent{
		ID:          uuid.NewString(),
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
	s.pendingAgentEvents = append(s.pendingAgentEvents, event)
	s.recordAgentInboxChangedLocked(event)
}

func (s *Store) recordAgentInboxChangedLocked(event *AgentEvent) {
	if event == nil || event.AgentID == "" {
		return
	}
	change := AgentInboxChangedEvent{
		WorkspaceID:      s.state.WorkspaceID,
		AgentID:          event.AgentID,
		Box:              normalizeInboxBox(event.Box),
		EventID:          event.ID,
		NotificationType: event.Type,
	}
	if agent := s.state.Agents[event.AgentID]; agent != nil {
		change.DaemonID = agent.DaemonID
	}
	s.pendingInboxChanges = append(s.pendingInboxChanges, change)
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
	value = strings.ToValidUTF8(value, "\uFFFD")
	if limit < 0 {
		limit = 0
	}
	if runeCount(value) <= limit {
		return value
	}
	return firstRunes(value, limit) + "..."
}

func firstRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	count := 0
	for idx := range value {
		if count == limit {
			return value[:idx]
		}
		count++
	}
	return value
}

func runeCount(value string) int {
	count := 0
	for range value {
		count++
	}
	return count
}
