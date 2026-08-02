package notty

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
)

const reverseWindowDuration = 5 * time.Minute

type OpenReverseWindowOutcome string

const (
	OpenReverseWindowAcceptedNew             OpenReverseWindowOutcome = "accepted_new"
	OpenReverseWindowExactReplayStoredResult OpenReverseWindowOutcome = "exact_replay_stored_result"
	OpenReverseWindowOperationMismatch       OpenReverseWindowOutcome = "operation_mismatch"
	OpenReverseWindowGenerationConflict      OpenReverseWindowOutcome = "window_generation_conflict"
)

type ConsumeReverseWindowOutcome string

const (
	ConsumeReverseWindowAccepted                ConsumeReverseWindowOutcome = "accepted"
	ConsumeReverseWindowExactReplayStoredResult ConsumeReverseWindowOutcome = "exact_replay_stored_result"
)

var (
	ErrReverseWindowInvalidRequest     = errors.New("invalid reverse-window request")
	ErrReverseWindowIdentityMismatch   = errors.New("reverse-window identity mismatch")
	ErrReverseWindowExpired            = errors.New("reverse window expired")
	ErrReverseWindowFrontierNotReached = errors.New("reverse-window content frontier not reached")
	ErrReverseWindowRootMismatch       = errors.New("reverse-window root entry mismatch")
	ErrReverseWindowPathClaimed        = errors.New("reverse-window path is claimed")
)

type OpenReverseWindowRequest struct {
	OriginDaemonID           string `json:"-"`
	OriginScope              string `json:"-"`
	OperationID              string `json:"operationId"`
	ExpectedWindowGeneration int64  `json:"expectedWindowGeneration"`
	EntryID                  string `json:"entryId"`
	ContentDocumentID        string `json:"contentDocumentId"`
	ExpectedDesiredPath      string `json:"expectedDesiredPath"`
}

type OpenReverseWindowResult struct {
	Outcome                 OpenReverseWindowOutcome `json:"outcome"`
	WindowGeneration        int64                    `json:"windowGeneration,omitempty"`
	CurrentWindowGeneration int64                    `json:"currentWindowGeneration"`
	OpenedAt                time.Time                `json:"openedAt,omitempty"`
	ReverseUntil            time.Time                `json:"reverseUntil,omitempty"`
	TombstoneUpdateID       int64                    `json:"tombstoneUpdateId,omitempty"`
	RootDocumentID          string                   `json:"-"`
	RootUpdate              []byte                   `json:"-"`
}

type ConsumeReverseWindowRequest struct {
	OriginDaemonID       string `json:"-"`
	OriginScope          string `json:"-"`
	TombstoneOperationID string `json:"tombstoneOperationId"`
	RestoreOperationID   string `json:"restoreOperationId"`
	WindowGeneration     int64  `json:"windowGeneration"`
	ContentStateVector   []byte `json:"contentStateVector"`
}

type ConsumeReverseWindowResult struct {
	Outcome          ConsumeReverseWindowOutcome `json:"outcome"`
	WindowGeneration int64                       `json:"windowGeneration"`
	ConsumedAt       time.Time                   `json:"consumedAt"`
	RestoreUpdateID  int64                       `json:"restoreUpdateId"`
	RootDocumentID   string                      `json:"-"`
	RootUpdate       []byte                      `json:"-"`
}

type normalizedReverseOrigin struct {
	daemonID  string
	scope     string
	actorID   string
	actorType string
}

type normalizedOpenReverseWindowRequest struct {
	origin                   normalizedReverseOrigin
	operationID              string
	expectedWindowGeneration int64
	entryID                  string
	contentDocumentID        string
	desiredPath              string
	fingerprint              string
}

type normalizedConsumeReverseWindowRequest struct {
	origin               normalizedReverseOrigin
	tombstoneOperationID string
	restoreOperationID   string
	windowGeneration     int64
	contentStateVector   []byte
}

