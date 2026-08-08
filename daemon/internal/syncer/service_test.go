package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

type documentUpdateTestResponse struct {
	Accepted bool  `json:"accepted"`
	Applied  bool  `json:"applied"`
	UpdateID int64 `json:"updateId"`
}

func TestNewServiceHasFreshReadinessSignal(t *testing.T) {
	first := New(Config{})
	if first.Ready() == nil {
		t.Fatal("Ready() returned nil")
	}
	select {
	case <-first.Ready():
		t.Fatal("new service reported ready before Run startup")
	default:
	}

	first.signalReady()
	first.signalReady()
	select {
	case <-first.Ready():
	case <-time.After(time.Second):
		t.Fatal("Ready() did not close after signalReady")
	}

	second := New(Config{})
	if second.Ready() == first.Ready() {
		t.Fatal("separate service generations share a readiness signal")
	}
	select {
	case <-second.Ready():
		t.Fatal("new generation inherited the prior generation's readiness")
	default:
	}
}

func TestNilServiceReadyIsNil(t *testing.T) {
	var service *Service
	if service.Ready() != nil {
		t.Fatal("nil Service.Ready() should return nil")
	}
}

func newDocumentUpdateWebsocketTestRuntime(t *testing.T, cache *documentCache, handler func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int)) *workspaceRuntime {
	t.Helper()
	runtime := &workspaceRuntime{
		cfg:      Config{BackendURL: "http://document-update.test", AgentID: "daemon_agent"},
		client:   http.DefaultClient,
		docCache: cache,
	}
	runtime.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		req := httptest.NewRequest(http.MethodGet, "/ws/documents-sync?actor="+url.QueryEscape(firstNonEmptyText(record.ActorID, runtime.cfg.AgentID))+"&actor_type="+url.QueryEscape(firstNonEmptyText(record.ActorType, "daemon")), bytes.NewReader(record.Update))
		req = req.WithContext(ctx)
		response, status := handler(documentID, record.Update, req)
		if status == 0 {
			status = http.StatusOK
		}
		if status >= http.StatusBadRequest {
			return &backendStatusError{Method: "WS", URL: "/ws/documents-sync", StatusCode: status, Body: "test failure"}
		}
		if !response.Accepted {
			return errors.New("document update was not accepted")
		}
		return nil
	}
	return runtime
}

func newApplyingDocumentUpdateWebsocketTestRuntime(t *testing.T, cache *documentCache, serverDoc *crdt.Doc, sent *int) *workspaceRuntime {
	t.Helper()
	return newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		if sent != nil {
			*sent = *sent + 1
		}
		if serverDoc != nil {
			if err := crdt.ApplyUpdateV1(serverDoc, update, "server"); err != nil {
				t.Fatalf("apply websocket update to server doc: %v", err)
			}
		}
		return documentUpdateTestResponse{Accepted: true, Applied: true, UpdateID: int64(1)}, http.StatusOK
	})
}

func TestServicePeriodicCadencesKeepDaemonOnlineIndependently(t *testing.T) {
	const backendOnlineWindow = 30 * time.Second

	if daemonStatusHeartbeatInterval != 10*time.Second {
		t.Fatalf("daemon heartbeat interval = %s, want 10s", daemonStatusHeartbeatInterval)
	}
	if daemonStatusHeartbeatInterval >= backendOnlineWindow {
		t.Fatalf("daemon heartbeat interval %s must stay inside backend online window %s", daemonStatusHeartbeatInterval, backendOnlineWindow)
	}
	if workspaceRefreshInterval != time.Minute {
		t.Fatalf("workspace refresh interval = %s, want 1m", workspaceRefreshInterval)
	}
	if daemonStatusHeartbeatInterval == workspaceRefreshInterval {
		t.Fatal("daemon heartbeat and workspace refresh must use independent schedules")
	}
}

func TestDaemonStatusHeartbeatRunsWithoutWorkspaceRefresh(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		BackendURL:  server.URL,
		WorkspaceID: "workspace:test",
		DaemonToken: "daemon_token",
	}
	service := &Service{
		cfg:          cfg,
		client:       server.Client(),
		daemonStatus: newDaemonStatusReporter(cfg, server.Client()),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ticks := make(chan time.Time, 1)
	go func() {
		defer close(done)
		service.runDaemonStatusHeartbeat(ctx, ticks)
	}()

	ticks <- time.Now()
	select {
	case request := <-requests:
		if request.Method != http.MethodPatch || !strings.HasSuffix(request.URL.Path, "/daemon/status") {
			t.Fatalf("heartbeat request = %s %s, want daemon status PATCH", request.Method, request.URL.Path)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for daemon status heartbeat")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop after cancellation")
	}
}

func TestDaemonStatusHeartbeatSignalsRefreshOnlyWhenCatalogDiscoveryCompletes(t *testing.T) {
	tests := []struct {
		name        string
		catalog     *RuntimeModelCatalog
		wantRefresh bool
	}{
		{
			name: "success",
			catalog: &RuntimeModelCatalog{Models: []RuntimeModel{{
				Model: "gpt-5.6-sol",
			}}},
			wantRefresh: true,
		},
		{
			name: "failure",
			catalog: &RuntimeModelCatalog{
				Models: []RuntimeModel{},
				Error:  "model catalog unavailable",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := &asyncCatalogDriver{
				catalogs: []*RuntimeModelCatalog{test.catalog},
				called:   make(chan struct{}, 1),
			}
			registry := newRuntimeRegistry(driver)
			registry.DetectAll(context.Background())

			service := &Service{
				runtimes:      registry,
				refreshNeeded: make(chan struct{}, 1),
			}
			ctx, cancel := context.WithCancel(context.Background())
			ticks := make(chan time.Time)
			done := make(chan struct{})
			go func() {
				defer close(done)
				service.runDaemonStatusHeartbeat(ctx, ticks)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})

			ticks <- time.Now()
			select {
			case <-driver.called:
			case <-time.After(time.Second):
				t.Fatal("heartbeat did not run catalog discovery")
			}
			// The unbuffered send is accepted only after the first heartbeat
			// has completed its discovery and optional refresh signal.
			ticks <- time.Now()

			select {
			case <-service.refreshNeeded:
				if !test.wantRefresh {
					t.Fatal("failed catalog discovery woke agent reconciliation")
				}
			default:
				if test.wantRefresh {
					t.Fatal("successful catalog discovery did not wake agent reconciliation")
				}
			}
		})
	}
}

func TestRunReportsDaemonOnlineOnlyAfterInitialRefreshSucceeds(t *testing.T) {
	workspaceStarted := make(chan struct{}, 1)
	allowWorkspace := make(chan struct{})
	statusRequests := make(chan struct{}, 1)
	workspaceStreamStarted := make(chan struct{})
	workspaceStreamExited := make(chan struct{})
	var workspaceStreamStartedOnce sync.Once
	var workspaceStreamExitedOnce sync.Once
	workspaceUpgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daemon/status"):
			statusRequests <- struct{}{}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			select {
			case workspaceStarted <- struct{}{}:
			default:
			}
			select {
			case <-allowWorkspace:
				writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
			case <-r.Context().Done():
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := workspaceUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			workspaceStreamStartedOnce.Do(func() { close(workspaceStreamStarted) })
			defer workspaceStreamExitedOnce.Do(func() { close(workspaceStreamExited) })
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{
		BackendURL:         server.URL,
		WorkspaceID:        "workspace:test",
		DaemonToken:        "daemon_token",
		DataDir:            t.TempDir(),
		WorkspaceDir:       t.TempDir(),
		AgentWorkspaceRoot: t.TempDir(),
		AgentID:            "daemon_agent",
		AgentToolBaseURL:   "http://127.0.0.1:0",
	}
	primaryRuntime, storeCloses := newWorkspaceRuntimeWithDeterministicStores(
		t,
		cfg,
		server.Client(),
		cfg.WorkspaceDir,
		cfg.AgentID,
		"daemon",
	)
	watcher := newScriptedWorkspaceWatcher()
	primaryRuntime.replica.newWatcher = func() (workspaceWatcher, error) { return watcher, nil }
	service := &Service{
		cfg:            cfg,
		client:         server.Client(),
		daemonStatus:   newDaemonStatusReporter(cfg, server.Client()),
		primaryRuntime: primaryRuntime,
		agentRuntimes:  map[string]*managedWorkspaceRuntime{},
		agentWorkers:   map[string]*managedAgentWorker{},
		ready:          make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.Run(ctx)
	}()

	select {
	case <-workspaceStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("timed out waiting for initial workspace recovery")
	}
	select {
	case <-service.Ready():
		cancel()
		t.Fatal("service reported ready before initial workspace recovery completed")
	default:
	}
	select {
	case <-statusRequests:
		cancel()
		t.Fatal("daemon reported online before initial workspace recovery completed")
	default:
	}
	close(allowWorkspace)
	select {
	case <-statusRequests:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("daemon did not report online after initial workspace recovery completed")
	}
	select {
	case <-service.Ready():
	case <-time.After(time.Second):
		cancel()
		t.Fatal("service did not report ready after successful startup")
	}
	select {
	case <-workspaceStreamStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("workspace stream did not complete its handshake")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	select {
	case <-workspaceStreamExited:
	case <-time.After(time.Second):
		t.Fatal("workspace stream handler did not observe the client close")
	}
	assertDeterministicWorkspaceStoresClosed(t, storeCloses)
}

func TestRunStopsDaemonHeartbeatBeforeBlockingFatalShutdown(t *testing.T) {
	var statusCount atomic.Int64
	initialStatus := make(chan struct{}, 1)
	laterStatus := make(chan struct{}, 1)
	heartbeatTicks := make(chan time.Time, 1)
	workspaceStreamStarted := make(chan struct{})
	releaseWorkspaceStream := make(chan struct{})
	thirdReportStarted := make(chan struct{})
	releaseThirdReport := make(chan struct{})
	var workspaceStreamOnce sync.Once
	var releaseWorkspaceStreamOnce sync.Once
	var thirdReportOnce sync.Once
	var releaseThirdReportOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/daemon/status"):
			count := statusCount.Add(1)
			if count == 1 {
				initialStatus <- struct{}{}
			} else {
				select {
				case laterStatus <- struct{}{}:
				default:
				}
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			workspaceStreamOnce.Do(func() { close(workspaceStreamStarted) })
			<-releaseWorkspaceStream
			w.WriteHeader(http.StatusGone)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	serverClient := server.Client()
	serverTransport := serverClient.Transport
	var transportStatusCount atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/daemon/status") && transportStatusCount.Add(1) == 3 {
			thirdReportOnce.Do(func() { close(thirdReportStarted) })
			<-releaseThirdReport
		}
		return serverTransport.RoundTrip(r)
	})}

	cfg := Config{
		BackendURL:         server.URL,
		WorkspaceID:        "workspace:test",
		DaemonToken:        "daemon_token",
		DataDir:            t.TempDir(),
		WorkspaceDir:       t.TempDir(),
		AgentWorkspaceRoot: t.TempDir(),
		AgentID:            "daemon_agent",
		AgentToolBaseURL:   "http://127.0.0.1:0",
	}
	primaryRuntime, storeCloses := newWorkspaceRuntimeWithDeterministicStores(
		t,
		cfg,
		client,
		cfg.WorkspaceDir,
		cfg.AgentID,
		"daemon",
	)
	service := &Service{
		cfg:            cfg,
		client:         client,
		daemonStatus:   newDaemonStatusReporter(cfg, client),
		primaryRuntime: primaryRuntime,
		agentRuntimes:  map[string]*managedWorkspaceRuntime{},
		agentWorkers:   map[string]*managedAgentWorker{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runFinished := make(chan struct{})
	blockingRuntimeDone := make(chan struct{})
	shutdownBlocked := make(chan struct{})
	var blockingRuntimeDoneOnce sync.Once
	var shutdownBlockedOnce sync.Once
	t.Cleanup(func() {
		cancel()
		releaseWorkspaceStreamOnce.Do(func() { close(releaseWorkspaceStream) })
		releaseThirdReportOnce.Do(func() { close(releaseThirdReport) })
		blockingRuntimeDoneOnce.Do(func() { close(blockingRuntimeDone) })
		select {
		case <-runFinished:
		case <-time.After(time.Second):
		}
	})
	go func() {
		defer close(runFinished)
		done <- service.run(ctx, heartbeatTicks)
	}()

	select {
	case <-initialStatus:
	case <-time.After(time.Second):
		t.Fatal("daemon did not report initial online status")
	}
	select {
	case <-workspaceStreamStarted:
	case <-time.After(time.Second):
		t.Fatal("workspace event stream did not start")
	}

	service.mu.Lock()
	service.agentRuntimes["blocking"] = &managedWorkspaceRuntime{
		cancel: func() {
			shutdownBlockedOnce.Do(func() { close(shutdownBlocked) })
		},
		done: blockingRuntimeDone,
	}
	service.mu.Unlock()

	heartbeatTicks <- time.Now()
	select {
	case <-laterStatus:
	case <-time.After(time.Second):
		t.Fatal("daemon did not emit a periodic heartbeat")
	}
	heartbeatTicks <- time.Now()
	select {
	case <-thirdReportStarted:
	case <-time.After(time.Second):
		t.Fatal("third daemon heartbeat did not enter the blocking transport")
	}
	releaseWorkspaceStreamOnce.Do(func() { close(releaseWorkspaceStream) })
	select {
	case <-shutdownBlocked:
		t.Fatal("fatal shutdown reached runtime teardown before the in-flight heartbeat joined")
	case <-time.After(100 * time.Millisecond):
	}
	releaseThirdReportOnce.Do(func() { close(releaseThirdReport) })
	select {
	case <-shutdownBlocked:
	case <-time.After(time.Second):
		t.Fatal("fatal workspace drain did not enter teardown after the in-flight heartbeat joined")
	}

	countAtShutdown := statusCount.Load()
	heartbeatTicks <- time.Now()
	select {
	case <-laterStatus:
		t.Fatalf("daemon status heartbeat advanced during blocking fatal shutdown: got %d requests, want %d", statusCount.Load(), countAtShutdown)
	case <-time.After(100 * time.Millisecond):
	}

	blockingRuntimeDoneOnce.Do(func() { close(blockingRuntimeDone) })
	select {
	case err := <-done:
		var statusErr *backendStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusGone {
			t.Fatalf("Run error = %v, want terminal workspace drain", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after shutdown unblocked")
	}
	assertDeterministicWorkspaceStoresClosed(t, storeCloses)
}

func TestComputeLocalTextEditsUsesUTF16Cursor(t *testing.T) {
	edits, err := computeLocalTextEdits("a🙂b", "a🙂Xb")
	if err != nil {
		t.Fatalf("compute edits: %v", err)
	}
	if len(edits) != 1 {
		t.Fatalf("expected one edit, got %#v", edits)
	}
	if edit := edits[0]; edit.Text != "X" || edit.Start != 3 || edit.Length != 1 {
		t.Fatalf("expected UTF-16 insert at 3, got %#v", edit)
	}
}

func TestBuildLocalUpdateFromBaseHandlesChineseTextDiff(t *testing.T) {
	const baseContent = "已完成\n"
	const localContent = "己完成\n"
	doc := newDocWithText(t, baseContent)
	update, _, err := buildLocalUpdateFromBase(doc.EncodeStateAsUpdate(), baseContent, localContent)
	if err != nil {
		t.Fatalf("build local update: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected local CRDT update")
	}
	if err := crdt.ApplyUpdateV1(doc, update, "local"); err != nil {
		t.Fatalf("apply local update: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != localContent {
		t.Fatalf("content mismatch: got %q want %q", got, localContent)
	}
}

func TestBuildLocalUpdateFromBaseHandlesMultipleTextEdits(t *testing.T) {
	const baseContent = "alpha\nbeta\ngamma\n"
	const localContent = "ALPHA\nbeta\nGAMMA\n"
	doc := newDocWithText(t, baseContent)
	update, _, err := buildLocalUpdateFromBase(doc.EncodeStateAsUpdate(), baseContent, localContent)
	if err != nil {
		t.Fatalf("build local update: %v", err)
	}
	if err := crdt.ApplyUpdateV1(doc, update, "local"); err != nil {
		t.Fatalf("apply local update: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != localContent {
		t.Fatalf("content mismatch: got %q want %q", got, localContent)
	}
}

func TestBuildLocalUpdateFromBaseReturnsNoUpdateForUnchangedContent(t *testing.T) {
	const baseContent = "base\n"
	update, observedState, err := buildLocalUpdateFromBase(crdtStateFromContent(baseContent), baseContent, baseContent)
	if err != nil {
		t.Fatalf("build local update: %v", err)
	}
	if len(update) != 0 {
		t.Fatalf("unchanged content should not produce update, got %v", update)
	}
	if len(observedState) == 0 {
		t.Fatal("expected observed CRDT state")
	}
}

func TestBuildLocalUpdateFromBaseRejectsInvalidUTF8Content(t *testing.T) {
	_, _, err := buildLocalUpdateFromBase(crdtStateFromContent("base"), "base", string([]byte{'b', 0xff, 'd'}))
	if !errors.Is(err, errUnsupportedTextContent) {
		t.Fatalf("expected unsupported text error, got %v", err)
	}
}

func TestReconcileTrackedDocumentSkipsInvalidUTF8LocalFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte{'b', 0xff, 'd'}, 0o644); err != nil {
		t.Fatalf("write invalid local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBase("base", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	var logBuf bytes.Buffer
	oldLogWriter := log.Writer()
	oldLogFlags := log.Flags()
	oldLogPrefix := log.Prefix()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldLogWriter)
		log.SetFlags(oldLogFlags)
		log.SetPrefix(oldLogPrefix)
	})

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send update for invalid UTF-8 local file; update bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("invalid local file should be skipped, got error: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("invalid local file should remain dirty for a later valid edit")
	}
	logged := logBuf.String()
	for _, want := range []string{
		"document reconcile skipped unsupported text content",
		"document_id=doc_1",
		`document_path="doc.md"`,
		fmt.Sprintf("local_path=%q", path),
		"reason=local file is not valid UTF-8",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("expected log to contain %q, got %q", want, logged)
		}
	}
}

func TestReconcileAgentReplicasKeepsUUIDActorWhenHandleChanges(t *testing.T) {
	cancelled := false
	service := &Service{
		agentRuntimes: map[string]*managedWorkspaceRuntime{
			"agent_1": {
				runtime: &workspaceRuntime{replica: &workspaceReplica{actorID: "agent_1"}},
				cancel: func() {
					cancelled = true
				},
			},
		},
		agentBaseCtx: context.Background(),
	}
	workspace := &workspaceResponse{
		Agents: []*agent{{ID: "agent_1", Handle: "renamed-agent"}},
	}
	if err := service.syncAgentRuntimes(context.Background(), workspace); err != nil {
		t.Fatalf("sync agent runtimes: %v", err)
	}
	if cancelled {
		t.Fatal("agent replica should not restart when only the handle changes")
	}
	runtime := service.agentRuntimes["agent_1"]
	if runtime == nil || runtime.runtime == nil || runtime.runtime.replica == nil || runtime.runtime.replica.actorID != "agent_1" {
		t.Fatalf("expected UUID actor to be preserved, got %#v", runtime)
	}
}

func TestReconcileAgentReplicasCleanupRemovesOnlyCanonicalAgentPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "agent_four")
	sibling := filepath.Join(root, "agent_five")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create target agent path: %v", err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("create sibling agent path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "sentinel"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write sibling sentinel: %v", err)
	}

	service := &Service{
		cfg: Config{AgentWorkspaceRoot: root},
		agentRuntimes: map[string]*managedWorkspaceRuntime{
			"agent/four": {},
		},
		agentBaseCtx: context.Background(),
	}
	if err := service.syncAgentRuntimes(context.Background(), &workspaceResponse{}); err != nil {
		t.Fatalf("sync agent runtimes: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical stale agent path should be removed, stat error = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(sibling, "sentinel")); err != nil || string(data) != "keep" {
		t.Fatalf("sibling agent path was modified: data=%q err=%v", data, err)
	}
}

