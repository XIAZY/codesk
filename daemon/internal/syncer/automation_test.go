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
	factory := newFakeAppServerFactory()
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
			default:
				t.Fatalf("unexpected box: %q", r.URL.Query().Get("box"))
				return nil, nil
			}
		}),
	}
	workspace := &workspaceResponse{
		Documents: []*document{{ID: "doc_spec", Path: "docs/spec.md"}},
		Agents:    []*agent{{ID: "agent_1", Handle: "reviewer", Name: "Reviewer", Role: "Review docs", Kind: "codex"}},
	}

	if err := service.driveAgentAutomation(context.Background(), workspace); err != nil {
		t.Fatalf("drive agent automation: %v", err)
	}
	app := factory.only(t)
	if len(app.turnStarts) != 1 {
		t.Fatalf("expected one notification turn, got %d", len(app.turnStarts))
	}
	if !strings.Contains(app.turnStarts[0].prompt, "You have new items in your notification center.") {
		t.Fatalf("unexpected prompt: %q", app.turnStarts[0].prompt)
	}
	if !strings.Contains(app.turnStarts[0].prompt, "docs/spec.md") {
		t.Fatalf("expected document summary in prompt, got %q", app.turnStarts[0].prompt)
	}
}

func TestBuildNotificationPromptIsSummaryOnly(t *testing.T) {
	prompt := buildNotificationPrompt(
		&agent{Handle: "reviewer", Role: "Review docs"},
		[]*agentEvent{{ID: "evt_1", Type: "thread.mentioned", Box: "for_me", Summary: "Please review this section"}},
		[]*agentEvent{{ID: "evt_2", Type: "document.updated", Box: "general", Summary: "docs/spec.md changed"}},
		&workspaceResponse{Threads: []*thread{{ID: "thread_1", Title: "Need review"}}},
	)

	for _, fragment := range []string{
		"You have new items in your notification center.",
		"For-me inbox:",
		"General inbox:",
		"Use the notification center tools if you want details",
		"Review docs",
	} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("expected prompt to contain %q, got %q", fragment, prompt)
		}
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
		&workspaceResponse{
			Documents: []*document{{ID: "doc_log", Path: "codex-agent.log"}},
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
		&workspaceResponse{
			Documents: []*document{{ID: "doc_log", Path: "codex-agent.log"}},
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

func TestToolGatewayCreateThreadResolvesPathQuoteToRelativeAnchors(t *testing.T) {
	service := newToolGatewayTestService(&agent{ID: "agent_1", Handle: "reviewer", Kind: "codex"}, "token_123")
	cache, err := newDocumentCache(t.TempDir())
	if err != nil {
		t.Fatalf("new document cache: %v", err)
	}
	service.docCache = cache
	service.agentReplicas = map[string]*managedReplica{}
	service.latestWorkspace = &workspaceResponse{
		Documents: []*document{{ID: "doc_spec", Path: "docs/spec.md", UpdateID: 1}},
	}
	doc := crdt.New(crdt.WithClientID(771))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "intro\nrepeat target\nother line\n", nil)
	})
	if err := cache.storeDoc("doc_spec", "docs/spec.md", 1, doc); err != nil {
		t.Fatalf("store cached doc: %v", err)
	}

	var seen map[string]any
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/threads" {
				t.Fatalf("unexpected backend request: %s %s", r.Method, r.URL.String())
			}
			if got := r.URL.Query().Get("actor"); got != "agent_1" {
				t.Fatalf("unexpected actor: %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			if seen["documentId"] != "doc_spec" {
				t.Fatalf("unexpected documentId: %#v", seen["documentId"])
			}
			if pathValue, ok := seen["path"]; ok && pathValue != "" {
				t.Fatalf("gateway should not forward path to backend, got %#v", seen["path"])
			}
			if seen["relativeStart"] == "" || seen["relativeEnd"] == "" {
				t.Fatalf("expected generated relative anchors, got %#v", seen)
			}
			if seen["kind"] != "text-range" || seen["excerpt"] != "target" {
				t.Fatalf("unexpected canonical anchor metadata: %#v", seen)
			}
			if _, ok := seen["line"]; ok {
				t.Fatalf("line is helper input and should not be forwarded: %#v", seen)
			}
			if _, ok := seen["start"]; ok {
				t.Fatalf("start is helper input and should not be forwarded: %#v", seen)
			}
			if _, ok := seen["end"]; ok {
				t.Fatalf("end is helper input and should not be forwarded: %#v", seen)
			}
			return jsonResponse(t, http.StatusCreated, toolThreadMutationResponse{
				Thread:  &thread{ID: "thread_1"},
				Message: &threadMessage{ID: "message_1", ThreadID: "thread_1"},
			}), nil
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
	if seen["relativeStart"] == seen["relativeEnd"] {
		t.Fatalf("expected distinct range anchors, got %#v", seen)
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
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/documents/by-path" {
				t.Fatalf("unexpected backend request: %s %s", r.Method, r.URL.String())
			}
			if got := r.URL.Query().Get("path"); got != "docs/spec.md" {
				t.Fatalf("unexpected path query: %q", got)
			}
			return jsonResponse(t, http.StatusOK, toolDocumentResponse{
				Document: &document{ID: "doc_spec", Path: "docs/spec.md"},
			}), nil
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

func newAutomationTestService(t *testing.T, factory *fakeAppServerFactory) *Service {
	t.Helper()
	service := &Service{
		cfg: Config{
			BackendURL:         "http://backend.test",
			WorkspaceDir:       filepath.Join(t.TempDir(), "workspace"),
			AgentWorkspaceRoot: filepath.Join(t.TempDir(), "agents"),
			RuntimeDir:         filepath.Join(t.TempDir(), "runtime"),
			AgentID:            "daemon_agent",
		},
	}
	service.sessions = newAgentSessionSupervisor(service.cfg, nil, factory.new)
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
