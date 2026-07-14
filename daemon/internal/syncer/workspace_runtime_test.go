package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
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

func TestWorkspaceRuntimeStartupRecoveryQueuesDurableSQLiteWork(t *testing.T) {
	root := t.TempDir()
	cache, err := newTestDocumentCache(t, root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	baseDoc := newDocWithText(t, "base")
	if err := cache.storeDoc("doc_incoming", "incoming.md", 1, baseDoc); err != nil {
		t.Fatalf("store incoming base: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, baseDoc, "base\nremote", "remote")
	if appended, err := cache.appendPendingRemoteUpdate("doc_incoming", "incoming.md", remoteUpdate); err != nil {
		t.Fatalf("append pending incoming: %v", err)
	} else if !appended {
		t.Fatal("expected pending incoming to append")
	}

	if err := cache.storeDoc("doc_outbox", "outbox.md", 1, baseDoc); err != nil {
		t.Fatalf("store outbox base: %v", err)
	}
	localUpdate := updateFromBaseDoc(t, baseDoc, "base\nlocal", "local")
	entry, unlock := cache.lockEntry("doc_outbox")
	if err := cache.storeOutboxUpdateLocked(entry, "doc_outbox", "outbox.md", outboxUpdateRecord{
		Update:          localUpdate,
		ObservedContent: "base\nlocal",
		ObservedState:   crdtStateFromContent("base\nlocal"),
		SourcePath:      filepath.Join(root, "outbox.md"),
		ActorID:         "daemon_agent",
		ActorType:       "daemon",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		unlock()
		t.Fatalf("store content outbox: %v", err)
	}
	unlock()

	if err := cache.appendThreadIntent("doc_thread_pending", threadOutboxIntent{
		IntentID:       "intent_pending",
		IdempotencyKey: "intent_pending",
		DocumentID:     "doc_thread_pending",
		DocumentPath:   "pending.md",
		ActorID:        "agent_1",
		ActorType:      "agent",
		Request: createThreadPayload{
			DocumentID: "doc_thread_pending",
			Body:       "Please check this.",
			Quote:      "target",
		},
		Status:    threadIntentPending,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append pending thread intent: %v", err)
	}
	if err := cache.appendThreadIntent("doc_thread_ready", threadOutboxIntent{
		IntentID:       "intent_ready",
		IdempotencyKey: "intent_ready",
		DocumentID:     "doc_thread_ready",
		DocumentPath:   "ready.md",
		ActorID:        "agent_1",
		ActorType:      "agent",
		Request: createThreadPayload{
			DocumentID:    "doc_thread_ready",
			Body:          "Please check this.",
			RelativeStart: "start",
			RelativeEnd:   "end",
		},
		Resolved: &backendCreateThreadPayload{
			DocumentID:        "doc_thread_ready",
			ClientOperationID: "intent_ready",
			Body:              "Please check this.",
			Kind:              "text-range",
			RelativeStart:     "start",
			RelativeEnd:       "end",
		},
		Status:    threadIntentReady,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append ready thread intent: %v", err)
	}
	if err := cache.storeLocalNamespaceIntent(newLocalCreateIntent("local-create.md", "daemon_agent", "daemon", "local-create-hash", time.Now().UnixNano())); err != nil {
		t.Fatalf("store local namespace intent: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("close cache: %v", err)
	}

	reopened, err := newTestDocumentCache(t, root)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	queue := newReconcileQueue()
	runtime := &workspaceRuntime{
		docCache:           reopened,
		reconcileQueue:     queue,
		threadDeliveryWake: make(chan struct{}, 1),
	}
	runtime.enqueueStartupStoreWork()

	dirty := queue.Drain()
	for _, documentID := range []string{"doc_incoming", "doc_outbox", "doc_thread_pending"} {
		if !containsString(dirty, documentID) {
			t.Fatalf("expected startup recovery to queue %s, got %#v", documentID, dirty)
		}
	}
	if !containsString(dirty, localCreateReconcileWake) {
		t.Fatalf("expected startup recovery to queue pending local create work, got %#v", dirty)
	}
	if containsString(dirty, "doc_thread_ready") {
		t.Fatalf("ready thread delivery must not dirty the document, got %#v", dirty)
	}
	select {
	case <-runtime.threadDeliveryWake:
	default:
		t.Fatal("expected ready thread intent to wake delivery on startup")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestWorkspaceRuntimeSyncDBsArePerLocalWorkspace(t *testing.T) {
	cfg := Config{WorkspaceDir: filepath.Join(t.TempDir(), "primary"), AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"), AgentID: "daemon_agent"}

	primary, err := newTestWorkspaceRuntime(t, cfg, http.DefaultClient, cfg.WorkspaceDir, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new primary runtime: %v", err)
	}

	agentRoot := filepath.Join(cfg.AgentWorkspaceRoot, "agent_1")
	agent, err := newTestWorkspaceRuntime(t, cfg, http.DefaultClient, agentRoot, "agent_1", "agent")
	if err != nil {
		t.Fatalf("new agent runtime: %v", err)
	}

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

func TestWorkspaceReplicaConstructionDoesNotStartUndrainedWatcher(t *testing.T) {
	replica := newWorkspaceReplica(Config{}, t.TempDir(), "daemon_agent", "daemon", nil, nil)

	if replica.watcher != nil {
		t.Fatal("workspace watcher must be created by Run, which owns its event consumer")
	}
	if err := replica.ensureDirectoryWatches(); err != nil {
		t.Fatalf("refresh watches before Run: %v", err)
	}
}

func TestWorkspaceRuntimeLocalCreateWakesReconcileQueue(t *testing.T) {
	runtime := &workspaceRuntime{
		localCreates:     newLocalCreateQueue(),
		reconcileQueue:   newReconcileQueue(),
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

	runtime, err := newTestWorkspaceRuntime(t, cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	server.installDocumentUpdateHook(t, runtime)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	server.assertRootDeleted(t, "docs/a.md", "docs/b.md", "notes/c.md")
}

func TestWorkspaceRuntimeProjectsRemoteClientLifecycleToFilesystem(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
		AgentID:            "daemon_agent",
	}
	runtime, err := newTestWorkspaceRuntime(t, cfg, http.DefaultClient, root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := context.Background()

	client := newRemoteLifecycleClientForTest("doc_root_remote")
	workspace := &workspaceResponse{RootDocumentID: client.rootDocumentID}
	if err := runtime.applyWorkspace(ctx, workspace); err != nil {
		t.Fatalf("apply initial workspace: %v", err)
	}
	reconcileRuntimeUntilIdle(t, ctx, runtime)

	const documentID = "doc_remote_1"
	sendRemoteClientUpdateToDaemon(t, runtime, client.rootDocumentID, client.upsertRootFile(t, documentID, "docs/client-created.md"))
	reconcileRuntimeDocumentIDs(t, ctx, runtime, []string{client.rootDocumentID})

	sendRemoteClientUpdateToDaemon(t, runtime, documentID, client.replaceContent(t, documentID, "client create\n"))
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	assertWorkspaceFileContent(t, root, "docs/client-created.md", "client create\n")

	sendRemoteClientUpdateToDaemon(t, runtime, client.rootDocumentID, client.upsertRootFile(t, documentID, "docs/client-renamed.md"))
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	assertWorkspaceFileMissing(t, root, "docs/client-created.md")
	assertWorkspaceFileContent(t, root, "docs/client-renamed.md", "client create\n")

	sendRemoteClientUpdateToDaemon(t, runtime, documentID, client.replaceContent(t, documentID, "client edit after rename\n"))
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	assertWorkspaceFileContent(t, root, "docs/client-renamed.md", "client edit after rename\n")

	sendRemoteClientUpdateToDaemon(t, runtime, client.rootDocumentID, client.tombstoneRootFile(t, documentID, "docs/client-renamed.md"))
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	assertWorkspaceFileMissing(t, root, "docs/client-renamed.md")
	assertRuntimeUntracked(t, runtime, documentID)
}

func TestReconcileStateCleanRemoteRenameMovesWithoutContentOutbox(t *testing.T) {
	root := t.TempDir()
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	baseState := baseDoc.EncodeStateAsUpdate()
	if err := cache.storeDoc("doc_clean_rename", "docs/new.md", 1, baseDoc); err != nil {
		t.Fatalf("store doc: %v", err)
	}
	oldPath := filepath.Join(root, "docs", "old.md")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir old path: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write old path: %v", err)
	}
	tracked := newTrackedFileForStateTest(t, root, cache, "doc_clean_rename", "docs/new.md", oldPath, "base\n", baseState)
	runtime := &workspaceRuntime{
		cfg:      Config{AgentID: "daemon_agent"},
		docCache: cache,
		sendDocumentUpdate: func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
			t.Fatalf("clean rename should not send content outbox for %s", documentID)
			return nil
		},
	}

	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_clean_rename", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile clean rename: %v", err)
	}
	assertWorkspaceFileMissing(t, root, "docs/old.md")
	assertWorkspaceFileContent(t, root, "docs/new.md", "base\n")
	if tracked.Path != filepath.Join(root, "docs", "new.md") {
		t.Fatalf("tracked path = %s, want new path", tracked.Path)
	}
	assertSQLiteOutboxEmpty(t, cache, "doc_clean_rename")
}

func TestReconcileStateDirtyRemoteRenameSendsLocalEditBeforeMovingPath(t *testing.T) {
	root := t.TempDir()
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	baseState := baseDoc.EncodeStateAsUpdate()
	if err := cache.storeDoc("doc_rename", "docs/new.md", 1, baseDoc); err != nil {
		t.Fatalf("store doc: %v", err)
	}
	oldPath := filepath.Join(root, "docs", "old.md")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir old path: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("base\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write dirty old path: %v", err)
	}

	tracked := newTrackedFileForStateTest(t, root, cache, "doc_rename", "docs/new.md", oldPath, "base\n", baseState)
	tracked.markLocalDirty()
	var sent []outboxUpdateRecord
	runtime := &workspaceRuntime{
		cfg:      Config{AgentID: "daemon_agent"},
		docCache: cache,
		sendDocumentUpdate: func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
			if documentID != "doc_rename" {
				t.Fatalf("sent document ID = %s", documentID)
			}
			sent = append(sent, record)
			return nil
		},
	}

	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_rename", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile dirty rename content: %v", err)
	}
	if len(sent) != 1 || sent[0].ObservedContent != "base\nlocal\n" {
		t.Fatalf("expected one local edit send before move, got %#v", sent)
	}
	assertWorkspaceFileContent(t, root, "docs/old.md", "base\nlocal\n")
	assertWorkspaceFileMissing(t, root, "docs/new.md")

	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_rename", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile dirty rename path: %v", err)
	}
	assertWorkspaceFileMissing(t, root, "docs/old.md")
	assertWorkspaceFileContent(t, root, "docs/new.md", "base\nlocal\n")
	if tracked.Path != filepath.Join(root, "docs", "new.md") {
		t.Fatalf("tracked path = %s, want new path", tracked.Path)
	}
	assertSQLiteOutboxEmpty(t, cache, "doc_rename")
}

func TestReconcileStateRemoteRenameWithDestinationCollisionPreservesCollidingBytes(t *testing.T) {
	root := t.TempDir()
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	baseState := baseDoc.EncodeStateAsUpdate()
	if err := cache.storeDoc("doc_collision", "docs/new.md", 1, baseDoc); err != nil {
		t.Fatalf("store doc: %v", err)
	}
	oldPath := filepath.Join(root, "docs", "old.md")
	newPath := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir old path: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write old path: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("collision\n"), 0o644); err != nil {
		t.Fatalf("write colliding path: %v", err)
	}
	tracked := newTrackedFileForStateTest(t, root, cache, "doc_collision", "docs/new.md", oldPath, "base\n", baseState)
	runtime := &workspaceRuntime{cfg: Config{AgentID: "daemon_agent"}, docCache: cache}

	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_collision", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile collision rename: %v", err)
	}
	assertWorkspaceFileMissing(t, root, "docs/old.md")
	assertWorkspaceFileContent(t, root, "docs/new.md", "base\n")
	assertRecoveredContent(t, root, "doc_collision_collision", "collision\n")
}

