package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	"notty/internal/yproto"
)

func TestComputeReplaceFindsInnerSpan(t *testing.T) {
	op := computeReplace("hello world", "hello brave world")
	if op != (replaceOp{Start: 6, End: 6, Text: "brave "}) {
		t.Fatalf("unexpected op: %#v", op)
	}
}

func TestComputeReplaceHandlesReplacement(t *testing.T) {
	op := computeReplace("alpha beta gamma", "alpha zeta gamma")
	if op.Start != 6 || op.End != 7 || op.Text != "z" {
		t.Fatalf("unexpected op: %#v", op)
	}
}

func TestReconcileAgentReplicasKeepsUUIDActorWhenHandleChanges(t *testing.T) {
	cancelled := false
	service := &Service{
		agentReplicas: map[string]*managedReplica{
			"agent_1": {
				replica: &workspaceReplica{actorID: "agent_1"},
				cancel: func() {
					cancelled = true
				},
			},
		},
	}
	workspace := &workspaceResponse{
		Agents: []*agent{{ID: "agent_1", Handle: "renamed-agent"}},
	}
	if err := service.reconcileAgentReplicas(context.Background(), workspace); err != nil {
		t.Fatalf("reconcile agent replicas: %v", err)
	}
	if cancelled {
		t.Fatal("agent replica should not restart when only the handle changes")
	}
	replica := service.agentReplicas["agent_1"]
	if replica == nil || replica.replica == nil || replica.replica.actorID != "agent_1" {
		t.Fatalf("expected UUID actor to be preserved, got %#v", replica)
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
	service := &Service{
		cfg: Config{WorkspaceDir: root},
		projectedByPath: map[string]*trackedFile{
			hiddenPath:  hidden,
			visiblePath: visible,
		},
	}

	if err := service.handleLocalChange(hiddenPath); err != nil {
		t.Fatalf("handle hidden local change: %v", err)
	}
	if hidden.isLocalDirty() {
		t.Fatal("hidden path was marked dirty")
	}
	if err := service.handleLocalChange(visiblePath); err != nil {
		t.Fatalf("handle visible local change: %v", err)
	}
	if !visible.isLocalDirty() {
		t.Fatal("visible path was not marked dirty")
	}
}

func TestTrackedFileClearConnOnlyClearsMatchingConnection(t *testing.T) {
	tracked := &trackedFile{}
	first := &websocket.Conn{}
	second := &websocket.Conn{}

	tracked.setConn(first)
	if cleared := tracked.clearConn(second); cleared {
		t.Fatal("expected mismatched connection clear to be ignored")
	}
	if got := tracked.getConn(); got != first {
		t.Fatalf("expected first connection to remain, got %#v", got)
	}

	tracked.setConn(second)
	if cleared := tracked.clearConn(first); cleared {
		t.Fatal("expected stale connection clear to be ignored")
	}
	if got := tracked.getConn(); got != second {
		t.Fatalf("expected second connection to remain, got %#v", got)
	}

	if cleared := tracked.clearConn(second); !cleared {
		t.Fatal("expected matching connection clear to succeed")
	}
	if got := tracked.getConn(); got != nil {
		t.Fatalf("expected connection to be nil, got %#v", got)
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
		client:          client,
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		agentReplicas:   map[string]*managedReplica{},
		agentWorkers:    map[string]*managedAgentWorker{},
	}

	if err := service.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	service.closeAgentWorkers()
	service.closeAgentReplicas()

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
			RuntimeDir:         t.TempDir(),
			AgentID:            "daemon_agent",
		},
		client:          client,
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		agentReplicas:   map[string]*managedReplica{},
		agentWorkers:    map[string]*managedAgentWorker{},
	}
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, factory.new)
	defer service.sessions.Shutdown()
	defer service.closeAgentWorkers()

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
	tracked := &trackedFile{Path: filepath.Join(t.TempDir(), "doc.md")}
	tracked.setProjectedContent("old")

	original := writeProjectedFile
	defer func() { writeProjectedFile = original }()

	writeProjectedFile = func(path, content string, expected projectedContentHash) error {
		if !tracked.matchesProjectedString("new") {
			t.Fatal("projected content hash was not updated before write")
		}
		return nil
	}

	clean, err := applyProjectedContent(tracked, "new")
	if err != nil {
		t.Fatalf("apply projected content: %v", err)
	}
	if !clean {
		t.Fatal("expected projected write to leave disk clean")
	}

	if !tracked.matchesProjectedString("new") {
		t.Fatal("unexpected projected content hash after write")
	}
}

