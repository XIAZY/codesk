package notty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Suite 2.4 of the corpus: WS-snapshot ↔ REST parity. The frontend hydrates a workspace from GET /workspace
// on load and from the workspace.snapshot WS event on (re)connect — two code paths that must serve the same
// shape for every shared collection, or a field present on one surface and missing on the other silently
// half-renders the app. This row builds one populated workspace, reads both surfaces, and asserts each
// shared collection is byte-identical after canonicalization. A drift on either surface is a red diff naming
// the collection.
func TestContractWorkspaceSnapshotRESTParity(t *testing.T) {
	fx := newWorkspaceRouteTestFixture(t)
	wsAPI := "/api/workspaces/" + fx.workspaceID

	// Populate the fixture's workspace through the real handlers so both surfaces have collections to compare.
	doc := authTestCreateDocument(t, fx.router, fx.token, fx.workspaceID, "docs/parity.md", "# Parity\n")
	var daemon CreateDaemonResponse
	authTestJSON(t, fx.router, http.MethodPost, wsAPI+"/daemons", fx.token, CreateDaemonRequest{Name: "Parity daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, fx.router, fx.workspaceID, daemon.Token)
	var agent Agent
	authTestJSON(t, fx.router, http.MethodPost, wsAPI+"/daemons/"+daemon.Daemon.ID+"/agents", fx.token, CreateAgentRequest{
		Handle: "parity-agent", Name: "Parity Agent", Role: "Exercises WS↔REST parity", Kind: "codex",
	}, http.StatusCreated, &agent)
	authTestStatusWithHeaders(t, fx.router, http.MethodPatch, wsAPI+"/agents/"+agent.ID+"/session", daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "parity-session"}, http.StatusOK)
	var thread struct {
		Thread Thread `json:"thread"`
	}
	authTestJSONWithHeaders(t, fx.router, http.MethodPost, wsAPI+"/threads", daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, CreateThreadRequest{DocumentID: doc.ID, Title: "Parity thread", Body: "parity", Excerpt: "parity"}, http.StatusCreated, &thread)
	var presence Presence
	authTestJSON(t, fx.router, http.MethodPost, wsAPI+"/presence", daemon.Token, UpsertPresenceRequest{
		ActorID: daemon.Daemon.ID, ActorType: "daemon", DocumentID: doc.ID, Activity: "viewing",
	}, http.StatusOK, &presence)

	// REST surface: GET /workspace.
	rec := authTestRequest(t, fx.router, http.MethodGet, wsAPI+"/workspace", fx.token, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /workspace: %d body=%s", rec.Code, rec.Body.String())
	}
	var restObj map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &restObj); err != nil {
		t.Fatalf("unmarshal REST workspace: %v", err)
	}

	// WS surface: the workspace.snapshot event served on connect (snapshot reflects the current state, so
	// populate first, then connect).
	httpServer := httptest.NewServer(fx.server.Routes())
	defer httpServer.Close()
	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fx.workspaceWSPath(""), fx.token)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set ws read deadline: %v", err)
	}
	var event EventEnvelope
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read workspace.snapshot: %v", err)
	}
	if event.Type != "workspace.snapshot" {
		t.Fatalf("first ws event = %q, want workspace.snapshot", event.Type)
	}
	snapRaw, err := json.Marshal(event.Data)
	if err != nil {
		t.Fatalf("re-marshal snapshot data: %v", err)
	}
	var snapObj map[string]json.RawMessage
	if err := json.Unmarshal(snapRaw, &snapObj); err != nil {
		t.Fatalf("unmarshal snapshot data: %v", err)
	}

	// Each shared collection must be byte-identical after canonicalization across the two surfaces.
	for _, key := range workspaceCollectionKeys {
		restVal, restOK := restObj[key]
		snapVal, snapOK := snapObj[key]
		if !restOK {
			t.Errorf("collection %q present on the snapshot but missing from GET /workspace", key)
			continue
		}
		if !snapOK {
			t.Errorf("collection %q present on GET /workspace but missing from the snapshot", key)
			continue
		}
		restCanon, err := canonicalizeContractJSON(restVal)
		if err != nil {
			t.Fatalf("canonicalize REST %q: %v", key, err)
		}
		snapCanon, err := canonicalizeContractJSON(snapVal)
		if err != nil {
			t.Fatalf("canonicalize snapshot %q: %v", key, err)
		}
		if string(restCanon) != string(snapCanon) {
			t.Errorf("collection %q shape differs between GET /workspace and workspace.snapshot.\n--- REST ---\n%s\n--- snapshot ---\n%s", key, restCanon, snapCanon)
		}
	}
}