func TestReconcileStateRemoteDeleteCleanAndDirtyOutputs(t *testing.T) {
	for _, tc := range []struct {
		name              string
		localContent      string
		expectFileDeleted bool
		expectArchived    bool
	}{
		{name: "clean", localContent: "base\n", expectFileDeleted: true},
		{name: "dirty", localContent: "base\nlocal\n", expectFileDeleted: true, expectArchived: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cache, err := newTestDocumentCache(t, t.TempDir())
			if err != nil {
				t.Fatalf("new cache: %v", err)
			}
			baseDoc := newDocWithText(t, "base\n")
			baseState := baseDoc.EncodeStateAsUpdate()
			if err := cache.storeDoc("doc_delete_"+tc.name, "docs/delete.md", 1, baseDoc); err != nil {
				t.Fatalf("store doc: %v", err)
			}
			path := filepath.Join(root, "docs", "delete.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir path: %v", err)
			}
			if err := os.WriteFile(path, []byte(tc.localContent), 0o644); err != nil {
				t.Fatalf("write path: %v", err)
			}
			documentID := "doc_delete_" + tc.name
			tracked := newTrackedFileForStateTest(t, root, cache, documentID, "docs/delete.md", path, "base\n", baseState)
			runtime := &workspaceRuntime{
				cfg:            Config{AgentID: "daemon_agent"},
				docCache:       cache,
				reconcileQueue: newReconcileQueue(),
			}
			replica := newWorkspaceReplica(Config{}, root, "daemon_agent", "daemon", runtime.markDocumentDirty, runtime.markLocalCreate)
			runtime.replica = replica
			addTrackedForStateTest(runtime, tracked)

			if err := runtime.projectRootRemovedEntry(rootProjectionEntry{ContentDocumentID: documentID}); err != nil {
				t.Fatalf("project root removed entry: %v", err)
			}
			if tc.expectFileDeleted {
				assertWorkspaceFileMissing(t, root, "docs/delete.md")
			}
			assertRuntimeUntracked(t, runtime, documentID)
			if tc.expectArchived {
				assertRecoveredContent(t, root, documentID, tc.localContent)
			}
		})
	}
}

