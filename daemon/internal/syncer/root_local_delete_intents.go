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

func (c *workspaceStore) beginRootLocalDeleteIntent(intent rootLocalDeleteIntent) error {
	if c == nil || c.db == nil {
		return nil
	}
	intent.RootDocumentID = strings.TrimSpace(intent.RootDocumentID)
	intent.ContentDocumentID = strings.TrimSpace(intent.ContentDocumentID)
	intent.EntryID = strings.TrimSpace(intent.EntryID)
	intent.DesiredPath = strings.TrimSpace(intent.DesiredPath)
	intent.MaterializedPath = strings.TrimSpace(intent.MaterializedPath)
	intent.TombstoneOperationID = strings.TrimSpace(intent.TombstoneOperationID)
	if intent.RootDocumentID == "" || intent.ContentDocumentID == "" || intent.EntryID == "" ||
		intent.DesiredPath == "" || intent.MaterializedPath == "" || intent.TombstoneOperationID == "" {
		return errors.New("semantic root delete intent requires complete namespace and operation identity")
	}
	if intent.ExpectedWindowGeneration < 0 || intent.WindowGeneration != 0 || intent.RestoreOperationID != "" {
		return errors.New("semantic root delete intent has invalid initial generation or restore state")
	}
	if intent.Phase != rootLocalDeletePhaseTombstonePending {
		return fmt.Errorf("semantic root delete intent must begin in %q", rootLocalDeletePhaseTombstonePending)
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = intent.CreatedAt
	}
	return c.withTx(func(tx *sql.Tx) error {
		return insertRootLocalDeleteIntentTx(tx, intent)
	})
}

