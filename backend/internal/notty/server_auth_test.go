package notty

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRegisterAccountCreatesNoImplicitWorkspaceRows(t *testing.T) {
	server, router := newAuthTestServer(t)

	auth := authTestRegister(t, router, "zero-workspace@example.com", "owner-pass", "Zero Workspace")
	if len(auth.Workspaces) != 0 {
		t.Fatalf("registration should return zero workspaces, got %#v", auth.Workspaces)
	}

	var workspaceCount int
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&workspaceCount); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	var memberCount int
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM workspace_members`).Scan(&memberCount); err != nil {
		t.Fatalf("count workspace members: %v", err)
	}
	if workspaceCount != 0 || memberCount != 0 {
		t.Fatalf("registration should not create workspace rows, workspaces=%d members=%d", workspaceCount, memberCount)
	}

	var list struct {
		Workspaces []*Workspace `json:"workspaces"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces", auth.Token, nil, http.StatusOK, &list)
	if len(list.Workspaces) != 0 {
		t.Fatalf("workspace list should be empty for zero-workspace account, got %#v", list.Workspaces)
	}
}

func TestCreateWorkspaceRequiresExplicitValidSlugAndHandle(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "workspace-slug-one@example.com", "owner-pass", "Owner One")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Handle: "owner-one",
	}, http.StatusBadRequest, "Workspace slug is required.")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "Product Workspace",
		Handle: "owner-one",
	}, http.StatusBadRequest, "Workspace slug can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   " product-workspace ",
		Handle: "owner-one",
	}, http.StatusBadRequest, "Workspace slug can only contain lowercase letters, numbers, underscores, and dashes.")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name: "Product Workspace",
		Slug: "product-workspace",
	}, http.StatusBadRequest, "Handle is required.")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: "Owner One",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: " owner-one ",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")

	var first struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: "owner-one",
	}, http.StatusCreated, &first)
	if first.Workspace.Slug != "product-workspace" {
		t.Fatalf("expected exact submitted slug, got %#v", first.Workspace)
	}

	secondOwner := authTestRegister(t, router, "workspace-slug-two@example.com", "owner-pass", "Owner Two")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", secondOwner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: "owner-two",
	}, http.StatusBadRequest, "Workspace slug is already taken.")
}

func TestWorkspaceMemberAndAgentIdentifiersAreValidatedAndImmutable(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "identifier-owner@example.com", "owner-pass", "Identifier Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Identifier Tenant")
	_ = authTestRegister(t, router, "identifier-member@example.com", "member-pass", "Identifier Member")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email: "identifier-member@example.com",
	}, http.StatusBadRequest, "Handle is required.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-member@example.com",
		Handle: "Member One",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-member@example.com",
		Handle: " member_one ",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")

	member := authTestAddMember(t, router, owner.Token, workspace.ID, "identifier-member@example.com", "member_one")
	var repeatedAdd struct {
		Member WorkspaceMember `json:"member"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-member@example.com",
		Handle: "other_member",
	}, http.StatusCreated, &repeatedAdd)
	if repeatedAdd.Member.UserID != member.UserID || repeatedAdd.Member.UserHandle != member.UserHandle {
		t.Fatalf("re-adding existing member should return original user identity, first=%#v repeated=%#v", member, repeatedAdd.Member)
	}
	var userCount int
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = $1`, workspace.ID).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 2 {
		t.Fatalf("re-adding existing member should not create a replacement user, got %d users", userCount)
	}

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Runtime daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "member_one",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusBadRequest, "Handle is already taken.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "Agent One",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: " agent_one ",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "agent_one",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusCreated, &agent)

	_ = authTestRegister(t, router, "identifier-other-member@example.com", "member-pass", "Identifier Other Member")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-other-member@example.com",
		Handle: "agent_one",
	}, http.StatusBadRequest, "Handle is already taken.")

	var updatedAgent Agent
	authTestJSON(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+agent.ID, owner.Token, UpdateAgentRequest{
		Handle: "renamed_agent",
		Name:   "Renamed Agent",
		Role:   "Still reviews changes",
	}, http.StatusOK, &updatedAgent)
	if updatedAgent.Handle != "agent_one" || updatedAgent.Name != "Renamed Agent" {
		t.Fatalf("agent update should mutate profile fields but keep handle, got %#v", updatedAgent)
	}
}

func TestAuthenticatedWorkspaceUserMutationEndpointsAreUnavailable(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "no-user-endpoints@example.com", "owner-pass", "Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "No User Endpoints Tenant")

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/users", owner.Token, map[string]string{
		"name":   "Blocked User",
		"handle": "blocked_user",
		"role":   "Blocked",
	}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/users/user-blocked", owner.Token, map[string]string{
		"name": "Blocked User",
	}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/users/user-blocked", owner.Token, nil, http.StatusNotFound)
}

