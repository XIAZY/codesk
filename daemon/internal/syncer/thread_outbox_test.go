package syncer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
