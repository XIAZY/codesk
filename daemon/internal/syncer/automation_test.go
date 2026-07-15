package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	crdt "notty/internal/ycrdt"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResponse(t *testing.T, status int, payload any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func TestDriveAgentAutomationStartsNotificationTurnFromInbox(t *testing.T) {
	factory := newFakeRuntimeDriver()
	service := newAutomationTestService(t, factory)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/agents/agent_1/inbox" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
			}
			switch r.URL.Query().Get("box") {
			case "for_me":
				return jsonResponse(t, http.StatusOK, toolInboxResponse{
					Items: []*agentEvent{{
						ID:         "evt_1",
						AgentID:    "agent_1",
						Type:       "thread.mentioned",
						Box:        "for_me",
						Status:     "pending",
						DocumentID: "doc_spec",
						ThreadID:   "thread_spec",
						Summary:    "mentioned in spec",
						UpdatedAt:  time.Unix(10, 0).UTC(),
					}},
				}), nil
			case "general":
				return jsonResponse(t, http.StatusOK, toolInboxResponse{}), nil
			case "muted":
				// Turn-assembly count fetch (task #2): the firing turn decorates the prompt with a muted count.
				return jsonResponse(t, http.StatusOK, toolInboxResponse{
					Items: []*agentEvent{
						{ID: "muted_1", AgentID: "agent_1", Type: "document.updated", Box: "muted", Status: "pending"},
						{ID: "muted_2", AgentID: "agent_1", Type: "document.updated", Box: "muted", Status: "pending"},
					},
				}), nil
			default:
				t.Fatalf("unexpected box: %q", r.URL.Query().Get("box"))
				return nil, nil
			}
		}),
	}
	workspace := &workspaceResponse{
		Agents: []*agent{{ID: "agent_1", Handle: "reviewer", Name: "Reviewer", Role: "Review docs", Kind: "codex"}},
	}

	// Reconcile is the desired-state authority that creates the resident session;
	// notifications only deliver into an existing one (findings 29/30).
	if err := service.sessions.ensureSession(context.Background(), workspace.Agents[0]); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := service.driveAgentAutomation(context.Background(), workspace); err != nil {
		t.Fatalf("drive agent automation: %v", err)
	}
	process := factory.only(t)
	starts := process.inputsByKind(RuntimeInputStartTurn)
	if len(starts) != 1 {
		t.Fatalf("expected one notification turn, got %d", len(starts))
	}
	if !strings.Contains(starts[0].Text, "You have new items in your notification center.") {
		t.Fatalf("unexpected prompt: %q", starts[0].Text)
	}
	if !strings.Contains(starts[0].Text, "mentioned in spec") {
		t.Fatalf("expected document summary in prompt, got %q", starts[0].Text)
	}
	// The firing turn carries a count-only pointer to the muted box (2 items), with no muted details.
	if !strings.Contains(starts[0].Text, "Muted inbox: 2 item(s)") {
		t.Fatalf("expected the muted count pointer in the prompt, got %q", starts[0].Text)
	}
	if strings.Contains(starts[0].Text, "muted_1") || strings.Contains(starts[0].Text, "muted_2") {
		t.Fatalf("muted item details must not appear in the prompt (count only), got %q", starts[0].Text)
	}
	if strings.Contains(starts[0].Text, "not subscribed to") {
		t.Fatalf("the muted line must not carry the '(document updates you have not subscribed to)' parenthetical, got %q", starts[0].Text)
	}
}

func TestBuildNotificationPromptIsSummaryOnly(t *testing.T) {
	prompt := buildNotificationPrompt(
		&agent{Handle: "reviewer", Role: "Review docs"},
		[]*agentEvent{{ID: "evt_1", Type: "thread.mentioned", Box: "for_me", Summary: "Please review this section"}},
		[]*agentEvent{{ID: "evt_2", Type: "document.updated", Box: "general", Summary: "docs/spec.md changed"}},
		0,
		&workspaceResponse{Threads: []*thread{{ID: "thread_1", Title: "Need review"}}},
	)

	for _, fragment := range []string{
		"You have new items in your notification center.",
		"For-me inbox:",
		"General inbox:",
		"run notty-agent-tool list-inbox to inspect full inbox, if you need to",
	} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("expected prompt to contain %q, got %q", fragment, prompt)
		}
	}
	// The Role suffix is removed (the role already lives in the session system prompt), and the old
	// notification-center-tools closer is replaced by the single list-inbox closer.
	if strings.Contains(prompt, "Role:") || strings.Contains(prompt, "Review docs") {
		t.Fatalf("notification prompt must not carry the Role block, got %q", prompt)
	}
	if strings.Contains(prompt, "Use the notification center tools") {
		t.Fatalf("notification prompt closer must be the single list-inbox line, got %q", prompt)
	}
	if strings.Contains(prompt, "response policy:") || strings.Contains(prompt, "notty-agent-tool reply-thread") {
		t.Fatalf("notification prompt should not force action/tool details, got %q", prompt)
	}
}