func TestIgnoredWorkspacePathPolicy(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		path    string
		ignored bool
	}{
		{path: filepath.Join(root, ".notty", "cache", "state.txt"), ignored: true},
		{path: filepath.Join(root, ".env"), ignored: true},
		{path: filepath.Join(root, "docs", ".cache", "state.bin"), ignored: true},
		{path: filepath.Join(root, "docs", ".draft.md"), ignored: true},
		{path: filepath.Join(root, "docs", "spec.md"), ignored: false},
		{path: root, ignored: false},
		{path: filepath.Dir(root), ignored: true},
	}
	for _, tc := range cases {
		if got := isIgnoredWorkspaceAbsolutePath(root, tc.path); got != tc.ignored {
			t.Fatalf("isIgnoredWorkspaceAbsolutePath(%q) = %v, want %v", tc.path, got, tc.ignored)
		}
	}
}

func TestScanWorkspaceFilesIgnoresDotPaths(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"visible.md":               "visible",
		"docs/spec.md":             "spec",
		".notty/cache/state.txt":   "internal",
		".env":                     "secret",
		"docs/.cache/state.bin":    "cache",
		"docs/.draft.md":           "draft",
		"docs/nested/visible.md":   "nested",
		"docs/nested/.ignored.txt": "ignored",
	}
	for path, content := range files {
		absolutePath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(absolutePath, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	scanned, err := scanWorkspaceFiles(root)
	if err != nil {
		t.Fatalf("scan workspace files: %v", err)
	}
	got := make(map[string]string, len(scanned))
	for absolutePath, content := range scanned {
		relative, err := filepath.Rel(root, absolutePath)
		if err != nil {
			t.Fatalf("rel %s: %v", absolutePath, err)
		}
		got[filepath.ToSlash(relative)] = content
	}
	want := map[string]string{
		"visible.md":             "visible",
		"docs/spec.md":           "spec",
		"docs/nested/visible.md": "nested",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanned files mismatch:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestScanWorkspaceFilesUsesOccupantClassification(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, ".linked-target.md")
	if err := os.WriteFile(targetPath, []byte("linked content\n"), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	linkedPath := filepath.Join(root, "linked.md")
	if err := os.Symlink(targetPath, linkedPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target.md"), filepath.Join(root, "dangling.md")); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory.md"), 0o755); err != nil {
		t.Fatalf("create directory occupant: %v", err)
	}

	files, err := scanWorkspaceFiles(root)
	if err != nil {
		t.Fatalf("scan classified workspace: %v", err)
	}
	if got := files[linkedPath]; got != "linked content\n" {
		t.Fatalf("symlinked file content = %q, want linked content", got)
	}
	if len(files) != 1 {
		t.Fatalf("classified workspace files = %#v, want only symlinked regular content", files)
	}
}

func TestNextAwarenessClientIDIsSafeForYjsClients(t *testing.T) {
	awarenessClientCounter.Store(0)
	for index := 0; index < 10; index++ {
		clientID := nextAwarenessClientID()
		if clientID == 0 || clientID > (1<<53)-1 {
			t.Fatalf("awareness client id %d is outside the JS-safe Yjs range", clientID)
		}
	}
}

func TestHandleLocalChangeIgnoresDotPaths(t *testing.T) {
	root := t.TempDir()
	hiddenPath := filepath.Join(root, ".notty", "cache", "state.txt")
	visiblePath := filepath.Join(root, "doc.md")
	hidden := &trackedFile{Path: hiddenPath}
	hidden.setProjectedContent("hidden")
	visible := &trackedFile{Path: visiblePath}
	visible.setProjectedContent("visible")
	replica := &workspaceReplica{
		rootDir: root,
		projectedByPath: map[string]*trackedFile{
			hiddenPath:  hidden,
			visiblePath: visible,
		},
	}

	if err := replica.handleLocalChange(hiddenPath); err != nil {
		t.Fatalf("handle hidden local change: %v", err)
	}
	if hidden.isLocalDirty() {
		t.Fatal("hidden path was marked dirty")
	}
	if err := replica.handleLocalChange(visiblePath); err != nil {
		t.Fatalf("handle visible local change: %v", err)
	}
	if !visible.isLocalDirty() {
		t.Fatal("visible path was not marked dirty")
	}
}

func TestReconcileQueueCoalescesDocumentIDs(t *testing.T) {
	queue := newReconcileQueue()
	queue.Mark("doc_b")
	queue.Mark("doc_a")
	queue.Mark("doc_b")
	queue.Mark(" ")

	if queue.Len() != 2 {
		t.Fatalf("expected two unique dirty documents, got %d", queue.Len())
	}
	if got := queue.Drain(); !reflect.DeepEqual(got, []string{"doc_a", "doc_b"}) {
		t.Fatalf("unexpected drain result: %#v", got)
	}
	if queue.Len() != 0 {
		t.Fatalf("expected empty queue after drain, got %d", queue.Len())
	}
}

func TestWorkspaceReplicaLocalChangeMarksDirtyDocument(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	tracked := &trackedFile{DocumentID: "doc_1", Path: path}
	tracked.setProjectedContent("base")

	queue := newReconcileQueue()
	replica := &workspaceReplica{
		rootDir:         root,
		markDirty:       queue.Mark,
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}
	if got := queue.Drain(); !reflect.DeepEqual(got, []string{"doc_1"}) {
		t.Fatalf("expected dirty document to be queued, got %#v", got)
	}
}

func TestReconcileDirtyDocumentsRequeuesOutboxAfterHTTPError(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	entry, unlock := cache.lockEntry("doc_1")
	if err := cache.storeOutboxUpdateLocked(entry, "doc_1", "doc.md", outboxUpdateRecord{
		Update:          []byte{1, 2, 3},
		ObservedContent: "local",
		SourcePath:      filepath.Join(t.TempDir(), "doc.md"),
	}); err != nil {
		unlock()
		t.Fatalf("store outbox: %v", err)
	}
	unlock()

	queue := newReconcileQueue()
	queue.Mark("doc_1")
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         filepath.Join(t.TempDir(), "doc.md"),
	}
	attempts := 0
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		attempts++
		if documentID != "doc_1" {
			t.Fatalf("unexpected document id %q", documentID)
		}
		return documentUpdateTestResponse{}, http.StatusServiceUnavailable
	})
	service.reconcileQueue = queue
	service.replica = &workspaceReplica{
		projectedByID: map[string]*trackedFile{"doc_1": tracked},
	}

	if err := service.reconcileDirtyDocuments(context.Background()); err == nil {
		t.Fatal("expected reconcile dirty documents to report backend send failure")
	}
	if attempts != 1 {
		t.Fatalf("expected one websocket send attempt, got %d", attempts)
	}
	if got := queue.Drain(); !reflect.DeepEqual(got, []string{"doc_1"}) {
		t.Fatalf("expected failed outbox to be requeued, got %#v", got)
	}
}

func TestReconcileDirtyDocumentsDoesNotDropLaterIDsAfterError(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	entry, unlock := cache.lockEntry("doc_bad")
	if err := cache.storeOutboxUpdateLocked(entry, "doc_bad", "bad.md", outboxUpdateRecord{
		Update:          []byte{1, 2, 3},
		ObservedContent: "bad",
		SourcePath:      filepath.Join(t.TempDir(), "bad.md"),
	}); err != nil {
		unlock()
		t.Fatalf("store bad outbox: %v", err)
	}
	unlock()
	entry, unlock = cache.lockEntry("doc_retry")
	if err := cache.storeOutboxUpdateLocked(entry, "doc_retry", "retry.md", outboxUpdateRecord{
		Update:          []byte{4, 5, 6},
		ObservedContent: "retry",
		SourcePath:      filepath.Join(t.TempDir(), "retry.md"),
	}); err != nil {
		unlock()
		t.Fatalf("store retry outbox: %v", err)
	}
	unlock()

	queue := newReconcileQueue()
	queue.Mark("doc_bad")
	queue.Mark("doc_retry")
	trackedBad := &trackedFile{DocumentID: "doc_bad", DocumentPath: "bad.md"}
	trackedRetry := &trackedFile{
		DocumentID:   "doc_retry",
		DocumentPath: "retry.md",
	}
	attempts := 0
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		attempts++
		return documentUpdateTestResponse{}, http.StatusServiceUnavailable
	})
	service.reconcileQueue = queue
	service.replica = &workspaceReplica{projectedByID: map[string]*trackedFile{
		"doc_bad":   trackedBad,
		"doc_retry": trackedRetry,
	}}

	if err := service.reconcileDirtyDocuments(context.Background()); err == nil {
		t.Fatal("expected backend send error")
	}
	if attempts != 2 {
		t.Fatalf("expected both dirty documents to be attempted, got %d", attempts)
	}
	if got := queue.Drain(); !reflect.DeepEqual(got, []string{"doc_bad", "doc_retry"}) {
		t.Fatalf("expected failed and still-pending documents to be requeued, got %#v", got)
	}
}

func TestAgentStatusWorkerWritesLatestStateAfterInFlightUpdate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var calls []updateAgentSessionRequest
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		if agentID != "agent_1" {
			t.Fatalf("unexpected agent id: %s", agentID)
		}
		mu.Lock()
		calls = append(calls, payload)
		mu.Unlock()
		if payload.Status == "working" {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	worker := newAgentStatusWorker("agent_1", updater)
	defer worker.Stop()

	worker.Publish(updateAgentSessionRequest{Status: "working", CurrentTurnID: "turn_1"})
	<-started
	worker.Publish(updateAgentSessionRequest{Status: "idle", CurrentTurnID: "", CurrentActivity: "Idle"})
	close(release)

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 2 && calls[len(calls)-1].Status == "idle"
	})
}

func TestAgentStatusWorkerRetriesLatestStateAfterFailure(t *testing.T) {
	firstCall := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var calls []updateAgentSessionRequest
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		mu.Lock()
		calls = append(calls, payload)
		count := len(calls)
		mu.Unlock()
		if count == 1 {
			once.Do(func() { close(firstCall) })
			return errors.New("temporary failure")
		}
		return nil
	}
	worker := newAgentStatusWorker("agent_1", updater)
	defer worker.Stop()

	worker.Publish(updateAgentSessionRequest{Status: "working", CurrentTurnID: "turn_1"})
	<-firstCall
	worker.Publish(updateAgentSessionRequest{Status: "idle", CurrentTurnID: "", CurrentActivity: "Idle"})

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 2 && calls[1].Status == "idle"
	})
}

func TestRefreshSharesFetchedWorkspaceWithWorkersAndReplicas(t *testing.T) {
	var workspaceSyncRequests atomic.Int32
	workspace := workspaceResponse{
		Agents: []*agent{{
			ID:     "agent_1",
			Handle: "agent-one",
			Name:   "Agent One",
			Kind:   "codex",
		}},
	}
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
				workspaceSyncRequests.Add(1)
				body, err := json.Marshal(workspace)
				if err != nil {
					t.Fatalf("marshal workspace: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/") && r.URL.Query().Get("status") == "pending":
				body, err := json.Marshal(toolInboxResponse{Items: []*agentEvent{}})
				if err != nil {
					t.Fatalf("marshal inbox: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
				return nil, nil
			}
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{
		cfg: Config{
			BackendURL:         "http://backend.test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        client,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		agentBaseCtx:  ctx,
	}
	defer service.closePrimaryRuntime()

	if err := service.refresh(ctx, service.snapshotEpoch.Load()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	service.closeAgentWorkers()
	service.closeAgentRuntimes()

	if got := workspaceSyncRequests.Load(); got != 1 {
		t.Fatalf("expected one shared workspace sync request during refresh, got %d", got)
	}
}

func TestInitialRefreshFailsFastOnAgentStartupError(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.startErr = errors.New("codex missing")
	var workspaceRequests atomic.Int32
	workspace := workspaceResponse{
		Agents: []*agent{{
			ID:     "agent_1",
			Handle: "agent-one",
			Kind:   "codex",
		}},
	}
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
				workspaceRequests.Add(1)
				body, err := json.Marshal(workspace)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
				body, err := json.Marshal(toolInboxResponse{Items: []*agentEvent{}})
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}
		}),
	}
	service := &Service{
		cfg: Config{
			BackendURL:         "http://backend.test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        client,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		agentBaseCtx:  context.Background(),
	}
	defer service.closePrimaryRuntime()
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, newFakeRuntimeRegistry(factory))
	defer service.sessions.Shutdown()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	started := time.Now()
	err := service.refreshInitialWorkspace(context.Background())
	if err == nil {
		t.Fatal("expected initial refresh to fail on resident agent startup error")
	}
	var startupErr *agentSessionStartupError
	if !errors.As(err, &startupErr) || startupErr.AgentID != "agent_1" {
		t.Fatalf("expected agent startup error, got %T %v", err, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("agent startup errors should fail fast, took %s", elapsed)
	}
	if got := workspaceRequests.Load(); got != 1 {
		t.Fatalf("expected no retry after fatal agent startup error, got %d workspace requests", got)
	}
}

func TestRefreshStartsAgentSessionFromWorkspaceSnapshot(t *testing.T) {
	factory := newFakeRuntimeDriver()
	var workspaceRequests atomic.Int32
	workspace := workspaceResponse{
		Agents: []*agent{{
			ID:           "agent_1",
			Handle:       "agent-one",
			Name:         "Agent One",
			Kind:         "codex",
			SystemPrompt: "runtime instructions",
		}},
	}
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
				workspaceRequests.Add(1)
				body, err := json.Marshal(workspace)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
				body, err := json.Marshal(toolInboxResponse{Items: []*agentEvent{}})
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
				return nil, nil
			}
		}),
	}
	service := &Service{
		cfg: Config{
			BackendURL:         "http://backend.test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        client,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		agentBaseCtx:  context.Background(),
	}
	defer service.closePrimaryRuntime()
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, newFakeRuntimeRegistry(factory))
	defer service.sessions.Shutdown()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	if err := service.refresh(context.Background(), service.snapshotEpoch.Load()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := workspaceRequests.Load(); got != 1 {
		t.Fatalf("expected one workspace request, got %d", got)
	}
	process := factory.only(t)
	process.mu.Lock()
	started := process.started
	process.mu.Unlock()
	if !started {
		t.Fatal("expected daemon refresh to start runtime process for workspace agent")
	}
	starts := process.inputsByKind(RuntimeInputStartSession)
	if len(starts) != 1 {
		t.Fatalf("expected one runtime start session input, got %#v", starts)
	}
	if starts[0].Instructions != "runtime instructions" {
		t.Fatalf("expected workspace agent system prompt to reach runtime, got %#v", starts[0])
	}
}

