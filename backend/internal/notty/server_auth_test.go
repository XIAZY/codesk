package notty

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestAuthenticatedWorkspaceRoutesIsolateTenantsAndIgnoreSpoofedActor(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "owner-auth@example.com", "owner-pass", "Owner")
	if len(owner.Workspaces) != 0 {
		t.Fatalf("registration should not create implicit workspaces, got %#v", owner.Workspaces)
	}
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Tenant One")

	authTestStatus(t, router, http.MethodGet, "/api/workspace", owner.Token, nil, http.StatusNotFound)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", "", nil, http.StatusUnauthorized)

	outsider := authTestRegister(t, router, "outsider-auth@example.com", "outsider-pass", "Outsider")
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", outsider.Token, nil, http.StatusForbidden)

	member := authTestAddMember(t, router, owner.Token, workspace.ID, "outsider-auth@example.com", "outsider")
	var outsiderWorkspace struct {
		WorkspaceID   string `json:"workspaceId"`
		CurrentUserID string `json:"currentUserId"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", outsider.Token, nil, http.StatusOK, &outsiderWorkspace)
	if outsiderWorkspace.WorkspaceID != workspace.ID || outsiderWorkspace.CurrentUserID != member.UserID {
		t.Fatalf("expected outsider membership user in workspace response, got %#v member=%#v", outsiderWorkspace, member)
	}

	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/auth.md", "# Auth\n")
	var threadResponse struct {
		Thread Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads?actor=spoof&actor_type=agent", owner.Token, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Auth provenance",
		Body:       "This should be owned by the authenticated workspace user.",
		Excerpt:    "# Auth",
	}, http.StatusCreated, &threadResponse)
	if threadResponse.Thread.CreatedByID != workspace.OwnerUserID || threadResponse.Thread.CreatedByType != "human" {
		t.Fatalf("expected authenticated author, got id=%q type=%q owner=%q", threadResponse.Thread.CreatedByID, threadResponse.Thread.CreatedByType, workspace.OwnerUserID)
	}
}

func TestCreateWorkspaceAllocatesUniqueSlugForRepeatedNames(t *testing.T) {
	router := newAuthTestRouter(t)

	firstOwner := authTestRegister(t, router, "workspace-slug-one@example.com", "owner-pass", "Owner One")
	secondOwner := authTestRegister(t, router, "workspace-slug-two@example.com", "owner-pass", "Owner Two")

	var first struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", firstOwner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Handle: "owner-one",
	}, http.StatusCreated, &first)

	var second struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", secondOwner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Handle: "owner-two",
	}, http.StatusCreated, &second)

	if first.Workspace.Slug == "" || second.Workspace.Slug == "" {
		t.Fatalf("expected slugs to be allocated, got first=%#v second=%#v", first.Workspace, second.Workspace)
	}
	if first.Workspace.Slug == second.Workspace.Slug {
		t.Fatalf("expected repeated workspace names to get unique slugs, got %q", first.Workspace.Slug)
	}
}

func TestDaemonTokenIsWorkspaceScopedAndCanActAsWorkspaceAgent(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "daemon-owner@example.com", "owner-pass", "Daemon Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Tenant")
	otherWorkspace := authTestCreateWorkspace(t, router, owner.Token, "Other Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/daemon.md", "# Daemon\n")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Local daemon"}, http.StatusCreated, &daemonResponse)
	if daemonResponse.Token == "" {
		t.Fatal("expected daemon token")
	}
	if daemonResponse.Daemon.ConnectionStatus != "disconnected" {
		t.Fatalf("new daemon should start disconnected until it checks in, got %q", daemonResponse.Daemon.ConnectionStatus)
	}

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Review workspace changes",
		Kind:   "codex",
	}, http.StatusCreated, &agent)
	if agent.DaemonID != daemonResponse.Daemon.ID {
		t.Fatalf("expected agent daemon id %q, got %q", daemonResponse.Daemon.ID, agent.DaemonID)
	}

	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+agent.ID+"/session", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "thread-1"}, http.StatusOK)
	var daemonList struct {
		Daemons []*Daemon `json:"daemons"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, nil, http.StatusOK, &daemonList)
	if len(daemonList.Daemons) != 1 || daemonList.Daemons[0].ConnectionStatus != "online" {
		t.Fatalf("expected checked-in daemon to be online, got %#v", daemonList.Daemons)
	}
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+otherWorkspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusForbidden)

	var statusResponse struct {
		Daemon Daemon `json:"daemon"`
	}
	authTestJSON(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK, &statusResponse)
	if statusResponse.Daemon.ID != daemonResponse.Daemon.ID || statusResponse.Daemon.Version != "0.62.0" || len(statusResponse.Daemon.Runtimes) != 1 || !statusResponse.Daemon.Runtimes[0].Available {
		t.Fatalf("expected daemon status to update authenticated daemon, got %#v", statusResponse.Daemon)
	}
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", owner.Token, UpdateDaemonStatusRequest{Version: "human"}, http.StatusForbidden)

	var otherDaemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Other daemon"}, http.StatusCreated, &otherDaemonResponse)
	var otherAgent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+otherDaemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "other-agent",
		Name:   "Other Agent",
		Role:   "Owned by another daemon",
		Kind:   "codex",
	}, http.StatusCreated, &otherAgent)
	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+otherAgent.ID+"/session", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": otherAgent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "wrong-daemon"}, http.StatusForbidden)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+otherAgent.ID+"/session", daemonResponse.Token, UpdateAgentSessionRequest{Status: "idle", SessionID: "wrong-daemon-no-header"}, http.StatusForbidden)
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", daemonResponse.Token, CreateDaemonRequest{Name: "daemon-created-daemon"}, http.StatusForbidden)

	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, nil, http.StatusOK, &daemonList)
	if len(daemonList.Daemons) < 1 || daemonList.Daemons[0].Version != "0.62.0" || len(daemonList.Daemons[0].Runtimes) != 1 {
		t.Fatalf("expected daemon list to include runtime status, got %#v", daemonList.Daemons)
	}

	authTestStatusWithHeaders(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.Handle,
	}, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Handle-based agent note",
		Body:       "Handles are display names, not acting identities.",
		Excerpt:    "# Daemon",
	}, http.StatusForbidden)

	var threadResponse struct {
		Thread Thread `json:"thread"`
	}
	authTestJSONWithHeaders(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Agent note",
		Body:       "The daemon token may only speak as a verified workspace agent.",
		Excerpt:    "# Daemon",
	}, http.StatusCreated, &threadResponse)
	if threadResponse.Thread.CreatedByID != agent.ID || threadResponse.Thread.CreatedByType != "agent" {
		t.Fatalf("expected thread authored by acting agent, got id=%q type=%q agent=%q", threadResponse.Thread.CreatedByID, threadResponse.Thread.CreatedByType, agent.ID)
	}

	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID, owner.Token, nil, http.StatusOK)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusForbidden)
}