func TestBuildNotificationPromptUsesTriggeringThreadMessage(t *testing.T) {
	prompt := buildNotificationPrompt(
		&agent{ID: "agent_codex", Handle: "codex-agent"},
		[]*agentEvent{{
			ID:              "evt_reply",
			AgentID:         "agent_codex",
			Type:            "thread.replied",
			Box:             "for_me",
			DocumentID:      "doc_log",
			ThreadID:        "thread_log",
			ThreadMessageID: "msg_owner_followup",
			Summary:         "New reply in thread Cursor on line 1",
		}},
		nil,
		0,
		&workspaceResponse{
			Threads: []*thread{{
				ID:         "thread_log",
				DocumentID: "doc_log",
				Title:      "Cursor on line 1",
				Messages: []*threadMessage{
					{ID: "msg_owner_initial", ThreadID: "thread_log", AuthorHandle: "owner", Body: "hello"},
					{ID: "msg_agent_reply", ThreadID: "thread_log", AuthorHandle: "codex-agent", Body: "Yes. Before handling this, I checked my inbox."},
					{ID: "msg_owner_followup", ThreadID: "thread_log", AuthorHandle: "owner", Body: "is it currently empty?"},
				},
			}},
		},
	)

	if !strings.Contains(prompt, "trigger @owner: is it currently empty?") {
		t.Fatalf("expected triggering owner message in prompt, got %q", prompt)
	}
	if strings.Contains(prompt, "trigger @codex-agent") || strings.Contains(prompt, "latest @codex-agent") {
		t.Fatalf("prompt should not summarize the agent's prior reply as the triggering item, got %q", prompt)
	}
}

func TestBuildNotificationPromptDoesNotUseStaleLatestThreadMessageForEvent(t *testing.T) {
	prompt := buildNotificationPrompt(
		&agent{ID: "agent_codex", Handle: "codex-agent"},
		[]*agentEvent{{
			ID:              "evt_reply",
			AgentID:         "agent_codex",
			Type:            "thread.replied",
			Box:             "for_me",
			DocumentID:      "doc_log",
			ThreadID:        "thread_log",
			ThreadMessageID: "msg_owner_followup",
			Summary:         "New reply in thread Cursor on line 1",
			Prompt:          "A new reply was added in thread \"Cursor on line 1\" by @owner: is it currently empty?",
		}},
		nil,
		0,
		&workspaceResponse{
			Threads: []*thread{{
				ID:         "thread_log",
				DocumentID: "doc_log",
				Title:      "Cursor on line 1",
				Messages: []*threadMessage{
					{ID: "msg_agent_reply", ThreadID: "thread_log", AuthorHandle: "codex-agent", Body: "Yes. Before handling this, I checked my inbox."},
				},
			}},
		},
	)

	if !strings.Contains(prompt, "trigger: A new reply was added in thread \"Cursor on line 1\" by @owner: is it currently empty?") {
		t.Fatalf("expected event prompt fallback for missing triggering message, got %q", prompt)
	}
	if strings.Contains(prompt, "latest @codex-agent") || strings.Contains(prompt, "trigger @codex-agent") {
		t.Fatalf("prompt should not use stale latest thread message for a message-scoped event, got %q", prompt)
	}
}

