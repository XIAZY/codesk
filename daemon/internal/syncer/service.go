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
	primaryRuntime  *workspaceRuntime
	agentRuntimes   map[string]*managedWorkspaceRuntime
	agentWorkers    map[string]*managedAgentWorker
	latestWorkspace *workspaceResponse
}

type managedWorkspaceRuntime struct {
	runtime *workspaceRuntime
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
	ActorID               string
	ActorType             string
	FS                    *WorkspaceFS
	Owner                 *workspaceReplica
	Doc                   *crdt.Doc
	docMu                 *sync.Mutex
	cache                 *documentCache
	cacheEntry            *documentCacheEntry
	stateMu               sync.Mutex
	projecting            int
	localDirty            bool
	localDeleted          bool
	localMoved            bool
	remoteDeleted         bool
	hash                  projectedContentHash
	projectedContentKnown bool
	projectedContent      string
	projectedState        []byte
}

type projectedContentHash struct {
	size int
	sum  uint64
}

var projectedHashSeed = maphash.MakeSeed()
var awarenessClientCounter atomic.Uint64
var documentConnectMu sync.Mutex
var nextDocumentConnect time.Time
var trackedProjectedBaseCacheMaxBytes = 8 * 1024 * 1024

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
	RootDocumentID  string        `json:"rootDocumentId"`
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

type localCreateCandidate struct {
	Root      string
	Path      string
	ActorID   string
	ActorType string
}

type localCreateQueue struct {
	mu         sync.Mutex
	candidates map[string]localCreateCandidate
}

func newLocalCreateQueue() *localCreateQueue {
	return &localCreateQueue{candidates: map[string]localCreateCandidate{}}
}

func (q *localCreateQueue) Mark(candidate localCreateCandidate) {
	if q == nil || strings.TrimSpace(candidate.Path) == "" {
		return
	}
	q.mu.Lock()
	q.candidates[candidate.Path] = candidate
	q.mu.Unlock()
}

func (q *localCreateQueue) Drain() []localCreateCandidate {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	candidates := make([]localCreateCandidate, 0, len(q.candidates))
	for _, candidate := range q.candidates {
		candidates = append(candidates, candidate)
	}
	q.candidates = map[string]localCreateCandidate{}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates
}

func New(cfg Config) (*Service, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	primaryRuntime, err := newWorkspaceRuntime(cfg, client, cfg.WorkspaceDir, cfg.AgentID, "daemon")
	if err != nil {
		return nil, err
	}
	service := &Service{
		cfg:            cfg,
		client:         client,
		primaryRuntime: primaryRuntime,
		agentRuntimes:  map[string]*managedWorkspaceRuntime{},
		agentWorkers:   map[string]*managedAgentWorker{},
	}
	service.sessions = newAgentSessionSupervisor(cfg, service.updateRemoteAgentSession, nil)
	service.sessions.SetIdleWake(service.wakeAgentWorker)
	return service, nil
}

func (s *Service) ensurePrimaryRuntime() error {
	if s == nil {
		return nil
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 30 * time.Second}
	}
	if s.primaryRuntime != nil {
		return nil
	}
	runtime, err := newWorkspaceRuntime(s.cfg, s.client, s.cfg.WorkspaceDir, s.cfg.AgentID, "daemon")
	if err != nil {
		return err
	}
	s.primaryRuntime = runtime
	return nil
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
	if err := s.ensurePrimaryRuntime(); err != nil {
		_ = shutdownToolGateway(context.Background(), s.toolServer)
		return err
	}
	if err := s.refreshInitialWorkspace(ctx); err != nil {
		s.closeAgentWorkers()
		s.closeAgentRuntimes()
		if s.sessions != nil {
			s.sessions.Shutdown()
		}
		_ = shutdownToolGateway(context.Background(), s.toolServer)
		return err
	}
	primaryCtx, cancelPrimary := context.WithCancel(ctx)
	defer cancelPrimary()
	go func() {
		if err := s.primaryRuntime.Run(primaryCtx); err != nil && primaryCtx.Err() == nil {
			log.Printf("primary workspace runtime error: %v", err)
		}
	}()
	go s.workspaceEventLoop(ctx)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancelPrimary()
			s.closeAgentWorkers()
			s.closeAgentRuntimes()
			if s.sessions != nil {
				s.sessions.Shutdown()
			}
			_ = shutdownToolGateway(context.Background(), s.toolServer)
			return nil
		case <-ticker.C:
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
	case "workspace.snapshot", "presence.updated", "agent.run.updated", "thread.created", "thread.updated", "thread.message.created", "agent.event.updated":
		return false
	default:
		return true
	}
}

func shouldRefreshForEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "workspace.snapshot", "presence.updated", "agent.run.updated":
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

	if err := s.ensurePrimaryRuntime(); err != nil {
		return err
	}
	if s.primaryRuntime != nil {
		if err := s.primaryRuntime.applyWorkspace(ctx, workspace); err != nil {
			return err
		}
	}
	if err := s.syncAgentRuntimes(ctx, workspace); err != nil {
		return err
	}
	s.mu.Lock()
	s.latestWorkspace = workspace
	s.mu.Unlock()
	if err := s.syncAgentWorkers(ctx, workspace.Agents); err != nil {
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

func (s *workspaceRuntime) processPathChanges(ctx context.Context) (bool, error) {
	if s == nil || s.replica == nil {
		return false, nil
	}
	return s.replica.drainPathChanges(ctx, time.Now())
}

func (s *workspaceRuntime) processLocalCreates(ctx context.Context) error {
	if s == nil || s.localCreates == nil {
		return nil
	}
	candidates := s.localCreates.Drain()
	if len(candidates) == 0 {
		return nil
	}
	workspace, err := s.fetchWorkspace(ctx)
	if err != nil {
		for _, candidate := range candidates {
			s.markLocalCreate(candidate)
		}
		return err
	}
	if err := s.applyWorkspace(ctx, workspace); err != nil {
		for _, candidate := range candidates {
			s.markLocalCreate(candidate)
		}
		return err
	}
	if s.rootDocumentID != "" {
		if err := s.reconcileRootNamespace(ctx); err != nil {
			for _, candidate := range candidates {
				s.markLocalCreate(candidate)
			}
			return err
		}
	}
	desiredPaths, err := s.currentRootDesiredPaths()
	if err != nil {
		for _, candidate := range candidates {
			s.markLocalCreate(candidate)
		}
		return err
	}
	created := false
	var firstErr error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			s.markLocalCreate(candidate)
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			continue
		}
		relativePath, valid, err := s.validateLocalCreateCandidate(candidate, desiredPaths)
		if err != nil {
			s.markLocalCreate(candidate)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !valid {
			continue
		}
		document, err := s.createDocumentFromLocalCandidate(ctx, candidate, relativePath)
		if err != nil {
			s.markLocalCreate(candidate)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if document != nil {
			created = true
			if s.rootDocumentID != "" {
				if err := s.upsertRootFileEntry(ctx, document.ID, relativePath, candidate.ActorID, candidate.ActorType); err != nil {
					s.markLocalCreate(candidate)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				desiredPaths[relativePath] = struct{}{}
				s.markDocumentDirty(s.rootDocumentID)
			}
			s.markDocumentDirty(document.ID)
		}
	}
	if created {
		if err := s.reconcileRootNamespace(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *workspaceRuntime) validateLocalCreateCandidate(candidate localCreateCandidate, desiredPaths map[string]struct{}) (string, bool, error) {
	path := strings.TrimSpace(candidate.Path)
	root := strings.TrimSpace(candidate.Root)
	if root == "" || path == "" || isIgnoredWorkspaceAbsolutePath(root, path) {
		return "", false, nil
	}
	fs := NewWorkspaceFS(candidate.Root)
	snapshot, err := fs.Read(candidate.Path)
	if err != nil {
		return "", false, err
	}
	if !snapshot.Exists {
		return "", false, nil
	}
	relativePath := workspaceRelativePath(candidate.Root, candidate.Path)
	if relativePath == "" || relativePath == "." || isIgnoredDocumentPath(relativePath) {
		return "", false, nil
	}
	if _, ok := desiredPaths[relativePath]; ok {
		return "", false, nil
	}
	if s != nil && s.replica != nil {
		s.replica.mu.Lock()
		_, claimedPath := s.replica.projectedByPath[path]
		_, claimedDesiredPath := s.replica.projectedByPath[filepath.Join(root, filepath.FromSlash(relativePath))]
		for _, tracked := range s.replica.projectedByID {
			if tracked != nil && tracked.DocumentPath == relativePath {
				claimedPath = true
				break
			}
		}
		s.replica.mu.Unlock()
		if claimedPath || claimedDesiredPath {
			return "", false, nil
		}
	}
	return relativePath, true, nil
}

func (s *workspaceRuntime) createDocumentFromLocalCandidate(ctx context.Context, candidate localCreateCandidate, relativePath string) (*document, error) {
	fs := NewWorkspaceFS(candidate.Root)
	req, err := s.newBackendRequest(ctx, http.MethodPost, "/api/documents", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(candidate.ActorType) == "agent" && strings.TrimSpace(candidate.ActorID) != "" {
		applyBackendAuth(req.Header, s.cfg, candidate.ActorID)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return nil, &backendStatusError{Method: req.Method, URL: req.URL.String(), StatusCode: res.StatusCode, Body: string(body)}
	}
	var created document
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		return nil, err
	}
	created.Path = relativePath
	projectedSeq := int64(0)
	if s != nil && s.docCache != nil {
		doc := crdt.New()
		if err := s.docCache.storeDoc(created.ID, created.Path, created.UpdateID, doc); err != nil {
			doc.Close()
			return nil, err
		}
		doc.Close()
		seq, err := s.docCache.documentAppliedSeq(created.ID)
		if err != nil {
			return nil, err
		}
		projectedSeq = seq
	}
	tracked := &trackedFile{
		DocumentID:    created.ID,
		DocumentPath:  created.Path,
		Path:          candidate.Path,
		WorkspaceRoot: candidate.Root,
		FS:            fs,
		cache:         s.docCache,
	}
	if err := tracked.storeProjectedBaseAtSeq("", crdtStateFromContent(""), projectedSeq); err != nil {
		return nil, err
	}
	return &created, nil
}

func (s *Service) syncAgentWorkers(ctx context.Context, agents []*agent) error {
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
var errDocumentRemovedDuringReconcile = errors.New("document removed during reconciliation")

func (s *workspaceRuntime) reconcileDirtyDocuments(ctx context.Context) error {
	if s.reconcileQueue == nil {
		return nil
	}
	return s.reconcileDocumentIDs(ctx, s.reconcileQueue.Drain())
}

func (s *workspaceRuntime) reconcileTrackedDocuments(ctx context.Context) error {
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

func (s *workspaceRuntime) reconcileDocumentIDs(ctx context.Context, documentIDs []string) error {
	if s.docCache == nil || len(documentIDs) == 0 {
		return nil
	}
	trackedByDocument := s.collectTrackedByDocument()
	sort.Strings(documentIDs)
	documentIDs = s.rootFirstDocumentIDs(documentIDs)
	return s.reconcileDocumentIDsWithTracked(ctx, documentIDs, trackedByDocument)
}

func (s *workspaceRuntime) rootFirstDocumentIDs(documentIDs []string) []string {
	if s == nil || s.rootDocumentID == "" || len(documentIDs) < 2 {
		return documentIDs
	}
	rootIndex := -1
	for index, documentID := range documentIDs {
		if documentID == s.rootDocumentID {
			rootIndex = index
			break
		}
	}
	if rootIndex <= 0 {
		return documentIDs
	}
	ordered := make([]string, 0, len(documentIDs))
	ordered = append(ordered, documentIDs[rootIndex])
	ordered = append(ordered, documentIDs[:rootIndex]...)
	ordered = append(ordered, documentIDs[rootIndex+1:]...)
	return ordered
}

func (s *workspaceRuntime) reconcileDocumentIDsWithTracked(ctx context.Context, documentIDs []string, trackedByDocument map[string][]*trackedFile) error {
	if s.docCache == nil || len(documentIDs) == 0 {
		return nil
	}
	documentIDs = s.rootFirstDocumentIDs(documentIDs)
	var firstErr error
	for index, documentID := range documentIDs {
		if ctx.Err() != nil {
			for _, remainingID := range documentIDs[index:] {
				s.markDocumentDirty(remainingID)
			}
			return ctx.Err()
		}
		if s.rootDocumentID != "" && documentID == s.rootDocumentID {
			if err := s.reconcileRootNamespace(ctx); err != nil {
				s.markDocumentDirty(documentID)
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", documentID, err)
				}
			} else {
				trackedByDocument = s.collectTrackedByDocument()
			}
			continue
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

func (s *workspaceRuntime) collectTrackedByDocument() map[string][]*trackedFile {
	result := map[string][]*trackedFile{}
	if s == nil || s.replica == nil {
		return result
	}
	s.replica.mu.Lock()
	for documentID, tracked := range s.replica.projectedByID {
		result[documentID] = append(result[documentID], tracked)
	}
	s.replica.mu.Unlock()
	return result
}

func (s *workspaceRuntime) markDocumentDirty(documentID string) {
	if s != nil && s.reconcileQueue != nil {
		s.reconcileQueue.Mark(documentID)
	}
}

func (s *workspaceRuntime) documentNeedsReconcile(documentID string, trackedFiles []*trackedFile) bool {
	if documentID == "" {
		return false
	}
	if hasTrackedLocalDirty(trackedFiles) || hasTrackedLocalMetadataWork(trackedFiles) {
		return true
	}
	if s == nil || s.docCache == nil {
		return false
	}
	entry, unlock := s.docCache.lockEntry(documentID)
	defer unlock()
	outboxes, err := s.docCache.loadOutboxUpdatesLocked(entry, documentID)
	if err != nil || len(outboxes) > 0 {
		return true
	}
	count, err := s.docCache.pendingRemoteUpdateCountLocked(entry, documentID)
	if err != nil || count > 0 {
		return true
	}
	threadCount, err := s.docCache.materializableThreadIntentCountLocked(entry, documentID)
	return err != nil || threadCount > 0
}

func (s *workspaceRuntime) reconcileTrackedDocument(ctx context.Context, documentID string, trackedFiles []*trackedFile) error {
	if s == nil || s.docCache == nil || documentID == "" || len(trackedFiles) == 0 {
		return nil
	}
	cache := s.docCache
	documentPath := trackedFiles[0].DocumentPath
	var flushRecords []outboxUpdateRecord
	if err := func() error {
		entry, unlock := cache.lockEntry(documentID)
		defer unlock()

		outboxes, err := cache.loadOutboxUpdatesLocked(entry, documentID)
		if err != nil {
			return err
		}
		if len(outboxes) > 0 {
			flushRecords = outboxes
			return nil
		}
		pendingRemoteCount, err := cache.pendingRemoteUpdateCountLocked(entry, documentID)
		if err != nil {
			return err
		}
		pendingThreadCount, err := cache.pendingThreadIntentCountLocked(entry, documentID)
		if err != nil {
			return err
		}
		if pendingRemoteCount == 0 && pendingThreadCount == 0 && !trackedFilesHaveLocalReconcileWork(trackedFiles) {
			return nil
		}

		states, err := collectTrackedReconcileStates(trackedFiles)
		if err != nil {
			return err
		}
		handledMetadata, err := s.reconcileLocalMetadataOperations(ctx, cache, entry, documentID, states)
		if err != nil {
			if errors.Is(err, errDocumentRemovedDuringReconcile) {
				return nil
			}
			return err
		}
		if handledMetadata {
			return nil
		}

		missingBase := map[*trackedFile]trackedReconcileState{}
		projectOnlyDirty := map[*trackedFile]struct{}{}
		records := make([]outboxUpdateRecord, 0)
		for _, state := range states {
			if state.baseMissing {
				missingBase[state.tracked] = state
				continue
			}
			if !state.localDirty {
				continue
			}
			localContent := state.localContent
			update, observedState, err := buildLocalUpdateFromBase(state.baseState, state.baseContent, localContent)
			if err != nil {
				if errors.Is(err, errUnsupportedTextContent) {
					log.Printf("document reconcile skipped unsupported text content: document_id=%s document_path=%q local_path=%q reason=%s", state.tracked.DocumentID, state.tracked.DocumentPath, state.tracked.Path, unsupportedTextContentReason(err))
					state.tracked.markLocalDirty()
					continue
				}
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
			actorID, actorType := s.actorForTracked(state.tracked)
			record := outboxUpdateRecord{
				Update:          update,
				ObservedContent: state.localContent,
				ObservedState:   observedState,
				SourcePath:      state.tracked.Path,
				ActorID:         actorID,
				ActorType:       actorType,
				CreatedAt:       time.Now().UTC(),
			}
			records = append(records, record)
			state.tracked.markLocalDirty()
		}
		if len(records) > 0 {
			if err := cache.storeOutboxUpdatesLocked(entry, documentID, documentPath, records); err != nil {
				return err
			}
			flushRecords = records
			return nil
		}

		baseDoc, baseMetadata, baseState, err := cache.loadBaseDocLocked(entry, documentID, documentPath)
		if err != nil {
			return err
		}
		cacheContentKnown := baseState != nil
		projectedSeq := baseMetadata.AppliedSeq
		readyThreads, err := s.materializeThreadIntentsLocked(ctx, cache, entry, documentID, documentPath, baseDoc, cacheContentKnown)
		if err != nil {
			return err
		}
		if readyThreads > 0 {
			s.wakeThreadDelivery()
		}

		if pendingRemoteCount > 0 {
			nextProjectedSeq, err := applyPendingRemoteUpdatesLocked(cache, entry, documentID, baseDoc)
			if err != nil {
				return err
			}
			projectedSeq = nextProjectedSeq
			baseState = baseDoc.EncodeStateAsUpdate()
			cacheContentKnown = baseState != nil
		}

		hasReconcileWork := pendingRemoteCount > 0 || len(missingBase) > 0 || len(projectOnlyDirty) > 0
		for _, state := range states {
			if !state.fileExists && state.baseKnown {
				hasReconcileWork = true
				break
			}
			if state.tracked != nil {
				if desiredPath := state.tracked.desiredPath(); desiredPath != "" && desiredPath != state.tracked.Path {
					hasReconcileWork = true
					break
				}
			}
		}
		if !hasReconcileWork {
			return nil
		}

		finalContent := baseDoc.GetText("content").ToString()
		finalState := baseDoc.EncodeStateAsUpdate()
		for _, state := range states {
			pathReady, err := reconcileTrackedPathForProjection(state)
			if err != nil {
				return err
			}
			if !pathReady {
				state.tracked.markLocalDirty()
				continue
			}
			if _, ok := missingBase[state.tracked]; ok {
				if state.fileExists {
					if err := archiveUnknownWorkingCopy(state.tracked); err != nil {
						return err
					}
				}
				state.tracked.setProjectedSnapshot(projectedContentHash{}, false)
				if cacheContentKnown {
					clean, err := applyProjectedContent(state.tracked, finalContent, finalState, projectedSeq)
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
				clean, err := applyProjectedContent(state.tracked, finalContent, finalState, projectedSeq)
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
			clean, err := applyProjectedContent(state.tracked, finalContent, finalState, projectedSeq)
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
	}(); err != nil {
		return err
	}
	if len(flushRecords) > 0 {
		return s.flushOutboxUpdates(ctx, cache, documentID, documentPath, trackedFiles, flushRecords)
	}
	return nil
}

func (s *workspaceRuntime) reconcileLocalMetadataOperations(ctx context.Context, cache *documentCache, entry *documentCacheEntry, documentID string, states []trackedReconcileState) (bool, error) {
	for _, state := range states {
		tracked := state.tracked
		if tracked == nil || !tracked.isRemoteDeleted() {
			continue
		}
		if err := cleanupRemovedDocument(cache, entry, documentID, states); err != nil {
			return false, err
		}
		return true, errDocumentRemovedDuringReconcile
	}
	for _, state := range states {
		tracked := state.tracked
		if tracked == nil || !tracked.isLocalDeleted() {
			continue
		}
		if !state.baseKnown {
			tracked.clearLocalDeleted()
			tracked.clearLocalDirty()
			continue
		}
		actorID, actorType := s.actorForTracked(tracked)
		if err := s.tombstoneRootFileEntry(ctx, tracked.DocumentID, actorID, actorType); err != nil {
			return false, err
		}
		tracked.untrack()
		s.markDocumentDirty(s.rootDocumentID)
		return true, errDocumentRemovedDuringReconcile
	}
	for _, state := range states {
		tracked := state.tracked
		if tracked == nil || !tracked.isLocalMoved() {
			continue
		}
		if state.localDirty && state.localContent != state.baseContent {
			continue
		}
		nextPath := workspaceRelativePath(tracked.WorkspaceRoot, tracked.Path)
		if nextPath == "" || nextPath == "." || nextPath == tracked.DocumentPath {
			tracked.clearLocalMoved()
			continue
		}
		actorID, actorType := s.actorForTracked(tracked)
		if err := s.upsertRootFileEntry(ctx, tracked.DocumentID, nextPath, actorID, actorType); err != nil {
			return false, err
		}
		tracked.DocumentPath = nextPath
		if cache != nil {
			if _, err := cache.ensureDocument(tracked.DocumentID, nextPath); err != nil {
				return false, err
			}
		}
		tracked.clearLocalMoved()
		tracked.clearLocalDirty()
		s.markDocumentDirty(s.rootDocumentID)
		return true, nil
	}
	return false, nil
}

func cleanupRemovedDocument(cache *documentCache, entry *documentCacheEntry, documentID string, states []trackedReconcileState) error {
	for _, state := range states {
		tracked := state.tracked
		if tracked == nil {
			continue
		}
		if state.fileExists {
			if state.baseKnown && !state.localDirty {
				if err := tracked.workspaceFS().DeleteIfUnchanged(tracked.Path, projectedHashString(state.baseContent)); err != nil {
					if errors.Is(err, ErrUnsafeDelete) {
						if _, archiveErr := tracked.workspaceFS().Archive(tracked.Path, safeDocumentCacheName(documentID)); archiveErr != nil {
							return archiveErr
						}
					} else {
						return err
					}
				}
			} else if _, err := tracked.workspaceFS().Archive(tracked.Path, safeDocumentCacheName(documentID)); err != nil {
				return err
			}
		}
		tracked.untrack()
	}
	if cache != nil && entry != nil {
		if err := cache.removeDocumentLocked(entry, documentID); err != nil {
			return err
		}
	}
	return nil
}

func reconcileTrackedPathForProjection(state trackedReconcileState) (bool, error) {
	tracked := state.tracked
	if tracked == nil || tracked.isLocalMoved() {
		return !state.localDirty, nil
	}
	desiredPath := tracked.desiredPath()
	if desiredPath == "" || desiredPath == tracked.Path {
		return true, nil
	}
	if state.localDirty {
		return false, nil
	}
	fs := tracked.workspaceFS()
	if state.fileExists {
		if state.baseKnown && projectedHashString(state.baseContent) == projectedHashString(state.localContent) {
			if err := fs.MoveIfNoTarget(tracked.Path, desiredPath); err != nil {
				if errors.Is(err, ErrPathCollision) {
					if _, archiveErr := fs.Archive(desiredPath, safeDocumentCacheName(tracked.DocumentID)+"_collision"); archiveErr != nil {
						return false, archiveErr
					}
					if retryErr := fs.MoveIfNoTarget(tracked.Path, desiredPath); retryErr != nil {
						return false, retryErr
					}
				} else {
					return false, err
				}
			}
		} else if _, err := fs.Archive(tracked.Path, safeDocumentCacheName(tracked.DocumentID)); err != nil {
			return false, err
		}
	}
	tracked.setFilesystemPath(desiredPath)
	return true, nil
}

func hasTrackedLocalDirty(trackedFiles []*trackedFile) bool {
	for _, tracked := range trackedFiles {
		if tracked != nil && tracked.isLocalDirty() {
			return true
		}
	}
	return false
}

func hasTrackedLocalMetadataWork(trackedFiles []*trackedFile) bool {
	for _, tracked := range trackedFiles {
		if tracked != nil && (tracked.isLocalMoved() || tracked.isLocalDeleted() || tracked.isRemoteDeleted()) {
			return true
		}
	}
	return false
}

func (s *workspaceRuntime) actorForTracked(tracked *trackedFile) (string, string) {
	actorID := ""
	actorType := ""
	if tracked != nil {
		actorID = strings.TrimSpace(tracked.ActorID)
		actorType = strings.TrimSpace(tracked.ActorType)
	}
	if actorID == "" {
		actorID = strings.TrimSpace(s.cfg.AgentID)
	}
	if actorID == "" {
		actorID = "daemon"
	}
	if actorType == "" {
		actorType = "daemon"
	}
	return actorID, actorType
}

func (s *workspaceRuntime) flushOutboxUpdates(ctx context.Context, cache *documentCache, documentID, documentPath string, trackedFiles []*trackedFile, records []outboxUpdateRecord) error {
	for _, record := range records {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.sendDocumentCRDTUpdate(ctx, documentID, record); err != nil {
			return err
		}
		entry, unlock := cache.lockEntry(documentID)
		if err := finalizeSentOutbox(cache, entry, documentID, documentPath, trackedFiles, &record); err != nil {
			unlock()
			return err
		}
		unlock()
	}
	return nil
}

func (s *workspaceRuntime) sendDocumentCRDTUpdate(ctx context.Context, documentID string, record outboxUpdateRecord) error {
	if record.Update == nil || len(record.Update) == 0 {
		return nil
	}
	if s.sendDocumentUpdate != nil {
		return s.sendDocumentUpdate(ctx, documentID, record)
	}
	if s.documentSocket == nil {
		s.documentSocket = newWorkspaceDocumentSocket(s)
	}
	return s.documentSocket.SendSyncUpdate(ctx, documentID, record.Update)
}

func finalizeSentOutbox(cache *documentCache, entry *documentCacheEntry, documentID, documentPath string, trackedFiles []*trackedFile, record *outboxUpdateRecord) error {
	baseDoc, _, _, err := cache.loadBaseDocLocked(entry, documentID, documentPath)
	if err != nil {
		return err
	}
	states, err := collectTrackedReconcileStates(trackedFiles)
	if err != nil {
		return err
	}
	if err := crdt.ApplyUpdateV1(baseDoc, record.Update, "local-reconcile"); err != nil {
		return err
	}
	projectedSeq, err := cache.applyOutboxUpdateLocked(entry, documentID, documentPath, record)
	if err != nil {
		return err
	}
	_ = cache.maybeCreateDocumentSnapshot(documentID, projectedSeq, baseDoc)
	if err := cache.clearOutboxUpdate(documentID, record.ID); err != nil {
		return err
	}

	finalContent := baseDoc.GetText("content").ToString()
	finalState := baseDoc.EncodeStateAsUpdate()
	for _, state := range states {
		if state.tracked == nil {
			continue
		}
		if state.tracked.Path == record.SourcePath {
			clean, err := projectMergedContentOverLocalDisk(state.tracked, record.ObservedContent, record.ObservedState, projectedSeq, finalContent, finalState, projectedSeq)
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
		clean, err := applyProjectedContent(state.tracked, finalContent, finalState, projectedSeq)
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

func applyPendingRemoteUpdatesLocked(cache *documentCache, entry *documentCacheEntry, documentID string, doc *crdt.Doc) (int64, error) {
	if cache == nil || doc == nil {
		return 0, nil
	}
	_, projectedSeq, err := cache.applyPendingRemoteUpdatesLocked(entry, documentID, doc)
	return projectedSeq, err
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
		cleanTracked := !tracked.isLocalDirty() && !tracked.isLocalDeleted() && !tracked.isLocalMoved() && !tracked.isRemoteDeleted()
		snapshot, err := tracked.workspaceFS().Read(tracked.Path)
		if err == nil && !snapshot.Exists {
			baseContent, baseState, known, loadErr := tracked.loadProjectedBase()
			if loadErr != nil {
				return nil, loadErr
			}
			if known {
				state.baseContent = baseContent
				state.baseState = baseState
				state.baseKnown = true
			} else {
				state.baseMissing = true
			}
			states = append(states, state)
			continue
		}
		if err != nil {
			return nil, err
		}
		state.fileExists = true
		state.localContent = string(snapshot.Bytes)
		matchesProjected := tracked.hasProjectedContent() && tracked.matchesProjectedBytes(snapshot.Bytes)
		desiredPath := tracked.desiredPath()
		if cleanTracked && matchesProjected && (desiredPath == "" || desiredPath == tracked.Path) {
			tracked.clearLocalDirty()
			states = append(states, state)
			continue
		}
		var baseContent string
		var baseState []byte
		var known bool
		if matchesProjected {
			baseContent, baseState, known, err = tracked.loadProjectedBaseFromStore()
		} else {
			baseContent, baseState, known, err = tracked.loadProjectedBase()
		}
		if err != nil {
			return nil, err
		}
		if known {
			state.baseContent = baseContent
			state.baseState = baseState
			state.baseKnown = true
		} else {
			state.baseMissing = true
			if !tracked.isLocalDirty() {
				states = append(states, state)
				continue
			}
		}
		if state.baseMissing {
			state.localDirty = true
			states = append(states, state)
			continue
		}
		if snapshot.Hash == projectedHashString(state.baseContent) {
			tracked.clearLocalDirty()
			states = append(states, state)
			continue
		}
		state.localDirty = true
		states = append(states, state)
	}
	return states, nil
}

func trackedFilesHaveLocalReconcileWork(trackedFiles []*trackedFile) bool {
	for _, tracked := range trackedFiles {
		if tracked == nil {
			continue
		}
		if tracked.isLocalDirty() || tracked.isLocalDeleted() || tracked.isLocalMoved() || tracked.isRemoteDeleted() {
			return true
		}
		if desiredPath := tracked.desiredPath(); desiredPath != "" && desiredPath != tracked.Path {
			return true
		}
	}
	return false
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
	edits, err := computeLocalTextEdits(baseContent, localContent)
	if err != nil {
		return nil, nil, err
	}
	if len(edits) == 0 {
		return nil, doc.EncodeStateAsUpdate(), nil
	}
	update, err := doc.Update(func(txn *crdt.Transaction) error {
		for _, edit := range edits {
			switch edit.Kind {
			case localTextEditDelete:
				if edit.Length > 0 {
					if err := text.DeleteRange(txn, edit.Start, edit.Length); err != nil {
						return err
					}
				}
			case localTextEditInsert:
				if edit.Text != "" {
					if err := text.InsertValue(txn, edit.Start, edit.Text); err != nil {
						if errors.Is(err, crdt.ErrInvalidYTextString) {
							return unsupportedTextContent(err.Error())
						}
						return err
					}
				}
			}
		}
		return nil
	}, "daemon-local-reconcile")
	if err != nil {
		return nil, nil, err
	}
	return update, doc.EncodeStateAsUpdate(), nil
}

func applyProjectedContent(tracked *trackedFile, nextContent string, nextState []byte, projectedSeq int64) (bool, error) {
	previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(nextContent)
	if err := tracked.workspaceFS().WriteIfUnchanged(tracked.Path, previousHash, []byte(nextContent)); err != nil {
		if errors.Is(err, ErrDivergedWorkingCopy) {
			tracked.setProjectedSnapshot(previousHash, previousKnown)
			return false, nil
		}
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		return false, err
	}
	if err := tracked.storeProjectedBaseAtSeq(nextContent, nextState, projectedSeq); err != nil {
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

func projectMergedContentOverLocalDisk(tracked *trackedFile, currentDiskContent string, currentDiskState []byte, currentDiskSeq int64, mergedContent string, mergedState []byte, mergedSeq int64) (bool, error) {
	previousHash, previousKnown := tracked.projectedSnapshot()
	tracked.beginProjection()
	defer tracked.endProjection()
	tracked.setProjectedContent(mergedContent)
	if err := tracked.workspaceFS().WriteIfUnchanged(tracked.Path, projectedHashString(currentDiskContent), []byte(mergedContent)); err != nil {
		if errors.Is(err, ErrDivergedWorkingCopy) {
			if len(currentDiskState) == 0 {
				currentDiskState = crdtStateFromContent(currentDiskContent)
			}
			if baseErr := tracked.storeProjectedBaseAtSeq(currentDiskContent, currentDiskState, currentDiskSeq); baseErr != nil {
				tracked.setProjectedSnapshot(previousHash, previousKnown)
				return false, baseErr
			}
			return false, nil
		}
		tracked.setProjectedSnapshot(previousHash, previousKnown)
		return false, err
	}
	if err := tracked.storeProjectedBaseAtSeq(mergedContent, mergedState, mergedSeq); err != nil {
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
		FS:            NewWorkspaceFS(workspaceRootForDocumentPath(absolutePath, document.Path)),
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

		_, _, baseKnown, err := tracked.loadProjectedBase()
		if err != nil {
			return nil, err
		}
		if baseKnown {
			snapshot, readErr := tracked.workspaceFS().Read(absolutePath)
			if readErr == nil && snapshot.Exists {
				if !tracked.matchesProjectedBytes(snapshot.Bytes) {
					tracked.markLocalDirty()
				}
				return tracked, nil
			}
			if readErr != nil {
				return nil, readErr
			}
			return tracked, nil
		}
		if _, readErr := tracked.workspaceFS().Read(absolutePath); readErr != nil {
			return nil, readErr
		}
		tracked.markLocalDirty()
		return tracked, nil
	}
	snapshot, err := tracked.workspaceFS().Read(absolutePath)
	if err == nil && snapshot.Exists {
		tracked.setProjectedContent(string(snapshot.Bytes))
		return tracked, nil
	}
	if err != nil {
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
	root := tracked.WorkspaceRoot
	if root == "" {
		root = workspaceRootForDocumentPath(tracked.Path, tracked.DocumentPath)
	}
	if root == "" {
		return nil
	}
	_, err := tracked.workspaceFS().Archive(tracked.Path, safeDocumentCacheName(tracked.DocumentID))
	return err
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

var scanWorkspaceFilesForReconcile = scanWorkspaceFiles

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

func nextAwarenessClientID() uint64 {
	// Awareness messages are decoded by JS Yjs clients, whose varuint reader
	// rejects integers above Number.MAX_SAFE_INTEGER. Keep daemon-generated
	// awareness IDs small; CRDT document client IDs remain separate.
	return 10_000_000 + awarenessClientCounter.Add(1)
}

func (t *trackedFile) storeProjectedBaseAtSeq(content string, state []byte, projectedSeq int64) error {
	if t == nil || t.DocumentID == "" {
		return nil
	}
	if t.cache == nil {
		t.setProjectedBase(content, state)
		return nil
	}
	if err := t.cache.storeProjectedBase(t.DocumentID, content, state, projectedSeq); err != nil {
		return err
	}
	t.setProjectedBase(content, state)
	return nil
}

func (t *trackedFile) loadProjectedBase() (string, []byte, bool, error) {
	if t == nil || t.DocumentID == "" {
		return "", nil, false, nil
	}
	if content, state, known := t.projectedBase(); known {
		return content, state, true, nil
	}
	return t.loadProjectedBaseFromStore()
}

func (t *trackedFile) loadProjectedBaseFromStore() (string, []byte, bool, error) {
	if t == nil || t.DocumentID == "" {
		return "", nil, false, nil
	}
	if t.cache == nil {
		return "", nil, false, nil
	}
	content, state, known, err := t.cache.loadProjectedBase(t.DocumentID)
	if err != nil || !known {
		return content, state, known, err
	}
	t.setProjectedBase(content, state)
	return content, state, true, nil
}

func (t *trackedFile) workspaceFS() *WorkspaceFS {
	if t != nil && t.FS != nil {
		return t.FS
	}
	root := ""
	if t != nil {
		root = t.WorkspaceRoot
		if root == "" {
			root = workspaceRootForDocumentPath(t.Path, t.DocumentPath)
		}
	}
	return NewWorkspaceFS(root)
}

func (t *trackedFile) desiredPath() string {
	if t == nil || t.WorkspaceRoot == "" || t.DocumentPath == "" {
		return ""
	}
	return filepath.Join(t.WorkspaceRoot, filepath.FromSlash(t.DocumentPath))
}

func (t *trackedFile) setFilesystemPath(nextPath string) {
	if t == nil || nextPath == "" || nextPath == t.Path {
		return
	}
	if t.Owner != nil {
		t.Owner.setTrackedPath(t, nextPath)
		return
	}
	t.Path = nextPath
	t.WorkspaceRoot = workspaceRootForDocumentPath(nextPath, t.DocumentPath)
}

func (t *trackedFile) untrack() {
	if t == nil {
		return
	}
	if t.Owner != nil {
		t.Owner.untrack(t)
		return
	}
	t.clearLocalDirty()
	t.clearLocalDeleted()
	t.clearLocalMoved()
	t.clearRemoteDeleted()
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

func (t *trackedFile) projectedBase() (string, []byte, bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	if !t.projectedContentKnown || len(t.projectedState) == 0 {
		return "", nil, false
	}
	return t.projectedContent, append([]byte(nil), t.projectedState...), true
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
	t.projectedContent = ""
	t.projectedState = nil
}

func (t *trackedFile) setProjectedBase(content string, state []byte) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.projectedContentKnown = true
	t.hash = projectedHashString(content)
	if len(state) == 0 || len(content)+len(state) > trackedProjectedBaseCacheMaxBytes {
		t.projectedContent = ""
		t.projectedState = nil
		return
	}
	t.projectedContent = content
	t.projectedState = append([]byte(nil), state...)
}

func (t *trackedFile) setProjectedSnapshot(hash projectedContentHash, known bool) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.hash = hash
	t.projectedContentKnown = known
	if !known {
		t.projectedContent = ""
		t.projectedState = nil
		return
	}
	if t.projectedContent == "" || hash != projectedHashString(t.projectedContent) {
		t.projectedContent = ""
		t.projectedState = nil
	}
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

func (t *trackedFile) markLocalDeleted() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.localDeleted = true
	t.localDirty = true
}

func (t *trackedFile) clearLocalDeleted() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.localDeleted = false
}

func (t *trackedFile) isLocalDeleted() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.localDeleted
}

func (t *trackedFile) markLocalMoved() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.localMoved = true
	t.localDirty = true
}

func (t *trackedFile) clearLocalMoved() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.localMoved = false
}

func (t *trackedFile) isLocalMoved() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.localMoved
}

func (t *trackedFile) markRemoteDeleted() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.remoteDeleted = true
}

func (t *trackedFile) clearRemoteDeleted() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.remoteDeleted = false
}

func (t *trackedFile) isRemoteDeleted() bool {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.remoteDeleted
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