func TestRefreshStopsAgentSessionWhenWorkspaceSnapshotRemovesAgent(t *testing.T) {
	factory := newFakeRuntimeDriver()
	var workspaceRequests atomic.Int32
	withAgent := workspaceResponse{
		Agents: []*agent{{
			ID:     "agent_1",
			Handle: "agent-one",
			Name:   "Agent One",
			Kind:   "codex",
		}},
	}
	withoutAgents := workspaceResponse{}
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
				workspace := withAgent
				if workspaceRequests.Add(1) > 1 {
					workspace = withoutAgents
				}
				body, err := json.Marshal(workspace)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
				body, err := json.Marshal(toolInboxResponse{Items: []*agentEvent{}})
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
				return nil, nil
			}
		}),
	}
	service := &Service{
		cfg: Config{
			BackendURL:         "http://backend.test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        client,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		agentBaseCtx:  context.Background(),
	}
	defer service.closePrimaryRuntime()
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, newFakeRuntimeRegistry(factory))
	defer service.sessions.Shutdown()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	if err := service.refresh(context.Background(), service.snapshotEpoch.Load()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	process := factory.only(t)
	process.mu.Lock()
	started := process.started
	process.mu.Unlock()
	if !started {
		t.Fatal("expected initial refresh to start runtime process for workspace agent")
	}

	if err := service.refresh(context.Background(), service.snapshotEpoch.Load()); err != nil {
		t.Fatalf("removal refresh: %v", err)
	}
	if got := workspaceRequests.Load(); got != 2 {
		t.Fatalf("expected two workspace requests, got %d", got)
	}
	process = factory.only(t)
	process.mu.Lock()
	stopped := process.stopped
	process.mu.Unlock()
	if !stopped {
		t.Fatal("expected daemon refresh to stop runtime process when workspace snapshot removes agent")
	}
}

func TestInitialRefreshFailsFastOnUnauthorizedBackend(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("forbidden")),
			}, nil
		}),
	}
	service := &Service{
		cfg:    Config{BackendURL: "http://backend.test"},
		client: client,
	}

	started := time.Now()
	err := service.refreshInitialWorkspace(context.Background())
	if err == nil {
		t.Fatal("expected initial refresh to fail on forbidden backend")
	}
	var statusErr *backendStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden backend status error, got %T %v", err, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("forbidden backend should fail fast, took %s", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected no retry after forbidden backend response, got %d requests", got)
	}
}

func TestApplyProjectedContentUpdatesSnapshotBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{Path: path})
	tracked.setProjectedContent("old")

	clean, err := applyProjectedContent(tracked, "new", nil, 0)
	if err != nil {
		t.Fatalf("apply projected content: %v", err)
	}
	if !clean {
		t.Fatal("expected projected write to leave disk clean")
	}

	if !tracked.matchesProjectedString("new") {
		t.Fatal("unexpected projected content hash after write")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(content) != "new" {
		t.Fatalf("expected projected file content to update, got %q", content)
	}
}

func TestApplyProjectedContentRollsBackOnWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{Path: path})
	tracked.setProjectedContent("old")

	if _, err := applyProjectedContent(tracked, "new", nil, 0); err == nil {
		t.Fatal("expected projected write failure")
	}

	if !tracked.matchesProjectedString("old") {
		t.Fatal("expected rollback to original projected content hash")
	}
}

func TestApplyProjectedContentDoesNotAdvanceBaseAfterDurabilityFailure(t *testing.T) {
	tests := []struct {
		name            string
		replaceFile     func(wantErr error) func(string, string, os.FileMode) error
		wantDiskContent string
	}{
		{
			name: "staged sync",
			replaceFile: func(wantErr error) func(string, string, os.FileMode) error {
				return func(path, content string, mode os.FileMode) error {
					return replaceFileAtomicallyWith(
						path,
						content,
						mode,
						func(*os.File) error { return wantErr },
						func(_, _ string) error { return errors.New("commit ran after staged sync failure") },
					)
				}
			},
			wantDiskContent: "old",
		},
		{
			name: "post-rename flush",
			replaceFile: func(wantErr error) func(string, string, os.FileMode) error {
				return func(path, content string, mode os.FileMode) error {
					return replaceFileAtomicallyWith(
						path,
						content,
						mode,
						func(file *os.File) error { return file.Sync() },
						func(stagedPath, targetPath string) error {
							if err := os.Rename(stagedPath, targetPath); err != nil {
								return err
							}
							return wantErr
						},
					)
				}
			},
			wantDiskContent: "new",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "doc.md")
			if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
				t.Fatalf("write old projection: %v", err)
			}
			cache, err := newTestDocumentCache(t, t.TempDir())
			if err != nil {
				t.Fatalf("new cache: %v", err)
			}
			baseDoc := newDocWithText(t, "old")
			if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
				t.Fatalf("store base doc: %v", err)
			}
			baseRow, err := cache.ensureDocument("doc_1", "doc.md")
			if err != nil {
				t.Fatalf("load base row: %v", err)
			}
			fs := NewWorkspaceFS(root)
			wantErr := errors.New("injected durability failure")
			replaceFile := test.replaceFile(wantErr)
			tracked := &trackedFile{
				DocumentID:   "doc_1",
				DocumentPath: "doc.md",
				Path:         path,
				FS:           fs,
				cache:        cache,
			}
			tracked.setProjectedContent("old")
			if err := tracked.storeProjectedBaseAtSeq("old", baseDoc.EncodeStateAsUpdate(), baseRow.AppliedSeq); err != nil {
				t.Fatalf("store projected base: %v", err)
			}

			writeIfUnchanged := func(path string, expected projectedContentHash, content []byte) error {
				return fs.writeIfUnchangedWith(path, expected, content, replaceFile)
			}
			if _, err := applyProjectedContentWithWrite(
				tracked,
				"new",
				crdtStateFromContent("new"),
				baseRow.AppliedSeq+1,
				writeIfUnchanged,
			); !errors.Is(err, wantErr) {
				t.Fatalf("apply error = %v, want %v", err, wantErr)
			}
			projectedContent, _, projectedKnown, err := tracked.loadProjectedBase()
			if err != nil {
				t.Fatalf("load projected base: %v", err)
			}
			if !projectedKnown || projectedContent != "old" {
				t.Fatalf("projected base advanced after durability failure: known=%v content=%q", projectedKnown, projectedContent)
			}
			if !tracked.matchesProjectedString("old") {
				t.Fatal("in-memory projection advanced after durability failure")
			}
			if tracked.isLocalDirty() {
				t.Fatal("durability failure produced an outbound local edit")
			}
			diskContent, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read disk after durability failure: %v", err)
			}
			if string(diskContent) != test.wantDiskContent {
				t.Fatalf("disk content = %q, want %q", diskContent, test.wantDiskContent)
			}
		})
	}
}

func TestProjectMergedContentDoesNotAdvanceBaseAfterWriteFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatalf("write local content: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	baseRow, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load base row: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBaseAtSeq("base", baseDoc.EncodeStateAsUpdate(), baseRow.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	wantErr := errors.New("injected merge write failure")
	writeCalled := false
	writeIfUnchanged := func(gotPath string, expected projectedContentHash, content []byte) error {
		writeCalled = true
		if gotPath != path || expected != projectedHashString("local") || string(content) != "merged" {
			t.Fatalf("unexpected merge write: path=%q expected=%+v content=%q", gotPath, expected, content)
		}
		return wantErr
	}

	if _, err := projectMergedContentOverLocalDiskWithWrite(
		tracked,
		"local",
		crdtStateFromContent("local"),
		baseRow.AppliedSeq,
		"merged",
		crdtStateFromContent("merged"),
		baseRow.AppliedSeq+1,
		writeIfUnchanged,
	); !errors.Is(err, wantErr) {
		t.Fatalf("project merge error = %v, want %v", err, wantErr)
	}
	if !writeCalled {
		t.Fatal("merge write seam was not called")
	}
	projectedContent, _, projectedKnown, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !projectedKnown || projectedContent != "base" {
		t.Fatalf("projected base advanced after merge write failure: known=%v content=%q", projectedKnown, projectedContent)
	}
	if !tracked.matchesProjectedString("base") {
		t.Fatal("in-memory projection advanced after merge write failure")
	}
	if tracked.isLocalDirty() {
		t.Fatal("merge write failure marked the working copy dirty")
	}
}

func TestApplyProjectedContentDoesNotOverwriteDivergedDiskState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("old plus local edit"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{Path: path})
	tracked.setProjectedContent("old")

	clean, err := applyProjectedContent(tracked, "remote update", nil, 0)
	if err != nil {
		t.Fatalf("apply projected content: %v", err)
	}
	if clean {
		t.Fatal("expected diverged disk content to remain dirty")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(content) != "old plus local edit" {
		t.Fatalf("projection overwrote diverged disk content: %q", content)
	}
	if !tracked.matchesProjectedString("old") {
		t.Fatal("expected projected content hash to roll back after divergence")
	}
}

func TestApplyProjectedContentConflictDoesNotAdvanceProjectedSeq(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("old plus local edit"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "old")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	baseRow, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load base row: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("old")
	if err := tracked.storeProjectedBaseAtSeq("old", baseDoc.EncodeStateAsUpdate(), baseRow.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	remoteUpdate := updateFromBaseDoc(t, baseDoc, "remote update", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}
	doc, _, _, err := cache.loadBaseDoc("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load base doc: %v", err)
	}
	entry := cache.entryFor("doc_1")
	entry.mu.Lock()
	applied, remoteSeq, err := cache.applyPendingRemoteUpdatesLocked(entry, "doc_1", doc)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply remote update: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected one remote update, got %d", applied)
	}
	if remoteSeq <= baseRow.AppliedSeq {
		t.Fatalf("expected remote seq %d after base seq %d", remoteSeq, baseRow.AppliedSeq)
	}

	clean, err := applyProjectedContent(tracked, "remote update", doc.EncodeStateAsUpdate(), remoteSeq)
	if err != nil {
		t.Fatalf("apply projected content: %v", err)
	}
	if clean {
		t.Fatal("expected diverged disk to remain dirty")
	}
	row, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.AppliedSeq != remoteSeq {
		t.Fatalf("applied seq = %d, want remote seq %d", row.AppliedSeq, remoteSeq)
	}
	if row.ProjectedSeq != baseRow.AppliedSeq {
		t.Fatalf("projected seq advanced to %d despite write conflict, want base seq %d", row.ProjectedSeq, baseRow.AppliedSeq)
	}
	if _, ok, err := cache.loadDocumentSnapshotAt("doc_1", remoteSeq); err != nil {
		t.Fatalf("load remote snapshot: %v", err)
	} else if ok {
		t.Fatal("write conflict must not store projected snapshot at the unprojected remote seq")
	}
}

func TestProjectedBaseLivesInWorkspaceSQLiteWithCRDTState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "doc.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cache, err := newTestDocumentCache(t, workspaceSyncDBPath(root))
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	doc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/doc.md", 1, doc); err != nil {
		t.Fatalf("store doc: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "docs/doc.md",
		Path:         path,
		cache:        cache,
	}

	if err := tracked.storeProjectedBase("base", doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	assertSQLiteTableExists(t, cache, "documents")
	content, state, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || content != "base" {
		t.Fatalf("unexpected projected base known=%v content=%q", known, content)
	}
	projectedDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(projectedDoc, state, "projection-check"); err != nil {
		t.Fatalf("apply projected state: %v", err)
	}
	if got := projectedDoc.GetText("content").ToString(); got != "base" {
		t.Fatalf("projected CRDT state materialized to %q, want base", got)
	}
}

func TestReconcileTrackedDocumentNoopsWithoutDirtyOrPendingRemote(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("create unreadable-as-file path: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base")

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send update during no-op reconcile; update bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("no-op reconcile should not touch disk or CRDT state: %v", err)
	}
}

func TestReconcileArchivesUnknownProjectedBaseInsteadOfDiffing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
		t.Fatalf("write projection: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBase("base", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if _, err := cache.db.Exec(`update documents set projection_known = 0 where document_id = ?`, "doc_1"); err != nil {
		t.Fatalf("clear projection state: %v", err)
	}
	tracked.markLocalDirty()

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send update for dirty flag caused by projected write; update bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile unknown projected base: %v", err)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected clean rebuilt projection after archiving unknown local base")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rebuilt projection: %v", err)
	}
	if string(content) != "base" {
		t.Fatalf("expected cache projection to be restored, got %q", content)
	}
	recoveredDir := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName("doc_1"))
	entries, err := os.ReadDir(recoveredDir)
	if err != nil {
		t.Fatalf("read recovered dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one recovered file, got %d", len(entries))
	}
}

func TestWorkspaceDocumentSocketAppendsIncomingUpdateToPendingLog(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "cached local projection")); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}
	runtime := &workspaceRuntime{
		cfg:            Config{AgentID: "daemon_agent"},
		docCache:       cache,
		reconcileQueue: newReconcileQueue(),
	}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)
	runtime.documentSocket.SetDesiredDocuments([]*document{{ID: "doc_1", Path: "doc.md"}, {ID: "doc_2", Path: "other.md"}})
	remoteUpdate := updateFromBaseContent(t, "cached local projection", "cached local projection\nremote\n", "remote")
	if err := runtime.handleDocumentSyncMessage("doc_1", yproto.BuildSyncUpdate(remoteUpdate)); err != nil {
		t.Fatalf("handle incoming update: %v", err)
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one pending remote update, got %d", count)
	}
	otherCount, err := cache.pendingRemoteUpdateCount("doc_2")
	if err != nil {
		t.Fatalf("other pending count: %v", err)
	}
	if otherCount != 0 {
		t.Fatalf("expected no pending updates for doc_2, got %d", otherCount)
	}
	select {
	case <-runtime.reconcileQueue.Wake():
		if got := runtime.reconcileQueue.Drain(); !reflect.DeepEqual(got, []string{"doc_1"}) {
			t.Fatalf("expected dirty mark for doc_1, got %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected dirty mark for document")
	}
}

func TestWorkspaceDocumentSocketIgnoresServerSyncStep1(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "cached local projection")); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}
	runtime := &workspaceRuntime{
		cfg:            Config{AgentID: "daemon_agent"},
		docCache:       cache,
		reconcileQueue: newReconcileQueue(),
	}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)
	runtime.documentSocket.SetDesiredDocuments([]*document{{ID: "doc_1", Path: "doc.md"}})

	if err := runtime.handleDocumentSyncMessage("doc_1", yproto.BuildSyncStep1FromStateVector(nil)); err != nil {
		t.Fatalf("handle server sync step 1: %v", err)
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 0 {
		t.Fatalf("server sync step 1 should not append pending updates, got %d", count)
	}
	select {
	case documentID := <-runtime.reconcileQueue.Wake():
		t.Fatalf("server sync step 1 should not mark dirty, got %q", documentID)
	default:
	}
}

