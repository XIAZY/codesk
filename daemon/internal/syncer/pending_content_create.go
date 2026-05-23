package syncer

import (
	"context"
	"database/sql"
	"errors"
	"time"

	crdt "notty/internal/ycrdt"
)

const (
	MaxPendingContentCreatesPerCycle     = 32
	MaxPendingContentCreateBytesPerCycle = 64 << 20
	MaxSinglePendingCreateBytes          = 128 << 20
	PendingCreateRetention               = 24 * time.Hour
)

var pendingContentCreateStabilityDelay = 5 * time.Second

type PendingCreateLimits struct {
	MaxRows  int
	MaxBytes int64
}

type PendingContentCreate struct {
	EntryID           string
	ContentStreamID   string
	MaterializedPath  string
	RootMutationKey   string
	Status            string
	ContentOutboxID   sql.NullInt64
	ObservedStat      FileStat
	ObservedStatValid bool
}

type PendingContentCreateProcessor struct {
	State          *WorkspaceStateDB
	FS             *WorkspaceFS
	Capabilities   ScanCapabilities
	ActorID        string
	ActorType      string
	MaxSingleBytes int64
	Queue          func(streamID string)
}

func (s *WorkspaceStateDB) UpsertPendingContentCreate(ctx context.Context, create PendingContentCreate, caps ScanCapabilities) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if create.EntryID == "" || create.ContentStreamID == "" || create.MaterializedPath == "" || create.RootMutationKey == "" {
		return errors.New("pending content create requires entry id, content stream id, path, and root mutation key")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	observed := create.ObservedStat
	observedValid := caps.FileKeyReliable && observed.StatValid && observed.FileKey != ""
	observedFileKey := sql.NullString{String: observed.FileKey, Valid: observedValid}
	observedSize := sql.NullInt64{Int64: observed.SizeBytes, Valid: observedValid}
	observedMode := sql.NullInt64{Int64: int64(observed.Mode), Valid: observedValid}
	observedMTime := sql.NullInt64{Int64: observed.MTimeNS, Valid: observedValid}
	observedCTime := sql.NullInt64{Int64: observed.CTimeNS, Valid: observedValid}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_content_creates(
			entry_id, content_stream_id, materialized_path,
			observed_file_key, observed_size_bytes, observed_mode, observed_mtime_ns, observed_ctime_ns, observed_stat_valid,
			root_mutation_key, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'needs_bytes', ?, ?)
		ON CONFLICT(entry_id) DO UPDATE SET
			content_stream_id = excluded.content_stream_id,
			materialized_path = excluded.materialized_path,
			observed_file_key = excluded.observed_file_key,
			observed_size_bytes = excluded.observed_size_bytes,
			observed_mode = excluded.observed_mode,
			observed_mtime_ns = excluded.observed_mtime_ns,
			observed_ctime_ns = excluded.observed_ctime_ns,
			observed_stat_valid = excluded.observed_stat_valid,
			root_mutation_key = excluded.root_mutation_key,
			status = CASE
				WHEN pending_content_creates.status IN ('completed', 'cancelled') THEN pending_content_creates.status
				ELSE 'needs_bytes'
			END,
			updated_at = excluded.updated_at`,
		create.EntryID,
		create.ContentStreamID,
		create.MaterializedPath,
		observedFileKey,
		observedSize,
		observedMode,
		observedMTime,
		observedCTime,
		boolInt(observedValid),
		create.RootMutationKey,
		now,
		now,
	)
	return err
}

func (s *WorkspaceStateDB) ClaimPendingContentCreates(ctx context.Context, limit int) ([]PendingContentCreate, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	if limit <= 0 {
		limit = MaxPendingContentCreatesPerCycle
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT entry_id, content_stream_id, materialized_path,
			observed_file_key, observed_size_bytes, observed_mode, observed_mtime_ns, observed_ctime_ns, observed_stat_valid,
			root_mutation_key, status, content_outbox_id
		  FROM pending_content_creates
		 WHERE status = 'needs_bytes'
		 ORDER BY created_at ASC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	creates := []PendingContentCreate{}
	for rows.Next() {
		create, err := scanPendingContentCreate(rows)
		if err != nil {
			return nil, err
		}
		creates = append(creates, create)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, create := range creates {
		if _, err := tx.ExecContext(ctx, `UPDATE pending_content_creates SET status = 'reading', updated_at = ? WHERE entry_id = ? AND status = 'needs_bytes'`, time.Now().UTC().Format(time.RFC3339Nano), create.EntryID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for index := range creates {
		creates[index].Status = "reading"
	}
	return creates, nil
}

func (s *WorkspaceStateDB) ResetPendingContentCreate(ctx context.Context, entryID string) error {
	return s.setPendingContentCreateStatus(ctx, entryID, "needs_bytes")
}

func (s *WorkspaceStateDB) CancelPendingContentCreate(ctx context.Context, entryID string) error {
	return s.setPendingContentCreateStatus(ctx, entryID, "cancelled")
}

func (s *WorkspaceStateDB) MarkPendingContentCreateOutboxCreated(ctx context.Context, entryID string, outboxID int64) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_content_creates
		   SET status = 'outbox_created',
		       content_outbox_id = ?,
		       updated_at = ?
		 WHERE entry_id = ?`,
		outboxID,
		time.Now().UTC().Format(time.RFC3339Nano),
		entryID,
	)
	return err
}

