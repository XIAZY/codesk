package syncer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type StreamMutation struct {
	StreamID             string
	KindHint             string
	MutationKey          string
	UpdateBytes          []byte
	ActorID              string
	ActorType            string
	Reason               string
	DependsOnStreamID    string
	DependsOnMutationKey string
}

type StreamOutboxRow struct {
	ID             int64
	StreamID       string
	MutationKey    string
	UpdateSHA256   string
	UpdateBytes    []byte
	ActorID        string
	ActorType      string
	Reason         string
	KindHint       sql.NullString
	DependsOnID    sql.NullInt64
	LocalAppliedAt sql.NullString
	SentAt         sql.NullString
	AckedAt        sql.NullString
	AckUpdateID    sql.NullInt64
	DroppedAt      sql.NullString
	DropReason     sql.NullString
}

type StreamInboxRow struct {
	ID             int64
	StreamID       string
	UpdateSHA256   string
	UpdateBytes    []byte
	RemoteUpdateID sql.NullInt64
	ReceivedAt     string
	AppliedAt      sql.NullString
}

func (s *WorkspaceStateDB) EnsureLocalStream(ctx context.Context, streamID string, kind string) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	return ensureLocalStreamTx(ctx, nil, streamID, kind, s.db)
}

func ensureLocalStreamTx(ctx context.Context, tx *sql.Tx, streamID string, kind string, db ...*sql.DB) error {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return errors.New("stream id is required")
	}
	if kind = strings.TrimSpace(kind); kind == "" {
		kind = "unknown"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `
		INSERT INTO streams(stream_id, kind, latest_update_id, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(stream_id) DO UPDATE SET
			kind = CASE
				WHEN streams.kind = 'unknown' THEN excluded.kind
				WHEN excluded.kind = 'unknown' THEN streams.kind
				ELSE streams.kind
			END,
			updated_at = excluded.updated_at`
	args := []any{streamID, kind, now, now}
	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, query, args...)
	} else {
		if len(db) == 0 || db[0] == nil {
			return errors.New("state db is required")
		}
		_, err = db[0].ExecContext(ctx, query, args...)
	}
	return err
}

func (s *WorkspaceStateDB) UpsertOutbox(ctx context.Context, mutation StreamMutation) (StreamOutboxRow, error) {
	if s == nil || s.db == nil {
		return StreamOutboxRow{}, errors.New("state db is required")
	}
	mutation.StreamID = strings.TrimSpace(mutation.StreamID)
	mutation.MutationKey = strings.TrimSpace(mutation.MutationKey)
	if mutation.StreamID == "" {
		return StreamOutboxRow{}, errors.New("stream id is required")
	}
	if mutation.MutationKey == "" {
		return StreamOutboxRow{}, errors.New("mutation key is required")
	}
	if len(mutation.UpdateBytes) == 0 {
		return StreamOutboxRow{}, errors.New("mutation update is required")
	}
	if mutation.ActorID = strings.TrimSpace(mutation.ActorID); mutation.ActorID == "" {
		mutation.ActorID = "daemon"
	}
	if mutation.ActorType = strings.TrimSpace(mutation.ActorType); mutation.ActorType == "" {
		mutation.ActorType = "daemon"
	}
	if mutation.Reason = strings.TrimSpace(mutation.Reason); mutation.Reason == "" {
		mutation.Reason = "unspecified"
	}
	if err := s.EnsureLocalStream(ctx, mutation.StreamID, mutation.KindHint); err != nil {
		return StreamOutboxRow{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StreamOutboxRow{}, err
	}
	defer tx.Rollback()

	var dependsOnID any
	if strings.TrimSpace(mutation.DependsOnMutationKey) != "" {
		id, err := resolveOutboxDependencyID(ctx, tx, mutation.DependsOnStreamID, mutation.DependsOnMutationKey)
		if err != nil {
			return StreamOutboxRow{}, err
		}
		dependsOnID = id
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	sha := sha256HexBytes(mutation.UpdateBytes)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO stream_outbox(
			stream_id, mutation_key, update_sha256, update_bytes,
			actor_id, actor_type, reason, kind_hint, depends_on_id,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stream_id, mutation_key) DO NOTHING`,
		mutation.StreamID,
		mutation.MutationKey,
		sha,
		mutation.UpdateBytes,
		mutation.ActorID,
		mutation.ActorType,
		mutation.Reason,
		nullString(mutation.KindHint),
		dependsOnID,
		now,
	)
	if err != nil {
		return StreamOutboxRow{}, err
	}
	row, err := getOutboxByMutationKeyTx(ctx, tx, mutation.StreamID, mutation.MutationKey)
	if err != nil {
		return StreamOutboxRow{}, err
	}
	if row.UpdateSHA256 != sha {
		if row.LocalAppliedAt.Valid || row.SentAt.Valid || row.AckedAt.Valid {
			if err := tx.Commit(); err != nil {
				return StreamOutboxRow{}, err
			}
			return row, nil
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE stream_outbox
			   SET update_sha256 = ?,
			       update_bytes = ?,
			       actor_id = ?,
			       actor_type = ?,
			       reason = ?,
			       kind_hint = ?,
			       depends_on_id = ?,
			       dropped_at = NULL,
			       drop_reason = NULL
			 WHERE id = ?`,
			sha,
			mutation.UpdateBytes,
			mutation.ActorID,
			mutation.ActorType,
			mutation.Reason,
			nullString(mutation.KindHint),
			dependsOnID,
			row.ID,
		)
		if err != nil {
			return StreamOutboxRow{}, err
		}
		row, err = getOutboxByMutationKeyTx(ctx, tx, mutation.StreamID, mutation.MutationKey)
		if err != nil {
			return StreamOutboxRow{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return StreamOutboxRow{}, err
	}
	return row, nil
}

func (s *WorkspaceStateDB) OutboxDependencyAcked(ctx context.Context, outboxID int64) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("state db is required")
	}
	var ok int
	err := s.db.QueryRowContext(ctx, `
		SELECT CASE
			WHEN depends_on_id IS NULL THEN 1
			WHEN EXISTS (SELECT 1 FROM stream_outbox dep WHERE dep.id = stream_outbox.depends_on_id AND dep.acked_at IS NOT NULL) THEN 1
			ELSE 0
		END
		FROM stream_outbox WHERE id = ?`, outboxID).Scan(&ok)
	return ok != 0, err
}

