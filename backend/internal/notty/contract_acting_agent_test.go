package notty

import (
	"net/http"
	"testing"
)

// Suite 4.2 of the corpus (wire-half — the backend authorization boundary an agent's tool calls cross):
// a daemon may act as an agent ONLY via the X-Notty-Acting-Agent-ID header AND only for an agent it owns.
// The happy path (a daemon acting as its own agent) is exercised throughout the contract tier; this row
// pins the REJECTIONS that keep one daemon from impersonating another daemon's agent — the security-relevant
// half. A regression that stopped checking daemon ownership would let a compromised daemon author as any
// agent in the workspace; it goes red here.
func TestContractActingAgentTokenBoundary(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "acting-owner@example.com", "owner-pass", "Acting Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Acting Workspace")
	wsAPI := "/api/workspaces/" + ws.ID
	doc := authTestCreateDocument(t, router, owner.Token, ws.ID, "docs/acting.md", "# Acting\n")

	// Two daemons, each owning its own agent.
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

	threadReq := func() CreateThreadRequest {
		return CreateThreadRequest{DocumentID: doc.ID, Title: "Acting thread", Body: "body", Excerpt: "acting"}
	}

	// Happy path: daemon A acting as its OWN agent A → authorized.
	var created struct {
		Thread Thread `json:"thread"`
	}
	authTestJSONWithHeaders(t, router, http.MethodPost, wsAPI+"/threads", daemonA.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agentA.ID,
	}, threadReq(), http.StatusCreated, &created)

	// Boundary: daemon A presenting daemon B's agent id → rejected (the daemon does not own that agent).
	authTestStatusWithHeaders(t, router, http.MethodPost, wsAPI+"/threads", daemonA.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agentB.ID,
	}, threadReq(), http.StatusForbidden)

	// Boundary: an acting-agent id that does not exist at all → rejected.
	authTestStatusWithHeaders(t, router, http.MethodPost, wsAPI+"/threads", daemonA.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": "00000000-0000-0000-0000-000000000000",
	}, threadReq(), http.StatusForbidden)
}
