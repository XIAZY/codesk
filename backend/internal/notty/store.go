package notty

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
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

// documentQuiescenceWindow is how long after a subscribed document's last edit its inbox card matures and
// becomes deliverable — one delivery per edit session (task #2), not one per keystroke batch.
const documentQuiescenceWindow = 60 * time.Second

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
	pendingActivities     []*ActivityEvent
	committedActivities   []*ActivityEvent
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
	if err := s.refreshAgentSystemPromptsLocked(); err != nil {
		return fmt.Errorf("refresh agent system prompts: %w", err)
	}
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
	if s.state.DocumentCheckpoints == nil {
		s.state.DocumentCheckpoints = map[string]*DocumentCheckpoint{}
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
		DocumentCheckpoints: map[string]*DocumentCheckpoint{},
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
	existing := s.state.ContentDocuments[rootID]
	if existing == nil {
		// The deferred fk_workspaces_root_document constraint makes a workspace
		// without its root document row unrepresentable, and every workspace is
		// created with its root in one transaction (seedRootDocumentTx). Reaching
		// here means that invariant was violated — fail closed rather than
		// silently re-seed a document whose absence is itself the bug.
		return false, fmt.Errorf("workspace %s is missing root document %s", s.state.WorkspaceID, rootID)
	}
	changed := false
	if !existing.Hidden {
		existing.Hidden = true
		existing.UpdatedAt = time.Now().UTC()
		s.markDocumentDirtyLocked(existing.ID)
		changed = true
	}
	return changed, nil
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

func (s *Store) WorkspaceID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.WorkspaceID
}

func (s *Store) RootDocumentID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.RootDocumentID
}

func (s *Store) WorkspaceName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Name
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
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()
	return getThreadPostgres(s.db, workspaceID, id)
}

func (s *Store) ListThreadsForDocument(documentID string) ([]*Thread, error) {
	s.mu.RLock()
	document := s.state.ContentDocuments[documentID]
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()
	if document == nil || document.Hidden {
		return nil, ErrNotFound
	}
	return listThreadsForDocumentPostgres(s.db, workspaceID, documentID)
}

func (s *Store) ListAgentNotifications(agentID string, statuses ...string) ([]*AgentEvent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()
	resolvedAgentID, _, err := resolveAgentIdentityPostgres(db, workspaceID, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}

	trimmed := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if t := strings.TrimSpace(st); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	return listAgentEventsPostgres(db, workspaceID, resolvedAgentID, "", trimmed)
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

// SubscribeAgentDocument opts an agent in to a document's updates (task #2). Idempotent — a repeat
// subscribe is a no-op. The FK constraints enforce that the agent and document exist.
func (s *Store) SubscribeAgentDocument(agentID, documentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := subscribeAgentDocumentPostgres(s.db, s.state.WorkspaceID, agentID, documentID, time.Now().UTC())
	return err
}

// SubscribeAgentDocumentAndNotify subscribes the agent and, when a HUMAN subscribed someone OTHER than
// themselves (the Participants-panel case), cards that agent so it learns it was added (task #6). Self-
// subscribe via the tool (an agent principal) gets the CLI confirmation copy instead of a card, and an
// idempotent re-subscribe cards no one. Returns whether a new subscription row was created. The card is a
// directed second-person fact → for_me box, instant doorbell; its `subscription.added` type is deliberately
// NOT `document.`-prefixed so completing it does not advance the document watermark.
func (s *Store) SubscribeAgentDocumentAndNotify(agentID, documentID string, meta OperationMeta) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()

	// Build the card candidate (or nil) BEFORE the write tx — the human-vs-self policy and the actor/agent
	// lookups do not mutate anything, so they stay outside the transaction.
	card, err := s.buildSubscriptionAddedCardLocked(agentID, documentID, meta, now)
	if err != nil {
		return false, err
	}

	// The subscription insert and the conditional card upsert commit together (Thomas/Tom ruling): a card
	// write failure never strands a durable subscription with no retry path, and a tx failure means nothing
	// persisted so the error response is finally true.
	inserted, persisted, err := subscribeAgentDocumentWithCardPostgres(s.db, s.state.WorkspaceID, agentID, documentID, card, now)
	if err != nil {
		return false, err
	}

	// Doorbell from the PERSISTED event (its real id, correct even if a dedup row was reused), recorded
	// post-commit and best-effort: a failed broadcast must never un-subscribe anyone.
	if persisted != nil {
		s.recordAgentInboxChangedLocked(persisted)
	}
	return inserted, nil
}

// buildSubscriptionAddedCardLocked returns the subscription.added card for a genuine human-adds-someone-else
// subscribe, or nil when no card is due (a non-human/self-subscribe, or an unresolvable actor/agent). It only
// reads, so it runs before the write transaction.
func (s *Store) buildSubscriptionAddedCardLocked(agentID, documentID string, meta OperationMeta, now time.Time) (*AgentEvent, error) {
	if !strings.EqualFold(strings.TrimSpace(meta.ActorType), "human") {
		return nil, nil
	}
	actor, err := resolvePrincipalPostgres(s.db, s.state.WorkspaceID, meta.ActorID, meta.ActorType)
	if err != nil {
		if err == ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	if actor.ID == agentID {
		return nil, nil
	}
	agent, err := getAgentPostgres(s.db, s.state.WorkspaceID, agentID)
	if err != nil {
		if err == ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	document := s.state.ContentDocuments[documentID]
	return &AgentEvent{
		ID:          uuid.NewString(),
		AgentID:     agent.ID,
		AgentHandle: agent.Handle,
		Type:        "subscription.added",
		Box:         "for_me",
		Status:      "pending",
		DocumentID:  documentID,
		Summary:     fmt.Sprintf("You were added as a subscriber to %s", documentLabel(document)),
		Prompt:      fmt.Sprintf("@%s added you as a subscriber to %s. You will now receive notifications about new edits and thread messages on this document.", actor.Handle, documentLabel(document)),
		DedupKey:    fmt.Sprintf("subscription-added:%s:%s", documentID, agent.ID),
		CreatedAt:   now,
		UpdatedAt:   now,
		AvailableAt: now,
	}, nil
}

// UnsubscribeAgentDocument removes a subscription. Returns whether a row was removed (idempotent).
func (s *Store) UnsubscribeAgentDocument(agentID, documentID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return unsubscribeAgentDocumentPostgres(s.db, s.state.WorkspaceID, agentID, documentID)
}

// ListAgentDocumentSubscriptions returns the document ids an agent is subscribed to.
func (s *Store) ListAgentDocumentSubscriptions(agentID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listAgentDocumentSubscriptionsPostgres(s.db, s.state.WorkspaceID, agentID)
}

// ListDocumentSubscriberAgents returns the agents subscribed to a document — the doc→agents direction the
// Participants panel reads (task #4), the mirror of ListAgentDocumentSubscriptions. A thin projection
// (id/handle/name/kind) over the same fan-out helper the routing uses plus agent metadata, so the panel and
// the push path agree on who watches a document by construction. A missing/hidden document is ErrNotFound
// (404), like the sibling document reads; a subscription whose agent has since been deleted is skipped.
func (s *Store) ListDocumentSubscriberAgents(documentID string) ([]DocumentSubscriberAgent, error) {
	s.mu.RLock()
	document := s.state.ContentDocuments[documentID]
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()
	if document == nil || document.Hidden {
		return nil, ErrNotFound
	}
	ids, err := listDocumentSubscriberAgentIDsPostgres(db, workspaceID, documentID)
	if err != nil {
		return nil, err
	}
	agents := make([]DocumentSubscriberAgent, 0, len(ids))
	for _, id := range ids {
		agent, err := getAgentPostgres(db, workspaceID, id)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, err
		}
		agents = append(agents, DocumentSubscriberAgent{ID: agent.ID, Handle: agent.Handle, Name: agent.Name, Kind: agent.Kind})
	}
	return agents, nil
}

func (s *Store) DrainActivityChanges() []*ActivityEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.committedActivities) == 0 {
		return nil
	}
	activities := cloneActivityEvents(s.committedActivities)
	s.committedActivities = nil
	return activities
}