func (s *WorkspaceStateDB) ReadyLocalOutbox(ctx context.Context, streamID string, limit int) ([]StreamOutboxRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	return readyLocalOutboxWithQueryer(ctx, s.db, streamID, limit)
}

func readyLocalOutboxTx(ctx context.Context, tx *sql.Tx, streamID string, limit int) ([]StreamOutboxRow, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	return readyLocalOutboxWithQueryer(ctx, tx, streamID, limit)
}

func readyLocalOutboxWithQueryer(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, streamID string, limit int) ([]StreamOutboxRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT `+outboxColumns+`
		  FROM stream_outbox
		 WHERE stream_id = ?
		   AND dropped_at IS NULL
		   AND local_applied_at IS NULL
		   AND (
		     depends_on_id IS NULL
		     OR EXISTS (
		       SELECT 1 FROM stream_outbox dep
		        WHERE dep.id = stream_outbox.depends_on_id
		          AND dep.acked_at IS NOT NULL
		     )
		   )
		 ORDER BY id ASC
		 LIMIT ?`, streamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutboxRows(rows)
}

func (s *WorkspaceStateDB) MarkOutboxLocallyApplied(ctx context.Context, outboxID int64, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	return markOutboxLocallyAppliedWithExecer(ctx, s.db, outboxID, at)
}

func markOutboxLocallyAppliedTx(ctx context.Context, tx *sql.Tx, outboxID int64, at time.Time) error {
	if tx == nil {
		return errors.New("transaction is required")
	}
	return markOutboxLocallyAppliedWithExecer(ctx, tx, outboxID, at)
}

func markOutboxLocallyAppliedWithExecer(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, outboxID int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := execer.ExecContext(ctx, `UPDATE stream_outbox SET local_applied_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), outboxID)
	return err
}

