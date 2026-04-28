package syncer

import (
	"bytes"
	"context"
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
	Doc                   *crdt.Doc
	docMu                 *sync.Mutex
	AwarenessClientID     uint64
	cache                 *documentCache
	cacheEntry            *documentCacheEntry
	Conn                  *websocket.Conn
	connMu                sync.Mutex
	stateMu               sync.Mutex
	projecting            int
	hash                  projectedContentHash
	projectedContent      string
	projectedContentKnown bool
}

type projectedContentHash struct {
	size int
	sum  uint64
}

var projectedHashSeed = maphash.MakeSeed()
var awarenessClientCounter atomic.Uint64

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
		case <-ticker.C:
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
		nextContent := tracked.contentString()
		if tracked.Path != absolutePath {
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
			unlockDoc := tracked.lockDoc()
			reply, changed, err := yproto.ReadSyncMessage(reader, tracked.Doc, "remote")
			var nextContent string
			var persistErr error
			if changed {
				nextContent = tracked.Doc.GetText("content").ToString()
				persistErr = tracked.persistCachedStateLocked()
			}
			unlockDoc()
			if err != nil {
				log.Printf("daemon ws sync error doc=%s err=%v", tracked.DocumentID, err)
				continue
			}
			if persistErr != nil {
				log.Printf("daemon cache persist error doc=%s err=%v", tracked.DocumentID, persistErr)
			}
			if len(reply) > 0 {
				_ = tracked.write(reply)
			}
			if changed {
				if err := applyProjectedContent(tracked, nextContent); err != nil {
					log.Printf("daemon local projection error doc=%s err=%v", tracked.DocumentID, err)
				}
			}
		case yproto.MessageAwareness:
		}
	}
}

func (s *Service) handleLocalChange(path string) error {
	s.mu.Lock()
	tracked, ok := s.projectedByPath[path]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	update, err := handleTrackedLocalChange(tracked, path)
	if err != nil {
		return err
	}
	if len(update) == 0 || tracked.getConn() == nil {
		return nil
	}
	if err := tracked.write(yproto.BuildSyncUpdate(update)); err != nil {
		log.Printf("daemon ws write error doc=%s err=%v", tracked.DocumentID, err)
		return err
	}
	return nil
}

func applyProjectedContent(tracked *trackedFile, nextContent string) error {
	previousContent, previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(nextContent)
	if err := writeProjectedFile(tracked.Path, nextContent, previousHash); err != nil {
		tracked.setProjectedSnapshot(previousContent, previousHash, previousKnown)
		if errors.Is(err, errProjectedFileDiverged) {
			return nil
		}
		return err
	}
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
		return nil, nil
	}
	baseContent, _, known := tracked.projectedSnapshot()
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
		return update, nil
	}
	if err := projectMergedContentOverLocalDisk(tracked, current, mergedContent); err != nil {
		return nil, err
	}
	return update, nil
}

func projectMergedContentOverLocalDisk(tracked *trackedFile, currentDiskContent, mergedContent string) error {
	previousContent, previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(mergedContent)
	if err := writeProjectedFile(tracked.Path, mergedContent, projectedHashString(currentDiskContent)); err != nil {
		tracked.setProjectedSnapshot(previousContent, previousHash, previousKnown)
		if errors.Is(err, errProjectedFileDiverged) {
			return nil
		}
		return err
	}
	return nil
}

func materializeTrackedFile(ctx context.Context, cache *documentCache, document *document, absolutePath string) (*trackedFile, error) {
	materialized, err := cache.materialize(ctx, document)
	if err != nil {
		return nil, err
	}
	tracked := &trackedFile{
		DocumentID:        document.ID,
		DocumentPath:      document.Path,
		Path:              absolutePath,
		Doc:               materialized.Doc,
		docMu:             materialized.DocMu,
		AwarenessClientID: nextAwarenessClientID(),
		cache:             cache,
		cacheEntry:        materialized.Entry,
	}
	if materialized.ContentKnown {
		if err := applyProjectedContent(tracked, materialized.Content); err != nil {
			return nil, err
		}
	} else {
		tracked.setProjectedSnapshot("", projectedContentHash{}, false)
	}
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
				changed = true
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
	unlockDoc := t.lockDoc()
	defer unlockDoc()
	clientID := t.AwarenessClientID
	if clientID == 0 {
		clientID = uint64(t.Doc.ClientID())
	}
	return clientID, yproto.BuildSyncStep1(t.Doc)
}

func nextAwarenessClientID() uint64 {
	return uint64(time.Now().UnixNano()) + awarenessClientCounter.Add(1)
}

func (t *trackedFile) contentString() string {
	unlockDoc := t.lockDoc()
	defer unlockDoc()
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
	return t.cache.maybeStoreDoc(t.DocumentID, t.DocumentPath, 0, t.Doc)
}

func (t *trackedFile) projectedHash() projectedContentHash {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.hash
}

func (t *trackedFile) projectedSnapshot() (string, projectedContentHash, bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.projectedContent, t.hash, t.projectedContentKnown
}

func (t *trackedFile) hasProjectedContent() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.projectedContentKnown
}

func (t *trackedFile) setProjectedContent(content string) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.projectedContent = strings.Clone(content)
	t.projectedContentKnown = true
	t.hash = projectedHashString(content)
}

func (t *trackedFile) setProjectedHash(hash projectedContentHash) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.hash = hash
	t.projectedContent = ""
	t.projectedContentKnown = false
}

func (t *trackedFile) setProjectedSnapshot(content string, hash projectedContentHash, known bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.hash = hash
	t.projectedContentKnown = known
	if known {
		t.projectedContent = strings.Clone(content)
		return
	}
	t.projectedContent = ""
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