func TestWorkspaceDocumentSocketServerSyncStep1PreservesDurableOutbox(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}
	localUpdate := updateFromBaseContent(t, "base\n", "base\nlocal\n", "local")
	entry, unlock := cache.lockEntry("doc_1")
	err = cache.storeOutboxUpdateLocked(entry, "doc_1", "doc.md", outboxUpdateRecord{
		Update:          localUpdate,
		ObservedContent: "base\nlocal\n",
		ObservedState:   crdtStateFromContent("base\nlocal\n"),
		SourcePath:      filepath.Join(t.TempDir(), "doc.md"),
		ActorID:         "daemon_agent",
		ActorType:       "daemon",
		CreatedAt:       time.Now().UTC(),
	})
	unlock()
	if err != nil {
		t.Fatalf("store durable outbox: %v", err)
	}
	runtime := &workspaceRuntime{
		cfg:            Config{AgentID: "daemon_agent"},
		docCache:       cache,
		reconcileQueue: newReconcileQueue(),
	}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)
	runtime.documentSocket.SetDesiredDocuments([]*document{{ID: "doc_1", Path: "doc.md"}})

	stateVector := crdt.EncodeStateVectorV1(baseDoc)
	if err := runtime.handleDocumentSyncMessage("doc_1", yproto.BuildSyncStep1FromStateVector(stateVector)); err != nil {
		t.Fatalf("handle server sync step 1: %v", err)
	}
	entry, unlock = cache.lockEntry("doc_1")
	outbox, err := cache.loadOutboxUpdateLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if outbox == nil || !bytes.Equal(outbox.Update, localUpdate) {
		t.Fatal("server sync step 1 should not clear durable content outbox")
	}
	select {
	case documentID := <-runtime.reconcileQueue.Wake():
		t.Fatalf("server sync step 1 should not mark dirty, got %q", documentID)
	default:
	}
}

func TestWorkspaceDocumentSocketRunOnceStopsWhenContextCancelsIdleWebsocket(t *testing.T) {
	initialRead := make(chan struct{})
	clientClosed := make(chan struct{})
	handlerErr := make(chan error, 1)
	var initialOnce sync.Once
	var closedOnce sync.Once
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/documents-sync" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			select {
			case handlerErr <- err:
			default:
			}
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				closedOnce.Do(func() { close(clientClosed) })
				return
			}
			documentID, inner, err := yproto.DecodeDocumentMessage(payload)
			if err != nil {
				select {
				case handlerErr <- err:
				default:
				}
				return
			}
			if documentID != "doc_1" {
				select {
				case handlerErr <- fmt.Errorf("unexpected document id %q", documentID):
				default:
				}
				return
			}
			if topLevel, _, err := yproto.DecodeProtocolMessage(inner); err != nil || topLevel != yproto.MessageSync {
				select {
				case handlerErr <- fmt.Errorf("unexpected sync payload top=%d err=%v", topLevel, err):
				default:
				}
				return
			}
			initialOnce.Do(func() { close(initialRead) })
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &workspaceRuntime{
		cfg:            Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		docCache:       nil,
		reconcileQueue: newReconcileQueue(),
	}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)
	runtime.documentSocket.SetDesiredDocuments([]*document{{ID: "doc_1", Path: "doc.md"}})
	done := make(chan error, 1)
	go func() {
		done <- runtime.documentSocket.runOnce(ctx)
	}()

	select {
	case <-initialRead:
	case err := <-handlerErr:
		t.Fatalf("websocket handler error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for document socket to send initial sync message")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("document socket did not exit after context cancellation")
	}
	select {
	case <-clientClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe client websocket close after context cancellation")
	}
}

func TestWorkspaceDocumentSocketRunOnceSendsSyncStep1ForDesiredDocuments(t *testing.T) {
	received := make(chan string, 2)
	handlerErr := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/documents-sync" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			select {
			case handlerErr <- err:
			default:
			}
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				select {
				case handlerErr <- err:
				default:
				}
				return
			}
			documentID, inner, err := yproto.DecodeDocumentMessage(payload)
			if err != nil {
				select {
				case handlerErr <- err:
				default:
				}
				return
			}
			if syncType := decodeInitialSyncType(t, inner); syncType != yproto.SyncStep1 {
				select {
				case handlerErr <- fmt.Errorf("sync type = %d, want SyncStep1", syncType):
				default:
				}
				return
			}
			received <- documentID
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &workspaceRuntime{
		cfg:            Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		reconcileQueue: newReconcileQueue(),
	}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)
	runtime.documentSocket.SetDesiredDocuments([]*document{
		{ID: "doc_b", Path: "b.md"},
		{ID: "doc_a", Path: "a.md"},
	})
	done := make(chan error, 1)
	go func() {
		done <- runtime.documentSocket.runOnce(ctx)
	}()

	got := []string{}
	for len(got) < 2 {
		select {
		case documentID := <-received:
			got = append(got, documentID)
		case err := <-handlerErr:
			t.Fatalf("websocket handler error: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for sync steps, got %#v", got)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("document socket did not stop after cancel")
	}
	if !reflect.DeepEqual(got, []string{"doc_a", "doc_b"}) {
		t.Fatalf("sync steps sent for %#v, want sorted doc_a/doc_b", got)
	}
}

func TestWorkspaceDocumentSocketInitialSyncDoesNotAdvertiseMissingCacheState(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	runtime := &workspaceRuntime{cfg: Config{AgentID: "daemon_agent"}, docCache: cache}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)

	stateVector := decodeInitialSyncStateVector(t, runtime.documentSocket.initialSyncStep(&document{ID: "doc_1", Path: "doc.md"}))
	if len(stateVector) != 0 {
		t.Fatalf("fresh daemon must not advertise missing local cache state, got %v", stateVector)
	}
}

func TestWorkspaceDocumentSocketInitialSyncUsesOnlyVerifiedLocalCacheState(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	cachedDoc := newDocWithText(t, "cached content")
	if err := cache.storeDoc("doc_1", "doc.md", 1, cachedDoc); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}
	runtime := &workspaceRuntime{cfg: Config{AgentID: "daemon_agent"}, docCache: cache}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)

	stateVector := decodeInitialSyncStateVector(t, runtime.documentSocket.initialSyncStep(&document{ID: "doc_1", Path: "doc.md"}))
	if !bytes.Equal(stateVector, crdt.EncodeStateVectorV1(cachedDoc)) {
		t.Fatalf("expected daemon to advertise cached local state vector, got %v", stateVector)
	}
}

func TestDocumentCacheLocalStateVectorRequiresAppliedCRDTRows(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if _, err := cache.ensureDocument("doc_1", "doc.md"); err != nil {
		t.Fatalf("ensure document: %v", err)
	}

	stateVector := cache.localStateVector("doc_1")
	if len(stateVector) != 0 {
		t.Fatalf("metadata-only cache must not advertise local state, got %v", stateVector)
	}
}

func decodeInitialSyncStateVector(t *testing.T, payload []byte) []byte {
	t.Helper()
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol message: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("expected sync message, got %d", messageType)
	}
	syncType, stateVector, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode sync message: %v", err)
	}
	if syncType != yproto.SyncStep1 {
		t.Fatalf("expected sync step 1, got %d", syncType)
	}
	return stateVector
}

func decodeInitialSyncType(t *testing.T, payload []byte) uint64 {
	t.Helper()
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol message: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("expected sync message, got %d", messageType)
	}
	syncType, _, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode sync message: %v", err)
	}
	return syncType
}

func TestReconcileTrackedDocumentMergesLocalEditWithPendingRemoteUpdate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, baseDoc, "base\nremote\n", "remote")
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	if err := crdt.ApplyUpdateV1(serverDoc, remoteUpdate, "server-remote"); err != nil {
		t.Fatalf("apply server remote: %v", err)
	}
	var sentLocalUpdate []byte
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		if documentID != "doc_1" {
			t.Fatalf("unexpected document id: %s", documentID)
		}
		sentLocalUpdate = append([]byte(nil), update...)
		if err := crdt.ApplyUpdateV1(serverDoc, update, "server-local"); err != nil {
			t.Fatalf("apply local update to server doc: %v", err)
		}
		return documentUpdateTestResponse{Accepted: true, Applied: true, UpdateID: 2}, http.StatusOK
	})

	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local edit: %v", err)
	}
	if err := markTrackedLocalDirty(tracked, path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}

	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if len(sentLocalUpdate) == 0 {
		t.Fatal("expected local CRDT update to be sent")
	}
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile pending remote: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projected merge: %v", err)
	}
	for _, part := range []string{"base\n", "local\n", "remote\n"} {
		if !strings.Contains(string(content), part) {
			t.Fatalf("expected merged file content to contain %q, got %q", part, content)
		}
	}
	if !tracked.matchesProjectedString(string(content)) {
		t.Fatal("expected projected content hash to advance to merged content")
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to clear after successful reconcile")
	}
}

func TestReconcileTrackedDocumentDefersLocalUpdateWhenProjectedBaseMissingCRDTState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("already synced\nnext\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("already synced\n")
	tracked.markLocalDirty()

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send a full-file update when cached CRDT state is missing; update bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to remain until a matching CRDT base is available")
	}
}

func TestReconcileTrackedDocumentArchivesLocalUpdateWithoutProjectedBase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("large existing local file\nnew append\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "")); err != nil {
		t.Fatalf("store empty remote base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("large existing local file\n")
	tracked.markLocalDirty()

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send local update before a projected base is established; update bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to clear after archiving unknown base and projecting backend")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projected file: %v", err)
	}
	if string(content) != "" {
		t.Fatalf("expected empty backend projection, got %q", content)
	}
	recoveredDir := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName("doc_1"))
	entries, err := os.ReadDir(recoveredDir)
	if err != nil {
		t.Fatalf("read recovered dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one recovered file, got %d", len(entries))
	}
}

func TestMaterializeExistingLocalFileUsesCachedBaseAsProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "append.txt")
	baseContent := "1\n2\n"
	localContent := "1\n2\n3\n"
	if err := os.WriteFile(path, []byte(localContent), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, baseContent)
	if err := cache.storeDoc("doc_append", "append.txt", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	if err := (&trackedFile{
		DocumentID:    "doc_append",
		DocumentPath:  "append.txt",
		Path:          path,
		WorkspaceRoot: root,
		cache:         cache,
	}).storeProjectedBase(baseContent, baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	tracked, err := materializeTrackedFile(context.Background(), cache, &document{
		ID:   "doc_append",
		Path: "append.txt",
	}, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != baseContent {
		t.Fatalf("expected cached CRDT base %q, known=%v got %q", baseContent, known, projectedBase)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected existing local content ahead of the cached base to be marked dirty")
	}
	if tracked.matchesProjectedString(localContent) {
		t.Fatal("existing local append was incorrectly treated as a clean projection")
	}

	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	sentUpdates := 0
	service := newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, &sentUpdates)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if sentUpdates != 1 {
		t.Fatalf("expected one local update, got %d", sentUpdates)
	}
	if got := serverDoc.GetText("content").ToString(); got != localContent {
		t.Fatalf("server content mismatch: got %q want %q", got, localContent)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to clear")
	}
}

func TestReconcileArchivesUnknownLocalFileAfterPendingRemoteEstablishesBase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "append.txt")
	baseContent := "base\n"
	localContent := "base\nlocal\n"
	if err := os.WriteFile(path, []byte(localContent), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("")
	tracked.markLocalDirty()
	remoteUpdate := updateFromBaseContent(t, "", baseContent, "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_append", "append.txt", remoteUpdate); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}

	sentUpdates := 0
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		sentUpdates++
		return documentUpdateTestResponse{Accepted: true, Applied: true}, http.StatusOK
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if sentUpdates != 0 {
		t.Fatalf("expected no local update from unknown base, got %d", sentUpdates)
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != baseContent {
		t.Fatalf("expected projected base to be established from remote update, known=%v base=%q", known, projectedBase)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to clear after projecting backend base")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projected file: %v", err)
	}
	if string(content) != baseContent {
		t.Fatalf("expected backend base projection, got %q", content)
	}
	recoveredDir := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName("doc_append"))
	entries, err := os.ReadDir(recoveredDir)
	if err != nil {
		t.Fatalf("read recovered dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one recovered file, got %d", len(entries))
	}
	recovered, err := os.ReadFile(filepath.Join(recoveredDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read recovered file: %v", err)
	}
	if string(recovered) != localContent {
		t.Fatalf("expected unknown local file to be archived, got %q", recovered)
	}
}

func TestSingleWriterAppendPressureReconcilesIncrementalBatches(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "append.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append file: %v", err)
	}
	defer file.Close()

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "")
	if err := cache.storeDoc("doc_append", "append.txt", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("")
	if err := tracked.storeProjectedBase(""); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	serverDoc := newDocWithText(t, "")
	sentUpdates := 0
	maxUpdateBytes := 0
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		sentUpdates++
		if len(update) > maxUpdateBytes {
			maxUpdateBytes = len(update)
		}
		if err := crdt.ApplyUpdateV1(serverDoc, update, "server"); err != nil {
			t.Fatalf("apply server update: %v", err)
		}
		return documentUpdateTestResponse{Accepted: true, Applied: true}, http.StatusOK
	})

	var expected strings.Builder
	const totalLines = 30000
	const batchSize = 1000
	started := time.Now()
	for i := 1; i <= totalLines; i++ {
		line := fmt.Sprintf("%d\n", i)
		expected.WriteString(line)
		if err := writeFullString(file, line); err != nil {
			t.Fatalf("append line %d: %v", i, err)
		}
		if err := markTrackedLocalDirty(tracked, path); err != nil {
			t.Fatalf("mark line %d dirty: %v", i, err)
		}
		if i%batchSize != 0 {
			continue
		}
		if err := service.reconcileTrackedDocument(context.Background(), "doc_append", []*trackedFile{tracked}); err != nil {
			t.Fatalf("reconcile after line %d: %v", i, err)
		}
		if got := serverDoc.GetText("content").ToString(); got != expected.String() {
			t.Fatalf("server content mismatch after line %d: got %d bytes want %d bytes", i, len(got), expected.Len())
		}
		if tracked.isLocalDirty() {
			t.Fatalf("dirty flag remained after line %d", i)
		}
	}
	if sentUpdates != totalLines/batchSize {
		t.Fatalf("expected one CRDT update per reconciliation batch, got %d", sentUpdates)
	}
	if maxUpdateBytes > 64*1024 {
		t.Fatalf("incremental append update was too large: max=%d bytes", maxUpdateBytes)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("append pressure test took too long: %s", elapsed)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read append file: %v", err)
	}
	if string(content) != expected.String() {
		t.Fatalf("local file mismatch: got %d bytes want %d bytes", len(content), expected.Len())
	}
}

func TestReconcileCapturesSequentialLocalAppendsAcrossCycles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "append.txt")
	if err := os.WriteFile(path, []byte("1\n2\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "1\n")
	if err := cache.storeDoc("doc_append", "append.txt", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("1\n")
	if err := tracked.storeProjectedBase("1\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	service := newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, nil)

	if err := service.reconcileTrackedDocument(context.Background(), "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := serverDoc.GetText("content").ToString(); got != "1\n2\n" {
		t.Fatalf("server after first reconcile got %q", got)
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != "1\n2\n" {
		t.Fatalf("expected sent local content as projected base, known=%v base=%q", known, projectedBase)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to clear after first append")
	}
	if err := os.WriteFile(path, []byte("1\n2\n3\n"), 0o644); err != nil {
		t.Fatalf("write second local file: %v", err)
	}
	tracked.markLocalDirty()

	if err := service.reconcileTrackedDocument(context.Background(), "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := serverDoc.GetText("content").ToString(); got != "1\n2\n3\n" {
		t.Fatalf("server after second reconcile got %q", got)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to clear after reconciling concurrent append")
	}
}

func TestReconcileRebasesAppendFromStaleWorkspaceBase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "append.txt")
	if err := os.WriteFile(path, []byte("1\n3\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	projectedDoc := newDocWithText(t, "1\n")
	if err := cache.storeDoc("doc_append", "append.txt", 1, projectedDoc); err != nil {
		t.Fatalf("store projected doc: %v", err)
	}
	projectedRow, err := cache.ensureDocument("doc_append", "append.txt")
	if err != nil {
		t.Fatalf("load projected row: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, projectedDoc, "1\n2\n", "remote")
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, projectedDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server projected base: %v", err)
	}
	if err := crdt.ApplyUpdateV1(serverDoc, remoteUpdate, "remote"); err != nil {
		t.Fatalf("apply server remote: %v", err)
	}
	if _, err := cache.appendPendingRemoteUpdate("doc_append", "append.txt", remoteUpdate); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}
	entry, unlock := cache.lockEntry("doc_append")
	baseDoc, _, _, err := cache.loadBaseDocLocked(entry, "doc_append", "append.txt")
	if err != nil {
		unlock()
		t.Fatalf("load base doc: %v", err)
	}
	if _, err := applyPendingRemoteUpdatesLocked(cache, entry, "doc_append", baseDoc); err != nil {
		unlock()
		t.Fatalf("apply pending remote: %v", err)
	}
	unlock()
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("1\n")
	if err := tracked.storeProjectedBaseAtSeq("1\n", projectedDoc.EncodeStateAsUpdate(), projectedRow.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	sentUpdates := 0
	service := newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, &sentUpdates)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if sentUpdates != 1 {
		t.Fatalf("expected one rebased local update, got %d", sentUpdates)
	}
	if got := serverDoc.GetText("content").ToString(); !containsAll(got, "1\n", "2\n", "3\n") {
		t.Fatalf("expected CRDT state to contain local and remote lines, got %q", got)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if got := string(content); !containsAll(got, "1\n", "2\n", "3\n") {
		t.Fatalf("expected local file to receive merged projection, got %q", content)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to clear")
	}
}

func TestReconcileSendsLocalEditBeforeApplyingPendingRemoteUpdate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, baseDoc, "base\nremote\n", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append remote: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		return documentUpdateTestResponse{Accepted: true, Applied: true, UpdateID: 2}, http.StatusOK
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to clear after accepted outgoing update")
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected accepted local update to leave pending remote updates for the next pass, got %d", count)
	}

	cachedDoc, _, _, err := cache.loadBaseDoc("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load cached doc after local finalize: %v", err)
	}
	if got := cachedDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("expected accepted local update to be visible before pending incoming folds, got %q", got)
	}

	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	count, err = cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count after second reconcile: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected second reconcile to drain pending remote updates, got %d", count)
	}

	cachedDoc, _, _, err = cache.loadBaseDoc("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load cached doc: %v", err)
	}
	if got := cachedDoc.GetText("content").ToString(); !containsAll(got, "base\n", "remote\n", "local\n") {
		t.Fatalf("expected local edit and remote update to both survive, got %q", got)
	}
}