func TestApplyProjectedContentRollsBackOnWriteFailure(t *testing.T) {
	tracked := &trackedFile{Path: filepath.Join(t.TempDir(), "doc.md")}
	tracked.setProjectedContent("old")

	original := writeProjectedFile
	defer func() { writeProjectedFile = original }()

	writeProjectedFile = func(path, content string, expected projectedContentHash) error {
		return errors.New("disk failure")
	}

	if _, err := applyProjectedContent(tracked, "new"); err == nil {
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

	clean, err := applyProjectedContent(tracked, "remote update")
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

func TestProjectedBaseLivesUnderWorkspaceNottyWithCRDTState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "doc.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := newDocWithText(t, "base")
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "docs/doc.md",
		Path:         path,
	}

	if err := tracked.storeProjectedBase("base", doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	projectionDir := filepath.Join(root, ".notty", "projections", "doc_1")
	if _, err := os.Stat(filepath.Join(projectionDir, "base.txt")); err != nil {
		t.Fatalf("expected workspace projection text: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectionDir, "base.state.bin")); err != nil {
		t.Fatalf("expected workspace projection state: %v", err)
	}
	content, state, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || content != "base" {
		t.Fatalf("unexpected projected base known=%v content=%q", known, content)
	}
	if !projectedStateMatchesContent(state, "base") {
		t.Fatal("projected CRDT state does not materialize to projected text")
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
	tracked.writeSyncUpdate = func(update []byte) error {
		t.Fatalf("must not send update during no-op reconcile; update bytes=%d", len(update))
		return nil
	}

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("no-op reconcile should not touch disk or CRDT state: %v", err)
	}
}

func TestReconcileTrackedDocumentClearsProjectedWriteDirtyWithoutLoadingBase(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(tracked.projectionDir(), "base.state.bin"), []byte{0xff, 0xff, 0xff}, 0o644); err != nil {
		t.Fatalf("corrupt projection state: %v", err)
	}
	tracked.markLocalDirty()
	tracked.writeSyncUpdate = func(update []byte) error {
		t.Fatalf("must not send update for dirty flag caused by projected write; update bytes=%d", len(update))
		return nil
	}

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile projected-write dirty flag: %v", err)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected false local dirty flag to clear when disk still matches projection")
	}
}

