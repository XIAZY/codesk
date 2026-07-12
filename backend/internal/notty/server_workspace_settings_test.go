package notty

import (
	"net/http"
	"testing"
)

func patchStr(value string) *string {
	return &value
}

func TestWorkspaceSettingsPatchAuthZAndValidation(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "ws-settings-owner@example.com", "owner-pass", "Settings Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Settings Tenant")
	target := "/api/workspaces/" + workspace.ID + "/workspace"

	// Owner renames.
	var updated struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateWorkspaceRequest{
		Name: patchStr("Settings Tenant Renamed"),
	}, http.StatusOK, &updated)
	if updated.Workspace.Name != "Settings Tenant Renamed" {
		t.Fatalf("expected updated name, got %#v", updated.Workspace)
	}

	// The update survives independent reads (list endpoint).
	var listed struct {
		Workspaces []*Workspace `json:"workspaces"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces", owner.Token, nil, http.StatusOK, &listed)
	found := false
	for _, ws := range listed.Workspaces {
		if ws.ID == workspace.ID {
			found = true
			if ws.Name != "Settings Tenant Renamed" {
				t.Fatalf("list did not reflect update: %#v", ws)
			}
		}
	}
	if !found {
		t.Fatal("workspace missing from list")
	}

	// Admin may rename.
	admin := authTestRegister(t, router, "ws-settings-admin@example.com", "owner-pass", "Settings Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "settings-admin")
	if _, err := server.sqlDB().Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}
	authTestJSON(t, router, http.MethodPatch, target, admin.Token, UpdateWorkspaceRequest{
		Name: patchStr("Admin Renamed"),
	}, http.StatusOK, &updated)

	// A plain member may not manage workspace settings.
	member := authTestRegister(t, router, "ws-settings-member@example.com", "owner-pass", "Settings Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "settings-member")
	authTestStatus(t, router, http.MethodPatch, target, member.Token, UpdateWorkspaceRequest{
		Name: patchStr("Member Renamed"),
	}, http.StatusForbidden)

	// A daemon token must not pass the human gate, despite the daemon
	// permission bypass in requirePermission.
	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Settings daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, target, daemonResponse.Token, UpdateWorkspaceRequest{
		Name: patchStr("Daemon Renamed"),
	}, http.StatusForbidden)

	// A non-member never reaches the handler.
	outsider := authTestRegister(t, router, "ws-settings-outsider@example.com", "owner-pass", "Settings Outsider")
	authTestStatus(t, router, http.MethodPatch, target, outsider.Token, UpdateWorkspaceRequest{
		Name: patchStr("Outsider Renamed"),
	}, http.StatusForbidden)

	// Validation: empty patch, empty name.
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, UpdateWorkspaceRequest{}, http.StatusBadRequest)
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, UpdateWorkspaceRequest{Name: patchStr("   ")}, http.StatusBadRequest)
}

func TestWorkspaceSlugIsImmutableAfterCreation(t *testing.T) {
	server, router := newAuthTestServer(t)
	if server == nil {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

	owner := authTestRegister(t, router, "ws-slug-immutable@example.com", "owner-pass", "Slug Immutable Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Slug Pinned")
	original, err := getWorkspace(server.sqlDB(), workspace.ID)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	originalSlug := original.Slug
	target := "/api/workspaces/" + workspace.ID + "/workspace"

	// A stale/malicious client sends {name, slug} — name updates, slug is silently ignored.
	var updated struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, map[string]string{
		"name": "Renamed Workspace",
		"slug": "attacker-slug",
	}, http.StatusOK, &updated)
	if updated.Workspace.Name != "Renamed Workspace" {
		t.Fatalf("name should update, got %q", updated.Workspace.Name)
	}
	if updated.Workspace.Slug != originalSlug {
		t.Fatalf("slug must be immutable: got %q, want %q", updated.Workspace.Slug, originalSlug)
	}

	// Independent reload confirms the slug is unchanged in the database.
	reloaded, err := getWorkspace(server.sqlDB(), workspace.ID)
	if err != nil {
		t.Fatalf("reload workspace: %v", err)
	}
	if reloaded.Slug != originalSlug {
		t.Fatalf("DB slug must be immutable: got %q, want %q", reloaded.Slug, originalSlug)
	}

	// Slug-only PATCH (no name) is rejected as 400 — nothing to update.
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, map[string]string{
		"slug": "another-attempt",
	}, http.StatusBadRequest)
}

func TestWorkspaceDeleteRequiresOwnerAndExactNameThenCascades(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "ws-delete-owner@example.com", "owner-pass", "Delete Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Delete Tenant")
	target := "/api/workspaces/" + workspace.ID

	// Populate the workspace so the cascade has something real to prove:
	// a document, a thread with a message, a daemon, and a last-accessed ref.
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/doomed.md", "doomed content\n")
	var threadResponse struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, target+"/threads", owner.Token, CreateThreadRequest{
		DocumentID:    document.ID,
		Title:         "Doomed thread",
		Body:          "This all goes away.",
		RelativeStart: "anchor-start",
		RelativeEnd:   "anchor-end",
		Excerpt:       "doomed",
	}, http.StatusCreated, &threadResponse)
	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, target+"/daemons", owner.Token, CreateDaemonRequest{Name: "Doomed daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, target+"/last-accessed", owner.Token, UpdateLastAccessedRequest{}, http.StatusOK)
	// An agent with a document subscription, so the cascade sweep proves the subscription row
	// disappears rather than passing vacuously against an empty table.
	authTestReportCodexRuntime(t, router, workspace.ID, daemonResponse.Token)
	var subscriberAgent Agent
	authTestJSON(t, router, http.MethodPost, target+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "doomed-agent", Name: "Doomed Agent", Role: "subscribes then cascades away", Kind: "codex",
	}, http.StatusCreated, &subscriberAgent)
	authTestStatus(t, router, http.MethodPost, target+"/agents/"+subscriberAgent.ID+"/document-subscriptions", owner.Token, SubscribeDocumentRequest{DocumentID: document.ID}, http.StatusOK)

	// Non-owner roles are refused before any destructive work.
	admin := authTestRegister(t, router, "ws-delete-admin@example.com", "owner-pass", "Delete Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "delete-admin")
	if _, err := server.sqlDB().Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}
	authTestStatus(t, router, http.MethodDelete, target, admin.Token, DeleteWorkspaceRequest{ConfirmName: "Delete Tenant"}, http.StatusForbidden)
	authTestStatus(t, router, http.MethodDelete, target, daemonResponse.Token, DeleteWorkspaceRequest{ConfirmName: "Delete Tenant"}, http.StatusForbidden)

	// Watch the broker: workspace.deleted must fire only when a delete
	// actually happens — never on refused attempts.
	events, unsubscribe := server.workspaceBroker(workspace.ID).Subscribe()
	defer unsubscribe()
	drainWorkspaceDeleted := func() int {
		count := 0
		for {
			select {
			case event := <-events:
				if event.Type == "workspace.deleted" {
					count++
				}
			default:
				return count
			}
		}
	}

	// The confirmation must echo the exact current name.
	authTestStatus(t, router, http.MethodDelete, target, owner.Token, DeleteWorkspaceRequest{ConfirmName: "delete tenant"}, http.StatusBadRequest)
	authTestStatus(t, router, http.MethodDelete, target, owner.Token, DeleteWorkspaceRequest{}, http.StatusBadRequest)
	if _, err := getWorkspace(server.sqlDB(), workspace.ID); err != nil {
		t.Fatalf("workspace must survive refused deletions: %v", err)
	}
	if got := drainWorkspaceDeleted(); got != 0 {
		t.Fatalf("refused deletions must not broadcast workspace.deleted, got %d", got)
	}

	// Owner + exact name deletes, root document and all (the deferred
	// fk_workspaces_root_document RESTRICT must not block the cascade).
	authTestStatus(t, router, http.MethodDelete, target, owner.Token, DeleteWorkspaceRequest{ConfirmName: "Delete Tenant"}, http.StatusNoContent)
	if got := drainWorkspaceDeleted(); got != 1 {
		t.Fatalf("successful deletion should broadcast workspace.deleted exactly once, got %d", got)
	}

	// Deletion proof: zero rows remain in any workspace-scoped table.
	for _, table := range []string{
		"users", "documents", "document_updates", "document_checkpoints",
		"threads", "thread_messages", "thread_participants",
		"agents", "agent_runs", "agent_events", "agent_document_views",
		"agent_document_subscriptions",
		"activities", "presences", "daemons",
		"workspace_members", "workspace_invites",
	} {
		var count int
		if err := server.sqlDB().QueryRow(
			`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = $1::uuid`, workspace.ID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("table %s still has %d rows for the deleted workspace", table, count)
		}
	}
	var workspaceCount int
	if err := server.sqlDB().QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id = $1::uuid`, workspace.ID).Scan(&workspaceCount); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if workspaceCount != 0 {
		t.Fatal("workspace row survived deletion")
	}

	// accounts.last_accessed_workspace_id was nulled by ON DELETE SET NULL.
	var lastAccessed *string
	if err := server.sqlDB().QueryRow(
		`SELECT last_accessed_workspace_id::text FROM accounts WHERE id = $1`, owner.Account.ID,
	).Scan(&lastAccessed); err != nil {
		t.Fatalf("read last accessed: %v", err)
	}
	if lastAccessed != nil {
		t.Fatalf("last_accessed_workspace_id should be NULL, got %q", *lastAccessed)
	}

	// The slug is freed for reuse, and requests against the deleted workspace
	// no longer authenticate.
	authTestCreateWorkspace(t, router, owner.Token, "Delete Tenant")
	recorder := authTestRequest(t, router, http.MethodDelete, target, owner.Token, nil, DeleteWorkspaceRequest{ConfirmName: "Delete Tenant"})
	if recorder.Code == http.StatusNoContent {
		t.Fatalf("second delete must not succeed, got %d", recorder.Code)
	}
}
