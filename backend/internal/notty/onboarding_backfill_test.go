package notty

import (
	"database/sql"
	"testing"
)

// seedContentDocument inserts a non-root, non-hidden document into a workspace — a "created a
// document" signal, distinct from the auto-created root namespace doc — and returns its id.
func seedContentDocument(t *testing.T, db *sql.DB, workspaceID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`INSERT INTO documents (workspace_id, id, hidden, client_id_seed, updated_at)
		 VALUES ($1, gen_random_uuid(), false, 1, now()) RETURNING id::text`, workspaceID).Scan(&id); err != nil {
		t.Fatalf("seed content document: %v", err)
	}
	return id
}

func seedThread(t *testing.T, db *sql.DB, workspaceID, documentID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO threads
		   (workspace_id, id, document_id, title, status, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
		 VALUES ($1, gen_random_uuid(), $2, 't', 'open', 'human', 'h', 'n', now(), now())`, workspaceID, documentID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
}

// accountWorkspaceUserID returns the account's workspace user id — the identity that authors invites.
func accountWorkspaceUserID(t *testing.T, db *sql.DB, accountID, workspaceID string) string {
	t.Helper()
	var userID string
	if err := db.QueryRow(
		`SELECT user_id::text FROM workspace_members WHERE account_id = $1 AND workspace_id = $2`, accountID, workspaceID).Scan(&userID); err != nil {
		t.Fatalf("workspace user id: %v", err)
	}
	return userID
}

// seedInviteBy records an invite authored by a specific workspace user (member_invited is derived
// from authorship, so who invites determines who is backfilled).
func seedInviteBy(t *testing.T, db *sql.DB, workspaceID, userID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO workspace_invites (id, workspace_id, token_hash, created_by_user_id, expires_at, created_at)
		 VALUES (gen_random_uuid(), $1, gen_random_uuid()::text, $2, now() + interval '1 day', now())`, workspaceID, userID); err != nil {
		t.Fatalf("seed invite by user: %v", err)
	}
}

// seedInviteByOtherUser records an invite authored by a NEW, unrelated workspace user — used to
// prove member_invited does not over-mark other members of a workspace that merely has invites.
func seedInviteByOtherUser(t *testing.T, db *sql.DB, workspaceID string) {
	t.Helper()
	var userID string
	if err := db.QueryRow(
		`INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		 VALUES ($1, gen_random_uuid(), 'inviter', 'Inviter', 'owner', 'human', 'active', now(), now()) RETURNING id::text`, workspaceID).Scan(&userID); err != nil {
		t.Fatalf("seed other workspace user: %v", err)
	}
	seedInviteBy(t, db, workspaceID, userID)
}

func backfilledKeys(t *testing.T, db *sql.DB, accountID string) []string {
	t.Helper()
	completions, err := listOnboardingCompletions(db, accountID)
	if err != nil {
		t.Fatalf("list completions: %v", err)
	}
	keys := make([]string, 0, len(completions))
	for _, c := range completions {
		keys = append(keys, c.ItemKey)
	}
	return keys
}

// The one-time learning backfill seeds each account's already-earned milestones — all of them, not
// one — excludes the signal-less keys, doesn't count the auto-created root document, and leaves any
// account that already has completion rows completely untouched.
func TestOnboardingBackfillDerivesEarnedLearningMilestones(t *testing.T) {
	server, router := newAuthTestServer(t)
	db := server.sqlDB()

	// Account A: a workspace with a content document, a thread, and an invite A itself authored →
	// four learning milestones (including member_invited, derived from A's own authorship).
	accountA := authTestRegister(t, router, "backfill-active@example.com", "owner-pass", "Backfill Active")
	wsA := authTestCreateWorkspace(t, router, accountA.Token, "Active Tenant")
	docA := seedContentDocument(t, db, wsA.ID)
	seedThread(t, db, wsA.ID, docA)
	seedInviteBy(t, db, wsA.ID, accountWorkspaceUserID(t, db, accountA.Account.ID, wsA.ID))

	// Account B: a workspace with only the auto-created root document, plus an invite authored by a
	// DIFFERENT user — so B earns only workspace_created (the root doesn't count, and a stranger's
	// invite must not over-mark member_invited).
	accountB := authTestRegister(t, router, "backfill-empty@example.com", "owner-pass", "Backfill Empty")
	wsB := authTestCreateWorkspace(t, router, accountB.Token, "Empty Tenant")
	seedInviteByOtherUser(t, db, wsB.ID)

	// Account C: qualifies (a content document) BUT already has a completion row. The per-account
	// guard must skip it entirely — and, crucially, its stray row must not stop A and B from being
	// backfilled (the failure the old table-empty guard would have had).
	accountC := authTestRegister(t, router, "backfill-stray@example.com", "owner-pass", "Backfill Stray")
	wsC := authTestCreateWorkspace(t, router, accountC.Token, "Stray Tenant")
	seedContentDocument(t, db, wsC.ID)
	if err := insertOnboardingCompletion(db, accountC.Account.ID, OnboardingMemberInvited); err != nil {
		t.Fatalf("seed stray row: %v", err)
	}

	if err := backfillOnboardingLearningCompletions(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// A gets ALL of its true milestones — not just one. (Six separate INSERTs would give exactly
	// one, because a transaction sees its own writes and the per-account guard would go false after
	// the first; the single UNION statement is what makes all three land.)
	aKeys := backfilledKeys(t, db, accountA.Account.ID)
	for _, want := range []string{OnboardingWorkspaceCreated, OnboardingFirstDocumentCreated, OnboardingFirstThreadCreated, OnboardingMemberInvited} {
		if !hasKey(aKeys, want) {
			t.Errorf("account A missing earned milestone %q; got %v", want, aKeys)
		}
	}
	// ...and not the signal-less keys, nor milestones it never earned.
	for _, notWant := range []string{OnboardingAccountIntroSeen, OnboardingFirstDocumentEdited, OnboardingFirstThreadReplied} {
		if hasKey(aKeys, notWant) {
			t.Errorf("account A has un-earned/signal-less key %q; got %v", notWant, aKeys)
		}
	}

	// B: only workspace_created — the root doc doesn't count, and a stranger's invite doesn't over-mark.
	bKeys := backfilledKeys(t, db, accountB.Account.ID)
	if len(bKeys) != 1 || bKeys[0] != OnboardingWorkspaceCreated {
		t.Errorf("account B keys = %v, want exactly [workspace_created] (root must not count; stranger's invite must not over-mark)", bKeys)
	}

	// C: untouched — still exactly its stray row, no derived milestones even though it qualifies.
	cKeys := backfilledKeys(t, db, accountC.Account.ID)
	if len(cKeys) != 1 || cKeys[0] != OnboardingMemberInvited {
		t.Errorf("account C keys = %v, want exactly its stray [member_invited] (guard skips accounts with rows)", cKeys)
	}

	// Idempotent: a second run changes nothing.
	before := backfilledKeys(t, db, accountA.Account.ID)
	if err := backfillOnboardingLearningCompletions(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if after := backfilledKeys(t, db, accountA.Account.ID); len(after) != len(before) {
		t.Errorf("second backfill changed account A: before %v, after %v", before, after)
	}
}
