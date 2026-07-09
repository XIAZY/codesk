package notty

import (
	"net/http"
	"testing"
)

// Task #2 commit (b): the document-subscription endpoints. Subscribe/unsubscribe are idempotent, list
// round-trips, and the agent-ownership boundary (4.2) is enforced — a daemon acting as its own agent cannot
// touch another agent's subscriptions. Driven the product way: daemon token + X-Notty-Acting-Agent-ID.
func TestDocumentSubscriptionEndpoints(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "subs-owner@example.com", "owner-pass", "Subs Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Subs Workspace")
	wsAPI := "/api/workspaces/" + ws.ID
	doc := authTestCreateDocument(t, router, owner.Token, ws.ID, "docs/subs.md", "start\n")

	var daemonA CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, wsAPI+"/daemons", owner.Token, CreateDaemonRequest{Name: "Daemon A"}, http.StatusCreated, &daemonA)
	authTestReportCodexRuntime(t, router, ws.ID, daemonA.Token)
	var agentA Agent
	authTestJSON(t, router, http.MethodPost, wsAPI+"/daemons/"+daemonA.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "agent-a", Name: "Agent A", Role: "owned by daemon A", Kind: "codex",
	}, http.StatusCreated, &agentA)

	var daemonB CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, wsAPI+"/daemons", owner.Token, CreateDaemonRequest{Name: "Daemon B"}, http.StatusCreated, &daemonB)
	authTestReportCodexRuntime(t, router, ws.ID, daemonB.Token)
	var agentB Agent
	authTestJSON(t, router, http.MethodPost, wsAPI+"/daemons/"+daemonB.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "agent-b", Name: "Agent B", Role: "owned by daemon B", Kind: "codex",
	}, http.StatusCreated, &agentB)

	hdrA := map[string]string{"X-Notty-Acting-Agent-ID": agentA.ID}
	subPath := wsAPI + "/agents/" + agentA.ID + "/document-subscriptions"

	// Subscribe is idempotent: twice → 200 both times.
	authTestStatusWithHeaders(t, router, http.MethodPost, subPath, daemonA.Token, hdrA, SubscribeDocumentRequest{DocumentID: doc.ID}, http.StatusOK)
	authTestStatusWithHeaders(t, router, http.MethodPost, subPath, daemonA.Token, hdrA, SubscribeDocumentRequest{DocumentID: doc.ID}, http.StatusOK)

	// List round-trips the subscription.
	var listResp struct {
		DocumentIDs []string `json:"documentIds"`
	}
	authTestJSONWithHeaders(t, router, http.MethodGet, subPath, daemonA.Token, hdrA, nil, http.StatusOK, &listResp)
	if len(listResp.DocumentIDs) != 1 || listResp.DocumentIDs[0] != doc.ID {
		t.Fatalf("expected the subscribed document in the list, got %#v", listResp.DocumentIDs)
	}

	// Boundary (4.2): daemon A, acting as its own agent A, cannot manage another agent's subscriptions.
	crossPath := wsAPI + "/agents/" + agentB.ID + "/document-subscriptions"
	authTestStatusWithHeaders(t, router, http.MethodPost, crossPath, daemonA.Token, hdrA, SubscribeDocumentRequest{DocumentID: doc.ID}, http.StatusForbidden)

	// Human-principal policy (Tom's ruling): a human owner passes the same shared boundary — no special-case
	// human rejection. "Agent-only" means no human UI is built on this, not that the API rejects humans.
	authTestStatusWithHeaders(t, router, http.MethodPost, subPath, owner.Token, nil, SubscribeDocumentRequest{DocumentID: doc.ID}, http.StatusOK)

	// Unsubscribe is idempotent: twice → 200, and the list is then empty.
	delPath := subPath + "/" + doc.ID
	authTestStatusWithHeaders(t, router, http.MethodDelete, delPath, daemonA.Token, hdrA, nil, http.StatusOK)
	authTestStatusWithHeaders(t, router, http.MethodDelete, delPath, daemonA.Token, hdrA, nil, http.StatusOK)
	var afterResp struct {
		DocumentIDs []string `json:"documentIds"`
	}
	authTestJSONWithHeaders(t, router, http.MethodGet, subPath, daemonA.Token, hdrA, nil, http.StatusOK, &afterResp)
	if len(afterResp.DocumentIDs) != 0 {
		t.Fatalf("expected no subscriptions after unsubscribe, got %#v", afterResp.DocumentIDs)
	}
}