func TestLegacyNoAuthRoutesAreNotRegistered(t *testing.T) {
	router := newAuthTestRouter(t)

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodGet, path: "/api/workspace"},
		{method: http.MethodPost, path: "/api/documents", body: map[string]string{}},
		{method: http.MethodPost, path: "/api/users", body: map[string]string{"name": "Blocked", "handle": "blocked"}},
		{method: http.MethodPost, path: "/api/agents", body: map[string]string{"handle": "blocked", "name": "Blocked", "kind": "codex"}},
		{method: http.MethodPost, path: "/api/threads", body: map[string]string{"documentId": "doc_missing"}},
		{method: http.MethodPost, path: "/api/presence", body: map[string]string{"actorId": "user_missing"}},
		{method: http.MethodPost, path: "/api/agent-runs", body: map[string]string{"agentId": "agent_missing"}},
		{method: http.MethodGet, path: "/ws"},
		{method: http.MethodGet, path: "/ws/documents/doc_missing"},
		{method: http.MethodGet, path: "/ws/documents-sync"},
	}

	for _, tc := range cases {
		authTestStatus(t, router, tc.method, tc.path, "", tc.body, http.StatusNotFound)
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

	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

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
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", otherDaemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)
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

func TestCreateDaemonAgentValidatesReportedRuntimeKind(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		runtimes    []RuntimeDetection
		wantStatus  int
		wantKind    string
		wantErrText []string
	}{
		{
			name:        "malformed kind",
			kind:        "bad kind",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: true}},
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"invalid agent kind"},
		},
		{
			name:        "no runtime report",
			kind:        "codex",
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"has not reported runtime availability"},
		},
		{
			name:        "runtime unavailable",
			kind:        "codex",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: false, Reason: "codex command not found"}},
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"runtime \"codex\" is unavailable", "codex command not found"},
		},
		{
			name:        "different runtime reported",
			kind:        "claude-code",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: true}},
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"runtime \"claude-code\" is not reported"},
		},
		{
			name:       "runtime available",
			kind:       "Claude-Code",
			runtimes:   []RuntimeDetection{{Kind: "claude-code", Available: true, Version: "claude test"}},
			wantStatus: http.StatusCreated,
			wantKind:   "claude-code",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newAuthTestRouter(t)
			emailSuffix := strings.ReplaceAll(tc.name, " ", "-")
			owner := authTestRegister(t, router, "runtime-"+emailSuffix+"@example.com", "owner-pass", "Runtime Owner")
			workspace := authTestCreateWorkspace(t, router, owner.Token, "Runtime Tenant "+tc.name)
			var daemonResponse CreateDaemonResponse
			authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Runtime daemon"}, http.StatusCreated, &daemonResponse)
			if tc.runtimes != nil {
				authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
					Version:  "0.62.0",
					OS:       "linux",
					Arch:     "arm64",
					Runtimes: tc.runtimes,
				}, http.StatusOK)
			}
			recorder := authTestRequest(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, nil, CreateAgentRequest{
				Handle: "runtime-agent",
				Name:   "Runtime Agent",
				Role:   "Exercises runtime validation",
				Kind:   tc.kind,
			})
			if recorder.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d body=%s", tc.wantStatus, recorder.Code, recorder.Body.String())
			}
			if len(tc.wantErrText) > 0 {
				var errorResponse struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &errorResponse); err != nil {
					t.Fatalf("decode error response: %v body=%s", err, recorder.Body.String())
				}
				for _, want := range tc.wantErrText {
					if !strings.Contains(errorResponse.Error, want) {
						t.Fatalf("expected error %q to contain %q", errorResponse.Error, want)
					}
				}
			}
			if tc.wantStatus != http.StatusCreated {
				return
			}
			var agent Agent
			if err := json.Unmarshal(recorder.Body.Bytes(), &agent); err != nil {
				t.Fatalf("decode agent response: %v body=%s", err, recorder.Body.String())
			}
			if agent.Kind != tc.wantKind {
				t.Fatalf("expected agent kind %q, got %#v", tc.wantKind, agent)
			}
		})
	}
}