func TestDaemonReinstallTokenRotatesDaemonToken(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "daemon-reinstall-owner@example.com", "owner-pass", "Daemon Reinstall Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Reinstall Tenant")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Local daemon"}, http.StatusCreated, &daemonResponse)
	if daemonResponse.Token == "" || daemonResponse.Daemon == nil {
		t.Fatalf("expected daemon token and daemon, got %#v", daemonResponse)
	}

	var reinstallResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/reinstall-token", owner.Token, nil, http.StatusOK, &reinstallResponse)
	if reinstallResponse.Token == "" {
		t.Fatal("expected reinstall token")
	}
	if reinstallResponse.Token == daemonResponse.Token {
		t.Fatal("expected reinstall token to differ from original token")
	}
	if reinstallResponse.Daemon.ID != daemonResponse.Daemon.ID {
		t.Fatalf("expected reinstall token for same daemon, got %#v", reinstallResponse.Daemon)
	}

	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", reinstallResponse.Token, nil, http.StatusOK)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusForbidden)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", reinstallResponse.Token, nil, http.StatusOK)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/reinstall-token", reinstallResponse.Token, nil, http.StatusForbidden)
}

func TestDaemonTokenDocumentUpdateHTTPRouteRemoved(t *testing.T) {
	_, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "daemon-document-owner@example.com", "owner-pass", "Daemon Document Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Document Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/daemon-update.md", "alpha")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Local daemon"}, http.StatusCreated, &daemonResponse)

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "daemon-editor",
		Name:   "Daemon Editor",
		Role:   "Applies local document edits",
		Kind:   "codex",
	}, http.StatusCreated, &agent)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID+"/updates", bytes.NewReader([]byte{0, 0}))
	request.Header.Set("Authorization", "Bearer "+daemonResponse.Token)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Notty-Acting-Agent-ID", agent.ID)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected removed authenticated document update route, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID, owner.Token, map[string]string{"path": "docs/renamed.md"}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID, owner.Token, nil, http.StatusNotFound)
}

