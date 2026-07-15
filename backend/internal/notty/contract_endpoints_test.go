package notty

import (
	"net/http"
	"testing"
)

// Suite 2 of the consolidated test corpus: the endpoint×state golden matrix. It generalizes the two
// GET /workspace rows in contract_workspace_test.go to the rest of the read surface — each read endpoint,
// driven on live Postgres against a canonical state (empty / populated), gets its response canonicalized
// and pinned to a committed golden, with the A1 never-null invariant checked on every collection-bearing
// row. A backend response-shape change becomes a red, reviewable diff in the PR that caused it, and the
// frontend imports the same goldens as fixtures. New rows only add a builder + a table entry.
//
// State builders reuse the real handlers (never direct store writes), so the goldens are exactly what the
// wire serves. The populated builder returns the created entity ids so per-entity GETs (a single thread,
// a document's threads) can be driven against a known-good reference graph.

// contractFixture carries the ids a populated workspace exposes, for driving per-entity read endpoints.
type contractFixture struct {
	OwnerToken  string
	WorkspaceID string
	DocumentID  string
	DaemonID    string
	DaemonToken string
	AgentID     string
	ThreadID    string
}

// buildContractPopulatedFixture seeds the same healthy, everything-present shape as buildPopulatedWorkspace
// (online daemon, one agent, an idle session, an agent-authored thread on the root document, daemon
// presence) but returns the entity ids the per-entity rows need. Distinct emails from the workspace-row
// builder so the two can run in the same package without colliding.
func buildContractPopulatedFixture(t *testing.T, router http.Handler) contractFixture {
	t.Helper()
	owner := authTestRegister(t, router, "contract-endpoints-owner@example.com", "owner-pass", "Contract Endpoints Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Contract Endpoints Workspace")
	document := authTestCreateDocument(t, router, owner.Token, ws.ID, "docs/endpoints.md", "# Endpoints\n")

	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Contract daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, ws.ID, daemon.Token)

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons/"+daemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "contract-agent",
		Name:   "Contract Agent",
		Role:   "Exercises the endpoint matrix",
		Kind:   "codex",
	}, http.StatusCreated, &agent)

	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+ws.ID+"/agents/"+agent.ID+"/session", daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "contract-session", CurrentActivity: "Idle"}, http.StatusOK)

	var thread struct {
		Thread Thread `json:"thread"`
	}
	authTestJSONWithHeaders(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/threads", daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Contract thread",
		Body:       "A populated-state thread authored by the agent.",
		Excerpt:    "contract",
	}, http.StatusCreated, &thread)

	var presence Presence
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/presence", daemon.Token, UpsertPresenceRequest{
		ActorID:    daemon.Daemon.ID,
		ActorType:  "daemon",
		DocumentID: document.ID,
		Activity:   "viewing",
	}, http.StatusOK, &presence)

	return contractFixture{
		OwnerToken:  owner.Token,
		WorkspaceID: ws.ID,
		DocumentID:  document.ID,
		DaemonID:    daemon.Daemon.ID,
		DaemonToken: daemon.Token,
		AgentID:     agent.ID,
		ThreadID:    thread.Thread.ID,
	}
}

// buildContractEmptyFixture seeds a just-created workspace with a single empty document (no threads),
// so the empty-state rows exercise the "present but empty" shape the frontend renders for a fresh space.
func buildContractEmptyFixture(t *testing.T, router http.Handler) contractFixture {
	t.Helper()
	owner := authTestRegister(t, router, "contract-endpoints-empty-owner@example.com", "owner-pass", "Contract Endpoints Empty Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Contract Endpoints Empty Workspace")
	document := authTestCreateDocument(t, router, owner.Token, ws.ID, "docs/empty.md", "")
	return contractFixture{OwnerToken: owner.Token, WorkspaceID: ws.ID, DocumentID: document.ID}
}

