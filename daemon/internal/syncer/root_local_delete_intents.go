package syncer

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type rootLocalDeletePhase string

const (
	rootLocalDeletePhaseLegacyFloor       rootLocalDeletePhase = "legacy_floor"
	rootLocalDeletePhaseTombstonePending  rootLocalDeletePhase = "tombstone_pending"
	rootLocalDeletePhaseWindowOpen        rootLocalDeletePhase = "window_open"
	rootLocalDeletePhaseContentSyncing    rootLocalDeletePhase = "content_syncing"
	rootLocalDeletePhaseRestorePending    rootLocalDeletePhase = "restore_pending"
	rootLocalDeletePhaseProjectionPending rootLocalDeletePhase = "projection_pending"
)

type rootLocalDeleteIntent struct {
	RootDocumentID             string
	ContentDocumentID          string
	EntryID                    string
	DesiredPath                string
	MaterializedPath           string
	TombstoneOperationID       string
	ExpectedWindowGeneration   int64
	WindowGeneration           int64
	OpenedAt                   time.Time
	ReverseUntil               time.Time
	RestoreOperationID         string
	RequiredContentStateVector []byte
	ObservedFileIdentity       string
	ObservedContentSHA256      string
	Phase                      rootLocalDeletePhase
	Attempts                   int
	NextAttemptAt              time.Time
	LastError                  string
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

const rootLocalDeleteIntentsV2Schema = `create table %s (
	root_document_id text not null,
	content_document_id text not null,
	entry_id text not null default '',
	desired_path text not null default '',
	materialized_path text not null default '',
	tombstone_operation_id text,
	expected_window_generation integer,
	window_generation integer,
	opened_at integer,
	reverse_until integer,
	restore_operation_id text,
	required_content_state_vector blob,
	observed_file_identity text,
	observed_content_sha256 text,
	phase text not null default 'legacy_floor' check (
		phase in (
			'legacy_floor',
			'tombstone_pending',
			'window_open',
			'content_syncing',
			'restore_pending',
			'projection_pending'
		)
	),
	attempts integer not null default 0,
	next_attempt_at integer,
	last_error text,
	created_at integer not null,
	updated_at integer not null default 0,
	primary key (root_document_id, content_document_id)
)`

func (c *workspaceStore) migrateRootLocalDeleteIntents() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.withTx(func(tx *sql.Tx) error {
		exists, hasPhase, err := rootLocalDeleteIntentSchemaState(tx)
		if err != nil {
			return err
		}
		switch {
		case !exists:
			if _, err := tx.Exec(fmt.Sprintf(rootLocalDeleteIntentsV2Schema, "root_local_delete_intents")); err != nil {
				return err
			}
		case !hasPhase:
			if _, err := tx.Exec(fmt.Sprintf(rootLocalDeleteIntentsV2Schema, "root_local_delete_intents_v2")); err != nil {
				return err
			}
			if _, err := tx.Exec(`insert into root_local_delete_intents_v2 (
					root_document_id, content_document_id, phase, created_at, updated_at
				) select root_document_id, content_document_id, ?, created_at, created_at
				from root_local_delete_intents`, rootLocalDeletePhaseLegacyFloor); err != nil {
				return err
			}
			if _, err := tx.Exec(`drop table root_local_delete_intents`); err != nil {
				return err
			}
			if _, err := tx.Exec(`alter table root_local_delete_intents_v2 rename to root_local_delete_intents`); err != nil {
				return err
			}
		}
		return createRootLocalDeleteIntentIndexes(tx)
	})
}

func rootLocalDeleteIntentSchemaState(tx *sql.Tx) (exists bool, hasPhase bool, err error) {
	if tx == nil {
		return false, false, errors.New("sqlite transaction is required")
	}
	var count int
	if err := tx.QueryRow(`select count(*) from sqlite_master
		where type = 'table' and name = 'root_local_delete_intents'`).Scan(&count); err != nil {
		return false, false, err
	}
	if count == 0 {
		return false, false, nil
	}
	rows, err := tx.Query(`pragma table_info(root_local_delete_intents)`)
	if err != nil {
		return true, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return true, false, err
		}
		if name == "phase" {
			hasPhase = true
		}
	}
	return true, hasPhase, rows.Err()
}