func TestToolGatewayRepliesAsOwningAgent(t *testing.T) {
	var actorSeen string
	var actorTypeSeen string

	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/threads/thread_1/messages" {
				t.Fatalf("unexpected backend request: %s %s", r.Method, r.URL.Path)
			}
			actorSeen = r.URL.Query().Get("actor")
			actorTypeSeen = r.URL.Query().Get("actor_type")
			return jsonResponse(t, http.StatusCreated, toolThreadMutationResponse{
				Thread:  &thread{ID: "thread_1"},
				Message: &threadMessage{ID: "message_1", ThreadID: "thread_1"},
			}), nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-tools/reply-thread", strings.NewReader(`{"threadId":"thread_1","body":"Looks good","kind":"comment"}`))
	req.Header.Set("Authorization", "Bearer token_123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	service.handleReplyThreadTool(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if actorSeen != "agent_1" || actorTypeSeen != "agent" {
		t.Fatalf("unexpected actor attribution: actor=%q actorType=%q", actorSeen, actorTypeSeen)
	}
}

func TestToolGatewayCreateThreadQueuesPathQuoteIntent(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	queue := newReconcileQueue()
	rootID := "doc_root_test"
	if err := cache.storeRootProjectionEntries(rootID, []rootProjectionEntry{{
		EntryID:           "doc_spec",
		ContentDocumentID: "doc_spec",
		DesiredPath:       "docs/spec.md",
		MaterializedPath:  "docs/spec.md",
		Active:            true,
	}}); err != nil {
		t.Fatalf("store root projection: %v", err)
	}
	runtime := &workspaceRuntime{rootDocumentID: rootID, docCache: cache, reconcileQueue: queue}
	service.agentRuntimes = map[string]*managedWorkspaceRuntime{"agent_1": {runtime: runtime}}
	doc := crdt.New(crdt.WithClientID(771))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "intro\nrepeat target\nother line\n", nil)
	})
	if err := cache.storeDoc("doc_spec", "docs/spec.md", 1, doc); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}

	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatalf("create-thread should queue a thread intent, not POST immediately: %s %s", r.Method, r.URL.String())
			return nil, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-tools/create-thread", strings.NewReader(`{"path":"docs/spec.md","line":2,"quote":"target","body":"Please review this."}`))
	req.Header.Set("Authorization", "Bearer token_123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	service.handleCreateThreadTool(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var response toolThreadMutationResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Queued || response.IntentID == "" {
		t.Fatalf("expected queued response with intent id, got %#v", response)
	}
	entry, unlock := cache.lockEntry("doc_spec")
	intents, err := cache.loadThreadIntentsLocked(entry, "doc_spec")
	unlock()
	if err != nil {
		t.Fatalf("load thread intents: %v", err)
	}
	if len(intents) != 1 {
		t.Fatalf("expected one thread intent, got %d", len(intents))
	}
	intent := intents[0]
	if intent.Status != threadIntentPending || intent.IntentID != response.IntentID {
		t.Fatalf("unexpected intent state: %#v", intent)
	}
	if intent.ActorID != "agent_1" || intent.ActorType != "agent" {
		t.Fatalf("unexpected intent actor: %#v", intent)
	}
	if intent.Request.Path != "" || intent.Request.DocumentID != "doc_spec" || intent.Request.Quote != "target" || intent.Request.Line != 2 {
		t.Fatalf("unexpected queued request: %#v", intent.Request)
	}
	dirty := queue.Drain()
	if len(dirty) != 1 || dirty[0] != "doc_spec" {
		t.Fatalf("expected queued document reconcile wake, got %#v", dirty)
	}
}

func TestToolGatewayRejectsUnknownToken(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	req := httptest.NewRequest(http.MethodPost, "/agent-tools/reply-thread", strings.NewReader(`{"threadId":"thread_1","body":"x"}`))
	req.Header.Set("Authorization", "Bearer missing")
	rec := httptest.NewRecorder()

	service.handleReplyThreadTool(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", rec.Code)
	}
}

