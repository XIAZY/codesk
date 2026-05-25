package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	crdt "notty/internal/ycrdt"
)

func TestReconcileQueueWakeCoalescesSignals(t *testing.T) {
	queue := newReconcileQueue()
	queue.Mark("doc_1")
	queue.Mark("doc_2")

	select {
	case <-queue.Wake():
	case <-time.After(time.Second):
		t.Fatal("expected first dirty mark to wake reconcile loop")
	}

	select {
	case <-queue.Wake():
		t.Fatal("expected wake channel to coalesce repeated dirty marks")
	default:
	}

	if got := queue.Drain(); len(got) != 2 || got[0] != "doc_1" || got[1] != "doc_2" {
		t.Fatalf("unexpected dirty drain: %#v", got)
	}
}

func TestWorkspaceRuntimeSyncDBsArePerLocalWorkspace(t *testing.T) {
	cfg := Config{WorkspaceDir: filepath.Join(t.TempDir(), "primary"), AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"), AgentID: "daemon_agent"}

	primary, err := newWorkspaceRuntime(cfg, http.DefaultClient, cfg.WorkspaceDir, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new primary runtime: %v", err)
	}
	defer primary.replica.watcher.Close()

	agentRoot := filepath.Join(cfg.AgentWorkspaceRoot, "agent_1")
	agent, err := newWorkspaceRuntime(cfg, http.DefaultClient, agentRoot, "agent_1", "agent")
	if err != nil {
		t.Fatalf("new agent runtime: %v", err)
	}
	defer agent.replica.watcher.Close()

	if primary.docCache == nil || agent.docCache == nil {
		t.Fatal("expected both runtimes to have document caches")
	}
	if primary.docCache.path == agent.docCache.path {
		t.Fatalf("runtime caches must be isolated, both used %s", primary.docCache.path)
	}
	if primary.docCache.path != filepath.Join(cfg.WorkspaceDir, ".notty", "sync.db") {
		t.Fatalf("primary cache path = %s, want workspace-local sync db", primary.docCache.path)
	}
	if agent.docCache.path != filepath.Join(agentRoot, ".notty", "sync.db") {
		t.Fatalf("agent cache path = %s, want agent-local sync db", agent.docCache.path)
	}

	if err := primary.docCache.storeDoc("doc_1", "doc.md", 1, newDocWithText(t, "primary")); err != nil {
		t.Fatalf("store primary doc: %v", err)
	}
	if got := agent.docCache.localStateVector("doc_1"); len(got) != 0 {
		t.Fatalf("agent cache should not contain primary state, got vector %v", got)
	}
}

func TestWorkspaceRuntimeLocalCreateWakesReconcileQueue(t *testing.T) {
	runtime := &workspaceRuntime{
		localCreates:     newLocalCreateQueue(),
		reconcileQueue:   newReconcileQueue(),
		documentSyncs:    map[string]*managedDocumentSync{},
		initialWorkspace: nil,
	}

	runtime.markLocalCreate(localCreateCandidate{Root: "/workspace", Path: "/workspace/new.md", ActorID: "daemon", ActorType: "daemon"})

	select {
	case <-runtime.reconcileQueue.Wake():
	case <-time.After(time.Second):
		t.Fatal("expected local create to wake the reconcile loop")
	}
	if got := runtime.localCreates.Drain(); len(got) != 1 || got[0].Path != "/workspace/new.md" {
		t.Fatalf("unexpected local create candidates: %#v", got)
	}
}

func TestWorkspaceRuntimeCreateEditDeleteMultipleFilesRegression(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
		AgentID:            "daemon_agent",
	}

	server := newWorkspaceRuntimeRegressionServer(t)
	defer server.Close()
	cfg.BackendURL = server.URL

	runtime, err := newWorkspaceRuntime(cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer runtime.replica.watcher.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer runtime.closeDocumentSyncs()

	initial := map[string]string{
		"docs/a.md":  "alpha\n",
		"docs/b.md":  "bravo\n",
		"notes/c.md": "charlie\n",
	}
	for rel, content := range initial {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		runtime.localCreates.Mark(localCreateCandidate{Root: root, Path: path, ActorID: cfg.AgentID, ActorType: "daemon"})
	}

	if err := runtime.processLocalCreates(ctx); err != nil {
		t.Fatalf("process local creates: %v", err)
	}
	if err := runtime.reconcileDirtyDocuments(ctx); err != nil {
		t.Fatalf("reconcile initial local create updates: %v", err)
	}
	server.assertContents(t, initial)

	edits := map[string]string{
		"docs/a.md":  "alpha edited\n",
		"docs/b.md":  "bravo edited\n",
		"notes/c.md": "charlie edited\n",
	}
	for rel, content := range edits {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("edit %s: %v", rel, err)
		}
		if err := runtime.replica.handleLocalChange(path); err != nil {
			t.Fatalf("handle local change %s: %v", rel, err)
		}
	}
	if err := runtime.reconcileDirtyDocuments(ctx); err != nil {
		t.Fatalf("reconcile edits: %v", err)
	}
	server.assertContents(t, edits)

	for rel := range edits {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove %s: %v", rel, err)
		}
	}
	if err := runtime.replica.reconcileLocalWorkspace(ctx); err != nil {
		t.Fatalf("reconcile local deletes: %v", err)
	}
	if err := runtime.reconcileDirtyDocuments(ctx); err != nil {
		t.Fatalf("reconcile deletes: %v", err)
	}
	server.assertDeleted(t, "docs/a.md", "docs/b.md", "notes/c.md")
}

