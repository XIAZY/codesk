package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"notty/internal/yproto"
)

type workspaceEventEnvelope struct {
	Type string `json:"type"`
}

type Service struct {
	cfg             Config
	client          *http.Client
	watcher         *fsnotify.Watcher
	sessions        *agentSessionSupervisor
	toolServer      *http.Server
	mu              sync.Mutex
	projectedByPath map[string]*trackedFile
	projectedByID   map[string]*trackedFile
	agentReplicas   map[string]*managedReplica
	agentWorkers    map[string]*managedAgentWorker
	latestWorkspace *workspaceResponse
	docCache        *documentCache
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
	StateVector           string
	Doc                   *crdt.Doc
	docMu                 *sync.Mutex
	AwarenessClientID     uint64
	cache                 *documentCache
	cacheEntry            *documentCacheEntry
	Conn                  *websocket.Conn
	writeSyncUpdate       func([]byte) error
	connMu                sync.Mutex
	stateMu               sync.Mutex
	projecting            int
	localDirty            bool
	syncFromScratch       bool
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

type workspaceResponse struct {
	Documents   []*document   `json:"documents"`
	Agents      []*agent      `json:"agents"`
	AgentRuns   []*agentRun   `json:"agentRuns"`
	Threads     []*thread     `json:"threads"`
	AgentEvents []*agentEvent `json:"agentEvents"`
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
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	cache, err := newDocumentCache(cfg.CacheDir)
	if err != nil {
		_ = watcher.Close()
		return nil, err
	}
	service := &Service{
		cfg:             cfg,
		client:          client,
		watcher:         watcher,
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		agentReplicas:   map[string]*managedReplica{},
		agentWorkers:    map[string]*managedAgentWorker{},
		docCache:        cache,
	}
	service.sessions = newAgentSessionSupervisor(cfg, service.updateRemoteAgentSession, nil)
	service.sessions.SetIdleWake(service.wakeAgentWorker)
	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	defer s.watcher.Close()
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
	if err := s.watcher.Add(s.cfg.WorkspaceDir); err != nil {
		return err
	}
	if err := s.refreshInitialWorkspace(ctx); err != nil {
		return err
	}
	go s.workspaceEventLoop(ctx)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	reconcileTicker := time.NewTicker(2 * time.Second)
	defer reconcileTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.closeConnections()
			s.closeAgentWorkers()
			s.closeAgentReplicas()
			s.sessions.Shutdown()
			_ = shutdownToolGateway(context.Background(), s.toolServer)
			return nil
		case event := <-s.watcher.Events:
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					_ = s.watcher.Add(event.Name)
				}
			}
			if event.Op&fsnotify.Write != 0 {
				if err := s.handleLocalChange(event.Name); err != nil {
					fmt.Printf("local change error for %s: %v\n", event.Name, err)
				}
			}
			if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				if err := s.reconcileLocalWorkspace(ctx); err != nil {
					fmt.Printf("local reconcile error: %v\n", err)
				}
			}
		case err := <-s.watcher.Errors:
			if err != nil {
				fmt.Printf("watcher error: %v\n", err)
			}
		case <-reconcileTicker.C:
			if err := s.reconcileTrackedDocuments(ctx); err != nil {
				fmt.Printf("document reconcile error: %v\n", err)
			}
		case <-ticker.C:
			if err := s.reconcileTrackedDocuments(ctx); err != nil {
				fmt.Printf("document reconcile error: %v\n", err)
			}
			if err := s.reconcileLocalWorkspace(ctx); err != nil {
				fmt.Printf("local reconcile error: %v\n", err)
			}
			if err := s.sendPresence(ctx); err != nil {
				fmt.Printf("presence error: %v\n", err)
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
	wsURL, err := url.Parse(s.cfg.BackendURL)
	if err != nil {
		return err
	}
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = "/ws"
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), nil)
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
		if shouldWakeAgentWorkersForEvent(event.Type) {
			s.wakeAllAgentWorkers()
		}
	}
}

func shouldWakeAgentWorkersForEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "workspace.snapshot", "presence.updated", "agent.run.updated":
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

	activeIDs := make(map[string]struct{}, len(workspace.Documents))
	for _, document := range workspace.Documents {
		activeIDs[document.ID] = struct{}{}
		if err := s.ensureTracked(ctx, document); err != nil {
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
		s.sessions.Reconcile(ctx, workspace.Agents)
	}
	return s.removeMissingTracked(activeIDs)
}

func (s *Service) fetchWorkspace(ctx context.Context) (*workspaceResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/workspace", nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

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

func (s *Service) removeMissingTracked(activeIDs map[string]struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for documentID, tracked := range s.projectedByID {
		if _, ok := activeIDs[documentID]; ok {
			continue
		}
		if tracked.Conn != nil {
			_ = tracked.Conn.Close()
		}
		delete(s.projectedByID, documentID)
		delete(s.projectedByPath, tracked.Path)
		if err := os.Remove(tracked.Path); err != nil && !errorsIsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Service) ensureTracked(ctx context.Context, document *document) error {
	absolutePath := filepath.Join(s.cfg.WorkspaceDir, filepath.FromSlash(document.Path))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return err
	}
	_ = s.watcher.Add(filepath.Dir(absolutePath))

	s.mu.Lock()
	tracked, exists := s.projectedByID[document.ID]
	s.mu.Unlock()

	if exists {
		tracked.StateVector = document.StateVector
		if tracked.Path != absolutePath {
			nextContent := tracked.contentString()
			if err := moveLocalFile(tracked.Path, absolutePath, nextContent); err != nil {
				return err
			}
			s.mu.Lock()
			delete(s.projectedByPath, tracked.Path)
			tracked.Path = absolutePath
			tracked.DocumentPath = document.Path
			s.projectedByPath[absolutePath] = tracked
			s.mu.Unlock()
			tracked.setProjectedContent(nextContent)
			if err := tracked.storeProjectedBase(nextContent); err != nil {
				return err
			}
		}
		if tracked.getConn() == nil {
			if err := s.connectDocument(tracked); err != nil {
				return err
			}
		}
		return nil
	}

	tracked, err := materializeTrackedFile(ctx, s.docCache, document, absolutePath)
	if err != nil {
		return err
	}
	if err := s.connectDocument(tracked); err != nil {
		return err
	}

	s.mu.Lock()
	s.projectedByPath[absolutePath] = tracked
	s.projectedByID[document.ID] = tracked
	s.mu.Unlock()
	return nil
}

func (s *Service) connectDocument(tracked *trackedFile) error {
	if current := tracked.getConn(); current != nil {
		return nil
	}
	paceDocumentConnect()
	wsURL, err := url.Parse(s.cfg.BackendURL)
	if err != nil {
		return err
	}
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = "/ws/documents/" + tracked.DocumentID
	clientID, syncStep := tracked.initialSyncState()
	wsURL.RawQuery = url.Values{
		"client_id":  {fmt.Sprintf("%d", clientID)},
		"actor_id":   {s.cfg.AgentID},
		"actor_type": {"agent"},
	}.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		return err
	}
	log.Printf("daemon ws open doc=%s url=%s", tracked.DocumentID, wsURL.String())
	tracked.setConn(conn)
	go s.readLoop(tracked, conn)
	if err := tracked.write(syncStep); err != nil {
		return err
	}
	if err := tracked.write(yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
		clientID: {
			Clock: 1,
			State: []byte(fmt.Sprintf(`{"actorId":"%s","activity":"Syncing %s"}`, s.cfg.AgentID, filepath.Base(tracked.Path))),
		},
	}, []uint64{clientID})); err != nil {
		return err
	}
	return nil
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

func (s *Service) readLoop(tracked *trackedFile, conn *websocket.Conn) {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			tracked.clearConn(conn)
			log.Printf("daemon ws read close doc=%s err=%v", tracked.DocumentID, err)
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
		if err != nil {
			continue
		}
		switch topLevel {
		case yproto.MessageSync:
			reply, _, err := handleRemoteSyncMessage(reader, tracked, "remote")
			if err != nil {
				log.Printf("daemon ws sync error doc=%s err=%v", tracked.DocumentID, err)
				continue
			}
			if len(reply) > 0 {
				_ = tracked.write(reply)
			}
		case yproto.MessageAwareness:
		}
	}
}