func TestCachedDaemonDoesNotAnswerServerSyncStep1FromProjectionCache(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if err := cache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "cached local projection")); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_1",
		DocumentPath: "doc.md",
		cache:        cache,
	}
	payload := yproto.BuildSyncStep1FromStateVector(nil)
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode sync step1: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("unexpected message type %d", messageType)
	}

	reply, appended, err := handleRemoteSyncMessage(reader, tracked, "remote")
	if err != nil {
		t.Fatalf("handle server sync step1: %v", err)
	}
	if appended {
		t.Fatal("sync step1 should not append a remote update")
	}
	if len(reply) != 0 {
		t.Fatalf("cached daemon projection should not be sent back as a sync step2 reply, got %d bytes", len(reply))
	}
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
	tracked.writeSyncUpdate = func(update []byte) error {
		sentLocalUpdate = append([]byte(nil), update...)
		return crdt.ApplyUpdateV1(serverDoc, update, "server-local")
	}
	tracked.writeSyncStep1 = func(_ []byte) error {
		tracked.setServerStateVector(crdt.EncodeStateVectorV1(serverDoc))
		return nil
	}

	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local edit: %v", err)
	}
	if err := markTrackedLocalDirty(tracked, path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if len(sentLocalUpdate) == 0 {
		t.Fatal("expected local CRDT update to be sent")
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
	if err := os.MkdirAll(tracked.projectionDir(), 0o755); err != nil {
		t.Fatalf("create projection dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tracked.projectionDir(), "base.txt"), []byte("already synced\n"), 0o644); err != nil {
		t.Fatalf("write projection text without state: %v", err)
	}
	tracked.markLocalDirty()
	tracked.writeSyncUpdate = func(update []byte) error {
		t.Fatalf("must not send a full-file update when cached CRDT state is missing; update bytes=%d", len(update))
		return nil
	}

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to remain until a matching CRDT base is available")
	}
}

func TestReconcileTrackedDocumentDefersLocalUpdateWithoutProjectedBase(t *testing.T) {
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
	tracked.writeSyncUpdate = func(update []byte) error {
		t.Fatalf("must not send local update before a projected base is established; update bytes=%d", len(update))
		return nil
	}

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile document: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to remain until projection base is established")
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
	tracked.writeSyncUpdate = func(update []byte) error {
		sentUpdates++
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)
	if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
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

func TestReconcileRebasesDirtyLocalFileAfterPendingRemoteEstablishesBase(t *testing.T) {
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
	tracked.writeSyncUpdate = func(update []byte) error {
		sentUpdates++
		return nil
	}
	if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if sentUpdates != 0 {
		t.Fatalf("expected no local update before rebasing, got %d", sentUpdates)
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != baseContent {
		t.Fatalf("expected projected base to be established from remote update, known=%v base=%q", known, projectedBase)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag to remain after rebasing")
	}

	baseDoc, _, _, err := cache.loadBaseDoc("doc_append", "append.txt")
	if err != nil {
		t.Fatalf("load cached base doc: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked.writeSyncUpdate = func(update []byte) error {
		sentUpdates++
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)
	if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if sentUpdates != 1 {
		t.Fatalf("expected one local update after rebasing, got %d", sentUpdates)
	}
	if got := serverDoc.GetText("content").ToString(); got != localContent {
		t.Fatalf("server content mismatch: got %q want %q", got, localContent)
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
	tracked.writeSyncUpdate = func(update []byte) error {
		sentUpdates++
		if len(update) > maxUpdateBytes {
			maxUpdateBytes = len(update)
		}
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)

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
		if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
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

func TestReconcileKeepsSentLocalAppendAsWorkspaceBaseWhenProjectionDiverges(t *testing.T) {
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
	tracked.writeSyncUpdate = func(update []byte) error {
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)

	original := writeProjectedFile
	defer func() { writeProjectedFile = original }()
	divergeOnce := true
	writeProjectedFile = func(path string, content string, expected projectedContentHash) error {
		if divergeOnce && content == "1\n2\n" {
			divergeOnce = false
			if err := os.WriteFile(path, []byte("1\n2\n3\n"), 0o644); err != nil {
				return err
			}
		}
		return original(path, content, expected)
	}

	if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
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
		t.Fatalf("expected sent local content as projected base after divergence, known=%v base=%q", known, projectedBase)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to remain for concurrent append")
	}

	if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
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
	remoteUpdate := updateFromBaseDoc(t, projectedDoc, "1\n2\n", "remote")
	sharedDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(sharedDoc, projectedDoc.EncodeStateAsUpdate(), "projected"); err != nil {
		t.Fatalf("apply projected state: %v", err)
	}
	if err := crdt.ApplyUpdateV1(sharedDoc, remoteUpdate, "remote"); err != nil {
		t.Fatalf("apply remote update: %v", err)
	}
	if err := cache.storeDoc("doc_append", "append.txt", 1, sharedDoc); err != nil {
		t.Fatalf("store shared doc: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, sharedDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked := &trackedFile{
		DocumentID:   "doc_append",
		DocumentPath: "append.txt",
		Path:         path,
		cache:        cache,
	}
	tracked.setProjectedContent("1\n")
	if err := tracked.storeProjectedBase("1\n", projectedDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	sentUpdates := 0
	tracked.writeSyncUpdate = func(update []byte) error {
		sentUpdates++
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)
	if err := reconcileTrackedDocument(cache, "doc_append", []*trackedFile{tracked}); err != nil {
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

func TestReconcileDoesNotDeleteRemoteUpdateAfterRemoteProjectionDiverges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
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

	original := writeProjectedFile
	defer func() { writeProjectedFile = original }()
	divergeOnce := true
	writeProjectedFile = func(path string, content string, expected projectedContentHash) error {
		if divergeOnce && content == "base\nremote\n" {
			divergeOnce = false
			if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
				return err
			}
		}
		return original(path, content, expected)
	}

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected local dirty flag after remote projection raced with local write")
	}
	projectedBase, _, known, err := tracked.loadProjectedBase()
	if err != nil {
		t.Fatalf("load projected base: %v", err)
	}
	if !known || projectedBase != "base\n" {
		t.Fatalf("expected remote-only projection divergence to keep old base, known=%v base=%q", known, projectedBase)
	}

	cachedDoc, _, _, err := cache.loadBaseDoc("doc_1", "doc.md")
	if err != nil {
		t.Fatalf("load cached doc: %v", err)
	}
	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, cachedDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	sentUpdates := 0
	tracked.writeSyncUpdate = func(update []byte) error {
		sentUpdates++
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if sentUpdates != 1 {
		t.Fatalf("expected one rebased local update, got %d", sentUpdates)
	}
	if got := serverDoc.GetText("content").ToString(); !containsAll(got, "base\n", "remote\n", "local\n") {
		t.Fatalf("expected local edit and remote update to both survive, got %q", got)
	}
}

func TestReconcileTrackedDocumentClearsCorruptPendingRemoteLog(t *testing.T) {
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
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("append corrupt pending remote: %v", err)
	}

	err = reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked})
	if err == nil || !strings.Contains(err.Error(), "doc_1") {
		t.Fatalf("expected document-scoped corrupt pending error, got %v", err)
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected corrupt pending log to be cleared, got %d pending updates", count)
	}
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("expected second reconcile after clearing corrupt log to be clean, got %v", err)
	}
}

func TestReconcileTrackedDocumentClearsPendingMetadataHashMismatch(t *testing.T) {
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
	metadataPath := cache.metadataPath("doc_1")
	var metadata documentCacheMetadata
	payload, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	metadata.PendingSHA256 = "stale-hash"
	nextPayload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	if err := os.WriteFile(metadataPath, nextPayload, 0o644); err != nil {
		t.Fatalf("write stale metadata: %v", err)
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

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("expected stale pending metadata to be cleared without failing reconcile: %v", err)
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected stale pending log to be cleared, got %d pending updates", count)
	}
}

func TestServiceReconcileTrackedDocumentsSharesRemoteUpdateAcrossAgentWorkspaces(t *testing.T) {
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

	mainPath := filepath.Join(t.TempDir(), "doc.md")
	agentPath := filepath.Join(t.TempDir(), "doc.md")
	for _, path := range []string{mainPath, agentPath} {
		if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
			t.Fatalf("write projection: %v", err)
		}
	}
	mainTracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: mainPath, cache: cache}
	agentTracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: agentPath, cache: cache}
	for _, tracked := range []*trackedFile{mainTracked, agentTracked} {
		tracked.setProjectedContent("base")
		if err := tracked.storeProjectedBase("base"); err != nil {
			t.Fatalf("store projected base: %v", err)
		}
	}
	service := &Service{
		docCache:        cache,
		projectedByID:   map[string]*trackedFile{"doc_1": mainTracked},
		projectedByPath: map[string]*trackedFile{mainPath: mainTracked},
		agentReplicas: map[string]*managedReplica{
			"agent_1": {
				replica: &workspaceReplica{
					projectedByID:   map[string]*trackedFile{"doc_1": agentTracked},
					projectedByPath: map[string]*trackedFile{agentPath: agentTracked},
				},
			},
		},
	}
	if err := service.reconcileTrackedDocuments(context.Background()); err != nil {
		t.Fatalf("reconcile tracked documents: %v", err)
	}
	for _, path := range []string{mainPath, agentPath} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read projection: %v", err)
		}
		if string(content) != "base remote" {
			t.Fatalf("unexpected projection at %s: %q", path, content)
		}
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

