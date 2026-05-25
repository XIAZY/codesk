package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThreadOutboxPipelineQueuesMaterializesDeliversAndDeletesIntent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\ntarget\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "base\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}

	threadRequests := make(chan backendCreateThreadPayload, 2)
	threadKeys := make(chan string, 2)
	documentUpdates := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/documents/doc_1/updates":
			documentUpdates <- struct{}{}
			writeJSONResponse(w, http.StatusOK, postDocumentUpdateResponse{Accepted: true, Applied: true, UpdateID: 2})
		case r.Method == http.MethodPost && r.URL.Path == "/api/threads":
			var payload backendCreateThreadPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode thread create payload: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if got := r.URL.Query().Get("actor"); got != "agent_1" {
				t.Errorf("unexpected actor query: %q", got)
			}
			threadRequests <- payload
			threadKeys <- r.Header.Get("X-Notty-Idempotency-Key")
			writeJSONResponse(w, http.StatusCreated, toolThreadMutationResponse{Thread: &thread{ID: "thread_1"}})
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	runtime := &workspaceRuntime{
		cfg:            Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		client:         server.Client(),
		docCache:       cache,
		reconcileQueue: newReconcileQueue(),
	}
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	service.primaryRuntime = runtime
	service.latestWorkspace = &workspaceResponse{Documents: []*document{{ID: "doc_1", Path: "doc.md", UpdateID: 1}}}
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatalf("tool path should not call backend directly, got %s %s", r.Method, r.URL.String())
			return nil, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-tools/create-thread", strings.NewReader(`{"path":"doc.md","quote":"target","body":"Please review this."}`))
	req.Header.Set("Authorization", "Bearer token_123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	service.handleCreateThreadTool(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected tool status: %d body=%s", rec.Code, rec.Body.String())
	}
	var response toolThreadMutationResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode tool response: %v", err)
	}
	if !response.Queued || response.IntentID == "" {
		t.Fatalf("expected queued tool response, got %#v", response)
	}

	intents := loadThreadIntentsForTest(t, cache, "doc_1")
	if len(intents) != 1 || intents[0].Status != threadIntentPending || intents[0].IntentID != response.IntentID {
		t.Fatalf("expected one pending intent from tool path, got %#v", intents)
	}
	if dirty := runtime.reconcileQueue.Drain(); len(dirty) != 1 || dirty[0] != "doc_1" {
		t.Fatalf("expected tool path to mark document dirty, got %#v", dirty)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runtime.threadDeliveryLoop(ctx)

	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, WorkspaceRoot: root, cache: cache}
	tracked.setProjectedContent("base\n")
	if err := tracked.storeProjectedBase("base\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}
	tracked.markLocalDirty()
	trackedByDocument := map[string][]*trackedFile{"doc_1": []*trackedFile{tracked}}
	if err := runtime.reconcileDocumentIDsWithTracked(ctx, []string{"doc_1"}, trackedByDocument); err != nil {
		t.Fatalf("first reconcile content outbox: %v", err)
	}
	select {
	case <-documentUpdates:
	case <-time.After(2 * time.Second):
		t.Fatal("expected content outbox POST before thread materialization")
	}

	if err := runtime.reconcileDocumentIDsWithTracked(ctx, []string{"doc_1"}, trackedByDocument); err != nil {
		t.Fatalf("second reconcile materialize thread intent: %v", err)
	}
	var payload backendCreateThreadPayload
	select {
	case payload = <-threadRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("expected delivery worker to POST materialized thread intent")
	}
	key := <-threadKeys
	if key == "" || key != payload.ClientOperationID || key != response.IntentID {
		t.Fatalf("expected stable idempotency key/header/body/intent match, header=%q payload=%#v response=%#v", key, payload, response)
	}
	if payload.DocumentID != "doc_1" || payload.Kind != "text-range" || payload.RelativeStart == "" || payload.RelativeEnd == "" || payload.Excerpt != "target" {
		t.Fatalf("unexpected delivered thread payload: %#v", payload)
	}
	select {
	case extra := <-threadRequests:
		t.Fatalf("expected exactly one thread POST, got extra payload %#v", extra)
	case <-time.After(150 * time.Millisecond):
	}
	waitForNoThreadIntents(t, cache, "doc_1")
}

