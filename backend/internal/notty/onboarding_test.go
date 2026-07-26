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
	all := []string{
		OnboardingAccountIntroSeen, OnboardingWorkspaceCreated,
		OnboardingFirstDocumentCreated, OnboardingFirstDocumentEdited,
		OnboardingFirstThreadCreated, OnboardingFirstThreadReplied,
		OnboardingLocalEnvironmentConnected, OnboardingFirstAgentCreated,
		OnboardingFirstAgentRunStarted, OnboardingFirstDocumentWatcherAdded,
		OnboardingMemberInvited, OnboardingDismissed,
	}
	for _, key := range all {
		if !isOnboardingItemKey(key) {
			t.Errorf("isOnboardingItemKey(%q) = false, want true", key)
		}
	}
	if isOnboardingItemKey("not_a_real_key") {
		t.Errorf("isOnboardingItemKey(unknown) = true, want false")
	}

	// The two server-witnessed milestones must be rejected as client inserts; every other
	// known key (user actions + the dismiss marker) must be accepted.
	backendOnly := map[string]bool{
		OnboardingLocalEnvironmentConnected: true,
		OnboardingFirstAgentRunStarted:      true,
	}
	for _, key := range all {
		got := isClientOnboardingItemKey(key)
		want := !backendOnly[key]
		if got != want {
			t.Errorf("isClientOnboardingItemKey(%q) = %v, want %v", key, got, want)
		}
	}
	if isClientOnboardingItemKey("not_a_real_key") {
		t.Errorf("isClientOnboardingItemKey(unknown) = true, want false")
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

// The client POST surface accepts only client-insertable items: an unknown key and the two
// server-witnessed milestones are rejected, and nothing is persisted on rejection.
func TestOnboardingRejectsUnknownAndBackendOnlyClientInserts(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "onboarding-reject@example.com", "owner-pass", "Onboarding Reject")

	rejected := []string{
		"totally_made_up_key",
		OnboardingLocalEnvironmentConnected,
		OnboardingFirstAgentRunStarted,
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

// The two server-witnessed milestones reach the table through the store (the backend seams'
// path, not the client POST), the store insert is idempotent, and completions never leak
// across accounts.
func TestOnboardingBackendInsertAndAccountScoping(t *testing.T) {
	server, router := newAuthTestServer(t)
	accountA := authTestRegister(t, router, "onboarding-scope-a@example.com", "owner-pass", "Onboarding Scope A")
	accountB := authTestRegister(t, router, "onboarding-scope-b@example.com", "owner-pass", "Onboarding Scope B")
	db := server.sqlDB()

	// Backend seam path: a server-witnessed milestone the client may not claim.
	if err := insertOnboardingCompletion(db, accountA.Account.ID, OnboardingLocalEnvironmentConnected); err != nil {
		t.Fatalf("backend insert: %v", err)
	}
	// Write-once at the store layer too: a repeat is a no-op, not a primary-key error.
	if err := insertOnboardingCompletion(db, accountA.Account.ID, OnboardingLocalEnvironmentConnected); err != nil {
		t.Fatalf("idempotent backend re-insert: %v", err)
	}

	aCompletions, err := listOnboardingCompletions(db, accountA.Account.ID)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(aCompletions) != 1 || aCompletions[0].ItemKey != OnboardingLocalEnvironmentConnected {
		t.Fatalf("account A completions = %+v, want exactly one %q", aCompletions, OnboardingLocalEnvironmentConnected)
	}

	// Account B never witnessed the milestone — its set must be empty (no cross-account leak).
	bCompletions, err := listOnboardingCompletions(db, accountB.Account.ID)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bCompletions) != 0 {
		t.Fatalf("account B completions = %+v, want none", bCompletions)
	}

	// The same isolation must hold through the read endpoint.
	if keys := onboardingKeys(t, authTestRequest(t, router, http.MethodGet, "/api/onboarding", accountA.Token, nil, nil)); !hasKey(keys, OnboardingLocalEnvironmentConnected) {
		t.Fatalf("account A GET keys = %v, want %q", keys, OnboardingLocalEnvironmentConnected)
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