func (c *workspaceStore) recordRootLocalDeleteIntentAttempt(
	rootDocumentID,
	contentDocumentID,
	tombstoneOperationID string,
	expectedWindowGeneration int64,
	now time.Time,
) (rootLocalDeleteIntent, error) {
	if c == nil || c.db == nil {
		return rootLocalDeleteIntent{}, nil
	}
	if expectedWindowGeneration < 0 {
		return rootLocalDeleteIntent{}, errors.New("expected window generation cannot be negative")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var intent rootLocalDeleteIntent
	err := c.withTx(func(tx *sql.Tx) error {
		result, err := tx.Exec(`update root_local_delete_intents
			set attempts = attempts + 1,
				next_attempt_at = null,
				last_error = null,
				updated_at = ?
			where root_document_id = ?
				and content_document_id = ?
				and phase = ?
				and tombstone_operation_id = ?
				and coalesce(expected_window_generation, 0) = ?`,
			unixNano(now), strings.TrimSpace(rootDocumentID), strings.TrimSpace(contentDocumentID),
			rootLocalDeletePhaseTombstonePending, strings.TrimSpace(tombstoneOperationID), expectedWindowGeneration)
		if err != nil {
			return err
		}
		if err := requireSingleRootIntentTransition(result, "record tombstone attempt"); err != nil {
			return err
		}
		intent, err = loadRootLocalDeleteIntentTx(tx, rootDocumentID, contentDocumentID)
		return err
	})
	return intent, err
}

func (c *workspaceStore) replaceRootLocalDeleteIntentAfterGenerationConflict(
	rootDocumentID,
	contentDocumentID,
	oldOperationID string,
	oldExpectedGeneration int64,
	freshOperationID string,
	currentGeneration int64,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	oldOperationID = strings.TrimSpace(oldOperationID)
	freshOperationID = strings.TrimSpace(freshOperationID)
	if oldOperationID == "" || freshOperationID == "" || oldOperationID == freshOperationID {
		return errors.New("generation conflict replacement requires a distinct fresh operation id")
	}
	if oldExpectedGeneration < 0 || currentGeneration < 0 {
		return errors.New("window generations cannot be negative")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.withTx(func(tx *sql.Tx) error {
		intent, err := loadRootLocalDeleteIntentTx(tx, rootDocumentID, contentDocumentID)
		if err != nil {
			return err
		}
		if intent.Phase != rootLocalDeletePhaseTombstonePending ||
			intent.TombstoneOperationID != oldOperationID ||
			intent.ExpectedWindowGeneration != oldExpectedGeneration ||
			intent.WindowGeneration != 0 {
			return errors.New("generation conflict no longer matches the pending tombstone operation")
		}
		result, err := tx.Exec(`delete from root_local_delete_intents
			where root_document_id = ? and content_document_id = ?
				and phase = ? and tombstone_operation_id = ?
				and coalesce(expected_window_generation, 0) = ?`,
			strings.TrimSpace(rootDocumentID), strings.TrimSpace(contentDocumentID),
			rootLocalDeletePhaseTombstonePending, oldOperationID, oldExpectedGeneration)
		if err != nil {
			return err
		}
		if err := requireSingleRootIntentTransition(result, "terminalize conflicted tombstone operation"); err != nil {
			return err
		}
		intent.TombstoneOperationID = freshOperationID
		intent.ExpectedWindowGeneration = currentGeneration
		intent.WindowGeneration = 0
		intent.OpenedAt = time.Time{}
		intent.ReverseUntil = time.Time{}
		intent.RestoreOperationID = ""
		intent.RequiredContentStateVector = nil
		intent.ObservedFileIdentity = ""
		intent.ObservedContentSHA256 = ""
		intent.Phase = rootLocalDeletePhaseTombstonePending
		intent.Attempts = 0
		intent.NextAttemptAt = time.Time{}
		intent.LastError = ""
		intent.CreatedAt = now
		intent.UpdatedAt = now
		return insertRootLocalDeleteIntentTx(tx, intent)
	})
}

func (c *workspaceStore) acceptRootLocalDeleteWindow(
	rootDocumentID,
	contentDocumentID,
	tombstoneOperationID string,
	expectedWindowGeneration,
	windowGeneration int64,
	openedAt,
	reverseUntil,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	if expectedWindowGeneration < 0 || windowGeneration <= 0 {
		return errors.New("accepted reverse window requires a positive generation")
	}
	if openedAt.IsZero() || !reverseUntil.After(openedAt) {
		return errors.New("accepted reverse window requires a valid backend deadline")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set window_generation = ?,
			opened_at = ?,
			reverse_until = ?,
			phase = ?,
			attempts = 0,
			next_attempt_at = null,
			last_error = null,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and phase = ?
			and tombstone_operation_id = ?
			and coalesce(expected_window_generation, 0) = ?
			and window_generation is null`,
		windowGeneration, unixNano(openedAt), unixNano(reverseUntil), rootLocalDeletePhaseWindowOpen,
		unixNano(now), strings.TrimSpace(rootDocumentID), strings.TrimSpace(contentDocumentID),
		rootLocalDeletePhaseTombstonePending, strings.TrimSpace(tombstoneOperationID), expectedWindowGeneration)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "accept reverse window")
}

func (c *workspaceStore) beginRootLocalDeleteRestore(
	rootDocumentID,
	contentDocumentID,
	tombstoneOperationID,
	restoreOperationID string,
	windowGeneration int64,
	observedFileIdentity,
	observedContentSHA256 string,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	tombstoneOperationID = strings.TrimSpace(tombstoneOperationID)
	restoreOperationID = strings.TrimSpace(restoreOperationID)
	if tombstoneOperationID == "" || restoreOperationID == "" || tombstoneOperationID == restoreOperationID {
		return errors.New("restore requires distinct durable tombstone and restore operation ids")
	}
	if windowGeneration <= 0 {
		return errors.New("restore requires the accepted positive window generation")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set restore_operation_id = ?,
			observed_file_identity = ?,
			observed_content_sha256 = ?,
			phase = ?,
			attempts = 0,
			next_attempt_at = null,
			last_error = null,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and phase = ?
			and tombstone_operation_id = ?
			and window_generation = ?
			and restore_operation_id is null`,
		restoreOperationID, nullableRootIntentText(observedFileIdentity), nullableRootIntentText(observedContentSHA256),
		rootLocalDeletePhaseContentSyncing, unixNano(now), strings.TrimSpace(rootDocumentID),
		strings.TrimSpace(contentDocumentID), rootLocalDeletePhaseWindowOpen, tombstoneOperationID, windowGeneration)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "begin reverse restore")
}

func (c *workspaceStore) markRootLocalDeleteRestorePending(
	rootDocumentID,
	contentDocumentID,
	tombstoneOperationID,
	restoreOperationID string,
	windowGeneration int64,
	requiredContentStateVector []byte,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	if strings.TrimSpace(tombstoneOperationID) == "" || strings.TrimSpace(restoreOperationID) == "" ||
		strings.TrimSpace(tombstoneOperationID) == strings.TrimSpace(restoreOperationID) {
		return errors.New("restore-ready transition requires distinct operation ids")
	}
	if windowGeneration <= 0 || len(requiredContentStateVector) == 0 {
		return errors.New("restore-ready transition requires accepted generation and content frontier")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set required_content_state_vector = ?,
			phase = ?,
			attempts = 0,
			next_attempt_at = null,
			last_error = null,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and phase = ?
			and tombstone_operation_id = ?
			and restore_operation_id = ?
			and window_generation = ?`,
		requiredContentStateVector, rootLocalDeletePhaseRestorePending, unixNano(now),
		strings.TrimSpace(rootDocumentID), strings.TrimSpace(contentDocumentID),
		rootLocalDeletePhaseContentSyncing, strings.TrimSpace(tombstoneOperationID),
		strings.TrimSpace(restoreOperationID), windowGeneration)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "mark reverse restore pending")
}

func insertRootLocalDeleteIntentTx(tx *sql.Tx, intent rootLocalDeleteIntent) error {
	if tx == nil {
		return errors.New("sqlite transaction is required")
	}
	_, err := tx.Exec(`insert into root_local_delete_intents (
			root_document_id, content_document_id, entry_id, desired_path,
			materialized_path, tombstone_operation_id, expected_window_generation,
			window_generation, opened_at, reverse_until, restore_operation_id,
			required_content_state_vector, observed_file_identity,
			observed_content_sha256, phase, attempts, next_attempt_at, last_error,
			created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.RootDocumentID, intent.ContentDocumentID, intent.EntryID, intent.DesiredPath,
		intent.MaterializedPath, nullableRootIntentText(intent.TombstoneOperationID),
		nullablePositiveInt64(intent.ExpectedWindowGeneration), nullablePositiveInt64(intent.WindowGeneration),
		nullableUnixNano(intent.OpenedAt), nullableUnixNano(intent.ReverseUntil),
		nullableRootIntentText(intent.RestoreOperationID), intent.RequiredContentStateVector,
		nullableRootIntentText(intent.ObservedFileIdentity), nullableRootIntentText(intent.ObservedContentSHA256),
		intent.Phase, intent.Attempts, nullableUnixNano(intent.NextAttemptAt), nullableRootIntentText(intent.LastError),
		unixNano(intent.CreatedAt), unixNano(intent.UpdatedAt))
	return err
}

func loadRootLocalDeleteIntentTx(tx *sql.Tx, rootDocumentID, contentDocumentID string) (rootLocalDeleteIntent, error) {
	if tx == nil {
		return rootLocalDeleteIntent{}, errors.New("sqlite transaction is required")
	}
	return scanRootLocalDeleteIntent(tx.QueryRow(`select
		root_document_id, content_document_id, entry_id, desired_path,
		materialized_path, tombstone_operation_id, expected_window_generation,
		window_generation, opened_at, reverse_until, restore_operation_id,
		required_content_state_vector, observed_file_identity,
		observed_content_sha256, phase, attempts, next_attempt_at, last_error,
		created_at, updated_at
	from root_local_delete_intents
	where root_document_id = ? and content_document_id = ?`,
		strings.TrimSpace(rootDocumentID), strings.TrimSpace(contentDocumentID)))
}

func requireSingleRootIntentTransition(result sql.Result, transition string) error {
	if result == nil {
		return fmt.Errorf("%s did not produce a sqlite result", transition)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%s matched %d intents, want 1", transition, count)
	}
	return nil
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

func (c *workspaceStore) loadRootLocalDeleteIntents(rootDocumentID string) ([]rootLocalDeleteIntent, error) {
	if c == nil || c.db == nil {
		return nil, nil
	}
	rows, err := c.db.Query(`select
		root_document_id, content_document_id, entry_id, desired_path,
		materialized_path, tombstone_operation_id, expected_window_generation,
		window_generation, opened_at, reverse_until, restore_operation_id,
		required_content_state_vector, observed_file_identity,
		observed_content_sha256, phase, attempts, next_attempt_at, last_error,
		created_at, updated_at
	from root_local_delete_intents
	where root_document_id = ?
	order by created_at, content_document_id`, strings.TrimSpace(rootDocumentID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []rootLocalDeleteIntent
	for rows.Next() {
		intent, err := scanRootLocalDeleteIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (c *workspaceStore) updateRootLocalDeleteObservation(
	intent rootLocalDeleteIntent,
	fileIdentity,
	contentSHA256 string,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set observed_file_identity = ?,
			observed_content_sha256 = ?,
			required_content_state_vector = null,
			phase = ?,
			attempts = 0,
			next_attempt_at = null,
			last_error = null,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and tombstone_operation_id = ?
			and window_generation = ?
			and restore_operation_id = ?
			and phase in (?, ?)`,
		nullableRootIntentText(fileIdentity), nullableRootIntentText(contentSHA256),
		rootLocalDeletePhaseContentSyncing, unixNano(now),
		strings.TrimSpace(intent.RootDocumentID), strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.TombstoneOperationID), intent.WindowGeneration,
		strings.TrimSpace(intent.RestoreOperationID),
		rootLocalDeletePhaseContentSyncing, rootLocalDeletePhaseRestorePending)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "refresh reverse-window content observation")
}

func (c *workspaceStore) recordRootLocalDeleteRestoreAttempt(intent rootLocalDeleteIntent, now time.Time) error {
	if c == nil || c.db == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set attempts = attempts + 1,
			next_attempt_at = null,
			last_error = null,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and tombstone_operation_id = ?
			and restore_operation_id = ?
			and window_generation = ?
			and phase = ?`,
		unixNano(now), strings.TrimSpace(intent.RootDocumentID), strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.TombstoneOperationID), strings.TrimSpace(intent.RestoreOperationID),
		intent.WindowGeneration, rootLocalDeletePhaseRestorePending)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "record reverse-window restore attempt")
}

func (c *workspaceStore) markRootLocalDeleteProjectionPending(intent rootLocalDeleteIntent, now time.Time) error {
	if c == nil || c.db == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set phase = ?,
			attempts = 0,
			next_attempt_at = null,
			last_error = null,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and tombstone_operation_id = ?
			and restore_operation_id = ?
			and window_generation = ?
			and phase = ?`,
		rootLocalDeletePhaseProjectionPending, unixNano(now),
		strings.TrimSpace(intent.RootDocumentID), strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.TombstoneOperationID), strings.TrimSpace(intent.RestoreOperationID),
		intent.WindowGeneration, rootLocalDeletePhaseRestorePending)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "mark reverse-window projection pending")
}

func (c *workspaceStore) replaceRootLocalDeleteIntentAfterProjectionAbsence(
	intent rootLocalDeleteIntent,
	freshOperationID string,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	freshOperationID = strings.TrimSpace(freshOperationID)
	if freshOperationID == "" || freshOperationID == strings.TrimSpace(intent.TombstoneOperationID) {
		return errors.New("projection absence requires a distinct fresh tombstone operation")
	}
	if intent.WindowGeneration <= 0 {
		return errors.New("projection absence requires the accepted positive generation")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set tombstone_operation_id = ?,
			expected_window_generation = ?,
			window_generation = null,
			opened_at = null,
			reverse_until = null,
			restore_operation_id = null,
			required_content_state_vector = null,
			observed_file_identity = null,
			observed_content_sha256 = null,
			phase = ?,
			attempts = 0,
			next_attempt_at = null,
			last_error = null,
			created_at = ?,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and entry_id = ?
			and desired_path = ?
			and materialized_path = ?
			and tombstone_operation_id = ?
			and restore_operation_id = ?
			and window_generation = ?
			and phase = ?`,
		freshOperationID, intent.WindowGeneration, rootLocalDeletePhaseTombstonePending,
		unixNano(now), unixNano(now),
		strings.TrimSpace(intent.RootDocumentID), strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.EntryID), strings.TrimSpace(intent.DesiredPath),
		strings.TrimSpace(intent.MaterializedPath), strings.TrimSpace(intent.TombstoneOperationID),
		strings.TrimSpace(intent.RestoreOperationID), intent.WindowGeneration,
		rootLocalDeletePhaseProjectionPending)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "replace absent restored projection")
}

func (c *workspaceStore) updateRootLocalDeleteProjectionObservation(
	intent rootLocalDeleteIntent,
	fileIdentity,
	contentSHA256 string,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set observed_file_identity = ?,
			observed_content_sha256 = ?,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and tombstone_operation_id = ?
			and restore_operation_id = ?
			and window_generation = ?
			and phase = ?`,
		nullableRootIntentText(fileIdentity), nullableRootIntentText(contentSHA256), unixNano(now),
		strings.TrimSpace(intent.RootDocumentID), strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.TombstoneOperationID), strings.TrimSpace(intent.RestoreOperationID),
		intent.WindowGeneration, rootLocalDeletePhaseProjectionPending)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "refresh restored projection observation")
}

func (c *workspaceStore) rekeyRootLocalDeleteProjectionPath(
	intent rootLocalDeleteIntent,
	materializedPath,
	fileIdentity,
	contentSHA256 string,
	now time.Time,
) error {
	if c == nil || c.db == nil {
		return nil
	}
	materializedPath = strings.TrimSpace(materializedPath)
	if materializedPath == "" || materializedPath == strings.TrimSpace(intent.MaterializedPath) {
		return errors.New("reverse-window projection rekey requires a distinct materialized path")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := c.db.Exec(`update root_local_delete_intents
		set materialized_path = ?,
			observed_file_identity = ?,
			observed_content_sha256 = ?,
			updated_at = ?
		where root_document_id = ?
			and content_document_id = ?
			and entry_id = ?
			and desired_path = ?
			and materialized_path = ?
			and tombstone_operation_id = ?
			and restore_operation_id = ?
			and window_generation = ?
			and phase = ?`,
		materializedPath, nullableRootIntentText(fileIdentity), nullableRootIntentText(contentSHA256), unixNano(now),
		strings.TrimSpace(intent.RootDocumentID), strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.EntryID), strings.TrimSpace(intent.DesiredPath), strings.TrimSpace(intent.MaterializedPath),
		strings.TrimSpace(intent.TombstoneOperationID), strings.TrimSpace(intent.RestoreOperationID),
		intent.WindowGeneration, rootLocalDeletePhaseProjectionPending)
	if err != nil {
		return err
	}
	return requireSingleRootIntentTransition(result, "rekey restored projection path")
}

func (c *workspaceStore) deleteRootLocalDeleteIntent(intent rootLocalDeleteIntent) error {
	if c == nil || c.db == nil {
		return nil
	}
	result, err := c.db.Exec(`delete from root_local_delete_intents
		where root_document_id = ?
			and content_document_id = ?
			and entry_id = ?
			and desired_path = ?
			and materialized_path = ?
			and phase = ?
			and coalesce(tombstone_operation_id, '') = ?
			and coalesce(window_generation, 0) = ?`,
		strings.TrimSpace(intent.RootDocumentID),
		strings.TrimSpace(intent.ContentDocumentID),
		strings.TrimSpace(intent.EntryID),
		strings.TrimSpace(intent.DesiredPath),
		strings.TrimSpace(intent.MaterializedPath),
		intent.Phase,
		strings.TrimSpace(intent.TombstoneOperationID),
		intent.WindowGeneration,
	)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errRootLocalDeleteIntentChanged
	}
	return nil
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