func TestWorkspaceRuntimeDropsStaleLocalCreateCandidates(t *testing.T) {
	root := t.TempDir()
	desiredPath := filepath.Join(root, "docs", "desired.md")
	trackedPath := filepath.Join(root, "docs", "tracked.md")
	for _, path := range []string{desiredPath, trackedPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("local bytes\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	var createAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{Documents: []*document{{ID: "doc_desired", Path: "docs/desired.md", UpdateID: 1}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/documents":
			createAttempts++
			http.Error(w, "stale local create should have been dropped", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := Config{BackendURL: server.URL, WorkspaceDir: root, AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"), AgentID: "daemon_agent"}
	runtime, err := newWorkspaceRuntime(cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer runtime.replica.watcher.Close()
	defer runtime.closeDocumentSyncs()
	tracked := &trackedFile{DocumentID: "doc_tracked", DocumentPath: "docs/tracked.md", Path: trackedPath, WorkspaceRoot: root}
	runtime.replica.projectedByPath[trackedPath] = tracked
	runtime.replica.projectedByID[tracked.DocumentID] = tracked

	runtime.localCreates.Mark(localCreateCandidate{Root: root, Path: desiredPath, ActorID: cfg.AgentID, ActorType: "daemon"})
	runtime.localCreates.Mark(localCreateCandidate{Root: root, Path: trackedPath, ActorID: cfg.AgentID, ActorType: "daemon"})
	if err := runtime.processLocalCreates(context.Background()); err != nil {
		t.Fatalf("process local creates: %v", err)
	}
	if createAttempts != 0 {
		t.Fatalf("expected no backend creates for stale candidates, got %d", createAttempts)
	}
	if got := runtime.localCreates.Drain(); len(got) != 0 {
		t.Fatalf("expected stale candidates to be dropped, got %#v", got)
	}
}

func TestWorkspaceRuntimeOutboxPostDoesNotBlockPendingRemoteAppend(t *testing.T) {
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
	entry, unlock := cache.lockEntry("doc_1")
	if err := cache.storeOutboxUpdateLocked(entry, "doc_1", "doc.md", outboxUpdateRecord{
		Update:          localUpdate,
		ObservedContent: "base\nlocal\n",
		ObservedState:   crdtStateFromContent("base\nlocal\n"),
		SourcePath:      path,
		ActorID:         "daemon_agent",
		ActorType:       "daemon",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		unlock()
		t.Fatalf("store outbox: %v", err)
	}
	unlock()

	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/documents/doc_1/updates" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		close(postStarted)
		<-releasePost
		writeJSONResponse(w, http.StatusOK, postDocumentUpdateResponse{Accepted: true, Applied: true, UpdateID: 2})
	}))
	defer server.Close()

	runtime := &workspaceRuntime{
		cfg:      Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		client:   server.Client(),
		docCache: cache,
	}
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, WorkspaceRoot: root, cache: cache}
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- runtime.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked})
	}()
	select {
	case <-postStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected outbox POST to start")
	}

	remoteUpdate := updateFromBaseDoc(t, baseDoc, "base\nremote\n", "remote")
	appendDone := make(chan error, 1)
	go func() {
		_, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate)
		appendDone <- err
	}()
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatalf("append pending remote: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(releasePost)
		t.Fatal("pending remote append was blocked by slow outbox POST")
	}
	close(releasePost)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestProductionReconcileTrackedDocumentOnlyRuntimeLoop(t *testing.T) {
	root := "."
	matches := map[string]int{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		count := strings.Count(string(data), "reconcileTrackedDocument(")
		if count > 0 {
			matches[path] = count
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk syncer sources: %v", err)
	}
	if matches["service.go"] != 2 || len(matches) != 1 {
		t.Fatalf("reconcileTrackedDocument must stay owned by the runtime loop; production matches: %#v", matches)
	}
}

func TestWorkspaceRuntimeRunReconcilesLocalCreateEvents(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
		AgentID:            "daemon_agent",
	}

	server := newWorkspaceRuntimeRegressionServer(t)
	defer server.Close()
	cfg.BackendURL = server.URL

	runtime, err := newWorkspaceRuntime(cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runtime.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("runtime run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		runtime.closeDocumentSyncs()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop")
		}
	})

	initial := map[string]string{
		"events/a.md": "alpha\n",
		"events/b.md": "bravo\n",
	}
	for rel, content := range initial {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	server.waitForContents(t, initial, 15*time.Second)
}

