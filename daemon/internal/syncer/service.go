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
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	primaryStream   *streamProjection
	agentStreams    map[string]*managedStreamProjection
	streamSyncs     map[string]*managedStreamSync
	agentWorkers    map[string]*managedAgentWorker
	latestWorkspace *workspaceResponse
	state           *WorkspaceStateDB
	workspaceFS     *WorkspaceFS
	syncLoop        *WorkspaceSyncLoop
	streamSender    *StreamSender
}

type managedStreamProjection struct {
	projection *streamProjection
	cancel     context.CancelFunc
}

type managedAgentWorker struct {
	cancel context.CancelFunc
	wake   chan struct{}
}

type managedStreamSync struct {
	sync   *streamSync
	cancel context.CancelFunc
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

func New(cfg Config) (*Service, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	service := &Service{
		cfg:          cfg,
		client:       client,
		agentStreams: map[string]*managedStreamProjection{},
		streamSyncs:  map[string]*managedStreamSync{},
		agentWorkers: map[string]*managedAgentWorker{},
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
		s.closeStreamSyncs()
		s.closeAgentWorkers()
		s.closeAgentReplicas()
		if s.sessions != nil {
			s.sessions.Shutdown()
		}
		_ = shutdownToolGateway(context.Background(), s.toolServer)
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
			s.closeStreamSyncs()
			s.closeAgentWorkers()
			s.closeAgentReplicas()
			if s.sessions != nil {
				s.sessions.Shutdown()
			}
			_ = shutdownToolGateway(context.Background(), s.toolServer)
			return nil
		case <-reconcileTicker.C:
			if err := s.reconcileWorkspaceStreams(ctx); err != nil {
				fmt.Printf("stream reconcile error: %v\n", err)
			}
		case <-ticker.C:
			if err := s.reconcileWorkspaceStreams(ctx); err != nil {
				fmt.Printf("stream reconcile error: %v\n", err)
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
	if err := s.ensureWorkspaceStreamSync(ctx, workspace); err != nil {
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

func projectedHashString(content string) projectedContentHash {
	return projectedContentHash{size: len(content), sum: maphash.String(projectedHashSeed, content)}
}

func projectedHashBytes(content []byte) projectedContentHash {
	return projectedContentHash{size: len(content), sum: maphash.Bytes(projectedHashSeed, content)}
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
