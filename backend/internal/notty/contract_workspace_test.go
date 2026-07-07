package notty

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Contract-regression tier (PR1). Each row drives a real HTTP handler on live Postgres against a
// canonical workspace state, canonicalizes the response, and diffs a committed golden — so a backend
// response-shape change is a red, reviewable diff in the PR that caused it, and the frontend imports
// the same goldens as fixtures. This file starts the tier with the empty-state row for GET /workspace,
// the A1-critical case (the null-collection switch crash lived exactly here).

// workspaceCollectionKeys are the top-level collection fields of the workspace response. The A1
// invariant: none may ever serialize as JSON null — the frontend reducer dereferences them, and a
// null is what white-screened workspace switching. They must be [] / {}.
var workspaceCollectionKeys = []string{
	"users", "daemons", "agents", "agentRuns", "threads", "agentEvents", "presences", "activities",
}

func contractGoldenPath(name string) string {
	return filepath.Join("testdata", "contract", name+".json")
}

// assertContractGolden compares canonical against the committed golden, or (re)writes it under
// NOTTY_UPDATE_GOLDEN=1. A drift is red and must be regenerated inside the same reviewable PR.
func assertContractGolden(t *testing.T, name string, canonical []byte) {
	t.Helper()
	path := contractGoldenPath(name)
	if os.Getenv("NOTTY_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, canonical, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("regenerated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (re-run with NOTTY_UPDATE_GOLDEN=1 to generate it)", path, err)
	}
	if string(canonical) != string(want) {
		t.Fatalf("response shape drifted from %s.\nIf the change is intended, re-run with NOTTY_UPDATE_GOLDEN=1 to regenerate.\n--- got ---\n%s\n--- want ---\n%s", path, canonical, want)
	}
}

// assertNoNullCollections is the A1 invariant: each named collection field is present and non-null.
func assertNoNullCollections(t *testing.T, raw []byte, keys []string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal response for null-collection check: %v", err)
	}
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			t.Errorf("A1: collection field %q missing from response", k)
			continue
		}
		if string(v) == "null" {
			t.Errorf("A1: collection field %q is JSON null — must be [] / {} (the field the switch crash dereferenced)", k)
		}
	}
}

// buildEmptyWorkspace is the `empty` state builder: a just-created workspace seeded through the real
// registration + create-workspace handlers — owner member only, no daemons / agents / threads /
// activities / presences. Returns the owner token and workspace id for driving the row's endpoint.
func buildEmptyWorkspace(t *testing.T, router http.Handler) (ownerToken, workspaceID string) {
	t.Helper()
	owner := authTestRegister(t, router, "contract-empty-owner@example.com", "owner-pass", "Contract Empty Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Contract Empty Workspace")
	return owner.Token, ws.ID
}

func TestContractWorkspaceEmptyState(t *testing.T) {
	_, router := newAuthTestServer(t)
	ownerToken, workspaceID := buildEmptyWorkspace(t, router)

	recorder := authTestRequest(t, router, http.MethodGet, "/api/workspaces/"+workspaceID+"/workspace", ownerToken, nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /workspace: got %d body=%s", recorder.Code, recorder.Body.String())
	}
	raw := recorder.Body.Bytes()

	// A1 first — the incident invariant, checked against the real bytes before any golden exists.
	assertNoNullCollections(t, raw, workspaceCollectionKeys)

	// A2 — canonical shape pinned as a committed golden.
	canonical, err := canonicalizeContractJSON(raw)
	if err != nil {
		t.Fatalf("canonicalize GET /workspace: %v", err)
	}
	assertContractGolden(t, "workspace_get_empty", canonical)
}
