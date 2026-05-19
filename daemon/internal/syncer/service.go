package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	crdt "notty/internal/ycrdt"
)

type workspaceEventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type agentInboxChangedEvent struct {
	WorkspaceID      string `json:"workspaceId"`
	DaemonID         string `json:"daemonId"`
	AgentID          string `json:"agentId"`
	Box              string `json:"box"`
	EventID          string `json:"eventId"`
	NotificationType string `json:"notificationType"`
}

type Service struct {
	cfg             Config
	client          *http.Client
	sessions        *agentSessionSupervisor
	toolServer      *http.Server
	mu              sync.Mutex
	primaryReplica  *workspaceReplica
	agentReplicas   map[string]*managedReplica
	documentSyncs   map[string]*managedDocumentSync
	agentWorkers    map[string]*managedAgentWorker
	latestWorkspace *workspaceResponse
	docCache        *documentCache
	reconcileQueue  *reconcileQueue
}

type managedReplica struct {
	replica *workspaceReplica
	cancel  context.CancelFunc
}

type managedAgentWorker struct {
	cancel context.CancelFunc
	wake   chan struct{}
}

type managedDocumentSync struct {
	sync   *documentSync
	cancel context.CancelFunc
}

type trackedFile struct {
	DocumentID            string
	DocumentPath          string
	Path                  string
	WorkspaceRoot         string
	ActorID               string
	ActorType             string
	Doc                   *crdt.Doc
	docMu                 *sync.Mutex
	cache                 *documentCache
	cacheEntry            *documentCacheEntry
	stateMu               sync.Mutex
	projecting            int
	localDirty            bool
	hash                  projectedContentHash
	projectedContentKnown bool
}

type projectedContentHash struct {
	size int
	sum  uint64
}

var projectedHashSeed = maphash.MakeSeed()
var awarenessClientCounter atomic.Uint64
var documentConnectMu sync.Mutex
var nextDocumentConnect time.Time

const documentConnectInterval = 100 * time.Millisecond
const backendErrorBodyLimit = 4096

type backendStatusError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *backendStatusError) Error() string {
	if e == nil {
		return ""
	}
	message := fmt.Sprintf("%s %s failed with HTTP %d", e.Method, e.URL, e.StatusCode)
	if strings.TrimSpace(e.Body) != "" {
		message += ": " + strings.TrimSpace(e.Body)
	}
	return message
}

type workspaceResponse struct {
	CurrentDaemonID string        `json:"currentDaemonId"`
	Documents       []*document   `json:"documents"`
	Agents          []*agent      `json:"agents"`
	AgentRuns       []*agentRun   `json:"agentRuns"`
	Threads         []*thread     `json:"threads"`
	AgentEvents     []*agentEvent `json:"agentEvents"`
}

type document struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	UpdateID int64  `json:"updateId"`
}

type upsertPresenceRequest struct {
	ActorID   string `json:"actorId"`
	ActorType string `json:"actorType"`
	FilePath  string `json:"filePath"`
	Mode      string `json:"mode"`
	Activity  string `json:"activity"`
}

type createDocumentRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type updateDocumentRequest struct {
	Path string `json:"path"`
}

type postDocumentUpdateResponse struct {
	Accepted bool  `json:"accepted"`
	Applied  bool  `json:"applied"`
	UpdateID int64 `json:"updateId"`
}

func New(cfg Config) (*Service, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	cache, err := newDocumentCache(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	queue := newReconcileQueue()
	primaryReplica, err := newWorkspaceReplica(cfg, cfg.WorkspaceDir, cfg.AgentID, "daemon", queue.Mark)
	if err != nil {
		return nil, err
	}
	primaryReplica.client = client
	primaryReplica.docCache = cache
	service := &Service{
		cfg:            cfg,
		client:         client,
		primaryReplica: primaryReplica,
		agentReplicas:  map[string]*managedReplica{},
		documentSyncs:  map[string]*managedDocumentSync{},
		agentWorkers:   map[string]*managedAgentWorker{},
		docCache:       cache,
		reconcileQueue: queue,
	}
	service.sessions = newAgentSessionSupervisor(cfg, service.updateRemoteAgentSession, nil)
	service.sessions.SetIdleWake(service.wakeAgentWorker)
	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	toolServer, err := s.startToolGateway()
	if err != nil {
		return err
	}
	s.toolServer = toolServer
	if err := os.MkdirAll(s.cfg.WorkspaceDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.AgentWorkspaceRoot, 0o755); err != nil {
		return err
	}
	if err := s.refreshInitialWorkspace(ctx); err != nil {
		s.closeDocumentSyncs()
		s.closeAgentWorkers()
		s.closeAgentReplicas()
		if s.sessions != nil {
			s.sessions.Shutdown()
		}
		_ = shutdownToolGateway(context.Background(), s.toolServer)
		return err
	}
	primaryCtx, cancelPrimary := context.WithCancel(ctx)
	defer cancelPrimary()
	go func() {
		if err := s.primaryReplica.Run(primaryCtx); err != nil && primaryCtx.Err() == nil {
			log.Printf("primary workspace replica error: %v", err)
		}
	}()
	go s.workspaceEventLoop(ctx)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	reconcileTicker := time.NewTicker(2 * time.Second)
	defer reconcileTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancelPrimary()
			s.closeDocumentSyncs()
			s.closeAgentWorkers()
			s.closeAgentReplicas()
			if s.sessions != nil {
				s.sessions.Shutdown()
			}
			_ = shutdownToolGateway(context.Background(), s.toolServer)
			return nil
		case <-reconcileTicker.C:
			if err := s.reconcileDirtyDocuments(ctx); err != nil {
				fmt.Printf("document reconcile error: %v\n", err)
			}
		case <-ticker.C:
			if err := s.reconcileTrackedDocuments(ctx); err != nil {
				fmt.Printf("document reconcile error: %v\n", err)
			}
			if err := s.refresh(ctx); err != nil {
				fmt.Printf("workspace refresh error: %v\n", err)
			}
		}
	}
}