func (s *WorkspaceStateDB) HasPendingContentCreates(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("state db is required")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_content_creates WHERE status = 'needs_bytes'`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *WorkspaceStateDB) TryCompletePendingContentCreate(ctx context.Context, streamID string, projectedStateID int64) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_content_creates
		   SET status = 'completed',
		       updated_at = ?
		 WHERE content_stream_id = ?
		   AND status = 'outbox_created'
		   AND EXISTS (
		     SELECT 1 FROM stream_outbox
		      WHERE stream_outbox.id = pending_content_creates.content_outbox_id
		        AND stream_outbox.acked_at IS NOT NULL
		   )
		   AND EXISTS (
		     SELECT 1 FROM content_projection
		      WHERE content_projection.stream_id = pending_content_creates.content_stream_id
		        AND content_projection.projected_state_id IS NOT NULL
		        AND content_projection.projected_state_id = ?
		   )`,
		now,
		streamID,
		projectedStateID,
	)
	return err
}

func (s *WorkspaceStateDB) ReapPendingContentCreates(ctx context.Context, retention time.Duration) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("state db is required")
	}
	if retention <= 0 {
		retention = PendingCreateRetention
	}
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `DELETE FROM pending_content_creates WHERE status IN ('completed', 'cancelled') AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (p PendingContentCreateProcessor) Process(ctx context.Context, limits PendingCreateLimits) (bool, error) {
	if p.State == nil {
		return false, errors.New("state db is required")
	}
	if p.FS == nil {
		return false, errors.New("workspace fs is required")
	}
	if limits.MaxRows <= 0 {
		limits.MaxRows = MaxPendingContentCreatesPerCycle
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = MaxPendingContentCreateBytesPerCycle
	}
	maxSingle := p.MaxSingleBytes
	if maxSingle <= 0 {
		maxSingle = MaxSinglePendingCreateBytes
	}

	batch, err := p.State.ClaimPendingContentCreates(ctx, limits.MaxRows)
	if err != nil || len(batch) == 0 {
		return false, err
	}

	more := false
	var bytesRead int64
	for _, create := range batch {
		if limits.MaxBytes > 0 && bytesRead >= limits.MaxBytes {
			if err := p.State.ResetPendingContentCreate(ctx, create.EntryID); err != nil {
				return true, err
			}
			more = true
			continue
		}
		if limits.MaxBytes > 0 && bytesRead > 0 && create.ObservedStat.SizeBytes > 0 && bytesRead+create.ObservedStat.SizeBytes > limits.MaxBytes {
			if err := p.State.ResetPendingContentCreate(ctx, create.EntryID); err != nil {
				return true, err
			}
			more = true
			continue
		}

		current, err := p.FS.Stat(ctx, create.MaterializedPath)
		if err != nil {
			if resetErr := p.State.ResetPendingContentCreate(ctx, create.EntryID); resetErr != nil {
				return true, resetErr
			}
			return true, err
		}
		if !current.Exists {
			if err := p.State.CancelPendingContentCreate(ctx, create.EntryID); err != nil {
				return true, err
			}
			if err := p.State.InsertScanHint(ctx, ScanHintPath, create.MaterializedPath, "create-path-missing"); err != nil {
				return true, err
			}
			continue
		}
		if pendingContentCreateStabilityDelay > 0 {
			select {
			case <-ctx.Done():
				if err := p.State.ResetPendingContentCreate(ctx, create.EntryID); err != nil {
					return true, err
				}
				return true, ctx.Err()
			case <-time.After(pendingContentCreateStabilityDelay):
			}
			next, err := p.FS.Stat(ctx, create.MaterializedPath)
			if err != nil {
				if resetErr := p.State.ResetPendingContentCreate(ctx, create.EntryID); resetErr != nil {
					return true, resetErr
				}
				return true, err
			}
			if !next.Exists {
				if err := p.State.CancelPendingContentCreate(ctx, create.EntryID); err != nil {
					return true, err
				}
				if err := p.State.InsertScanHint(ctx, ScanHintPath, create.MaterializedPath, "create-path-missing"); err != nil {
					return true, err
				}
				continue
			}
			if !SameOpenFileStat(current, next, p.Capabilities) {
				if err := p.State.ResetPendingContentCreate(ctx, create.EntryID); err != nil {
					return true, err
				}
				if err := p.State.InsertScanHint(ctx, ScanHintPath, create.MaterializedPath, "create-stat-changing"); err != nil {
					return true, err
				}
				more = true
				continue
			}
			current = next
		}

		read, ok, err := p.FS.ReadBytesStable(ctx, create.MaterializedPath, StableReadOptions{
			ExpectedStat: &current,
			Capabilities: p.Capabilities,
			MaxBytes:     maxSingle,
		})
		if err != nil {
			if resetErr := p.State.ResetPendingContentCreate(ctx, create.EntryID); resetErr != nil {
				return true, resetErr
			}
			return true, err
		}
		if !ok {
			if err := p.State.ResetPendingContentCreate(ctx, create.EntryID); err != nil {
				return true, err
			}
			if err := p.State.InsertScanHint(ctx, ScanHintPath, create.MaterializedPath, "create-stat-changed"); err != nil {
				return true, err
			}
			continue
		}
		bytesRead += int64(len(read.Bytes))
		if limits.MaxBytes > 0 && bytesRead > limits.MaxBytes && len(read.Bytes) > 0 {
			more = true
		}

		outbox, err := p.State.UpsertOutbox(ctx, StreamMutation{
			StreamID:             create.ContentStreamID,
			KindHint:             "content",
			MutationKey:          "content:init:" + create.ContentStreamID,
			UpdateBytes:          BuildInitialContentUpdate(read.Bytes),
			ActorID:              p.ActorID,
			ActorType:            p.ActorType,
			Reason:               "content-create-local",
			DependsOnMutationKey: create.RootMutationKey,
		})
		if err != nil {
			if resetErr := p.State.ResetPendingContentCreate(ctx, create.EntryID); resetErr != nil {
				return true, resetErr
			}
			return true, err
		}
		if err := p.State.MarkPendingContentCreateOutboxCreated(ctx, create.EntryID, outbox.ID); err != nil {
			return true, err
		}
		if p.Queue != nil {
			p.Queue(create.ContentStreamID)
		}
	}
	hasMore, err := p.State.HasPendingContentCreates(ctx)
	if err != nil {
		return more, err
	}
	return more || hasMore, nil
}

func BuildInitialContentUpdate(content []byte) []byte {
	doc := crdt.New()
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, string(content), nil)
	}, "content-init")
	return doc.EncodeStateAsUpdate()
}

func (s *WorkspaceStateDB) setPendingContentCreateStatus(ctx context.Context, entryID string, status string) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE pending_content_creates SET status = ?, updated_at = ? WHERE entry_id = ?`, status, time.Now().UTC().Format(time.RFC3339Nano), entryID)
	return err
}

func scanPendingContentCreate(scanner interface{ Scan(...any) error }) (PendingContentCreate, error) {
	var create PendingContentCreate
	var fileKey sql.NullString
	var size sql.NullInt64
	var mode sql.NullInt64
	var mtime sql.NullInt64
	var ctime sql.NullInt64
	var observedValid int
	err := scanner.Scan(
		&create.EntryID,
		&create.ContentStreamID,
		&create.MaterializedPath,
		&fileKey,
		&size,
		&mode,
		&mtime,
		&ctime,
		&observedValid,
		&create.RootMutationKey,
		&create.Status,
		&create.ContentOutboxID,
	)
	if err != nil {
		return PendingContentCreate{}, err
	}
	create.ObservedStat = FileStat{
		Path:      create.MaterializedPath,
		Kind:      FileKindFile,
		Exists:    true,
		FileKey:   fileKey.String,
		SizeBytes: size.Int64,
		Mode:      uint32(mode.Int64),
		MTimeNS:   mtime.Int64,
		CTimeNS:   ctime.Int64,
		StatValid: observedValid != 0,
	}
	create.ObservedStatValid = observedValid != 0
	return create, nil
}