func TestReconcileDoesNotAdvanceRemoteBehindDisconnectedLocalDirtyWorkspace(t *testing.T) {
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
	if err := tracked.storeProjectedBase("base\n"); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()

	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("expected disconnected local dirty workspace to remain dirty")
	}
	count, err := cache.pendingRemoteUpdateCount("doc_1")
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected pending remote update to remain for retry, got %d", count)
	}
	materialized, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "doc.md"})
	if err != nil {
		t.Fatalf("materialize cache: %v", err)
	}
	if got := materialized.Doc.GetText("content").ToString(); got != "base\n" {
		t.Fatalf("shared base advanced despite unsent local edit: %q", got)
	}
}

func TestMaterializeTrackedFileProjectsMissingFileFromSharedCache(t *testing.T) {
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
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(content) != "remote content" {
		t.Fatalf("unexpected cached content projection: %q", content)
	}
	if tracked.DocumentID != "doc_1" || tracked.Path != path {
		t.Fatalf("unexpected tracked file: %#v", tracked)
	}
	if !tracked.matchesProjectedString("remote content") {
		t.Fatal("expected cached content hash to be recorded")
	}
	if tracked.shouldSyncFromScratch() {
		t.Fatal("expected cached projection to avoid full websocket bootstrap")
	}
}