func TestCreateDaemonAgentAppearsInDaemonWorkspaceSnapshot(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "runtime-snapshot-owner@example.com", "owner-pass", "Runtime Snapshot Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Runtime Snapshot Tenant")
	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Runtime daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex test",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

	var created Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "runtime-agent",
		Name:   "Runtime Agent",
		Role:   "Exercises daemon pickup",
		Kind:   "codex",
	}, http.StatusCreated, &created)

	var snapshot struct {
		CurrentDaemonID string   `json:"currentDaemonId"`
		Agents          []*Agent `json:"agents"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusOK, &snapshot)
	if snapshot.CurrentDaemonID != daemonResponse.Daemon.ID {
		t.Fatalf("expected current daemon %q, got %q", daemonResponse.Daemon.ID, snapshot.CurrentDaemonID)
	}
	if len(snapshot.Agents) != 1 {
		t.Fatalf("expected daemon workspace snapshot to include one agent, got %#v", snapshot.Agents)
	}
	got := snapshot.Agents[0]
	if got.ID != created.ID || got.DaemonID != daemonResponse.Daemon.ID || got.Kind != "codex" {
		t.Fatalf("expected created runtime agent in daemon snapshot, got %#v created=%#v daemon=%#v", got, created, daemonResponse.Daemon)
	}
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
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

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

func TestWorkspaceInviteRouteRequiresManagementRole(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "invite-owner@example.com", "owner-pass", "Invite Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Workspace")

	member := authTestRegister(t, router, "invite-member@example.com", "owner-pass", "Invite Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "member-handle")

	invitedByMember := authTestRegister(t, router, "invite-member-target@example.com", "owner-pass", "Invite Member Target")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", member.Token, AddWorkspaceMemberRequest{
		Email:  invitedByMember.Account.Email,
		Handle: "blocked-member",
	}, http.StatusForbidden)

	admin := authTestRegister(t, router, "invite-admin@example.com", "owner-pass", "Invite Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "admin-handle")
	if _, err := server.store.db.Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}
	adminInviteTarget := authTestRegister(t, router, "invite-admin-target@example.com", "owner-pass", "Invite Admin Target")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", admin.Token, AddWorkspaceMemberRequest{
		Email:  adminInviteTarget.Account.Email,
		Handle: "admin-invite",
	}, http.StatusCreated)

	ownerInviteTarget := authTestRegister(t, router, "invite-owner-target@example.com", "owner-pass", "Invite Owner Target")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  ownerInviteTarget.Account.Email,
		Handle: "owner-invite",
	}, http.StatusCreated)
}

func TestWorkspaceAdminOnlyActionsRejectMembersAndAllowAdmins(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "admin-only-owner@example.com", "owner-pass", "Admin Only Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Admin Only Workspace")

	admin := authTestRegister(t, router, "admin-only-admin@example.com", "owner-pass", "Admin Only Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "admin-handle")
	if _, err := server.store.db.Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}

	member := authTestRegister(t, router, "admin-only-member@example.com", "owner-pass", "Admin Only Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "member-handle")

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", member.Token, CreateDaemonRequest{
		Name: "member-daemon",
	}, http.StatusForbidden)

	var ownerDaemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{
		Name: "owner-daemon",
	}, http.StatusCreated, &ownerDaemon)

	var ownerAgent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+ownerDaemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "owner-agent",
		Name:   "Owner Agent",
		Role:   "Owner runs admin-only actions",
		Kind:   "codex",
	}, http.StatusCreated, &ownerAgent)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+ownerDaemon.Daemon.ID+"/agents", member.Token, CreateAgentRequest{
		Handle: "member-agent",
		Name:   "Member Agent",
		Role:   "Blocked",
		Kind:   "codex",
	}, http.StatusForbidden)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/agents", member.Token, CreateAgentRequest{
		DaemonID: ownerDaemon.Daemon.ID,
		Handle:   "member-agent-2",
		Name:     "Member Agent 2",
		Role:     "Blocked",
		Kind:     "codex",
	}, http.StatusForbidden)

	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+ownerAgent.ID, member.Token, UpdateAgentRequest{
		Name: "Blocked patch",
	}, http.StatusForbidden)

	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/agents/"+ownerAgent.ID, member.Token, nil, http.StatusForbidden)
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/daemons/"+ownerDaemon.Daemon.ID, member.Token, nil, http.StatusForbidden)

	var adminDaemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", admin.Token, CreateDaemonRequest{
		Name: "admin-daemon",
	}, http.StatusCreated, &adminDaemon)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+adminDaemon.Daemon.ID+"/agents", admin.Token, CreateAgentRequest{
		Handle: "admin-agent",
		Name:   "Admin Agent",
		Role:   "Allowed",
		Kind:   "codex",
	}, http.StatusCreated)
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
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", token, CreateWorkspaceRequest{
		Name:   name,
		Slug:   authTestIdentifierFromName(name, 64),
		Handle: "owner",
	}, http.StatusCreated, &response)
	if response.Workspace.ID == "" || response.Member.UserID == "" {
		t.Fatalf("expected workspace response, got %#v", response)
	}
	return authTestWorkspace{ID: response.Workspace.ID, OwnerUserID: response.Member.UserID}
}

func authTestIdentifierFromName(name string, maxLen int) string {
	value := strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	lastSeparator := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastSeparator = false
			continue
		}
		if r == '_' || r == '-' || r == ' ' {
			if !lastSeparator && builder.Len() > 0 {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
	}
	identifier := strings.Trim(builder.String(), "-")
	if len(identifier) > maxLen {
		identifier = strings.Trim(identifier[:maxLen], "-")
	}
	if len(identifier) < 2 {
		return "ws"
	}
	return identifier
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

func authTestErrorContains(t *testing.T, router http.Handler, method string, target string, token string, body any, want int, wantError string) {
	t.Helper()
	var response struct {
		Error string `json:"error"`
	}
	authTestJSON(t, router, method, target, token, body, want, &response)
	if !strings.Contains(response.Error, wantError) {
		t.Fatalf("%s %s expected error containing %q, got %q", method, target, wantError, response.Error)
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
