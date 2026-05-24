package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	MaxPendingScanHints      = 10_000
	ScanHintDrainLimit       = 1_000
	MaxCachedDirChildren     = 1_000
	MaxCachedDirJSONBytes    = 64 * 1024
	PeriodicFullScanInterval = 5 * time.Minute
)

type WorkspaceStateDB struct {
	db *sql.DB
}

type ScanHintKind string

const (
	ScanHintPath ScanHintKind = "path"
	ScanHintDir  ScanHintKind = "dir"
	ScanHintFull ScanHintKind = "full"
)

type ScanHint struct {
	Kind   ScanHintKind
	Path   string
	Reason string
}

type ScanState struct {
	CursorPath              string
	Incomplete              bool
	LastFullScanAt          sql.NullString
	DirectoryMTimeReliable  bool
	FileKeyReliable         bool
	CTimeReliable           bool
	CapabilitiesInitialized bool
	LastCapabilityProbeAt   sql.NullString
}

type ScanCapabilityProber interface {
	TestFileKeyReliability(context.Context) bool
	TestDirectoryMTimeReliability(context.Context) bool
	TestCTimeReliability(context.Context) bool
}

type DirectoryScanCacheEntry struct {
	Path     string
	MTimeNS  int64
	CTimeNS  int64
	Children []string
}

func (s ScanState) Capabilities() ScanCapabilities {
	return ScanCapabilities{
		DirectoryMTimeReliable: s.DirectoryMTimeReliable,
		FileKeyReliable:        s.FileKeyReliable,
		CTimeReliable:          s.CTimeReliable,
	}
}

func OpenWorkspaceStateDB(root string) (*WorkspaceStateDB, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	statePath := filepath.Join(root, ".notty", "state.sqlite")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", statePath+"?_busy_timeout=5000&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	state := &WorkspaceStateDB{db: db}
	if err := state.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return state, nil
}

func (s *WorkspaceStateDB) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *WorkspaceStateDB) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *WorkspaceStateDB) init(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	pragmas := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA synchronous = FULL`,
	}
	for _, statement := range pragmas {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, statement := range stateSQLiteSchemaStatements() {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := s.ensureSchemaCompatibility(ctx); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scan_state (
			id, cursor_path, incomplete, last_full_scan_at,
			directory_mtime_reliable, file_key_reliable, ctime_reliable,
			capabilities_initialized, last_capability_probe_at, updated_at
		) VALUES (
			1, '', 0, NULL,
			0, 0, 0,
			0, NULL, ?
		) ON CONFLICT(id) DO NOTHING`, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE pending_content_creates SET status = 'needs_bytes', updated_at = ? WHERE status = 'reading'`, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE fs_jobs SET status = 'pending', updated_at = ? WHERE status = 'running'`, now); err != nil {
		return err
	}
	return nil
}

func (s *WorkspaceStateDB) ensureSchemaCompatibility(ctx context.Context) error {
	statements := []string{
		`ALTER TABLE stream_outbox ADD COLUMN dropped_at TEXT`,
		`ALTER TABLE stream_outbox ADD COLUMN drop_reason TEXT`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil && !isDuplicateSQLiteColumnError(err) {
			return err
		}
	}
	return nil
}

func isDuplicateSQLiteColumnError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