func TestDocumentCacheRejectsInvalidRemoteUpdate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "base")); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBase("base"); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Fatal("expected invalid remote update to be rejected before it enters sqlite log")
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid remote update must not be durable, got %d pending updates", count)
	}
}

func TestReconcileTrackedDocumentAppliesPendingRemoteFromSQLiteLog(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	cacheRoot := t.TempDir()
	cache, err := newTestDocumentCache(t, cacheRoot)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "base")); err != nil {
		t.Fatalf("store base: %v", err)
	}
	update := updateFromBaseContent(t, "base", "base\nremote", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", update); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}
	cache, err = newTestDocumentCache(t, cacheRoot)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}

	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBase("base"); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send local update while applying pending remote")
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("expected pending remote to apply without failing reconcile: %v", err)
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected pending remote to be applied, got %d pending updates", count)
	}
}

func TestWorkspaceRuntimeReconcileTrackedDocumentsAppliesPendingRemoteUpdate(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, baseDoc, "base remote", "remote")
	for i := 0; i < 10; i++ {
		appended, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate)
		if err != nil {
			t.Fatalf("append pending remote %d: %v", i, err)
		}
		if i == 0 && !appended {
			t.Fatal("expected first remote update append")
		}
		if i > 0 && appended {
			t.Fatalf("expected duplicate remote update %d to be deduped", i)
		}
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one pending remote update, got %d", count)
	}

	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
		t.Fatalf("write projection: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBase("base"); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	runtime := &workspaceRuntime{
		docCache: cache,
		client:   http.DefaultClient,
		replica: &workspaceReplica{
			projectedByID:   map[string]*trackedFile{"doc_1": tracked},
			projectedByPath: map[string]*trackedFile{path: tracked},
		},
	}
	if err := runtime.reconcileTrackedDocuments(context.Background()); err != nil {
		t.Fatalf("reconcile tracked documents: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projection: %v", err)
	}
	if string(content) != "base remote" {
		t.Fatalf("unexpected projection at %s: %q", path, content)
	}
}

func TestDocumentCacheDedupesConcurrentRemoteDeliveries(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	update := updateFromBaseContent(t, "", "concurrent", "remote")
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	appended := atomic.Int32{}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", update)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				appended.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append pending remote: %v", err)
		}
	}
	if got := appended.Load(); got != 1 {
		t.Fatalf("expected exactly one concurrent append to win, got %d", got)
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one framed pending update, got %d", count)
	}
}

func TestReconcileKeepsIncomingPendingWhenOutgoingSendFails(t *testing.T) {
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, baseDoc, "base\nremote\n", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append remote: %v", err)
	}
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		return documentUpdateTestResponse{}, http.StatusServiceUnavailable
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err == nil {
		t.Fatal("expected local outgoing update to remain queued when backend send fails")
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected disconnected local dirty workspace to remain dirty")
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected incoming remote update to stay pending until local outgoing succeeds, got %d pending updates", count)
	}
	materialized, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "doc.md"})
	if err != nil {
		t.Fatalf("materialize cache: %v", err)
	}
	if got := materialized.Doc.GetText("content").ToString(); got != "base\n" {
		t.Fatalf("shared base should not advance while local outgoing is still queued, got %q", got)
	}
	entry, unlock := cache.lockEntry("doc_1")
	outbox, err := cache.loadOutboxUpdateLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if outbox == nil || len(outbox.Update) == 0 {
		t.Fatal("expected local outgoing update to remain in durable outbox")
	}

	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	if err := crdt.ApplyUpdateV1(serverDoc, remoteUpdate, "server-remote"); err != nil {
		t.Fatalf("apply server remote: %v", err)
	}
	service = newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, nil)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := serverDoc.GetText("content").ToString(); !containsAll(got, "base\n", "remote\n", "local\n") {
		t.Fatalf("expected retried local and incoming remote content to converge, got %q", got)
	}
}

func TestMaterializeTrackedFileDefersProjectionToReconcile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "remote.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := &document{
		ID:   "doc_1",
		Path: "docs/remote.md",
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "docs/remote.md", 1, newDocWithText(t, "remote content")); err != nil {
		t.Fatalf("store cached document: %v", err)
	}

	tracked, err := materializeTrackedFile(context.Background(), cache, doc, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialization must not project files directly, stat err=%v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("missing projected base should schedule reconciliation")
	}
	if tracked.DocumentID != "doc_1" || tracked.Path != path {
		t.Fatalf("unexpected tracked file: %#v", tracked)
	}
}

func TestMaterializeTrackedFileWithoutCacheCreatesEmptyProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "new.md")
	tracked, err := materializeTrackedFile(context.Background(), nil, &document{
		ID:   "doc_new",
		Path: "docs/new.md",
	}, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created projection: %v", err)
	}
	if len(content) != 0 || !tracked.matchesProjectedString("") {
		t.Fatalf("expected empty projection, content=%q", content)
	}
}

func TestMaterializeTrackedFileWithoutCachePreservesExistingLocalContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "local.md")
	local := []byte("local content\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if err := os.WriteFile(path, local, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	tracked, err := materializeTrackedFile(context.Background(), nil, &document{
		ID:   "doc_local",
		Path: "docs/local.md",
	}, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved projection: %v", err)
	}
	if string(content) != string(local) || !tracked.matchesProjectedString(string(local)) {
		t.Fatalf("local content was not preserved, got %q want %q", content, local)
	}
}

func TestReconcileProjectsMissingFileFromSharedCache(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "remote.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := &document{
		ID:   "doc_1",
		Path: "docs/remote.md",
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "docs/remote.md", 1, newDocWithText(t, "remote content")); err != nil {
		t.Fatalf("store cached document: %v", err)
	}

	tracked, err := materializeTrackedFile(context.Background(), cache, doc, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send update while initializing missing projection")
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projected file: %v", err)
	}
	if string(content) != "remote content" {
		t.Fatalf("unexpected cached content projection: %q", content)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected initialized projection to be clean")
	}
	if !tracked.matchesProjectedString("remote content") {
		t.Fatal("expected cached content hash to be recorded")
	}
	if _, _, known, err := tracked.loadProjectedBase(); err != nil || !known {
		t.Fatalf("expected projected base to be stored, known=%v err=%v", known, err)
	}
}

func TestReconcileArchivesUnknownWorkingCopyBeforeProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "recover.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("local draft"), 0o644); err != nil {
		t.Fatalf("write local draft: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "docs/recover.md", 1, newDocWithText(t, "server content")); err != nil {
		t.Fatalf("store cached document: %v", err)
	}

	tracked, err := materializeTrackedFile(context.Background(), cache, &document{
		ID:   "doc_1",
		Path: "docs/recover.md",
	}, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not upload an unknown working copy; bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rebuilt projection: %v", err)
	}
	if string(content) != "server content" {
		t.Fatalf("expected backend projection, got %q", content)
	}
	recoveredDir := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName("doc_1"))
	entries, err := os.ReadDir(recoveredDir)
	if err != nil {
		t.Fatalf("read recovered dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one recovered file, got %d", len(entries))
	}
	recovered, err := os.ReadFile(filepath.Join(recoveredDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read recovered file: %v", err)
	}
	if string(recovered) != "local draft" {
		t.Fatalf("unexpected recovered content: %q", recovered)
	}
	if _, _, known, err := tracked.loadProjectedBase(); err != nil || !known {
		t.Fatalf("expected rebuilt projection base, known=%v err=%v", known, err)
	}
}

func TestReconcileArchivesUnknownWorkingCopyWithoutCacheContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("local edit"), 0o644); err != nil {
		t.Fatalf("write local edit: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.markLocalDirty()
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		t.Fatalf("must not send local update without projected base; bytes=%d", len(update))
		return documentUpdateTestResponse{}, http.StatusInternalServerError
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile unknown local edit: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown working copy should be removed after archive, stat err=%v", err)
	}
	recoveredDir := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName("doc_1"))
	entries, err := os.ReadDir(recoveredDir)
	if err != nil {
		t.Fatalf("read recovered dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one recovered file, got %d", len(entries))
	}
	if !tracked.isLocalDirty() {
		t.Fatal("document should remain dirty until backend content is available")
	}
}

func TestReconcileUsesSQLiteProjectedBase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local edit: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	sends := 0
	service := newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, &sends)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if sends != 1 {
		t.Fatalf("expected one outgoing update, got %d", sends)
	}
	if got := serverDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("unexpected server content: %q", got)
	}
}

func TestHandleLocalChangeIgnoresProjectedWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	doc := newDocWithText(t, "hello")
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	})
	tracked.setProjectedContent("hello")

	replica := &workspaceReplica{
		rootDir:         root,
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	updateDocText(t, doc, "hello world", "remote")
	if _, err := applyProjectedContent(tracked, "hello world", nil, 0); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello world" {
		t.Fatalf("expected daemon-projected write to be ignored, got %q", got)
	}
}

func TestWorkspaceReplicaHandleLocalChangeIgnoresProjectedWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	doc := newDocWithText(t, "hello")
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	})
	tracked.setProjectedContent("hello")

	replica := &workspaceReplica{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	updateDocText(t, doc, "hello world", "remote")
	if _, err := applyProjectedContent(tracked, "hello world", nil, 0); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello world" {
		t.Fatalf("expected replica-projected write to be ignored, got %q", got)
	}
}

func TestHandleLocalChangeWhileDisconnectedOnlyMarksDirty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("hello brave"), 0o644); err != nil {
		t.Fatalf("write changed file: %v", err)
	}

	doc := newDocWithText(t, "hello")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
	tracked.setProjectedContent("hello")

	replica := &workspaceReplica{
		rootDir:         root,
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("first local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello" {
		t.Fatalf("local change should not mutate CRDT before reconciliation, got %q", got)
	}
	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("second local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello" {
		t.Fatalf("local change should still not mutate CRDT before reconciliation, got %q", got)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag while disconnected")
	}
	if !tracked.matchesProjectedString("hello") {
		t.Fatal("expected projected content hash to remain at the last projected base")
	}
}

func TestWorkspaceReplicaHandleLocalChangeWhileDisconnectedOnlyMarksDirty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("hello brave"), 0o644); err != nil {
		t.Fatalf("write changed file: %v", err)
	}

	doc := newDocWithText(t, "hello")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
	tracked.setProjectedContent("hello")

	replica := &workspaceReplica{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("first local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello" {
		t.Fatalf("local change should not mutate replica CRDT before reconciliation, got %q", got)
	}
	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("second local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello" {
		t.Fatalf("replica local change should still not mutate CRDT before reconciliation, got %q", got)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected replica local dirty flag while disconnected")
	}
	if !tracked.matchesProjectedString("hello") {
		t.Fatal("expected replica projected content hash to remain at the last projected base")
	}
}

func TestReconcileLocalWorkspacePrefersMoveForSameContent(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "docs", "old.md")
	newPath := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("same"), 0o644); err != nil {
		t.Fatalf("write moved file: %v", err)
	}

	doc := newDocWithText(t, "same")
	tracked := &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/old.md",
		Path:          oldPath,
		WorkspaceRoot: root,
		Doc:           doc,
	}
	tracked.setProjectedContent("same")

	replica := &workspaceReplica{
		rootDir:         root,
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{oldPath: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}
	tracked.Owner = replica
	tracked.FS = replica.fs

	if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if tracked.Path != newPath {
		t.Fatalf("expected tracked path to move to %q, got %q", newPath, tracked.Path)
	}
	if tracked.WorkspaceRoot != root {
		t.Fatalf("expected tracked workspace root to remain %q, got %q", root, tracked.WorkspaceRoot)
	}
	if !tracked.isLocalMoved() || !tracked.isLocalDirty() {
		t.Fatal("expected local clean move to be queued for central reconciliation")
	}
}

func TestReconcileLocalWorkspaceSkipsMissingTrackedFileDuringProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "projecting.md")

	doc := newDocWithText(t, "projecting")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
	tracked.setProjectedContent("projecting")
	tracked.beginProjection()
	defer tracked.endProjection()

	replica := &workspaceReplica{
		rootDir:         root,
		projectedByPath: map[string]*trackedFile{path: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}

	if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if replica.projectedByID["doc_1"] != tracked || replica.projectedByPath[path] != tracked {
		t.Fatalf("expected projection-tracked file to remain registered")
	}
}

func TestReconcileLocalWorkspaceReobservesTrackedFileOmittedByScan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		symlink bool
	}{
		{name: "regular file"},
		{name: "symlinked file", symlink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "docs", "present.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir docs: %v", err)
			}
			contentPath := path
			if tc.symlink {
				contentPath = filepath.Join(root, ".present-target.md")
			}
			if err := os.WriteFile(contentPath, []byte("present"), 0o644); err != nil {
				t.Fatalf("write tracked content: %v", err)
			}
			if tc.symlink {
				if err := os.Symlink(contentPath, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}

			tracked := &trackedFile{
				DocumentID:    "doc_1",
				DocumentPath:  "docs/present.md",
				Path:          path,
				WorkspaceRoot: root,
				Doc:           newDocWithText(t, "present"),
			}
			tracked.setProjectedContent("present")

			replica := &workspaceReplica{
				rootDir:         root,
				fs:              NewWorkspaceFS(root),
				projectedByPath: map[string]*trackedFile{path: tracked},
				projectedByID:   map[string]*trackedFile{"doc_1": tracked},
			}
			tracked.Owner = replica
			tracked.FS = replica.fs

			previousScan := scanWorkspaceFilesForReconcile
			scanWorkspaceFilesForReconcile = func(gotRoot string) (map[string]string, error) {
				if gotRoot != root {
					t.Fatalf("scan root = %q, want %q", gotRoot, root)
				}
				return map[string]string{}, nil
			}
			defer func() { scanWorkspaceFilesForReconcile = previousScan }()

			if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
				t.Fatalf("reconcile workspace: %v", err)
			}
			if tracked.isLocalDeleted() || tracked.isLocalDirty() {
				t.Fatal("scan omission promoted a still-present tracked file to a local deletion")
			}

			if err := os.WriteFile(contentPath, []byte("changed"), 0o644); err != nil {
				t.Fatalf("write local change: %v", err)
			}
			if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
				t.Fatalf("reconcile changed workspace: %v", err)
			}
			if tracked.isLocalDeleted() || !tracked.isLocalDirty() {
				t.Fatal("scan omission hid changed bytes or promoted them to a local deletion")
			}
		})
	}
}

func TestWorkspaceReplicaReconcileSkipsMissingTrackedFileDuringProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "projecting.md")

	doc := newDocWithText(t, "projecting")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
	tracked.setProjectedContent("projecting")
	tracked.beginProjection()
	defer tracked.endProjection()

	replica := &workspaceReplica{
		rootDir:         root,
		projectedByPath: map[string]*trackedFile{path: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}

	if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile replica workspace: %v", err)
	}
	if replica.projectedByID["doc_1"] != tracked || replica.projectedByPath[path] != tracked {
		t.Fatalf("expected replica projection-tracked file to remain registered")
	}
}