func handleRemoteSyncMessage(reader *bytes.Reader, tracked *trackedFile, origin any) ([]byte, bool, error) {
	messageType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		return nil, false, err
	}
	switch messageType {
	case yproto.SyncStep1:
		if tracked.cache != nil {
			doc, _, _, err := tracked.cache.loadBaseDoc(tracked.DocumentID, tracked.DocumentPath)
			if err != nil {
				return nil, false, err
			}
			reply, err := yproto.BuildSyncStep2(doc, data)
			return reply, false, err
		}
		unlockDoc := tracked.lockDoc()
		defer unlockDoc()
		if tracked.Doc == nil {
			reply, err := yproto.BuildSyncStep2(crdt.New(), data)
			return reply, false, err
		}
		reply, err := yproto.BuildSyncStep2(tracked.Doc, data)
		return reply, false, err
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
	baseKnown    bool
}

type localReconcileUpdate struct {
	state  trackedReconcileState
	update []byte
}

var errProjectedBaseDoesNotMatchCRDTState = errors.New("projected base does not match cached CRDT state")

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
	for _, documentID := range documentIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := reconcileTrackedDocument(s.docCache, documentID, trackedByDocument[documentID]); err != nil {
			return fmt.Errorf("%s: %w", documentID, err)
		}
	}
	return nil
}