func (s *WorkspaceStateDB) NextSendableOutboxRow(ctx context.Context) (*StreamOutboxRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT `+outboxColumns+`
		  FROM stream_outbox
		 WHERE acked_at IS NULL
		   AND dropped_at IS NULL
		   AND local_applied_at IS NOT NULL
		   AND (
		     depends_on_id IS NULL
		     OR EXISTS (
		       SELECT 1 FROM stream_outbox dep
		        WHERE dep.id = stream_outbox.depends_on_id
		          AND dep.acked_at IS NOT NULL
		     )
		   )
		 ORDER BY id ASC
		 LIMIT 1`)
	outbox, err := scanOutboxRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &outbox, nil
}

func (s *WorkspaceStateDB) DependentOutboxStreamIDs(ctx context.Context, outboxID int64) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	if outboxID <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT stream_id
		  FROM stream_outbox
		 WHERE depends_on_id = ?
		   AND local_applied_at IS NULL
		   AND acked_at IS NULL
		   AND dropped_at IS NULL
		 ORDER BY stream_id`, outboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	streamIDs := []string{}
	for rows.Next() {
		var streamID string
		if err := rows.Scan(&streamID); err != nil {
			return nil, err
		}
		if strings.TrimSpace(streamID) != "" {
			streamIDs = append(streamIDs, streamID)
		}
	}
	return streamIDs, rows.Err()
}

func (s *WorkspaceStateDB) PendingOutboxCount(ctx context.Context, streamID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return 0, errors.New("stream id is required")
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM stream_outbox
		 WHERE stream_id = ?
		   AND acked_at IS NULL
		   AND dropped_at IS NULL`, streamID).Scan(&count)
	return count, err
}

func (s *WorkspaceStateDB) HasOutboxCreatedAfter(ctx context.Context, streamID string, after string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	after = strings.TrimSpace(after)
	if streamID == "" || after == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM stream_outbox
		 WHERE stream_id = ?
		   AND created_at > ?`, streamID, after).Scan(&count)
	return count > 0, err
}

func (s *WorkspaceStateDB) HasOutbox(ctx context.Context, streamID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM stream_outbox
		 WHERE stream_id = ?`, streamID).Scan(&count)
	return count > 0, err
}

func (s *WorkspaceStateDB) MarkOutboxSent(ctx context.Context, outboxID int64, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE stream_outbox SET sent_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), outboxID)
	return err
}

func (s *WorkspaceStateDB) MarkOutboxAcked(ctx context.Context, outboxID int64, ackUpdateID int64, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE stream_outbox SET acked_at = ?, ack_update_id = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), ackUpdateID, outboxID)
	return err
}

func (s *WorkspaceStateDB) MarkOutboxDropped(ctx context.Context, outboxID int64, reason string, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "dropped"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE stream_outbox
		   SET dropped_at = ?,
		       drop_reason = ?
		 WHERE id = ?
		   AND acked_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano),
		reason,
		outboxID,
	)
	return err
}

func (s *WorkspaceStateDB) DropPendingOutboxForStream(ctx context.Context, streamID string, reason string, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return errors.New("stream id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "stream-not-live"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE stream_outbox
		   SET dropped_at = ?,
		       drop_reason = ?
		 WHERE stream_id = ?
		   AND acked_at IS NULL
		   AND dropped_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano),
		reason,
		streamID,
	)
	return err
}

func (s *WorkspaceStateDB) InsertInboxUpdate(ctx context.Context, streamID string, update []byte, remoteUpdateID int64) (StreamInboxRow, bool, error) {
	if s == nil || s.db == nil {
		return StreamInboxRow{}, false, errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return StreamInboxRow{}, false, errors.New("stream id is required")
	}
	if len(update) == 0 {
		return StreamInboxRow{}, false, errors.New("inbox update is required")
	}
	if err := s.EnsureLocalStream(ctx, streamID, "unknown"); err != nil {
		return StreamInboxRow{}, false, err
	}
	sha := sha256HexBytes(update)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO stream_inbox(stream_id, update_sha256, update_bytes, remote_update_id, received_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(stream_id, update_sha256) DO NOTHING`,
		streamID,
		sha,
		update,
		nullInt64(remoteUpdateID),
		now,
	)
	if err != nil {
		return StreamInboxRow{}, false, err
	}
	affected, _ := result.RowsAffected()
	row := s.db.QueryRowContext(ctx, `
		SELECT id, stream_id, update_sha256, update_bytes, remote_update_id, received_at, applied_at
		  FROM stream_inbox
		 WHERE stream_id = ? AND update_sha256 = ?`, streamID, sha)
	inbox, err := scanInboxRow(row)
	return inbox, affected > 0, err
}

func (s *WorkspaceStateDB) UnappliedInbox(ctx context.Context, streamID string, limit int) ([]StreamInboxRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	return unappliedInboxWithQueryer(ctx, s.db, streamID, limit)
}