type reverseWindowRow struct {
	documentID         string
	rootDocumentID     string
	entryID            string
	desiredPath        string
	originDaemonID     string
	originScope        string
	windowGeneration   int64
	tombstoneOperation string
	fingerprint        string
	tombstoneUpdateID  int64
	openedAt           time.Time
	reverseUntil       time.Time
	restoreOperation   string
	restoreUpdateID    int64
	consumedAt         time.Time
	consumed           bool
}

type reverseWindowDocumentHead struct {
	documentID   string
	clientIDSeed uint64
	updateID     int64
}

func (s *Store) ReadDocumentReverseGeneration(ctx context.Context, documentID string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("workspace store is required")
	}
	documentID, err := canonicalReverseWindowUUID(documentID, "content document id")
	if err != nil {
		return 0, err
	}
	s.mu.RLock()
	workspaceID := s.workspaceID
	db := s.db
	s.mu.RUnlock()
	var generation sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT reverse_window.window_generation
		FROM documents document
		LEFT JOIN document_reverse_windows reverse_window
		  ON reverse_window.workspace_id = document.workspace_id
		 AND reverse_window.document_id = document.id
		WHERE document.workspace_id = $1::uuid AND document.id = $2::uuid`,
		workspaceID, documentID,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if !generation.Valid {
		return 0, nil
	}
	if generation.Int64 <= 0 {
		return 0, fmt.Errorf("persisted reverse-window generation %d is invalid", generation.Int64)
	}
	return generation.Int64, nil
}

func (s *Store) OpenOrReplaceReverseWindow(ctx context.Context, request OpenReverseWindowRequest) (OpenReverseWindowResult, error) {
	if s == nil || s.db == nil {
		return OpenReverseWindowResult{}, errors.New("workspace store is required")
	}
	req, err := normalizeOpenReverseWindowRequest(request)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}
	defer tx.Rollback()

	rootHead, err := lockReverseWindowRootHead(ctx, tx, s.workspaceID)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}
	current, present, err := loadReverseWindowForUpdate(ctx, tx, s.workspaceID, req.contentDocumentID)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}
	if present && current.rootDocumentID != rootHead.documentID {
		return OpenReverseWindowResult{}, errors.New("reverse window references a non-current root document")
	}
	if present && current.originDaemonID == req.origin.daemonID && current.originScope == req.origin.scope && current.tombstoneOperation == req.operationID {
		if current.fingerprint != req.fingerprint {
			return OpenReverseWindowResult{Outcome: OpenReverseWindowOperationMismatch}, nil
		}
		if current.windowGeneration <= 0 {
			return OpenReverseWindowResult{}, errors.New("current reverse window has no accepted generation")
		}
		return openReverseWindowResultFromRow(OpenReverseWindowExactReplayStoredResult, current), nil
	}

	currentGeneration := int64(0)
	if present {
		currentGeneration = current.windowGeneration
		if currentGeneration <= 0 {
			return OpenReverseWindowResult{}, errors.New("current reverse window has no accepted generation")
		}
	}
	if req.expectedWindowGeneration != currentGeneration {
		return OpenReverseWindowResult{
			Outcome:                 OpenReverseWindowGenerationConflict,
			CurrentWindowGeneration: currentGeneration,
		}, nil
	}
	if exists, err := reverseWindowDocumentExists(ctx, tx, s.workspaceID, req.contentDocumentID); err != nil {
		return OpenReverseWindowResult{}, err
	} else if !exists {
		return OpenReverseWindowResult{}, ErrNotFound
	}

	rootDoc, err := restoreReverseWindowDocumentTx(ctx, tx, s.workspaceID, rootHead)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}
	defer rootDoc.Close()
	entry, err := rootFileEntryForCommand(rootDoc, req.entryID, req.contentDocumentID, req.desiredPath)
	if err != nil {
		return OpenReverseWindowResult{}, fmt.Errorf("%w: %v", ErrReverseWindowRootMismatch, err)
	}
	if entry.Deleted {
		return OpenReverseWindowResult{}, fmt.Errorf("%w: root entry is already deleted", ErrReverseWindowRootMismatch)
	}
	rootUpdate, applied, err := setRootFileDeleted(rootDoc, entry, true)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}
	if !applied || len(rootUpdate) == 0 {
		return OpenReverseWindowResult{}, errors.New("root tombstone mutation was not applied")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rootUpdateID, rootStateVector, err := persistReverseWindowRootUpdate(ctx, tx, s.workspaceID, rootHead, rootDoc, rootUpdate, req.origin, now)
	if err != nil {
		return OpenReverseWindowResult{}, err
	}
	result := OpenReverseWindowResult{
		Outcome:           OpenReverseWindowAcceptedNew,
		WindowGeneration:  currentGeneration + 1,
		OpenedAt:          now,
		ReverseUntil:      now.Add(reverseWindowDuration),
		TombstoneUpdateID: rootUpdateID,
		RootDocumentID:    rootHead.documentID,
		RootUpdate:        append([]byte(nil), rootUpdate...),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_reverse_windows (
			document_id, workspace_id, root_document_id, entry_id, desired_path,
			origin_daemon_id, origin_scope, window_generation,
			tombstone_operation_id, tombstone_request_fingerprint,
			tombstone_update_id, opened_at, reverse_until,
			restore_operation_id, restore_update_id, consumed_at,
			created_at, updated_at
		) VALUES (
			$1::uuid, $2::uuid, $3::uuid, $4, $5,
			$6::uuid, $7, $8, $9::uuid, $10,
			$11, $12, $13, NULL, NULL, NULL, $12, $12
		)
		ON CONFLICT (document_id) DO UPDATE SET
			workspace_id = EXCLUDED.workspace_id,
			root_document_id = EXCLUDED.root_document_id,
			entry_id = EXCLUDED.entry_id,
			desired_path = EXCLUDED.desired_path,
			origin_daemon_id = EXCLUDED.origin_daemon_id,
			origin_scope = EXCLUDED.origin_scope,
			window_generation = EXCLUDED.window_generation,
			tombstone_operation_id = EXCLUDED.tombstone_operation_id,
			tombstone_request_fingerprint = EXCLUDED.tombstone_request_fingerprint,
			tombstone_update_id = EXCLUDED.tombstone_update_id,
			opened_at = EXCLUDED.opened_at,
			reverse_until = EXCLUDED.reverse_until,
			restore_operation_id = NULL,
			restore_update_id = NULL,
			consumed_at = NULL,
			updated_at = EXCLUDED.updated_at`,
		req.contentDocumentID,
		s.workspaceID,
		rootHead.documentID,
		req.entryID,
		req.desiredPath,
		req.origin.daemonID,
		req.origin.scope,
		result.WindowGeneration,
		req.operationID,
		req.fingerprint,
		rootUpdateID,
		result.OpenedAt,
		result.ReverseUntil,
	); err != nil {
		return OpenReverseWindowResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OpenReverseWindowResult{}, err
	}
	s.recordReverseWindowRootHeadLocked(rootHead.documentID, rootUpdateID, rootStateVector, now)
	return result, nil
}