func (s *WorkspaceStateDB) GetScanState(ctx context.Context) (ScanState, error) {
	if s == nil || s.db == nil {
		return ScanState{}, errors.New("state db is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT cursor_path, incomplete, last_full_scan_at,
			directory_mtime_reliable, file_key_reliable, ctime_reliable,
			capabilities_initialized, last_capability_probe_at
		FROM scan_state WHERE id = 1`)
	return scanScanState(row)
}

func (s *WorkspaceStateDB) SaveScanCursor(ctx context.Context, cursorPath string, incomplete bool) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE scan_state
		   SET cursor_path = ?,
		       incomplete = ?,
		       updated_at = ?
		 WHERE id = 1`,
		normalizeStateRelPath(cursorPath),
		boolInt(incomplete),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *WorkspaceStateDB) MarkFullScanComplete(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		UPDATE scan_state
		   SET cursor_path = '',
		       incomplete = 0,
		       last_full_scan_at = ?,
		       updated_at = ?
		 WHERE id = 1`, now, now)
	return err
}

func (s *WorkspaceStateDB) MaybeInsertPeriodicFullScanHint(ctx context.Context, interval time.Duration) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("state db is required")
	}
	if interval <= 0 {
		interval = PeriodicFullScanInterval
	}
	scanState, err := s.GetScanState(ctx)
	if err != nil {
		return false, err
	}
	if scanState.Incomplete {
		return false, nil
	}
	due := true
	if scanState.LastFullScanAt.Valid {
		if last, err := time.Parse(time.RFC3339Nano, scanState.LastFullScanAt.String); err == nil {
			due = time.Since(last) >= interval
		}
	}
	if !due {
		return false, nil
	}
	if err := s.InsertScanHint(ctx, ScanHintFull, "", "periodic-full-scan"); err != nil {
		return false, err
	}
	return true, nil
}

func (s *WorkspaceStateDB) InitializeScanCapabilities(ctx context.Context, prober ScanCapabilityProber) (ScanState, error) {
	if s == nil || s.db == nil {
		return ScanState{}, errors.New("state db is required")
	}
	if prober == nil {
		return ScanState{}, errors.New("scan capability prober is required")
	}
	current, err := s.GetScanState(ctx)
	if err != nil {
		return ScanState{}, err
	}
	if current.CapabilitiesInitialized {
		return current, nil
	}

	fileKeyReliable := prober.TestFileKeyReliability(ctx)
	directoryMTimeReliable := prober.TestDirectoryMTimeReliability(ctx)
	ctimeReliable := prober.TestCTimeReliability(ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScanState{}, err
	}
	defer tx.Rollback()
	var initialized bool
	if err := tx.QueryRowContext(ctx, `SELECT capabilities_initialized FROM scan_state WHERE id = 1`).Scan(&initialized); err != nil {
		return ScanState{}, err
	}
	if !initialized {
		if _, err := tx.ExecContext(ctx, `
			UPDATE scan_state SET
				directory_mtime_reliable = ?,
				file_key_reliable = ?,
				ctime_reliable = ?,
				capabilities_initialized = 1,
				last_capability_probe_at = ?,
				updated_at = ?
			WHERE id = 1`,
			boolInt(directoryMTimeReliable),
			boolInt(fileKeyReliable),
			boolInt(ctimeReliable),
			now,
			now,
		); err != nil {
			return ScanState{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO scan_hints(kind, path, reason, created_at) VALUES ('full', '', 'capability-probe', ?)`, now); err != nil {
			return ScanState{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ScanState{}, err
	}
	return s.GetScanState(ctx)
}

func stateSQLiteSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS streams (
			stream_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			latest_state_id INTEGER,
			projected_state_id INTEGER,
			latest_update_id INTEGER NOT NULL DEFAULT 0,
			latest_state_vector BLOB,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS stream_states (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			stream_id TEXT NOT NULL,
			state_update BLOB NOT NULL,
			state_vector BLOB NOT NULL,
			materialized_text_sha256 TEXT,
			created_at TEXT NOT NULL,
			FOREIGN KEY(stream_id) REFERENCES streams(stream_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_stream_states_stream_id ON stream_states(stream_id, id)`,
		`CREATE TABLE IF NOT EXISTS stream_inbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			stream_id TEXT NOT NULL,
			update_sha256 TEXT NOT NULL,
			update_bytes BLOB NOT NULL,
			remote_update_id INTEGER,
			received_at TEXT NOT NULL,
			applied_at TEXT,
			UNIQUE(stream_id, update_sha256)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_stream_inbox_unapplied ON stream_inbox(stream_id, applied_at, id)`,
		`CREATE TABLE IF NOT EXISTS stream_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			stream_id TEXT NOT NULL,
			mutation_key TEXT NOT NULL,
			update_sha256 TEXT NOT NULL,
			update_bytes BLOB NOT NULL,
			actor_id TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			reason TEXT NOT NULL,
			kind_hint TEXT,
			depends_on_id INTEGER,
			local_applied_at TEXT,
			created_at TEXT NOT NULL,
			sent_at TEXT,
			acked_at TEXT,
			ack_update_id INTEGER,
			dropped_at TEXT,
			drop_reason TEXT,
			UNIQUE(stream_id, mutation_key),
			FOREIGN KEY(depends_on_id) REFERENCES stream_outbox(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_stream_outbox_pending ON stream_outbox(acked_at, depends_on_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_stream_outbox_local_ready ON stream_outbox(stream_id, local_applied_at, depends_on_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_stream_outbox_sendable ON stream_outbox(dropped_at, acked_at, local_applied_at, depends_on_id, id)`,
		`CREATE TABLE IF NOT EXISTS manifest_projection (
			entry_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			content_stream_id TEXT,
			desired_path TEXT NOT NULL,
			materialized_path TEXT NOT NULL,
			file_key TEXT,
			size_bytes INTEGER,
			mode INTEGER,
			mtime_ns INTEGER,
			ctime_ns INTEGER,
			stat_valid INTEGER NOT NULL DEFAULT 0,
			last_clean_hash TEXT,
			root_projected_state_id INTEGER NOT NULL,
			tombstoned INTEGER NOT NULL DEFAULT 0,
			pending_create INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`DROP INDEX IF EXISTS idx_manifest_projection_materialized_path`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_manifest_projection_materialized_path ON manifest_projection(materialized_path) WHERE tombstoned = 0 AND materialized_path != ''`,
		`CREATE INDEX IF NOT EXISTS idx_manifest_projection_file_key ON manifest_projection(file_key)`,
		`CREATE TABLE IF NOT EXISTS content_projection (
			stream_id TEXT PRIMARY KEY,
			entry_id TEXT NOT NULL,
			materialized_path TEXT NOT NULL,
			projected_state_id INTEGER,
			projected_hash TEXT,
			file_key TEXT,
			size_bytes INTEGER,
			mode INTEGER,
			mtime_ns INTEGER,
			ctime_ns INTEGER,
			stat_valid INTEGER NOT NULL DEFAULT 0,
			dirty INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`DROP INDEX IF EXISTS idx_content_projection_materialized_path`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_content_projection_materialized_path ON content_projection(materialized_path) WHERE materialized_path != ''`,
		`CREATE TABLE IF NOT EXISTS scan_hints (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			path TEXT,
			reason TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scan_hints_kind_path ON scan_hints(kind, path)`,
		`CREATE TABLE IF NOT EXISTS directory_scan_cache (
			path TEXT PRIMARY KEY,
			mtime_ns INTEGER NOT NULL,
			ctime_ns INTEGER,
			entry_count INTEGER NOT NULL DEFAULT 0,
			children_json TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS scan_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			cursor_path TEXT,
			incomplete INTEGER NOT NULL DEFAULT 0,
			last_full_scan_at TEXT,
			directory_mtime_reliable INTEGER NOT NULL DEFAULT 0,
			file_key_reliable INTEGER NOT NULL DEFAULT 0,
			ctime_reliable INTEGER NOT NULL DEFAULT 0,
			capabilities_initialized INTEGER NOT NULL DEFAULT 0,
			last_capability_probe_at TEXT,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pending_content_creates (
			entry_id TEXT PRIMARY KEY,
			content_stream_id TEXT NOT NULL,
			materialized_path TEXT NOT NULL,
			observed_file_key TEXT,
			observed_size_bytes INTEGER,
			observed_mode INTEGER,
			observed_mtime_ns INTEGER,
			observed_ctime_ns INTEGER,
			observed_stat_valid INTEGER NOT NULL DEFAULT 0,
			root_mutation_key TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('needs_bytes', 'reading', 'outbox_created', 'completed', 'cancelled')),
			content_outbox_id INTEGER,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(content_outbox_id) REFERENCES stream_outbox(id),
			CHECK (
				observed_stat_valid = 0 OR (
					observed_file_key IS NOT NULL AND
					observed_size_bytes IS NOT NULL AND
					observed_mode IS NOT NULL AND
					observed_mtime_ns IS NOT NULL AND
					observed_ctime_ns IS NOT NULL
				)
			)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_content_creates_status ON pending_content_creates(status, created_at)`,
		`CREATE TABLE IF NOT EXISTS fs_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_key TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL,
			entry_id TEXT,
			stream_id TEXT,
			source_path TEXT,
			target_path TEXT,
			expected_hash TEXT,
			target_hash TEXT,
			target_state_id INTEGER,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fs_jobs_pending ON fs_jobs(status, id)`,
	}
}

func (s *WorkspaceStateDB) InsertScanHint(ctx context.Context, kind ScanHintKind, path string, reason string) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	path = normalizeStateRelPath(path)
	if isIgnoredStatePath(path) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM scan_hints`).Scan(&count); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if count >= MaxPendingScanHints {
		if _, err := tx.ExecContext(ctx, `DELETE FROM scan_hints`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO scan_hints(kind, path, reason, created_at) VALUES ('full', '', 'hint-overflow', ?)`, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "unspecified"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO scan_hints(kind, path, reason, created_at) VALUES (?, ?, ?, ?)`, string(kind), path, reason, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *WorkspaceStateDB) DrainScanHints(ctx context.Context, limit int) ([]ScanHint, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	if limit <= 0 {
		limit = ScanHintDrainLimit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE(path, ''), reason FROM scan_hints ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		id   int64
		hint ScanHint
	}
	drained := []row{}
	for rows.Next() {
		var next row
		if err := rows.Scan(&next.id, &next.hint.Kind, &next.hint.Path, &next.hint.Reason); err != nil {
			return nil, err
		}
		drained = append(drained, next)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(drained) == 0 {
		return nil, tx.Commit()
	}
	ids := make([]any, 0, len(drained))
	placeholders := make([]string, 0, len(drained))
	hints := make([]ScanHint, 0, len(drained))
	for _, next := range drained {
		ids = append(ids, next.id)
		placeholders = append(placeholders, "?")
		hints = append(hints, next.hint)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scan_hints WHERE id IN (`+strings.Join(placeholders, ",")+`)`, ids...); err != nil {
		return nil, err
	}
	return hints, tx.Commit()
}

func (s *WorkspaceStateDB) StoreDirectoryScanCache(ctx context.Context, path string, mtimeNS int64, ctimeNS int64, children []string) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	path = normalizeStateRelPath(path)
	if len(children) > MaxCachedDirChildren {
		_, err := s.db.ExecContext(ctx, `DELETE FROM directory_scan_cache WHERE path = ?`, path)
		return err
	}
	payload, err := json.Marshal(children)
	if err != nil {
		return err
	}
	if len(payload) > MaxCachedDirJSONBytes {
		_, err := s.db.ExecContext(ctx, `DELETE FROM directory_scan_cache WHERE path = ?`, path)
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO directory_scan_cache(path, mtime_ns, ctime_ns, entry_count, children_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			mtime_ns = excluded.mtime_ns,
			ctime_ns = excluded.ctime_ns,
			entry_count = excluded.entry_count,
			children_json = excluded.children_json,
			updated_at = excluded.updated_at`,
		path,
		mtimeNS,
		ctimeNS,
		len(children),
		string(payload),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *WorkspaceStateDB) LoadDirectoryScanCache(ctx context.Context, path string) (DirectoryScanCacheEntry, bool, error) {
	if s == nil || s.db == nil {
		return DirectoryScanCacheEntry{}, false, nil
	}
	path = normalizeStateRelPath(path)
	var entry DirectoryScanCacheEntry
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT path, mtime_ns, ctime_ns, children_json FROM directory_scan_cache WHERE path = ?`, path).Scan(
		&entry.Path,
		&entry.MTimeNS,
		&entry.CTimeNS,
		&payload,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectoryScanCacheEntry{}, false, nil
	}
	if err != nil {
		return DirectoryScanCacheEntry{}, false, err
	}
	if err := json.Unmarshal([]byte(payload), &entry.Children); err != nil {
		return DirectoryScanCacheEntry{}, false, err
	}
	return entry, true, nil
}

func (s *WorkspaceStateDB) DeleteDirectoryScanCache(ctx context.Context, path string) error {
	if s == nil || s.db == nil {
		return nil
	}
	path = normalizeStateRelPath(path)
	_, err := s.db.ExecContext(ctx, `DELETE FROM directory_scan_cache WHERE path = ?`, path)
	return err
}

func scanScanState(scanner interface{ Scan(...any) error }) (ScanState, error) {
	var state ScanState
	var incomplete int
	var directoryMTimeReliable int
	var fileKeyReliable int
	var ctimeReliable int
	var capabilitiesInitialized int
	if err := scanner.Scan(
		&state.CursorPath,
		&incomplete,
		&state.LastFullScanAt,
		&directoryMTimeReliable,
		&fileKeyReliable,
		&ctimeReliable,
		&capabilitiesInitialized,
		&state.LastCapabilityProbeAt,
	); err != nil {
		return ScanState{}, err
	}
	state.Incomplete = incomplete != 0
	state.DirectoryMTimeReliable = directoryMTimeReliable != 0
	state.FileKeyReliable = fileKeyReliable != 0
	state.CTimeReliable = ctimeReliable != 0
	state.CapabilitiesInitialized = capabilitiesInitialized != 0
	return state, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func normalizeStateRelPath(value string) string {
	value = filepath.ToSlash(filepath.Clean(strings.TrimSpace(value)))
	if value == "." {
		return ""
	}
	return strings.TrimPrefix(value, "/")
}

func isIgnoredStatePath(path string) bool {
	return isIgnoredWorkspaceRelativePath(path)
}