func TestReconcileLocalWorkspaceDeletesMissingTrackedDocument(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "docs", "gone.md")

	doc := newDocWithText(t, "gone")
	tracked := &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/gone.md",
		Path:          oldPath,
		WorkspaceRoot: root,
		Doc:           doc,
	}
	tracked.setProjectedContent("gone")

	replica := &workspaceReplica{
		rootDir:         root,
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{oldPath: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}
	tracked.Owner = replica
	tracked.FS = replica.fs

	if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if !tracked.isLocalDeleted() || !tracked.isLocalDirty() {
		t.Fatal("expected missing tracked file to be queued as local deletion")
	}
}

func TestReconcileLocalWorkspaceDeletesTrackedFileReplacedByDirectory(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "docs", "gone.md")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatalf("mkdir replacement directory: %v", err)
	}

	tracked := &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/gone.md",
		Path:          oldPath,
		WorkspaceRoot: root,
		Doc:           newDocWithText(t, "gone"),
	}
	tracked.setProjectedContent("gone")

	replica := &workspaceReplica{
		rootDir:         root,
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{oldPath: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}
	tracked.Owner = replica
	tracked.FS = replica.fs

	if err := replica.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if !tracked.isLocalDeleted() || !tracked.isLocalDirty() {
		t.Fatal("expected tracked file replaced by a directory to be queued as a local deletion")
	}
}

func TestCentralReconcilePublishesQueuedCleanMove(t *testing.T) {
	root := t.TempDir()
	newPath := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("same"), 0o644); err != nil {
		t.Fatalf("write moved file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "same")
	if err := cache.storeDoc("doc_1", "docs/old.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/old.md",
		Path:          newPath,
		WorkspaceRoot: root,
		cache:         cache,
	})
	tracked.setProjectedContent("same")
	if err := tracked.storeProjectedBase("same", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalMoved()

	service := &workspaceRuntime{
		cfg:            Config{BackendURL: "http://backend.test", AgentID: "daemon_agent"},
		client:         http.DefaultClient,
		docCache:       cache,
		rootDocumentID: "doc_root_test",
	}
	var rootUpdates int
	service.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		if documentID != service.rootDocumentID {
			t.Fatalf("unexpected document update: %s", documentID)
		}
		rootUpdates++
		return cache.clearOutboxUpdates(documentID)
	}

	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile move: %v", err)
	}
	if rootUpdates != 1 {
		t.Fatalf("expected one root websocket update, got %d", rootUpdates)
	}
	rootDoc, _, _, err := cache.loadBaseDoc(service.rootDocumentID, rootDocumentPath)
	if err != nil {
		t.Fatalf("load root doc: %v", err)
	}
	defer rootDoc.Close()
	entries, err := decodeRootEntries(rootDoc)
	if err != nil {
		t.Fatalf("decode root entries: %v", err)
	}
	entry := entries[rootEntryIDForDocument("doc_1")]
	if entry.desiredPath() != "docs/new.md" || entry.Deleted {
		t.Fatalf("expected root entry move to docs/new.md, got %#v", entry)
	}
	if tracked.isLocalMoved() || tracked.isLocalDirty() {
		t.Fatal("expected clean move markers to clear after root update")
	}
	if tracked.DocumentPath != "docs/new.md" {
		t.Fatalf("expected tracked document path to advance, got %q", tracked.DocumentPath)
	}
}

func TestCentralReconcileRemoteDeleteArchivesDirtyWorkingCopy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "doc.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("dirty local"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "docs/doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	replica := &workspaceReplica{
		rootDir:         root,
		fs:              NewWorkspaceFS(root),
		projectedByID:   map[string]*trackedFile{},
		projectedByPath: map[string]*trackedFile{},
	}
	tracked := &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/doc.md",
		Path:          path,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
		cache:         cache,
	}
	replica.projectedByID["doc_1"] = tracked
	replica.projectedByPath[path] = tracked
	tracked.setProjectedContent("base")
	if err := tracked.storeProjectedBase("base", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markRemoteDeleted()

	service := &workspaceRuntime{docCache: cache, client: http.DefaultClient}
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile remote delete: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected dirty working copy to move out of canonical path, stat err=%v", err)
	}
	recovered, err := filepath.Glob(filepath.Join(root, ".notty", "recovered", "*", "*"))
	if err != nil {
		t.Fatalf("glob recovered: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("expected one archived dirty file, got %v", recovered)
	}
	content, err := os.ReadFile(recovered[0])
	if err != nil {
		t.Fatalf("read archived file: %v", err)
	}
	if string(content) != "dirty local" {
		t.Fatalf("archived content mismatch: %q", content)
	}
	if len(replica.projectedByID) != 0 || len(replica.projectedByPath) != 0 {
		t.Fatal("expected removed document to be untracked")
	}
}

func TestLocalCreateCreatesEmptyDocumentAndKeepsLocalBytesDirty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "local bytes\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write local create: %v", err)
	}
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/documents" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode create: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"doc_created","updateId":1}`))
	}))
	defer server.Close()
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	service := &workspaceRuntime{
		cfg:      Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		client:   server.Client(),
		docCache: cache,
		replica:  &workspaceReplica{rootDir: root, fs: NewWorkspaceFS(root)},
	}

	created, err := service.createDocumentFromLocalCandidate(context.Background(), localCreateCandidate{Root: root, Path: path, ActorID: "daemon_agent", ActorType: "daemon"}, "docs/new.md")
	if err != nil {
		t.Fatalf("create from local candidate: %v", err)
	}
	if created == nil || created.ID != "doc_created" {
		t.Fatalf("unexpected created doc: %#v", created)
	}
	if seen["documentId"] == "" || seen["clientOperationId"] == "" {
		t.Fatalf("expected stable document/op IDs in create payload: %#v", seen)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if string(after) != content {
		t.Fatalf("local create must not rewrite the working file, got %q", after)
	}
	tracked := &trackedFile{DocumentID: "doc_created", DocumentPath: "docs/new.md", Path: path, WorkspaceRoot: root, cache: cache}
	base, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || base != "" {
		t.Fatalf("expected empty projected base so local bytes reconcile as first update, known=%v base=%q", known, base)
	}
}

func TestLocalCreateIntentRetriesWithStableDocumentAndOperationID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("local bytes\n"), 0o644); err != nil {
		t.Fatalf("write local create: %v", err)
	}

	rootID := "doc_root_retry"
	var createPayloads []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{RootDocumentID: rootID})
		case r.Method == http.MethodPost && r.URL.Path == "/api/documents":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			if _, err := uuid.Parse(payload["documentId"]); err != nil {
				http.Error(w, "document id must be a bare uuid", http.StatusBadRequest)
				return
			}
			createPayloads = append(createPayloads, payload)
			if len(createPayloads) == 1 {
				http.Error(w, "temporary backend failure", http.StatusInternalServerError)
				return
			}
			writeJSONResponse(w, http.StatusCreated, &document{ID: payload["documentId"], UpdateID: 1})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	rootDoc := crdt.New()
	if err := cache.storeDoc(rootID, rootDocumentPath, 1, rootDoc); err != nil {
		t.Fatalf("store root doc: %v", err)
	}
	rootDoc.Close()
	runtime := &workspaceRuntime{
		cfg:          Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		client:       server.Client(),
		docCache:     cache,
		localCreates: newLocalCreateQueue(),
		replica: &workspaceReplica{
			rootDir:         root,
			fs:              NewWorkspaceFS(root),
			projectedByPath: map[string]*trackedFile{},
			projectedByID:   map[string]*trackedFile{},
		},
	}
	runtime.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		return cache.clearOutboxUpdates(documentID)
	}

	runtime.localCreates.Mark(localCreateCandidate{Root: root, Path: path, ActorID: "daemon_agent", ActorType: "daemon"})
	if err := runtime.processLocalCreates(context.Background()); err == nil {
		t.Fatal("first local create pass should surface backend failure")
	}
	pending, err := cache.loadPendingLocalNamespaceIntents()
	if err != nil {
		t.Fatalf("load pending intents: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending intents after failure = %#v", pending)
	}

	if err := runtime.processLocalCreates(context.Background()); err != nil {
		t.Fatalf("retry local create: %v", err)
	}
	if len(createPayloads) != 2 {
		t.Fatalf("create attempts = %#v", createPayloads)
	}
	if createPayloads[0]["documentId"] == "" || createPayloads[0]["clientOperationId"] == "" {
		t.Fatalf("first create payload missing stable IDs: %#v", createPayloads[0])
	}
	if _, err := uuid.Parse(createPayloads[0]["documentId"]); err != nil {
		t.Fatalf("local create document ID should be a bare UUID, got %q: %v", createPayloads[0]["documentId"], err)
	}
	if createPayloads[0]["documentId"] != createPayloads[1]["documentId"] ||
		createPayloads[0]["clientOperationId"] != createPayloads[1]["clientOperationId"] {
		t.Fatalf("retry used different identity: %#v", createPayloads)
	}
	pending, err = cache.loadPendingLocalNamespaceIntents()
	if err != nil {
		t.Fatalf("reload pending intents: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending intents after success = %#v", pending)
	}
}

func TestLocalCreateIntentKeepsClaimedPathAfterRootUpsertRestart(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "docs", "local.md")
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	localBytes := []byte("local bytes\n")
	if err := os.WriteFile(localPath, localBytes, 0o644); err != nil {
		t.Fatalf("write local create: %v", err)
	}

	intent := newLocalCreateIntent("docs/local.md", "daemon_agent", "daemon", sha256Hex(localBytes), time.Now().UnixNano())
	rootID := "doc_root_restart"
	remoteID := "doc_remote"
	var createPayloads []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{RootDocumentID: rootID})
		case r.Method == http.MethodPost && r.URL.Path == "/api/documents":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createPayloads = append(createPayloads, payload)
			if payload["documentId"] != intent.DocumentID || payload["clientOperationId"] != intent.ClientOperationID {
				t.Fatalf("unexpected create payload: %#v want doc=%s op=%s", payload, intent.DocumentID, intent.ClientOperationID)
			}
			writeJSONResponse(w, http.StatusOK, &document{ID: intent.DocumentID, UpdateID: 1})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	rootDoc := crdt.New()
	if _, err := UpsertRootFile(rootDoc, remoteID, "docs/local.md", rootMutationActor{}); err != nil {
		t.Fatalf("upsert remote root entry: %v", err)
	}
	if _, err := UpsertRootFile(rootDoc, intent.DocumentID, intent.WorkspaceRelativePath, rootMutationActor{}); err != nil {
		t.Fatalf("upsert local root entry: %v", err)
	}
	if err := cache.storeDoc(rootID, rootDocumentPath, 1, rootDoc); err != nil {
		t.Fatalf("store root doc: %v", err)
	}
	rootDoc.Close()
	localDoc := crdt.New()
	if err := cache.storeDoc(intent.DocumentID, intent.WorkspaceRelativePath, 1, localDoc); err != nil {
		t.Fatalf("store local content doc: %v", err)
	}
	localDoc.Close()
	remoteDoc := crdt.New()
	if err := cache.storeDoc(remoteID, "docs/local.md", 1, remoteDoc); err != nil {
		t.Fatalf("store remote content doc: %v", err)
	}
	remoteDoc.Close()
	if err := cache.storeLocalNamespaceIntent(intent); err != nil {
		t.Fatalf("store pending local intent: %v", err)
	}

	runtime := &workspaceRuntime{
		cfg:          Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		client:       server.Client(),
		docCache:     cache,
		localCreates: newLocalCreateQueue(),
		replica: &workspaceReplica{
			rootDir:         root,
			projectedByPath: map[string]*trackedFile{},
			projectedByID:   map[string]*trackedFile{},
			fs:              NewWorkspaceFS(root),
			markDirty: func(string) {
			},
		},
	}
	runtime.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		return nil
	}

	if err := runtime.processLocalCreates(context.Background()); err != nil {
		t.Fatalf("process restart local create intent: %v", err)
	}
	if len(createPayloads) != 1 {
		t.Fatalf("create payloads = %#v", createPayloads)
	}
	pending, err := cache.loadPendingLocalNamespaceIntents()
	if err != nil {
		t.Fatalf("load pending intents: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending intents after resolution = %#v", pending)
	}
	projection, err := cache.loadRootProjectionEntries(rootID)
	if err != nil {
		t.Fatalf("load root projection: %v", err)
	}
	localProjection := projection[rootEntryIDForDocument(intent.DocumentID)]
	if localProjection.MaterializedPath != "docs/local.md" {
		t.Fatalf("local materialized path = %q, want docs/local.md; projection=%#v", localProjection.MaterializedPath, projection)
	}
	remoteProjection := projection[rootEntryIDForDocument(remoteID)]
	if remoteProjection.MaterializedPath != "docs/local (doc_remote).md" {
		t.Fatalf("remote materialized path = %q, want docs/local (doc_remote).md; projection=%#v", remoteProjection.MaterializedPath, projection)
	}
	runtime.replica.mu.Lock()
	localTracked := runtime.replica.projectedByID[intent.DocumentID]
	remoteTracked := runtime.replica.projectedByID[remoteID]
	_, localPathTracked := runtime.replica.projectedByPath[localPath]
	runtime.replica.mu.Unlock()
	if localTracked == nil || localTracked.Path != localPath || !localPathTracked {
		t.Fatalf("local tracked path = %#v localPathTracked=%v, want %s", localTracked, localPathTracked, localPath)
	}
	remoteConflictPath := filepath.Join(root, "docs", "local (doc_remote).md")
	if remoteTracked == nil || remoteTracked.Path != remoteConflictPath {
		t.Fatalf("remote tracked path = %#v, want %s", remoteTracked, remoteConflictPath)
	}
	if got, err := os.ReadFile(localPath); err != nil || string(got) != string(localBytes) {
		t.Fatalf("local bytes changed: content=%q err=%v", string(got), err)
	}
}

func TestOutgoingOutboxKeepsLocalUpdateWhenBackendSendFails(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache, ActorID: "agent_1", ActorType: "agent"})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()

	sendAttempts := 0
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		sendAttempts++
		if got := r.URL.Query().Get("actor"); got != "agent_1" {
			t.Fatalf("expected actor query agent_1, got %q", got)
		}
		if got := r.URL.Query().Get("actor_type"); got != "agent" {
			t.Fatalf("expected actor_type query agent, got %q", got)
		}
		return documentUpdateTestResponse{}, http.StatusServiceUnavailable
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err == nil {
		t.Fatal("expected first reconcile to fail while backend is disconnected")
	}
	if sendAttempts != 1 {
		t.Fatalf("expected one send attempt, got %d", sendAttempts)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("dirty flag must remain until backend convergence")
	}
	entry, unlock := cache.lockEntry("doc_1")
	outbox, err := cache.loadOutboxUpdateLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if outbox == nil || len(outbox.Update) == 0 {
		t.Fatal("expected durable outgoing outbox after failed send")
	}
	if outbox.ActorID != "agent_1" || outbox.ActorType != "agent" {
		t.Fatalf("expected actor identity to be stored in outbox, got %s/%s", outbox.ActorID, outbox.ActorType)
	}

	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	service = newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, &sendAttempts)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if got := serverDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("server content mismatch after retry: %q", got)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to clear after converged retry")
	}
	entry, unlock = cache.lockEntry("doc_1")
	outbox, err = cache.loadOutboxUpdateLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("reload outbox: %v", err)
	}
	if outbox != nil {
		t.Fatal("expected outbox to clear after backend convergence")
	}
}

func TestOutgoingOutboxStoresAllDirtyWorkspacesWithActorAttribution(t *testing.T) {
	primaryRoot := t.TempDir()
	agentRoot := t.TempDir()
	primaryPath := filepath.Join(primaryRoot, "doc.md")
	agentPath := filepath.Join(agentRoot, "doc.md")
	if err := os.WriteFile(primaryPath, []byte("base\nprimary\n"), 0o644); err != nil {
		t.Fatalf("write primary: %v", err)
	}
	if err := os.WriteFile(agentPath, []byte("base\nagent\n"), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	primaryTracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: primaryPath, WorkspaceRoot: primaryRoot, cache: cache, ActorID: "daemon_agent", ActorType: "daemon"})
	agentTracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: agentPath, WorkspaceRoot: agentRoot, cache: cache, ActorID: "agent_1", ActorType: "agent"})
	for _, tracked := range []*trackedFile{primaryTracked, agentTracked} {
		tracked.setProjectedContent("base\n")
		if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
			t.Fatalf("store projected base: %v", err)
		}
		tracked.markLocalDirty()
	}
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		return documentUpdateTestResponse{}, http.StatusServiceUnavailable
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{primaryTracked, agentTracked}); err == nil {
		t.Fatal("expected backend failure to keep multi-workspace outbox")
	}
	entry, unlock := cache.lockEntry("doc_1")
	records, err := cache.loadOutboxUpdatesLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load outbox records: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected two outbox records, got %d", len(records))
	}
	actors := map[string]string{}
	for _, record := range records {
		actors[record.ActorID] = record.ActorType
	}
	if actors["daemon_agent"] != "daemon" || actors["agent_1"] != "agent" {
		t.Fatalf("unexpected actors in outbox: %#v", actors)
	}
}

