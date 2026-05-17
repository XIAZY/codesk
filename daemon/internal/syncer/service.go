package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/gorilla/websocket"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
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

type trackedFile struct {
	DocumentID            string
	DocumentPath          string
	Path                  string
	WorkspaceRoot         string
	StateVector           string
	Doc                   *crdt.Doc
	docMu                 *sync.Mutex
	AwarenessClientID     uint64
	cache                 *documentCache
	cacheEntry            *documentCacheEntry
	Conn                  *websocket.Conn
	writeSyncUpdate       func([]byte) error
	writeSyncStep1        func([]byte) error
	connMu                sync.Mutex
	stateMu               sync.Mutex
	projecting            int
	localDirty            bool
	syncFromScratch       bool
	serverStateVector     []byte
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
	ID          string `json:"id"`
	Path        string `json:"path"`
	StateVector string `json:"stateVector"`
	UpdateID    int64  `json:"updateId"`
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
		s.closePrimaryReplica()
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
			s.closePrimaryReplica()
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

func handleRemoteSyncMessage(reader *bytes.Reader, tracked *trackedFile, origin any) ([]byte, bool, error) {
	messageType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		return nil, false, err
	}
	switch messageType {
	case yproto.SyncStep1:
		tracked.setServerStateVector(data)
		if tracked.cache != nil {
			// The backend is the source of truth. Cached daemon projections are
			// only local bases for explicit dirty-file reconciliation; replying
			// to the server's SyncStep1 with the whole cache can rebroadcast
			// checkpoint-tail history as duplicate updates.
			return nil, false, nil
		}
		unlockDoc := tracked.lockDoc()
		defer unlockDoc()
		if tracked.Doc == nil {
			doc := crdt.New()
			defer doc.Close()
			update, err := doc.EncodeStateAsUpdateV1(data)
			if err != nil {
				return nil, false, err
			}
			return yproto.BuildSyncStep2FromUpdate(update), false, nil
		}
		update, err := tracked.Doc.EncodeStateAsUpdateV1(data)
		if err != nil {
			return nil, false, err
		}
		return yproto.BuildSyncStep2FromUpdate(update), false, nil
	case yproto.SyncStep2, yproto.SyncUpdate:
		if tracked.cache != nil {
			appended, err := tracked.cache.appendPendingRemoteUpdate(tracked.DocumentID, tracked.DocumentPath, data)
			return nil, appended, err
		}
		unlockDoc := tracked.lockDoc()
		defer unlockDoc()
		if tracked.Doc == nil {
			tracked.Doc = crdt.New()
		}
		if err := crdt.ApplyUpdateV1(tracked.Doc, data, origin); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	default:
		return nil, false, errors.New("unknown sync message type")
	}
}

