package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	crdt "notty/internal/ycrdt"
)

type StreamRecord struct {
	StreamID          string
	Kind              string
	LatestStateID     sql.NullInt64
	ProjectedStateID  sql.NullInt64
	LatestUpdateID    int64
	LatestStateVector []byte
	CreatedAt         string
	UpdatedAt         string
}

type StreamStateRecord struct {
	ID                     int64
	StreamID               string
	StateUpdate            []byte
	StateVector            []byte
	MaterializedTextSHA256 sql.NullString
	CreatedAt              string
}

type StreamQueueApplyResult struct {
	StateID     int64
	StateVector []byte
	LocalOutbox []StreamOutboxRow
	Inbox       []StreamInboxRow
}

type streamQueueApplyOptions struct {
	LocalLimit   int
	InboxLimit   int
	BeforeCommit func() error
}

func (s *WorkspaceStateDB) GetOrCreateStream(ctx context.Context, streamID string, kind string) (StreamRecord, error) {
	if err := s.EnsureLocalStream(ctx, streamID, kind); err != nil {
		return StreamRecord{}, err
	}
	return s.GetStream(ctx, streamID)
}

func (s *WorkspaceStateDB) GetStream(ctx context.Context, streamID string) (StreamRecord, error) {
	if s == nil || s.db == nil {
		return StreamRecord{}, errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return StreamRecord{}, errors.New("stream id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT stream_id, kind, latest_state_id, projected_state_id,
		       latest_update_id, latest_state_vector, created_at, updated_at
		  FROM streams
		 WHERE stream_id = ?`, streamID)
	return scanStreamRecord(row)
}

func (s *WorkspaceStateDB) LoadStreamDoc(ctx context.Context, streamID string, stateID sql.NullInt64) (*crdt.Doc, error) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return nil, errors.New("stream id is required")
	}
	doc := crdt.New(crdt.WithGUID(streamID))
	if !stateID.Valid {
		return doc, nil
	}
	state, err := s.LoadStreamState(ctx, stateID.Int64)
	if err != nil {
		doc.Close()
		return nil, err
	}
	if err := crdt.ApplyUpdateV1(doc, state.StateUpdate, "state-load"); err != nil {
		doc.Close()
		return nil, err
	}
	return doc, nil
}

func (s *WorkspaceStateDB) LoadLatestStreamDoc(ctx context.Context, streamID string, kind string) (*crdt.Doc, StreamRecord, error) {
	stream, err := s.GetOrCreateStream(ctx, streamID, kind)
	if err != nil {
		return nil, StreamRecord{}, err
	}
	doc, err := s.LoadStreamDoc(ctx, streamID, stream.LatestStateID)
	if err != nil {
		return nil, StreamRecord{}, err
	}
	return doc, stream, nil
}

func (s *WorkspaceStateDB) LoadStreamState(ctx context.Context, stateID int64) (StreamStateRecord, error) {
	if s == nil || s.db == nil {
		return StreamStateRecord{}, errors.New("state db is required")
	}
	if stateID <= 0 {
		return StreamStateRecord{}, errors.New("state id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, stream_id, state_update, state_vector, materialized_text_sha256, created_at
		  FROM stream_states
		 WHERE id = ?`, stateID)
	return scanStreamStateRecord(row)
}

func (s *WorkspaceStateDB) InsertStreamState(ctx context.Context, streamID string, stateUpdate []byte, stateVector []byte, materializedTextSHA256 string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("state db is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return 0, errors.New("stream id is required")
	}
	if len(stateUpdate) == 0 {
		return 0, errors.New("state update is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO stream_states(stream_id, state_update, state_vector, materialized_text_sha256, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		streamID,
		stateUpdate,
		stateVector,
		nullString(materializedTextSHA256),
		now,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *WorkspaceStateDB) UpdateLatestStreamState(ctx context.Context, streamID string, stateID int64, stateVector []byte) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if strings.TrimSpace(streamID) == "" || stateID <= 0 {
		return errors.New("stream id and state id are required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE streams
		   SET latest_state_id = ?,
		       latest_state_vector = ?,
		       updated_at = ?
		 WHERE stream_id = ?`,
		stateID,
		stateVector,
		time.Now().UTC().Format(time.RFC3339Nano),
		streamID,
	)
	return err
}

func (s *WorkspaceStateDB) UpdateProjectedStreamState(ctx context.Context, streamID string, stateID int64) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if strings.TrimSpace(streamID) == "" || stateID <= 0 {
		return errors.New("stream id and state id are required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE streams
		   SET projected_state_id = ?,
		       updated_at = ?
		 WHERE stream_id = ?`,
		stateID,
		time.Now().UTC().Format(time.RFC3339Nano),
		streamID,
	)
	return err
}

func (s *WorkspaceStateDB) ApplyStreamQueueAtomically(ctx context.Context, streamID string, kind string, doc *crdt.Doc, materializedTextSHA256 string) (StreamQueueApplyResult, error) {
	return s.applyStreamQueueAtomically(ctx, streamID, kind, doc, materializedTextSHA256, streamQueueApplyOptions{})
}

func (s *WorkspaceStateDB) applyStreamQueueAtomically(ctx context.Context, streamID string, kind string, doc *crdt.Doc, materializedTextSHA256 string, opts streamQueueApplyOptions) (StreamQueueApplyResult, error) {
	if s == nil || s.db == nil {
		return StreamQueueApplyResult{}, errors.New("state db is required")
	}
	if doc == nil {
		return StreamQueueApplyResult{}, errors.New("stream doc is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return StreamQueueApplyResult{}, errors.New("stream id is required")
	}
	if kind = strings.TrimSpace(kind); kind == "" {
		kind = "unknown"
	}
	if opts.LocalLimit <= 0 {
		opts.LocalLimit = 100
	}
	if opts.InboxLimit <= 0 {
		opts.InboxLimit = 100
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StreamQueueApplyResult{}, err
	}
	defer tx.Rollback()

	if err := ensureLocalStreamTx(ctx, tx, streamID, kind); err != nil {
		return StreamQueueApplyResult{}, err
	}
	localOutbox, err := readyLocalOutboxTx(ctx, tx, streamID, opts.LocalLimit)
	if err != nil {
		return StreamQueueApplyResult{}, err
	}
	for _, row := range localOutbox {
		if err := crdt.ApplyUpdateV1(doc, row.UpdateBytes, "local-outbox"); err != nil {
			return StreamQueueApplyResult{}, fmt.Errorf("apply local outbox %d: %w", row.ID, err)
		}
	}
	inbox, err := unappliedInboxTx(ctx, tx, streamID, opts.InboxLimit)
	if err != nil {
		return StreamQueueApplyResult{}, err
	}
	for _, row := range inbox {
		if err := crdt.ApplyUpdateV1(doc, row.UpdateBytes, "remote-inbox"); err != nil {
			return StreamQueueApplyResult{}, fmt.Errorf("apply inbox %d: %w", row.ID, err)
		}
	}

	if kind != "root" {
		materializedTextSHA256 = contentSHA256([]byte(doc.GetText("content").ToString()))
	}
	stateUpdate := doc.EncodeStateAsUpdate()
	stateVector, err := doc.StateVectorV1()
	if err != nil {
		return StreamQueueApplyResult{}, err
	}
	stateID, err := insertStreamStateTx(ctx, tx, streamID, stateUpdate, stateVector, materializedTextSHA256)
	if err != nil {
		return StreamQueueApplyResult{}, err
	}
	if err := updateLatestStreamStateTx(ctx, tx, streamID, stateID, stateVector); err != nil {
		return StreamQueueApplyResult{}, err
	}
	appliedAt := time.Now().UTC()
	for _, row := range localOutbox {
		if err := markOutboxLocallyAppliedTx(ctx, tx, row.ID, appliedAt); err != nil {
			return StreamQueueApplyResult{}, err
		}
	}
	for _, row := range inbox {
		if err := markInboxAppliedTx(ctx, tx, row.ID, appliedAt); err != nil {
			return StreamQueueApplyResult{}, err
		}
	}
	if opts.BeforeCommit != nil {
		if err := opts.BeforeCommit(); err != nil {
			return StreamQueueApplyResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return StreamQueueApplyResult{}, err
	}
	return StreamQueueApplyResult{
		StateID:     stateID,
		StateVector: stateVector,
		LocalOutbox: localOutbox,
		Inbox:       inbox,
	}, nil
}

func insertStreamStateTx(ctx context.Context, tx *sql.Tx, streamID string, stateUpdate []byte, stateVector []byte, materializedTextSHA256 string) (int64, error) {
	if tx == nil {
		return 0, errors.New("transaction is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return 0, errors.New("stream id is required")
	}
	if len(stateUpdate) == 0 {
		return 0, errors.New("state update is required")
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO stream_states(stream_id, state_update, state_vector, materialized_text_sha256, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		streamID,
		stateUpdate,
		stateVector,
		nullString(materializedTextSHA256),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func updateLatestStreamStateTx(ctx context.Context, tx *sql.Tx, streamID string, stateID int64, stateVector []byte) error {
	if tx == nil {
		return errors.New("transaction is required")
	}
	if strings.TrimSpace(streamID) == "" || stateID <= 0 {
		return errors.New("stream id and state id are required")
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE streams
		   SET latest_state_id = ?,
		       latest_state_vector = ?,
		       updated_at = ?
		 WHERE stream_id = ?`,
		stateID,
		stateVector,
		time.Now().UTC().Format(time.RFC3339Nano),
		streamID,
	)
	return err
}

func scanStreamRecord(scanner interface{ Scan(...any) error }) (StreamRecord, error) {
	var record StreamRecord
	if err := scanner.Scan(
		&record.StreamID,
		&record.Kind,
		&record.LatestStateID,
		&record.ProjectedStateID,
		&record.LatestUpdateID,
		&record.LatestStateVector,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return StreamRecord{}, err
	}
	return record, nil
}

func scanStreamStateRecord(scanner interface{ Scan(...any) error }) (StreamStateRecord, error) {
	var record StreamStateRecord
	if err := scanner.Scan(
		&record.ID,
		&record.StreamID,
		&record.StateUpdate,
		&record.StateVector,
		&record.MaterializedTextSHA256,
		&record.CreatedAt,
	); err != nil {
		return StreamStateRecord{}, err
	}
	return record, nil
}
