package notty

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Onboarding item keys mirror the frontend ONBOARDING_EVENT_KEYS verbatim, plus a
// dismissed key for early-skip. A row in onboarding_completions means the item is done;
// no row means not done. The table is insert-only — nothing is ever updated or deleted at
// runtime — so completion is write-once and can never drift back to "incomplete" the way
// the old live-derived state could (deleting your last document used to un-complete a step).
const (
	OnboardingAccountIntroSeen          = "account_intro_seen"
	OnboardingWorkspaceCreated          = "workspace_created"
	OnboardingFirstDocumentCreated      = "first_document_created"
	OnboardingFirstDocumentEdited       = "first_document_edited"
	OnboardingFirstThreadCreated        = "first_thread_created"
	OnboardingFirstThreadReplied        = "first_thread_replied"
	OnboardingLocalEnvironmentConnected = "local_environment_connected"
	OnboardingFirstAgentCreated         = "first_agent_created"
	OnboardingFirstAgentRunStarted      = "first_agent_run_started"
	OnboardingFirstDocumentWatcherAdded = "first_document_watcher_added"
	OnboardingMemberInvited             = "member_invited"
	OnboardingDismissed                 = "onboarding_dismissed"
)

// onboardingBackendItemKeys are the two milestones only the server witnesses — the daemon
// connecting and an agent run starting can both happen while the browser is closed, so the
// backend inserts them at its own seams and a client is not allowed to claim them.
var onboardingBackendItemKeys = map[string]bool{
	OnboardingLocalEnvironmentConnected: true,
	OnboardingFirstAgentRunStarted:      true,
}

// onboardingItemKeys is the full set of valid item keys (client actions + the two
// backend-witnessed milestones + the dismiss marker). Any insert must be one of these; an
// unknown key is rejected so a typo can never silently create a permanent phantom row.
var onboardingItemKeys = map[string]bool{
	OnboardingAccountIntroSeen:          true,
	OnboardingWorkspaceCreated:          true,
	OnboardingFirstDocumentCreated:      true,
	OnboardingFirstDocumentEdited:       true,
	OnboardingFirstThreadCreated:        true,
	OnboardingFirstThreadReplied:        true,
	OnboardingLocalEnvironmentConnected: true,
	OnboardingFirstAgentCreated:         true,
	OnboardingFirstAgentRunStarted:      true,
	OnboardingFirstDocumentWatcherAdded: true,
	OnboardingMemberInvited:             true,
	OnboardingDismissed:                 true,
}

// isOnboardingItemKey reports whether key is a recognized onboarding item.
func isOnboardingItemKey(key string) bool { return onboardingItemKeys[key] }

// isClientOnboardingItemKey reports whether the client may insert key directly. It excludes
// the two server-witnessed milestones; everything else — user actions and the dismiss
// marker — is a legitimate client insert.
func isClientOnboardingItemKey(key string) bool {
	return onboardingItemKeys[key] && !onboardingBackendItemKeys[key]
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
