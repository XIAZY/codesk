package notty

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
)

const (
	StreamKindRoot    = "root"
	StreamKindContent = "content"
	StreamKindUnknown = "unknown"
)

type ApplyStreamUpdateResult struct {
	Accepted    bool
	Applied     bool
	UpdateID    int64
	StateVector []byte
	Kind        string
}

type StreamHead struct {
	WorkspaceID string
	StreamID    string
	Kind        string
	StateVector []byte
	UpdateID    int64
	UpdatedAt   time.Time
}

type LegacyRootStorageFingerprint struct {
	TextJSON    string
	EntriesJSON string
}

func (s *Store) BootstrapWorkspaceStreams() (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("store database is required")
	}
	workspaceID := strings.TrimSpace(s.workspaceID)
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}
	workspaceName := strings.TrimSpace(s.workspaceName)
	if workspaceName == "" {
		workspaceName = workspaceID
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	rootStreamID, err := bootstrapWorkspaceStreamsTx(tx, workspaceID, workspaceName)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return rootStreamID, nil
}

func (s *Store) EnsureStream(streamID string, kind string) error {
	if s == nil || s.db == nil {
		return errors.New("store database is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return errors.New("stream id is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureStreamHeadExistsTx(tx, workspaceID, streamID, kind, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func bootstrapWorkspaceStreamsTx(tx *sql.Tx, workspaceID string, workspaceName string) (string, error) {
	var rootStreamID sql.NullString
	err := tx.QueryRow(`SELECT root_stream_id FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&rootStreamID)
	switch {
	case err == nil && strings.TrimSpace(rootStreamID.String) != "":
		if err := ensureStreamHeadExistsTx(tx, workspaceID, rootStreamID.String, StreamKindRoot, time.Now().UTC()); err != nil {
			return "", err
		}
		return rootStreamID.String, nil
	case err != nil && err != sql.ErrNoRows:
		return "", err
	}

	now := time.Now().UTC()
	nextRootStreamID := "root_" + uuid.NewString()
	if err == sql.ErrNoRows {
		slug := normalizeWorkspaceSlug(workspaceID)
		if _, insertErr := tx.Exec(
			`INSERT INTO workspaces (id, slug, name, root_stream_id, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			workspaceID,
			slug,
			workspaceName,
			nextRootStreamID,
			now,
			now,
		); insertErr != nil {
			return "", insertErr
		}
	} else {
		if _, updateErr := tx.Exec(
			`UPDATE workspaces
			    SET root_stream_id = $1,
			        updated_at = $2
			  WHERE id = $3`,
			nextRootStreamID,
			now,
			workspaceID,
		); updateErr != nil {
			return "", updateErr
		}
	}
	if err := ensureStreamHeadExistsTx(tx, workspaceID, nextRootStreamID, StreamKindRoot, now); err != nil {
		return "", err
	}
	return nextRootStreamID, nil
}

func (s *Store) ApplyStreamUpdate(streamID string, update []byte, meta OperationMeta) (ApplyStreamUpdateResult, error) {
	if s == nil || s.db == nil {
		return ApplyStreamUpdateResult{}, errors.New("store database is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return ApplyStreamUpdateResult{}, errors.New("stream id is required")
	}
	if len(update) == 0 {
		return ApplyStreamUpdateResult{}, errors.New("stream update is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	defer tx.Rollback()

	result, err := applyStreamUpdateTx(tx, workspaceID, streamID, update, meta)
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	return result, nil
}

func applyStreamUpdateTx(tx *sql.Tx, workspaceID string, streamID string, update []byte, meta OperationMeta) (ApplyStreamUpdateResult, error) {
	now := time.Now().UTC()
	hash := streamUpdateHash(update)
	allowedKind, err := streamAccessAllowedTx(tx, workspaceID, streamID)
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}

	var existingID int64
	err = tx.QueryRow(
		`SELECT id
		   FROM crdt_stream_updates
		  WHERE workspace_id = $1 AND stream_id = $2 AND update_sha256 = $3`,
		workspaceID,
		streamID,
		hash,
	).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return ApplyStreamUpdateResult{}, err
	}
	if err == nil {
		head, headErr := getStreamHeadTx(tx, workspaceID, streamID)
		if headErr != nil && headErr != sql.ErrNoRows {
			return ApplyStreamUpdateResult{}, headErr
		}
		return ApplyStreamUpdateResult{
			Accepted:    true,
			Applied:     false,
			UpdateID:    head.UpdateID,
			StateVector: append([]byte(nil), head.StateVector...),
			Kind:        allowedKind,
		}, nil
	}

	doc, head, err := restoreStreamDocForUpdateTx(tx, workspaceID, streamID)
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	rootStream, err := isRootStreamTx(tx, workspaceID, streamID, head.Kind)
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	var previousRoot RootManifest
	if rootStream {
		previousRoot, err = ReadRootManifest(doc)
		if err != nil {
			return ApplyStreamUpdateResult{}, err
		}
	}
	var beforeLegacyRootStorage LegacyRootStorageFingerprint
	if rootStream {
		beforeLegacyRootStorage, err = legacyRootStorageFingerprint(doc)
		if err != nil {
			return ApplyStreamUpdateResult{}, err
		}
	}
	beforeSV, err := doc.StateVectorV1()
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	beforeState := doc.EncodeStateAsUpdate()
	if err := crdt.ApplyUpdateV1(doc, update, "stream-update"); err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	afterSV, err := doc.StateVectorV1()
	if err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	afterState := doc.EncodeStateAsUpdate()
	if rootStream {
		afterLegacyRootStorage, err := legacyRootStorageFingerprint(doc)
		if err != nil {
			return ApplyStreamUpdateResult{}, err
		}
		if beforeLegacyRootStorage != afterLegacyRootStorage {
			return ApplyStreamUpdateResult{}, errors.New("legacy root storage is read-only; use field maps")
		}
		nextRoot, err := ReadRootManifest(doc)
		if err != nil {
			return ApplyStreamUpdateResult{}, err
		}
		if err := ValidateRootManifest(previousRoot, nextRoot); err != nil {
			return ApplyStreamUpdateResult{}, err
		}
	}
	if bytes.Equal(beforeSV, afterSV) && bytes.Equal(beforeState, afterState) {
		if head.StreamID == "" {
			if err := ensureStreamHeadTx(tx, workspaceID, streamID, allowedKind, afterSV, 0, now); err != nil {
				return ApplyStreamUpdateResult{}, err
			}
		}
		return ApplyStreamUpdateResult{
			Accepted:    true,
			Applied:     false,
			UpdateID:    head.UpdateID,
			StateVector: afterSV,
			Kind:        allowedKind,
		}, nil
	}

	actorID := strings.TrimSpace(meta.ActorID)
	if actorID == "" {
		actorID = "unknown"
	}
	actorType := strings.TrimSpace(meta.ActorType)
	if actorType == "" {
		actorType = "unknown"
	}

	var updateID int64
	if err := tx.QueryRow(
		`INSERT INTO crdt_stream_updates (workspace_id, stream_id, update, update_sha256, actor_id, actor_type, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id`,
		workspaceID,
		streamID,
		update,
		hash,
		actorID,
		actorType,
		now,
	).Scan(&updateID); err != nil {
		return ApplyStreamUpdateResult{}, err
	}

	kind := firstNonEmptyString(nonUnknownStreamKind(head.Kind), allowedKind)
	if strings.TrimSpace(kind) == "" {
		kind = StreamKindUnknown
	}
	if err := ensureStreamHeadTx(tx, workspaceID, streamID, kind, afterSV, updateID, now); err != nil {
		return ApplyStreamUpdateResult{}, err
	}
	if err := maybeInsertStreamCheckpointTx(tx, workspaceID, streamID, updateID, doc, afterSV, now); err != nil {
		return ApplyStreamUpdateResult{}, err
	}

	return ApplyStreamUpdateResult{
		Accepted:    true,
		Applied:     true,
		UpdateID:    updateID,
		StateVector: afterSV,
		Kind:        kind,
	}, nil
}

func legacyRootStorageFingerprint(doc *crdt.Doc) (LegacyRootStorageFingerprint, error) {
	if doc == nil {
		return LegacyRootStorageFingerprint{}, errors.New("root manifest doc is required")
	}
	entriesJSON, err := doc.GetMap(RootManifestMapName).JSON()
	if err != nil {
		return LegacyRootStorageFingerprint{}, err
	}
	return LegacyRootStorageFingerprint{
		TextJSON:    canonicalRootStorageJSON(doc.GetText(RootManifestTextName).ToString()),
		EntriesJSON: canonicalRootStorageJSON(entriesJSON),
	}, nil
}

func canonicalRootStorageJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(canonical)
}

func nonUnknownStreamKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" || kind == StreamKindUnknown {
		return ""
	}
	return kind
}

func streamAccessAllowedTx(tx *sql.Tx, workspaceID string, streamID string) (string, error) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return "", errors.New("stream id is required")
	}
	var rootStreamID sql.NullString
	err := tx.QueryRow(`SELECT root_stream_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&rootStreamID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("workspace %q is missing root stream", workspaceID)
	}
	if err != nil {
		return "", err
	}
	rootID := strings.TrimSpace(rootStreamID.String)
	if rootID == "" {
		return "", fmt.Errorf("workspace %q is missing root stream", workspaceID)
	}
	if streamID == rootID {
		return StreamKindRoot, nil
	}
	rootDoc, _, err := restoreStreamDocForUpdateTx(tx, workspaceID, rootID)
	if err != nil {
		return "", err
	}
	defer rootDoc.Close()
	manifest, err := ReadRootManifest(rootDoc)
	if err != nil {
		return "", err
	}
	for _, entry := range manifest.EntriesByID {
		if entry.Kind == RootEntryKindFile && entry.Tombstone == nil && strings.TrimSpace(entry.ContentStreamID) == streamID {
			return StreamKindContent, nil
		}
	}
	return "", fmt.Errorf("stream %q is not referenced by root manifest", streamID)
}

func isRootStreamTx(tx *sql.Tx, workspaceID string, streamID string, kind string) (bool, error) {
	if strings.TrimSpace(kind) == StreamKindRoot {
		return true, nil
	}
	var rootStreamID sql.NullString
	err := tx.QueryRow(`SELECT root_stream_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&rootStreamID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(rootStreamID.String) != "" && rootStreamID.String == streamID, nil
}

func (s *Store) RestoreStreamDoc(streamID string) (*crdt.Doc, StreamHead, error) {
	if s == nil || s.db == nil {
		return nil, StreamHead{}, errors.New("store database is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	doc, head, err := restoreStreamDoc(s.db, workspaceID, strings.TrimSpace(streamID))
	if err != nil {
		return nil, StreamHead{}, err
	}
	return doc, head, nil
}

func (s *Store) AuthorizeStreamAccess(streamID string) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("store database is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	kind, err := streamAccessAllowedTx(tx, workspaceID, strings.TrimSpace(streamID))
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return kind, nil
}

func restoreStreamDoc(db *sql.DB, workspaceID string, streamID string) (*crdt.Doc, StreamHead, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, StreamHead{}, err
	}
	defer tx.Rollback()
	doc, head, err := restoreStreamDocForUpdateTx(tx, workspaceID, streamID)
	if err != nil {
		return nil, StreamHead{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, StreamHead{}, err
	}
	return doc, head, nil
}

func restoreStreamDocAtUpdate(db *sql.DB, workspaceID string, streamID string, updateID int64) (*crdt.Doc, StreamHead, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, StreamHead{}, err
	}
	defer tx.Rollback()
	head, err := getStreamHeadTx(tx, workspaceID, streamID)
	if err != nil {
		return nil, StreamHead{}, err
	}
	if updateID <= 0 {
		head.UpdateID = 0
		head.StateVector = nil
		return crdt.New(crdt.WithGUID(streamID)), head, nil
	}
	if updateID > 0 && updateID < head.UpdateID {
		head.UpdateID = updateID
	}
	doc, _, err := restoreStreamDocForHeadTx(tx, head)
	if err != nil {
		return nil, StreamHead{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, StreamHead{}, err
	}
	return doc, head, nil
}

func (s *Store) EncodeStreamSyncUpdates(streamID string, stateVector []byte) (StreamHead, [][]byte, error) {
	if s == nil || s.db == nil {
		return StreamHead{}, nil, errors.New("store database is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}
	streamID = strings.TrimSpace(streamID)
	tx, err := s.db.Begin()
	if err != nil {
		return StreamHead{}, nil, err
	}
	defer tx.Rollback()
	allowedKind, err := streamAccessAllowedTx(tx, workspaceID, streamID)
	if err != nil {
		return StreamHead{}, nil, err
	}
	doc, head, err := restoreStreamDocForUpdateTx(tx, workspaceID, streamID)
	if err != nil {
		return StreamHead{}, nil, err
	}
	defer doc.Close()
	currentStateVector, err := doc.StateVectorV1()
	if err != nil {
		return StreamHead{}, nil, err
	}
	head.StateVector = currentStateVector
	if strings.TrimSpace(head.Kind) == "" || head.Kind == StreamKindUnknown {
		head.Kind = allowedKind
	}
	if head.UpdateID <= 0 {
		if err := tx.Commit(); err != nil {
			return StreamHead{}, nil, err
		}
		return head, nil, nil
	}
	update, err := doc.EncodeStateAsUpdateV1(stateVector)
	if err != nil {
		return StreamHead{}, nil, err
	}
	if len(update) == 0 {
		if err := tx.Commit(); err != nil {
			return StreamHead{}, nil, err
		}
		return head, nil, nil
	}
	if err := tx.Commit(); err != nil {
		return StreamHead{}, nil, err
	}
	return head, [][]byte{update}, nil
}

func (s *Store) GetAuthorizedStreamHead(streamID string) (StreamHead, error) {
	if s == nil || s.db == nil {
		return StreamHead{}, errors.New("store database is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = "ws_notty"
	}
	streamID = strings.TrimSpace(streamID)
	tx, err := s.db.Begin()
	if err != nil {
		return StreamHead{}, err
	}
	defer tx.Rollback()
	allowedKind, err := streamAccessAllowedTx(tx, workspaceID, streamID)
	if err != nil {
		return StreamHead{}, err
	}
	head, err := getStreamHeadTx(tx, workspaceID, streamID)
	if err == sql.ErrNoRows {
		return StreamHead{}, ErrNotFound
	}
	if err != nil {
		return StreamHead{}, err
	}
	if strings.TrimSpace(head.Kind) == "" || head.Kind == StreamKindUnknown {
		head.Kind = allowedKind
	}
	if err := tx.Commit(); err != nil {
		return StreamHead{}, err
	}
	return head, nil
}

func (s *Store) GetStreamHead(streamID string) (StreamHead, error) {
	if s == nil || s.db == nil {
		return StreamHead{}, errors.New("store database is required")
	}
	workspaceID := s.workspaceID
	if workspaceID == "" {
		workspaceID = s.state.WorkspaceID
	}
	head, err := getStreamHead(s.db, workspaceID, strings.TrimSpace(streamID))
	if err == sql.ErrNoRows {
		return StreamHead{}, ErrNotFound
	}
	return head, err
}

func getStreamHead(db *sql.DB, workspaceID string, streamID string) (StreamHead, error) {
	tx, err := db.Begin()
	if err != nil {
		return StreamHead{}, err
	}
	defer tx.Rollback()
	head, err := getStreamHeadTx(tx, workspaceID, streamID)
	if err != nil {
		return StreamHead{}, err
	}
	if err := tx.Commit(); err != nil {
		return StreamHead{}, err
	}
	return head, nil
}

func restoreStreamDocForUpdateTx(tx *sql.Tx, workspaceID string, streamID string) (*crdt.Doc, StreamHead, error) {
	head, err := getStreamHeadTx(tx, workspaceID, streamID)
	if err == sql.ErrNoRows {
		return crdt.New(crdt.WithGUID(streamID)), StreamHead{WorkspaceID: workspaceID, StreamID: streamID, Kind: StreamKindUnknown}, nil
	}
	if err != nil {
		return nil, StreamHead{}, err
	}
	return restoreStreamDocForHeadTx(tx, head)
}

func restoreStreamDocForHeadTx(tx *sql.Tx, head StreamHead) (*crdt.Doc, StreamHead, error) {
	doc := crdt.New(crdt.WithGUID(head.StreamID))
	appliedThrough := int64(0)
	var checkpointUpdateID int64
	var checkpointState []byte
	err := tx.QueryRow(
		`SELECT update_id, crdt_state
		   FROM crdt_stream_checkpoints
		  WHERE workspace_id = $1 AND stream_id = $2 AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		head.WorkspaceID,
		head.StreamID,
		head.UpdateID,
	).Scan(&checkpointUpdateID, &checkpointState)
	if err != nil && err != sql.ErrNoRows {
		return nil, StreamHead{}, err
	}
	if err == nil && len(checkpointState) > 0 {
		if applyErr := crdt.ApplyUpdateV1(doc, checkpointState, "stream-checkpoint"); applyErr != nil {
			return nil, StreamHead{}, applyErr
		}
		appliedThrough = checkpointUpdateID
	}
	if appliedThrough > head.UpdateID {
		appliedThrough = 0
	}
	if appliedThrough >= head.UpdateID {
		return doc, head, nil
	}

	rows, err := tx.Query(
		`SELECT update
		   FROM crdt_stream_updates
		  WHERE workspace_id = $1
		    AND stream_id = $2
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		head.WorkspaceID,
		head.StreamID,
		appliedThrough,
		head.UpdateID,
	)
	if err != nil {
		return nil, StreamHead{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var next []byte
		if err := rows.Scan(&next); err != nil {
			return nil, StreamHead{}, err
		}
		if err := crdt.ApplyUpdateV1(doc, next, "stream-tail"); err != nil {
			return nil, StreamHead{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, StreamHead{}, err
	}
	return doc, head, nil
}

func getStreamHeadTx(tx *sql.Tx, workspaceID string, streamID string) (StreamHead, error) {
	head := StreamHead{}
	err := tx.QueryRow(
		`SELECT workspace_id, stream_id, kind, state_vector, update_id, updated_at
		   FROM crdt_stream_heads
		  WHERE workspace_id = $1 AND stream_id = $2`,
		workspaceID,
		streamID,
	).Scan(&head.WorkspaceID, &head.StreamID, &head.Kind, &head.StateVector, &head.UpdateID, &head.UpdatedAt)
	return head, err
}

func ensureStreamHeadTx(tx *sql.Tx, workspaceID string, streamID string, kind string, stateVector []byte, updateID int64, updatedAt time.Time) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = StreamKindUnknown
	}
	if stateVector == nil {
		stateVector = []byte{}
	}
	_, err := tx.Exec(
		`INSERT INTO crdt_stream_heads (workspace_id, stream_id, kind, state_vector, update_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (workspace_id, stream_id)
		 DO UPDATE SET kind = CASE
		                         WHEN crdt_stream_heads.kind = 'unknown' THEN EXCLUDED.kind
		                         WHEN EXCLUDED.kind = 'unknown' THEN crdt_stream_heads.kind
		                         ELSE crdt_stream_heads.kind
		                       END,
		               state_vector = EXCLUDED.state_vector,
		               update_id = EXCLUDED.update_id,
		               updated_at = EXCLUDED.updated_at`,
		workspaceID,
		streamID,
		kind,
		stateVector,
		updateID,
		updatedAt,
	)
	return err
}

func ensureStreamHeadExistsTx(tx *sql.Tx, workspaceID string, streamID string, kind string, updatedAt time.Time) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = StreamKindUnknown
	}
	_, err := tx.Exec(
		`INSERT INTO crdt_stream_heads (workspace_id, stream_id, kind, state_vector, update_id, updated_at)
		 VALUES ($1, $2, $3, $4, 0, $5)
		 ON CONFLICT (workspace_id, stream_id)
		 DO UPDATE SET kind = CASE
		                         WHEN crdt_stream_heads.kind = 'unknown' THEN EXCLUDED.kind
		                         ELSE crdt_stream_heads.kind
		                       END`,
		workspaceID,
		streamID,
		kind,
		[]byte{},
		updatedAt,
	)
	return err
}

func maybeInsertStreamCheckpointTx(tx *sql.Tx, workspaceID string, streamID string, updateID int64, doc *crdt.Doc, stateVector []byte, createdAt time.Time) error {
	if updateID <= 0 {
		return nil
	}
	var lastCheckpointID int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(update_id), 0)
		   FROM crdt_stream_checkpoints
		  WHERE workspace_id = $1 AND stream_id = $2`,
		workspaceID,
		streamID,
	).Scan(&lastCheckpointID); err != nil {
		return err
	}
	if lastCheckpointID != 0 && updateID-lastCheckpointID < postgresCheckpointInterval {
		return nil
	}
	stateUpdate, err := doc.EncodeStateAsUpdateV1(nil)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO crdt_stream_checkpoints (workspace_id, stream_id, update_id, crdt_state, state_vector, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (workspace_id, stream_id, update_id)
		 DO UPDATE SET crdt_state = EXCLUDED.crdt_state,
		               state_vector = EXCLUDED.state_vector,
		               created_at = EXCLUDED.created_at`,
		workspaceID,
		streamID,
		updateID,
		stateUpdate,
		stateVector,
		createdAt,
	)
	return err
}

func streamUpdateHash(update []byte) string {
	sum := sha256.Sum256(update)
	return hex.EncodeToString(sum[:])
}

func streamRoomID(workspaceID string, streamID string) string {
	return fmt.Sprintf("%s:%s", workspaceID, streamID)
}