func (s *Service) refreshInitialWorkspace(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	backoff := 500 * time.Millisecond
	var lastErr error
	for {
		if err := s.refresh(ctx); err == nil {
			return nil
		} else {
			lastErr = err
			if isFatalInitializationError(err) {
				fmt.Printf("initial refresh fatal error: %v\n", err)
				return err
			}
			fmt.Printf("initial refresh error: %v; retrying\n", err)
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func isFatalInitializationError(err error) bool {
	var agentErr *agentSessionStartupError
	if errors.As(err, &agentErr) {
		return true
	}
	var statusErr *backendStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden
	}
	return false
}

func (s *Service) workspaceEventLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.runWorkspaceEventStream(ctx); err != nil && ctx.Err() == nil {
			log.Printf("workspace event stream error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (s *Service) runWorkspaceEventStream(ctx context.Context) error {
	conn, _, err := dialWorkspaceWebsocket(ctx, s.cfg, "/ws", nil, "")
	if err != nil {
		return err
	}
	defer conn.Close()

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var event workspaceEventEnvelope
		if err := json.Unmarshal(payload, &event); err != nil {
			continue
		}
		if shouldRefreshForEvent(event.Type) {
			if err := s.refresh(ctx); err != nil && ctx.Err() == nil {
				log.Printf("workspace refresh error: %v", err)
			}
		}
		if change, ok := parseAgentInboxChangedEvent(event); ok {
			s.wakeAgentWorker(change.AgentID)
			continue
		}
		if shouldWakeAgentWorkersForEvent(event.Type) {
			s.wakeAllAgentWorkers()
		}
	}
}

func parseAgentInboxChangedEvent(event workspaceEventEnvelope) (agentInboxChangedEvent, bool) {
	if strings.TrimSpace(event.Type) != "agent.inbox.changed" || len(event.Data) == 0 {
		return agentInboxChangedEvent{}, false
	}
	var change agentInboxChangedEvent
	if err := json.Unmarshal(event.Data, &change); err != nil {
		return agentInboxChangedEvent{}, false
	}
	if strings.TrimSpace(change.AgentID) == "" {
		return agentInboxChangedEvent{}, false
	}
	return change, true
}

func shouldWakeAgentWorkersForEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "workspace.snapshot", "presence.updated", "agent.run.updated", "document.updated", "thread.created", "thread.updated", "thread.message.created", "agent.event.updated":
		return false
	default:
		return true
	}
}

func shouldRefreshForEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "workspace.snapshot", "presence.updated", "document.updated", "agent.run.updated":
		return false
	default:
		return true
	}
}