func TestWorkspaceRuntimeMaterializesThreadIntentBeforePendingRemote(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("target\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	baseDoc := newDocWithText(t, "target\n")
	if err := cache.storeDoc("doc_1", "doc.md", 1, baseDoc); err != nil {
		t.Fatalf("store base doc: %v", err)
	}
	remoteUpdate := updateFromBaseDoc(t, baseDoc, "remote\n", "remote")
	if _, err := cache.appendPendingRemoteUpdate("doc_1", "doc.md", remoteUpdate); err != nil {
		t.Fatalf("append pending remote: %v", err)
	}
	intent := threadOutboxIntent{
		IntentID:       "threadintent_order",
		IdempotencyKey: "threadintent_order",
		DocumentID:     "doc_1",
		DocumentPath:   "doc.md",
		ActorID:        "agent_1",
		ActorType:      "agent",
		Request: createThreadPayload{
			DocumentID: "doc_1",
			Body:       "Please check this.",
			Quote:      "target",
		},
		Status:    threadIntentPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := cache.appendThreadIntent("doc_1", intent); err != nil {
		t.Fatalf("append thread intent: %v", err)
	}
	tracked := &trackedFile{DocumentID: "doc_1", DocumentPath: "doc.md", Path: path, WorkspaceRoot: root, cache: cache}
	tracked.setProjectedContent("target\n")
	if err := tracked.storeProjectedBase("target\n", baseDoc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("store projected base: %v", err)
	}

	runtime := &workspaceRuntime{docCache: cache, reconcileQueue: newReconcileQueue()}
	if err := runtime.reconcileTrackedDocument(context.Background(), "doc_1", []*trackedFile{tracked}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	entry, unlock := cache.lockEntry("doc_1")
	intents, err := cache.loadThreadIntentsLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load thread intents: %v", err)
	}
	if len(intents) != 1 {
		t.Fatalf("expected one intent, got %d", len(intents))
	}
	if intents[0].Status != threadIntentReady || intents[0].Resolved == nil {
		t.Fatalf("expected ready materialized intent, got %#v", intents[0])
	}
	if intents[0].Resolved.RelativeStart == "" || intents[0].Resolved.RelativeEnd == "" || intents[0].Resolved.Excerpt != "target" {
		t.Fatalf("unexpected resolved payload: %#v", intents[0].Resolved)
	}
}

func loadThreadIntentsForTest(t *testing.T, cache *documentCache, documentID string) []threadOutboxIntent {
	t.Helper()
	entry, unlock := cache.lockEntry(documentID)
	intents, err := cache.loadThreadIntentsLocked(entry, documentID)
	unlock()
	if err != nil {
		t.Fatalf("load thread intents: %v", err)
	}
	return intents
}

func waitForNoThreadIntents(t *testing.T, cache *documentCache, documentID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		intents := loadThreadIntentsForTest(t, cache, documentID)
		if len(intents) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected thread intent to be deleted after delivery, still got %#v", intents)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestThreadDeliveryRetriesReadyIntentWithIdempotencyKey(t *testing.T) {
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	intent := threadOutboxIntent{
		IntentID:       "threadintent_send",
		IdempotencyKey: "threadintent_send",
		DocumentID:     "doc_1",
		DocumentPath:   "doc.md",
		ActorID:        "agent_1",
		ActorType:      "agent",
		Status:         threadIntentReady,
		Resolved: &backendCreateThreadPayload{
			DocumentID:        "doc_1",
			ClientOperationID: "threadintent_send",
			Body:              "Please check this.",
			Kind:              "text-range",
			RelativeStart:     "start",
			RelativeEnd:       "end",
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := cache.appendThreadIntent("doc_1", intent); err != nil {
		t.Fatalf("append thread intent: %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/threads" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("X-Notty-Idempotency-Key"); got != "threadintent_send" {
			t.Fatalf("unexpected idempotency key: %q", got)
		}
		if got := r.URL.Query().Get("actor"); got != "agent_1" {
			t.Fatalf("unexpected actor query: %q", got)
		}
		if requests == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, http.StatusCreated, toolThreadMutationResponse{Thread: &thread{ID: "thread_1"}})
	}))
	defer server.Close()

	runtime := &workspaceRuntime{
		cfg:      Config{BackendURL: server.URL, AgentID: "daemon_agent"},
		client:   server.Client(),
		docCache: cache,
	}
	if err := runtime.deliverDueThreadIntents(context.Background()); err != nil {
		t.Fatalf("first delivery pass: %v", err)
	}
	entry, unlock := cache.lockEntry("doc_1")
	intents, err := cache.loadThreadIntentsLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load intent after retryable failure: %v", err)
	}
	if len(intents) != 1 || intents[0].Status != threadIntentReady || intents[0].Attempts != 1 || intents[0].NextAttemptAt.IsZero() {
		t.Fatalf("expected ready retry state, got %#v", intents)
	}
	intents[0].NextAttemptAt = time.Now().Add(-time.Second)
	if err := cache.updateThreadIntent(intents[0]); err != nil {
		t.Fatalf("make intent due: %v", err)
	}
	if err := runtime.deliverDueThreadIntents(context.Background()); err != nil {
		t.Fatalf("second delivery pass: %v", err)
	}
	entry, unlock = cache.lockEntry("doc_1")
	intents, err = cache.loadThreadIntentsLocked(entry, "doc_1")
	unlock()
	if err != nil {
		t.Fatalf("load intent after success: %v", err)
	}
	if len(intents) != 0 {
		t.Fatalf("expected successful intent to be deleted, got %#v", intents)
	}
}
