package notty

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Onboarding item keys. The frontend's "Getting started" surface stores exactly three *learning*
// milestones in onboarding_completions — the ones a user action produces and the one-time backfill
// seeds. A row means the item is done; no row means not done. The table is insert-only — never
// updated or deleted at runtime — so completion is write-once and can never drift back to
// "incomplete" the way the old live-derived state could (deleting your last document used to
// un-complete a step).
//
// Everything else is deliberately NOT stored here:
//   - Setup state (local environment connected, agent created, agent-at-work) is per-workspace and
//     stays live-derived on the frontend — a write-once row would keep asserting "connected" after
//     the daemon is removed, a lie under any scoping.
//   - The checklist-dismiss preference is per-workspace client state (localStorage), not an
//     account-durable milestone, so it has no key here.
//   - The remaining milestone names below have no honest existence signal or no surfaced step; they
//     are named only so the backfill/exclusion tests can assert they never land.
const (
	// Stored + client-insertable: exactly the learning milestones the frontend writes and the
	// backfill seeds.
	OnboardingFirstDocumentCreated = "first_document_created"
	OnboardingFirstThreadCreated   = "first_thread_created"
	OnboardingMemberInvited        = "member_invited"

	// Recognized names that are deliberately NOT stored (no honest signal, setup-state, or no
	// surfaced step). Not client-insertable, not backend-witnessed — named for exclusion tests only.
	OnboardingAccountIntroSeen          = "account_intro_seen"
	OnboardingWorkspaceCreated          = "workspace_created"
	OnboardingFirstDocumentEdited       = "first_document_edited"
	OnboardingFirstThreadReplied        = "first_thread_replied"
	OnboardingFirstDocumentWatcherAdded = "first_document_watcher_added"
)

// Each stored item key is registered in exactly one of the two maps below — client-insertable
// or backend-witnessed. The full valid set and the client/backend classification are derived
// from them, so a key can never be half-registered (valid but unclassified, or listed valid in
// one place and forgotten in another).

// onboardingClientItemKeys are the items a client may insert directly — exactly the three learning
// milestones the frontend writes (create-document, start-discussion, invite-team). This set IS the
// API contract: POST accepts only these, so we never accept a write for a key nothing reads.
var onboardingClientItemKeys = map[string]bool{
	OnboardingFirstDocumentCreated: true,
	OnboardingFirstThreadCreated:   true,
	OnboardingMemberInvited:        true,
}

// onboardingBackendItemKeys are milestones only the server witnesses — inserted by the backend
// at its own seams, never client-insertable. Empty today: every stored milestone is a client
// action. The split is kept deliberately even so — the moment a server-witnessed *learning*
// milestone appears, the client-forgery guard must already exist rather than be reintroduced
// under pressure (which is how the hole gets opened). The write-split tests assert behavior, so
// the structure stays safe to extend.
var onboardingBackendItemKeys = map[string]bool{}

// isOnboardingItemKey reports whether key is a recognized onboarding item (client or
// backend). Any insert must be one of these; an unknown key is rejected so a typo can never
// silently create a permanent phantom row.
func isOnboardingItemKey(key string) bool {
	return onboardingClientItemKeys[key] || onboardingBackendItemKeys[key]
}

// isClientOnboardingItemKey reports whether the client may insert key directly — i.e. it is one of
// the three learning milestones the frontend writes, not a server-witnessed key.
func isClientOnboardingItemKey(key string) bool {
	return onboardingClientItemKeys[key]
}

// OnboardingCompletion is one completed onboarding item for an account.
type OnboardingCompletion struct {
	ItemKey     string    `json:"itemKey"`
	CompletedAt time.Time `json:"completedAt"`
}

// insertOnboardingCompletion records item completion for an account, write-once. A repeat
// insert of the same (account, item) is a no-op (ON CONFLICT DO NOTHING), so the original
// completed_at is preserved and callers never need an existence check first.
func insertOnboardingCompletion(db *sql.DB, accountID string, itemKey string) error {
	if db == nil {
		return errors.New("database is required")
	}
	accountID = strings.TrimSpace(accountID)
	itemKey = strings.TrimSpace(itemKey)
	if !isUUIDString(accountID) {
		return ErrNotFound
	}
	if !isOnboardingItemKey(itemKey) {
		return errors.New("unknown onboarding item")
	}
	_, err := db.Exec(
		`INSERT INTO onboarding_completions (account_id, item_key, completed_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (account_id, item_key) DO NOTHING`,
		accountID, itemKey, time.Now().UTC(),
	)
	return err
}

// listOnboardingCompletions returns the account's completed onboarding items in completion
// order. The slice is empty (never nil) when nothing is completed, so it JSON-encodes as []
// rather than null and the frontend always receives a real key-set.
func listOnboardingCompletions(db *sql.DB, accountID string) ([]OnboardingCompletion, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	accountID = strings.TrimSpace(accountID)
	if !isUUIDString(accountID) {
		return nil, ErrNotFound
	}
	rows, err := db.Query(
		`SELECT item_key, completed_at
		   FROM onboarding_completions
		  WHERE account_id = $1
		  ORDER BY completed_at ASC, item_key ASC`,
		accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	completions := []OnboardingCompletion{}
	for rows.Next() {
		var c OnboardingCompletion
		if err := rows.Scan(&c.ItemKey, &c.CompletedAt); err != nil {
			return nil, err
		}
		completions = append(completions, c)
	}
	return completions, rows.Err()
}
