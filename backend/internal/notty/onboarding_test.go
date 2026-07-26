package notty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// onboardingKeys extracts the completed item keys from a GET/POST onboarding response,
// failing if the request itself was not 200.
func onboardingKeys(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("onboarding request status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Completions []OnboardingCompletion `json:"completions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode onboarding response: %v", err)
	}
	keys := make([]string, 0, len(payload.Completions))
	for _, c := range payload.Completions {
		keys = append(keys, c.ItemKey)
	}
	return keys
}

func hasKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// The item-key classification is the contract the write-split rides on, and it needs no
// database — a pure guard so a typo'd key can never persist and a client can never claim a
// server-witnessed milestone.
func TestOnboardingItemKeyClassification(t *testing.T) {
	// The eight learning milestones + dismiss are the only stored keys, and each is a client
	// action today (the backend map is empty), so every valid key is also client-insertable.
	stored := []string{
		OnboardingAccountIntroSeen, OnboardingWorkspaceCreated,
		OnboardingFirstDocumentCreated, OnboardingFirstDocumentEdited,
		OnboardingFirstThreadCreated, OnboardingFirstThreadReplied,
		OnboardingFirstDocumentWatcherAdded, OnboardingMemberInvited,
		OnboardingDismissed,
	}
	for _, key := range stored {
		if !isOnboardingItemKey(key) {
			t.Errorf("isOnboardingItemKey(%q) = false, want true", key)
		}
		if !isClientOnboardingItemKey(key) {
			t.Errorf("isClientOnboardingItemKey(%q) = false, want true (backend map is empty)", key)
		}
	}
	if isOnboardingItemKey("not_a_real_key") {
		t.Errorf("isOnboardingItemKey(unknown) = true, want false")
	}

	// The three "Add an AI teammate" chapter items are per-workspace setup state, deliberately
	// NOT stored here — the table must never recognize them. If one is ever re-added to a map
	// this fails, which is the intended guard.
	notStored := []string{
		"local_environment_connected",
		"first_agent_created",
		"first_agent_run_started",
	}
	for _, key := range notStored {
		if isOnboardingItemKey(key) {
			t.Errorf("isOnboardingItemKey(%q) = true, want false — setup state is live-derived, not stored", key)
		}
	}
}

// The account's completed-item set round-trips through the HTTP surface: empty to start,
// insert-only, idempotent on repeat, and readable back through GET.
func TestOnboardingCompletionsRoundTripThroughEndpoints(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "onboarding-roundtrip@example.com", "owner-pass", "Onboarding Roundtrip")

	// Empty to start — and the collection must serialize as [] never null, so the frontend
	// always reads a real key-set.
	getRec := authTestRequest(t, router, http.MethodGet, "/api/onboarding", owner.Token, nil, nil)
	if got := onboardingKeys(t, getRec); len(got) != 0 {
		t.Fatalf("fresh account onboarding keys = %v, want none", got)
	}
	if body := getRec.Body.String(); !strings.Contains(body, `"completions":[]`) {
		t.Fatalf("empty onboarding response must encode completions as [], got %s", body)
	}

	// A client insert records the item and echoes the updated set.
	postRec := authTestRequest(t, router, http.MethodPost, "/api/onboarding", owner.Token, nil, map[string]any{"itemKey": OnboardingFirstDocumentCreated})
	if got := onboardingKeys(t, postRec); !hasKey(got, OnboardingFirstDocumentCreated) {
		t.Fatalf("after insert, keys = %v, want %q present", got, OnboardingFirstDocumentCreated)
	}

	// Re-inserting the same item is a write-once no-op: still exactly one row, no error.
	repeatRec := authTestRequest(t, router, http.MethodPost, "/api/onboarding", owner.Token, nil, map[string]any{"itemKey": OnboardingFirstDocumentCreated})
	if got := onboardingKeys(t, repeatRec); len(got) != 1 || got[0] != OnboardingFirstDocumentCreated {
		t.Fatalf("after idempotent re-insert, keys = %v, want exactly [%q]", got, OnboardingFirstDocumentCreated)
	}

	// A second distinct item joins the set.
	authTestRequest(t, router, http.MethodPost, "/api/onboarding", owner.Token, nil, map[string]any{"itemKey": OnboardingMemberInvited})
	finalRec := authTestRequest(t, router, http.MethodGet, "/api/onboarding", owner.Token, nil, nil)
	final := onboardingKeys(t, finalRec)
	if !hasKey(final, OnboardingFirstDocumentCreated) || !hasKey(final, OnboardingMemberInvited) {
		t.Fatalf("final keys = %v, want both %q and %q", final, OnboardingFirstDocumentCreated, OnboardingMemberInvited)
	}
	if len(final) != 2 {
		t.Fatalf("final keys = %v, want exactly two", final)
	}
}