func TestToolGatewayGetsDocumentByPathAsAuthorizedAgent(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	cache, err := newTestDocumentCache(t, t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	rootID := "doc_root_test"
	if err := cache.storeRootProjectionEntries(rootID, []rootProjectionEntry{{
		EntryID:           "doc_spec",
		ContentDocumentID: "doc_spec",
		DesiredPath:       "docs/original.md",
		MaterializedPath:  "docs/spec.md",
		Active:            true,
	}}); err != nil {
		t.Fatalf("store root projection: %v", err)
	}
	runtime := &workspaceRuntime{
		rootDocumentID: rootID,
		docCache:       cache,
	}
	service.agentRuntimes = map[string]*managedWorkspaceRuntime{"agent_1": {runtime: runtime}}
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatalf("get-document-by-path should use local root projection, not backend: %s %s", r.Method, r.URL.String())
			return nil, nil
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "/agent-tools/get-document-by-path?path=docs%2Fspec.md", nil)
	req.Header.Set("Authorization", "Bearer token_123")
	rec := httptest.NewRecorder()

	service.handleGetDocumentByPathTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"path\":\"docs/spec.md\"") {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}

func TestToolGatewayListsInboxForRequestedBox(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/agents/agent_1/inbox" {
				t.Fatalf("unexpected backend request: %s %s", r.Method, r.URL.String())
			}
			if got := r.URL.Query().Get("box"); got != "general" {
				t.Fatalf("unexpected box query: %q", got)
			}
			if got := r.URL.Query().Get("status"); got != "pending" {
				t.Fatalf("unexpected status query: %q", got)
			}
			return jsonResponse(t, http.StatusOK, toolInboxResponse{
				Items: []*agentEvent{{ID: "docinbox:general:agent_1:doc_spec", AgentID: "agent_1", Box: "general", Type: "document.updated"}},
			}), nil
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "/agent-tools/list-inbox?box=general", nil)
	req.Header.Set("Authorization", "Bearer token_123")
	rec := httptest.NewRecorder()

	service.handleListInboxTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"box\":\"general\"") {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}

// Guard for the bare-list-inbox=all-boxes change: the automation loop must keep fetching the PUSHED boxes
// explicitly (for_me + general) and never issue a bare all-box fetch — a bare fetch would pull muted items
// into the wake path and re-create the ambient wakes the feature removed. This keeps the ergonomic default
// (bare CLI list = everything) from leaking into the turn-scheduling path.
func TestAutomationNeverIssuesBareAllBoxInboxFetch(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	var boxes []string
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/agents/agent_1/inbox" {
				t.Fatalf("unexpected backend path: %s", r.URL.Path)
			}
			box := strings.TrimSpace(r.URL.Query().Get("box"))
			if box == "" {
				t.Fatalf("automation must never issue a bare all-box inbox fetch — that would pull muted items into the wake path")
			}
			boxes = append(boxes, box)
			return jsonResponse(t, http.StatusOK, toolInboxResponse{Items: nil}), nil
		}),
	}

	if _, _, err := service.fetchPendingInboxForAgent(context.Background(), "agent_1"); err != nil {
		t.Fatalf("fetch pending inbox: %v", err)
	}
	got := map[string]bool{}
	for _, b := range boxes {
		got[b] = true
	}
	if !got["for_me"] || !got["general"] {
		t.Fatalf("automation must fetch for_me and general explicitly, got %v", boxes)
	}
	if got["muted"] {
		t.Fatalf("automation must never fetch the muted box")
	}
}

