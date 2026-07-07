package notty

import (
	"net/http"
	"testing"
)

func TestThreadAnchorPatchReanchorsPreservesExcerptIdempotentlyWithHonestBroadcasts(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "thread-anchor-owner@example.com", "owner-pass", "Anchor Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Anchor Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/anchor.md", "anchor content\n")

	var created struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", owner.Token, CreateThreadRequest{
		DocumentID:    document.ID,
		Title:         "Anchor thread",
		Body:          "Re-anchor me.",
		RelativeStart: "start-1",
		RelativeEnd:   "end-1",
		Excerpt:       "original excerpt",
	}, http.StatusCreated, &created)
	if created.Thread.Anchor.RelativeStart != "start-1" || created.Thread.Anchor.Excerpt != "original excerpt" {
		t.Fatalf("unexpected created anchor: %#v", created.Thread.Anchor)
	}
	target := "/api/workspaces/" + workspace.ID + "/threads/" + created.Thread.ID + "/anchor"

	// Watch the workspace broker so broadcast honesty is asserted, not assumed.
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
	strptr := func(s string) *string { return &s }

	// A plain member re-anchors with a fresh excerpt → position + excerpt change, exactly one broadcast.
	member := authTestRegister(t, router, "thread-anchor-member@example.com", "owner-pass", "Anchor Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "anchor-member")
	var updated struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPatch, target, member.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-2", RelativeEnd: "end-2", Excerpt: strptr("re-anchored excerpt"),
	}, http.StatusOK, &updated)
	if updated.Thread.Anchor.RelativeStart != "start-2" || updated.Thread.Anchor.RelativeEnd != "end-2" {
		t.Fatalf("expected re-anchored position, got %#v", updated.Thread.Anchor)
	}
	if updated.Thread.Anchor.Excerpt != "re-anchored excerpt" {
		t.Fatalf("a provided excerpt should replace, got %q", updated.Thread.Anchor.Excerpt)
	}
	if updated.Thread.Anchor.Kind != "text-range" {
		t.Fatalf("expected text-range kind, got %q", updated.Thread.Anchor.Kind)
	}
	if got := drainThreadUpdated(); got != 1 {
		t.Fatalf("a real re-anchor should broadcast exactly once, got %d", got)
	}

	// Omitted excerpt preserves the stored one while the position still moves (the partial-update ruling).
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-3", RelativeEnd: "end-3", // excerpt omitted
	}, http.StatusOK, &updated)
	if updated.Thread.Anchor.RelativeStart != "start-3" {
		t.Fatalf("position should move, got %#v", updated.Thread.Anchor)
	}
	if updated.Thread.Anchor.Excerpt != "re-anchored excerpt" {
		t.Fatalf("an omitted excerpt must preserve the stored one, got %q", updated.Thread.Anchor.Excerpt)
	}
	if got := drainThreadUpdated(); got != 1 {
		t.Fatalf("moving the position is a real change, expected one broadcast, got %d", got)
	}
	movedAt := updated.Thread.UpdatedAt

	// Idempotent no-op: identical position, preserved excerpt → 200, unchanged updatedAt, no broadcast.
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-3", RelativeEnd: "end-3",
	}, http.StatusOK, &updated)
	if !updated.Thread.UpdatedAt.Equal(movedAt) {
		t.Fatalf("idempotent re-anchor must not bump updatedAt: %s -> %s", movedAt, updated.Thread.UpdatedAt)
	}
	if got := drainThreadUpdated(); got != 0 {
		t.Fatalf("idempotent no-op must not broadcast, got %d", got)
	}

	// Validation + isolation.
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{RelativeStart: "only-start"}, http.StatusBadRequest)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/threads/not-a-uuid/anchor", owner.Token, UpdateThreadAnchorRequest{RelativeStart: "s", RelativeEnd: "e"}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/threads/00000000-0000-4000-8000-000000000000/anchor", owner.Token, UpdateThreadAnchorRequest{RelativeStart: "s", RelativeEnd: "e"}, http.StatusNotFound)

	// Agent/daemon principals are 403 today — mirrors /status; opening re-anchor to agents is a later call.
	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Anchor daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, target, daemonResponse.Token, UpdateThreadAnchorRequest{RelativeStart: "s", RelativeEnd: "e"}, http.StatusForbidden)

	// Non-members never reach the handler.
	outsider := authTestRegister(t, router, "thread-anchor-outsider@example.com", "owner-pass", "Anchor Outsider")
	authTestStatus(t, router, http.MethodPatch, target, outsider.Token, UpdateThreadAnchorRequest{RelativeStart: "s", RelativeEnd: "e"}, http.StatusForbidden)

	// Every refused/invalid attempt stayed silent on the broker.
	if got := drainThreadUpdated(); got != 0 {
		t.Fatalf("refused/invalid attempts must not broadcast, got %d", got)
	}
}