type trackedReconcileState struct {
	tracked      *trackedFile
	localDirty   bool
	localContent string
	baseContent  string
	baseState    []byte
	baseKnown    bool
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
		if err := reconcileTrackedDocument(s.docCache, documentID, tracked); err != nil {
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

func (s *Service) closePrimaryReplica() {
	if s == nil || s.primaryReplica == nil {
		return
	}
	s.primaryReplica.closeConnections()
}

func reconcileTrackedDocument(cache *documentCache, documentID string, trackedFiles []*trackedFile) error {
	if cache == nil || documentID == "" || len(trackedFiles) == 0 {
		return nil
	}
	documentPath := trackedFiles[0].DocumentPath
	entry, unlock := cache.lockEntry(documentID)
	defer unlock()

	outbox, err := cache.loadOutboxUpdateLocked(entry, documentID)
	if err != nil {
		return err
	}
	if outbox != nil {
		if !outboxConverged(outbox, trackedFiles) {
			_ = sendOutboxUpdateProbe(trackedFiles, outbox)
			if outboxConverged(outbox, trackedFiles) {
				return finalizeConvergedOutbox(cache, entry, documentID, documentPath, trackedFiles, outbox)
			}
			return nil
		}
		return finalizeConvergedOutbox(cache, entry, documentID, documentPath, trackedFiles, outbox)
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
	baseContent := baseDoc.GetText("content").ToString()
	states, err := collectTrackedReconcileStates(trackedFiles, baseContent, baseState)
	if err != nil {
		return err
	}
	hasLocalDirty := false
	for _, state := range states {
		if state.localDirty {
			hasLocalDirty = true
			break
		}
	}
	if pendingRemoteCount == 0 && !hasLocalDirty {
		return nil
	}

	skippedLocalDirty := map[*trackedFile]bool{}
	projectOnlyDirty := map[*trackedFile]struct{}{}
	for _, state := range states {
		if !state.localDirty {
			continue
		}
		if !state.baseKnown {
			state.tracked.markLocalDirty()
			skippedLocalDirty[state.tracked] = true
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
			skippedLocalDirty[state.tracked] = false
			continue
		}
		if len(update) == 0 {
			state.tracked.clearLocalDirty()
			projectOnlyDirty[state.tracked] = struct{}{}
			continue
		}
		targetStateVector := crdtStateVectorFromState(observedState)
		if len(targetStateVector) == 0 {
			return errors.New("failed to derive local update target state vector")
		}
		record := outboxUpdateRecord{
			Update:            update,
			TargetStateVector: targetStateVector,
			ObservedContent:   state.localContent,
			ObservedState:     observedState,
			SourcePath:        state.tracked.Path,
			CreatedAt:         time.Now().UTC(),
		}
		if err := cache.storeOutboxUpdateLocked(entry, documentID, documentPath, record); err != nil {
			return err
		}
		state.tracked.markLocalDirty()
		_ = sendOutboxUpdateProbe(trackedFiles, &record)
		if outboxConverged(&record, trackedFiles) {
			return finalizeConvergedOutbox(cache, entry, documentID, documentPath, trackedFiles, &record)
		}
		return nil
	}
	if pendingRemoteCount == 0 && len(skippedLocalDirty) == 0 && len(projectOnlyDirty) == 0 {
		return nil
	}

	if pendingRemoteCount > 0 {
		if err := cache.forEachPendingRemoteUpdateLocked(entry, documentID, func(update []byte) error {
			if len(update) == 0 {
				return nil
			}
			return crdt.ApplyUpdateV1(baseDoc, update, "remote-reconcile")
		}); err != nil {
			if clearErr := cache.clearPendingRemoteUpdatesLocked(entry, documentID); clearErr != nil {
				return fmt.Errorf("apply pending remote update for %s: %w; clear corrupt pending log: %v", documentID, err, clearErr)
			}
			return fmt.Errorf("cleared corrupt pending remote updates for %s: %w", documentID, err)
		}
	}
	metadata.DocumentID = documentID
	if metadata.Path == "" {
		metadata.Path = documentPath
	}
	metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(baseDoc))
	metadata.UpdatedAt = time.Now().UTC()
	if err := cache.storeDocLocked(entry, metadata, baseDoc); err != nil {
		return err
	}
	if err := cache.clearPendingRemoteUpdatesLocked(entry, documentID); err != nil {
		return err
	}

	finalContent := baseDoc.GetText("content").ToString()
	finalState := baseDoc.EncodeStateAsUpdate()
	for _, state := range states {
		if _, ok := projectOnlyDirty[state.tracked]; ok {
			clean, err := applyProjectedContent(state.tracked, finalContent, finalState)
			if err != nil {
				return err
			}
			if clean {
				state.tracked.clearLocalDirty()
				state.tracked.setSyncFromScratch(false)
			} else {
				state.tracked.markLocalDirty()
			}
			continue
		}
		if canRebase, skipped := skippedLocalDirty[state.tracked]; skipped {
			if canRebase && (pendingRemoteCount > 0 || len(baseState) > 0 || metadata.StateVector == emptyDocumentStateVector) {
				if err := state.tracked.storeProjectedBase(finalContent, finalState); err != nil {
					return err
				}
				state.tracked.setProjectedContent(finalContent)
				state.tracked.setSyncFromScratch(false)
			}
			state.tracked.markLocalDirty()
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
			state.tracked.setSyncFromScratch(false)
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

func outboxConverged(record *outboxUpdateRecord, trackedFiles []*trackedFile) bool {
	if record == nil {
		return true
	}
	for _, tracked := range trackedFiles {
		if tracked != nil && tracked.serverStateVectorCovers(record.TargetStateVector) {
			return true
		}
	}
	return false
}

func sendOutboxUpdateProbe(trackedFiles []*trackedFile, record *outboxUpdateRecord) error {
	if record == nil || len(record.Update) == 0 {
		return nil
	}
	candidates := make([]*trackedFile, 0, len(trackedFiles))
	for _, tracked := range trackedFiles {
		if tracked != nil && tracked.Path == record.SourcePath {
			candidates = append(candidates, tracked)
		}
	}
	for _, tracked := range trackedFiles {
		if tracked != nil && tracked.Path != record.SourcePath {
			candidates = append(candidates, tracked)
		}
	}
	var lastErr error
	for _, tracked := range candidates {
		if err := tracked.sendSyncUpdate(record.Update); err != nil {
			lastErr = err
			continue
		}
		if err := tracked.sendSyncStep1(record.TargetStateVector); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no tracked document connection available")
}

func finalizeConvergedOutbox(cache *documentCache, entry *documentCacheEntry, documentID, documentPath string, trackedFiles []*trackedFile, record *outboxUpdateRecord) error {
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
	baseDoc, metadata, baseState, err := cache.loadBaseDocLocked(entry, documentID, documentPath)
	if err != nil {
		return err
	}
	baseContent := baseDoc.GetText("content").ToString()
	states, err := collectTrackedReconcileStates(trackedFiles, baseContent, baseState)
	if err != nil {
		return err
	}
	if pendingRemoteCount > 0 {
		if err := cache.forEachPendingRemoteUpdateLocked(entry, documentID, func(update []byte) error {
			if len(update) == 0 {
				return nil
			}
			return crdt.ApplyUpdateV1(baseDoc, update, "remote-reconcile")
		}); err != nil {
			if clearErr := cache.clearPendingRemoteUpdatesLocked(entry, documentID); clearErr != nil {
				return fmt.Errorf("apply pending remote update for %s: %w; clear corrupt pending log: %v", documentID, err, clearErr)
			}
			return fmt.Errorf("cleared corrupt pending remote updates for %s: %w", documentID, err)
		}
	}
	if err := crdt.ApplyUpdateV1(baseDoc, record.Update, "local-reconcile"); err != nil {
		return err
	}
	metadata.DocumentID = documentID
	if metadata.Path == "" {
		metadata.Path = documentPath
	}
	metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(baseDoc))
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
				state.tracked.setSyncFromScratch(false)
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
			state.tracked.setSyncFromScratch(false)
		} else {
			state.tracked.markLocalDirty()
		}
	}
	return nil
}

func collectTrackedReconcileStates(trackedFiles []*trackedFile, sharedBaseContent string, sharedBaseState []byte) ([]trackedReconcileState, error) {
	states := make([]trackedReconcileState, 0, len(trackedFiles))
	for _, tracked := range trackedFiles {
		if tracked == nil {
			continue
		}
		state := trackedReconcileState{tracked: tracked}
		if tracked.isProjecting() || !tracked.hasProjectedContent() {
			states = append(states, state)
			continue
		}
		if !tracked.isLocalDirty() {
			states = append(states, state)
			continue
		}
		contentBytes, err := readFileLocked(tracked.Path)
		if errors.Is(err, os.ErrNotExist) {
			states = append(states, state)
			continue
		}
		if err != nil {
			return nil, err
		}
		if tracked.matchesProjectedBytes(contentBytes) {
			tracked.clearLocalDirty()
			states = append(states, state)
			continue
		}
		state.localContent = string(contentBytes)
		state.localDirty = true
		baseContent, baseState, known, err := tracked.loadProjectedBase()
		if err != nil {
			return nil, err
		}
		if known {
			state.baseContent = baseContent
			state.baseState = baseState
			state.baseKnown = true
		} else {
			state.baseContent = sharedBaseContent
			state.baseState = sharedBaseState
		}
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
	tracked.setSyncFromScratch(false)
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
			tracked.setSyncFromScratch(false)
			return false, nil
		}
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		return false, err
	}
	if err := tracked.storeProjectedBase(mergedContent, mergedState); err != nil {
		return false, err
	}
	tracked.setSyncFromScratch(false)
	return true, nil
}

func materializeTrackedFile(ctx context.Context, cache *documentCache, document *document, absolutePath string) (*trackedFile, error) {
	tracked := &trackedFile{
		DocumentID:        document.ID,
		DocumentPath:      document.Path,
		Path:              absolutePath,
		WorkspaceRoot:     workspaceRootForDocumentPath(absolutePath, document.Path),
		StateVector:       document.StateVector,
		Doc:               crdt.New(),
		docMu:             &sync.Mutex{},
		AwarenessClientID: nextAwarenessClientID(),
		cache:             cache,
		syncFromScratch:   true,
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
			text := materialized.Doc.GetText("content").ToString()
			state := materialized.Doc.EncodeStateAsUpdate()
			content, readErr := readFileLocked(absolutePath)
			if readErr == nil {
				if err := tracked.storeProjectedBase(text, state); err != nil {
					return nil, err
				}
				// Existing disk bytes are only clean if they match a CRDT-derived base.
				// Otherwise reconciliation must treat them as local edits.
				tracked.setProjectedContent(text)
				if !tracked.matchesProjectedBytes(content) {
					tracked.markLocalDirty()
				}
				tracked.setSyncFromScratch(false)
				return tracked, nil
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
			if err := writeIfChanged(absolutePath, text); err != nil {
				return nil, err
			}
			tracked.setProjectedContent(text)
			if err := tracked.storeProjectedBase(text, state); err != nil {
				return nil, err
			}
			tracked.setSyncFromScratch(false)
			return tracked, nil
		}
	}
	content, err := readFileLocked(absolutePath)
	if err == nil {
		if cache == nil {
			tracked.setProjectedContent(string(content))
			return tracked, nil
		}
		// The canonical base is not known yet. Keep a placeholder projection so
		// the next reconcile sees the existing file as dirty, but defer update
		// generation until a server-derived base arrives.
		tracked.setProjectedContent("")
		tracked.markLocalDirty()
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

func (t *trackedFile) write(payload []byte) error {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.Conn == nil {
		return errors.New("document websocket is not connected")
	}
	return t.Conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (t *trackedFile) sendSyncUpdate(update []byte) error {
	if t.writeSyncUpdate != nil {
		return t.writeSyncUpdate(update)
	}
	return t.write(yproto.BuildSyncUpdate(update))
}

func (t *trackedFile) sendSyncStep1(stateVector []byte) error {
	if t.writeSyncStep1 != nil {
		return t.writeSyncStep1(stateVector)
	}
	return t.write(yproto.BuildSyncStep1FromStateVector(stateVector))
}

func (t *trackedFile) getConn() *websocket.Conn {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	return t.Conn
}

func (t *trackedFile) setConn(conn *websocket.Conn) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	t.Conn = conn
}

func (t *trackedFile) clearConn(conn *websocket.Conn) bool {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.Conn != conn {
		return false
	}
	t.Conn = nil
	return true
}

func (t *trackedFile) initialSyncState() (uint64, []byte) {
	clientID := t.AwarenessClientID
	if clientID == 0 {
		if t.Doc != nil {
			clientID = uint64(t.Doc.ClientID())
		} else {
			clientID = nextAwarenessClientID()
			t.AwarenessClientID = clientID
		}
	}
	if t.shouldSyncFromScratch() {
		return clientID, yproto.BuildSyncStep1FromStateVector(nil)
	}
	if t.cache != nil {
		if stateVector := t.cache.cachedStateVector(t.DocumentID); len(stateVector) > 0 {
			return clientID, yproto.BuildSyncStep1FromStateVector(stateVector)
		}
	}
	if t.StateVector != "" {
		if stateVector, err := base64.StdEncoding.DecodeString(t.StateVector); err == nil {
			return clientID, yproto.BuildSyncStep1FromStateVector(stateVector)
		}
	}
	unlockDoc := t.lockDoc()
	defer unlockDoc()
	if t.Doc == nil {
		return clientID, yproto.BuildSyncStep1FromStateVector(nil)
	}
	return clientID, yproto.BuildSyncStep1FromStateVector(crdt.EncodeStateVectorV1(t.Doc))
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

func stateVectorCovers(current []byte, target []byte) bool {
	if len(target) == 0 {
		return true
	}
	if len(current) == 0 {
		return false
	}
	currentVector, err := crdt.DecodeStateVectorV1(current)
	if err != nil {
		return false
	}
	targetVector, err := crdt.DecodeStateVectorV1(target)
	if err != nil {
		return false
	}
	for client, clock := range targetVector {
		if currentVector.Clock(client) < clock {
			return false
		}
	}
	return true
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

func (t *trackedFile) setSyncFromScratch(value bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.syncFromScratch = value
}

func (t *trackedFile) shouldSyncFromScratch() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.syncFromScratch
}

func (t *trackedFile) setServerStateVector(stateVector []byte) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.serverStateVector = append(t.serverStateVector[:0], stateVector...)
}

func (t *trackedFile) serverStateVectorCovers(target []byte) bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return stateVectorCovers(t.serverStateVector, target)
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

func crdtStateVectorFromState(state []byte) []byte {
	doc := crdt.New()
	if len(state) > 0 {
		if err := crdt.ApplyUpdateV1(doc, state, "state-vector"); err != nil {
			return nil
		}
	}
	return crdt.EncodeStateVectorV1(doc)
}

func errorsIsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