func (s *Store) ConsumeReverseWindow(ctx context.Context, request ConsumeReverseWindowRequest) (ConsumeReverseWindowResult, error) {
	if s == nil || s.db == nil {
		return ConsumeReverseWindowResult{}, errors.New("workspace store is required")
	}
	req, err := normalizeConsumeReverseWindowRequest(request)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	defer tx.Rollback()
	rootHead, err := lockReverseWindowRootHead(ctx, tx, s.workspaceID)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	window, present, err := loadReverseWindowByIdentityForUpdate(ctx, tx, s.workspaceID, req.origin, req.tombstoneOperationID)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	if !present || window.rootDocumentID != rootHead.documentID {
		return ConsumeReverseWindowResult{}, ErrReverseWindowIdentityMismatch
	}
	if window.consumed && reverseWindowIdentityMatches(window, req) && window.restoreOperation == req.restoreOperationID {
		return ConsumeReverseWindowResult{
			Outcome:          ConsumeReverseWindowExactReplayStoredResult,
			WindowGeneration: window.windowGeneration,
			ConsumedAt:       window.consumedAt,
			RestoreUpdateID:  window.restoreUpdateID,
			RootDocumentID:   rootHead.documentID,
		}, nil
	}
	if window.consumed || !reverseWindowIdentityMatches(window, req) {
		return ConsumeReverseWindowResult{}, ErrReverseWindowIdentityMismatch
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.Before(window.reverseUntil) {
		return ConsumeReverseWindowResult{}, ErrReverseWindowExpired
	}
	if len(req.contentStateVector) == 0 {
		return ConsumeReverseWindowResult{}, ErrReverseWindowFrontierNotReached
	}
	requiredVector, err := crdt.DecodeStateVectorV1(req.contentStateVector)
	if err != nil {
		return ConsumeReverseWindowResult{}, fmt.Errorf("%w: %v", ErrReverseWindowFrontierNotReached, err)
	}
	contentHead, err := lockReverseWindowContentHead(ctx, tx, s.workspaceID, window.documentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsumeReverseWindowResult{}, ErrReverseWindowIdentityMismatch
	}
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	contentDoc, err := restoreReverseWindowDocumentTx(ctx, tx, s.workspaceID, contentHead)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	currentVector, err := crdt.DecodeStateVectorV1(crdt.EncodeStateVectorV1(contentDoc))
	contentDoc.Close()
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	if !reverseWindowStateVectorDominates(currentVector, requiredVector) {
		return ConsumeReverseWindowResult{}, ErrReverseWindowFrontierNotReached
	}

	rootDoc, err := restoreReverseWindowDocumentTx(ctx, tx, s.workspaceID, rootHead)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	defer rootDoc.Close()
	entry, err := rootFileEntryForCommand(rootDoc, window.entryID, window.documentID, window.desiredPath)
	if err != nil {
		return ConsumeReverseWindowResult{}, fmt.Errorf("%w: %v", ErrReverseWindowRootMismatch, err)
	}
	if !entry.Deleted {
		return ConsumeReverseWindowResult{}, fmt.Errorf("%w: root entry is already active", ErrReverseWindowRootMismatch)
	}
	claimed, err := rootPathClaimedByOtherActiveEntry(rootDoc, window.entryID, window.desiredPath)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	if claimed {
		return ConsumeReverseWindowResult{}, ErrReverseWindowPathClaimed
	}
	rootUpdate, applied, err := setRootFileDeleted(rootDoc, entry, false)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	if !applied || len(rootUpdate) == 0 {
		return ConsumeReverseWindowResult{}, errors.New("root restore mutation was not applied")
	}
	rootUpdateID, rootStateVector, err := persistReverseWindowRootUpdate(ctx, tx, s.workspaceID, rootHead, rootDoc, rootUpdate, req.origin, now)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE document_reverse_windows
		SET restore_operation_id = $1::uuid,
			restore_update_id = $2,
			consumed_at = $3,
			updated_at = $3
		WHERE workspace_id = $4::uuid
		  AND document_id = $5::uuid
		  AND window_generation = $6
		  AND origin_daemon_id = $7::uuid
		  AND origin_scope = $8
		  AND tombstone_operation_id = $9::uuid
		  AND consumed_at IS NULL`,
		req.restoreOperationID,
		rootUpdateID,
		now,
		s.workspaceID,
		window.documentID,
		req.windowGeneration,
		req.origin.daemonID,
		req.origin.scope,
		req.tombstoneOperationID,
	)
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	if affected != 1 {
		return ConsumeReverseWindowResult{}, errors.New("reverse window changed during restore")
	}
	if err := tx.Commit(); err != nil {
		return ConsumeReverseWindowResult{}, err
	}
	s.recordReverseWindowRootHeadLocked(rootHead.documentID, rootUpdateID, rootStateVector, now)
	return ConsumeReverseWindowResult{
		Outcome:          ConsumeReverseWindowAccepted,
		WindowGeneration: req.windowGeneration,
		ConsumedAt:       now,
		RestoreUpdateID:  rootUpdateID,
		RootDocumentID:   rootHead.documentID,
		RootUpdate:       append([]byte(nil), rootUpdate...),
	}, nil
}

func normalizeOpenReverseWindowRequest(request OpenReverseWindowRequest) (normalizedOpenReverseWindowRequest, error) {
	origin, err := normalizeReverseWindowOrigin(request.OriginDaemonID, request.OriginScope)
	if err != nil {
		return normalizedOpenReverseWindowRequest{}, err
	}
	operationID, err := canonicalReverseWindowUUID(request.OperationID, "operation id")
	if err != nil {
		return normalizedOpenReverseWindowRequest{}, err
	}
	if request.ExpectedWindowGeneration < 0 {
		return normalizedOpenReverseWindowRequest{}, fmt.Errorf("%w: expected generation must not be negative", ErrReverseWindowInvalidRequest)
	}
	entryID := strings.TrimSpace(request.EntryID)
	if entryID == "" {
		return normalizedOpenReverseWindowRequest{}, fmt.Errorf("%w: root entry id is required", ErrReverseWindowInvalidRequest)
	}
	contentDocumentID, err := canonicalReverseWindowUUID(request.ContentDocumentID, "content document id")
	if err != nil {
		return normalizedOpenReverseWindowRequest{}, err
	}
	desiredPath, err := normalizeRootCommandPath(request.ExpectedDesiredPath)
	if err != nil {
		return normalizedOpenReverseWindowRequest{}, fmt.Errorf("%w: %v", ErrReverseWindowInvalidRequest, err)
	}
	req := normalizedOpenReverseWindowRequest{
		origin:                   origin,
		operationID:              operationID,
		expectedWindowGeneration: request.ExpectedWindowGeneration,
		entryID:                  entryID,
		contentDocumentID:        contentDocumentID,
		desiredPath:              desiredPath,
	}
	req.fingerprint, err = reverseWindowRequestFingerprint(req)
	return req, err
}

func normalizeConsumeReverseWindowRequest(request ConsumeReverseWindowRequest) (normalizedConsumeReverseWindowRequest, error) {
	origin, err := normalizeReverseWindowOrigin(request.OriginDaemonID, request.OriginScope)
	if err != nil {
		return normalizedConsumeReverseWindowRequest{}, err
	}
	tombstoneOperationID, err := canonicalReverseWindowUUID(request.TombstoneOperationID, "tombstone operation id")
	if err != nil {
		return normalizedConsumeReverseWindowRequest{}, err
	}
	restoreOperationID, err := canonicalReverseWindowUUID(request.RestoreOperationID, "restore operation id")
	if err != nil {
		return normalizedConsumeReverseWindowRequest{}, err
	}
	if tombstoneOperationID == restoreOperationID {
		return normalizedConsumeReverseWindowRequest{}, fmt.Errorf("%w: tombstone and restore operation ids must be distinct", ErrReverseWindowInvalidRequest)
	}
	if request.WindowGeneration <= 0 {
		return normalizedConsumeReverseWindowRequest{}, fmt.Errorf("%w: accepted window generation must be positive", ErrReverseWindowInvalidRequest)
	}
	return normalizedConsumeReverseWindowRequest{
		origin:               origin,
		tombstoneOperationID: tombstoneOperationID,
		restoreOperationID:   restoreOperationID,
		windowGeneration:     request.WindowGeneration,
		contentStateVector:   append([]byte(nil), request.ContentStateVector...),
	}, nil
}

func normalizeReverseWindowOrigin(daemonID, scope string) (normalizedReverseOrigin, error) {
	canonicalDaemonID, err := canonicalReverseWindowUUID(daemonID, "origin daemon id")
	if err != nil {
		return normalizedReverseOrigin{}, err
	}
	scope = strings.TrimSpace(scope)
	if scope == "primary" {
		return normalizedReverseOrigin{
			daemonID:  canonicalDaemonID,
			scope:     scope,
			actorID:   canonicalDaemonID,
			actorType: "daemon",
		}, nil
	}
	const agentPrefix = "agent:"
	if !strings.HasPrefix(scope, agentPrefix) {
		return normalizedReverseOrigin{}, fmt.Errorf("%w: invalid origin scope", ErrReverseWindowInvalidRequest)
	}
	agentID, err := canonicalReverseWindowUUID(strings.TrimPrefix(scope, agentPrefix), "origin agent id")
	if err != nil {
		return normalizedReverseOrigin{}, err
	}
	return normalizedReverseOrigin{
		daemonID:  canonicalDaemonID,
		scope:     agentPrefix + agentID,
		actorID:   agentID,
		actorType: "agent",
	}, nil
}

func canonicalReverseWindowUUID(value, label string) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("%w: %s is invalid", ErrReverseWindowInvalidRequest, label)
	}
	return parsed.String(), nil
}

func reverseWindowRequestFingerprint(req normalizedOpenReverseWindowRequest) (string, error) {
	payload, err := json.Marshal(struct {
		Version                  int    `json:"version"`
		OriginDaemonID           string `json:"originDaemonId"`
		OriginScope              string `json:"originScope"`
		OperationID              string `json:"operationId"`
		ExpectedWindowGeneration int64  `json:"expectedWindowGeneration"`
		EntryID                  string `json:"entryId"`
		ContentDocumentID        string `json:"contentDocumentId"`
		ExpectedDesiredPath      string `json:"expectedDesiredPath"`
	}{
		Version:                  1,
		OriginDaemonID:           req.origin.daemonID,
		OriginScope:              req.origin.scope,
		OperationID:              req.operationID,
		ExpectedWindowGeneration: req.expectedWindowGeneration,
		EntryID:                  req.entryID,
		ContentDocumentID:        req.contentDocumentID,
		ExpectedDesiredPath:      req.desiredPath,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func lockReverseWindowRootHead(ctx context.Context, tx *sql.Tx, workspaceID string) (reverseWindowDocumentHead, error) {
	var head reverseWindowDocumentHead
	var clientIDSeed int64
	err := tx.QueryRowContext(ctx, `SELECT document.id::text, document.client_id_seed, head.update_id
		FROM workspaces workspace
		JOIN documents document
		  ON document.workspace_id = workspace.id
		 AND document.id = workspace.root_document_id
		JOIN document_heads head
		  ON head.workspace_id = document.workspace_id
		 AND head.document_id = document.id
		WHERE workspace.id = $1::uuid
		FOR UPDATE OF document, head`, workspaceID,
	).Scan(&head.documentID, &clientIDSeed, &head.updateID)
	if errors.Is(err, sql.ErrNoRows) {
		return reverseWindowDocumentHead{}, ErrNotFound
	}
	if err != nil {
		return reverseWindowDocumentHead{}, err
	}
	head.clientIDSeed = uint64(clientIDSeed)
	return head, nil
}

func lockReverseWindowContentHead(ctx context.Context, tx *sql.Tx, workspaceID, documentID string) (reverseWindowDocumentHead, error) {
	var head reverseWindowDocumentHead
	var clientIDSeed int64
	err := tx.QueryRowContext(ctx, `SELECT document.id::text, document.client_id_seed, head.update_id
		FROM documents document
		JOIN document_heads head
		  ON head.workspace_id = document.workspace_id
		 AND head.document_id = document.id
		WHERE document.workspace_id = $1::uuid AND document.id = $2::uuid
		FOR SHARE OF document, head`, workspaceID, documentID,
	).Scan(&head.documentID, &clientIDSeed, &head.updateID)
	if err != nil {
		return reverseWindowDocumentHead{}, err
	}
	head.clientIDSeed = uint64(clientIDSeed)
	return head, nil
}

func reverseWindowDocumentExists(ctx context.Context, tx *sql.Tx, workspaceID, documentID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM documents WHERE workspace_id = $1::uuid AND id = $2::uuid
	)`, workspaceID, documentID).Scan(&exists)
	return exists, err
}