func TestMaterializeTrackedFileCreatesEmptyFileForKnownEmptyDocument(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "empty.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}

	tracked, err := materializeTrackedFile(context.Background(), cache, &document{
		ID:          "doc_1",
		Path:        "docs/empty.md",
		StateVector: emptyDocumentStateVector,
	}, path)
	if err != nil {
		t.Fatalf("materialize tracked file: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(content) != "" {
		t.Fatalf("unexpected materialized content: %q", content)
	}
	if !tracked.hasProjectedContent() || !tracked.matchesProjectedString("") {
		t.Fatal("expected known empty projection to be recorded")
	}
}

func TestHandleLocalChangeSkipsProjectedWriteWhileProjectionIsInFlight(t *testing.T) {
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

	service := &Service{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	original := writeProjectedFile
	defer func() { writeProjectedFile = original }()

	writeProjectedFile = func(path, content string, expected projectedContentHash) error {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			return err
		}
		if err := service.handleLocalChange(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(content), 0o644)
	}

	updateDocText(t, doc, "hello world", "remote")
	if _, err := applyProjectedContent(tracked, "hello world"); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if got := doc.GetText("content").ToString(); got != "hello world" {
		t.Fatalf("projected write should not be replayed from a truncated file, got %q", got)
	}
	if !tracked.matchesProjectedString("hello world") {
		t.Fatal("unexpected projected content hash after projected write")
	}
}

func TestWorkspaceReplicaHandleLocalChangeSkipsProjectedWriteWhileProjectionIsInFlight(t *testing.T) {
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

	original := writeProjectedFile
	defer func() { writeProjectedFile = original }()

	writeProjectedFile = func(path, content string, expected projectedContentHash) error {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			return err
		}
		if err := replica.handleLocalChange(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(content), 0o644)
	}

	updateDocText(t, doc, "hello world", "remote")
	if _, err := applyProjectedContent(tracked, "hello world"); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if got := doc.GetText("content").ToString(); got != "hello world" {
		t.Fatalf("replica projected write should not be replayed from a truncated file, got %q", got)
	}
	if !tracked.matchesProjectedString("hello world") {
		t.Fatal("unexpected replica projected content hash after projected write")
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

	service := &Service{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	updateDocText(t, doc, "hello world", "remote")
	if _, err := applyProjectedContent(tracked, "hello world"); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if err := service.handleLocalChange(path); err != nil {
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
	if _, err := applyProjectedContent(tracked, "hello world"); err != nil {
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

	service := &Service{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	if err := service.handleLocalChange(path); err != nil {
		t.Fatalf("first local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello" {
		t.Fatalf("local change should not mutate CRDT before reconciliation, got %q", got)
	}
	if err := service.handleLocalChange(path); err != nil {
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

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer watcher.Close()

	doc := newDocWithText(t, "same")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       oldPath,
		Doc:        doc,
		Conn:       &websocket.Conn{},
	}
	tracked.setProjectedContent("same")

	var sawMove bool
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodPatch && r.URL.Path == "/api/documents/doc_1":
				sawMove = true
				var payload updateDocumentRequest
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("decode move payload: %v", err)
				}
				if payload.Path != "docs/new.md" {
					t.Fatalf("unexpected move target: %q", payload.Path)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"doc_1"}`)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
				body, err := json.Marshal(workspaceResponse{
					Documents: []*document{{ID: "doc_1", Path: "docs/new.md"}},
				})
				if err != nil {
					t.Fatalf("marshal workspace: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				return nil, nil
			}
		}),
	}

	service := &Service{
		cfg: Config{
			BackendURL:   "http://backend.test",
			WorkspaceDir: root,
			AgentID:      "daemon_agent",
		},
		client:          client,
		watcher:         watcher,
		projectedByPath: map[string]*trackedFile{oldPath: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}

	if err := service.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if !sawMove {
		t.Fatal("expected reconcile to issue move request")
	}
	if tracked.Path != newPath {
		t.Fatalf("expected tracked path to move to %q, got %q", newPath, tracked.Path)
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

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatalf("projection reconcile should not call backend, got %s %s", r.Method, r.URL.Path)
			return nil, nil
		}),
	}

	service := &Service{
		cfg: Config{
			BackendURL:   "http://backend.test",
			WorkspaceDir: root,
			AgentID:      "daemon_agent",
		},
		client:          client,
		projectedByPath: map[string]*trackedFile{path: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}

	if err := service.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if service.projectedByID["doc_1"] != tracked || service.projectedByPath[path] != tracked {
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

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatalf("replica projection reconcile should not call backend, got %s %s", r.Method, r.URL.Path)
			return nil, nil
		}),
	}

	replica := &workspaceReplica{
		rootDir:         root,
		backendURL:      "http://backend.test",
		client:          client,
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

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer watcher.Close()

	doc := newDocWithText(t, "gone")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       oldPath,
		Doc:        doc,
	}
	tracked.setProjectedContent("gone")

	var sawDelete bool
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodDelete && r.URL.Path == "/api/documents/doc_1":
				sawDelete = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"status":"deleted"}`)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
				body, err := json.Marshal(workspaceResponse{Documents: []*document{}})
				if err != nil {
					t.Fatalf("marshal workspace: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				return nil, nil
			}
		}),
	}

	service := &Service{
		cfg: Config{
			BackendURL:   "http://backend.test",
			WorkspaceDir: root,
			AgentID:      "daemon_agent",
		},
		client:          client,
		watcher:         watcher,
		projectedByPath: map[string]*trackedFile{oldPath: tracked},
		projectedByID:   map[string]*trackedFile{"doc_1": tracked},
	}

	if err := service.reconcileLocalWorkspace(context.Background()); err != nil {
		t.Fatalf("reconcile workspace: %v", err)
	}
	if !sawDelete {
		t.Fatal("expected reconcile to delete missing tracked document")
	}
	if len(service.projectedByID) != 0 || len(service.projectedByPath) != 0 {
		t.Fatalf("expected tracked maps to be empty after delete, got ids=%d paths=%d", len(service.projectedByID), len(service.projectedByPath))
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
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: cache}
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()

	sendAttempts := 0
	tracked.writeSyncUpdate = func(update []byte) error {
		sendAttempts++
		return errors.New("backend disconnected")
	}
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
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

	serverDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(serverDoc, baseDoc.EncodeStateAsUpdate(), "server-base"); err != nil {
		t.Fatalf("apply server base: %v", err)
	}
	tracked.writeSyncUpdate = func(update []byte) error {
		sendAttempts++
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	ackTrackedFromServer(tracked, serverDoc)
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
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

func TestOutgoingOutboxWaitsForBackendStateVectorBeforeClearing(t *testing.T) {
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
	tracked.writeSyncUpdate = func(update []byte) error {
		sends++
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	tracked.writeSyncStep1 = func(_ []byte) error {
		return nil
	}
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("dirty flag must remain without backend state-vector proof")
	}
	if sends != 1 {
		t.Fatalf("expected one send before ack, got %d", sends)
	}

	tracked.setServerStateVector(crdt.EncodeStateVectorV1(serverDoc))
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("ack reconcile: %v", err)
	}
	if sends != 1 {
		t.Fatalf("converged outbox should finalize without resending, sends=%d", sends)
	}
	if tracked.isLocalDirty() {
		t.Fatal("expected dirty flag to clear after observed convergence")
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
	tracked.writeSyncUpdate = func(update []byte) error {
		return crdt.ApplyUpdateV1(serverDoc, update, "server")
	}
	tracked.writeSyncStep1 = func(_ []byte) error {
		return nil
	}
	if err := reconcileTrackedDocument(cache, "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := serverDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("server should have accepted first send before restart, got %q", got)
	}

	reopened, err := newDocumentCache(cacheRoot)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	restarted := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, cache: reopened}
	restarted.setProjectedContent("base\n")
	restarted.markLocalDirty()
	duplicateSends := 0
	restarted.writeSyncUpdate = func(update []byte) error {
		duplicateSends++
		return crdt.ApplyUpdateV1(serverDoc, update, "server-duplicate")
	}
	ackTrackedFromServer(restarted, serverDoc)
	if err := reconcileTrackedDocument(reopened, "doc_1", []*trackedFile{restarted}); err != nil {
		t.Fatalf("restarted reconcile: %v", err)
	}
	if duplicateSends != 1 {
		t.Fatalf("expected one idempotent resend after restart, got %d", duplicateSends)
	}
	if got := serverDoc.GetText("content").ToString(); got != "base\nlocal\n" {
		t.Fatalf("duplicate resend changed server content: %q", got)
	}
	if restarted.isLocalDirty() {
		t.Fatal("expected restarted daemon to clear dirty flag after convergence")
	}
}

func TestShouldWakeAgentWorkersForEventIncludesDocumentUpdates(t *testing.T) {
	if shouldWakeAgentWorkersForEvent("document.updated") {
		t.Fatal("document.updated should not be the agent wake source; agent.inbox.changed should")
	}
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

func ackTrackedFromServer(tracked *trackedFile, serverDoc *crdt.Doc) {
	tracked.writeSyncStep1 = func(_ []byte) error {
		tracked.setServerStateVector(crdt.EncodeStateVectorV1(serverDoc))
		return nil
	}
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
