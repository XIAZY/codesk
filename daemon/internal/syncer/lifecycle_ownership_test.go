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

func startManagedWorkspaceRuntimeForTest(
	ctx context.Context,
	runtime *workspaceRuntime,
	ready chan<- error,
) (*managedWorkspaceRuntime, <-chan error) {
	runtimeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	result := make(chan error, 1)
	managed := &managedWorkspaceRuntime{runtime: runtime, cancel: cancel, done: done}
	go func() {
		runErr := runtime.run(runtimeCtx, ready)
		managed.recordResult(runErr)
		result <- runErr
		close(done)
	}()
	return managed, result
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
}

func TestSyncAgentRuntimesHonorsRepeatedFailureBackoff(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AgentWorkspaceRoot: filepath.Join(root, "agents")}
	workspace := &workspaceResponse{Agents: []*agent{{ID: "agent-1"}}}
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
	done := make(chan struct{})
	close(done)
	stoppedAt := time.Now()
	managed := &managedWorkspaceRuntime{
		runtime:        runtime,
		cancel:         func() {},
		done:           done,
		runErr:         errors.New("injected repeated runtime failure"),
		startedAt:      stoppedAt.Add(-time.Second),
		stoppedAt:      stoppedAt,
		restartAttempt: 1,
	}
	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{"agent-1": managed},
	}
	defer service.closeAgentRuntimes()

	if err := service.syncAgentRuntimes(context.Background(), workspace); err != nil {
		t.Fatalf("sync during backoff: %v", err)
	}
	service.mu.Lock()
	duringBackoff := service.agentRuntimes["agent-1"]
	service.mu.Unlock()
	if duringBackoff != managed {
		t.Fatal("managed runtime restarted before its repeated-failure backoff elapsed")
	}

	managed.resultMu.Lock()
	managed.stoppedAt = time.Now().Add(-agentRuntimeRestartBaseDelay - time.Millisecond)
	managed.startedAt = managed.stoppedAt.Add(-time.Second)
	managed.resultMu.Unlock()
	if err := service.syncAgentRuntimes(context.Background(), workspace); err != nil {
		t.Fatalf("sync after backoff: %v", err)
	}
	service.mu.Lock()
	replacement := service.agentRuntimes["agent-1"]
	service.mu.Unlock()
	if replacement == managed {
		t.Fatal("managed runtime was not replaced after its backoff elapsed")
	}
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
	runtime.initialWorkspace = workspace
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	managed, result := startManagedWorkspaceRuntimeForTest(context.Background(), runtime, make(chan error, 1))
	if err := <-result; !errors.Is(err, wantErr) {
		t.Fatalf("agent runtime error = %v, want %v", err, wantErr)
	}
	<-managed.done

	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{"agent-1": managed},
	}
	defer service.closeAgentRuntimes()
	if err := service.syncAgentRuntimes(context.Background(), workspace); err != nil {
		t.Fatalf("sync agent runtimes: %v", err)
	}
	service.mu.Lock()
	replacement := service.agentRuntimes["agent-1"]
	service.mu.Unlock()
	if replacement == managed {
		t.Fatal("stopped agent runtime remained registered instead of being restarted")
	}
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
	runtime.initialWorkspace = workspace
	runtime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	ready := make(chan error, 1)
	managed, result := startManagedWorkspaceRuntimeForTest(context.Background(), runtime, ready)
	if err := <-ready; err != nil {
		t.Fatalf("agent runtime startup: %v", err)
	}
	badPath := filepath.Join(runtime.replica.rootDir, strings.Repeat("x", 5000))
	watcher.events <- fsnotify.Event{Name: badPath, Op: fsnotify.Create}
	if err := <-result; err == nil {
		t.Fatal("watcher event processing failure did not stop the agent runtime")
	}
	<-managed.done

	service := &Service{
		cfg:           cfg,
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{"agent-1": managed},
	}
	defer service.closeAgentRuntimes()
	if err := service.syncAgentRuntimes(context.Background(), workspace); err != nil {
		t.Fatalf("sync agent runtimes: %v", err)
	}
	service.mu.Lock()
	replacement := service.agentRuntimes["agent-1"]
	service.mu.Unlock()
	if replacement == managed {
		t.Fatal("fatal event left a dead agent runtime registered instead of restarting it")
	}
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
