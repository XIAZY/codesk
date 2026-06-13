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

	"github.com/gorilla/websocket"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

type documentUpdateTestResponse struct {
	Accepted bool  `json:"accepted"`
	Applied  bool  `json:"applied"`
	UpdateID int64 `json:"updateId"`
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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

func TestIgnoredWorkspacePathPolicy(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		path    string
		ignored bool
	}{
		{path: filepath.Join(root, ".notty", "codex-agent.log"), ignored: true},
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
		".notty/codex-agent.log":   "internal",
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
	hiddenPath := filepath.Join(root, ".notty", "codex-agent.log")
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        client,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}

	if err := service.refresh(ctx); err != nil {
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
	factory := newFakeAppServerFactory()
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
			WorkspaceDir:       t.TempDir(),
			AgentWorkspaceRoot: t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:        client,
		agentRuntimes: map[string]*managedWorkspaceRuntime{},
		agentWorkers:  map[string]*managedAgentWorker{},
	}
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, factory.new)
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
	tracked := &trackedFile{Path: path}
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
	tracked := &trackedFile{Path: path}
	tracked.setProjectedContent("old")

	if _, err := applyProjectedContent(tracked, "new", nil, 0); err == nil {
		t.Fatal("expected projected write failure")
	}

	if !tracked.matchesProjectedString("old") {
		t.Fatal("expected rollback to original projected content hash")
	}
}

func TestApplyProjectedContentDoesNotOverwriteDivergedDiskState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("old plus local edit"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	tracked := &trackedFile{Path: path}
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(workspaceSyncDBPath(root))
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("create unreadable-as-file path: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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

	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "")); err != nil {
		t.Fatalf("store empty remote base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
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

	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "")
	if err := cache.storeDoc("doc_append", "append.txt", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	}
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
		if err := appendOpenFileLocked(file, line); err != nil {
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

	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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

	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(cacheRoot)
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
	cache, err = newDocumentCache(cacheRoot)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		Path:         path,
		cache:        cache,
	}
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
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

func TestCentralReconcilePublishesQueuedCleanMove(t *testing.T) {
	root := t.TempDir()
	newPath := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("same"), 0o644); err != nil {
		t.Fatalf("write moved file: %v", err)
	}
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "same")
	if err := cache.storeDoc("doc_1", "docs/old.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:    "doc_1",
		DocumentPath:  "docs/old.md",
		Path:          newPath,
		WorkspaceRoot: root,
		cache:         cache,
	}
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
	cache, err := newDocumentCache(t.TempDir())
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	service := &workspaceRuntime{cfg: Config{BackendURL: server.URL, AgentID: "daemon_agent"}, client: server.Client(), docCache: cache}

	created, err := service.createDocumentFromLocalCandidate(context.Background(), localCreateCandidate{Root: root, Path: path, ActorID: "daemon_agent", ActorType: "daemon"}, "docs/new.md")
	if err != nil {
		t.Fatalf("create from local candidate: %v", err)
	}
	if created == nil || created.ID != "doc_created" {
		t.Fatalf("unexpected created doc: %#v", created)
	}
	if len(seen) != 0 {
		t.Fatalf("unexpected create payload: %#v", seen)
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

func TestOutgoingOutboxKeepsLocalUpdateWhenBackendSendFails(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache, ActorID: "agent_1", ActorType: "agent"}
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
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base: %v", err)
	}
	primaryTracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: primaryPath, WorkspaceRoot: primaryRoot, cache: cache, ActorID: "daemon_agent", ActorType: "daemon"}
	agentTracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: agentPath, WorkspaceRoot: agentRoot, cache: cache, ActorID: "agent_1", ActorType: "agent"}
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(t.TempDir())
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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
	cache, err := newDocumentCache(cacheRoot)
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
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

	reopened, err := newDocumentCache(cacheRoot)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	restarted := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: reopened}
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

func TestShouldWakeAgentWorkersForEventUsesAgentInboxChanged(t *testing.T) {
	if shouldWakeAgentWorkersForEvent("thread.message.created") {
		t.Fatal("thread.message.created should not be the agent wake source; agent.inbox.changed should")
	}
	if !shouldWakeAgentWorkersForEvent("agent.inbox.changed") {
		t.Fatal("agent.inbox.changed should wake agent workers")
	}
	payload := json.RawMessage(`{"agentId":"agent_1","eventId":"aevt_1","box":"for_me","notificationType":"thread.mentioned"}`)
	change, ok := parseAgentInboxChangedEvent(workspaceEventEnvelope{Type: "agent.inbox.changed", Data: payload})
	if !ok || change.AgentID != "agent_1" || change.EventID != "aevt_1" {
		t.Fatalf("failed to parse inbox change: ok=%v change=%#v", ok, change)
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
