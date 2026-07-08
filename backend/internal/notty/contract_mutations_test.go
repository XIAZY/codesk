package notty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Suite 2.2 of the corpus: mutation response shapes. Where suite 2.1 pins what the read surface returns,
// this pins what a successful write returns — the object the frontend consumes straight out of a create /
// update call (it renders from the response, not a re-fetch). Each row performs one real mutation on live
// Postgres and pins the canonicalized response to a committed golden, so a create/update contract drift is
// a red, reviewable diff. Secret-bearing responses (the daemon-create token) are deferred to the redaction
// row in 2.4 so a random token never lands in a golden.

// assertResponseGolden asserts the status, canonicalizes the response body, and pins/diffs the golden.
func assertResponseGolden(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, goldenName string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("%s: got status %d, want %d; body=%s", goldenName, rec.Code, wantStatus, rec.Body.String())
	}
	canonical, err := canonicalizeContractJSON(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("canonicalize %s: %v", goldenName, err)
	}
	assertContractGolden(t, goldenName, canonical)
}

// decodeInto unmarshals a recorder's JSON body into dst, failing the test on error.
func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response body: %v; body=%s", err, rec.Body.String())
	}
}

func TestContractMutationResponseShapes(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "contract-mutations-owner@example.com", "owner-pass", "Contract Mutations Owner")

	// workspace create → {workspace, member}
	recWs := authTestRequest(t, router, http.MethodPost, "/api/workspaces", owner.Token, nil, CreateWorkspaceRequest{
		Name: "Mutation Workspace", Slug: "mutation-workspace", Handle: "owner",
	})
	assertResponseGolden(t, recWs, http.StatusCreated, "mutation_workspace_create")
	var wsResp struct {
		Workspace Workspace `json:"workspace"`
	}
	decodeInto(t, recWs, &wsResp)
	ws := "/api/workspaces/" + wsResp.Workspace.ID

	// document create → DocumentMetadata (idempotent create with a client-supplied id)
	recDoc := authTestRequest(t, router, http.MethodPost, ws+"/documents", owner.Token, nil, CreateDocumentRequest{
		DocumentID: "11111111-1111-1111-1111-111111111111",
	})
	assertResponseGolden(t, recDoc, http.StatusCreated, "mutation_document_create")
	var docResp DocumentMetadata
	decodeInto(t, recDoc, &docResp)

	// A daemon + agent to author a thread the product way (daemon-create response itself is deferred to
	// the redaction row because it carries a token).
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, ws+"/daemons", owner.Token, CreateDaemonRequest{Name: "Mutation daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, wsResp.Workspace.ID, daemon.Token)
	var agent Agent
	recAgent := authTestRequest(t, router, http.MethodPost, ws+"/daemons/"+daemon.Daemon.ID+"/agents", owner.Token, nil, CreateAgentRequest{
		Handle: "mutation-agent", Name: "Mutation Agent", Role: "Exercises the mutation shapes", Kind: "codex",
	})
	assertResponseGolden(t, recAgent, http.StatusCreated, "mutation_agent_create")
	decodeInto(t, recAgent, &agent)

	// thread create → {thread}
	recThread := authTestRequest(t, router, http.MethodPost, ws+"/threads", daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, CreateThreadRequest{
		DocumentID: docResp.ID,
		Title:      "Mutation thread",
		Body:       "A thread created to pin the create response shape.",
		Excerpt:    "mutation",
	})
	assertResponseGolden(t, recThread, http.StatusCreated, "mutation_thread_create")

	// presence upsert → Presence
	recPresence := authTestRequest(t, router, http.MethodPost, ws+"/presence", daemon.Token, nil, UpsertPresenceRequest{
		ActorID:    daemon.Daemon.ID,
		ActorType:  "daemon",
		DocumentID: docResp.ID,
		Activity:   "viewing",
	})
	assertResponseGolden(t, recPresence, http.StatusOK, "mutation_presence_upsert")
}
