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
// THREE milestones are backfilled — exactly the learning keys #14's "Getting started" surfaces
// (confirmed by Eva against the checklist): first_document_created (create-document step),
// first_thread_created (start-discussion step), member_invited (invite-team step). Everything else
// is deliberately NOT backfilled:
//   - workspace_created, first_thread_replied, first_document_watcher_added — no checklist item in
//     #14, so a backfilled row would be written and never read. NOT a forward-compatible superset:
//     if a future step ever needs one, add the key AND its derivation then.
//   - account_intro_seen, first_document_edited — no honest existence signal in the DB. Left out so
//     they fail toward the user re-earning them once, never toward a permanently-broken new path.
//   - local_environment_connected, first_agent_created, first_agent_run_started, and the agent step
//     (agent-at-work) — SETUP-state, kept live-derived per workspace, because a write-once row would
//     keep asserting "connected" after the daemon/agent is gone.
//
// Run-once, per-account, single statement:
//   - Per-account guard: a row is inserted only for an account that has NO completion rows yet, so a
//     stray row affects only its own account. Unlike the earlier form, NO milestone is unconditional
//     now, so an account with a workspace but no content earns 0 rows and stays eligible on later
//     boots — harmless: it matches nothing until it earns a real milestone, and once it does the
//     frontend client-insert + ON CONFLICT DO NOTHING dedupe with any server-side re-derivation.
//   - Single INSERT (UNION ALL guarded once at the outer WHERE): the NOT EXISTS check evaluates
//     against the pre-statement snapshot. Separate per-milestone INSERTs would be wrong — a
//     transaction sees its own writes, so the guard would go false after the first insert and every
//     account would get exactly one milestone.
//   - ON CONFLICT DO NOTHING makes re-execution and concurrent boots idempotent.
//
// Item keys are the string literals of the onboarding.go consts (OnboardingFirstDocumentCreated etc.);
// TestOnboardingBackfill* pins that they land as the expected three.
const onboardingLearningBackfillStatement = `
INSERT INTO onboarding_completions (account_id, item_key, completed_at)
SELECT sub.account_id, sub.item_key, now()
  FROM (
    -- A "created document" is a CONTENT document = a non-hidden documents row. The workspace's
    -- auto-created root namespace doc and any deleted docs are hidden, so NOT d.hidden excludes
    -- exactly what the frontend's documentCount (rootNamespace.documents.length) excludes.
    SELECT DISTINCT m.account_id, 'first_document_created'::text AS item_key
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM documents d WHERE d.workspace_id = m.workspace_id AND NOT d.hidden)
    UNION ALL
    -- thread EXISTS = created (the start-discussion step completes on created, NOT replied).
    SELECT DISTINCT m.account_id, 'first_thread_created'
      FROM workspace_members m WHERE m.status = 'active'
       AND EXISTS (SELECT 1 FROM threads t WHERE t.workspace_id = m.workspace_id)
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