func unappliedInboxTx(ctx context.Context, tx *sql.Tx, streamID string, limit int) ([]StreamInboxRow, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	return unappliedInboxWithQueryer(ctx, tx, streamID, limit)
}

func unappliedInboxWithQueryer(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, streamID string, limit int) ([]StreamInboxRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT id, stream_id, update_sha256, update_bytes, remote_update_id, received_at, applied_at
		  FROM stream_inbox
		 WHERE stream_id = ? AND applied_at IS NULL
		 ORDER BY id ASC
		 LIMIT ?`, streamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inbox := []StreamInboxRow{}
	for rows.Next() {
		row, err := scanInboxRow(rows)
		if err != nil {
			return nil, err
		}
		inbox = append(inbox, row)
	}
	return inbox, rows.Err()
}

func (s *WorkspaceStateDB) MarkInboxApplied(ctx context.Context, inboxID int64, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	return markInboxAppliedWithExecer(ctx, s.db, inboxID, at)
}

func markInboxAppliedTx(ctx context.Context, tx *sql.Tx, inboxID int64, at time.Time) error {
	if tx == nil {
		return errors.New("transaction is required")
	}
	return markInboxAppliedWithExecer(ctx, tx, inboxID, at)
}

func markInboxAppliedWithExecer(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, inboxID int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := execer.ExecContext(ctx, `UPDATE stream_inbox SET applied_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), inboxID)
	return err
}

const outboxColumns = `id, stream_id, mutation_key, update_sha256, update_bytes, actor_id, actor_type, reason, kind_hint, depends_on_id, local_applied_at, sent_at, acked_at, ack_update_id, dropped_at, drop_reason`

func resolveOutboxDependencyID(ctx context.Context, tx *sql.Tx, streamID string, mutationKey string) (int64, error) {
	mutationKey = strings.TrimSpace(mutationKey)
	if mutationKey == "" {
		return 0, errors.New("dependency mutation key is required")
	}
	var row *sql.Row
	if strings.TrimSpace(streamID) != "" {
		row = tx.QueryRowContext(ctx, `SELECT id FROM stream_outbox WHERE stream_id = ? AND mutation_key = ? AND dropped_at IS NULL`, strings.TrimSpace(streamID), mutationKey)
	} else {
		row = tx.QueryRowContext(ctx, `SELECT id FROM stream_outbox WHERE mutation_key = ? AND dropped_at IS NULL`, mutationKey)
	}
	var id int64
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("outbox dependency %q not found", mutationKey)
		}
		return 0, err
	}
	return id, nil
}

func getOutboxByMutationKeyTx(ctx context.Context, tx *sql.Tx, streamID string, mutationKey string) (StreamOutboxRow, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+outboxColumns+` FROM stream_outbox WHERE stream_id = ? AND mutation_key = ?`, streamID, mutationKey)
	return scanOutboxRow(row)
}

func scanOutboxRows(rows *sql.Rows) ([]StreamOutboxRow, error) {
	outbox := []StreamOutboxRow{}
	for rows.Next() {
		row, err := scanOutboxRow(rows)
		if err != nil {
			return nil, err
		}
		outbox = append(outbox, row)
	}
	return outbox, rows.Err()
}

func scanOutboxRow(scanner interface{ Scan(...any) error }) (StreamOutboxRow, error) {
	var row StreamOutboxRow
	err := scanner.Scan(
		&row.ID,
		&row.StreamID,
		&row.MutationKey,
		&row.UpdateSHA256,
		&row.UpdateBytes,
		&row.ActorID,
		&row.ActorType,
		&row.Reason,
		&row.KindHint,
		&row.DependsOnID,
		&row.LocalAppliedAt,
		&row.SentAt,
		&row.AckedAt,
		&row.AckUpdateID,
		&row.DroppedAt,
		&row.DropReason,
	)
	return row, err
}

func scanInboxRow(scanner interface{ Scan(...any) error }) (StreamInboxRow, error) {
	var row StreamInboxRow
	err := scanner.Scan(
		&row.ID,
		&row.StreamID,
		&row.UpdateSHA256,
		&row.UpdateBytes,
		&row.RemoteUpdateID,
		&row.ReceivedAt,
		&row.AppliedAt,
	)
	return row, err
}

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nullString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
