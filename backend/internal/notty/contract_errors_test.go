package notty

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Suite 2.3 of the corpus: the error envelope is uniform across status classes. Every error path must
// return exactly {"error": "<non-empty message>"} — one key, a non-null non-empty string — so the frontend
// can parse any failure the same way regardless of which handler or class produced it. A handler that
// invents a different error shape (a bare string, an extra field, a null message, a status/body mismatch)
// goes red here. Messages themselves are not pinned (they are human copy); the shape and the status are.
//
// The shape is uniform BY CONSTRUCTION: every error in the package flows through the one writeError helper
// (server_http.go), so this test's real job is to catch a handler that bypasses it. It samples across the
// 4xx classes on real handlers — 400 missing-slug, 401 no-credential, 403 non-human on a human-only
// endpoint, 404 unknown entity, 409 slug-conflict. The remaining classes (410 expired-invite via
// workspaceInviteErrorStatus, 413 oversized-diff via ErrDocumentDiffTooLarge) go through the same helper
// and are exercised for status by their own handler tests; they are candidates to fold in here if a
// dedicated red-proof of the envelope on those paths is wanted later.

// assertErrorEnvelope asserts the response is the canonical error shape at the expected status.
func assertErrorEnvelope(t *testing.T, gotStatus int, body []byte, wantStatus int, label string) {
	t.Helper()
	if gotStatus != wantStatus {
		t.Fatalf("%s: got status %d, want %d; body=%s", label, gotStatus, wantStatus, body)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("%s: error body is not a JSON object: %v; body=%s", label, err, body)
	}
	if len(obj) != 1 {
		t.Fatalf("%s: error envelope must have exactly one key, got %d: %s", label, len(obj), body)
	}
	raw, ok := obj["error"]
	if !ok {
		t.Fatalf("%s: error envelope missing the \"error\" key: %s", label, body)
	}
	var message string
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("%s: \"error\" is not a JSON string (null or object?): %s", label, body)
	}
	if message == "" {
		t.Fatalf("%s: \"error\" message is empty", label)
	}
}

func TestContractErrorEnvelopePerClass(t *testing.T) {
	_, router := newAuthTestServer(t)
	fx := buildContractPopulatedFixture(t, router)
	ws := "/api/workspaces/" + fx.WorkspaceID

	// 400 — a create-workspace request missing its required slug.
	rec := authTestRequest(t, router, http.MethodPost, "/api/workspaces", fx.OwnerToken, nil, CreateWorkspaceRequest{Name: "No Slug"})
	assertErrorEnvelope(t, rec.Code, rec.Body.Bytes(), http.StatusBadRequest, "400 missing slug")

	// 401 — an authenticated endpoint with no credential.
	rec = authTestRequest(t, router, http.MethodGet, "/api/auth/me", "", nil, nil)
	assertErrorEnvelope(t, rec.Code, rec.Body.Bytes(), http.StatusUnauthorized, "401 no credential")

	// 403 — a non-human principal (a daemon token) on a human-only endpoint (create agent).
	rec = authTestRequest(t, router, http.MethodPost, ws+"/daemons/"+fx.DaemonID+"/agents", fx.DaemonToken, nil, CreateAgentRequest{
		Handle: "forbidden-agent", Name: "Forbidden", Role: "should be rejected", Kind: "codex",
	})
	assertErrorEnvelope(t, rec.Code, rec.Body.Bytes(), http.StatusForbidden, "403 non-human on human-only endpoint")

	// 404 — a GET for a thread id that does not exist in this workspace.
	rec = authTestRequest(t, router, http.MethodGet, ws+"/threads/00000000-0000-0000-0000-000000000000", fx.OwnerToken, nil, nil)
	assertErrorEnvelope(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, "404 unknown thread")

	// 409 — renaming a workspace's slug to one already held by another workspace the same owner owns.
	var wsA struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", fx.OwnerToken, CreateWorkspaceRequest{
		Name: "Conflict A", Slug: "err-conflict-slug-a", Handle: "owner",
	}, http.StatusCreated, &wsA)
	var wsB struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", fx.OwnerToken, CreateWorkspaceRequest{
		Name: "Conflict B", Slug: "err-conflict-slug-b", Handle: "owner",
	}, http.StatusCreated, &wsB)
	takenSlug := "err-conflict-slug-a"
	rec = authTestRequest(t, router, http.MethodPatch, "/api/workspaces/"+wsB.Workspace.ID+"/workspace", fx.OwnerToken, nil, UpdateWorkspaceRequest{Slug: &takenSlug})
	assertErrorEnvelope(t, rec.Code, rec.Body.Bytes(), http.StatusConflict, "409 slug already taken")
}