// The client POST surface stores only recognized learning milestones. An unknown key and the
// three setup-state items — which are live-derived per workspace and never stored — are all
// rejected with nothing persisted, so no phantom rows and no way to store a milestone meant to
// track live workspace state.
func TestOnboardingRejectsUnknownAndNonStoredKeys(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "onboarding-reject@example.com", "owner-pass", "Onboarding Reject")

	rejected := []string{
		"totally_made_up_key",
		"local_environment_connected",
		"first_agent_created",
		"first_agent_run_started",
	}
	for _, key := range rejected {
		rec := authTestRequest(t, router, http.MethodPost, "/api/onboarding", owner.Token, nil, map[string]any{"itemKey": key})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST itemKey=%q status = %d, want 400", key, rec.Code)
		}
	}

	getRec := authTestRequest(t, router, http.MethodGet, "/api/onboarding", owner.Token, nil, nil)
	if got := onboardingKeys(t, getRec); len(got) != 0 {
		t.Fatalf("after rejected inserts, keys = %v, want none persisted", got)
	}
}

// A completion recorded through the store is write-once (idempotent re-insert), readable back,
// and never leaks across accounts.
func TestOnboardingCompletionStoreScopingAndIdempotency(t *testing.T) {
	server, router := newAuthTestServer(t)
	accountA := authTestRegister(t, router, "onboarding-scope-a@example.com", "owner-pass", "Onboarding Scope A")
	accountB := authTestRegister(t, router, "onboarding-scope-b@example.com", "owner-pass", "Onboarding Scope B")
	db := server.sqlDB()

	if err := insertOnboardingCompletion(db, accountA.Account.ID, OnboardingFirstDocumentCreated); err != nil {
		t.Fatalf("store insert: %v", err)
	}
	// Write-once at the store layer: a repeat is a no-op, not a primary-key error.
	if err := insertOnboardingCompletion(db, accountA.Account.ID, OnboardingFirstDocumentCreated); err != nil {
		t.Fatalf("idempotent re-insert: %v", err)
	}

	aCompletions, err := listOnboardingCompletions(db, accountA.Account.ID)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(aCompletions) != 1 || aCompletions[0].ItemKey != OnboardingFirstDocumentCreated {
		t.Fatalf("account A completions = %+v, want exactly one %q", aCompletions, OnboardingFirstDocumentCreated)
	}

	// Account B never recorded it — its set must be empty (no cross-account leak).
	bCompletions, err := listOnboardingCompletions(db, accountB.Account.ID)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bCompletions) != 0 {
		t.Fatalf("account B completions = %+v, want none", bCompletions)
	}

	// The same isolation must hold through the read endpoint.
	if keys := onboardingKeys(t, authTestRequest(t, router, http.MethodGet, "/api/onboarding", accountA.Token, nil, nil)); !hasKey(keys, OnboardingFirstDocumentCreated) {
		t.Fatalf("account A GET keys = %v, want %q", keys, OnboardingFirstDocumentCreated)
	}
	if keys := onboardingKeys(t, authTestRequest(t, router, http.MethodGet, "/api/onboarding", accountB.Token, nil, nil)); len(keys) != 0 {
		t.Fatalf("account B GET keys = %v, want none", keys)
	}
}

// Both onboarding endpoints are account-scoped and must reject an unauthenticated caller —
// onboarding state is never world-readable or world-writable.
func TestOnboardingEndpointsRequireAuth(t *testing.T) {
	_, router := newAuthTestServer(t)

	getRec := authTestRequest(t, router, http.MethodGet, "/api/onboarding", "", nil, nil)
	if getRec.Code == http.StatusOK {
		t.Fatalf("unauthenticated GET /api/onboarding = 200, want rejected")
	}
	postRec := authTestRequest(t, router, http.MethodPost, "/api/onboarding", "", nil, map[string]any{"itemKey": OnboardingFirstDocumentCreated})
	if postRec.Code == http.StatusOK {
		t.Fatalf("unauthenticated POST /api/onboarding = 200, want rejected")
	}
}
