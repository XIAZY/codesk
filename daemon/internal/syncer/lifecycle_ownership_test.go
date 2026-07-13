package syncer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	crdt "notty/internal/ycrdt"
)

type scriptedWorkspaceWatcher struct {
	events     chan fsnotify.Event
	errors     chan error
	closed     chan struct{}
	closeOnce  sync.Once
	closeCount atomic.Int32
	add        func(string) error
}

func newScriptedWorkspaceWatcher() *scriptedWorkspaceWatcher {
	return &scriptedWorkspaceWatcher{
		events: make(chan fsnotify.Event),
		errors: make(chan error),
		closed: make(chan struct{}),
	}
}

func (w *scriptedWorkspaceWatcher) Add(path string) error {
	if w.add != nil {
		return w.add(path)
	}
	return nil
}

func (w *scriptedWorkspaceWatcher) Close() error {
	w.closeOnce.Do(func() {
		w.closeCount.Add(1)
		close(w.closed)
	})
	return nil
}

func (w *scriptedWorkspaceWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *scriptedWorkspaceWatcher) Errors() <-chan error          { return w.errors }

func TestWorkspaceReplicaStartupDrainsEventsBeforeFirstAdd(t *testing.T) {
	root := t.TempDir()
	watcher := newScriptedWorkspaceWatcher()
	watcher.add = func(path string) error {
		select {
		case watcher.events <- fsnotify.Event{Name: path, Op: fsnotify.Write}:
			return nil
		case <-watcher.closed:
			return fsnotify.ErrClosed
		}
	}
	replica := newWorkspaceReplica(Config{}, root, "daemon", "daemon", nil, nil)
	replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- replica.run(ctx, ready) }()

	select {
	case err := <-ready:
		cancel()
		if runErr := <-done; runErr != nil {
			t.Fatalf("replica shutdown: %v", runErr)
		}
		if err != nil {
			t.Fatalf("replica startup: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		// Release the intentionally modelled Windows cycle so a RED run exits
		// cleanly instead of stranding the package test process.
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			select {
			case <-watcher.events:
			case <-watcher.closed:
			}
		}()
		select {
		case <-ready:
		case <-time.After(2 * time.Second):
			cancel()
			_ = watcher.Close()
			<-drainDone
			t.Fatal("replica startup could not be released after diagnostic drain")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = watcher.Close()
			t.Fatal("replica did not stop after releasing blocked Add")
		}
		<-drainDone
		t.Fatal("replica called Add before any watcher event consumer was active")
	}
	if got := watcher.closeCount.Load(); got != 1 {
		t.Fatalf("watcher close count = %d, want 1", got)
	}
}

func TestWorkspaceRuntimeStartupPropagatesReplicaAddFailure(t *testing.T) {
	wantErr := errors.New("injected watcher Add failure")
	watcher := newScriptedWorkspaceWatcher()
	watcher.add = func(string) error { return wantErr }
	runtime, err := newWorkspaceRuntime(
		Config{WorkspaceDir: t.TempDir(), AgentID: "daemon"},
		http.DefaultClient,
		t.TempDir(),
		"daemon",
		"daemon",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- runtime.run(ctx, ready) }()

	select {
	case err := <-ready:
		if errors.Is(err, wantErr) {
			select {
			case runErr := <-done:
				if !errors.Is(runErr, wantErr) {
					t.Fatalf("runtime error = %v, want %v", runErr, wantErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("runtime did not return its startup failure")
			}
			return
		}
		cancel()
		<-done
		t.Fatalf("runtime published ready instead of returning replica startup failure: %v", err)
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("runtime did not report a startup result")
	}
}

func TestWorkspaceReplicaWatcherErrorAfterReadyIsFatal(t *testing.T) {
	wantErr := errors.New("injected watcher pump failure")
	watcher := newScriptedWorkspaceWatcher()
	replica := newWorkspaceReplica(Config{}, t.TempDir(), "daemon", "daemon", nil, nil)
	replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- replica.run(ctx, ready) }()
	if err := <-ready; err != nil {
		cancel()
		<-done
		t.Fatalf("replica startup: %v", err)
	}
	watcher.errors <- wantErr
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("replica error = %v, want %v", err, wantErr)
		}
	case <-time.After(300 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("ready replica logged and ignored a fatal watcher error")
	}
}