type authTestWorkspace struct {
	ID          string
	OwnerUserID string
}

func newAuthTestRouter(t *testing.T) http.Handler {
	t.Helper()
	_, router := newAuthTestServer(t)
	return router
}

func newAuthTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := initPostgresSchema(db); err != nil {
		_ = db.Close()
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		_ = db.Close()
		t.Fatalf("clear tables: %v", err)
	}
	_ = db.Close()

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	server := NewServer(Config{DatabaseURL: dsn, JWTSecret: "test-secret"}, store)
	return server, server.Routes()
}

func authTestRegister(t *testing.T, router http.Handler, email string, password string, name string) AuthResponse {
	t.Helper()
	var response AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       email,
		Password:    password,
		DisplayName: name,
	}, http.StatusCreated, &response)
	if response.Token == "" || response.Account == nil {
		t.Fatalf("expected auth response, got %#v", response)
	}
	return response
}

func authTestCreateWorkspace(t *testing.T, router http.Handler, token string, name string) authTestWorkspace {
	t.Helper()
	var response struct {
		Workspace Workspace       `json:"workspace"`
		Member    WorkspaceMember `json:"member"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", token, CreateWorkspaceRequest{Name: name}, http.StatusCreated, &response)
	if response.Workspace.ID == "" || response.Member.UserID == "" {
		t.Fatalf("expected workspace response, got %#v", response)
	}
	return authTestWorkspace{ID: response.Workspace.ID, OwnerUserID: response.Member.UserID}
}

func authTestAddMember(t *testing.T, router http.Handler, token string, workspaceID string, email string, handle string) WorkspaceMember {
	t.Helper()
	var response struct {
		Member WorkspaceMember `json:"member"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspaceID+"/members", token, AddWorkspaceMemberRequest{
		Email:  email,
		Handle: handle,
	}, http.StatusCreated, &response)
	if response.Member.UserID == "" {
		t.Fatalf("expected member response, got %#v", response)
	}
	return response.Member
}

func authTestCreateDocument(t *testing.T, router http.Handler, token string, workspaceID string, path string, content string) DocumentMetadata {
	t.Helper()
	var document DocumentMetadata
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspaceID+"/documents", token, CreateDocumentRequest{}, http.StatusCreated, &document)
	if document.ID == "" {
		t.Fatalf("expected document response, got %#v", document)
	}
	return document
}

func authTestStatus(t *testing.T, router http.Handler, method string, target string, token string, body any, want int) {
	t.Helper()
	authTestStatusWithHeaders(t, router, method, target, token, nil, body, want)
}

func authTestStatusWithHeaders(t *testing.T, router http.Handler, method string, target string, token string, headers map[string]string, body any, want int) {
	t.Helper()
	recorder := authTestRequest(t, router, method, target, token, headers, body)
	if recorder.Code != want {
		t.Fatalf("%s %s expected status %d, got %d body=%s", method, target, want, recorder.Code, recorder.Body.String())
	}
}

func authTestJSON(t *testing.T, router http.Handler, method string, target string, token string, body any, want int, out any) {
	t.Helper()
	authTestJSONWithHeaders(t, router, method, target, token, nil, body, want, out)
}

func authTestJSONWithHeaders(t *testing.T, router http.Handler, method string, target string, token string, headers map[string]string, body any, want int, out any) {
	t.Helper()
	recorder := authTestRequest(t, router, method, target, token, headers, body)
	if recorder.Code != want {
		t.Fatalf("%s %s expected status %d, got %d body=%s", method, target, want, recorder.Code, recorder.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response for %s %s: %v body=%s", method, target, err, recorder.Body.String())
		}
	}
}

func authTestRequest(t *testing.T, router http.Handler, method string, target string, token string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload := []byte(nil)
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}
