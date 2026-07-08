package notty

import (
	"encoding/base64"
	"net/http"
	"testing"
)

// TestThreadAnchorStateVectorRoundTripsAndValidates covers task #16: the anchor-time Y.js state vector
// travels verbatim through create + re-anchor, participates in the idempotency comparison (a vector-only
// change is a real change), and is validated (base64 + size + document-kind-forbidden) by the one shared
// validator. Mirrors the #95 broker emission-count pattern: 1 broadcast on change, 0 on no-op / refused.
func TestThreadAnchorStateVectorRoundTripsAndValidates(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "anchor-sv-owner@example.com", "owner-pass", "SV Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "SV Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/sv.md", "state vector content\n")

	vec1 := base64.StdEncoding.EncodeToString([]byte("state-vector-one"))
	vec2 := base64.StdEncoding.EncodeToString([]byte("state-vector-two-different"))

	// Create carries a state vector; the response serves it back verbatim.
	var created struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", owner.Token, CreateThreadRequest{
		DocumentID:    document.ID,
		Title:         "SV thread",
		Body:          "anchor me with a state vector",
		RelativeStart: "start-1",
		RelativeEnd:   "end-1",
		Excerpt:       "sv",
		StateAtAnchor: vec1,
	}, http.StatusCreated, &created)
	if created.Thread.Anchor.StateAtAnchor != vec1 {
		t.Fatalf("create must persist the state vector, got %q", created.Thread.Anchor.StateAtAnchor)
	}
	target := "/api/workspaces/" + workspace.ID + "/threads/" + created.Thread.ID + "/anchor"

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

	// Re-anchor with a fresh position AND a fresh vector → one broadcast, new vector served.
	var updated struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-2", RelativeEnd: "end-2", Excerpt: strptr("sv2"), StateAtAnchor: vec2,
	}, http.StatusOK, &updated)
	if updated.Thread.Anchor.StateAtAnchor != vec2 {
		t.Fatalf("re-anchor must write the fresh vector, got %q", updated.Thread.Anchor.StateAtAnchor)
	}
	if got := drainThreadUpdated(); got != 1 {
		t.Fatalf("a real re-anchor should broadcast once, got %d", got)
	}
	movedAt := updated.Thread.UpdatedAt

	// Idempotent no-op: identical position + preserved excerpt + identical vector → no write, no broadcast.
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-2", RelativeEnd: "end-2", StateAtAnchor: vec2,
	}, http.StatusOK, &updated)
	if !updated.Thread.UpdatedAt.Equal(movedAt) {
		t.Fatalf("identical re-anchor (incl. vector) must not bump updatedAt")
	}
	if got := drainThreadUpdated(); got != 0 {
		t.Fatalf("idempotent no-op must not broadcast, got %d", got)
	}

	// Vector-only change: same position + excerpt, different vector alone → a real change, one broadcast.
	authTestJSON(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-2", RelativeEnd: "end-2", StateAtAnchor: vec1,
	}, http.StatusOK, &updated)
	if updated.Thread.Anchor.StateAtAnchor != vec1 {
		t.Fatalf("vector-only change should update the vector, got %q", updated.Thread.Anchor.StateAtAnchor)
	}
	if got := drainThreadUpdated(); got != 1 {
		t.Fatalf("a vector-only change is a real change — expected one broadcast, got %d", got)
	}

	// Validation: malformed base64 → 400; a document-kind anchor (no range) may not carry a vector → 400.
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		RelativeStart: "start-3", RelativeEnd: "end-3", StateAtAnchor: "not!!valid!!base64",
	}, http.StatusBadRequest)
	authTestStatus(t, router, http.MethodPatch, target, owner.Token, UpdateThreadAnchorRequest{
		StateAtAnchor: vec1, // no relatives → document kind → a vector is forbidden
	}, http.StatusBadRequest)

	// Legacy shape: a thread created without a vector serves an empty one (omitted in JSON).
	var legacy struct {
		Thread *Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", owner.Token, CreateThreadRequest{
		DocumentID: document.ID, Title: "legacy", Body: "no vector", RelativeStart: "l1", RelativeEnd: "l2",
	}, http.StatusCreated, &legacy)
	if legacy.Thread.Anchor.StateAtAnchor != "" {
		t.Fatalf("a thread created without a vector must serve empty, got %q", legacy.Thread.Anchor.StateAtAnchor)
	}

	// The refused/invalid attempts stayed silent on the broker.
	if got := drainThreadUpdated(); got != 0 {
		t.Fatalf("refused/invalid attempts must not broadcast, got %d", got)
	}
}
