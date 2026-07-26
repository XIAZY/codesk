package notty

import (
	"database/sql"
	"fmt"
)

// onboardingLearningBackfillStatement seeds onboarding_completions for accounts that existed
// before server-controlled onboarding shipped, so a returning user is NOT re-onboarded when the
// frontend cuts over (task #14) to reading the DB. It derives each account's already-earned
// LEARNING milestones from existing state — the same existence signals the old live derivation
// used — aggregated across the account's active workspaces.
//
// SIX milestones are backfilled, each reproducing today's derivation. The other two onboarding keys
// are deliberately NOT, because neither has an honest existence signal — leaving them out fails
// toward the user re-earning them once, never toward a permanently-broken new-user path:
//   - account_intro_seen — purely "saw the intro modal", nothing in the DB witnesses it. Backfilling
//     it would silently skip the intro for a brand-new user. Left out: an existing user sees the
//     intro once and dismisses it, and with the other milestones marked the guide reports complete.
//   - first_document_edited — "has a document" is not "edited one", and updated_at moves for other
//     reasons. Left out: re-earned by a single edit.
//
// The three SETUP-state milestones (local_environment_connected, first_agent_created,
// first_agent_run_started) are absent by design — they stay live-derived per workspace, because a
// write-once row would keep asserting "connected" after the daemon/agent is gone.
//
// Run-once, per-account, single statement:
//   - Per-account guard: a row is inserted only for an account that has NO completion rows yet.
//     So a stray row affects only its own account (not the whole table), and a new account created
//     later — having no rows — is derived from its own (near-empty) state: only workspace_created
//     ends up true, leaving the rest of onboarding to show. No launch-time cutoff/marker needed.
//   - Single INSERT (UNION ALL of the six milestone selects, guarded once at the outer WHERE):
//     the NOT EXISTS check evaluates against the pre-statement snapshot. Running it as six separate
//     INSERTs would be wrong — a transaction sees its own writes, so the guard would go false after
//     the first insert and every account would get exactly one milestone.
//   - ON CONFLICT DO NOTHING makes re-execution and concurrent boots idempotent.
//
// Item keys are the string literals of the onboarding.go consts (OnboardingWorkspaceCreated etc.);
// TestOnboardingBackfill* pins that they land as the expected six.
const onboardingLearningBackfillStatement = `
INSERT INTO onboarding_completions (account_id, item_key, completed_at)
SELECT sub.account_id, sub.item_key, now()
  FROM (
    SELECT DISTINCT m.account_id, 'workspace_created'::text AS item_key
      FROM workspace_members m WHERE m.status = 'active'
    UNION ALL
    -- A "created document" is a CONTENT document = a non-hidden documents row. The workspace's
    -- auto-created root namespace doc and any deleted docs are hidden, so NOT d.hidden excludes
    -- exactly what the frontend's documentCount (rootNamespace.documents.length) excludes.
    SELECT DISTINCT m.account_id, 'first_document_created'
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM documents d WHERE d.workspace_id = m.workspace_id AND NOT d.hidden)
    UNION ALL
    SELECT DISTINCT m.account_id, 'first_thread_created'
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM threads t WHERE t.workspace_id = m.workspace_id)
    UNION ALL
    SELECT DISTINCT m.account_id, 'first_thread_replied'
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM thread_messages tm WHERE tm.workspace_id = m.workspace_id)
    UNION ALL
    SELECT DISTINCT m.account_id, 'first_document_watcher_added'
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM agent_document_subscriptions s WHERE s.workspace_id = m.workspace_id)
    UNION ALL
    -- member_invited: THIS account invited someone — its own workspace user created an invite
    -- (workspace_members.user_id = workspace_invites.created_by_user_id). Faithful to today's
    -- per-inviter flag; does NOT over-mark other members of a workspace that merely has invites.
    SELECT DISTINCT m.account_id, 'member_invited'
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM workspace_invites i
                    WHERE i.workspace_id = m.workspace_id AND i.created_by_user_id = m.user_id)
  ) sub
 WHERE NOT EXISTS (SELECT 1 FROM onboarding_completions c WHERE c.account_id = sub.account_id)
ON CONFLICT DO NOTHING`

// backfillOnboardingLearningCompletions runs the one-time learning-milestone backfill (see
// onboardingLearningBackfillStatement). Wired into OpenDatabase after schema init; safe to run on
// every boot — the per-account guard + ON CONFLICT make it a no-op once an account has rows.
func backfillOnboardingLearningCompletions(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(onboardingLearningBackfillStatement); err != nil {
		return fmt.Errorf("onboarding learning backfill: %w", err)
	}
	return nil
}