// assertEndpointGolden drives one read endpoint, asserts 200, checks the A1 never-null invariant on the
// named collection fields, canonicalizes the response, and pins/diffs the golden. collectionKeys may be
// empty for single-object responses (a thread, an account) that carry no top-level collection.
func assertEndpointGolden(t *testing.T, router http.Handler, method, path, token, goldenName string, collectionKeys []string) {
	t.Helper()
	recorder := authTestRequest(t, router, method, path, token, nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s: got %d body=%s", method, path, recorder.Code, recorder.Body.String())
	}
	raw := recorder.Body.Bytes()
	if len(collectionKeys) > 0 {
		assertNoNullCollections(t, raw, collectionKeys)
	}
	canonical, err := canonicalizeContractJSON(raw)
	if err != nil {
		t.Fatalf("canonicalize %s %s: %v", method, path, err)
	}
	assertContractGolden(t, goldenName, canonical)
}

func TestContractEndpointsPopulatedState(t *testing.T) {
	_, router := newAuthTestServer(t)
	fx := buildContractPopulatedFixture(t, router)
	ws := "/api/workspaces/" + fx.WorkspaceID

	assertEndpointGolden(t, router, http.MethodGet, "/api/auth/me", fx.OwnerToken, "auth_me_populated", []string{"workspaces"})
	assertEndpointGolden(t, router, http.MethodGet, "/api/workspaces", fx.OwnerToken, "workspaces_list_populated", []string{"workspaces"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/members", fx.OwnerToken, "members_populated", []string{"members"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/daemons", fx.OwnerToken, "daemons_populated", []string{"daemons"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/documents/"+fx.DocumentID+"/threads", fx.OwnerToken, "document_threads_populated", []string{"threads"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/threads/"+fx.ThreadID, fx.OwnerToken, "thread_get_populated", nil)
}

func TestContractEndpointsEmptyState(t *testing.T) {
	_, router := newAuthTestServer(t)
	fx := buildContractEmptyFixture(t, router)
	ws := "/api/workspaces/" + fx.WorkspaceID

	assertEndpointGolden(t, router, http.MethodGet, "/api/auth/me", fx.OwnerToken, "auth_me_empty", []string{"workspaces"})
	assertEndpointGolden(t, router, http.MethodGet, "/api/workspaces", fx.OwnerToken, "workspaces_list_empty", []string{"workspaces"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/members", fx.OwnerToken, "members_empty", []string{"members"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/daemons", fx.OwnerToken, "daemons_empty", []string{"daemons"})
	assertEndpointGolden(t, router, http.MethodGet, ws+"/documents/"+fx.DocumentID+"/threads", fx.OwnerToken, "document_threads_empty", []string{"threads"})
}

// TestContractDocumentSubscriptionsGolden pins the document-subscriptions response shape (task #2) across
// empty and populated states. All three endpoints return {documentIds:[…]}; goldening the GET after a
// subscribe covers the shape subscribe/unsubscribe also return.
func TestContractDocumentSubscriptionsGolden(t *testing.T) {
	_, router := newAuthTestServer(t)
	fx := buildContractPopulatedFixture(t, router)
	subs := "/api/workspaces/" + fx.WorkspaceID + "/agents/" + fx.AgentID + "/document-subscriptions"

	// Empty: no subscriptions yet.
	assertEndpointGolden(t, router, http.MethodGet, subs, fx.OwnerToken, "document_subscriptions_empty", []string{"documentIds"})

	// Populated: subscribe (owner passes the shared boundary), then pin the list shape.
	authTestStatus(t, router, http.MethodPost, subs, fx.OwnerToken, SubscribeDocumentRequest{DocumentID: fx.DocumentID}, http.StatusOK)
	assertEndpointGolden(t, router, http.MethodGet, subs, fx.OwnerToken, "document_subscriptions_populated", []string{"documentIds"})
}

// TestContractDocumentSubscribersGolden pins the doc→subscribers read shape (task #4, Participants panel)
// across empty and populated states, and demonstrates the one-write-path property: subscribing through the
// existing agent endpoint is what surfaces the agent in this read — there is no second write path.
func TestContractDocumentSubscribersGolden(t *testing.T) {
	_, router := newAuthTestServer(t)
	fx := buildContractPopulatedFixture(t, router)
	ws := "/api/workspaces/" + fx.WorkspaceID
	subscribers := ws + "/documents/" + fx.DocumentID + "/subscribers"

	// Empty: the document has no subscribers yet.
	assertEndpointGolden(t, router, http.MethodGet, subscribers, fx.OwnerToken, "document_subscribers_empty", []string{"agents"})

	// Populated: subscribe the agent through the existing agent endpoint (owner passes the shared boundary),
	// then pin the subscriber list — the same write the CLI/daemon uses, surfaced doc→agents.
	subs := ws + "/agents/" + fx.AgentID + "/document-subscriptions"
	authTestStatus(t, router, http.MethodPost, subs, fx.OwnerToken, SubscribeDocumentRequest{DocumentID: fx.DocumentID}, http.StatusOK)
	assertEndpointGolden(t, router, http.MethodGet, subscribers, fx.OwnerToken, "document_subscribers_populated", []string{"agents"})
}

// TestDocumentSubscribersReadAuthzAndBehavior pins the read's authz (workspace-member only, unknown doc 404)
// and the one-write-path round trip: an owner-initiated subscribe surfaces the agent with its lean
// projection, an unsubscribe removes it — same rows the CLI writes, no divergent write path.
func TestDocumentSubscribersReadAuthzAndBehavior(t *testing.T) {
	_, router := newAuthTestServer(t)
	fx := buildContractPopulatedFixture(t, router)
	ws := "/api/workspaces/" + fx.WorkspaceID
	subscribers := ws + "/documents/" + fx.DocumentID + "/subscribers"

	// A non-member never reaches the handler (the requireWorkspace subtree gate).
	outsider := authTestRegister(t, router, "document-subscribers-outsider@example.com", "owner-pass", "Subscribers Outsider")
	authTestStatus(t, router, http.MethodGet, subscribers, outsider.Token, nil, http.StatusForbidden)

	// Unknown document → 404, like the sibling document reads.
	authTestStatus(t, router, http.MethodGet, ws+"/documents/00000000-0000-4000-8000-000000000000/subscribers", fx.OwnerToken, nil, http.StatusNotFound)

	// One write path: subscribing through the existing agent endpoint surfaces the agent here, with the lean
	// id/handle/name/kind projection and nothing more.
	subs := ws + "/agents/" + fx.AgentID + "/document-subscriptions"
	authTestStatus(t, router, http.MethodPost, subs, fx.OwnerToken, SubscribeDocumentRequest{DocumentID: fx.DocumentID}, http.StatusOK)

	var populated struct {
		Agents []DocumentSubscriberAgent `json:"agents"`
	}
	authTestJSON(t, router, http.MethodGet, subscribers, fx.OwnerToken, nil, http.StatusOK, &populated)
	if len(populated.Agents) != 1 {
		t.Fatalf("subscribe-on-behalf must surface exactly one subscriber, got %#v", populated.Agents)
	}
	if got := populated.Agents[0]; got.ID != fx.AgentID || got.Handle != "contract-agent" || got.Name != "Contract Agent" || got.Kind != "codex" {
		t.Fatalf("subscriber projection mismatch: %#v", got)
	}

	// Unsubscribe through the same endpoint removes it from the read.
	authTestStatus(t, router, http.MethodDelete, subs+"/"+fx.DocumentID, fx.OwnerToken, nil, http.StatusOK)
	var emptied struct {
		Agents []DocumentSubscriberAgent `json:"agents"`
	}
	authTestJSON(t, router, http.MethodGet, subscribers, fx.OwnerToken, nil, http.StatusOK, &emptied)
	if len(emptied.Agents) != 0 {
		t.Fatalf("unsubscribe must remove the agent from the read, got %#v", emptied.Agents)
	}
}
