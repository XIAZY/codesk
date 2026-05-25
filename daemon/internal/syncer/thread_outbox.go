package syncer

import (
	"database/sql"
	"time"
)

const (
	threadIntentPending = "pending"
	threadIntentReady   = "ready"
	threadIntentFailed  = "failed"
)

type threadOutboxIntent struct {
	IntentID       string                      `json:"intentId"`
	IdempotencyKey string                      `json:"idempotencyKey"`
	DocumentID     string                      `json:"documentId"`
	DocumentPath   string                      `json:"documentPath"`
	ActorID        string                      `json:"actorId"`
	ActorType      string                      `json:"actorType"`
	RunID          string                      `json:"runId,omitempty"`
	Request        createThreadPayload         `json:"request"`
	Resolved       *backendCreateThreadPayload `json:"resolved,omitempty"`
	Status         string                      `json:"status"`
	Attempts       int                         `json:"attempts,omitempty"`
	NextAttemptAt  time.Time                   `json:"nextAttemptAt,omitempty"`
	LastAttemptAt  time.Time                   `json:"lastAttemptAt,omitempty"`
	LastError      string                      `json:"lastError,omitempty"`
	LastStatusCode int                         `json:"lastStatusCode,omitempty"`
	CreatedAt      time.Time                   `json:"createdAt"`
	UpdatedAt      time.Time                   `json:"updatedAt"`
}