func TestOutgoingOutboxClearsOnHTTPAcceptance(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	sends := 0
	service := newApplyingDocumentUpdateWebsocketTestRuntime(t, cache, serverDoc, &sends)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if sends != 1 {
		t.Fatalf("expected one websocket update, got %d", sends)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to clear after backend acceptance")
	}
	entry, unlock := cache.lockEntry("doc_1")
	outbox, err := cache.loadOutboxUpdateLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if outbox != nil {
		t.Fatal("expected outbox to clear after backend acceptance")
	}
}

func TestOutgoingOutboxFinalizeConflictStoresAcceptedLocalProjectedSeq(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1)
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	baseRow, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load base row: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBaseAtSeq("base\n", baseDoc.EncodeStateAsUpdate(), baseRow.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()

	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		if err := crdt.ApplyUpdateV1(serverDoc, update, "server-local"); err != nil {
			t.Fatalf("apply local update to server doc: %v", err)
		}
		if err := os.WriteFile(path, []byte("base\nlocal\nsecond edit\n"), 0o644); err != nil {
			t.Fatalf("write concurrent local edit: %v", err)
		}
		return documentUpdateTestResponse{Accepted: true, Applied: true, UpdateID: 2}, http.StatusOK
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("concurrent local edit should keep file dirty")
	}
	if got := serverDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("server content = %q, want accepted local content", got)
	}
	row, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.AppliedSeq <= baseRow.AppliedSeq {
		t.Fatalf("expected accepted local update to advance applied seq beyond %d, got %d", baseRow.AppliedSeq, row.AppliedSeq)
	}
	if row.ProjectedSeq != row.AppliedSeq {
		t.Fatalf("projected seq = %d, want accepted local seq %d", row.ProjectedSeq, row.AppliedSeq)
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != "base\nlocal\n" {
		t.Fatalf("projected base = known %v content %q, want accepted local content", known, projectedBase)
	}
	assertDocumentSnapshotContent(t, cache, "doc_1", row.AppliedSeq, "base\nlocal\n")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if string(content) != "base\nlocal\nsecond edit\n" {
		t.Fatalf("reconcile overwrote concurrent edit: %q", content)
	}
}

func TestOutgoingOutboxFinalizeDuplicateDoesNotRegressProjectedSeq(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1)
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	localUpdate := updateFromBaseDoc(t, baseDoc, "base\nlocal\n", "local")
	localDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(localDoc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base to local doc: %v", err)
	}
	if err := crdt.ApplyUpdateV1(localDoc, localUpdate, "local"); err != nil {
		t.Fatalf("apply local update: %v", err)
	}
	record := &outboxUpdateRecord{
		ID:              "dup_local",
		Update:          localUpdate,
		ObservedContent: "base\nlocal\n",
		ObservedState:   localDoc.EncodeStateAsUpdate(),
		SourcePath:      path,
		ActorID:         "agent_1",
		ActorType:       "agent",
	}
	entry := cache.entryFor("doc_1")
	entry.mu.Lock()
	localSeq, err := cache.applyOutboxUpdateLocked(entry, "doc_1", "doc.md", record)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply first local update: %v", err)
	}

	currentDoc, _, _, err := cache.loadBaseDoc("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load current doc: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, currentDoc, "base\nlocal\nremote\n", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append remote update: %v", err)
	}
	entry.mu.Lock()
	_, remoteSeq, err := cache.applyPendingRemoteUpdatesLocked(entry, "doc_1", currentDoc)
	entry.mu.Unlock()
	if err != nil {
		t.Fatalf("apply remote update: %v", err)
	}
	if remoteSeq <= localSeq {
		t.Fatalf("remote seq %d must be after local seq %d", remoteSeq, localSeq)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base\nlocal\n")
	if err := tracked.storeProjectedBaseAtSeq("base\nlocal\n", record.ObservedState, localSeq); err != nil {
		t.Fatalf("store local projected base: %v", err)
	}

	entry, unlock := cache.lockEntry("doc_1")
	err = finalizeSentOutbox(cache, entry, "doc_1", "doc.md", []*trackedFile{tracked}, record)
	unlock()
	if err != nil {
		t.Fatalf("finalize duplicate outbox: %v", err)
	}
	row, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.AppliedSeq != remoteSeq {
		t.Fatalf("applied seq = %d, want remote seq %d", row.AppliedSeq, remoteSeq)
	}
	if row.ProjectedSeq != remoteSeq {
		t.Fatalf("projected seq regressed to %d, want remote seq %d", row.ProjectedSeq, remoteSeq)
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != "base\nlocal\nremote\n" {
		t.Fatalf("projected base = known %v content %q, want merged remote content", known, projectedBase)
	}
	assertDocumentSnapshotContent(t, cache, "doc_1", remoteSeq, "base\nlocal\nremote\n")
}

func TestFinalizeSentOutboxIgnoresSnapshotFailureAndClearsOutbox(t *testing.T) {
	withDocumentSnapshotEveryUpdates(t, 1)
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	baseRow, err := cache.ensureDocument("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load base row: %v", err)
	}
	localUpdate := updateFromBaseDoc(t, baseDoc, "base\nlocal\n", "local")
	localDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(localDoc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base to local doc: %v", err)
	}
	if err := crdt.ApplyUpdateV1(localDoc, localUpdate, "local"); err != nil {
		t.Fatalf("apply local update: %v", err)
	}
	record := outboxUpdateRecord{
		ID:              "snapshot_fail",
		Update:          localUpdate,
		ObservedContent: "base\nlocal\n",
		ObservedState:   localDoc.EncodeStateAsUpdate(),
		SourcePath:      path,
		ActorID:         "agent_1",
		ActorType:       "agent",
	}
	entry := cache.entryFor("doc_1")
	entry.mu.Lock()
	if err := cache.storeOutboxUpdateLocked(entry, "doc_1", "doc.md", record); err != nil {
		entry.mu.Unlock()
		t.Fatalf("store outbox: %v", err)
	}
	entry.mu.Unlock()
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBaseAtSeq("base\n", baseDoc.EncodeStateAsUpdate(), baseRow.AppliedSeq); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	withDocumentSnapshotStoreHook(t, func(documentID string, seq int64) error {
		return errors.New("snapshot insert failed")
	})

	entry, unlock := cache.lockEntry("doc_1")
	err = finalizeSentOutbox(cache, entry, "doc_1", "doc.md", []*trackedFile{tracked}, &record)
	unlock()
	if err != nil {
		t.Fatalf("finalize outbox with snapshot failure: %v", err)
	}
	entry, unlock = cache.lockEntry("doc_1")
	outbox, err := cache.loadOutboxUpdateLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load outbox after finalize: %v", err)
	}
	if outbox != nil {
		t.Fatal("snapshot failure must not prevent durable outbox clear")
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != "base\nlocal\n" {
		t.Fatalf("projected base = known %v content %q, want local content", known, projectedBase)
	}
}

func TestOutgoingOutboxSurvivesCacheReopenAndResendsIdempotently(t *testing.T) {
	root := t.TempDir()
	cacheRoot := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newTestDocumentCache(t, cacheRoot)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache})
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	service := newDocumentUpdateWebsocketTestRuntime(t, cache, func(documentID string, update []byte, r *http.Request) (documentUpdateTestResponse, int) {
		return documentUpdateTestResponse{}, http.StatusServiceUnavailable
	})
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err == nil {
		t.Fatal("expected first reconcile to keep outbox after backend failure")
	}

	reopened, err := newTestDocumentCache(t, cacheRoot)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	restarted := newTestTrackedFile(t, &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: reopened})
	restarted.setProjectedContent("base\n")
	restarted.markLocalDirty()
	duplicateSends := 0
	service = newApplyingDocumentUpdateWebsocketTestRuntime(t, reopened, serverDoc, &duplicateSends)
	if err := service.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{restarted}); err != nil {
		t.Fatalf("restarted reconcile: %v", err)
	}
	if duplicateSends != 1 {
		t.Fatalf("expected one durable outbox resend after restart, got %d", duplicateSends)
	}
	if got := serverDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("duplicate resend changed server content: %q", got)
	}
	if restarted.isLocalDirty() {
		t.Fatal("expected restarted daemon to clear dirty flag after convergence")
	}
}

func TestParseAgentInboxChangedEventTargetsOneAgent(t *testing.T) {
	payload := json.RawMessage(`{"agentId":"agent_1","eventId":"aevt_1","box":"for_me","notificationType":"thread.mentioned"}`)
	change, ok := parseAgentInboxChangedEvent(workspaceEventEnvelope{Type: "agent.inbox.changed", Data: payload})
	if !ok || change.AgentID != "agent_1" || change.EventID != "aevt_1" {
		t.Fatalf("failed to parse inbox change: ok=%v change=%#v", ok, change)
	}
	if _, ok := parseAgentInboxChangedEvent(workspaceEventEnvelope{Type: "thread.message.created", Data: payload}); ok {
		t.Fatal("non-inbox workspace events must not wake agent workers")
	}
	if _, ok := parseAgentInboxChangedEvent(workspaceEventEnvelope{Type: "agent.inbox.changed", Data: json.RawMessage(`{"eventId":"aevt_1"}`)}); ok {
		t.Fatal("inbox change without agent id must not wake agent workers")
	}
}

func newDocWithText(t *testing.T, content string) *crdt.Doc {
	t.Helper()
	doc := crdt.New()
	updateDocText(t, doc, content, "test")
	return doc
}

func (t *trackedFile) storeProjectedBase(content string, states ...[]byte) error {
	if t == nil || t.DocumentID == "" || t.cache == nil {
		return nil
	}
	var state []byte
	if len(states) > 0 {
		state = states[0]
	}
	projectedSeq, err := t.cache.documentAppliedSeq(t.DocumentID)
	if err != nil {
		return err
	}
	return t.storeProjectedBaseAtSeq(content, state, projectedSeq)
}

func updateDocText(t *testing.T, doc *crdt.Doc, content string, origin any) {
	t.Helper()
	text := doc.GetText("content")
	current := text.ToString()
	currentLength := text.Len()
	doc.Transact(func(txn *crdt.Transaction) {
		if current != "" {
			text.Delete(txn, 0, currentLength)
		}
		if content != "" {
			text.Insert(txn, 0, content, nil)
		}
	}, origin)
}

func updateFromBaseContent(t *testing.T, baseContent, nextContent string, origin any) []byte {
	t.Helper()
	doc := newDocWithText(t, baseContent)
	return updateFromBaseDoc(t, doc, nextContent, origin)
}

func updateFromBaseDoc(t *testing.T, baseDoc *crdt.Doc, nextContent string, origin any) []byte {
	t.Helper()
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, baseDoc.EncodeStateAsUpdate(), "base"); err != nil {
		t.Fatalf("apply base state: %v", err)
	}
	var update []byte
	unsubscribe := doc.OnUpdate(func(next []byte, observedOrigin any) {
		if observedOrigin == origin {
			update = append([]byte(nil), next...)
		}
	})
	baseContent := doc.GetText("content").ToString()
	updateDocText(t, doc, nextContent, origin)
	unsubscribe()
	if len(update) == 0 && baseContent != nextContent {
		t.Fatal("expected CRDT update from base content")
	}
	return update
}

func encodeDocState(doc *crdt.Doc) string {
	return base64.StdEncoding.EncodeToString(doc.EncodeStateAsUpdate())
}

func encodeStateForContent(t *testing.T, content string) string {
	t.Helper()
	return encodeDocState(newDocWithText(t, content))
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition was not met before timeout")
	}
}

func TestSnapshotAppliesWorkspaceWithoutRESTFetch(t *testing.T) {
	var restFetches atomic.Int32
	upgrader := websocket.Upgrader{}
	snapshotData := workspaceResponse{
		RootDocumentID: "doc_root",
		Agents: []*agent{{
			ID:     "agent_snap",
			Handle: "snap-agent",
			Kind:   "codex",
		}},
	}
	snapshotJSON, _ := json.Marshal(snapshotData)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(workspaceEventEnvelope{
				Type: "workspace.snapshot",
				Data: snapshotJSON,
			})
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			restFetches.Add(1)
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()
	stopWorker := startTestRefreshWorker(t, service, ctx)

	done := make(chan error, 1)
	go func() { done <- service.runWorkspaceEventStream(ctx) }()

	waitUntil(t, func() bool {
		service.mu.Lock()
		ws := service.latestWorkspace
		service.mu.Unlock()
		return ws != nil && len(ws.Agents) == 1 && ws.Agents[0].ID == "agent_snap"
	})
	cancel()
	<-done
	stopWorker()

	if got := restFetches.Load(); got != 0 {
		t.Fatalf("snapshot should not trigger REST fetch, got %d", got)
	}
}

type fetchConcurrencyTracker struct {
	inner         http.RoundTripper
	inflight      atomic.Int32
	maxConcurrent atomic.Int32
}

func (ct *fetchConcurrencyTracker) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/workspace") {
		cur := ct.inflight.Add(1)
		defer ct.inflight.Add(-1)
		for {
			old := ct.maxConcurrent.Load()
			if cur <= old || ct.maxConcurrent.CompareAndSwap(old, cur) {
				break
			}
		}
	}
	return ct.inner.RoundTrip(req)
}

func TestEventBurstCoalescesWithSingleRefreshOwner(t *testing.T) {
	var restFetches atomic.Int32
	sentinelJSON, _ := json.Marshal(workspaceResponse{Agents: []*agent{{ID: "sentinel", Kind: "codex"}}})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for i := 0; i < 20; i++ {
				_ = conn.WriteJSON(workspaceEventEnvelope{Type: "agent.updated"})
			}
			_ = conn.WriteJSON(workspaceEventEnvelope{Type: "workspace.snapshot", Data: sentinelJSON})
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			restFetches.Add(1)
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{Agents: []*agent{{ID: "sentinel", Kind: "codex"}}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tracker := &fetchConcurrencyTracker{inner: server.Client().Transport}

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        &http.Client{Transport: tracker},
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()
	stopWorker := startTestRefreshWorker(t, service, ctx)

	done := make(chan error, 1)
	go func() { done <- service.runWorkspaceEventStream(ctx) }()

	waitUntil(t, func() bool {
		service.mu.Lock()
		ws := service.latestWorkspace
		service.mu.Unlock()
		return ws != nil && len(ws.Agents) == 1 && ws.Agents[0].ID == "sentinel"
	})
	cancel()
	<-done
	stopWorker()

	if got := restFetches.Load(); got > 5 {
		t.Fatalf("20-event burst should coalesce into far fewer refreshes, got %d", got)
	}
	if got := tracker.maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent client-side REST fetches = %d, want exactly 1 (single owner)", got)
	}
}