func createRootLocalDeleteIntentIndexes(tx *sql.Tx) error {
	for _, statement := range []string{
		`create unique index if not exists root_local_delete_intents_tombstone_operation
			on root_local_delete_intents (tombstone_operation_id)
			where tombstone_operation_id is not null and tombstone_operation_id <> ''`,
		`create unique index if not exists root_local_delete_intents_restore_operation
			on root_local_delete_intents (restore_operation_id)
			where restore_operation_id is not null and restore_operation_id <> ''`,
		`create unique index if not exists root_local_delete_intents_materialized_path
			on root_local_delete_intents (root_document_id, materialized_path)
			where materialized_path <> ''`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (c *workspaceStore) storeRootLocalDeleteIntent(intent rootLocalDeleteIntent) error {
	if c == nil || c.db == nil {
		return nil
	}
	intent.RootDocumentID = strings.TrimSpace(intent.RootDocumentID)
	intent.ContentDocumentID = strings.TrimSpace(intent.ContentDocumentID)
	if intent.RootDocumentID == "" || intent.ContentDocumentID == "" {
		return errors.New("root and content document ids are required")
	}
	if intent.Phase == "" {
		intent.Phase = rootLocalDeletePhaseLegacyFloor
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = intent.CreatedAt
	}
	_, err := c.db.Exec(`insert into root_local_delete_intents (
			root_document_id, content_document_id, entry_id, desired_path,
			materialized_path, tombstone_operation_id, expected_window_generation,
			window_generation, opened_at, reverse_until, restore_operation_id,
			required_content_state_vector, observed_file_identity,
			observed_content_sha256, phase, attempts, next_attempt_at, last_error,
			created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(root_document_id, content_document_id) do update set
			entry_id = excluded.entry_id,
			desired_path = excluded.desired_path,
			materialized_path = excluded.materialized_path,
			tombstone_operation_id = excluded.tombstone_operation_id,
			expected_window_generation = excluded.expected_window_generation,
			window_generation = excluded.window_generation,
			opened_at = excluded.opened_at,
			reverse_until = excluded.reverse_until,
			restore_operation_id = excluded.restore_operation_id,
			required_content_state_vector = excluded.required_content_state_vector,
			observed_file_identity = excluded.observed_file_identity,
			observed_content_sha256 = excluded.observed_content_sha256,
			phase = excluded.phase,
			attempts = excluded.attempts,
			next_attempt_at = excluded.next_attempt_at,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		intent.RootDocumentID,
		intent.ContentDocumentID,
		intent.EntryID,
		intent.DesiredPath,
		intent.MaterializedPath,
		nullableRootIntentText(intent.TombstoneOperationID),
		nullablePositiveInt64(intent.ExpectedWindowGeneration),
		nullablePositiveInt64(intent.WindowGeneration),
		nullableUnixNano(intent.OpenedAt),
		nullableUnixNano(intent.ReverseUntil),
		nullableRootIntentText(intent.RestoreOperationID),
		intent.RequiredContentStateVector,
		nullableRootIntentText(intent.ObservedFileIdentity),
		nullableRootIntentText(intent.ObservedContentSHA256),
		intent.Phase,
		intent.Attempts,
		nullableUnixNano(intent.NextAttemptAt),
		nullableRootIntentText(intent.LastError),
		unixNano(intent.CreatedAt),
		unixNano(intent.UpdatedAt),
	)
	return err
}

func (c *workspaceStore) loadRootLocalDeleteIntent(rootDocumentID, contentDocumentID string) (rootLocalDeleteIntent, bool, error) {
	if c == nil || c.db == nil {
		return rootLocalDeleteIntent{}, false, nil
	}
	row := c.db.QueryRow(`select
		root_document_id, content_document_id, entry_id, desired_path,
		materialized_path, tombstone_operation_id, expected_window_generation,
		window_generation, opened_at, reverse_until, restore_operation_id,
		required_content_state_vector, observed_file_identity,
		observed_content_sha256, phase, attempts, next_attempt_at, last_error,
		created_at, updated_at
	from root_local_delete_intents
	where root_document_id = ? and content_document_id = ?`,
		strings.TrimSpace(rootDocumentID), strings.TrimSpace(contentDocumentID))
	intent, err := scanRootLocalDeleteIntent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return rootLocalDeleteIntent{}, false, nil
	}
	return intent, err == nil, err
}

type rootLocalDeleteIntentScanner interface {
	Scan(...any) error
}

func scanRootLocalDeleteIntent(row rootLocalDeleteIntentScanner) (rootLocalDeleteIntent, error) {
	var intent rootLocalDeleteIntent
	var tombstoneOperationID, restoreOperationID sql.NullString
	var expectedGeneration, generation, openedAt, reverseUntil, nextAttemptAt sql.NullInt64
	var observedFileIdentity, observedContentSHA256, lastError sql.NullString
	var createdAt, updatedAt int64
	if err := row.Scan(
		&intent.RootDocumentID,
		&intent.ContentDocumentID,
		&intent.EntryID,
		&intent.DesiredPath,
		&intent.MaterializedPath,
		&tombstoneOperationID,
		&expectedGeneration,
		&generation,
		&openedAt,
		&reverseUntil,
		&restoreOperationID,
		&intent.RequiredContentStateVector,
		&observedFileIdentity,
		&observedContentSHA256,
		&intent.Phase,
		&intent.Attempts,
		&nextAttemptAt,
		&lastError,
		&createdAt,
		&updatedAt,
	); err != nil {
		return rootLocalDeleteIntent{}, err
	}
	intent.TombstoneOperationID = tombstoneOperationID.String
	intent.ExpectedWindowGeneration = expectedGeneration.Int64
	intent.WindowGeneration = generation.Int64
	intent.OpenedAt = timeFromNullable(openedAt)
	intent.ReverseUntil = timeFromNullable(reverseUntil)
	intent.RestoreOperationID = restoreOperationID.String
	intent.ObservedFileIdentity = observedFileIdentity.String
	intent.ObservedContentSHA256 = observedContentSHA256.String
	intent.NextAttemptAt = timeFromNullable(nextAttemptAt)
	intent.LastError = lastError.String
	intent.CreatedAt = time.Unix(0, createdAt).UTC()
	intent.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return intent, nil
}

func nullableRootIntentText(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullablePositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