type workspaceRuntimeRegressionServer struct {
	*httptest.Server
	mu       sync.Mutex
	nextID   int
	byID     map[string]*regressionDocument
	byPath   map[string]string
	deleted  map[string]struct{}
	requests []string
}

type regressionDocument struct {
	meta *document
	doc  *crdt.Doc
}

func newWorkspaceRuntimeRegressionServer(t *testing.T) *workspaceRuntimeRegressionServer {
	t.Helper()
	regression := &workspaceRuntimeRegressionServer{
		byID:    map[string]*regressionDocument{},
		byPath:  map[string]string{},
		deleted: map[string]struct{}{},
	}
	server := httptest.NewServer(http.HandlerFunc(regression.handle))
	regression.Server = server
	return regression
}

func (s *workspaceRuntimeRegressionServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/documents":
		var req createDocumentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.nextID++
		id := fmt.Sprintf("doc_%d", s.nextID)
		if s.nextID == 1 {
			id = "doc_a"
		} else if s.nextID == 2 {
			id = "doc_b"
		} else if s.nextID == 3 {
			id = "doc_c"
		}
		doc := crdt.New()
		if req.Content != "" {
			text := doc.GetText("content")
			doc.Transact(func(txn *crdt.Transaction) {
				text.Insert(txn, 0, req.Content, nil)
			}, "server-create")
		}
		meta := &document{ID: id, Path: req.Path, UpdateID: 1}
		s.byID[id] = &regressionDocument{meta: meta, doc: doc}
		s.byPath[req.Path] = id
		delete(s.deleted, req.Path)
		writeJSONResponse(w, http.StatusCreated, meta)
		return

	case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
		documents := make([]*document, 0, len(s.byID))
		for _, current := range s.byID {
			copy := *current.meta
			documents = append(documents, &copy)
		}
		writeJSONResponse(w, http.StatusOK, &workspaceResponse{Documents: documents})
		return

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/documents/") && strings.HasSuffix(r.URL.Path, "/updates"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/documents/"), "/updates")
		current := s.byID[id]
		if current == nil {
			http.Error(w, "missing document", http.StatusNotFound)
			return
		}
		update, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := crdt.ApplyUpdateV1(current.doc, update, "server-update"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		current.meta.UpdateID++
		writeJSONResponse(w, http.StatusOK, postDocumentUpdateResponse{Accepted: true, Applied: true, UpdateID: current.meta.UpdateID})
		return

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/documents/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/documents/")
		current := s.byID[id]
		if current == nil {
			http.Error(w, "missing document", http.StatusNotFound)
			return
		}
		delete(s.byID, id)
		delete(s.byPath, current.meta.Path)
		s.deleted[current.meta.Path] = struct{}{}
		writeJSONResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}

	http.Error(w, "unexpected request", http.StatusNotFound)
}

func (s *workspaceRuntimeRegressionServer) assertContents(t *testing.T, want map[string]string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, content := range want {
		id := s.byPath[path]
		if id == "" {
			t.Fatalf("missing backend document for %s", path)
		}
		current := s.byID[id]
		if current == nil {
			t.Fatalf("missing backend document id %s for %s", id, path)
		}
		if got := current.doc.GetText("content").ToString(); got != content {
			t.Fatalf("backend content for %s = %q, want %q", path, got, content)
		}
	}
}

func (s *workspaceRuntimeRegressionServer) waitForContents(t *testing.T, want map[string]string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]string
	for time.Now().Before(deadline) {
		last = s.contents()
		matches := true
		for path, content := range want {
			if last[path] != content {
				matches = false
				break
			}
		}
		if matches {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("backend contents did not converge: got %#v want %#v", last, want)
}

func (s *workspaceRuntimeRegressionServer) contents() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]string{}
	for path, id := range s.byPath {
		current := s.byID[id]
		if current == nil || current.doc == nil {
			continue
		}
		result[path] = current.doc.GetText("content").ToString()
	}
	return result
}

func (s *workspaceRuntimeRegressionServer) assertDeleted(t *testing.T, paths ...string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range paths {
		if _, ok := s.deleted[path]; !ok {
			t.Fatalf("expected %s to be deleted; deleted=%#v", path, s.deleted)
		}
		if id := s.byPath[path]; id != "" {
			t.Fatalf("expected %s to be removed from byPath, still mapped to %s", path, id)
		}
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