func TestServicePrimaryWatcherFailureAfterReadyIsFatal(t *testing.T) {
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rootDocumentId":"root","agents":[]}`))
		case "/ws", "/ws/documents-sync":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err == nil {
				_ = conn.Close()
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toolAddr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	watcher := newScriptedWorkspaceWatcher()
	runtime, err := newWorkspaceRuntime(
		Config{BackendURL: backend.URL, WorkspaceDir: root, AgentID: "daemon"},
		backend.Client(),
		root,
		"daemon",
		"daemon",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	service := &Service{
		cfg: Config{
			BackendURL:         backend.URL,
			WorkspaceDir:       root,
			AgentWorkspaceRoot: filepath.Join(root, "agents"),
			AgentID:            "daemon",
			AgentToolBaseURL:   "http://" + toolAddr,
		},
		client:         backend.Client(),
		sessions:       newAgentSessionSupervisor(Config{}, nil, newRuntimeRegistry()),
		primaryRuntime: runtime,
		agentRuntimes:  map[string]*managedWorkspaceRuntime{},
		agentWorkers:   map[string]*managedAgentWorker{},
		ready:          make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	select {
	case <-service.ready:
	case err := <-done:
		t.Fatalf("service stopped before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("service did not become ready")
	}

	wantErr := errors.New("injected primary watcher failure")
	watcher.errors <- wantErr
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("service error = %v, want %v", err, wantErr)
		}
	case <-time.After(500 * time.Millisecond):
		cancel()
		err := <-done
		t.Fatalf("service stayed online after its primary watcher failed; shutdown error: %v", err)
	}
}

func TestServiceDoesNotReportOnlineBeforePrimaryRuntimeReady(t *testing.T) {
	var statusReports atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rootDocumentId":"","agents":[]}`))
		case "/api/daemon/status":
			statusReports.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toolAddr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wantErr := errors.New("injected primary watcher Add failure")
	watcher := newScriptedWorkspaceWatcher()
	watcher.add = func(string) error { return wantErr }
	runtime, err := newWorkspaceRuntime(
		Config{BackendURL: backend.URL, WorkspaceDir: root, AgentID: "daemon"},
		backend.Client(),
		root,
		"daemon",
		"daemon",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	cfg := Config{
		BackendURL:         backend.URL,
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(root, "agents"),
		AgentID:            "daemon",
		AgentToolBaseURL:   "http://" + toolAddr,
	}
	service := &Service{
		cfg:             cfg,
		client:          backend.Client(),
		daemonStatus:    newDaemonStatusReporter(cfg, backend.Client()),
		primaryRuntime:  runtime,
		agentRuntimes:   map[string]*managedWorkspaceRuntime{},
		agentWorkers:    map[string]*managedAgentWorker{},
		latestWorkspace: &workspaceResponse{},
	}
	if err := service.Run(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("service error = %v, want %v", err, wantErr)
	}
	if got := statusReports.Load(); got != 0 {
		t.Fatalf("daemon published %d online heartbeat(s) before primary readiness, want 0", got)
	}
}

func TestServiceAgentExitIsNonfatalAndSupervisorStopsRestarts(t *testing.T) {
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rootDocumentId":"root","agents":[{"id":"agent-1","handle":"agent","kind":"codex"}]}`))
		case "/ws", "/ws/documents-sync":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err == nil {
				_ = conn.Close()
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toolAddr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := Config{
		BackendURL:         backend.URL,
		DataDir:            t.TempDir(),
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(root, "agents"),
		AgentID:            "daemon",
		AgentToolBaseURL:   "http://" + toolAddr,
	}
	driver := newFakeRuntimeDriver()
	service := &Service{
		cfg:           cfg,
		client:        backend.Client(),
		sessions:      newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver)),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		ready:         make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	select {
	case <-service.ready:
	case err := <-done:
		cancel()
		t.Fatalf("service stopped before ready: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("service did not become ready")
	}
	process := driver.only(t)
	close(process.events)
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.sessions.mu.Lock()
		session := service.sessions.sessions["agent-1"]
		disconnected := session != nil && session.state == "disconnected"
		service.sessions.mu.Unlock()
		if disconnected {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("agent exit did not mark only that session disconnected")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		cancel()
		t.Fatalf("agent exit terminated the core service: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not join its agent supervisor")
	}
	time.Sleep(2100 * time.Millisecond)
	driver.mu.Lock()
	processCount := len(driver.processes)
	driver.mu.Unlock()
	if processCount != 1 {
		t.Fatalf("agent supervisor restarted after shutdown; process count = %d, want 1", processCount)
	}
}

func TestServiceRestartsFailedAgentRuntimeWithoutWorkspaceRefresh(t *testing.T) {
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/workspace":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rootDocumentId":"","agents":[{"id":"agent-1","handle":"agent","kind":"codex"}]}`))
		case r.URL.Path == "/ws" || r.URL.Path == "/ws/documents-sync":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case strings.HasPrefix(r.URL.Path, "/api/agents/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toolAddr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	service := &Service{
		cfg: Config{
			BackendURL:         backend.URL,
			DataDir:            t.TempDir(),
			WorkspaceDir:       root,
			AgentWorkspaceRoot: filepath.Join(root, "agents"),
			AgentID:            "daemon",
			AgentToolBaseURL:   "http://" + toolAddr,
		},
		client:        backend.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		ready:         make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-runDone:
				if err != nil {
					t.Errorf("service shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("service did not stop")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case <-service.ready:
	case err := <-runDone:
		cancel()
		t.Fatalf("service stopped before ready: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("service did not become ready")
	}

	failed, watcher := waitForAgentRuntimeWatcher(t, service, "agent-1", nil)
	if err := watcher.Close(); err != nil {
		t.Fatalf("inject agent watcher failure: %v", err)
	}
	select {
	case <-failed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("failed agent runtime did not stop")
	}

	replacement, replacementWatcher := waitForAgentRuntimeWatcher(t, service, "agent-1", failed)
	select {
	case err := <-runDone:
		cancel()
		t.Fatalf("agent runtime failure terminated the core service: %v", err)
	default:
	}

	// A second quick failure is attempt 1 and therefore waits for the 100 ms
	// backoff. Cancel during that window to prove teardown cannot resurrect it.
	if err := replacementWatcher.Close(); err != nil {
		t.Fatalf("inject replacement watcher failure: %v", err)
	}
	select {
	case <-replacement.done:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement agent runtime did not stop")
	}
	stop()
	time.Sleep(2 * agentRuntimeRestartBaseDelay)
	service.mu.Lock()
	remaining := len(service.agentRuntimes)
	service.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("agent runtime supervisor resurrected %d runtime(s) after shutdown", remaining)
	}
}

func waitForAgentRuntimeWatcher(
	t *testing.T,
	service *Service,
	agentID string,
	previous *managedWorkspaceRuntime,
) (*managedWorkspaceRuntime, workspaceWatcher) {
	t.Helper()
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		service.mu.Lock()
		managed := service.agentRuntimes[agentID]
		service.mu.Unlock()
		if managed != nil && managed != previous && managed.runtime != nil && managed.runtime.replica != nil && !managed.stopped() {
			managed.runtime.replica.watchMu.Lock()
			watcher := managed.runtime.replica.watcher
			managed.runtime.replica.watchMu.Unlock()
			if watcher != nil {
				return managed, watcher
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("agent runtime did not start a replacement watcher without a workspace refresh")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAgentRuntimeReplacementProcessesIntentAcceptedByRetiredGeneration(t *testing.T) {
	root := t.TempDir()
	threadRequests := make(chan string, 1)
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
			writeJSONResponse(w, http.StatusOK, workspaceResponse{
				RootDocumentID: "root",
				Agents:         []*agent{{ID: "agent-1"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/threads":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			threadRequests <- string(body)
			writeJSONResponse(w, http.StatusCreated, toolThreadMutationResponse{Thread: &thread{ID: "thread-1"}})
		case r.URL.Path == "/ws/documents-sync":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer backend.Close()

	cfg := Config{
		BackendURL:         backend.URL,
		AgentWorkspaceRoot: filepath.Join(root, "agents"),
		AgentID:            "daemon",
	}
	agentRoot := agentWorkspacePath(cfg, "agent-1")
	documentPath := "docs/spec.md"
	absolutePath := filepath.Join(agentRoot, filepath.FromSlash(documentPath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatal(err)
	}
	const documentContent = "root\ntarget\n"
	if err := os.WriteFile(absolutePath, []byte(documentContent), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime, err := newWorkspaceRuntime(
		cfg,
		backend.Client(),
		agentRoot,
		"agent-1",
		"agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	rootID := "root"
	documentID := "doc-1"
	rootDoc := crdt.New()
	if _, err := UpsertRootFile(rootDoc, documentID, documentPath, rootMutationActor{}); err != nil {
		rootDoc.Close()
		t.Fatal(err)
	}
	if err := runtime.docCache.storeDoc(rootID, rootDocumentPath, 1, rootDoc); err != nil {
		rootDoc.Close()
		t.Fatal(err)
	}
	rootDoc.Close()
	if err := runtime.docCache.storeRootProjectionEntries(rootID, []rootProjectionEntry{{
		EntryID:           documentID,
		ContentDocumentID: documentID,
		DesiredPath:       documentPath,
		MaterializedPath:  documentPath,
		Active:            true,
	}}); err != nil {
		t.Fatal(err)
	}
	baseDoc := newDocWithText(t, documentContent)
	if err := runtime.docCache.storeDoc(documentID, documentPath, 1, baseDoc); err != nil {
		baseDoc.Close()
		t.Fatal(err)
	}
	appliedSeq, err := runtime.docCache.documentAppliedSeq(documentID)
	if err != nil {
		baseDoc.Close()
		t.Fatal(err)
	}
	if err := runtime.docCache.storeProjectedBase(documentID, documentContent, baseDoc.EncodeStateAsUpdate(), appliedSeq); err != nil {
		baseDoc.Close()
		t.Fatal(err)
	}
	baseDoc.Close()
	runtime.rootDocumentID = rootID
	workspace := &workspaceResponse{
		RootDocumentID: rootID,
		Agents:         []*agent{{ID: "agent-1"}},
	}
	service := newToolGatewayTestService(
		&agent{ID: "agent-1", Handle: "reviewer", Kind: "codex"},
		"tool-token",
	)
	service.cfg = cfg
	service.client = backend.Client()
	ctx, cancel := context.WithCancel(context.Background())
	runtimeCtx, runtimeCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	managed := &managedWorkspaceRuntime{
		runtime:    runtime,
		cancel:     runtimeCancel,
		done:       done,
		parentCtx:  ctx,
		runtimeCtx: runtimeCtx,
		workspace:  workspace,
		startedAt:  time.Now(),
	}
	service.agentRuntimes = map[string]*managedWorkspaceRuntime{"agent-1": managed}

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toolAddr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	service.cfg.AgentToolBaseURL = "http://" + toolAddr
	gateway, err := service.startToolGateway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = gateway.Drain(drainCtx)
		drainCancel()
		cancel()
		service.closeAgentRuntimes()
	})

	_, unlockTarget := runtime.docCache.lockEntry(documentID)
	var targetUnlock sync.Once
	t.Cleanup(func() { targetUnlock.Do(unlockTarget) })
	requestDone := make(chan toolRequestResult, 1)
	go func() {
		req, err := http.NewRequest(
			http.MethodPost,
			"http://"+toolAddr+"/agent-tools/create-thread",
			strings.NewReader(`{"path":"docs/spec.md","quote":"target","body":"late old-generation intent"}`),
		)
		if err != nil {
			requestDone <- toolRequestResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer tool-token")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			requestDone <- toolRequestResult{err: err}
			return
		}
		defer res.Body.Close()
		body, readErr := io.ReadAll(res.Body)
		requestDone <- toolRequestResult{status: res.StatusCode, body: string(body), err: readErr}
	}()
	waitForManagedRuntimeBorrower(t, managed)
	gateway.mu.Lock()
	activeHandlers := gateway.activeHandlers
	gateway.mu.Unlock()
	if activeHandlers != 1 {
		t.Fatalf("active tool handlers = %d, want 1 admitted cache borrower", activeHandlers)
	}

	superviseManagedRuntimeFailureForTest(service, managed, done, errors.New("injected agent runtime failure"))
	select {
	case <-managed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("failed agent runtime did not stop")
	}
	waitForManagedRuntimeRetiring(t, managed)
	if err := runtime.pathLocks.cleanupExpired(time.Now().UTC()); err != nil {
		t.Fatalf("replacement closed the old runtime while an admitted tool borrower still owned it: %v", err)
	}
	select {
	case result := <-requestDone:
		t.Fatalf(
			"admitted tool request finished before its target entry was released: status=%d body=%q err=%v",
			result.status,
			result.body,
			result.err,
		)
	case <-time.After(150 * time.Millisecond):
	}
	targetUnlock.Do(unlockTarget)
	var result toolRequestResult
	select {
	case result = <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("admitted tool request did not finish after releasing the cache connection")
	}
	if result.err != nil || result.status != http.StatusCreated {
		t.Errorf("tool request lost its runtime generation during replacement: status=%d body=%q err=%v", result.status, result.body, result.err)
	}
	replacement, _ := waitForAgentRuntimeWatcher(t, service, "agent-1", managed)
	waitForRuntimeReconcileIdle(t, replacement.runtime)

	newReq, err := http.NewRequest(
		http.MethodGet,
		"http://"+toolAddr+"/agent-tools/list-documents",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	newReq.Header.Set("Authorization", "Bearer tool-token")
	newRes, err := http.DefaultClient.Do(newReq)
	if err != nil {
		t.Fatalf("new-generation tool request: %v", err)
	}
	newBody, readErr := io.ReadAll(newRes.Body)
	_ = newRes.Body.Close()
	if readErr != nil || newRes.StatusCode != http.StatusOK {
		t.Fatalf(
			"new tool request did not resolve to the replacement generation: status=%d body=%q err=%v",
			newRes.StatusCode,
			string(newBody),
			readErr,
		)
	}
	select {
	case body := <-threadRequests:
		if !strings.Contains(body, "late old-generation intent") {
			t.Fatalf("replacement delivered the wrong thread intent: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not recover the thread intent accepted by the retired runtime generation")
	}
	waitForManagedRuntimeStoresToClose(t, runtime)
}

func TestAgentScopedToolDoesNotFallBackToPrimaryWhileRuntimeRetires(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rootID := "primary-root"
	if err := cache.storeRootProjectionEntries(rootID, []rootProjectionEntry{{
		EntryID:           "primary-doc",
		ContentDocumentID: "primary-doc",
		DesiredPath:       "primary/secret.md",
		MaterializedPath:  "primary/secret.md",
		Active:            true,
	}}); err != nil {
		t.Fatal(err)
	}
	service := newToolGatewayTestService(&agent{ID: "agent-1"}, "tool-token")
	service.primaryRuntime = &workspaceRuntime{rootDocumentID: rootID, docCache: cache}
	managed := &managedWorkspaceRuntime{runtime: &workspaceRuntime{}}
	managed.retire()
	service.agentRuntimes = map[string]*managedWorkspaceRuntime{"agent-1": managed}

	req := httptest.NewRequest(http.MethodGet, "/agent-tools/list-documents", nil)
	req.Header.Set("Authorization", "Bearer tool-token")
	recorder := httptest.NewRecorder()
	service.handleListDocumentsTool(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("retiring agent request status = %d body=%q, want unavailable", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "workspace runtime is unavailable") || strings.Contains(body, "primary/secret.md") {
		t.Fatalf("retiring agent request touched the primary runtime: %q", body)
	}
}

func TestAgentWorkspaceApplyBorrowSurvivesConcurrentRetirement(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AgentWorkspaceRoot: filepath.Join(root, "agents")}
	runtime, err := newWorkspaceRuntime(
		cfg,
		http.DefaultClient,
		agentWorkspacePath(cfg, "agent-1"),
		"agent-1",
		"agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.docCache.db.SetMaxOpenConns(1)
	workspace := &workspaceResponse{
		RootDocumentID: "root",
		Agents:         []*agent{{ID: "agent-1"}},
	}
	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		service.closeAgentRuntimes()
	})
	runtimeCtx, runtimeCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	managed := &managedWorkspaceRuntime{
		runtime:    runtime,
		cancel:     runtimeCancel,
		done:       done,
		parentCtx:  ctx,
		runtimeCtx: runtimeCtx,
		workspace:  workspace,
		startedAt:  time.Now(),
	}
	service.agentRuntimes["agent-1"] = managed

	heldConn, err := runtime.docCache.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var heldConnClose sync.Once
	defer heldConnClose.Do(func() { _ = heldConn.Close() })
	waitCount := runtime.docCache.db.Stats().WaitCount
	applyDone := make(chan error, 1)
	go func() { applyDone <- service.syncAgentRuntimes(ctx, workspace) }()
	waitForDocumentCacheWaiter(t, runtime.docCache, waitCount)

	superviseManagedRuntimeFailureForTest(service, managed, done, errors.New("injected concurrent runtime failure"))
	select {
	case <-managed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("failed agent runtime did not stop")
	}
	waitForManagedRuntimeRetiring(t, managed)
	if err := runtime.pathLocks.cleanupExpired(time.Now().UTC()); err != nil {
		t.Fatalf("replacement closed the old runtime while applyWorkspace still borrowed it: %v", err)
	}
	select {
	case err := <-applyDone:
		t.Fatalf("workspace apply finished before its held cache connection was released: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	heldConnClose.Do(func() { _ = heldConn.Close() })
	var applyErr error
	select {
	case applyErr = <-applyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("workspace apply did not finish after releasing the cache connection")
	}
	if applyErr != nil {
		t.Errorf("workspace apply lost its runtime generation during retirement: %v", applyErr)
	}
	_, _ = waitForAgentRuntimeWatcher(t, service, "agent-1", managed)
	waitForManagedRuntimeStoresToClose(t, runtime)
}

func superviseManagedRuntimeFailureForTest(
	service *Service,
	managed *managedWorkspaceRuntime,
	done chan struct{},
	runErr error,
) {
	service.agentRuntimeSupervisors.Add(1)
	managed.recordResult(runErr)
	close(done)
	go func() {
		defer service.agentRuntimeSupervisors.Done()
		service.superviseStoppedAgentWorkspaceRuntime(managed.runtime.replica.actorID, managed)
	}()
}

func waitForManagedRuntimeStoresToClose(t *testing.T, runtime *workspaceRuntime) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := runtime.pathLocks.cleanupExpired(time.Now().UTC()); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("retired runtime stores remained open after its borrowers released")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForManagedRuntimeBorrower(t *testing.T, runtime *managedWorkspaceRuntime) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var stableSince time.Time
	for time.Now().Before(deadline) {
		runtime.borrowMu.Lock()
		borrowers := runtime.borrowers
		runtime.borrowMu.Unlock()
		if borrowers == 1 {
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= 100*time.Millisecond {
				return
			}
		} else {
			stableSince = time.Time{}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tool request did not retain the old runtime generation while waiting to append its intent")
}

func waitForManagedRuntimeRetiring(t *testing.T, runtime *managedWorkspaceRuntime) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.borrowMu.Lock()
		retiring := runtime.retiring
		runtime.borrowMu.Unlock()
		if retiring {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("failed agent runtime did not begin retirement")
}

func waitForRuntimeReconcileIdle(t *testing.T, runtime *workspaceRuntime) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var stableSince time.Time
	for time.Now().Before(deadline) {
		if runtime != nil && runtime.reconcileQueue != nil && runtime.reconcileQueue.Len() == 0 {
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= 100*time.Millisecond {
				return
			}
		} else {
			stableSince = time.Time{}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("replacement runtime did not finish its startup reconcile work")
}

func waitForDocumentCacheWaiter(t *testing.T, cache *documentCache, previous int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for cache.db.Stats().WaitCount == previous && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cache.db.Stats().WaitCount == previous {
		t.Fatal("runtime consumer did not begin using the document cache")
	}
}

type countingPathLockStore struct {
	closes  *atomic.Int32
	onClose func()
	once    sync.Once
}

func (s *countingPathLockStore) cleanupExpired(time.Time) error { return nil }
func (s *countingPathLockStore) lock(paths []string) ([]pathLockLease, error) {
	leases := make([]pathLockLease, len(paths))
	return leases, nil
}
func (s *countingPathLockStore) release([]pathLockLease) error { return nil }
func (s *countingPathLockStore) Close() error {
	s.once.Do(func() {
		s.closes.Add(1)
		if s.onClose != nil {
			s.onClose()
		}
	})
	return nil
}

func TestServiceTeardownUsesReverseDependencyOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	record := func(stage string) {
		mu.Lock()
		got = append(got, stage)
		mu.Unlock()
	}
	teardown := &serviceTeardown{
		cancelCore:       func() { record("cancel-core") },
		closeIngress:     func() error { record("close-ingress"); return nil },
		joinCore:         func() { record("join-core") },
		drainGateway:     func() error { record("drain-gateway"); return nil },
		joinAgentTree:    func() { record("join-agent-tree") },
		closeRuntimeData: func() error { record("close-runtime-data"); return nil },
	}
	if err := teardown.Close(); err != nil {
		t.Fatal(err)
	}
	if err := teardown.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cancel-core",
		"close-ingress",
		"join-core",
		"drain-gateway",
		"join-agent-tree",
		"close-runtime-data",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown sequence = %v, want %v", got, want)
	}
}

func TestServiceTeardownWaitsForBlockedCoreRefreshBeforeClosingRuntimeData(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshDone := make(chan struct{})
	go func() {
		close(refreshStarted)
		<-releaseRefresh
		close(refreshDone)
	}()
	<-refreshStarted
	var runtimeDataClosed atomic.Bool
	teardown := &serviceTeardown{
		cancelCore:    func() {},
		closeIngress:  func() error { return nil },
		joinCore:      func() { <-refreshDone },
		drainGateway:  func() error { return nil },
		joinAgentTree: func() {},
		closeRuntimeData: func() error {
			runtimeDataClosed.Store(true)
			return nil
		},
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- teardown.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("teardown returned before the blocked core refresh joined: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if runtimeDataClosed.Load() {
		t.Fatal("runtime data closed while a core refresh still owned it")
	}
	close(releaseRefresh)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("teardown did not finish after the core refresh joined")
	}
	if !runtimeDataClosed.Load() {
		t.Fatal("runtime data remained open after core quiescence")
	}
}

func TestWorkspaceRuntimeClosesDocumentCacheBeforePathLocks(t *testing.T) {
	cache, err := newDocumentCache(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	var mu sync.Mutex
	var got []string
	record := func(stage string) {
		mu.Lock()
		got = append(got, stage)
		mu.Unlock()
	}
	var closes atomic.Int32
	runtime := &workspaceRuntime{
		docCache: cache,
		pathLocks: &countingPathLockStore{
			closes:  &closes,
			onClose: func() { record("path-lock-store") },
		},
		closeDocumentCache: func() error {
			record("document-cache")
			return cache.Close()
		},
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"document-cache", "path-lock-store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime store close sequence = %v, want %v", got, want)
	}
}

func TestWorkspaceFSReusesOnePathLockStoreAcrossOperations(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "document.md")
	if err := os.WriteFile(path, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	var closes atomic.Int32
	runtime, err := newWorkspaceRuntimeWithOpeners(
		Config{},
		http.DefaultClient,
		root,
		"daemon",
		"daemon",
		func(string) (pathLockLeaseStore, error) {
			opens.Add(1)
			return &countingPathLockStore{closes: &closes}, nil
		},
		newDocumentCache,
	)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = runtime.Close()
		}
	}()
	fs := runtime.replica.fs
	if err := fs.CleanupStaleLocks(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := fs.Read(path); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fs.Read(path); err != nil {
				t.Errorf("concurrent read: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := opens.Load(); got != 1 {
		t.Fatalf("path-lock store open/schema-init count = %d, want 1 per runtime", got)
	}
	if got := closes.Load(); got != 0 {
		t.Fatalf("path-lock store close count during operations = %d, want 0", got)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if got := closes.Load(); got != 1 {
		t.Fatalf("path-lock store close count after quiescence = %d, want 1", got)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("path-lock store close count after second close = %d, want 1", got)
	}
}

func TestWorkspaceRuntimeConstructorFailureClosesAcquiredPathLocks(t *testing.T) {
	wantErr := errors.New("injected document cache open failure")
	var opens atomic.Int32
	var closes atomic.Int32
	runtime, err := newWorkspaceRuntimeWithOpeners(
		Config{},
		http.DefaultClient,
		t.TempDir(),
		"daemon",
		"daemon",
		func(string) (pathLockLeaseStore, error) {
			opens.Add(1)
			return &countingPathLockStore{closes: &closes}, nil
		},
		func(string) (*documentCache, error) { return nil, wantErr },
	)
	if runtime != nil {
		t.Fatal("constructor returned a partial runtime")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("constructor error = %v, want %v", err, wantErr)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("path-lock store open count = %d, want 1", got)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("path-lock store close count after constructor failure = %d, want 1", got)
	}
}

func TestManagedAgentRuntimeRetainsStoresUntilRegistryOwnerCloses(t *testing.T) {
	root := t.TempDir()
	service := &Service{
		cfg: Config{
			AgentWorkspaceRoot: filepath.Join(root, "agents"),
		},
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.syncAgentRuntimes(ctx, &workspaceResponse{Agents: []*agent{{ID: "agent-1"}}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer service.closeAgentRuntimes()
	service.mu.Lock()
	managed := service.agentRuntimes["agent-1"]
	service.mu.Unlock()
	if managed == nil || managed.runtime == nil {
		cancel()
		t.Fatal("agent runtime was not registered")
	}
	cancel()
	select {
	case <-managed.done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent runtime did not stop after parent cancellation")
	}
	if err := managed.runtime.pathLocks.cleanupExpired(time.Now().UTC()); err != nil {
		t.Fatalf("agent runtime closed its stores before the registry owner drained tool users: %v", err)
	}
	service.closeAgentRuntimes()
	if err := managed.runtime.pathLocks.cleanupExpired(time.Now().UTC()); err == nil {
		t.Fatal("registry owner did not close the agent runtime stores")
	}
}

func startSupervisedAgentWorkspaceRuntimeForTest(
	t *testing.T,
	service *Service,
	ctx context.Context,
	agentID string,
	runtime *workspaceRuntime,
	workspace *workspaceResponse,
	restartAttempt int,
	ready chan<- error,
) *managedWorkspaceRuntime {
	t.Helper()
	runtime.initialWorkspace = workspace
	service.mu.Lock()
	managed := service.startAgentWorkspaceRuntimeAttemptWithReady(
		ctx,
		agentID,
		runtime,
		workspace,
		restartAttempt,
		ready,
	)
	service.agentRuntimes[agentID] = managed
	service.mu.Unlock()
	return managed
}

func TestManagedWorkspaceRuntimeRestartBackoffClassifiesFailures(t *testing.T) {
	terminalErr := &backendStatusError{StatusCode: http.StatusUnauthorized}
	cases := []struct {
		name    string
		attempt int
		err     error
		want    time.Duration
	}{
		{name: "clean stop", attempt: 4, want: 0},
		{name: "canceled", attempt: 4, err: context.Canceled, want: 0},
		{name: "first runtime failure restarts immediately", err: errors.New("boom"), want: 0},
		{name: "first retry backs off", attempt: 1, err: errors.New("boom"), want: 100 * time.Millisecond},
		{name: "second retry doubles", attempt: 2, err: errors.New("boom"), want: 200 * time.Millisecond},
		{name: "backoff is capped", attempt: 10, err: errors.New("boom"), want: 5 * time.Second},
		{name: "terminal auth is classified conservatively", err: terminalErr, want: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedWorkspaceRuntimeRestartDelay(tc.attempt, tc.err); got != tc.want {
				t.Fatalf("restart delay = %s, want %s", got, tc.want)
			}
		})
	}
	now := time.Now()
	stable := &managedWorkspaceRuntime{
		startedAt:      now.Add(-agentRuntimeStableWindow),
		stoppedAt:      now,
		restartAttempt: 7,
	}
	if delayAttempt, nextAttempt := stable.restartPlan(); delayAttempt != 0 || nextAttempt != 0 {
		t.Fatalf(
			"stable runtime restart plan = delay attempt %d, next attempt %d; want 0, 0",
			delayAttempt,
			nextAttempt,
		)
	}
}

func TestSyncAgentRuntimesHonorsRepeatedFailureBackoff(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AgentWorkspaceRoot: filepath.Join(root, "agents")}
	workspace := &workspaceResponse{Agents: []*agent{{ID: "agent-1"}}}
	wantErr := errors.New("injected repeated runtime failure")
	watcher := newScriptedWorkspaceWatcher()
	watcher.add = func(string) error { return wantErr }
	runtime, err := newWorkspaceRuntime(
		cfg,
		http.DefaultClient,
		agentWorkspacePath(cfg, "agent-1"),
		"agent-1",
		"agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		service.closeAgentRuntimes()
	})
	ready := make(chan error, 1)
	managed := startSupervisedAgentWorkspaceRuntimeForTest(
		t,
		service,
		ctx,
		"agent-1",
		runtime,
		workspace,
		1,
		ready,
	)
	if err := <-ready; !errors.Is(err, wantErr) {
		t.Fatalf("agent runtime error = %v, want %v", err, wantErr)
	}
	select {
	case <-managed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("failed agent runtime did not stop")
	}
	if err := service.syncAgentRuntimes(ctx, workspace); err != nil {
		t.Fatalf("workspace refresh during completion-owned backoff: %v", err)
	}
	time.Sleep(agentRuntimeRestartBaseDelay / 2)
	service.mu.Lock()
	duringBackoff := service.agentRuntimes["agent-1"]
	service.mu.Unlock()
	if duringBackoff != managed {
		t.Fatal("managed runtime restarted before its repeated-failure backoff elapsed")
	}
	replacement, _ := waitForAgentRuntimeWatcher(t, service, "agent-1", managed)
	if replacement.restartAttempt != 2 {
		t.Fatalf("replacement restart attempt = %d, want 2", replacement.restartAttempt)
	}
}

func TestSyncAgentRuntimesRestartsAfterStartupAddFailure(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AgentWorkspaceRoot: filepath.Join(root, "agents")}
	workspace := &workspaceResponse{Agents: []*agent{{ID: "agent-1"}}}
	wantErr := errors.New("injected agent watcher Add failure")
	watcher := newScriptedWorkspaceWatcher()
	watcher.add = func(string) error { return wantErr }
	runtime, err := newWorkspaceRuntime(
		cfg,
		http.DefaultClient,
		agentWorkspacePath(cfg, "agent-1"),
		"agent-1",
		"agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		service.closeAgentRuntimes()
	})
	ready := make(chan error, 1)
	managed := startSupervisedAgentWorkspaceRuntimeForTest(
		t,
		service,
		ctx,
		"agent-1",
		runtime,
		workspace,
		0,
		ready,
	)
	if err := <-ready; !errors.Is(err, wantErr) {
		t.Fatalf("agent runtime error = %v, want %v", err, wantErr)
	}
	select {
	case <-managed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("failed agent runtime did not stop")
	}
	replacement, _ := waitForAgentRuntimeWatcher(t, service, "agent-1", managed)
	select {
	case <-replacement.done:
		t.Fatal("replacement agent runtime stopped immediately")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSyncAgentRuntimesRestartsAfterFatalWatcherEvent(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AgentWorkspaceRoot: filepath.Join(root, "agents")}
	workspace := &workspaceResponse{Agents: []*agent{{ID: "agent-1"}}}
	watcher := newScriptedWorkspaceWatcher()
	runtime, err := newWorkspaceRuntime(
		cfg,
		http.DefaultClient,
		agentWorkspacePath(cfg, "agent-1"),
		"agent-1",
		"agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		service.closeAgentRuntimes()
	})
	ready := make(chan error, 1)
	managed := startSupervisedAgentWorkspaceRuntimeForTest(
		t,
		service,
		ctx,
		"agent-1",
		runtime,
		workspace,
		0,
		ready,
	)
	if err := <-ready; err != nil {
		t.Fatalf("agent runtime startup: %v", err)
	}
	badPath := filepath.Join(runtime.replica.rootDir, strings.Repeat("x", 5000))
	watcher.events <- fsnotify.Event{Name: badPath, Op: fsnotify.Create}
	select {
	case <-managed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher event processing failure did not stop the agent runtime")
	}
	if runErr, _ := managed.result(); runErr == nil {
		t.Fatal("watcher event processing failure returned nil")
	}
	replacement, _ := waitForAgentRuntimeWatcher(t, service, "agent-1", managed)
	select {
	case <-replacement.done:
		t.Fatal("replacement agent runtime stopped immediately")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServiceEarlyWorkspaceSetupFailureClosesGateway(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: Config{
		AgentToolBaseURL:   "http://" + addr,
		WorkspaceDir:       filepath.Join(blocker, "workspace"),
		AgentWorkspaceRoot: filepath.Join(root, "agents"),
	}}
	if err := service.Run(context.Background()); err == nil {
		t.Fatal("expected workspace setup failure")
	}
	conn, dialErr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if conn != nil {
		_ = conn.Close()
	}
	if service.toolServer != nil {
		_ = service.toolServer.Close()
	}
	if dialErr == nil {
		t.Fatal("tool gateway still accepted connections after setup failed")
	}
}

func TestToolGatewayCloseIngressDefersListenerTeardownToDrain(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		cfg:      Config{AgentToolBaseURL: "http://" + addr},
		sessions: newAgentSessionSupervisor(Config{}, nil, newRuntimeRegistry()),
	}
	gateway, err := service.startToolGateway()
	if err != nil {
		t.Fatal(err)
	}
	drained := false
	defer func() {
		if drained {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = gateway.Drain(ctx)
	}()
	if err := gateway.CloseIngress(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gateway.Done():
		t.Fatal("CloseIngress took listener ownership away from Server.Shutdown")
	case <-time.After(50 * time.Millisecond):
	}
	res, err := http.Get("http://" + addr + "/agent-tools/list-documents")
	if err != nil {
		t.Fatalf("closed ingress should reject through the HTTP gate: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("closed ingress status = %d, want %d", res.StatusCode, http.StatusServiceUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gateway.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	drained = true
}

func TestWorkspaceEventLoopStopsWhenContextIsCanceled(t *testing.T) {
	connected := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			connected <- conn
		}
	}))
	defer backend.Close()

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{cfg: Config{BackendURL: backend.URL}}
	done := make(chan struct{})
	go func() {
		service.workspaceEventLoop(ctx, make(chan error, 1))
		close(done)
	}()
	var serverConn *websocket.Conn
	select {
	case serverConn = <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("workspace event loop did not connect")
	}
	cancel()
	stoppedOnCancel := false
	select {
	case <-done:
		stoppedOnCancel = true
	case <-time.After(300 * time.Millisecond):
	}
	_ = serverConn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace event loop did not stop after cleanup closed the peer")
	}
	if !stoppedOnCancel {
		t.Fatal("workspace event loop remained blocked in ReadMessage after context cancellation")
	}
}

type toolRequestResult struct {
	status int
	body   string
	err    error
}

func TestToolGatewayDrainWaitsForAdmittedHandlersAfterDeadline(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := newWorkspaceRuntime(
		Config{},
		http.DefaultClient,
		t.TempDir(),
		"daemon",
		"daemon",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.rootDocumentID = "root"
	defer runtime.Close()
	service := &Service{
		cfg:            Config{AgentToolBaseURL: "http://" + addr},
		primaryRuntime: runtime,
		sessions:       newAgentSessionSupervisor(Config{}, nil, newRuntimeRegistry()),
	}
	process := &fakeRuntimeProcess{events: make(chan RuntimeEvent)}
	service.sessions.mu.Lock()
	service.sessions.sessions["tool"] = &managedAgentSession{
		agent:     &agent{ID: "", Handle: "tool"},
		process:   process,
		toolToken: "tool-token",
		state:     "idle",
	}
	service.sessions.mu.Unlock()
	gateway, err := service.startToolGateway()
	if err != nil {
		t.Fatal(err)
	}
	drainStarted := false
	drainCompleted := false
	defer func() {
		if !drainStarted || drainCompleted {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = gateway.Drain(ctx)
		}
	}()

	heldConn, err := runtime.docCache.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitCount := runtime.docCache.db.Stats().WaitCount
	requestDone := make(chan toolRequestResult, 1)
	go func() {
		req, reqErr := http.NewRequest(http.MethodGet, "http://"+addr+"/agent-tools/list-documents", nil)
		if reqErr != nil {
			requestDone <- toolRequestResult{err: reqErr}
			return
		}
		req.Header.Set("Authorization", "Bearer tool-token")
		res, reqErr := http.DefaultClient.Do(req)
		if reqErr != nil {
			requestDone <- toolRequestResult{err: reqErr}
			return
		}
		defer res.Body.Close()
		body, readErr := io.ReadAll(res.Body)
		requestDone <- toolRequestResult{status: res.StatusCode, body: string(body), err: readErr}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.docCache.db.Stats().WaitCount == waitCount && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if runtime.docCache.db.Stats().WaitCount == waitCount {
		_ = heldConn.Close()
		t.Fatal("tool request did not enter the document-cache handler")
	}

	drainDone := make(chan error, 1)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelDrain()
	drainStarted = true
	go func() { drainDone <- gateway.Drain(drainCtx) }()
	var earlyDrain error
	drainReturnedWhileBlocked := false
	select {
	case earlyDrain = <-drainDone:
		drainCompleted = true
		drainReturnedWhileBlocked = true
	case <-time.After(100 * time.Millisecond):
	}
	if err := heldConn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("tool handler did not finish after its cache dependency was released")
	}
	if !drainCompleted {
		select {
		case earlyDrain = <-drainDone:
			drainCompleted = true
		case <-time.After(3 * time.Second):
			t.Fatal("gateway drain did not finish after the admitted handler returned")
		}
	}
	if earlyDrain == nil || !errors.Is(earlyDrain, context.DeadlineExceeded) {
		t.Fatalf("gateway drain error = %v, want deadline exceeded after forced connection close", earlyDrain)
	}
	if drainReturnedWhileBlocked {
		t.Fatal("gateway Drain returned after its deadline while an admitted handler still owned runtime dependencies")
	}
}

func TestServiceShutdownDrainsToolRequestBeforeClosingRuntimeCache(t *testing.T) {
	workspaceSockets := make(chan *websocket.Conn, 4)
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rootDocumentId":"root","agents":[]}`))
		case "/ws", "/ws/documents-sync":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err == nil {
				workspaceSockets <- conn
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toolAddr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ready := make(chan struct{})
	service := &Service{
		cfg: Config{
			BackendURL:         backend.URL,
			WorkspaceDir:       root,
			AgentWorkspaceRoot: filepath.Join(root, "agents"),
			AgentID:            "daemon",
			AgentToolBaseURL:   "http://" + toolAddr,
		},
		client:        backend.Client(),
		sessions:      newAgentSessionSupervisor(Config{}, nil, newRuntimeRegistry()),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		ready:         ready,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	select {
	case <-ready:
	case err := <-runDone:
		cancel()
		t.Fatalf("service stopped before ready: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("service did not become ready")
	}

	runtime := service.primaryRuntime
	if runtime == nil || runtime.docCache == nil {
		cancel()
		<-runDone
		t.Fatal("ready service has no primary document cache")
	}
	process := &fakeRuntimeProcess{events: make(chan RuntimeEvent)}
	service.sessions.mu.Lock()
	service.sessions.sessions["tool"] = &managedAgentSession{
		agent:     &agent{ID: "", Handle: "tool"},
		process:   process,
		toolToken: "tool-token",
		state:     "idle",
	}
	service.sessions.mu.Unlock()
	probeReq, err := http.NewRequest(http.MethodGet, "http://"+toolAddr+"/agent-tools/list-documents", nil)
	if err != nil {
		cancel()
		<-runDone
		t.Fatal(err)
	}
	probeReq.Header.Set("Authorization", "Bearer tool-token")
	probeRes, err := http.DefaultClient.Do(probeReq)
	if err != nil {
		cancel()
		<-runDone
		t.Fatalf("authorized tool probe: %v", err)
	}
	_, _ = io.Copy(io.Discard, probeRes.Body)
	_ = probeRes.Body.Close()
	if probeRes.StatusCode != http.StatusOK {
		cancel()
		<-runDone
		t.Fatalf("authorized tool probe status = %d, want 200", probeRes.StatusCode)
	}
	// Let startup reconciliation finish so the next increase in WaitCount is
	// attributable to the deliberately blocked tool request below.
	stableWaitCount := runtime.docCache.db.Stats().WaitCount
	stableSince := time.Now()
	for time.Since(stableSince) < 100*time.Millisecond {
		time.Sleep(time.Millisecond)
		current := runtime.docCache.db.Stats().WaitCount
		if current != stableWaitCount {
			stableWaitCount = current
			stableSince = time.Now()
		}
	}
	heldConn, err := runtime.docCache.db.Conn(context.Background())
	if err != nil {
		cancel()
		<-runDone
		t.Fatal(err)
	}

	waitCount := runtime.docCache.db.Stats().WaitCount
	requestDone := make(chan toolRequestResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, "http://"+toolAddr+"/agent-tools/list-documents", nil)
		if err != nil {
			requestDone <- toolRequestResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer tool-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			requestDone <- toolRequestResult{err: err}
			return
		}
		defer res.Body.Close()
		body, readErr := io.ReadAll(res.Body)
		requestDone <- toolRequestResult{status: res.StatusCode, body: string(body), err: readErr}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.docCache.db.Stats().WaitCount == waitCount && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if runtime.docCache.db.Stats().WaitCount == waitCount {
		_ = heldConn.Close()
		cancel()
		<-runDone
		t.Fatal("tool request did not begin using the document cache")
	}

	cancel()
	var earlyRequest *toolRequestResult
	select {
	case result := <-requestDone:
		earlyRequest = &result
	case <-time.After(300 * time.Millisecond):
	}
	if err := runtime.pathLocks.cleanupExpired(time.Now().UTC()); err != nil {
		_ = heldConn.Close()
		t.Fatalf("path-lock store closed while a tool cache user was still active: %v", err)
	}
	var earlyRun *error
	select {
	case err := <-runDone:
		earlyRun = &err
	default:
	}
	if err := heldConn.Close(); err != nil {
		t.Fatal(err)
	}
	var result toolRequestResult
	if earlyRequest != nil {
		result = *earlyRequest
	} else {
		select {
		case result = <-requestDone:
		case <-time.After(3 * time.Second):
			t.Fatal("tool request did not drain after releasing the cache")
		}
	}
	var runErr error
	if earlyRun != nil {
		runErr = *earlyRun
	} else {
		select {
		case runErr = <-runDone:
		case <-time.After(3 * time.Second):
			t.Fatal("service did not stop after tool request drained")
		}
	}
	for {
		select {
		case conn := <-workspaceSockets:
			_ = conn.Close()
		default:
			goto socketsClosed
		}
	}

socketsClosed:
	if earlyRequest != nil {
		t.Fatalf("tool request returned before shutdown released its cache dependency: status=%d body=%q err=%v", result.status, result.body, result.err)
	}
	if earlyRun != nil {
		t.Fatalf("service returned before its in-flight tool request drained: %v", runErr)
	}
	if result.err != nil || result.status != http.StatusOK {
		t.Fatalf("drained tool request result: status=%d body=%q err=%v", result.status, result.body, result.err)
	}
	if runErr != nil {
		t.Fatalf("service shutdown: %v", runErr)
	}
	if err := runtime.pathLocks.cleanupExpired(time.Now().UTC()); err == nil {
		t.Fatal("path-lock store remained open after service quiescence")
	}
}