func (s *Service) refresh(ctx context.Context) error {
	workspace, err := s.fetchWorkspace(ctx)
	if err != nil {
		return err
	}

	if s.primaryReplica != nil {
		if err := s.primaryReplica.applyWorkspace(ctx, workspace); err != nil {
			return err
		}
	}
	if err := s.reconcileDocumentSyncs(ctx, workspace.Documents); err != nil {
		return err
	}
	s.mu.Lock()
	s.latestWorkspace = workspace
	s.mu.Unlock()
	if err := s.reconcileAgentReplicas(ctx, workspace); err != nil {
		return err
	}
	if err := s.reconcileAgentWorkers(ctx, workspace.Agents); err != nil {
		return err
	}
	if s.sessions != nil {
		if err := s.sessions.Reconcile(ctx, workspace.Agents); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) fetchWorkspace(ctx context.Context) (*workspaceResponse, error) {
	req, err := s.newBackendRequest(ctx, http.MethodGet, "/api/workspace", nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return nil, &backendStatusError{
			Method:     req.Method,
			URL:        req.URL.String(),
			StatusCode: res.StatusCode,
			Body:       string(body),
		}
	}

	var workspace workspaceResponse
	if err := json.NewDecoder(res.Body).Decode(&workspace); err != nil {
		return nil, err
	}
	return &workspace, nil
}

func (s *Service) reconcileAgentWorkers(ctx context.Context, agents []*agent) error {
	desired := make(map[string]struct{}, len(agents))
	skills := agentSkillExecutor{service: s}

	s.mu.Lock()
	for _, currentAgent := range agents {
		if currentAgent == nil {
			continue
		}
		desired[currentAgent.ID] = struct{}{}
		if _, ok := s.agentWorkers[currentAgent.ID]; ok {
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		worker := &managedAgentWorker{cancel: cancel, wake: make(chan struct{}, 1)}
		s.agentWorkers[currentAgent.ID] = worker
		go s.agentWorkerLoop(workerCtx, skills, currentAgent.ID)
		select {
		case worker.wake <- struct{}{}:
		default:
		}
	}

	staleIDs := make([]string, 0)
	for agentID := range s.agentWorkers {
		if _, ok := desired[agentID]; !ok {
			staleIDs = append(staleIDs, agentID)
		}
	}
	stale := make([]*managedAgentWorker, 0, len(staleIDs))
	for _, agentID := range staleIDs {
		stale = append(stale, s.agentWorkers[agentID])
		delete(s.agentWorkers, agentID)
	}
	s.mu.Unlock()

	for _, worker := range stale {
		worker.cancel()
	}
	return nil
}

func (s *Service) closeAgentWorkers() {
	s.mu.Lock()
	workers := make([]*managedAgentWorker, 0, len(s.agentWorkers))
	for _, worker := range s.agentWorkers {
		workers = append(workers, worker)
	}
	s.agentWorkers = map[string]*managedAgentWorker{}
	s.mu.Unlock()
	for _, worker := range workers {
		worker.cancel()
	}
}

func (s *Service) agentWorkerLoop(ctx context.Context, skills agentSkillExecutor, agentID string) {
	s.mu.Lock()
	worker := s.agentWorkers[agentID]
	s.mu.Unlock()
	if worker == nil {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		if err := s.runAgentWorkerCycle(ctx, skills, agentID); err != nil {
			log.Printf("agent worker %s error: %v", agentID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-worker.wake:
		case <-ticker.C:
		}
	}
}

func (s *Service) runAgentWorkerCycle(ctx context.Context, skills agentSkillExecutor, agentID string) error {
	s.mu.Lock()
	workspace := s.latestWorkspace
	s.mu.Unlock()
	if workspace == nil {
		var err error
		workspace, err = s.fetchWorkspace(ctx)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.latestWorkspace = workspace
		s.mu.Unlock()
	}
	if workspace == nil {
		return nil
	}
	currentAgent := findAgentByID(workspace.Agents, agentID)
	if currentAgent == nil {
		return nil
	}
	return s.driveSingleAgentAutomation(ctx, skills, currentAgent, workspace)
}

func (s *Service) wakeAgentWorker(agentID string) {
	s.mu.Lock()
	worker := s.agentWorkers[agentID]
	s.mu.Unlock()
	if worker == nil {
		return
	}
	select {
	case worker.wake <- struct{}{}:
	default:
	}
}

func (s *Service) wakeAllAgentWorkers() {
	s.mu.Lock()
	workers := make([]*managedAgentWorker, 0, len(s.agentWorkers))
	for _, worker := range s.agentWorkers {
		workers = append(workers, worker)
	}
	s.mu.Unlock()
	for _, worker := range workers {
		select {
		case worker.wake <- struct{}{}:
		default:
		}
	}
}

func paceDocumentConnect() {
	documentConnectMu.Lock()
	defer documentConnectMu.Unlock()
	now := time.Now()
	if now.Before(nextDocumentConnect) {
		time.Sleep(nextDocumentConnect.Sub(now))
		now = time.Now()
	}
	nextDocumentConnect = now.Add(documentConnectInterval)
}

type trackedReconcileState struct {
	tracked      *trackedFile
	localDirty   bool
	localContent string
	baseContent  string
	baseState    []byte
	baseKnown    bool
	baseMissing  bool
	fileExists   bool
}

var errProjectedBaseDoesNotMatchCRDTState = errors.New("projected base does not match cached CRDT state")

func (s *Service) reconcileDirtyDocuments(ctx context.Context) error {
	if s.reconcileQueue == nil {
		return nil
	}
	return s.reconcileDocumentIDs(ctx, s.reconcileQueue.Drain())
}

func (s *Service) reconcileTrackedDocuments(ctx context.Context) error {
	if s.docCache == nil {
		return nil
	}
	trackedByDocument := s.collectTrackedByDocument()
	documentIDs := make([]string, 0, len(trackedByDocument))
	for documentID := range trackedByDocument {
		documentIDs = append(documentIDs, documentID)
	}
	sort.Strings(documentIDs)
	return s.reconcileDocumentIDsWithTracked(ctx, documentIDs, trackedByDocument)
}

func (s *Service) reconcileDocumentIDs(ctx context.Context, documentIDs []string) error {
	if s.docCache == nil || len(documentIDs) == 0 {
		return nil
	}
	trackedByDocument := s.collectTrackedByDocument()
	sort.Strings(documentIDs)
	return s.reconcileDocumentIDsWithTracked(ctx, documentIDs, trackedByDocument)
}

func (s *Service) reconcileDocumentIDsWithTracked(ctx context.Context, documentIDs []string, trackedByDocument map[string][]*trackedFile) error {
	if s.docCache == nil || len(documentIDs) == 0 {
		return nil
	}
	var firstErr error
	for index, documentID := range documentIDs {
		if ctx.Err() != nil {
			for _, remainingID := range documentIDs[index:] {
				s.markDocumentDirty(remainingID)
			}
			return ctx.Err()
		}
		tracked := trackedByDocument[documentID]
		if len(tracked) == 0 {
			continue
		}
		if err := s.reconcileTrackedDocument(ctx, documentID, tracked); err != nil {
			s.markDocumentDirty(documentID)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", documentID, err)
			}
			continue
		}
		if s.documentNeedsReconcile(documentID, tracked) {
			s.markDocumentDirty(documentID)
		}
	}
	return firstErr
}

func (s *Service) collectTrackedByDocument() map[string][]*trackedFile {
	result := map[string][]*trackedFile{}
	s.mu.Lock()
	replicas := make([]*workspaceReplica, 0, len(s.agentReplicas)+1)
	if s.primaryReplica != nil {
		replicas = append(replicas, s.primaryReplica)
	}
	for _, managed := range s.agentReplicas {
		if managed != nil && managed.replica != nil {
			replicas = append(replicas, managed.replica)
		}
	}
	s.mu.Unlock()
	for _, replica := range replicas {
		replica.mu.Lock()
		for documentID, tracked := range replica.projectedByID {
			result[documentID] = append(result[documentID], tracked)
		}
		replica.mu.Unlock()
	}
	return result
}

func (s *Service) markDocumentDirty(documentID string) {
	if s != nil && s.reconcileQueue != nil {
		s.reconcileQueue.Mark(documentID)
	}
}

func (s *Service) documentNeedsReconcile(documentID string, trackedFiles []*trackedFile) bool {
	if documentID == "" {
		return false
	}
	if hasTrackedLocalDirty(trackedFiles) {
		return true
	}
	if s == nil || s.docCache == nil {
		return false
	}
	entry, unlock := s.docCache.lockEntry(documentID)
	defer unlock()
	outbox, err := s.docCache.loadOutboxUpdateLocked(entry, documentID)
	if err != nil || outbox != nil {
		return true
	}
	count, err := s.docCache.pendingRemoteUpdateCountLocked(entry, documentID)
	return err != nil || count > 0
}

func (s *Service) reconcileTrackedDocument(ctx context.Context, documentID string, trackedFiles []*trackedFile) error {
	if s == nil || s.docCache == nil || documentID == "" || len(trackedFiles) == 0 {
		return nil
	}
	cache := s.docCache
	documentPath := trackedFiles[0].DocumentPath
	entry, unlock := cache.lockEntry(documentID)
	defer unlock()

	outbox, err := cache.loadOutboxUpdateLocked(entry, documentID)
	if err != nil {
		return err
	}
	if outbox != nil {
		if _, err := s.postDocumentUpdate(ctx, documentID, outbox); err != nil {
			return err
		}
		return finalizeAcceptedOutbox(cache, entry, documentID, documentPath, trackedFiles, outbox)
	}

	pendingRemoteCount, err := cache.pendingRemoteUpdateCountLocked(entry, documentID)
	if err != nil {
		if errors.Is(err, errPendingUpdateLogHashMismatch) {
			if clearErr := cache.clearPendingRemoteUpdatesLocked(entry, documentID); clearErr != nil {
				return fmt.Errorf("clear corrupt pending remote updates for %s: %w", documentID, clearErr)
			}
			pendingRemoteCount = 0
		} else {
			return err
		}
	}
	if pendingRemoteCount == 0 && !hasTrackedLocalDirty(trackedFiles) {
		return nil
	}

	baseDoc, metadata, baseState, err := cache.loadBaseDocLocked(entry, documentID, documentPath)
	if err != nil {
		return err
	}
	if pendingRemoteCount > 0 {
		if err := applyPendingRemoteUpdatesLocked(cache, entry, documentID, baseDoc); err != nil {
			return err
		}
		metadata.DocumentID = documentID
		if metadata.Path == "" {
			metadata.Path = documentPath
		}
		metadata.UpdatedAt = time.Now().UTC()
		if err := cache.storeDocLocked(entry, metadata, baseDoc); err != nil {
			return err
		}
		if err := cache.clearPendingRemoteUpdatesLocked(entry, documentID); err != nil {
			return err
		}
		baseState = baseDoc.EncodeStateAsUpdate()
	}
	cacheContentKnown := len(baseState) > 0
	baseContent := baseDoc.GetText("content").ToString()
	states, err := collectTrackedReconcileStates(trackedFiles)
	if err != nil {
		return err
	}
	hasReconcileWork := pendingRemoteCount > 0
	for _, state := range states {
		if state.localDirty || state.baseMissing {
			hasReconcileWork = true
			break
		}
	}
	if !hasReconcileWork {
		return nil
	}

	missingBase := map[*trackedFile]trackedReconcileState{}
	projectOnlyDirty := map[*trackedFile]struct{}{}
	for _, state := range states {
		if state.baseMissing {
			missingBase[state.tracked] = state
			continue
		}
		if !state.localDirty {
			continue
		}
		if state.baseContent == state.localContent && state.baseContent != baseContent {
			projectOnlyDirty[state.tracked] = struct{}{}
			continue
		}
		localContent := state.localContent
		update, observedState, err := buildLocalUpdateFromBase(state.baseState, state.baseContent, localContent)
		if err != nil {
			if !errors.Is(err, errProjectedBaseDoesNotMatchCRDTState) {
				return err
			}
			state.tracked.markLocalDirty()
			continue
		}
		if len(update) == 0 {
			state.tracked.clearLocalDirty()
			projectOnlyDirty[state.tracked] = struct{}{}
			continue
		}
		actorID := strings.TrimSpace(state.tracked.ActorID)
		actorType := strings.TrimSpace(state.tracked.ActorType)
		if actorID == "" {
			actorID = strings.TrimSpace(s.cfg.AgentID)
		}
		if actorID == "" {
			actorID = "daemon"
		}
		if actorType == "" {
			actorType = "daemon"
		}
		record := outboxUpdateRecord{
			Update:          update,
			ObservedContent: state.localContent,
			ObservedState:   observedState,
			SourcePath:      state.tracked.Path,
			ActorID:         actorID,
			ActorType:       actorType,
			CreatedAt:       time.Now().UTC(),
		}
		if err := cache.storeOutboxUpdateLocked(entry, documentID, documentPath, record); err != nil {
			return err
		}
		state.tracked.markLocalDirty()
		if _, err := s.postDocumentUpdate(ctx, documentID, &record); err != nil {
			return err
		}
		return finalizeAcceptedOutbox(cache, entry, documentID, documentPath, trackedFiles, &record)
	}
	if pendingRemoteCount == 0 && len(missingBase) == 0 && len(projectOnlyDirty) == 0 {
		return nil
	}

	finalContent := baseDoc.GetText("content").ToString()
	finalState := baseDoc.EncodeStateAsUpdate()
	for _, state := range states {
		if _, ok := missingBase[state.tracked]; ok {
			if state.fileExists {
				if err := archiveUnknownWorkingCopy(state.tracked); err != nil {
					return err
				}
			}
			state.tracked.setProjectedSnapshot(projectedContentHash{}, false)
			if cacheContentKnown {
				clean, err := applyProjectedContent(state.tracked, finalContent, finalState)
				if err != nil {
					return err
				}
				if clean {
					state.tracked.clearLocalDirty()
				} else {
					state.tracked.markLocalDirty()
				}
			} else {
				state.tracked.markLocalDirty()
			}
			continue
		}
		if _, ok := projectOnlyDirty[state.tracked]; ok {
			clean, err := applyProjectedContent(state.tracked, finalContent, finalState)
			if err != nil {
				return err
			}
			if clean {
				state.tracked.clearLocalDirty()
			} else {
				state.tracked.markLocalDirty()
			}
			continue
		}
		if state.localDirty {
			state.tracked.markLocalDirty()
			continue
		}
		clean, err := applyProjectedContent(state.tracked, finalContent, finalState)
		if err != nil {
			return err
		}
		if clean {
			state.tracked.clearLocalDirty()
		} else {
			state.tracked.markLocalDirty()
		}
	}
	return nil
}

func hasTrackedLocalDirty(trackedFiles []*trackedFile) bool {
	for _, tracked := range trackedFiles {
		if tracked != nil && tracked.isLocalDirty() {
			return true
		}
	}
	return false
}

func finalizeAcceptedOutbox(cache *documentCache, entry *documentCacheEntry, documentID, documentPath string, trackedFiles []*trackedFile, record *outboxUpdateRecord) error {
	pendingRemoteCount, err := cache.pendingRemoteUpdateCountLocked(entry, documentID)
	if err != nil {
		if errors.Is(err, errPendingUpdateLogHashMismatch) {
			if clearErr := cache.clearPendingRemoteUpdatesLocked(entry, documentID); clearErr != nil {
				return fmt.Errorf("clear corrupt pending remote updates for %s: %w", documentID, clearErr)
			}
			pendingRemoteCount = 0
		} else {
			return err
		}
	}
	baseDoc, metadata, _, err := cache.loadBaseDocLocked(entry, documentID, documentPath)
	if err != nil {
		return err
	}
	baseContent := baseDoc.GetText("content").ToString()
	_ = baseContent
	states, err := collectTrackedReconcileStates(trackedFiles)
	if err != nil {
		return err
	}
	if pendingRemoteCount > 0 {
		if err := applyPendingRemoteUpdatesLocked(cache, entry, documentID, baseDoc); err != nil {
			return err
		}
	}
	if err := crdt.ApplyUpdateV1(baseDoc, record.Update, "local-reconcile"); err != nil {
		return err
	}
	metadata.DocumentID = documentID
	if metadata.Path == "" {
		metadata.Path = documentPath
	}
	metadata.UpdatedAt = time.Now().UTC()
	if err := cache.storeDocLocked(entry, metadata, baseDoc); err != nil {
		return err
	}
	if err := cache.clearPendingRemoteUpdatesLocked(entry, documentID); err != nil {
		return err
	}
	if err := cache.clearOutboxUpdateLocked(entry, documentID); err != nil {
		return err
	}

	finalContent := baseDoc.GetText("content").ToString()
	finalState := baseDoc.EncodeStateAsUpdate()
	for _, state := range states {
		if state.tracked == nil {
			continue
		}
		if state.tracked.Path == record.SourcePath {
			clean, err := projectMergedContentOverLocalDisk(state.tracked, record.ObservedContent, record.ObservedState, finalContent, finalState)
			if err != nil {
				return err
			}
			if clean {
				state.tracked.clearLocalDirty()
			} else {
				state.tracked.markLocalDirty()
			}
			continue
		}
		if state.localDirty {
			state.tracked.markLocalDirty()
			continue
		}
		clean, err := applyProjectedContent(state.tracked, finalContent, finalState)
		if err != nil {
			return err
		}
		if clean {
			state.tracked.clearLocalDirty()
		} else {
			state.tracked.markLocalDirty()
		}
	}
	return nil
}

func applyPendingRemoteUpdatesLocked(cache *documentCache, entry *documentCacheEntry, documentID string, doc *crdt.Doc) error {
	if cache == nil || doc == nil {
		return nil
	}
	if err := cache.forEachPendingRemoteUpdateLocked(entry, documentID, func(update []byte) error {
		if len(update) == 0 {
			return nil
		}
		return crdt.ApplyUpdateV1(doc, update, "remote-reconcile")
	}); err != nil {
		if clearErr := cache.clearPendingRemoteUpdatesLocked(entry, documentID); clearErr != nil {
			return fmt.Errorf("apply pending remote update for %s: %w; clear corrupt pending log: %v", documentID, err, clearErr)
		}
		return fmt.Errorf("cleared corrupt pending remote updates for %s: %w", documentID, err)
	}
	return nil
}

func collectTrackedReconcileStates(trackedFiles []*trackedFile) ([]trackedReconcileState, error) {
	states := make([]trackedReconcileState, 0, len(trackedFiles))
	for _, tracked := range trackedFiles {
		if tracked == nil {
			continue
		}
		state := trackedReconcileState{tracked: tracked}
		if tracked.isProjecting() {
			states = append(states, state)
			continue
		}
		baseContent, baseState, known, err := tracked.loadProjectedBase()
		if err != nil {
			return nil, err
		}
		if known {
			state.baseContent = baseContent
			state.baseState = baseState
			state.baseKnown = true
			if !tracked.hasProjectedContent() {
				tracked.setProjectedContent(baseContent)
			}
		} else {
			state.baseMissing = true
		}
		contentBytes, err := readFileLocked(tracked.Path)
		if errors.Is(err, os.ErrNotExist) {
			states = append(states, state)
			continue
		}
		if err != nil {
			return nil, err
		}
		state.fileExists = true
		state.localContent = string(contentBytes)
		if state.baseMissing {
			state.localDirty = true
			states = append(states, state)
			continue
		}
		if projectedHashBytes(contentBytes) == projectedHashString(state.baseContent) {
			tracked.clearLocalDirty()
			states = append(states, state)
			continue
		}
		state.localDirty = true
		states = append(states, state)
	}
	return states, nil
}

func buildLocalUpdateFromBase(baseState []byte, baseContent, localContent string) ([]byte, []byte, error) {
	if baseContent == localContent {
		if len(baseState) > 0 {
			return nil, append([]byte(nil), baseState...), nil
		}
		return nil, crdtStateFromContent(baseContent), nil
	}
	doc := crdt.New()
	if len(baseState) > 0 {
		if err := crdt.ApplyUpdateV1(doc, baseState, "local-base"); err != nil {
			return nil, nil, err
		}
	}
	text := doc.GetText("content")
	if text.ToString() != baseContent {
		return nil, nil, errProjectedBaseDoesNotMatchCRDTState
	}
	replace := computeReplace(baseContent, localContent)
	currentLength := text.Len()
	var update []byte
	unsubscribe := doc.OnUpdate(func(next []byte, origin any) {
		if origin == "daemon-local-reconcile" {
			update = append([]byte(nil), next...)
		}
	})
	doc.Transact(func(txn *crdt.Transaction) {
		start, deleteLength := clampReplace(currentLength, replace)
		if deleteLength > 0 {
			text.Delete(txn, start, deleteLength)
		}
		if replace.Text != "" {
			text.Insert(txn, start, replace.Text, nil)
		}
	}, "daemon-local-reconcile")
	unsubscribe()
	return update, doc.EncodeStateAsUpdate(), nil
}

func applyProjectedContent(tracked *trackedFile, nextContent string, nextStates ...[]byte) (bool, error) {
	var nextState []byte
	if len(nextStates) > 0 {
		nextState = nextStates[0]
	}
	previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(nextContent)
	if err := writeProjectedFile(tracked.Path, nextContent, previousHash); err != nil {
		if errors.Is(err, errProjectedFileDiverged) {
			tracked.setProjectedSnapshot(previousHash, previousKnown)
			return false, nil
		}
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		return false, err
	}
	if err := tracked.storeProjectedBase(nextContent, nextState); err != nil {
		return false, err
	}
	return true, nil
}

func markTrackedLocalDirty(tracked *trackedFile, _ string) error {
	if tracked.isProjecting() {
		return nil
	}
	if !tracked.hasProjectedContent() {
		return nil
	}
	// Write events can arrive at high frequency. Reconciliation performs the
	// authoritative locked read and clears false positives such as late
	// fsnotify events from our own projection writes.
	tracked.markLocalDirty()
	return nil
}

func projectMergedContentOverLocalDisk(tracked *trackedFile, currentDiskContent string, currentDiskState []byte, mergedContent string, mergedState []byte) (bool, error) {
	previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(mergedContent)
	if err := writeProjectedFile(tracked.Path, mergedContent, projectedHashString(currentDiskContent)); err != nil {
		if errors.Is(err, errProjectedFileDiverged) {
			if len(currentDiskState) == 0 {
				currentDiskState = crdtStateFromContent(currentDiskContent)
			}
			if baseErr := tracked.storeProjectedBase(currentDiskContent, currentDiskState); baseErr != nil {
				tracked.setProjectedSnapshot(previousHash, previousKnown)
				return false, baseErr
			}
			tracked.setProjectedContent(currentDiskContent)
			return false, nil
		}
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		return false, err
	}
	if err := tracked.storeProjectedBase(mergedContent, mergedState); err != nil {
		return false, err
	}
	return true, nil
}

func materializeTrackedFile(ctx context.Context, cache *documentCache, document *document, absolutePath string) (*trackedFile, error) {
	tracked := &trackedFile{
		DocumentID:    document.ID,
		DocumentPath:  document.Path,
		Path:          absolutePath,
		WorkspaceRoot: workspaceRootForDocumentPath(absolutePath, document.Path),
		Doc:           crdt.New(),
		docMu:         &sync.Mutex{},
		cache:         cache,
	}
	if cache != nil {
		materialized, err := cache.materialize(ctx, document)
		if err != nil {
			return nil, err
		}
		if materialized.ContentKnown {
			tracked.Doc = materialized.Doc
			tracked.docMu = materialized.DocMu
			tracked.cacheEntry = materialized.Entry
		} else if materialized.Doc != nil {
			materialized.Doc.Close()
		}

		baseContent, _, baseKnown, err := tracked.loadProjectedBase()
		if err != nil {
			return nil, err
		}
		if baseKnown {
			tracked.setProjectedContent(baseContent)
			content, readErr := readFileLocked(absolutePath)
			if readErr == nil {
				if !tracked.matchesProjectedBytes(content) {
					tracked.markLocalDirty()
				}
				return tracked, nil
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
			return tracked, nil
		}
		if _, readErr := readFileLocked(absolutePath); readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		}
		tracked.markLocalDirty()
		return tracked, nil
	}
	content, err := readFileLocked(absolutePath)
	if err == nil {
		tracked.setProjectedContent(string(content))
		return tracked, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := writeIfChanged(absolutePath, ""); err != nil {
		return nil, err
	}
	tracked.setProjectedContent("")
	return tracked, nil
}

func archiveUnknownWorkingCopy(tracked *trackedFile) error {
	if tracked == nil || tracked.Path == "" {
		return nil
	}
	content, err := readFileLocked(tracked.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	root := tracked.WorkspaceRoot
	if root == "" {
		root = workspaceRootForDocumentPath(tracked.Path, tracked.DocumentPath)
	}
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName(tracked.DocumentID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Base(tracked.Path)
	if base == "." || base == string(filepath.Separator) {
		base = "document"
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for i := 0; i < 100; i++ {
		name := stamp + "-" + base
		if i > 0 {
			name = fmt.Sprintf("%s-%d-%s", stamp, i, base)
		}
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := file.Write(content); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Remove(tracked.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return errors.New("failed to allocate recovered workspace file name")
}

func (s *Service) updateRemoteAgentSession(ctx context.Context, agentID string, update updateAgentSessionRequest) error {
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	req, err := s.newBackendRequest(ctx, http.MethodPatch, "/api/agents/"+agentID+"/session", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("update agent session failed: %s", res.Status)
	}
	return nil
}

func (s *Service) postDocumentUpdate(ctx context.Context, documentID string, record *outboxUpdateRecord) (*postDocumentUpdateResponse, error) {
	if record == nil || len(record.Update) == 0 {
		return &postDocumentUpdateResponse{Accepted: true}, nil
	}
	req, err := s.newBackendRequest(ctx, http.MethodPost, "/api/documents/"+documentID+"/updates", bytes.NewReader(record.Update))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if record.ActorType == "agent" && strings.TrimSpace(record.ActorID) != "" {
		applyBackendAuth(req.Header, s.cfg, record.ActorID)
	}
	if strings.TrimSpace(s.cfg.DaemonToken) == "" {
		query := req.URL.Query()
		query.Set("actor", firstNonEmptyText(record.ActorID, s.cfg.AgentID))
		query.Set("actor_type", firstNonEmptyText(record.ActorType, "daemon"))
		req.URL.RawQuery = query.Encode()
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return nil, &backendStatusError{
			Method:     req.Method,
			URL:        req.URL.String(),
			StatusCode: res.StatusCode,
			Body:       string(body),
		}
	}
	var response postDocumentUpdateResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	if !response.Accepted {
		return nil, errors.New("document update was not accepted")
	}
	return &response, nil
}

func scanWorkspaceFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && isIgnoredWorkspaceAbsolutePath(root, path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		content, err := readFileLocked(path)
		if err != nil {
			return err
		}
		files[path] = string(content)
		return nil
	})
	return files, err
}

func isIgnoredDocumentPath(documentPath string) bool {
	return isIgnoredWorkspaceRelativePath(documentPath)
}

func isIgnoredWorkspaceAbsolutePath(root, absolutePath string) bool {
	if root == "" {
		return isIgnoredWorkspaceRelativePath(absolutePath)
	}
	relative, err := filepath.Rel(root, absolutePath)
	if err != nil {
		return true
	}
	if relative == "." {
		return false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	return isIgnoredWorkspaceRelativePath(relative)
}

func isIgnoredWorkspaceRelativePath(relativePath string) bool {
	clean := filepath.ToSlash(filepath.Clean(relativePath))
	if clean == "." || clean == "" {
		return false
	}
	for _, segment := range strings.FieldsFunc(clean, func(r rune) bool {
		return r == '/'
	}) {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

func findMovedPath(candidates map[string]string, matches func(string) bool) (string, bool) {
	paths := make([]string, 0, len(candidates))
	for path, candidateContent := range candidates {
		if matches(candidateContent) {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return "", false
	}
	sort.Strings(paths)
	return paths[0], true
}

func workspaceRelativePath(root, absolutePath string) string {
	relative, err := filepath.Rel(root, absolutePath)
	if err != nil {
		return filepath.ToSlash(absolutePath)
	}
	return filepath.ToSlash(relative)
}

var writeProjectedFile = writeProjectedFileLocked

func moveLocalFile(from, to, content string) error {
	if from == to {
		return writeIfChanged(to, content)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil && !errorsIsNotExist(err) {
		if writeErr := writeIfChanged(to, content); writeErr != nil {
			return writeErr
		}
	} else if err != nil {
		if writeErr := writeIfChanged(to, content); writeErr != nil {
			return writeErr
		}
	}
	if err := writeIfChanged(to, content); err != nil {
		return err
	}
	if err := os.Remove(from); err != nil && !errorsIsNotExist(err) {
		return err
	}
	return nil
}

func nextAwarenessClientID() uint64 {
	// Awareness messages are decoded by JS Yjs clients, whose varuint reader
	// rejects integers above Number.MAX_SAFE_INTEGER. Keep daemon-generated
	// awareness IDs small; CRDT document client IDs remain separate.
	return 10_000_000 + awarenessClientCounter.Add(1)
}

func (t *trackedFile) contentString() string {
	if base, _, ok, err := t.loadProjectedBase(); err == nil && ok {
		return base
	}
	unlockDoc := t.lockDoc()
	defer unlockDoc()
	if t.Doc == nil {
		return ""
	}
	return t.Doc.GetText("content").ToString()
}

func (t *trackedFile) lockDoc() func() {
	if t.docMu == nil {
		return func() {}
	}
	t.docMu.Lock()
	return t.docMu.Unlock
}

func (t *trackedFile) storeProjectedBase(content string, states ...[]byte) error {
	if t == nil || t.DocumentID == "" {
		return nil
	}
	var state []byte
	if len(states) > 0 {
		state = states[0]
	}
	if len(state) == 0 {
		state = crdtStateFromContent(content)
	}
	dir := t.projectionDir()
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte(content), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "base.state.bin"), state, 0o644)
}

func (t *trackedFile) loadProjectedBase() (string, []byte, bool, error) {
	if t == nil || t.DocumentID == "" {
		return "", nil, false, nil
	}
	dir := t.projectionDir()
	if dir == "" {
		return "", nil, false, nil
	}
	content, err := os.ReadFile(filepath.Join(dir, "base.txt"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	state, err := os.ReadFile(filepath.Join(dir, "base.state.bin"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	if !projectedStateMatchesContent(state, string(content)) {
		return "", nil, false, nil
	}
	return string(content), state, true, nil
}

func (t *trackedFile) projectedStateOrContentState(content string) []byte {
	_, state, known, err := t.loadProjectedBase()
	if err == nil && known && len(state) > 0 {
		return state
	}
	return crdtStateFromContent(content)
}

func (t *trackedFile) projectionDir() string {
	root := t.WorkspaceRoot
	if root == "" {
		root = workspaceRootForDocumentPath(t.Path, t.DocumentPath)
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".notty", "projections", safeDocumentCacheName(t.DocumentID))
}

func (t *trackedFile) projectedHash() projectedContentHash {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.hash
}

func (t *trackedFile) projectedSnapshot() (projectedContentHash, bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.hash, t.projectedContentKnown
}

func (t *trackedFile) hasProjectedContent() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.projectedContentKnown
}

func (t *trackedFile) setProjectedContent(content string) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.projectedContentKnown = true
	t.hash = projectedHashString(content)
}

func (t *trackedFile) setProjectedHash(hash projectedContentHash) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.hash = hash
	t.projectedContentKnown = false
}

func (t *trackedFile) setProjectedSnapshot(hash projectedContentHash, known bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.hash = hash
	t.projectedContentKnown = known
}

func (t *trackedFile) matchesProjectedString(content string) bool {
	return t.projectedHash() == projectedHashString(content)
}

func (t *trackedFile) matchesProjectedBytes(content []byte) bool {
	return t.projectedHash() == projectedHashBytes(content)
}

func projectedHashString(content string) projectedContentHash {
	return projectedContentHash{size: len(content), sum: maphash.String(projectedHashSeed, content)}
}

func projectedHashBytes(content []byte) projectedContentHash {
	return projectedContentHash{size: len(content), sum: maphash.Bytes(projectedHashSeed, content)}
}

func (t *trackedFile) beginProjection() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.projecting++
}

func (t *trackedFile) endProjection() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	if t.projecting > 0 {
		t.projecting--
	}
}

func (t *trackedFile) isProjecting() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.projecting > 0
}

func (t *trackedFile) markLocalDirty() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.localDirty = true
}

func (t *trackedFile) clearLocalDirty() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.localDirty = false
}

func (t *trackedFile) isLocalDirty() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.localDirty
}

type replaceOp struct {
	Start int
	End   int
	Text  string
}

func computeReplace(before, after string) replaceOp {
	start := 0
	for start < len(before) && start < len(after) && before[start] == after[start] {
		start++
	}
	beforeEnd := len(before)
	afterEnd := len(after)
	for beforeEnd > start && afterEnd > start && before[beforeEnd-1] == after[afterEnd-1] {
		beforeEnd--
		afterEnd--
	}
	return replaceOp{Start: start, End: beforeEnd, Text: strings.Clone(after[start:afterEnd])}
}

func clampReplace(contentLength int, replace replaceOp) (int, int) {
	start := replace.Start
	if start > contentLength {
		start = contentLength
	}
	end := replace.End
	if end > contentLength {
		end = contentLength
	}
	if end < start {
		end = start
	}
	return start, end - start
}

func workspaceRootForDocumentPath(absolutePath, documentPath string) string {
	absolutePath = filepath.Clean(absolutePath)
	documentPath = filepath.Clean(filepath.FromSlash(documentPath))
	if documentPath != "." && documentPath != "" {
		suffix := string(filepath.Separator) + documentPath
		if strings.HasSuffix(absolutePath, suffix) {
			return strings.TrimSuffix(absolutePath, suffix)
		}
	}
	return filepath.Dir(absolutePath)
}

func projectedStateMatchesContent(state []byte, content string) bool {
	doc := crdt.New()
	if len(state) > 0 {
		if err := crdt.ApplyUpdateV1(doc, state, "projection-check"); err != nil {
			return false
		}
	}
	return doc.GetText("content").ToString() == content
}

func crdtStateFromContent(content string) []byte {
	doc := crdt.New()
	if content != "" {
		text := doc.GetText("content")
		doc.Transact(func(txn *crdt.Transaction) {
			text.Insert(txn, 0, content, nil)
		}, "projection-state")
	}
	return doc.EncodeStateAsUpdate()
}

func errorsIsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
