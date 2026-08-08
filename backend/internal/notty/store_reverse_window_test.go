package notty

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDocumentReverseWindowSchemaEnforcesDurableInvariants(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	daemonID := seedStoreDaemonRuntime(t, store, "")
	contentDocumentID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := database.DB.Exec(`INSERT INTO documents (
		workspace_id, id, hidden, client_id_seed, updated_at
	) VALUES ($1, $2, false, 1, $3)`, store.workspaceID, contentDocumentID, now); err != nil {
		t.Fatalf("seed content document: %v", err)
	}

	insert := func(t *testing.T, documentID, scope string, generation any, openedAt, reverseUntil time.Time, restoreOperationID any, restoreUpdateID any, consumedAt any) error {
		t.Helper()
		_, err := database.DB.Exec(`INSERT INTO document_reverse_windows (
			document_id, workspace_id, root_document_id, entry_id, desired_path,
			origin_daemon_id, origin_scope, window_generation,
			tombstone_operation_id, tombstone_request_fingerprint,
			opened_at, reverse_until, restore_operation_id, restore_update_id,
			consumed_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, 'docs/a.md', $5, $6, $7, $8, 'fingerprint',
			$9, $10, $11, $12, $13, $9, $9)`,
			documentID, store.workspaceID, store.RootDocumentID(), documentID,
			daemonID, scope, generation, uuid.NewString(), openedAt, reverseUntil,
			restoreOperationID, restoreUpdateID, consumedAt)
		return err
	}

	if err := insert(t, contentDocumentID, "primary", nil, now, now.Add(5*time.Minute), nil, nil, nil); err != nil {
		t.Fatalf("insert valid reverse window: %v", err)
	}
	if _, err := database.DB.Exec(`DELETE FROM document_reverse_windows WHERE document_id = $1`, contentDocumentID); err != nil {
		t.Fatalf("clear valid reverse window: %v", err)
	}

	for _, tc := range []struct {
		name               string
		scope              string
		generation         any
		reverseUntil       time.Time
		restoreOperationID any
		restoreUpdateID    any
		consumedAt         any
	}{
		{name: "non-positive generation", scope: "primary", generation: int64(0), reverseUntil: now.Add(time.Minute)},
		{name: "invalid scope", scope: "agent", generation: 1, reverseUntil: now.Add(time.Minute)},
		{name: "non-increasing time", scope: "primary", generation: 1, reverseUntil: now},
		{name: "partial consumption", scope: "primary", generation: 1, reverseUntil: now.Add(time.Minute), restoreOperationID: uuid.NewString()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert(t, contentDocumentID, tc.scope, tc.generation, now, tc.reverseUntil, tc.restoreOperationID, tc.restoreUpdateID, tc.consumedAt); err == nil {
				t.Fatal("invalid reverse-window row was accepted")
			}
		})
	}
}