func loadReverseWindowForUpdate(ctx context.Context, tx *sql.Tx, workspaceID, documentID string) (reverseWindowRow, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT document_id::text, root_document_id::text, entry_id, desired_path,
		origin_daemon_id::text, origin_scope, COALESCE(window_generation, 0),
		tombstone_operation_id::text, tombstone_request_fingerprint,
		COALESCE(tombstone_update_id, 0), opened_at, reverse_until,
		COALESCE(restore_operation_id::text, ''), COALESCE(restore_update_id, 0), consumed_at
	FROM document_reverse_windows
	WHERE workspace_id = $1::uuid AND document_id = $2::uuid
	FOR UPDATE`, workspaceID, documentID)
	return scanReverseWindowRow(row)
}

func loadReverseWindowByIdentityForUpdate(
	ctx context.Context,
	tx *sql.Tx,
	workspaceID string,
	origin normalizedReverseOrigin,
	tombstoneOperationID string,
) (reverseWindowRow, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT document_id::text, root_document_id::text, entry_id, desired_path,
		origin_daemon_id::text, origin_scope, COALESCE(window_generation, 0),
		tombstone_operation_id::text, tombstone_request_fingerprint,
		COALESCE(tombstone_update_id, 0), opened_at, reverse_until,
		COALESCE(restore_operation_id::text, ''), COALESCE(restore_update_id, 0), consumed_at
	FROM document_reverse_windows
	WHERE workspace_id = $1::uuid
	  AND origin_daemon_id = $2::uuid
	  AND origin_scope = $3
	  AND tombstone_operation_id = $4::uuid
	FOR UPDATE`, workspaceID, origin.daemonID, origin.scope, tombstoneOperationID)
	return scanReverseWindowRow(row)
}

