package notty

import (
	"net/http"
	"testing"
)

func TestThreadStatusPatchResolvesReopensIdempotentlyWithHonestBroadcasts(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "thread-status-owner@example.com", "owner-pass", "Status Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Status Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/status.md", "status content\n")

	var created struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", owner.Token, CreateThreadRequest{
		DocumentID:    document.ID,
		Title:         "Status thread",
		Body:          "Resolve me.",
		RelativeStart: "anchor-start",
		RelativeEnd:   "anchor-end",
		Excerpt:       "status",
	}, http.StatusCreated, &created)
	if created.Thread.Status != "open" {
		t.Fatalf("new thread should be open, got %q", created.Thread.Status)
	}
	target := "/api/workspaces/" + workspace.ID + "/threads/" + created.Thread.ID + "/status"

	// Watch the workspace broker so broadcast honesty is asserted, not assumed:
	// real transitions emit thread.updated, idempotent no-ops stay silent.
	events, unsubscribe := server.workspaceBroker(workspace.ID).Subscribe()
	defer unsubscribe()
	drainThreadUpdated := func() int {
		count := 0
		for {
			select {
			case event := <-events:
				if event.Type == "thread.updated" {
					count++
				}
			default:
				return count
			}
		}
	}

	// A plain member resolves (any role — resolving is a judgment call).
	member := authTestRegister(t, router, "thread-status-member@example.com", "owner-pass", "Status Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "status-member")
	var updated struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPatch, target, member.Token, UpdateThreadRequest{Status: "resolved"}, http.StatusOK, &updated)
	if updated.Thread.Status != "resolved" {
		t.Fatalf("expected resolved, got %q", updated.Thread.Status)
	}
	resolvedAt := updated.Thread.UpdatedAt
	if got := drainThreadUpdated(); got != 1 {
		t.Fatalf("real transition should broadcast exactly once, got %d", got)
	}

	// Idempotent re-resolve: 200, unchanged updatedAt, no broadcast.
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadRequest{Status: "resolved"}, http.StatusOK, &updated)
	if updated.Thread.Status != "resolved" {
		t.Fatalf("re-resolve should keep resolved, got %q", updated.Thread.Status)
	}
	if !updated.Thread.UpdatedAt.Equal(resolvedAt) {
		t.Fatalf("idempotent re-resolve must not bump updatedAt: %s -> %s", resolvedAt, updated.Thread.UpdatedAt)
	}
	if got := drainThreadUpdated(); got != 0 {
		t.Fatalf("idempotent no-op must not broadcast, got %d events", got)
	}

	// Reversible: reopen succeeds and is visible on a plain GET.
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadRequest{Status: "open"}, http.StatusOK, &updated)
	if updated.Thread.Status != "open" {
		t.Fatalf("expected reopened thread, got %q", updated.Thread.Status)
	}
	if !updated.Thread.UpdatedAt.After(resolvedAt) {
		t.Fatal("real transition should bump updatedAt")
	}
	if got := drainThreadUpdated(); got != 1 {
		t.Fatalf("reopen should broadcast exactly once, got %d", got)
	}
	var fetched struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/threads/"+created.Thread.ID, owner.Token, nil, http.StatusOK, &fetched)
	if fetched.Thread.Status != "open" {
		t.Fatalf("GET should reflect reopen, got %q", fetched.Thread.Status)
	}

	// Validation and isolation.
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, UpdateThreadRequest{Status: "closed"}, http.StatusBadRequest)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/threads/not-a-uuid/status", owner.Token, UpdateThreadRequest{Status: "resolved"}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/threads/00000000-0000-4000-8000-000000000000/status", owner.Token, UpdateThreadRequest{Status: "resolved"}, http.StatusNotFound)

	// Agent/daemon principals are 403 today — opening resolve to agent tooling
	// is a deliberate later decision, not a default.
	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Status daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, target, daemonResponse.Token, UpdateThreadRequest{Status: "resolved"}, http.StatusForbidden)

	// Non-members never reach the handler.
	outsider := authTestRegister(t, router, "thread-status-outsider@example.com", "owner-pass", "Status Outsider")
	authTestStatus(t, router, http.MethodPatch, target, outsider.Token, UpdateThreadRequest{Status: "resolved"}, http.StatusForbidden)
}