func (c *workspaceStore) appendThreadIntent(documentID string, intent threadOutboxIntent) error {
	if c == nil || documentID == "" {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.writeThreadIntentLocked(documentID, intent)
}

func (c *workspaceStore) loadThreadIntentsLocked(_ *documentCacheEntry, documentID string) ([]threadOutboxIntent, error) {
	if c == nil || documentID == "" {
		return nil, nil
	}
	rows, err := c.db.Query(`select intent_id, document_id, status, idempotency_key, request_json, resolved_payload_json,
			actor_id, actor_type, run_id, attempts, next_attempt_at, last_attempt_at, last_status_code, last_error, created_at, updated_at
		from thread_outbox where document_id = ? order by created_at, intent_id`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanThreadIntents(rows)
}

func (c *workspaceStore) pendingThreadIntentCountLocked(entry *documentCacheEntry, documentID string) (int, error) {
	intents, err := c.loadThreadIntentsLocked(entry, documentID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, intent := range intents {
		if intent.Status == threadIntentPending {
			count++
		}
	}
	return count, nil
}

func (c *workspaceStore) materializableThreadIntentCountLocked(entry *documentCacheEntry, documentID string) (int, error) {
	intents, err := c.loadThreadIntentsLocked(entry, documentID)
	if err != nil {
		return 0, err
	}
	contentKnown := false
	contentChecked := false
	count := 0
	for _, intent := range intents {
		if intent.Status != threadIntentPending {
			continue
		}
		if threadIntentHasRelativeAnchors(intent.Request) {
			count++
			continue
		}
		if !contentChecked {
			contentKnown, err = c.documentContentKnown(documentID)
			if err != nil {
				return 0, err
			}
			contentChecked = true
		}
		if contentKnown {
			count++
		}
	}
	return count, nil
}

func (c *workspaceStore) loadDueReadyThreadIntents(now time.Time) ([]threadOutboxIntent, error) {
	if c == nil {
		return nil, nil
	}
	rows, err := c.db.Query(`select intent_id, document_id, status, idempotency_key, request_json, resolved_payload_json,
			actor_id, actor_type, run_id, attempts, next_attempt_at, last_attempt_at, last_status_code, last_error, created_at, updated_at
		from thread_outbox
		where status = ? and resolved_payload_json is not null and (next_attempt_at is null or next_attempt_at <= ?)
		order by created_at, intent_id`, threadIntentReady, unixNano(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanThreadIntents(rows)
}

func (c *workspaceStore) nextThreadIntentAttemptAt(now time.Time) (time.Time, bool, error) {
	if c == nil {
		return time.Time{}, false, nil
	}
	var next sql.NullInt64
	if err := c.db.QueryRow(`select min(coalesce(next_attempt_at, ?)) from thread_outbox where status = ? and resolved_payload_json is not null`, unixNano(now), threadIntentReady).Scan(&next); err != nil {
		return time.Time{}, false, err
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(0, next.Int64).UTC(), true, nil
}

func (c *workspaceStore) updateThreadIntent(intent threadOutboxIntent) error {
	if c == nil || intent.DocumentID == "" {
		return nil
	}
	entry := c.entryFor(intent.DocumentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.writeThreadIntentLocked(intent.DocumentID, intent)
}

func (c *workspaceStore) deleteThreadIntent(intent threadOutboxIntent) error {
	if c == nil || intent.DocumentID == "" || intent.IntentID == "" {
		return nil
	}
	entry := c.entryFor(intent.DocumentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	_, err := c.db.Exec(`delete from thread_outbox where intent_id = ?`, intent.IntentID)
	return err
}

func (c *workspaceStore) writeThreadIntentLocked(documentID string, intent threadOutboxIntent) error {
	if c == nil || documentID == "" || intent.IntentID == "" {
		return nil
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	intent.UpdatedAt = now
	if intent.Status == "" {
		intent.Status = threadIntentPending
	}
	if intent.DocumentID == "" {
		intent.DocumentID = documentID
	}
	requestJSON, err := marshalJSON(intent.Request)
	if err != nil {
		return err
	}
	resolvedJSON := any(nil)
	if intent.Resolved != nil {
		value, err := marshalJSON(intent.Resolved)
		if err != nil {
			return err
		}
		resolvedJSON = value
	}
	return c.withTx(func(tx *sql.Tx) error {
		if _, err := c.ensureDocumentTx(tx, documentID, intent.DocumentPath, 0, now); err != nil {
			return err
		}
		_, err := tx.Exec(`insert into thread_outbox (
				intent_id, document_id, status, idempotency_key, request_json, resolved_payload_json,
				actor_id, actor_type, run_id, attempts, next_attempt_at, last_attempt_at,
				last_status_code, last_error, created_at, updated_at
			) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			on conflict(intent_id) do update set
				status = excluded.status,
				request_json = excluded.request_json,
				resolved_payload_json = excluded.resolved_payload_json,
				actor_id = excluded.actor_id,
				actor_type = excluded.actor_type,
				run_id = excluded.run_id,
				attempts = excluded.attempts,
				next_attempt_at = excluded.next_attempt_at,
				last_attempt_at = excluded.last_attempt_at,
				last_status_code = excluded.last_status_code,
				last_error = excluded.last_error,
				updated_at = excluded.updated_at`,
			intent.IntentID, intent.DocumentID, intent.Status, intent.IdempotencyKey, requestJSON, resolvedJSON,
			intent.ActorID, intent.ActorType, intent.RunID, intent.Attempts, nullableUnixNano(intent.NextAttemptAt), nullableUnixNano(intent.LastAttemptAt),
			nullableInt(intent.LastStatusCode), nullableString(intent.LastError), unixNano(intent.CreatedAt), unixNano(intent.UpdatedAt))
		return err
	})
}

func scanThreadIntents(rows *sql.Rows) ([]threadOutboxIntent, error) {
	intents := []threadOutboxIntent{}
	for rows.Next() {
		var intent threadOutboxIntent
		var requestJSON string
		var resolvedJSON sql.NullString
		var actorID, actorType, runID, lastError sql.NullString
		var nextAttemptAt, lastAttemptAt, lastStatusCode sql.NullInt64
		var createdAt, updatedAt int64
		if err := rows.Scan(&intent.IntentID, &intent.DocumentID, &intent.Status, &intent.IdempotencyKey, &requestJSON, &resolvedJSON,
			&actorID, &actorType, &runID, &intent.Attempts, &nextAttemptAt, &lastAttemptAt, &lastStatusCode, &lastError, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if err := unmarshalJSON(requestJSON, &intent.Request); err != nil {
			return nil, err
		}
		if resolvedJSON.Valid && resolvedJSON.String != "" {
			var resolved backendCreateThreadPayload
			if err := unmarshalJSON(resolvedJSON.String, &resolved); err != nil {
				return nil, err
			}
			intent.Resolved = &resolved
		}
		intent.ActorID = actorID.String
		intent.ActorType = actorType.String
		intent.RunID = runID.String
		intent.NextAttemptAt = timeFromNullable(nextAttemptAt)
		intent.LastAttemptAt = timeFromNullable(lastAttemptAt)
		if lastStatusCode.Valid {
			intent.LastStatusCode = int(lastStatusCode.Int64)
		}
		intent.LastError = lastError.String
		intent.CreatedAt = time.Unix(0, createdAt).UTC()
		intent.UpdatedAt = time.Unix(0, updatedAt).UTC()
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortThreadIntents(intents)
	return intents, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