type reverseWindowRowScanner interface {
	Scan(dest ...any) error
}

func scanReverseWindowRow(scanner reverseWindowRowScanner) (reverseWindowRow, bool, error) {
	var row reverseWindowRow
	var consumedAt sql.NullTime
	err := scanner.Scan(
		&row.documentID,
		&row.rootDocumentID,
		&row.entryID,
		&row.desiredPath,
		&row.originDaemonID,
		&row.originScope,
		&row.windowGeneration,
		&row.tombstoneOperation,
		&row.fingerprint,
		&row.tombstoneUpdateID,
		&row.openedAt,
		&row.reverseUntil,
		&row.restoreOperation,
		&row.restoreUpdateID,
		&consumedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return reverseWindowRow{}, false, nil
	}
	if err != nil {
		return reverseWindowRow{}, false, err
	}
	row.consumed = consumedAt.Valid
	if consumedAt.Valid {
		row.consumedAt = consumedAt.Time
	}
	return row, true, nil
}

func openReverseWindowResultFromRow(outcome OpenReverseWindowOutcome, row reverseWindowRow) OpenReverseWindowResult {
	return OpenReverseWindowResult{
		Outcome:           outcome,
		WindowGeneration:  row.windowGeneration,
		OpenedAt:          row.openedAt,
		ReverseUntil:      row.reverseUntil,
		TombstoneUpdateID: row.tombstoneUpdateID,
		RootDocumentID:    row.rootDocumentID,
	}
}