func (s *Store) ListAgentInbox(agentID string, box string, statuses ...string) ([]*AgentEvent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()
	resolvedAgentID, agentHandle, err := resolveAgentIdentityPostgres(db, workspaceID, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}

	// Empty/absent box = ALL boxes (AlphaToad's ruling): no box filter here, and the synthetic walk below
	// includes every box. A specific --box normalizes and filters to that box. This "all" semantic lives ONLY
	// at this query/filter layer — normalizeInboxBox's ""→for_me default normalizes EVENT ROWS and stays untouched.
	rawBox := strings.TrimSpace(box)
	filterByBox := rawBox != ""
	queryBox := ""
	if filterByBox {
		queryBox = normalizeInboxBox(rawBox)
	}
	trimmed := make([]string, 0, len(statuses))
	allowed := map[string]struct{}{}
	for _, st := range statuses {
		if t := strings.TrimSpace(st); t != "" {
			trimmed = append(trimmed, t)
			allowed[t] = struct{}{}
		}
	}
	notifications, err := listAgentEventsPostgres(db, workspaceID, resolvedAgentID, queryBox, trimmed)
	if err != nil {
		return nil, err
	}
	if includesPendingStatus(allowed) {
		seen := map[string]struct{}{}
		for _, event := range notifications {
			seen[inboxDedupKey(event)] = struct{}{}
		}
		s.mu.RLock()
		synthetic, err := s.computedDocumentInboxLocked(resolvedAgentID, agentHandle)
		s.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		for _, event := range synthetic {
			if event == nil || (filterByBox && normalizeInboxBox(event.Box) != queryBox) {
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
		synthetic, ok, syntheticErr := s.syntheticDocumentInboxItemLocked(id)
		s.mu.RUnlock()
		if syntheticErr != nil {
			return nil, syntheticErr
		}
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
	s.mu.RLock()
	document, ok := s.state.ContentDocuments[strings.TrimSpace(documentID)]
	if !ok {
		s.mu.RUnlock()
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
	workspaceID := s.state.WorkspaceID
	resolvedDocumentID := document.ID
	s.mu.RUnlock()

	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	resolvedAgentID, _, err := resolveAgentIdentityPostgres(tx, workspaceID, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	view := &AgentDocumentView{
		AgentID:     resolvedAgentID,
		DocumentID:  resolvedDocumentID,
		UpdateID:    updateID,
		StateVector: stateVector,
		ViewedAt:    now,
	}
	if err := upsertAgentDocumentViewPostgres(tx, workspaceID, view); err != nil {
		return nil, err
	}
	if err := completeDocumentInboxEventsPostgres(tx, workspaceID, resolvedAgentID, resolvedDocumentID, updateID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return view, nil
}

func (s *Store) DiffDocument(agentID, documentID, fromSpec, toSpec string) (*DocumentDiff, error) {
	s.mu.RLock()
	document, ok := s.state.ContentDocuments[strings.TrimSpace(documentID)]
	if !ok || document.Hidden {
		s.mu.RUnlock()
		return nil, ErrNotFound
	}
	documentSnapshot := cloneDocument(document)
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()

	resolvedAgentID, _, err := resolveAgentIdentityPostgres(db, workspaceID, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	fromUpdateID, err := resolveDocumentVersionPostgres(db, workspaceID, resolvedAgentID, documentSnapshot, fromSpec, "last-viewed")
	if err != nil {
		return nil, err
	}
	toUpdateID, err := resolveDocumentVersionPostgres(db, workspaceID, resolvedAgentID, documentSnapshot, toSpec, "head")
	if err != nil {
		return nil, err
	}
	if fromUpdateID > toUpdateID {
		return nil, fmt.Errorf("from version %d is newer than to version %d", fromUpdateID, toUpdateID)
	}
	if fromUpdateID == toUpdateID {
		return emptyDocumentDiff(documentSnapshot.ID, fromUpdateID, toUpdateID), nil
	}

	fromContent, err := documentContentAtUpdatePostgres(db, workspaceID, documentSnapshot, fromUpdateID)
	if err != nil {
		return nil, err
	}
	toContent, err := documentContentAtUpdatePostgres(db, workspaceID, documentSnapshot, toUpdateID)
	if err != nil {
		return nil, err
	}
	return buildDocumentDiff(documentSnapshot.ID, fromUpdateID, toUpdateID, fromContent, toContent)
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
	case "muted":
		// Muted is the never-pushed box (task #2). It MUST be recognized here or the default branch below
		// silently routes it to for_me — a PUSHED box — which is the exact trap that would re-introduce
		// ambient document pushes. The daemon copy (automation.go) carries the same case.
		return "muted"
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

func (s *Store) computedDocumentInboxLocked(agentID string, agentHandle string) ([]*AgentEvent, error) {
	items := make([]*AgentEvent, 0)
	for _, document := range s.state.ContentDocuments {
		if document == nil || document.Hidden || document.UpdateID <= 0 {
			continue
		}
		handled, err := s.documentInboxHandledLocked(agentID, document)
		if err != nil {
			return nil, err
		}
		if handled {
			continue
		}
		fromUpdateID := int64(0)
		view, err := getAgentDocumentViewPostgres(s.db, s.state.WorkspaceID, agentID, document.ID)
		if err != nil && err != ErrNotFound {
			return nil, err
		}
		if view != nil {
			fromUpdateID = view.UpdateID
		}
		if fromUpdateID >= document.UpdateID {
			continue
		}
		box, eventType, err := s.documentInboxClassificationLocked(agentID, document)
		if err != nil {
			return nil, err
		}
		items = append(items, &AgentEvent{
			ID:           syntheticDocumentInboxID(box, agentID, document.ID),
			AgentID:      agentID,
			AgentHandle:  agentHandle,
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
	return items, nil
}

func (s *Store) documentInboxHandledLocked(agentID string, document *Document) (bool, error) {
	handled, err := documentInboxHandledPostgres(s.db, s.state.WorkspaceID, agentID, document.ID, document.UpdateID, document.UpdatedAt)
	if err != nil {
		return false, err
	}
	return handled, nil
}

func (s *Store) syntheticDocumentInboxItemLocked(id string) (*AgentEvent, bool, error) {
	spec, ok := parseSyntheticDocumentInboxID(id)
	if !ok {
		return nil, false, nil
	}
	agent, err := getAgentPostgres(s.db, s.state.WorkspaceID, spec.AgentID)
	if err != nil {
		if err == ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	items, err := s.computedDocumentInboxLocked(spec.AgentID, agent.Handle)
	if err != nil {
		return nil, false, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, true, nil
		}
	}
	return nil, false, nil
}

func (s *Store) documentInboxClassificationLocked(agentID string, document *Document) (box string, eventType string, err error) {
	if document == nil {
		return "muted", "document.updated", nil
	}
	// Subscription is the only thing that lifts a document gap out of the muted box (task #2). A subscribed
	// agent sees the gap in `general` (and gets it pushed via the persisted card); everyone else sees it
	// only when they explicitly query `--box muted`. Thread proximity no longer promotes to for_me —
	// thread.mentioned/replied are the sole for_me sources.
	subscribed, err := isAgentSubscribedToDocumentPostgres(s.db, s.state.WorkspaceID, agentID, document.ID)
	if err != nil {
		return "", "", err
	}
	if subscribed {
		return "general", "document.updated", nil
	}
	return "muted", "document.updated", nil
}

func shouldMarkDocumentViewedForEvent(event *AgentEvent) bool {
	if event == nil || event.AgentID == "" || event.DocumentID == "" || !strings.HasPrefix(event.Type, "document.") {
		return false
	}
	return event.Status == "completed" || event.Status == "dismissed"
}

func resolveDocumentVersionPostgres(q querier, workspaceID string, agentID string, document *Document, spec string, defaultSpec string) (int64, error) {
	value := strings.TrimSpace(strings.ToLower(spec))
	if value == "" {
		value = strings.TrimSpace(strings.ToLower(defaultSpec))
	}
	switch value {
	case "head", "current", "latest":
		return document.UpdateID, nil
	case "last-viewed", "last_viewed", "viewed":
		view, err := getAgentDocumentViewPostgres(q, workspaceID, agentID, document.ID)
		if err != nil && err != ErrNotFound {
			return 0, err
		}
		if view != nil {
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

	rollbackState := cloneDocumentState(s.state)
	rollbackDirtyDocuments := cloneStringSet(s.dirtyDocuments)
	rollbackPendingDocumentEvents := append([]documentUpdateRecord(nil), s.pendingDocumentEvents...)
	rollbackPendingActivities := append([]*ActivityEvent(nil), s.pendingActivities...)

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
		s.pendingActivities = rollbackPendingActivities
		delete(s.documentLocks, document.ID)
		return nil, err
	}
	// Emit the one-shot document.created cards after the create commits (task #3). This is deliberately the
	// creation seam, not the edit path: the UpdateID<=1 guard in enqueueDocumentInboxEventsLocked keeps
	// ignoring the seed update, and the replay guard above returns before we reach here — so a re-created
	// document never double-cards. Cards are instant (available now + inbox doorbell). Emission is best-effort
	// and never fails the create: the document is already durably committed, so a card/doorbell failure is
	// logged and the affected agent simply falls back to seeing the doc's muted version gap — returning an
	// error here would lie to the caller about a durable success and, via the replay guard, never re-card on
	// retry. The create HANDLER drains the doorbell via publishAgentInboxChanges.
	s.enqueueDocumentCreatedInboxEventsLocked(s.db, document, meta)
	created := cloneDocument(document)
	return created, nil
}

func (s *Store) CreateAgent(req CreateAgentRequest, meta OperationMeta) (*Agent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	documents := make([]*Document, 0, len(s.state.ContentDocuments))
	for _, document := range s.state.ContentDocuments {
		if document != nil {
			documents = append(documents, cloneDocument(document))
		}
	}
	s.mu.RUnlock()
	daemonID := strings.TrimSpace(req.DaemonID)
	kind, err := normalizeAgentRuntimeKind(req.Kind)
	if err != nil {
		return nil, err
	}
	agent, err := buildAgent(req.Handle, req.Name, req.Role, kind)
	if err != nil {
		return nil, err
	}
	agent.ID = uuid.NewString()
	agent.WorkspaceRoot = "agents/" + agent.ID
	agent.Model = strings.TrimSpace(req.Model)
	agent.ReasoningEffort = strings.TrimSpace(req.ReasoningEffort)
	now := time.Now().UTC()
	agent.UpdatedAt = now

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	daemons, err := listDaemons(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	var daemon *Daemon
	if daemonID == "" {
		for _, candidate := range daemons {
			if candidate == nil || candidate.Status != "active" || !candidate.DeletedAt.IsZero() {
				continue
			}
			if daemon != nil {
				return nil, errors.New("daemon id is required")
			}
			daemon = candidate
		}
		if daemon != nil {
			daemonID = daemon.ID
		}
	} else {
		for _, candidate := range daemons {
			if candidate != nil && candidate.ID == daemonID {
				daemon = candidate
				break
			}
		}
	}
	if daemonID == "" {
		return nil, errors.New("daemon id is required")
	}
	if daemon == nil || daemon.Status != "active" || !daemon.DeletedAt.IsZero() {
		return nil, ErrNotFound
	}
	if err := validateDaemonRuntimeKind(daemon, kind); err != nil {
		return nil, err
	}
	if err := validateAgentModelProfile(daemon, kind, agent.Model, agent.ReasoningEffort); err != nil {
		return nil, err
	}
	agent.DaemonID = daemonID
	if err := ensureWorkspaceHandleAvailableTx(tx, workspaceID, agent.Handle); err != nil {
		return nil, err
	}
	if err := insertAgentPostgres(tx, workspaceID, agent); err != nil {
		return nil, err
	}
	for _, document := range documents {
		view := &AgentDocumentView{
			AgentID:     agent.ID,
			DocumentID:  document.ID,
			UpdateID:    document.UpdateID,
			StateVector: document.StateVector,
			ViewedAt:    agent.UpdatedAt,
		}
		if err := upsertAgentDocumentViewPostgres(tx, workspaceID, view); err != nil {
			return nil, err
		}
	}
	activity := &ActivityEvent{
		Type:       "agent.created",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s created agent @%s", meta.ActorID, agent.Handle),
		OccurredAt: now,
		Provenance: meta,
	}
	if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	s.recordActivityCreated(activity)
	return cloneAgent(agent), nil
}

func (s *Store) UpdateAgent(id string, req UpdateAgentRequest, meta OperationMeta) (*Agent, error) {
	workspaceID := s.workspaceID
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	agent, err := getAgentForUpdatePostgres(tx, workspaceID, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		agent.Name = name
	}
	if role := strings.TrimSpace(req.Role); role != "" {
		agent.Role = role
	}
	agent.SystemPrompt = sharedAgentSystemPrompt(agent)
	now := time.Now().UTC()
	agent.UpdatedAt = now
	if err := upsertAgentPostgresTx(tx, workspaceID, agent); err != nil {
		return nil, err
	}
	activity := &ActivityEvent{
		Type:       "agent.updated",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s updated agent @%s", meta.ActorID, agent.Handle),
		OccurredAt: now,
		Provenance: meta,
	}
	if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	s.recordActivityCreated(activity)
	return cloneAgent(agent), nil
}

func (s *Store) DeleteAgent(id string, meta OperationMeta) (*Agent, error) {
	workspaceID := s.workspaceID
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	agent, err := getAgentForUpdatePostgres(tx, workspaceID, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if agent.CurrentRunID != "" {
		if run, err := getAgentRunForUpdatePostgres(tx, workspaceID, agent.CurrentRunID); err == nil && !isTerminalRunStatus(run.Status) {
			return nil, errors.New("stop the active run before deleting this agent")
		} else if err != nil && err != ErrNotFound {
			return nil, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM thread_participants WHERE workspace_id = $1::uuid AND participant_id = $2::uuid`, workspaceID, agent.ID); err != nil {
		return nil, err
	}
	result, err := tx.Exec(`DELETE FROM agents WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspaceID, agent.ID)
	if err != nil {
		return nil, err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if rows != 1 {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	activity := &ActivityEvent{
		Type:       "agent.deleted",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s deleted agent @%s", meta.ActorID, agent.Handle),
		OccurredAt: now,
		Provenance: meta,
	}
	if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	s.recordActivityCreated(activity)
	return cloneAgent(agent), nil
}

func (s *Store) StartAgentRun(req StartAgentRunRequest, meta OperationMeta) (*Agent, *AgentRun, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, nil, errors.New("prompt is required")
	}

	workspaceID := s.workspaceID
	agentID := strings.TrimSpace(req.AgentID)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	agent, err := getAgentForUpdatePostgres(tx, workspaceID, agentID)
	if err != nil {
		return nil, nil, err
	}
	if agent.CurrentRunID != "" {
		if active, err := getAgentRunForUpdatePostgres(tx, workspaceID, agent.CurrentRunID); err == nil && !isTerminalRunStatus(active.Status) {
			return nil, nil, fmt.Errorf("agent %s already has an active run", agent.Name)
		} else if err != nil && err != ErrNotFound {
			return nil, nil, err
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
		WorkspaceID:     workspaceID,
		WorkspaceRoot:   agent.WorkspaceRoot,
		WorkingDir:      ".",
		Prompt:          prompt,
		Status:          "queued",
		DesiredStatus:   "running",
		LogTail:         []string{},
		AssignedTaskRef: strings.TrimSpace(req.AssignedTaskRef),
		UpdatedAt:       now,
	}
	agent.Status = "queued"
	agent.CurrentRunID = run.ID
	agent.CurrentTask = summarizePrompt(prompt)
	agent.CurrentActivity = "Queued in daemon"
	agent.UpdatedAt = now
	if err := insertAgentRunPostgresTx(tx, workspaceID, run); err != nil {
		return nil, nil, err
	}
	if err := upsertAgentPostgresTx(tx, workspaceID, agent); err != nil {
		return nil, nil, err
	}
	activity := &ActivityEvent{
		Type:       "agent.run.created",
		ActorID:    agent.ID,
		ActorType:  "agent",
		Summary:    fmt.Sprintf("%s queued %s run", meta.ActorID, agent.Name),
		OccurredAt: now,
		Provenance: meta,
	}
	if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true
	s.recordActivityCreated(activity)
	return cloneAgent(agent), cloneAgentRun(run), nil
}

func (s *Store) UpdateAgentRun(id string, req UpdateAgentRunRequest, meta OperationMeta) (*AgentRun, *Agent, error) {
	workspaceID := s.workspaceID
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	run, err := getAgentRunForUpdatePostgres(tx, workspaceID, strings.TrimSpace(id))
	if err != nil {
		return nil, nil, err
	}
	agent, err := getAgentForUpdatePostgres(tx, workspaceID, run.AgentID)
	if err != nil {
		return nil, nil, err
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

	if err := updateAgentRunPostgresTx(tx, workspaceID, run); err != nil {
		return nil, nil, err
	}
	if err := upsertAgentPostgresTx(tx, workspaceID, agent); err != nil {
		return nil, nil, err
	}
	activity := &ActivityEvent{
		Type:       "agent.run.updated",
		ActorID:    agent.ID,
		ActorType:  "agent",
		Summary:    fmt.Sprintf("%s is %s", agent.Name, run.Status),
		OccurredAt: now,
		Provenance: meta,
	}
	if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true
	s.recordActivityCreated(activity)
	return cloneAgentRun(run), cloneAgent(agent), nil
}

func (s *Store) StopAgentRun(id string, meta OperationMeta) (*AgentRun, error) {
	workspaceID := s.workspaceID
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	run, err := getAgentRunForUpdatePostgres(tx, workspaceID, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	agent, err := getAgentForUpdatePostgres(tx, workspaceID, run.AgentID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	run.DesiredStatus = "stopped"
	run.UpdatedAt = now
	if agent != nil {
		agent.Status = "stopping"
		agent.CurrentActivity = "Waiting for daemon stop"
		agent.UpdatedAt = run.UpdatedAt
	}
	if err := updateAgentRunPostgresTx(tx, workspaceID, run); err != nil {
		return nil, err
	}
	if agent != nil {
		if err := upsertAgentPostgresTx(tx, workspaceID, agent); err != nil {
			return nil, err
		}
	}
	activity := &ActivityEvent{
		Type:       "agent.run.stop_requested",
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s requested stop for %s", meta.ActorID, run.AgentName),
		OccurredAt: run.UpdatedAt,
		Provenance: meta,
	}
	if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	s.recordActivityCreated(activity)
	return cloneAgentRun(run), nil
}

func (s *Store) UpdateAgentSession(id string, req UpdateAgentSessionRequest, meta OperationMeta) (*Agent, error) {
	workspaceID := s.workspaceID
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	agent, err := getAgentForUpdatePostgres(tx, workspaceID, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// Capture the prior semantic session fields so a heartbeat-only update (only
	// LastHeartbeatAt advancing) produces no activity row (see below).
	priorStatus := agent.Status
	priorSessionID := agent.SessionID
	priorTurnID := agent.CurrentTurnID
	priorActivity := agent.CurrentActivity
	nextStatus := strings.TrimSpace(req.Status)
	statusProvided := nextStatus != ""
	if statusProvided {
		switch nextStatus {
		case "idle", "working", "disconnected", "failed", "stalled":
			agent.Status = nextStatus
		default:
			return nil, fmt.Errorf("unsupported agent status %q", nextStatus)
		}
	}
	// statusChanged is a REAL transition — status supplied AND different from before.
	// A repeated same-status update (e.g. the 60s stalled heartbeat re-sending
	// Status:"stalled") is telemetry, not a transition, and must preserve turn +
	// activity exactly like a heartbeat-only update (blocker 20).
	statusChanged := statusProvided && nextStatus != priorStatus
	if sessionID := strings.TrimSpace(req.SessionID); sessionID != "" {
		agent.SessionID = sessionID
	}
	// Turn: an explicit turn always wins; otherwise only a real transition
	// (non-working) clears it. A heartbeat / same-status update preserves the turn.
	if turnID := strings.TrimSpace(req.CurrentTurnID); turnID != "" {
		agent.CurrentTurnID = turnID
	} else if statusChanged && agent.Status != "working" {
		agent.CurrentTurnID = ""
	}
	// Activity: an explicit activity always wins; otherwise default ONLY on a real
	// transition. A heartbeat / same-status update PRESERVES the existing activity —
	// defaulting it would rewrite a persisted stalled diagnostic to literal "Stalled"
	// and lose the human-facing detail (blockers 15/20).
	if activity := strings.TrimSpace(req.CurrentActivity); activity != "" {
		agent.CurrentActivity = activity
	} else if statusChanged {
		switch agent.Status {
		case "idle":
			agent.CurrentActivity = "Idle"
		case "working":
			agent.CurrentActivity = "Working"
		case "disconnected":
			agent.CurrentActivity = "Disconnected"
		case "failed":
			agent.CurrentActivity = "Failed"
		case "stalled":
			agent.CurrentActivity = "Stalled"
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
	updated := cloneAgent(agent)
	if err := upsertAgentPostgresTx(tx, workspaceID, updated); err != nil {
		return nil, err
	}
	// Emit an activity row ONLY when a semantic session field actually changed. A
	// heartbeat-only update (LastHeartbeatAt advanced, status/session/turn/activity
	// unchanged — the daemon's 60s liveness republish) is telemetry, not a human
	// activity; activities are never trimmed, so persisting one per working agent
	// per minute would grow the table unboundedly and flood the latest window.
	var activity *ActivityEvent
	if updated.Status != priorStatus || updated.SessionID != priorSessionID || updated.CurrentTurnID != priorTurnID || updated.CurrentActivity != priorActivity {
		activity = &ActivityEvent{
			Type:       "agent.session.updated",
			ActorID:    meta.ActorID,
			ActorType:  meta.ActorType,
			Summary:    fmt.Sprintf("%s session is %s", updated.Name, updated.Status),
			OccurredAt: now,
			Provenance: meta,
		}
		if err := insertActivityPostgres(tx, workspaceID, activity); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	if activity != nil {
		s.recordActivityCreated(activity)
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
	s.mu.RLock()
	document, ok := s.state.ContentDocuments[req.DocumentID]
	if !ok || document.Hidden {
		s.mu.RUnlock()
		return nil, nil, false, ErrNotFound
	}
	documentSnapshot := cloneDocument(document)
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()

	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, nil, false, errors.New("thread body is required")
	}
	author, err := resolvePrincipalPostgres(db, workspaceID, meta.ActorID, meta.ActorType)
	if err != nil {
		return nil, nil, false, err
	}
	clientOperationID := strings.TrimSpace(req.ClientOperationID)
	now := time.Now().UTC()
	anchor, err := buildThreadAnchorFromRequest(req)
	if err != nil {
		return nil, nil, false, err
	}
	mentionedIDs, err := extractMentionPrincipalIDsPostgres(db, workspaceID, body)
	if err != nil {
		return nil, nil, false, err
	}
	participantIDs := []string{author.ID}
	for _, mid := range mentionedIDs {
		if mid != "" && !containsText(participantIDs, mid) {
			participantIDs = append(participantIDs, mid)
		}
	}
	sort.Strings(participantIDs)
	thread := &Thread{
		ID:                uuid.NewString(),
		DocumentID:        documentSnapshot.ID,
		ClientOperationID: clientOperationID,
		Title:             firstNonEmptyString(strings.TrimSpace(req.Title), inferThreadTitleFromRequest(documentSnapshot, req)),
		Status:            "open",
		Anchor:            anchor,
		CreatedByID:       author.ID,
		CreatedByType:     author.Type,
		CreatedByHandle:   author.Handle,
		CreatedByName:     author.Name,
		ParticipantIDs:    participantIDs,
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
	events, err := collectThreadMentionEventsPostgres(db, workspaceID, thread, message, meta)
	if err != nil {
		return nil, nil, false, err
	}
	// Card the document's subscribers about the new thread (task #6), skipping the author and anyone the
	// mention collect already carded (mention's for_me wins over a general watcher card).
	subscriberSkip := []string{author.ID}
	for _, event := range events {
		subscriberSkip = append(subscriberSkip, event.AgentID)
	}
	subscriberEvents, err := collectThreadCreatedSubscriberEventsPostgres(db, workspaceID, thread, message, meta, subscriberSkip...)
	if err != nil {
		return nil, nil, false, err
	}
	events = append(events, subscriberEvents...)
	activity := &ActivityEvent{
		Type:       "thread.created",
		DocumentID: documentSnapshot.ID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s started a thread on %s", meta.ActorID, documentLabel(documentSnapshot)),
		OccurredAt: now,
		Provenance: meta,
	}
	committed, created, err := createThreadPostgres(db, workspaceID, thread, message, events, activity)
	if err != nil {
		return nil, nil, false, err
	}
	if !created {
		existing, err := findThreadByClientOperationPostgres(db, workspaceID, clientOperationID, author.ID, author.Type)
		if err != nil {
			return nil, nil, false, err
		}
		if existing != nil {
			return existing, firstThreadMessage(existing), false, nil
		}
		return nil, nil, false, errors.New("thread creation conflict")
	}
	// Defer inbox-change broadcasts until after the tx has committed.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range events {
		s.recordAgentInboxChangedLocked(event)
	}
	s.recordActivityCreatedLocked(activity)
	return committed, firstThreadMessage(committed), true, nil
}

func firstThreadMessage(thread *Thread) *ThreadMessage {
	if thread == nil || len(thread.Messages) == 0 {
		return nil
	}
	return thread.Messages[0]
}

// UpdateThreadStatus flips a thread between "open" and "resolved". Both
// directions are allowed (resolve is reversible by design). Re-applying the
// current status is an idempotent success that reports changed=false and
// leaves updated_at alone, so no-op re-resolves neither reorder thread lists
// nor emit broadcasts.
func (s *Store) UpdateThreadStatus(id string, req UpdateThreadRequest) (*Thread, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, false, ErrNotFound
	}
	status := strings.TrimSpace(req.Status)
	if status != "open" && status != "resolved" {
		return nil, false, errors.New(`status must be "open" or "resolved"`)
	}
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()
	return updateThreadStatusPostgres(db, workspaceID, id, status)
}

// UpdateThreadAnchor re-anchors a thread. It reuses the shared anchor validator, preserves the stored
// excerpt when the request omits one, and reports changed=false for a no-op (identical anchor) so the
// handler can skip the broadcast — the same idempotency contract as UpdateThreadStatus.
func (s *Store) UpdateThreadAnchor(id string, req UpdateThreadAnchorRequest) (*Thread, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, false, ErrNotFound
	}
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()
	return updateThreadAnchorPostgres(db, workspaceID, id, req)
}

func (s *Store) ReplyThread(id string, req ReplyThreadRequest, meta OperationMeta) (*Thread, *ThreadMessage, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil, ErrNotFound
	}
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()

	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, nil, errors.New("thread reply is required")
	}
	author, err := resolvePrincipalPostgres(db, workspaceID, meta.ActorID, meta.ActorType)
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
		ThreadID:     id,
		AuthorID:     author.ID,
		AuthorType:   author.Type,
		AuthorHandle: author.Handle,
		AuthorName:   author.Name,
		Body:         body,
		Kind:         kind,
		CreatedAt:    now,
	}
	updatedThread, allEvents, activity, err := replyThreadPostgres(db, workspaceID, id, message, meta)
	if err != nil {
		return nil, nil, err
	}
	// Defer inbox-change broadcasts until after the tx has committed.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range allEvents {
		s.recordAgentInboxChangedLocked(event)
	}
	s.recordActivityCreatedLocked(activity)
	return updatedThread, message, nil
}

func (s *Store) ClaimAgentEvent(req ClaimAgentEventRequest) (*AgentEvent, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	db := s.db
	s.mu.RUnlock()

	agentID, agentHandle, err := resolveAgentIdentityPostgres(db, workspaceID, req.AgentID)
	if err != nil {
		return nil, err
	}
	targetAgent, err := getAgentPostgres(db, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	claimedBy := strings.TrimSpace(req.ClaimedBy)
	targetDaemonID := strings.TrimSpace(targetAgent.DaemonID)
	switch claimedBy {
	case "", "daemon", "system":
		claimedBy = agentID
	case agentID:
	case targetDaemonID:
		if targetDaemonID == "" {
			return nil, errors.New("claimed_by must be the target agent or its daemon")
		}
		ok, err := daemonExistsPostgres(db, workspaceID, targetDaemonID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("claimed_by must be the target agent or its daemon")
		}
	default:
		return nil, errors.New("claimed_by must be the target agent or its daemon")
	}
	event, err := claimAgentEventPostgres(db, workspaceID, agentID, agentHandle, claimedBy)
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
	// UpdateAgentEvent writes to Postgres directly (no persist walk), so its
	// activity is inserted directly too; document_id references the existing
	// event's document, so the FK is already satisfied.
	activity := &ActivityEvent{
		Type:       "agent.event.updated",
		DocumentID: updated.DocumentID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s marked %s %s", meta.ActorID, updated.Type, updated.Status),
		OccurredAt: updated.UpdatedAt,
		Provenance: meta,
	}
	if err := insertActivityPostgres(s.db, workspaceID, activity); err != nil {
		return nil, err
	}
	s.recordActivityCreated(activity)
	return updated, nil
}

func (s *Store) UpsertPresence(req UpsertPresenceRequest) (*Presence, error) {
	s.mu.RLock()
	workspaceID := s.state.WorkspaceID
	s.mu.RUnlock()

	now := time.Now().UTC()
	selection := append([]int(nil), req.Selection...)
	if selection == nil {
		selection = []int{}
	}
	presence := &Presence{
		ActorID:    req.ActorID,
		ActorType:  req.ActorType,
		DocumentID: req.DocumentID,
		FilePath:   req.FilePath,
		Mode:       req.Mode,
		Selection:  selection,
		Activity:   req.Activity,
		UpdatedAt:  now,
	}
	if err := upsertPresencePostgres(s.db, workspaceID, presence); err != nil {
		return nil, err
	}
	return presence, nil
}

func (s *Store) persistLocked() error {
	s.ensureMaps()
	return s.persistDocumentMutationPostgresLocked()
}

func (s *Store) persistDocumentMutationLocked() error {
	s.ensureMaps()
	return s.persistDocumentMutationPostgresLocked()
}

// initialClientIDSeed is the CRDT client-id seed assigned to the first document
// in a workspace (its root). Sharing it between nextClientIDSeedLocked and the
// creation-time root seed keeps a root bootstrapped atomically byte-identical to
// one that was seeded lazily.
const initialClientIDSeed uint64 = 1001

func (s *Store) nextClientIDSeedLocked() uint64 {
	next := initialClientIDSeed
	for _, document := range s.state.ContentDocuments {
		if document.ClientIDSeed >= next {
			next = document.ClientIDSeed + 1
		}
	}
	return next
}

// appendActivityLocked buffers an activity for insertion in the current
// operation's persist transaction. Activities are Postgres-as-truth: they are
// inserted (never trimmed) and read back with a window at query time, so there
// is no in-memory retention or cap here.
func (s *Store) appendActivityLocked(event *ActivityEvent) {
	s.pendingActivities = append(s.pendingActivities, event)
}

func (s *Store) recordActivityCreated(activity *ActivityEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordActivityCreatedLocked(activity)
}

func (s *Store) recordActivityCreatedLocked(activity *ActivityEvent) {
	if activity == nil {
		return
	}
	s.committedActivities = append(s.committedActivities, cloneActivityEvent(activity))
}

func (s *Store) recordActivitiesCreatedLocked(activities []*ActivityEvent) {
	for _, activity := range activities {
		s.recordActivityCreatedLocked(activity)
	}
}

func cloneActivityEvent(activity *ActivityEvent) *ActivityEvent {
	if activity == nil {
		return nil
	}
	clone := *activity
	return &clone
}

func cloneActivityEvents(activities []*ActivityEvent) []*ActivityEvent {
	if len(activities) == 0 {
		return nil
	}
	clones := make([]*ActivityEvent, 0, len(activities))
	for _, activity := range activities {
		if clone := cloneActivityEvent(activity); clone != nil {
			clones = append(clones, clone)
		}
	}
	return clones
}

func cloneDocumentState(state WorkspaceState) WorkspaceState {
	copyState := state
	copyState.ContentDocuments = map[string]*Document{}
	for key, doc := range state.ContentDocuments {
		copyState.ContentDocuments[key] = cloneDocument(doc)
	}
	copyState.DocumentCheckpoints = map[string]*DocumentCheckpoint{}
	for key, checkpoint := range state.DocumentCheckpoints {
		if checkpoint == nil {
			continue
		}
		clone := *checkpoint
		copyState.DocumentCheckpoints[key] = &clone
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

// validateAgentModelProfile enforces the model / reasoning-effort selection
// contract against the selected runtime's reported model catalog. Capability is
// catalog presence, not a provider switch, so the runtime is selected by the
// normalized request kind (not a hardcoded provider) and the three catalog-state
// error strings are kept provider-agnostic.
//
// Rules:
//   - model=="" && effort=="" : full inheritance; always allowed, even when the
//     runtime reports no catalog (old daemon, or a runtime with none today).
//   - any explicit model/effort requires a present, error-free, non-empty catalog.
//   - explicit model must match a projected model; an explicit effort must be one
//     of that model's reasoning efforts (empty effort inherits the model's).
//   - model=="" && explicit effort uses provider-wide efforts when advertised,
//     otherwise it resolves the single visible default model. The
//     partial-inheritance state {model:"", effort} is persisted unchanged by the
//     caller (never canonicalized).
//
// It never mutates the agent; it only accepts or rejects, failing closed.
func validateAgentModelProfile(daemon *Daemon, kind, model, effort string) error {
	if model == "" && effort == "" {
		return nil
	}
	if daemon == nil {
		return ErrNotFound
	}
	var catalog *RuntimeModelCatalog
	found := false
	for i := range daemon.Runtimes {
		runtimeKind, err := normalizeAgentRuntimeKind(daemon.Runtimes[i].Kind)
		if err != nil || runtimeKind != kind {
			continue
		}
		found = true
		catalog = daemon.Runtimes[i].ModelCatalog
		break
	}
	if !found {
		return fmt.Errorf("runtime %q is not reported by daemon %q", kind, daemon.ID)
	}
	if catalog == nil {
		return fmt.Errorf("daemon %q has not reported a model catalog", daemon.ID)
	}
	if strings.TrimSpace(catalog.Error) != "" {
		return fmt.Errorf("model catalog discovery failed on daemon %q", daemon.ID)
	}
	if len(catalog.Models) == 0 {
		return fmt.Errorf("no models available on daemon %q", daemon.ID)
	}
	if model != "" {
		var selected *RuntimeModel
		for i := range catalog.Models {
			if catalog.Models[i].Model == model {
				selected = &catalog.Models[i]
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("model %q is not available on daemon %q", model, daemon.ID)
		}
		efforts := selected.ReasoningEfforts
		if len(efforts) == 0 {
			efforts = catalog.ReasoningEfforts
		}
		if effort != "" && !reasoningEffortSupported(efforts, effort) {
			return fmt.Errorf("reasoning effort %q is not available for model %q on daemon %q", effort, model, daemon.ID)
		}
		return nil
	}
	if len(catalog.ReasoningEfforts) > 0 {
		if reasoningEffortSupported(catalog.ReasoningEfforts, effort) {
			return nil
		}
		return fmt.Errorf("reasoning effort %q is not available for the runtime default on daemon %q", effort, daemon.ID)
	}
	// model=="" && effort!="" : resolve the single visible default model.
	var defaultModel *RuntimeModel
	for i := range catalog.Models {
		if !catalog.Models[i].IsDefault {
			continue
		}
		if defaultModel != nil {
			return fmt.Errorf("daemon %q reports more than one default model; reasoning effort for the default cannot be resolved", daemon.ID)
		}
		defaultModel = &catalog.Models[i]
	}
	if defaultModel == nil {
		return fmt.Errorf("daemon %q reports no default model; reasoning effort for the default cannot be resolved", daemon.ID)
	}
	if !reasoningEffortSupported(defaultModel.ReasoningEfforts, effort) {
		return fmt.Errorf("reasoning effort %q is not available for the default model on daemon %q", effort, daemon.ID)
	}
	return nil
}

func reasoningEffortSupported(efforts []string, effort string) bool {
	for _, candidate := range efforts {
		if candidate == effort {
			return true
		}
	}
	return false
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
Use notty-agent-tool list-inbox to see everything pending across all boxes; add --box for-me, --box general, or --box muted to filter to one. Use get-inbox-item, complete-inbox-item, dismiss-inbox-item, diff-document, and mark-document-viewed when needed.
Document updates are MUTED by default: they are never pushed and do not appear in for-me or general — they wait in the muted box. Your wake prompt shows only a COUNT of muted items ("N items in the muted inbox"); run notty-agent-tool list-inbox --box muted to review them on demand. To be actively notified of a specific document's edits, subscribe to it: notty-agent-tool subscribe-document --document-id <id> (and unsubscribe-document / list-subscriptions to manage). A subscribed document's edits arrive in your general inbox shortly after editing pauses; without a subscription you will not be interrupted by document changes.
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

func (s *Store) refreshAgentSystemPromptsLocked() error {
	agents, err := listAgentsPostgres(s.db, s.state.WorkspaceID)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		next := sharedAgentSystemPrompt(agent)
		if agent.SystemPrompt != next {
			if _, err := s.db.Exec(
				`UPDATE agents
				    SET system_prompt = $1,
				        updated_at = $2
				  WHERE workspace_id = $3::uuid
				    AND id = $4::uuid`,
				next,
				time.Now().UTC(),
				s.state.WorkspaceID,
				agent.ID,
			); err != nil {
				return err
			}
		}
	}
	return nil
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

const (
	daemonOnlineWindow = 30 * time.Second
	daemonStaleWindow  = 2 * time.Minute
)

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

func cloneAgentEvent(event *AgentEvent) *AgentEvent {
	if event == nil {
		return nil
	}
	clone := *event
	return &clone
}

func buildThreadAnchorFromRequest(req CreateThreadRequest) (ThreadAnchor, error) {
	return buildThreadAnchor(req.Kind, req.RelativeStart, req.RelativeEnd, req.Excerpt, req.StateAtAnchor)
}

// buildThreadAnchor is the single anchor validator shared by thread creation and re-anchoring, so the
// two paths cannot drift: kind inference, the both-or-neither relative-position rule, excerpt truncation,
// and state-vector validation are defined exactly once.
func buildThreadAnchor(kindRaw, relativeStartRaw, relativeEndRaw, excerptRaw, stateVectorRaw string) (ThreadAnchor, error) {
	relativeStart := strings.TrimSpace(relativeStartRaw)
	relativeEnd := strings.TrimSpace(relativeEndRaw)
	if (relativeStart == "") != (relativeEnd == "") {
		return ThreadAnchor{}, errors.New("relativeStart and relativeEnd must be provided together")
	}
	excerpt := truncateText(strings.TrimSpace(excerptRaw), 140)
	// The state vector is opaque base64 the backend never interprets, but it must be well-formed and
	// bounded before it is stored verbatim: decodable base64, ≤64KB.
	stateVector := strings.TrimSpace(stateVectorRaw)
	if stateVector != "" {
		if len(stateVector) > 64*1024 {
			return ThreadAnchor{}, errors.New("anchor state vector exceeds the 64KB limit")
		}
		if _, err := base64.StdEncoding.DecodeString(stateVector); err != nil {
			return ThreadAnchor{}, errors.New("anchor state vector must be base64-encoded")
		}
	}
	kind := strings.TrimSpace(kindRaw)
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
		// No range means no frontier: a document-kind anchor has no state vector to capture.
		if stateVector != "" {
			return ThreadAnchor{}, errors.New("document threads cannot include an anchor state vector")
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
	anchor.StateAtAnchor = stateVector
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

// enqueueDocumentCreatedInboxEventsLocked pushes a one-shot `document.created` card to every workspace agent
// except the creator when a new document is created (task #3, AlphaToad ruling). Where edits are muted
// unless an agent explicitly subscribed, creation is a rare, high-signal event worth a general-box card for
// workspace awareness. Delivery is INSTANT (AlphaToad's ruling): the card is immediately available
// (available_at = now) AND rings the agent.inbox.changed doorbell, so a creation wakes its recipients right
// away. This is the deliberate exception — recurring document.updated edits still ride poll+maturity with no
// doorbell, so the per-keystroke class this channel killed stays dead. A directory-push burst wakes every
// agent immediately; the daemon's busy-path one-slot follow-up collapses it into bounded turns per agent (and
// the wake prompt caps the general section at top-3 + overflow), so the burst never becomes N turns.
//
// Self-exclusion is the same `shouldNotifyAgentPostgres` actor check the thread paths use, covering human
// AND agent creators. Hidden documents never notify. Idempotency is twofold: CreateDocument's replay guard
// returns early before this runs, and the dedup key (`document-created:<docID>:<agentID>`) makes even a
// direct re-emit a no-op — so exactly one card per eligible agent, zero on replay. ToUpdateID carries the
// created version so completing the card advances the agent's document watermark (clearing its muted gap).
//
// Best-effort by contract: the document is already committed before this runs, so a card write MUST NOT fail
// the create. Every failure is logged (visibly, with doc + agent ids) and skipped per-agent — one bad row
// never un-cards the others, and any un-carded agent degrades to exactly the pre-#108 behavior (it still
// sees the document's version gap in its MUTED box, never silence). Returns whatever it managed to emit.
func (s *Store) enqueueDocumentCreatedInboxEventsLocked(q querier, document *Document, meta OperationMeta) []*AgentEvent {
	if document == nil || document.Hidden || document.UpdateID <= 0 {
		return nil
	}
	agents, err := listAgentsPostgres(q, s.state.WorkspaceID)
	if err != nil {
		// Can't enumerate recipients — degrade every agent to the muted-gap fallback, don't fail the create.
		log.Printf("document.created: listing agents for document %s failed, no cards emitted: %v", document.ID, err)
		return nil
	}
	now := time.Now().UTC()
	var events []*AgentEvent
	for _, agent := range agents {
		if agent == nil {
			continue
		}
		notify, err := shouldNotifyAgentPostgres(q, s.state.WorkspaceID, agent.ID, meta, "")
		if err != nil {
			log.Printf("document.created: notify check for agent %s on document %s failed, skipping: %v", agent.ID, document.ID, err)
			continue
		}
		if !notify {
			// The creator does not card themselves (human or agent actor).
			continue
		}
		event := &AgentEvent{
			ID:           uuid.NewString(),
			AgentID:      agent.ID,
			AgentHandle:  agent.Handle,
			Type:         "document.created",
			Box:          "general",
			Status:       "pending",
			DocumentID:   document.ID,
			FromUpdateID: 0,
			ToUpdateID:   document.UpdateID,
			Summary:      fmt.Sprintf("new %s created", documentLabel(document)),
			Prompt:       fmt.Sprintf("A new %s was created. Read it with notty-agent-tool get-document-by-path, or diff-document --document-id %s. Act only if you have useful feedback or edits.", documentLabel(document), document.ID),
			DedupKey:     fmt.Sprintf("document-created:%s:%s", document.ID, agent.ID),
			// Instant delivery: available immediately (no quiescence — creation is one-shot, nothing slides).
			AvailableAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		upserted, err := upsertDocumentInboxEventPostgres(q, s.state.WorkspaceID, event)
		if err != nil {
			log.Printf("document.created: writing card for agent %s on document %s failed, skipping: %v", agent.ID, document.ID, err)
			continue
		}
		if upserted != nil {
			events = append(events, upserted)
			// Instant wake: ring the inbox doorbell for this agent. Best-effort like the card write —
			// recordAgentInboxChangedLocked logs internally and never fails the committed create.
			s.recordAgentInboxChangedLocked(upserted)
		}
	}
	return events
}

func (s *Store) enqueueDocumentInboxEventsLocked(q querier, document *Document, meta OperationMeta) ([]*AgentEvent, error) {
	if document == nil || document.Hidden || document.UpdateID <= 1 {
		return nil, nil
	}
	// Muted-by-default routing (task #2): a document edit pushes ONLY to agents that explicitly subscribed
	// to this document. Non-subscribers get nothing — no row, no doorbell — and answer "what changed?" via
	// the on-demand watermark walk instead. This replaces the old all-agents fan-out that woke everyone on
	// every keystroke.
	subscriberIDs, err := listDocumentSubscriberAgentIDsPostgres(q, s.state.WorkspaceID, document.ID)
	if err != nil {
		return nil, err
	}
	var events []*AgentEvent
	for _, agentID := range subscriberIDs {
		agent, err := getAgentPostgres(q, s.state.WorkspaceID, agentID)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, err
		}
		notify, err := shouldNotifyAgentPostgres(q, s.state.WorkspaceID, agent.ID, meta, "")
		if err != nil {
			return nil, err
		}
		if !notify {
			continue
		}
		fromUpdateID := int64(0)
		view, err := getAgentDocumentViewPostgres(q, s.state.WorkspaceID, agent.ID, document.ID)
		if err != nil && err != ErrNotFound {
			return nil, err
		}
		if view != nil {
			fromUpdateID = view.UpdateID
		}
		if fromUpdateID >= document.UpdateID {
			continue
		}
		// Subscribed document updates always land in `general`; the old thread-proximity for_me promotion
		// died with the auto-subscription idea — thread.mentioned/replied are now the only for_me sources.
		event, err := s.upsertDocumentInboxEventLocked(q, agent, document, "general", "", "", fromUpdateID, document.UpdateID)
		if err != nil {
			return nil, err
		}
		if event != nil {
			events = append(events, event)
		}
	}
	return events, nil
}

func (s *Store) documentInboxTargetLocked(q querier, agentID string, document *Document) (box string, threadID string, threadTitle string, err error) {
	if document == nil {
		return "general", "", "", nil
	}
	found, tid, title, err := documentHasOpenThreadForParticipantPostgres(q, s.state.WorkspaceID, document.ID, agentID)
	if err != nil {
		return "", "", "", err
	}
	if !found {
		return "general", "", "", nil
	}
	return "for_me", tid, title, nil
}

func (s *Store) upsertDocumentInboxEventLocked(q querier, agent *Agent, document *Document, box string, threadID string, threadTitle string, fromUpdateID int64, toUpdateID int64) (*AgentEvent, error) {
	if agent == nil || document == nil || toUpdateID <= 0 {
		return nil, nil
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
		// Quiescence: the card matures documentQuiescenceWindow after this edit. The upsert's
		// DO UPDATE SET available_at = EXCLUDED.available_at slides it forward on every keystroke batch,
		// so a subscriber gets one delivery documentQuiescenceWindow after typing STOPS, not one per batch.
		AvailableAt: now.Add(documentQuiescenceWindow),
	}
	upserted, err := upsertDocumentInboxEventPostgres(q, s.state.WorkspaceID, event)
	if err != nil {
		return nil, err
	}
	return upserted, nil
}

func collectThreadMentionEventsPostgres(q querier, workspaceID string, thread *Thread, message *ThreadMessage, meta OperationMeta) ([]*AgentEvent, error) {
	if thread == nil || message == nil {
		return nil, nil
	}
	var events []*AgentEvent
	mentionedIDs, err := extractMentionPrincipalIDsPostgres(q, workspaceID, message.Body)
	if err != nil {
		return nil, err
	}
	for _, principalID := range mentionedIDs {
		agent, err := getAgentPostgres(q, workspaceID, principalID)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, err
		}
		notify, err := shouldNotifyAgentPostgres(q, workspaceID, agent.ID, meta, message.AuthorID)
		if err != nil {
			return nil, err
		}
		if !notify {
			continue
		}
		now := time.Now().UTC()
		dedupKey := fmt.Sprintf("thread-mentioned:%s:%s", message.ID, agent.ID)
		events = append(events, &AgentEvent{
			ID:              uuid.NewString(),
			AgentID:         agent.ID,
			AgentHandle:     agent.Handle,
			Type:            "thread.mentioned",
			Box:             "for_me",
			Status:          "pending",
			DocumentID:      thread.DocumentID,
			ThreadID:        thread.ID,
			ThreadMessageID: message.ID,
			Summary:         fmt.Sprintf("@%s was mentioned in thread %s", agent.Handle, thread.Title),
			Prompt:          fmt.Sprintf("You were mentioned by @%s in thread %q: %s", message.AuthorHandle, thread.Title, truncateText(message.Body, 240)),
			DedupKey:        dedupKey,
			CreatedAt:       now,
			UpdatedAt:       now,
			AvailableAt:     now,
		})
	}
	return events, nil
}

// collectThreadCreatedSubscriberEventsPostgres cards each document SUBSCRIBER when a new thread is created on
// the doc (task #6, AlphaToad ruling): a new thread is a one-shot fact, so it rides the instant class —
// general box, available_at = now, one doorbell per carded agent (the caller records it). Recipients are the
// doc's subscribers minus the author (`shouldNotifyAgentPostgres`) minus anyone already carded by the mention
// collect for this message (skipAgentIDs) — mention (for_me) wins over a general watcher card, no double
// card. Type is `thread.created` (a first-class one-shot, NOT a document.* type — so it must not advance the
// doc watermark on completion). Dedup key makes a re-create/retry idempotent.
func collectThreadCreatedSubscriberEventsPostgres(q querier, workspaceID string, thread *Thread, message *ThreadMessage, meta OperationMeta, skipAgentIDs ...string) ([]*AgentEvent, error) {
	if thread == nil || message == nil || thread.DocumentID == "" {
		return nil, nil
	}
	subscriberIDs, err := listDocumentSubscriberAgentIDsPostgres(q, workspaceID, thread.DocumentID)
	if err != nil {
		return nil, err
	}
	var events []*AgentEvent
	for _, agentID := range subscriberIDs {
		if containsText(skipAgentIDs, agentID) {
			continue
		}
		agent, err := getAgentPostgres(q, workspaceID, agentID)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, err
		}
		notify, err := shouldNotifyAgentPostgres(q, workspaceID, agent.ID, meta, message.AuthorID)
		if err != nil {
			return nil, err
		}
		if !notify {
			continue
		}
		now := time.Now().UTC()
		events = append(events, &AgentEvent{
			ID:              uuid.NewString(),
			AgentID:         agent.ID,
			AgentHandle:     agent.Handle,
			Type:            "thread.created",
			Box:             "general",
			Status:          "pending",
			DocumentID:      thread.DocumentID,
			ThreadID:        thread.ID,
			ThreadMessageID: message.ID,
			Summary:         fmt.Sprintf("New thread %q on a document you watch", thread.Title),
			Prompt:          fmt.Sprintf("@%s opened thread %q on a document you subscribe to: %s. Read it with notty-agent-tool get-thread --thread-id %s.", message.AuthorHandle, thread.Title, truncateText(message.Body, 240), thread.ID),
			DedupKey:        fmt.Sprintf("thread-created:%s:%s", thread.ID, agent.ID),
			CreatedAt:       now,
			UpdatedAt:       now,
			AvailableAt:     now,
		})
	}
	return events, nil
}

func collectThreadReplyEventsPostgres(q querier, workspaceID string, thread *Thread, message *ThreadMessage, meta OperationMeta, skipAgentIDs ...string) ([]*AgentEvent, error) {
	if thread == nil || message == nil {
		return nil, nil
	}
	var events []*AgentEvent
	for _, participantID := range thread.ParticipantIDs {
		if containsText(skipAgentIDs, participantID) {
			continue
		}
		agent, err := getAgentPostgres(q, workspaceID, participantID)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, err
		}
		notify, err := shouldNotifyAgentPostgres(q, workspaceID, agent.ID, meta, message.AuthorID)
		if err != nil {
			return nil, err
		}
		if !notify {
			continue
		}
		now := time.Now().UTC()
		dedupKey := fmt.Sprintf("thread-replied:%s:%s", message.ID, agent.ID)
		events = append(events, &AgentEvent{
			ID:              uuid.NewString(),
			AgentID:         agent.ID,
			AgentHandle:     agent.Handle,
			Type:            "thread.replied",
			Box:             "for_me",
			Status:          "pending",
			DocumentID:      thread.DocumentID,
			ThreadID:        thread.ID,
			ThreadMessageID: message.ID,
			Summary:         fmt.Sprintf("New reply in thread %s", thread.Title),
			Prompt:          fmt.Sprintf("A new reply was added in thread %q by @%s: %s", thread.Title, message.AuthorHandle, truncateText(message.Body, 240)),
			DedupKey:        dedupKey,
			CreatedAt:       now,
			UpdatedAt:       now,
			AvailableAt:     now,
		})
	}
	return events, nil
}

// collectThreadReplyWatcherEventsPostgres cards document SUBSCRIBERS who are NOT thread participants when a
// reply lands (task #6). Replies are a RECURRING stream, not one-shot facts, so a watcher card must NOT ring
// a doorbell per reply (that's the storm the muted design killed) — it rides the EDIT class: general box,
// sliding `documentQuiescenceWindow`, upserted per `thread-replied-watch:<threadID>:<agentID>` so a busy
// thread collapses to ONE delivery per watcher, matured after the thread quiets. Version fields stay 0 (a
// thread has no version — the type carries the semantics), and the summary is stateless (get-thread is the
// truth about how much happened). Participants keep their instant for_me `thread.replied` card (they're in
// the conversation); the exclusion is `thread.ParticipantIDs`, which already covers the author + everyone
// mentioned/participating. The caller UPSERTS these (no doorbell), separate from the doorbell-rung events.
func collectThreadReplyWatcherEventsPostgres(q querier, workspaceID string, thread *Thread, message *ThreadMessage, meta OperationMeta) ([]*AgentEvent, error) {
	if thread == nil || message == nil || thread.DocumentID == "" {
		return nil, nil
	}
	subscriberIDs, err := listDocumentSubscriberAgentIDsPostgres(q, workspaceID, thread.DocumentID)
	if err != nil {
		return nil, err
	}
	var events []*AgentEvent
	for _, agentID := range subscriberIDs {
		if containsText(thread.ParticipantIDs, agentID) {
			continue
		}
		agent, err := getAgentPostgres(q, workspaceID, agentID)
		if err != nil {
			if err == ErrNotFound {
				continue
			}
			return nil, err
		}
		notify, err := shouldNotifyAgentPostgres(q, workspaceID, agent.ID, meta, message.AuthorID)
		if err != nil {
			return nil, err
		}
		if !notify {
			continue
		}
		now := time.Now().UTC()
		events = append(events, &AgentEvent{
			ID:          uuid.NewString(),
			AgentID:     agent.ID,
			AgentHandle: agent.Handle,
			Type:        "thread.replied",
			Box:         "general",
			Status:      "pending",
			DocumentID:  thread.DocumentID,
			ThreadID:    thread.ID,
			Summary:     fmt.Sprintf("New replies in thread %q on a document you watch", thread.Title),
			Prompt:      fmt.Sprintf("New activity in thread %q on a document you subscribe to. Read it with notty-agent-tool get-thread --thread-id %s.", thread.Title, thread.ID),
			DedupKey:    fmt.Sprintf("thread-replied-watch:%s:%s", thread.ID, agent.ID),
			CreatedAt:   now,
			UpdatedAt:   now,
			// Coalesce like edits: the upsert slides available_at forward on each reply, so the watcher gets
			// one delivery documentQuiescenceWindow after the thread STOPS, not one per reply. No doorbell.
			AvailableAt: now.Add(documentQuiescenceWindow),
		})
	}
	return events, nil
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
	if agent, err := getAgentPostgres(s.db, s.state.WorkspaceID, event.AgentID); err == nil && agent != nil {
		change.DaemonID = agent.DaemonID
	} else if err != nil && err != ErrNotFound {
		log.Printf("recordAgentInboxChangedLocked: %v", err)
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