func (s *Service) collectTrackedByDocument() map[string][]*trackedFile {
	result := map[string][]*trackedFile{}
	s.mu.Lock()
	for documentID, tracked := range s.projectedByID {
		result[documentID] = append(result[documentID], tracked)
	}
	replicas := make([]*workspaceReplica, 0, len(s.agentReplicas))
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

func reconcileTrackedDocument(cache *documentCache, documentID string, trackedFiles []*trackedFile) error {
	if cache == nil || documentID == "" || len(trackedFiles) == 0 {
		return nil
	}
	documentPath := trackedFiles[0].DocumentPath
	entry, unlock := cache.lockEntry(documentID)
	defer unlock()

	baseDoc, metadata, baseState, err := cache.loadBaseDocLocked(entry, documentID, documentPath)
	if err != nil {
		return err
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
	baseContent := baseDoc.GetText("content").ToString()
	states, err := collectTrackedReconcileStates(trackedFiles, baseContent)
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

	localUpdates := make([]localReconcileUpdate, 0)
	skippedLocalDirty := map[*trackedFile]bool{}
	for _, state := range states {
		if !state.localDirty {
			continue
		}
		if !state.baseKnown {
			state.tracked.markLocalDirty()
			skippedLocalDirty[state.tracked] = true
			continue
		}
		update, err := buildLocalUpdateFromBase(baseState, state.baseContent, state.localContent)
		if err != nil {
			if errors.Is(err, errProjectedBaseDoesNotMatchCRDTState) {
				state.tracked.markLocalDirty()
				skippedLocalDirty[state.tracked] = false
				continue
			}
			return err
		}
		if len(update) == 0 {
			state.tracked.clearLocalDirty()
			continue
		}
		if err := state.tracked.sendSyncUpdate(update); err != nil {
			state.tracked.markLocalDirty()
			return nil
		}
		if entry.seenUpdateHashes == nil {
			entry.seenUpdateHashes = map[string]struct{}{}
		}
		entry.seenUpdateHashes[sha256Hex(update)] = struct{}{}
		localUpdates = append(localUpdates, localReconcileUpdate{state: state, update: update})
	}
	if pendingRemoteCount == 0 && len(localUpdates) == 0 && len(skippedLocalDirty) == 0 {
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
	for _, local := range localUpdates {
		if err := crdt.ApplyUpdateV1(baseDoc, local.update, "local-reconcile"); err != nil {
			return err
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
	successfulLocal := map[*trackedFile]string{}
	for _, local := range localUpdates {
		successfulLocal[local.state.tracked] = local.state.localContent
	}
	for _, state := range states {
		if currentLocal, ok := successfulLocal[state.tracked]; ok {
			if err := projectMergedContentOverLocalDisk(state.tracked, currentLocal, finalContent); err != nil {
				return err
			}
			if state.tracked.matchesProjectedString(finalContent) {
				state.tracked.clearLocalDirty()
				state.tracked.setSyncFromScratch(false)
			} else {
				state.tracked.markLocalDirty()
			}
			continue
		}
		if canRebase, skipped := skippedLocalDirty[state.tracked]; skipped {
			if canRebase && (pendingRemoteCount > 0 || len(baseState) > 0 || metadata.StateVector == emptyDocumentStateVector) {
				if err := state.tracked.storeProjectedBase(finalContent); err != nil {
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
		if err := applyProjectedContent(state.tracked, finalContent); err != nil {
			return err
		}
		if state.tracked.matchesProjectedString(finalContent) {
			state.tracked.clearLocalDirty()
			state.tracked.setSyncFromScratch(false)
		}
	}
	return nil
}

func collectTrackedReconcileStates(trackedFiles []*trackedFile, sharedBaseContent string) ([]trackedReconcileState, error) {
	states := make([]trackedReconcileState, 0, len(trackedFiles))
	for _, tracked := range trackedFiles {
		if tracked == nil {
			continue
		}
		state := trackedReconcileState{tracked: tracked}
		baseContent, known, err := tracked.loadProjectedBase()
		if err != nil {
			return nil, err
		}
		if known {
			state.baseContent = baseContent
			state.baseKnown = true
		} else {
			state.baseContent = sharedBaseContent
		}
		if tracked.isProjecting() || !tracked.hasProjectedContent() {
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
		state.localContent = string(contentBytes)
		if tracked.isLocalDirty() || !tracked.matchesProjectedBytes(contentBytes) {
			state.localDirty = true
		}
		states = append(states, state)
	}
	return states, nil
}

func buildLocalUpdateFromBase(baseState []byte, baseContent, localContent string) ([]byte, error) {
	if baseContent == localContent {
		return nil, nil
	}
	doc := crdt.New()
	if len(baseState) > 0 {
		if err := crdt.ApplyUpdateV1(doc, baseState, "local-base"); err != nil {
			return nil, err
		}
	}
	text := doc.GetText("content")
	if text.ToString() != baseContent {
		return nil, errProjectedBaseDoesNotMatchCRDTState
	}
	replace := computeReplace(baseContent, localContent)
	var update []byte
	unsubscribe := doc.OnUpdate(func(next []byte, origin any) {
		if origin == "daemon-local-reconcile" {
			update = append([]byte(nil), next...)
		}
	})
	doc.Transact(func(txn *crdt.Transaction) {
		start, deleteLength := clampReplace(text.Len(), replace)
		if deleteLength > 0 {
			text.Delete(txn, start, deleteLength)
		}
		if replace.Text != "" {
			text.Insert(txn, start, replace.Text, nil)
		}
	}, "daemon-local-reconcile")
	unsubscribe()
	return update, nil
}

func (s *Service) handleLocalChange(path string) error {
	s.mu.Lock()
	tracked, ok := s.projectedByPath[path]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return markTrackedLocalDirty(tracked, path)
}

func applyProjectedContent(tracked *trackedFile, nextContent string) error {
	previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(nextContent)
	if err := writeProjectedFile(tracked.Path, nextContent, previousHash); err != nil {
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		if errors.Is(err, errProjectedFileDiverged) {
			return nil
		}
		return err
	}
	if err := tracked.storeProjectedBase(nextContent); err != nil {
		return err
	}
	tracked.setSyncFromScratch(false)
	return nil
}

func handleTrackedLocalChange(tracked *trackedFile, path string) ([]byte, error) {
	if tracked.isProjecting() {
		return nil, nil
	}
	if !tracked.hasProjectedContent() {
		return nil, nil
	}
	contentBytes, err := readFileLocked(path)
	if err != nil {
		return nil, err
	}
	if tracked.matchesProjectedBytes(contentBytes) {
		return nil, nil
	}
	current := string(contentBytes)
	unlockDoc := tracked.lockDoc()
	defer unlockDoc()
	docContent := tracked.Doc.GetText("content").ToString()
	if current == docContent {
		tracked.setProjectedContent(current)
		if err := tracked.storeProjectedBase(current); err != nil {
			return nil, err
		}
		return nil, nil
	}
	baseContent, known, err := tracked.loadProjectedBase()
	if err != nil {
		return nil, err
	}
	if !known {
		baseContent = docContent
	}
	replace := computeReplace(baseContent, current)
	var update []byte
	text := tracked.Doc.GetText("content")
	unsubscribe := tracked.Doc.OnUpdate(func(next []byte, origin any) {
		if origin == "daemon-local" {
			update = append([]byte(nil), next...)
		}
	})
	tracked.Doc.Transact(func(txn *crdt.Transaction) {
		start, deleteLength := clampReplace(text.Len(), replace)
		if deleteLength > 0 {
			text.Delete(txn, start, deleteLength)
		}
		if replace.Text != "" {
			text.Insert(txn, start, replace.Text, nil)
		}
	}, "daemon-local")
	unsubscribe()

	mergedContent := text.ToString()
	if err := tracked.persistCachedStateLocked(); err != nil {
		return nil, err
	}
	if mergedContent == current {
		tracked.setProjectedContent(current)
		if err := tracked.storeProjectedBase(current); err != nil {
			return nil, err
		}
		return update, nil
	}
	if err := projectMergedContentOverLocalDisk(tracked, current, mergedContent); err != nil {
		return nil, err
	}
	return update, nil
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

func projectMergedContentOverLocalDisk(tracked *trackedFile, currentDiskContent, mergedContent string) error {
	previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(mergedContent)
	if err := writeProjectedFile(tracked.Path, mergedContent, projectedHashString(currentDiskContent)); err != nil {
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		if errors.Is(err, errProjectedFileDiverged) {
			return nil
		}
		return err
	}
	if err := tracked.storeProjectedBase(mergedContent); err != nil {
		return err
	}
	tracked.setSyncFromScratch(false)
	return nil
}

func materializeTrackedFile(ctx context.Context, cache *documentCache, document *document, absolutePath string) (*trackedFile, error) {
	tracked := &trackedFile{
		DocumentID:        document.ID,
		DocumentPath:      document.Path,
		Path:              absolutePath,
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
			_, readErr := readFileLocked(absolutePath)
			if readErr == nil {
				if err := tracked.storeProjectedBase(text); err != nil {
					return nil, err
				}
				// Existing disk bytes are only clean if they match a CRDT-derived base.
				// Otherwise reconciliation must treat them as local edits.
				tracked.setProjectedContent(text)
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
			if err := tracked.storeProjectedBase(text); err != nil {
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

func (s *Service) reconcileLocalWorkspace(ctx context.Context) error {
	actualFiles, err := scanWorkspaceFiles(s.cfg.WorkspaceDir)
	if err != nil {
		return err
	}

	s.mu.Lock()
	trackedFiles := make([]*trackedFile, 0, len(s.projectedByID))
	for _, tracked := range s.projectedByID {
		trackedFiles = append(trackedFiles, tracked)
	}
	s.mu.Unlock()

	sort.Slice(trackedFiles, func(i, j int) bool { return trackedFiles[i].Path < trackedFiles[j].Path })

	remaining := make(map[string]string, len(actualFiles))
	for path, content := range actualFiles {
		remaining[path] = content
	}

	changed := false
	for _, tracked := range trackedFiles {
		current, exists := remaining[tracked.Path]
		if exists {
			delete(remaining, tracked.Path)
		}
		if !tracked.hasProjectedContent() {
			continue
		}
		if tracked.isProjecting() {
			// File projection can briefly look like a local edit, move, or delete.
			continue
		}
		if exists {
			if !tracked.matchesProjectedString(current) {
				if err := s.handleLocalChange(tracked.Path); err != nil {
					return err
				}
			}
			continue
		}

		nextPath, foundMove := findMovedPath(remaining, tracked.matchesProjectedString)
		if foundMove {
			if err := s.moveRemoteDocument(ctx, tracked.DocumentID, workspaceRelativePath(s.cfg.WorkspaceDir, nextPath)); err != nil {
				return err
			}
			delete(remaining, nextPath)
			changed = true
			continue
		}

		if err := s.deleteRemoteDocument(ctx, tracked.DocumentID); err != nil {
			return err
		}
		changed = true
	}

	newPaths := make([]string, 0, len(remaining))
	for path := range remaining {
		newPaths = append(newPaths, path)
	}
	sort.Strings(newPaths)
	for _, path := range newPaths {
		if err := s.createRemoteDocument(ctx, workspaceRelativePath(s.cfg.WorkspaceDir, path), remaining[path]); err != nil {
			return err
		}
		changed = true
	}

	if changed {
		return s.refresh(ctx)
	}
	return nil
}

func (s *Service) closeConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tracked := range s.projectedByID {
		if conn := tracked.getConn(); conn != nil {
			_ = conn.Close()
		}
	}
}

func (s *Service) sendPresence(ctx context.Context) error {
	payload, err := json.Marshal(upsertPresenceRequest{
		ActorID:   s.cfg.AgentID,
		ActorType: "agent",
		FilePath:  "",
		Mode:      "syncing",
		Activity:  "materializing workspace",
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BackendURL+"/api/presence", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = s.client.Do(req)
	return err
}

func (s *Service) createRemoteDocument(ctx context.Context, path, content string) error {
	payload, err := json.Marshal(createDocumentRequest{Path: path, Content: content})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BackendURL+"/api/documents?actor="+url.QueryEscape(s.cfg.AgentID)+"&actor_type=agent", bytes.NewReader(payload))
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
		return fmt.Errorf("create document failed: %s", res.Status)
	}
	return nil
}

func (s *Service) moveRemoteDocument(ctx context.Context, documentID, path string) error {
	payload, err := json.Marshal(updateDocumentRequest{Path: path})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, s.cfg.BackendURL+"/api/documents/"+documentID+"?actor="+url.QueryEscape(s.cfg.AgentID)+"&actor_type=agent", bytes.NewReader(payload))
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
		return fmt.Errorf("move document failed: %s", res.Status)
	}
	return nil
}

func (s *Service) deleteRemoteDocument(ctx context.Context, documentID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.cfg.BackendURL+"/api/documents/"+documentID+"?actor="+url.QueryEscape(s.cfg.AgentID)+"&actor_type=agent", nil)
	if err != nil {
		return err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("delete document failed: %s", res.Status)
	}
	return nil
}

func (s *Service) updateRemoteAgentSession(ctx context.Context, agentID string, update updateAgentSessionRequest) error {
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, s.cfg.BackendURL+"/api/agents/"+agentID+"/session?actor="+url.QueryEscape(s.cfg.AgentID)+"&actor_type=agent", bytes.NewReader(payload))
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
	return clientID, yproto.BuildSyncStep1(t.Doc)
}

func nextAwarenessClientID() uint64 {
	return uint64(time.Now().UnixNano()) + awarenessClientCounter.Add(1)
}

func (t *trackedFile) contentString() string {
	if base, ok, err := t.loadProjectedBase(); err == nil && ok {
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

func (t *trackedFile) persistCachedStateLocked() error {
	if t.cache == nil {
		return nil
	}
	if t.Doc == nil {
		return nil
	}
	return t.cache.maybeStoreDoc(t.DocumentID, t.DocumentPath, 0, t.Doc)
}

func (t *trackedFile) storeProjectedBase(content string) error {
	if t.cache == nil {
		return nil
	}
	return t.cache.storeProjectedBase(t.DocumentID, t.projectionCacheKey(), content)
}

func (t *trackedFile) loadProjectedBase() (string, bool, error) {
	if t.cache == nil {
		return "", false, nil
	}
	return t.cache.loadProjectedBase(t.DocumentID, t.projectionCacheKey())
}

func (t *trackedFile) projectionCacheKey() string {
	if t.Path != "" {
		return t.Path
	}
	if t.DocumentPath != "" {
		return t.DocumentPath
	}
	return t.DocumentID
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

func errorsIsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