func reverseWindowIdentityMatches(row reverseWindowRow, req normalizedConsumeReverseWindowRequest) bool {
	return row.originDaemonID == req.origin.daemonID &&
		row.originScope == req.origin.scope &&
		row.tombstoneOperation == req.tombstoneOperationID &&
		row.windowGeneration == req.windowGeneration
}

func reverseWindowStateVectorDominates(current, required crdt.StateVector) bool {
	for client, requiredClock := range required {
		if current.Clock(client) < requiredClock {
			return false
		}
	}
	return true
}

func restoreReverseWindowDocumentTx(ctx context.Context, tx *sql.Tx, workspaceID string, head reverseWindowDocumentHead) (*crdt.Doc, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(head.clientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	var checkpointUpdateID int64
	err := tx.QueryRowContext(ctx, `SELECT update_id, crdt_state
		FROM document_checkpoints
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid AND update_id <= $3
		ORDER BY update_id DESC
		LIMIT 1`, workspaceID, head.documentID, head.updateID,
	).Scan(&checkpointUpdateID, &checkpointState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		doc.Close()
		return nil, err
	}
	if err == nil && checkpointState != "" {
		update, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
		if decodeErr != nil {
			doc.Close()
			return nil, decodeErr
		}
		if applyErr := crdt.ApplyUpdateV1(doc, update, "reverse-window-checkpoint"); applyErr != nil {
			doc.Close()
			return nil, applyErr
		}
		appliedThrough = checkpointUpdateID
	}
	rows, err := tx.QueryContext(ctx, `SELECT update
		FROM document_updates
		WHERE workspace_id = $1::uuid
		  AND document_id = $2::uuid
		  AND id > $3
		  AND id <= $4
		ORDER BY id ASC`, workspaceID, head.documentID, appliedThrough, head.updateID)
	if err != nil {
		doc.Close()
		return nil, err
	}
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			rows.Close()
			doc.Close()
			return nil, err
		}
		if err := crdt.ApplyUpdateV1(doc, update, "reverse-window-update"); err != nil {
			rows.Close()
			doc.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		doc.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		doc.Close()
		return nil, err
	}
	return doc, nil
}