func TestToolGatewaySubscribeUnsubscribeListDocuments(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			const path = "/api/agents/agent_1/document-subscriptions"
			switch {
			case r.Method == http.MethodPost && r.URL.Path == path:
				return jsonResponse(t, http.StatusOK, backendDocumentSubscriptionsResponse{DocumentIDs: []string{"doc_spec"}}), nil
			case r.Method == http.MethodGet && r.URL.Path == path:
				return jsonResponse(t, http.StatusOK, backendDocumentSubscriptionsResponse{DocumentIDs: []string{"doc_spec"}}), nil
			case r.Method == http.MethodDelete && r.URL.Path == path+"/doc_spec":
				return jsonResponse(t, http.StatusOK, backendDocumentSubscriptionsResponse{DocumentIDs: []string{}}), nil
			default:
				t.Fatalf("unexpected backend request: %s %s", r.Method, r.URL.String())
				return nil, nil
			}
		}),
	}

	call := func(handler http.HandlerFunc, method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer token_123")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// The gateway enriches the backend's bare {documentIds} into {documents:[{id,path}]}. This test service
	// has no workspace runtime, so the path index is empty — the HONESTY case: the doc still appears with an
	// id and an empty path (the CLI renders that as an id-only line) rather than erroring or vanishing.
	//
	// Subscribe proxies POST and returns the enriched post-change list.
	if rec := call(service.handleSubscribeDocumentTool, http.MethodPost, "/agent-tools/subscribe-document?document_id=doc_spec"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"documents\":[{\"id\":\"doc_spec\",\"path\":\"\"}]") {
		t.Fatalf("subscribe: status %d body=%s", rec.Code, rec.Body.String())
	}
	// List proxies GET.
	if rec := call(service.handleListDocumentSubscriptionsTool, http.MethodGet, "/agent-tools/list-subscriptions"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"id\":\"doc_spec\"") {
		t.Fatalf("list: status %d body=%s", rec.Code, rec.Body.String())
	}
	// Unsubscribe proxies DELETE (to the {documentID} path) and returns the emptied list.
	if rec := call(service.handleUnsubscribeDocumentTool, http.MethodPost, "/agent-tools/unsubscribe-document?document_id=doc_spec"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"documents\":[]") {
		t.Fatalf("unsubscribe: status %d body=%s", rec.Code, rec.Body.String())
	}
	// An unknown tool token is rejected before any backend call.
	if rec := (func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/agent-tools/subscribe-document?document_id=doc_spec", nil)
		req.Header.Set("Authorization", "Bearer bogus")
		rec := httptest.NewRecorder()
		service.handleSubscribeDocumentTool(rec, req)
		return rec
	}()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: want 401, got %d", rec.Code)
	}
}

func TestBuildSubscribedDocumentsDegradesUnknownIDToIDOnly(t *testing.T) {
	pathByID := map[string]string{"doc_spec": "specs/api.md"}
	got := buildSubscribedDocuments([]string{"doc_spec", "doc_gone"}, pathByID)
	if len(got) != 2 {
		t.Fatalf("want 2 documents, got %d: %+v", len(got), got)
	}
	// Known id carries its path from the projection.
	if got[0] != (subscribedDocument{ID: "doc_spec", Path: "specs/api.md"}) {
		t.Fatalf("known id: %+v", got[0])
	}
	// Unknown id (no projection entry) is kept with an empty path, not dropped — the honesty case.
	if got[1] != (subscribedDocument{ID: "doc_gone", Path: ""}) {
		t.Fatalf("unknown id: %+v", got[1])
	}
}

func TestToolGatewayDiffDocumentUsesVersionQuery(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/agents/agent_1/documents/doc_spec/diff" {
				t.Fatalf("unexpected backend request: %s %s", r.Method, r.URL.String())
			}
			if got := r.URL.Query().Get("from"); got != "12" {
				t.Fatalf("unexpected from query: %q", got)
			}
			if got := r.URL.Query().Get("to"); got != "head" {
				t.Fatalf("unexpected to query: %q", got)
			}
			return jsonResponse(t, http.StatusOK, toolDocumentDiffResponse{
				Diff: &documentDiff{DocumentID: "doc_spec", FromUpdateID: 12, ToUpdateID: 15, Unified: "+new\n"},
			}), nil
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "/agent-tools/diff-document?document_id=doc_spec&from=12&to=head", nil)
	req.Header.Set("Authorization", "Bearer token_123")
	rec := httptest.NewRecorder()

	service.handleDiffDocumentTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"fromUpdateId\":12") {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}

func newAutomationTestService(t *testing.T, factory *fakeRuntimeDriver) *Service {
	t.Helper()
	service := &Service{
		cfg: Config{
			BackendURL:         "http://backend.test",
			DataDir:            t.TempDir(),
			WorkspaceDir:       filepath.Join(t.TempDir(), "workspace"),
			AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
			AgentID:            "daemon_agent",
		},
	}
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, newFakeRuntimeRegistry(factory))
	return service
}

func newToolGatewayTestService(currentAgent *agent, token string) *Service {
	sessions := &agentSessionSupervisor{sessions: map[string]*managedAgentSession{}}
	sessions.sessions[currentAgent.ID] = &managedAgentSession{
		agent:     currentAgent,
		toolToken: token,
		state:     "idle",
	}
	return &Service{
		cfg:      Config{BackendURL: "http://backend.test"},
		sessions: sessions,
		client:   http.DefaultClient,
	}
}
