package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
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

	if err := applyProjectedContent(tracked, "new"); err != nil {
		t.Fatalf("apply projected content: %v", err)
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

	if err := applyProjectedContent(tracked, "new"); err == nil {
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

	if err := applyProjectedContent(tracked, "remote update"); err != nil {
		t.Fatalf("apply projected content: %v", err)
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

func TestHandleLocalChangeMergesLocalEditAfterProjectionConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	doc := newDocWithText(t, "base\n")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
	tracked.setProjectedContent("base\n")

	service := &Service{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	updateDocText(t, doc, "base\nremote\n", "remote")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local edit: %v", err)
	}
	if err := applyProjectedContent(tracked, "base\nremote\n"); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if err := service.handleLocalChange(path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}

	want := "base\nlocal\nremote\n"
	if got := doc.GetText("content").ToString(); got != want {
		t.Fatalf("expected merged CRDT content %q, got %q", want, got)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projected merge: %v", err)
	}
	if string(content) != want {
		t.Fatalf("expected merged file content %q, got %q", want, content)
	}
	if !tracked.matchesProjectedString(want) {
		t.Fatal("expected projected content hash to advance to merged content")
	}
}

func TestWorkspaceReplicaHandleLocalChangeMergesLocalEditAfterProjectionConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	doc := newDocWithText(t, "base\n")
	tracked := &trackedFile{
		DocumentID: "doc_1",
		Path:       path,
		Doc:        doc,
	}
	tracked.setProjectedContent("base\n")

	replica := &workspaceReplica{
		projectedByPath: map[string]*trackedFile{path: tracked},
	}

	updateDocText(t, doc, "base\nremote\n", "remote")
	if err := os.WriteFile(path, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write local edit: %v", err)
	}
	if err := applyProjectedContent(tracked, "base\nremote\n"); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}

	want := "base\nlocal\nremote\n"
	if got := doc.GetText("content").ToString(); got != want {
		t.Fatalf("expected replica merged CRDT content %q, got %q", want, got)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replica projected merge: %v", err)
	}
	if string(content) != want {
		t.Fatalf("expected replica merged file content %q, got %q", want, content)
	}
	if !tracked.matchesProjectedString(want) {
		t.Fatal("expected replica projected content hash to advance to merged content")
	}
}

func TestMaterializeTrackedFileWritesNewRemoteDocumentToDisk(t *testing.T) {
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
		t.Fatalf("unexpected materialized content: %q", content)
	}
	if tracked.DocumentID != "doc_1" || tracked.Path != path {
		t.Fatalf("unexpected tracked file: %#v", tracked)
	}
	if !tracked.matchesProjectedString("remote content") {
		t.Fatal("expected materialized content hash to be recorded")
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
	if err := applyProjectedContent(tracked, "hello world"); err != nil {
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
	if err := applyProjectedContent(tracked, "hello world"); err != nil {
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
	if err := applyProjectedContent(tracked, "hello world"); err != nil {
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
	if err := applyProjectedContent(tracked, "hello world"); err != nil {
		t.Fatalf("apply projected content: %v", err)
	}

	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("handle local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello world" {
		t.Fatalf("expected replica-projected write to be ignored, got %q", got)
	}
}

func TestHandleLocalChangeWhileDisconnectedDoesNotReapplySameDiff(t *testing.T) {
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
	if got := doc.GetText("content").ToString(); got != "hello brave" {
		t.Fatalf("unexpected content after first local change: %q", got)
	}
	if err := service.handleLocalChange(path); err != nil {
		t.Fatalf("second local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello brave" {
		t.Fatalf("local change was applied twice while disconnected: %q", got)
	}
	if !tracked.matchesProjectedString("hello brave") {
		t.Fatal("expected projected content hash to advance while disconnected")
	}
}

func TestWorkspaceReplicaHandleLocalChangeWhileDisconnectedDoesNotReapplySameDiff(t *testing.T) {
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
	if got := doc.GetText("content").ToString(); got != "hello brave" {
		t.Fatalf("unexpected content after first local change: %q", got)
	}
	if err := replica.handleLocalChange(path); err != nil {
		t.Fatalf("second local change: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "hello brave" {
		t.Fatalf("replica local change was applied twice while disconnected: %q", got)
	}
	if !tracked.matchesProjectedString("hello brave") {
		t.Fatal("expected replica projected content hash to advance while disconnected")
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

func TestShouldWakeAgentWorkersForEventIncludesDocumentUpdates(t *testing.T) {
	if !shouldWakeAgentWorkersForEvent("document.updated") {
		t.Fatal("document.updated is a product notification and should wake agent workers")
	}
	if !shouldWakeAgentWorkersForEvent("thread.replied") {
		t.Fatal("thread replies should wake agent workers")
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
	doc.Transact(func(txn *crdt.Transaction) {
		if current != "" {
			text.Delete(txn, 0, len(current))
		}
		if content != "" {
			text.Insert(txn, 0, content, nil)
		}
	}, origin)
}

func encodeDocState(doc *crdt.Doc) string {
	return base64.StdEncoding.EncodeToString(doc.EncodeStateAsUpdate())
}

func encodeStateForContent(t *testing.T, content string) string {
	t.Helper()
	return encodeDocState(newDocWithText(t, content))
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