func persistReverseWindowRootUpdate(
	ctx context.Context,
	tx *sql.Tx,
	workspaceID string,
	head reverseWindowDocumentHead,
	doc *crdt.Doc,
	update []byte,
	origin normalizedReverseOrigin,
	now time.Time,
) (int64, string, error) {
	var updateID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO document_updates (
			workspace_id, document_id, update, actor_id, actor_type, created_at
		) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
		RETURNING id`,
		workspaceID,
		head.documentID,
		update,
		actorUUIDOrNil(origin.actorID, origin.actorType),
		origin.actorType,
		now,
	).Scan(&updateID); err != nil {
		return 0, "", err
	}
	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	result, err := tx.ExecContext(ctx, `UPDATE document_heads
		SET state_vector = $1, update_id = $2, updated_at = $3
		WHERE workspace_id = $4::uuid AND document_id = $5::uuid AND update_id = $6`,
		stateVector, updateID, now, workspaceID, head.documentID, head.updateID)
	if err != nil {
		return 0, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, "", err
	}
	if affected != 1 {
		return 0, "", errors.New("root document head changed during reverse-window mutation")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET updated_at = $1
		WHERE workspace_id = $2::uuid AND id = $3::uuid`, now, workspaceID, head.documentID); err != nil {
		return 0, "", err
	}
	return updateID, stateVector, nil
}

func (s *Store) recordReverseWindowRootHeadLocked(documentID string, updateID int64, stateVector string, updatedAt time.Time) {
	document := s.state.ContentDocuments[documentID]
	if document == nil || document.UpdateID > updateID {
		return
	}
	document.UpdateID = updateID
	document.StateVector = stateVector
	document.UpdatedAt = updatedAt
}