func TestPeriodicTickerTriggersRefresh(t *testing.T) {
	var restFetches atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
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
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			restFetches.Add(1)
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case strings.HasSuffix(r.URL.Path, "/daemon/status"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ticks := make(chan time.Time, 1)
	cfg := Config{
		BackendURL:         server.URL,
		WorkspaceID:        "workspace:test",
		DaemonToken:        "daemon_token",
		DataDir:            t.TempDir(),
		WorkspaceDir:       t.TempDir(),
		AgentWorkspaceRoot: t.TempDir(),
		AgentID:            "daemon_agent",
		AgentToolBaseURL:   "http://127.0.0.1:0",
	}
	service := &Service{
		cfg:           cfg,
		client:        server.Client(),
		daemonStatus:  newDaemonStatusReporter(cfg, server.Client()),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		ready:         make(chan struct{}),
		refreshTicks:  ticks,
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	<-service.ready
	restFetches.Store(0)
	ticks <- time.Now()

	waitUntil(t, func() bool { return restFetches.Load() >= 1 })
	cancel()
	<-done

	if got := restFetches.Load(); got < 1 {
		t.Fatalf("periodic tick should trigger REST refresh, got %d fetches", got)
	}
}

func TestStreamReturnsPromptlyWhenRESTIsBlocked(t *testing.T) {
	restBlocked := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(workspaceEventEnvelope{Type: "agent.updated"})
			<-restBlocked
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			close(restBlocked)
			<-r.Context().Done()
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()
	stopWorker := startTestRefreshWorker(t, service, ctx)

	done := make(chan error, 1)
	go func() { done <- service.runWorkspaceEventStream(ctx) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWorkspaceEventStream did not return after websocket close while REST was blocked")
	}
	cancel()
	stopWorker()
}

func TestStreamReturnsPromptlyWhenApplyIsBlocked(t *testing.T) {
	snapshotJSON, _ := json.Marshal(workspaceResponse{
		Agents: []*agent{{ID: "agent_blocked", Kind: "codex"}},
	})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(workspaceEventEnvelope{
				Type: "workspace.snapshot",
				Data: snapshotJSON,
			})
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	service.refreshMu.Lock()
	stopWorker := startTestRefreshWorker(t, service, ctx)

	done := make(chan error, 1)
	go func() { done <- service.runWorkspaceEventStream(ctx) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWorkspaceEventStream did not return after websocket close while apply was blocked on refreshMu")
	}
	service.refreshMu.Unlock()
	cancel()
	stopWorker()
}

func TestSnapshotSupersedesInFlightREST(t *testing.T) {
	var restCount atomic.Int32
	restReached := make(chan struct{}, 1)
	snapshotData := workspaceResponse{Agents: []*agent{{ID: "agent_snap", Kind: "codex"}}}
	snapshotJSON, _ := json.Marshal(snapshotData)
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(workspaceEventEnvelope{Type: "agent.updated"})
			<-restReached
			_ = conn.WriteJSON(workspaceEventEnvelope{
				Type: "workspace.snapshot",
				Data: snapshotJSON,
			})
			_ = conn.WriteJSON(workspaceEventEnvelope{Type: "agent.updated"})
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			n := restCount.Add(1)
			if n == 1 {
				select {
				case restReached <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()
	stopWorker := startTestRefreshWorker(t, service, ctx)

	done := make(chan error, 1)
	go func() { done <- service.runWorkspaceEventStream(ctx) }()

	waitUntil(t, func() bool {
		service.mu.Lock()
		ws := service.latestWorkspace
		service.mu.Unlock()
		return ws != nil && len(ws.Agents) == 1 && ws.Agents[0].ID == "agent_snap"
	})
	waitUntil(t, func() bool { return restCount.Load() >= 2 })
	cancel()
	<-done
	stopWorker()
}

func TestStaleRESTSkippedBySnapshotEpoch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{Agents: []*agent{{ID: "stale", Kind: "codex"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentRuntimes()
	defer service.closeAgentWorkers()

	snapshotJSON, _ := json.Marshal(workspaceResponse{Agents: []*agent{{ID: "snap", Kind: "codex"}}})
	service.publishSnapshot(snapshotJSON)

	if err := service.refresh(context.Background(), 0); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	service.mu.Lock()
	ws := service.latestWorkspace
	service.mu.Unlock()
	if ws != nil {
		t.Fatal("stale REST result should have been skipped by epoch fence, but latestWorkspace was set")
	}
}

type blockingCloseTransport struct {
	inner        http.RoundTripper
	closeBlocked chan struct{}
	closeRelease chan struct{}
}

func (t *blockingCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/workspace") {
		return resp, err
	}
	resp.Body = &blockingCloseBody{ReadCloser: resp.Body, blocked: t.closeBlocked, release: t.closeRelease}
	return resp, nil
}

type blockingCloseBody struct {
	io.ReadCloser
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingCloseBody) Close() error {
	b.once.Do(func() { close(b.blocked) })
	<-b.release
	return b.ReadCloser.Close()
}

func TestTerminalAuthOutranksSnapshotCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			w.WriteHeader(http.StatusGone)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	closeBlocked := make(chan struct{})
	closeRelease := make(chan struct{})
	transport := &blockingCloseTransport{
		inner:        server.Client().Transport,
		closeBlocked: closeBlocked,
		closeRelease: closeRelease,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        &http.Client{Transport: transport},
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentRuntimes()
	defer service.closeAgentWorkers()

	drained := make(chan error, 1)
	service.refreshNeeded = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.runRefreshCoordinator(ctx, drained)
	}()
	stopCoordinator := sync.OnceFunc(func() { close(service.refreshNeeded); <-done })
	defer stopCoordinator()

	service.signalRefresh()
	<-closeBlocked

	snapshotJSON, _ := json.Marshal(workspaceResponse{})
	service.publishSnapshot(snapshotJSON)
	close(closeRelease)

	select {
	case err := <-drained:
		var statusErr *backendStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusGone {
			t.Fatalf("drained should carry 410, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal 410 was suppressed; drained channel never received the error")
	}
	stopCoordinator()
}

func TestAdmissionEpochFenceSkipsStaleREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			<-r.Context().Done()
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentRuntimes()
	defer service.closeAgentWorkers()

	snap1JSON, _ := json.Marshal(workspaceResponse{RootDocumentID: "snap1"})
	snap2JSON, _ := json.Marshal(workspaceResponse{RootDocumentID: "snap2"})

	service.refreshMu.Lock()

	stopWorker := startTestRefreshWorker(t, service, ctx)
	defer func() { cancel(); stopWorker() }()

	service.publishSnapshot(snap1JSON)
	service.signalRefresh()

	waitUntil(t, func() bool { return service.pendingSnapshot.Load() == nil })

	service.publishSnapshot(snap2JSON)

	service.refreshMu.Unlock()

	waitUntil(t, func() bool {
		service.mu.Lock()
		ws := service.latestWorkspace
		service.mu.Unlock()
		return ws != nil && ws.RootDocumentID == "snap2"
	})
}

func TestPostSnapshotRefreshIntentPreserved(t *testing.T) {
	var restFetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			restFetches.Add(1)
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentRuntimes()
	defer service.closeAgentWorkers()

	snapAJSON, _ := json.Marshal(workspaceResponse{RootDocumentID: "A"})
	snapBJSON, _ := json.Marshal(workspaceResponse{RootDocumentID: "B"})

	service.refreshMu.Lock()

	stopWorker := startTestRefreshWorker(t, service, ctx)
	defer func() { cancel(); stopWorker() }()

	service.publishSnapshot(snapAJSON)
	service.signalRefresh()

	waitUntil(t, func() bool { return service.pendingSnapshot.Load() == nil })

	service.publishSnapshot(snapBJSON)
	service.signalRefresh()

	service.refreshMu.Unlock()

	waitUntil(t, func() bool { return restFetches.Load() > 0 })
}

func TestProductionCoordinatorSingleOwner(t *testing.T) {
	var restCount atomic.Int32
	periodicBlocked := make(chan struct{}, 1)
	releaseREST := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
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
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			n := restCount.Add(1)
			if n > 1 {
				select {
				case periodicBlocked <- struct{}{}:
				default:
				}
				select {
				case <-releaseREST:
				case <-r.Context().Done():
					return
				}
			}
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case strings.HasSuffix(r.URL.Path, "/daemon/status"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ticks := make(chan time.Time, 1)
	tracker := &fetchConcurrencyTracker{inner: server.Client().Transport}
	cfg := Config{
		BackendURL:         server.URL,
		WorkspaceID:        "workspace:test",
		DaemonToken:        "daemon_token",
		DataDir:            t.TempDir(),
		WorkspaceDir:       t.TempDir(),
		AgentWorkspaceRoot: t.TempDir(),
		AgentID:            "daemon_agent",
		AgentToolBaseURL:   "http://127.0.0.1:0",
	}
	service := &Service{
		cfg:           cfg,
		client:        &http.Client{Transport: tracker},
		daemonStatus:  newDaemonStatusReporter(cfg, server.Client()),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		ready:         make(chan struct{}),
		refreshTicks:  ticks,
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	<-service.ready
	tracker.maxConcurrent.Store(0)

	ticks <- time.Now()
	<-periodicBlocked
	ticks <- time.Now()
	time.Sleep(200 * time.Millisecond)

	close(releaseREST)
	cancel()
	<-done

	if got := tracker.maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent client-side REST fetches = %d, want exactly 1 (single owner)", got)
	}
}

func TestReconnectSnapshotConvergence(t *testing.T) {
	var restFetches atomic.Int32
	var connCount atomic.Int32
	upgrader := websocket.Upgrader{}

	snapshotA := workspaceResponse{Agents: []*agent{{ID: "agent_A", Kind: "codex"}}}
	snapshotB := workspaceResponse{Agents: []*agent{{ID: "agent_B", Kind: "codex"}}}
	snapshotAJSON, _ := json.Marshal(snapshotA)
	snapshotBJSON, _ := json.Marshal(snapshotB)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/workspaces/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			n := connCount.Add(1)
			if n == 1 {
				_ = conn.WriteJSON(workspaceEventEnvelope{Type: "workspace.snapshot", Data: snapshotAJSON})
				time.Sleep(50 * time.Millisecond)
				_ = conn.Close()
				return
			}
			_ = conn.WriteJSON(workspaceEventEnvelope{Type: "workspace.snapshot", Data: snapshotBJSON})
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			restFetches.Add(1)
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			WorkspaceID:        "workspace:test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        server.Client(),
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()
	stopWorker := startTestRefreshWorker(t, service, ctx)

	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			err := service.runWorkspaceEventStream(ctx)
			if ctx.Err() != nil {
				done <- nil
				return
			}
			if err != nil {
				continue
			}
		}
		done <- nil
	}()

	waitUntil(t, func() bool {
		service.mu.Lock()
		ws := service.latestWorkspace
		service.mu.Unlock()
		return ws != nil && len(ws.Agents) == 1 && ws.Agents[0].ID == "agent_B"
	})

	cancel()
	<-done
	stopWorker()

	if got := restFetches.Load(); got != 0 {
		t.Fatalf("reconnect snapshots should not trigger REST fetch, got %d", got)
	}
}

func TestSnapshotAndRefreshAreSerialized(t *testing.T) {
	snapshotJSON, _ := json.Marshal(workspaceResponse{
		Agents: []*agent{{ID: "agent_snap", Kind: "codex"}},
	})

	service := &Service{
		cfg: Config{
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        http.DefaultClient,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
		agentBaseCtx:  context.Background(),
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentWorkers()
	defer service.closeAgentRuntimes()

	service.refreshMu.Lock()

	applied := make(chan struct{})
	go func() {
		service.applySnapshot(context.Background(), snapshotJSON)
		close(applied)
	}()

	select {
	case <-applied:
		t.Fatal("applySnapshot must block while refreshMu is held")
	case <-time.After(200 * time.Millisecond):
	}

	service.mu.Lock()
	ws := service.latestWorkspace
	service.mu.Unlock()
	if ws != nil {
		t.Fatal("snapshot applied before refreshMu was released")
	}

	service.refreshMu.Unlock()

	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("applySnapshot did not complete after refreshMu was released")
	}

	service.mu.Lock()
	ws = service.latestWorkspace
	service.mu.Unlock()
	if ws == nil || len(ws.Agents) != 1 || ws.Agents[0].ID != "agent_snap" {
		t.Fatalf("expected snapshot agent after lock release, got %+v", ws)
	}
}

func TestCoordinatorWorkerAndRuntimeSurviveFetchCtxCancellation(t *testing.T) {
	var refreshCount atomic.Int32
	var inboxPolls atomic.Int32
	fetchCtxCh := make(chan context.Context, 8)
	withAgent := workspaceResponse{
		Agents: []*agent{{
			ID:     "agent_1",
			Handle: "agent-one",
			Name:   "Agent One",
			Kind:   "codex",
		}},
	}
	withoutAgents := workspaceResponse{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workspace"):
			n := refreshCount.Add(1)
			var ws workspaceResponse
			switch n {
			case 1:
				ws = withAgent
			case 2:
				ws = withoutAgents
			default:
				ws = withAgent
			}
			writeJSONResponse(w, http.StatusOK, &ws)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/"):
			inboxPolls.Add(1)
			writeJSONResponse(w, http.StatusOK, toolInboxResponse{Items: []*agentEvent{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	serverTransport := server.Client().Transport
	service := &Service{
		cfg: Config{
			BackendURL:         server.URL,
			DataDir:            t.TempDir(),
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/workspace") {
				fetchCtxCh <- r.Context()
			}
			return serverTransport.RoundTrip(r)
		})},
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	defer service.closePrimaryRuntime()
	defer service.closeAgentRuntimes()
	defer service.closeAgentWorkers()
	stopWorker := startTestRefreshWorker(t, service, ctx)
	defer func() { cancel(); stopWorker() }()

	waitFetchCanceled := func() {
		t.Helper()
		select {
		case fctx := <-fetchCtxCh:
			select {
			case <-fctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("fetchCtx was not canceled by coordinator")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no fetch request was made")
		}
	}

	// Hot-add: first refresh adds agent_1.
	service.signalRefresh()
	waitFetchCanceled()

	// fetchCtx is now canceled; applyFetchedWorkspace has completed. Freeze records.
	service.mu.Lock()
	worker1 := service.agentWorkers["agent_1"]
	runtime1 := service.agentRuntimes["agent_1"]
	service.mu.Unlock()
	if worker1 == nil || runtime1 == nil {
		t.Fatal("hot-add should create both worker and runtime")
	}

	if runtime1.parentCtx != service.agentBaseCtx {
		t.Fatal("runtime parentCtx should be agentBaseCtx, not transient fetchCtx")
	}
	select {
	case <-worker1.done:
		t.Fatal("worker died after fetchCtx cancellation; should survive on agentBaseCtx")
	default:
	}
	if runtime1.stopped() {
		t.Fatal("runtime stopped after fetchCtx cancellation; should survive on agentBaseCtx")
	}

	// Wake worker and prove it completed an inbox poll after fetchCtx release.
	baseline := inboxPolls.Load()
	select {
	case worker1.wake <- struct{}{}:
	default:
	}
	waitUntil(t, func() bool { return inboxPolls.Load() > baseline })
	select {
	case <-worker1.done:
		t.Fatal("worker died after post-release inbox poll; should remain alive")
	default:
	}

	// Removal: second refresh removes agent_1.
	service.signalRefresh()
	waitFetchCanceled()
	service.mu.Lock()
	workerGone := service.agentWorkers["agent_1"] == nil
	runtimeGone := service.agentRuntimes["agent_1"] == nil
	service.mu.Unlock()
	if !workerGone {
		t.Fatal("worker map entry should be absent after removal refresh")
	}
	if !runtimeGone {
		t.Fatal("runtime map entry should be absent after removal refresh")
	}
	if runtime1.runtimeCtx.Err() == nil {
		t.Fatal("removed runtime runtimeCtx should be canceled by closeManagedWorkspaceRuntime")
	}
	select {
	case <-worker1.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not join after removal")
	}
	select {
	case <-runtime1.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not join after removal")
	}

	// Re-add: third refresh adds agent_1 back.
	service.signalRefresh()
	waitFetchCanceled()
	service.mu.Lock()
	worker2 := service.agentWorkers["agent_1"]
	runtime2 := service.agentRuntimes["agent_1"]
	service.mu.Unlock()
	if worker2 == nil || runtime2 == nil {
		t.Fatal("re-add should create new worker and runtime")
	}
	if worker2 == worker1 {
		t.Fatal("re-add should create a new worker, not reuse the removed one")
	}
	if runtime2.parentCtx != service.agentBaseCtx {
		t.Fatal("re-added runtime parentCtx should be agentBaseCtx")
	}
	select {
	case <-worker2.done:
		t.Fatal("re-added worker died immediately")
	default:
	}
	if runtime2.stopped() {
		t.Fatal("re-added runtime stopped immediately")
	}

	// Service shutdown: cancel ctx, wait for both new records to join.
	cancel()
	stopWorker()
	select {
	case <-worker2.done:
	case <-time.After(5 * time.Second):
		t.Fatal("re-added worker did not join on shutdown")
	}
	select {
	case <-runtime2.done:
	case <-time.After(5 * time.Second):
		t.Fatal("re-added runtime did not join on shutdown")
	}
	service.closeAgentWorkers()
	service.closeAgentRuntimes()
}

func startTestRefreshWorker(t *testing.T, s *Service, ctx context.Context) func() {
	t.Helper()
	if s.agentBaseCtx == nil {
		s.agentBaseCtx = ctx
	}
	s.refreshNeeded = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runRefreshCoordinator(ctx, nil)
	}()
	return sync.OnceFunc(func() { close(s.refreshNeeded); <-done })
}