func TestReconcileStateDuplicateMaterializedPathsRouteEditsByDocumentID(t *testing.T) {
	root := t.TempDir()
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	docA := newDocWithText(t, "alpha\n")
	docB := newDocWithText(t, "bravo\n")
	if err := cache.storeDoc("doc_a", "docs/same.md", 1, docA); err != nil {
		t.Fatalf("store doc_a: %v", err)
	}
	if err := cache.storeDoc("doc_b", "docs/same (doc_b).md", 1, docB); err != nil {
		t.Fatalf("store doc_b: %v", err)
	}
	pathA := filepath.Join(root, "docs", "same.md")
	pathB := filepath.Join(root, "docs", "same (doc_b).md")
	for path, content := range map[string]string{
		pathA: "alpha edit\n",
		pathB: "bravo edit\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	trackedA := newTrackedFileForStateTest(t, root, cache, "doc_a", "docs/same.md", pathA, "alpha\n", docA.EncodeStateAsUpdate())
	trackedB := newTrackedFileForStateTest(t, root, cache, "doc_b", "docs/same (doc_b).md", pathB, "bravo\n", docB.EncodeStateAsUpdate())
	trackedA.markLocalDirty()
	trackedB.markLocalDirty()
	sent := map[string]string{}
	runtime := &workspaceRuntime{
		cfg:      Config{AgentID: "daemon_agent"},
		docCache: cache,
		sendDocumentUpdate: func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
			sent[documentID] = record.ObservedContent
			return nil
		},
	}

	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_a", []*trackedFile{trackedA}); err != nil {
		t.Fatalf("reconcile doc_a: %v", err)
	}
	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_b", []*trackedFile{trackedB}); err != nil {
		t.Fatalf("reconcile doc_b: %v", err)
	}
	if got := sent["doc_a"]; got != "alpha edit\n" {
		t.Fatalf("doc_a sent content = %q", got)
	}
	if got := sent["doc_b"]; got != "bravo edit\n" {
		t.Fatalf("doc_b sent content = %q", got)
	}
	assertSQLiteOutboxEmpty(t, cache, "doc_a")
	assertSQLiteOutboxEmpty(t, cache, "doc_b")
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
	rootID := "doc_root_test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
			writeJSONResponse(w, http.StatusOK, &workspaceResponse{RootDocumentID: rootID})
		case r.Method == http.MethodPost && r.URL.Path == "/api/documents":
			createAttempts++
			http.Error(w, "stale local create should have been dropped", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := Config{BackendURL: server.URL, WorkspaceDir: root, AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"), AgentID: "daemon_agent"}
	runtime, err := newTestWorkspaceRuntime(t, cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	seedRoot := crdt.New()
	seedRootMap := seedRoot.GetMap(rootMapName)
	if _, err := seedRoot.Update(func(txn *crdt.Transaction) error {
		entriesMap, err := seedRootMap.SetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		return setRootFileEntry(txn, entriesMap, rootEntry{
			EntryID:           rootEntryIDForDocument("doc_desired"),
			ContentDocumentID: "doc_desired",
			Name:              "docs/desired.md",
		})
	}, "seed-root"); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := runtime.docCache.storeDoc(rootID, rootDocumentPath, 1, seedRoot); err != nil {
		t.Fatalf("store root: %v", err)
	}
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
	cache, err := newTestDocumentCache(t, t.TempDir())
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

	runtime := &workspaceRuntime{
		cfg:      Config{BackendURL: "http://backend.test", AgentID: "daemon_agent"},
		client:   http.DefaultClient,
		docCache: cache,
	}
	runtime.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		if documentID != "doc_1" {
			t.Fatalf("unexpected websocket document update: %s", documentID)
		}
		close(postStarted)
		<-releasePost
		return nil
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
		t.Fatal("expected websocket outbox send to start")
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
		t.Fatal("pending remote append was blocked by slow websocket send")
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

func TestProductionDocumentSyncUsesWorkspaceMuxSocketOnly(t *testing.T) {
	forbidden := []string{
		"/ws/documents/",
		"managedDocumentSync",
		"documentSyncs",
	}
	matches := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
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
		text := string(data)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				matches[path] = append(matches[path], token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk syncer sources: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("daemon document sync must use the workspace mux socket only; production matches: %#v", matches)
	}
}

func TestProductionDocumentNamespaceUsesRootProjectionOnly(t *testing.T) {
	forbidden := []string{
		"workspace.Documents",
		"workspaceDocuments",
		"ensureRootEntriesForVisibleDocuments",
		"desiredDocumentPaths",
	}
	matches := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
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
		text := string(data)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				matches[path] = append(matches[path], token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk syncer sources: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("daemon document namespace must come from root projection only; production matches: %#v", matches)
	}
}

func TestProductionProjectionDoesNotReplayHistoryForProjectionSeq(t *testing.T) {
	forbidden := "find" + "Projected" + "Seq"
	matches := map[string]int{}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
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
		if count := strings.Count(string(data), forbidden); count > 0 {
			matches[path] = count
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk syncer sources: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("projection must carry explicit seq instead of replaying history; production matches: %#v", matches)
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

	runtime, err := newTestWorkspaceRuntime(t, cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	server.installDocumentUpdateHook(t, runtime)
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

func TestWorkspaceRuntimeFilesystemLifecycleRecordsSQLiteAndDaemonCalls(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		WorkspaceDir:       root,
		AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
		AgentID:            "daemon_agent",
	}

	server := newWorkspaceRuntimeRegressionServer(t)
	defer server.Close()
	cfg.BackendURL = server.URL

	runtime, err := newTestWorkspaceRuntime(t, cfg, server.Client(), root, cfg.AgentID, "daemon")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	server.installDocumentUpdateHook(t, runtime)
	ctx := context.Background()
	emptyRoot := crdt.New()
	if err := runtime.docCache.storeDoc(server.rootDocumentID, rootDocumentPath, 1, emptyRoot); err != nil {
		t.Fatalf("store empty root: %v", err)
	}

	initialPath := filepath.Join(root, "docs", "lifecycle.md")
	if err := os.MkdirAll(filepath.Dir(initialPath), 0o755); err != nil {
		t.Fatalf("mkdir initial path: %v", err)
	}
	if err := os.WriteFile(initialPath, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	if err := runtime.replica.reconcileLocalWorkspace(ctx); err != nil {
		t.Fatalf("detect local create: %v", err)
	}
	if err := runtime.processLocalCreates(ctx); err != nil {
		t.Fatalf("process local create: %v", err)
	}
	reconcileRuntimeUntilIdle(t, ctx, runtime)

	documentID := server.documentIDForPath(t, "docs/lifecycle.md")
	if _, err := uuid.Parse(documentID); err != nil {
		t.Fatalf("local filesystem create document ID should be a bare UUID, got %q: %v", documentID, err)
	}
	server.assertContents(t, map[string]string{"docs/lifecycle.md": "alpha\n"})
	server.assertRootEntry(t, documentID, "docs/lifecycle.md", false)
	server.assertSyncUpdateCount(t, server.rootDocumentID, 1)
	server.assertSyncUpdateCount(t, documentID, 1)
	assertSQLiteDocumentContent(t, runtime.docCache, documentID, "alpha\n")
	assertSQLiteDocumentRowPath(t, runtime.docCache, documentID, "docs/lifecycle.md")
	assertSQLiteRootEntry(t, runtime.docCache, server.rootDocumentID, documentID, "docs/lifecycle.md", false)
	assertSQLiteRootProjectionEntry(t, runtime.docCache, server.rootDocumentID, documentID, "docs/lifecycle.md", true)
	assertSQLiteOutboxEmpty(t, runtime.docCache, documentID)
	assertSQLiteOutboxEmpty(t, runtime.docCache, server.rootDocumentID)

	editedContent := "HEAD\nalpha\nomega\n"
	if err := os.WriteFile(initialPath, []byte(editedContent), 0o644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}
	if err := runtime.replica.handleLocalChange(initialPath); err != nil {
		t.Fatalf("handle local edit: %v", err)
	}
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	server.assertContents(t, map[string]string{"docs/lifecycle.md": editedContent})
	server.assertSyncUpdateCount(t, documentID, 2)
	assertSQLiteDocumentContent(t, runtime.docCache, documentID, editedContent)
	assertSQLiteDocumentRowPath(t, runtime.docCache, documentID, "docs/lifecycle.md")
	assertSQLiteRootProjectionEntry(t, runtime.docCache, server.rootDocumentID, documentID, "docs/lifecycle.md", true)
	assertSQLiteOutboxEmpty(t, runtime.docCache, documentID)

	movedPath := filepath.Join(root, "renamed", "lifecycle.md")
	if err := os.MkdirAll(filepath.Dir(movedPath), 0o755); err != nil {
		t.Fatalf("mkdir moved path: %v", err)
	}
	if err := os.Rename(initialPath, movedPath); err != nil {
		t.Fatalf("move file: %v", err)
	}
	moveTime := time.Now()
	if err := runtime.replica.handleWatcherEvent(fsnotify.Event{Name: initialPath, Op: fsnotify.Rename}, moveTime); err != nil {
		t.Fatalf("handle old-path rename event: %v", err)
	}
	if err := runtime.replica.handleWatcherEvent(fsnotify.Event{Name: movedPath, Op: fsnotify.Create}, moveTime.Add(time.Millisecond)); err != nil {
		t.Fatalf("handle new-path create event: %v", err)
	}
	if pending, err := runtime.replica.drainPathChanges(ctx, moveTime.Add(time.Millisecond)); err != nil {
		t.Fatalf("drain local move path changes: %v", err)
	} else if pending {
		t.Fatal("matched local move should not leave pending path changes")
	}
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	server.assertRootEntry(t, documentID, "renamed/lifecycle.md", false)
	server.assertSyncUpdateCount(t, server.rootDocumentID, 2)
	assertSQLiteDocumentContent(t, runtime.docCache, documentID, editedContent)
	assertSQLiteDocumentRowPath(t, runtime.docCache, documentID, "renamed/lifecycle.md")
	assertSQLiteRootEntry(t, runtime.docCache, server.rootDocumentID, documentID, "renamed/lifecycle.md", false)
	assertSQLiteRootProjectionEntry(t, runtime.docCache, server.rootDocumentID, documentID, "renamed/lifecycle.md", true)
	assertWorkspaceFileMissing(t, root, "docs/lifecycle.md")
	assertWorkspaceFileContent(t, root, "renamed/lifecycle.md", editedContent)

	editAfterMove := "alpha\nomega\nTAIL\n"
	if err := os.WriteFile(movedPath, []byte(editAfterMove), 0o644); err != nil {
		t.Fatalf("write moved edit: %v", err)
	}
	if err := runtime.replica.handleLocalChange(movedPath); err != nil {
		t.Fatalf("handle moved edit: %v", err)
	}
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	server.assertContents(t, map[string]string{"renamed/lifecycle.md": editAfterMove})
	server.assertSyncUpdateCount(t, documentID, 3)
	assertSQLiteDocumentContent(t, runtime.docCache, documentID, editAfterMove)
	assertSQLiteDocumentRowPath(t, runtime.docCache, documentID, "renamed/lifecycle.md")
	assertSQLiteRootProjectionEntry(t, runtime.docCache, server.rootDocumentID, documentID, "renamed/lifecycle.md", true)

	if err := os.Remove(movedPath); err != nil {
		t.Fatalf("remove moved file: %v", err)
	}
	removeTime := time.Now()
	if err := runtime.replica.handleWatcherEvent(fsnotify.Event{Name: movedPath, Op: fsnotify.Remove}, removeTime); err != nil {
		t.Fatalf("handle local delete event: %v", err)
	}
	if pending, err := runtime.replica.drainPathChanges(ctx, removeTime.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain local delete path changes: %v", err)
	} else if pending {
		t.Fatal("expired local delete should not leave pending path changes")
	}
	reconcileRuntimeUntilIdle(t, ctx, runtime)
	server.assertRootEntry(t, documentID, "renamed/lifecycle.md", true)
	server.assertSyncUpdateCount(t, server.rootDocumentID, 3)
	assertSQLiteDocumentContent(t, runtime.docCache, documentID, editAfterMove)
	assertSQLiteRootEntry(t, runtime.docCache, server.rootDocumentID, documentID, "renamed/lifecycle.md", true)
	assertSQLiteRootProjectionEntry(t, runtime.docCache, server.rootDocumentID, documentID, "renamed/lifecycle.md", false)
	assertSQLiteOutboxEmpty(t, runtime.docCache, server.rootDocumentID)
	assertRuntimeUntracked(t, runtime, documentID)
}

type workspaceRuntimeRegressionServer struct {
	*httptest.Server
	mu             sync.Mutex
	nextID         int
	rootDocumentID string
	rootDoc        *crdt.Doc
	byID           map[string]*regressionDocument
	byPath         map[string]string
	deleted        map[string]struct{}
	requests       []string
	syncUpdates    []regressionSyncUpdate
}

type regressionDocument struct {
	meta *document
	doc  *crdt.Doc
}

type regressionSyncUpdate struct {
	DocumentID string
	ActorID    string
	ActorType  string
}

func newWorkspaceRuntimeRegressionServer(t *testing.T) *workspaceRuntimeRegressionServer {
	t.Helper()
	regression := &workspaceRuntimeRegressionServer{
		rootDocumentID: "doc_root_test",
		rootDoc:        crdt.New(),
		byID:           map[string]*regressionDocument{},
		byPath:         map[string]string{},
		deleted:        map[string]struct{}{},
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
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := strings.TrimSpace(req["documentId"])
		if id == "" {
			http.Error(w, "document id is required", http.StatusBadRequest)
			return
		}
		if _, err := uuid.Parse(id); err != nil {
			http.Error(w, "document id must be a bare uuid", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req["clientOperationId"]) == "" {
			http.Error(w, "client operation id is required", http.StatusBadRequest)
			return
		}
		if current := s.byID[id]; current != nil {
			writeJSONResponse(w, http.StatusOK, current.meta)
			return
		}
		doc := crdt.New()
		meta := &document{ID: id, UpdateID: 1}
		s.byID[id] = &regressionDocument{meta: meta, doc: doc}
		writeJSONResponse(w, http.StatusCreated, meta)
		return

	case r.Method == http.MethodGet && r.URL.Path == "/api/workspace":
		writeJSONResponse(w, http.StatusOK, &workspaceResponse{RootDocumentID: s.rootDocumentID})
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

func (s *workspaceRuntimeRegressionServer) installDocumentUpdateHook(t *testing.T, runtime *workspaceRuntime) {
	t.Helper()
	runtime.sendDocumentUpdate = func(ctx context.Context, documentID string, record outboxUpdateRecord) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.syncUpdates = append(s.syncUpdates, regressionSyncUpdate{
			DocumentID: documentID,
			ActorID:    record.ActorID,
			ActorType:  record.ActorType,
		})
		if documentID == s.rootDocumentID {
			if err := crdt.ApplyUpdateV1(s.rootDoc, record.Update, "server-root-update"); err != nil {
				return err
			}
			entries, err := decodeRootEntries(s.rootDoc)
			if err != nil {
				return err
			}
			s.byPath = map[string]string{}
			s.deleted = map[string]struct{}{}
			for _, entry := range entries {
				path := entry.desiredPath()
				if path == "" {
					continue
				}
				if entry.Deleted {
					s.deleted[path] = struct{}{}
					continue
				}
				s.byPath[path] = entry.ContentDocumentID
				if current := s.byID[entry.ContentDocumentID]; current != nil {
					current.meta.Path = path
				}
			}
		} else {
			current := s.byID[documentID]
			if current == nil {
				t.Fatalf("missing backend document %s", documentID)
			}
			if err := crdt.ApplyUpdateV1(current.doc, record.Update, "server-update"); err != nil {
				return err
			}
			current.meta.UpdateID++
		}
		return nil
	}
}

func (s *workspaceRuntimeRegressionServer) documentIDForPath(t *testing.T, path string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	documentID := s.byPath[path]
	if documentID == "" {
		entries, _ := decodeRootEntries(s.rootDoc)
		t.Fatalf("missing backend document path %s; byPath=%#v requests=%#v syncUpdates=%#v rootEntries=%#v", path, s.byPath, s.requests, s.syncUpdates, entries)
	}
	return documentID
}

func (s *workspaceRuntimeRegressionServer) assertRootEntry(t *testing.T, documentID, path string, deleted bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := decodeRootEntries(s.rootDoc)
	if err != nil {
		t.Fatalf("decode root entries: %v", err)
	}
	entry, ok := entries[rootEntryIDForDocument(documentID)]
	if !ok {
		t.Fatalf("missing root entry for %s: %#v", documentID, entries)
	}
	if got := entry.desiredPath(); got != path || entry.Deleted != deleted {
		t.Fatalf("root entry for %s = path %q deleted %v, want path %q deleted %v", documentID, got, entry.Deleted, path, deleted)
	}
}

func (s *workspaceRuntimeRegressionServer) assertSyncUpdateCount(t *testing.T, documentID string, want int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	got := 0
	for _, update := range s.syncUpdates {
		if update.DocumentID == documentID {
			got++
		}
	}
	if got != want {
		t.Fatalf("sync update count for %s = %d, want %d; updates=%#v", documentID, got, want, s.syncUpdates)
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

func (s *workspaceRuntimeRegressionServer) assertRootDeleted(t *testing.T, paths ...string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := decodeRootEntries(s.rootDoc)
	if err != nil {
		t.Fatalf("decode root entries: %v", err)
	}
	for _, path := range paths {
		var found bool
		for _, entry := range entries {
			if entry.desiredPath() != path {
				continue
			}
			found = true
			if !entry.Deleted {
				t.Fatalf("expected root entry for %s to be tombstoned, got %#v", path, entry)
			}
		}
		if !found {
			t.Fatalf("expected tombstoned root entry for %s, entries=%#v", path, entries)
		}
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type remoteLifecycleClientForTest struct {
	rootDocumentID string
	rootDoc        *crdt.Doc
	contentDocs    map[string]*crdt.Doc
}

func newRemoteLifecycleClientForTest(rootDocumentID string) *remoteLifecycleClientForTest {
	return &remoteLifecycleClientForTest{
		rootDocumentID: rootDocumentID,
		rootDoc:        crdt.New(),
		contentDocs:    map[string]*crdt.Doc{},
	}
}

func (c *remoteLifecycleClientForTest) upsertRootFile(t *testing.T, documentID, path string) []byte {
	t.Helper()
	return c.updateRoot(t, func(txn *crdt.Transaction, entriesMap *crdt.YMap) error {
		return setRootFileEntry(txn, entriesMap, rootEntry{
			EntryID:           rootEntryIDForDocument(documentID),
			Kind:              rootEntryKindFile,
			ContentDocumentID: documentID,
			Name:              path,
			Deleted:           false,
		})
	})
}

func (c *remoteLifecycleClientForTest) tombstoneRootFile(t *testing.T, documentID, path string) []byte {
	t.Helper()
	return c.updateRoot(t, func(txn *crdt.Transaction, entriesMap *crdt.YMap) error {
		return setRootFileEntry(txn, entriesMap, rootEntry{
			EntryID:           rootEntryIDForDocument(documentID),
			Kind:              rootEntryKindFile,
			ContentDocumentID: documentID,
			Name:              path,
			Deleted:           true,
		})
	})
}

func (c *remoteLifecycleClientForTest) updateRoot(t *testing.T, mutate func(*crdt.Transaction, *crdt.YMap) error) []byte {
	t.Helper()
	rootMap := c.rootDoc.GetMap(rootMapName)
	update, err := c.rootDoc.Update(func(txn *crdt.Transaction) error {
		entriesMap, ok, err := rootMap.GetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if !ok {
			entriesMap, err = rootMap.SetMap(txn, rootEntriesMapName)
			if err != nil {
				return err
			}
		}
		return mutate(txn, entriesMap)
	}, "remote-client-root")
	if err != nil {
		t.Fatalf("build remote root update: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected non-empty remote root update")
	}
	return update
}

func (c *remoteLifecycleClientForTest) replaceContent(t *testing.T, documentID, content string) []byte {
	t.Helper()
	doc := c.contentDocs[documentID]
	if doc == nil {
		doc = crdt.New()
		c.contentDocs[documentID] = doc
	}
	text := doc.GetText("content")
	update, err := doc.Update(func(txn *crdt.Transaction) error {
		if length := text.LenInTxn(txn); length > 0 {
			if err := text.DeleteRange(txn, 0, length); err != nil {
				return err
			}
		}
		if content == "" {
			return nil
		}
		return text.InsertValue(txn, 0, content)
	}, "remote-client-content")
	if err != nil {
		t.Fatalf("build remote content update: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected non-empty remote content update")
	}
	return update
}

func sendRemoteClientUpdateToDaemon(t *testing.T, runtime *workspaceRuntime, documentID string, update []byte) {
	t.Helper()
	if err := runtime.handleDocumentSyncMessage(documentID, yproto.BuildSyncUpdate(update)); err != nil {
		t.Fatalf("handle remote client update for %s: %v", documentID, err)
	}
}

func reconcileRuntimeUntilIdle(t *testing.T, ctx context.Context, runtime *workspaceRuntime) {
	t.Helper()
	for attempt := 0; attempt < 12; attempt++ {
		documentIDs := runtime.reconcileQueue.Drain()
		if len(documentIDs) == 0 {
			return
		}
		reconcileRuntimeDocumentIDs(t, ctx, runtime, documentIDs)
	}
	t.Fatalf("runtime reconcile queue did not drain; remaining=%#v", runtime.reconcileQueue.Drain())
}

func reconcileRuntimeDocumentIDs(t *testing.T, ctx context.Context, runtime *workspaceRuntime, documentIDs []string) {
	t.Helper()
	if err := runtime.reconcileDocumentIDs(ctx, documentIDs); err != nil {
		t.Fatalf("reconcile documents %v: %v", documentIDs, err)
	}
}

func newTrackedFileForStateTest(t *testing.T, root string, cache *documentCache, documentID, documentPath, absolutePath, baseContent string, baseState []byte) *trackedFile {
	t.Helper()
	tracked := &trackedFile{
		DocumentID:    documentID,
		DocumentPath:  documentPath,
		Path:          absolutePath,
		WorkspaceRoot: root,
		FS:            NewWorkspaceFS(root),
		cache:         cache,
	}
	tracked.setProjectedContent(baseContent)
	if err := tracked.storeProjectedBase(baseContent, baseState); err != nil {
		t.Fatalf("store projected base for %s: %v", documentID, err)
	}
	return tracked
}

func addTrackedForStateTest(runtime *workspaceRuntime, tracked *trackedFile) {
	runtime.replica.mu.Lock()
	defer runtime.replica.mu.Unlock()
	tracked.Owner = runtime.replica
	tracked.FS = runtime.replica.fs
	runtime.replica.projectedByID[tracked.DocumentID] = tracked
	runtime.replica.projectedByPath[tracked.Path] = tracked
}

func assertWorkspaceFileContent(t *testing.T, root, rel, want string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read workspace file %s: %v", rel, err)
	}
	if got := string(data); got != want {
		t.Fatalf("workspace file %s = %q, want %q", rel, got, want)
	}
}

func assertWorkspaceFileMissing(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("workspace file %s still exists", rel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat workspace file %s: %v", rel, err)
	}
}

func assertRecoveredContent(t *testing.T, root, reason, want string) {
	t.Helper()
	recoveredRoot := filepath.Join(root, ".notty", "recovered", safeDocumentCacheName(reason))
	var matches []string
	err := filepath.WalkDir(recoveredRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(data) == want {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read recovered files under %s: %v", recoveredRoot, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no recovered file under %s had content %q", recoveredRoot, want)
	}
}

func assertRuntimeUntracked(t *testing.T, runtime *workspaceRuntime, documentID string) {
	t.Helper()
	runtime.replica.mu.Lock()
	defer runtime.replica.mu.Unlock()
	if tracked := runtime.replica.projectedByID[documentID]; tracked != nil {
		t.Fatalf("document %s remained tracked after root tombstone: %#v", documentID, tracked)
	}
}

func assertSQLiteDocumentContent(t *testing.T, cache *documentCache, documentID, want string) {
	t.Helper()
	doc, _, _, err := cache.loadBaseDoc(documentID, "")
	if err != nil {
		t.Fatalf("load sqlite document %s: %v", documentID, err)
	}
	defer doc.Close()
	if got := doc.GetText("content").ToString(); got != want {
		t.Fatalf("sqlite content for %s = %q, want %q", documentID, got, want)
	}
}

func assertSQLiteDocumentRowPath(t *testing.T, cache *documentCache, documentID, want string) {
	t.Helper()
	var got string
	if err := cache.db.QueryRow(`select path from documents where document_id = ?`, documentID).Scan(&got); err != nil {
		t.Fatalf("load sqlite document row %s: %v", documentID, err)
	}
	if got != want {
		t.Fatalf("sqlite document path for %s = %q, want %q", documentID, got, want)
	}
}

func assertSQLiteRootEntry(t *testing.T, cache *documentCache, rootDocumentID, documentID, path string, deleted bool) {
	t.Helper()
	rootDoc, _, _, err := cache.loadBaseDoc(rootDocumentID, rootDocumentPath)
	if err != nil {
		t.Fatalf("load sqlite root document: %v", err)
	}
	defer rootDoc.Close()
	entries, err := decodeRootEntries(rootDoc)
	if err != nil {
		t.Fatalf("decode sqlite root entries: %v", err)
	}
	entry, ok := entries[rootEntryIDForDocument(documentID)]
	if !ok {
		t.Fatalf("missing sqlite root entry for %s: %#v", documentID, entries)
	}
	if got := entry.desiredPath(); got != path || entry.Deleted != deleted {
		t.Fatalf("sqlite root entry for %s = path %q deleted %v, want path %q deleted %v", documentID, got, entry.Deleted, path, deleted)
	}
}

func assertSQLiteRootProjectionEntry(t *testing.T, cache *documentCache, rootDocumentID, documentID, path string, active bool) {
	t.Helper()
	entries, err := cache.loadRootProjectionEntries(rootDocumentID)
	if err != nil {
		t.Fatalf("load sqlite root projection entries: %v", err)
	}
	entry, ok := entries[rootEntryIDForDocument(documentID)]
	if !ok {
		t.Fatalf("missing sqlite root projection for %s: %#v", documentID, entries)
	}
	if entry.Active != active {
		t.Fatalf("sqlite root projection active for %s = %v, want %v; entry=%#v", documentID, entry.Active, active, entry)
	}
	if entry.DesiredPath != path {
		t.Fatalf("sqlite root projection desired path for %s = %q, want %q; entry=%#v", documentID, entry.DesiredPath, path, entry)
	}
	if active && entry.MaterializedPath != path {
		t.Fatalf("sqlite root projection materialized path for %s = %q, want %q; entry=%#v", documentID, entry.MaterializedPath, path, entry)
	}
}

func assertSQLiteOutboxEmpty(t *testing.T, cache *documentCache, documentID string) {
	t.Helper()
	var count int
	if err := cache.db.QueryRow(`select count(*) from content_outbox where document_id = ?`, documentID).Scan(&count); err != nil {
		t.Fatalf("count sqlite outbox for %s: %v", documentID, err)
	}
	if count != 0 {
		t.Fatalf("sqlite outbox for %s has %d rows, want empty", documentID, count)
	}
}
