package syncer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	crdt "notty/internal/ycrdt"
)

type ManifestProjectionRow struct {
	EntryID              string
	Kind                 string
	ContentStreamID      string
	DesiredPath          string
	MaterializedPath     string
	Stat                 FileStat
	LastCleanHash        string
	RootProjectedStateID int64
	Tombstoned           bool
	PendingCreate        bool
	UpdatedAt            string
}

type ContentProjectionRow struct {
	StreamID         string
	EntryID          string
	MaterializedPath string
	ProjectedStateID sql.NullInt64
	ProjectedHash    string
	Stat             FileStat
	Dirty            bool
	UpdatedAt        string
}

type FSJob struct {
	ID            int64
	JobKey        string
	Kind          string
	EntryID       string
	StreamID      string
	SourcePath    string
	TargetPath    string
	ExpectedHash  string
	TargetHash    string
	TargetStateID sql.NullInt64
	Status        string
	Attempts      int
	LastError     sql.NullString
	CreatedAt     string
	UpdatedAt     string
}

func (s *WorkspaceStateDB) LoadManifestProjection(ctx context.Context) (map[string]ManifestProjectionRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT entry_id, kind, content_stream_id, desired_path, materialized_path,
		       file_key, size_bytes, mode, mtime_ns, ctime_ns, stat_valid,
		       last_clean_hash, root_projected_state_id, tombstoned, pending_create, updated_at
		  FROM manifest_projection`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]ManifestProjectionRow{}
	for rows.Next() {
		row, err := scanManifestProjectionRow(rows)
		if err != nil {
			return nil, err
		}
		result[row.EntryID] = row
	}
	return result, rows.Err()
}

func (s *WorkspaceStateDB) UpsertManifestProjection(ctx context.Context, row ManifestProjectionRow) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if strings.TrimSpace(row.EntryID) == "" || strings.TrimSpace(row.Kind) == "" {
		return errors.New("manifest projection requires entry id and kind")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	stat := row.Stat
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if strings.TrimSpace(row.MaterializedPath) != "" && !row.Tombstoned {
		if _, err := tx.ExecContext(ctx, `
			UPDATE manifest_projection
			   SET materialized_path = '', updated_at = ?
			 WHERE entry_id != ?
			   AND materialized_path = ?
			   AND tombstoned = 0`,
			now,
			row.EntryID,
			row.MaterializedPath,
		); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO manifest_projection(
			entry_id, kind, content_stream_id, desired_path, materialized_path,
			file_key, size_bytes, mode, mtime_ns, ctime_ns, stat_valid,
			last_clean_hash, root_projected_state_id, tombstoned, pending_create, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(entry_id) DO UPDATE SET
			kind = excluded.kind,
			content_stream_id = excluded.content_stream_id,
			desired_path = excluded.desired_path,
			materialized_path = excluded.materialized_path,
			file_key = excluded.file_key,
			size_bytes = excluded.size_bytes,
			mode = excluded.mode,
			mtime_ns = excluded.mtime_ns,
			ctime_ns = excluded.ctime_ns,
			stat_valid = excluded.stat_valid,
			last_clean_hash = COALESCE(excluded.last_clean_hash, manifest_projection.last_clean_hash),
			root_projected_state_id = excluded.root_projected_state_id,
			tombstoned = excluded.tombstoned,
			pending_create = excluded.pending_create,
			updated_at = excluded.updated_at`,
		row.EntryID,
		row.Kind,
		nullString(row.ContentStreamID),
		row.DesiredPath,
		row.MaterializedPath,
		nullString(stat.FileKey),
		nullInt64AllowZero(stat.SizeBytes, stat.StatValid),
		nullInt64AllowZero(int64(stat.Mode), stat.StatValid),
		nullInt64AllowZero(stat.MTimeNS, stat.StatValid),
		nullInt64AllowZero(stat.CTimeNS, stat.StatValid),
		boolInt(stat.StatValid),
		nullString(row.LastCleanHash),
		row.RootProjectedStateID,
		boolInt(row.Tombstoned),
		boolInt(row.PendingCreate),
		now,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *WorkspaceStateDB) GetContentProjection(ctx context.Context, streamID string) (*ContentProjectionRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("state db is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT stream_id, entry_id, materialized_path, projected_state_id, projected_hash,
		       file_key, size_bytes, mode, mtime_ns, ctime_ns, stat_valid,
		       dirty, updated_at
		  FROM content_projection
		 WHERE stream_id = ?`, strings.TrimSpace(streamID))
	projection, err := scanContentProjectionRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &projection, nil
}

func (s *WorkspaceStateDB) UpsertContentProjection(ctx context.Context, row ContentProjectionRow) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if strings.TrimSpace(row.StreamID) == "" || strings.TrimSpace(row.EntryID) == "" || strings.TrimSpace(row.MaterializedPath) == "" {
		return errors.New("content projection requires stream id, entry id, and path")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	stat := row.Stat
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if strings.TrimSpace(row.MaterializedPath) != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE content_projection
			   SET materialized_path = '', updated_at = ?
			 WHERE stream_id != ?
			   AND materialized_path = ?`,
			now,
			row.StreamID,
			row.MaterializedPath,
		); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO content_projection(
			stream_id, entry_id, materialized_path, projected_state_id, projected_hash,
			file_key, size_bytes, mode, mtime_ns, ctime_ns, stat_valid,
			dirty, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stream_id) DO UPDATE SET
			entry_id = excluded.entry_id,
			materialized_path = excluded.materialized_path,
			projected_state_id = excluded.projected_state_id,
			projected_hash = excluded.projected_hash,
			file_key = excluded.file_key,
			size_bytes = excluded.size_bytes,
			mode = excluded.mode,
			mtime_ns = excluded.mtime_ns,
			ctime_ns = excluded.ctime_ns,
			stat_valid = excluded.stat_valid,
			dirty = excluded.dirty,
			updated_at = excluded.updated_at`,
		row.StreamID,
		row.EntryID,
		row.MaterializedPath,
		nullInt64FromSQL(row.ProjectedStateID),
		nullString(row.ProjectedHash),
		nullString(stat.FileKey),
		nullInt64AllowZero(stat.SizeBytes, stat.StatValid),
		nullInt64AllowZero(int64(stat.Mode), stat.StatValid),
		nullInt64AllowZero(stat.MTimeNS, stat.StatValid),
		nullInt64AllowZero(stat.CTimeNS, stat.StatValid),
		boolInt(stat.StatValid),
		boolInt(row.Dirty),
		now,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *WorkspaceStateDB) MarkContentProjectionDirty(ctx context.Context, streamID string, dirty bool) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE content_projection SET dirty = ?, updated_at = ? WHERE stream_id = ?`, boolInt(dirty), time.Now().UTC().Format(time.RFC3339Nano), streamID)
	return err
}

func (s *WorkspaceStateDB) HasBlockingFSJob(ctx context.Context, streamID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("state db is required")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fs_jobs WHERE stream_id = ? AND status IN ('pending', 'running')`, streamID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *WorkspaceStateDB) InsertFSJob(ctx context.Context, job FSJob) (FSJob, error) {
	if s == nil || s.db == nil {
		return FSJob{}, errors.New("state db is required")
	}
	job.JobKey = strings.TrimSpace(job.JobKey)
	job.Kind = strings.TrimSpace(job.Kind)
	if job.JobKey == "" || job.Kind == "" {
		return FSJob{}, errors.New("fs job requires job key and kind")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fs_jobs(
			job_key, kind, entry_id, stream_id, source_path, target_path,
			expected_hash, target_hash, target_state_id,
			status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		ON CONFLICT(job_key) DO NOTHING`,
		job.JobKey,
		job.Kind,
		nullString(job.EntryID),
		nullString(job.StreamID),
		nullString(job.SourcePath),
		nullString(job.TargetPath),
		nullString(job.ExpectedHash),
		nullString(job.TargetHash),
		nullInt64FromSQL(job.TargetStateID),
		now,
		now,
	)
	if err != nil {
		return FSJob{}, err
	}
	return s.getFSJobByKey(ctx, job.JobKey)
}

func (s *WorkspaceStateDB) RunPendingFSJobs(ctx context.Context, fs *WorkspaceFS) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if fs == nil {
		return errors.New("workspace fs is required")
	}
	for {
		job, err := s.nextPendingFSJob(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.markFSJobRunning(ctx, job.ID); err != nil {
			return err
		}
		if err := s.runFSJob(ctx, fs, job); err != nil {
			if markErr := s.markFSJobFailed(ctx, job.ID, err); markErr != nil {
				return markErr
			}
			return err
		}
		if err := s.markFSJobDone(ctx, job.ID); err != nil {
			return err
		}
	}
}

func (s *WorkspaceStateDB) runFSJob(ctx context.Context, fs *WorkspaceFS, job FSJob) error {
	switch job.Kind {
	case "write-content":
		if !job.TargetStateID.Valid {
			return errors.New("write-content job requires target state id")
		}
		state, err := s.LoadStreamState(ctx, job.TargetStateID.Int64)
		if err != nil {
			return err
		}
		doc := crdt.New(crdt.WithGUID(job.StreamID))
		defer doc.Close()
		if err := crdt.ApplyUpdateV1(doc, state.StateUpdate, "fs-job"); err != nil {
			return err
		}
		content := []byte(doc.GetText("content").ToString())
		targetHash := contentSHA256(content)
		if job.TargetHash != "" && targetHash != job.TargetHash {
			return fmt.Errorf("write-content job target hash mismatch: got %s want %s", targetHash, job.TargetHash)
		}
		if ok, stat, err := fs.targetAlreadyHasHash(ctx, job.TargetPath, targetHash); err != nil {
			return err
		} else if ok {
			return s.finishWriteContentJob(ctx, job, targetHash, stat)
		} else if job.ExpectedHash == "" && stat.Exists {
			_ = s.markWriteContentJobDiverged(ctx, job, targetHash, stat)
			return &FSError{Op: "write", Path: job.TargetPath, Err: ErrDivergedWorkingCopy}
		}
		if err := fs.WriteIfSHA256Unchanged(ctx, job.TargetPath, job.ExpectedHash, content); err != nil {
			if errors.Is(err, ErrDivergedWorkingCopy) {
				_ = s.MarkContentProjectionDirty(ctx, job.StreamID, true)
			}
			return err
		}
		stat, err := fs.Stat(ctx, job.TargetPath)
		if err != nil {
			return err
		}
		return s.finishWriteContentJob(ctx, job, targetHash, stat)
	case "move-entry":
		return fs.MoveIfNoTarget(job.SourcePath, job.TargetPath)
	case "delete-clean-entry":
		return fs.DeleteIfSHA256Unchanged(ctx, job.TargetPath, job.ExpectedHash)
	case "mkdir":
		return os.MkdirAll(fs.Abs(job.TargetPath), 0o755)
	default:
		return fmt.Errorf("unknown fs job kind %q", job.Kind)
	}
}

func (s *WorkspaceStateDB) finishWriteContentJob(ctx context.Context, job FSJob, targetHash string, stat FileStat) error {
	if err := s.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         job.StreamID,
		EntryID:          job.EntryID,
		MaterializedPath: normalizeStateRelPath(job.TargetPath),
		ProjectedStateID: job.TargetStateID,
		ProjectedHash:    targetHash,
		Stat:             stat,
		Dirty:            false,
	}); err != nil {
		return err
	}
	if err := s.UpdateProjectedStreamState(ctx, job.StreamID, job.TargetStateID.Int64); err != nil {
		return err
	}
	if err := s.UpdateManifestCleanHash(ctx, job.EntryID, targetHash, stat); err != nil {
		return err
	}
	return s.TryCompletePendingContentCreate(ctx, job.StreamID, job.TargetStateID.Int64)
}

func (s *WorkspaceStateDB) markWriteContentJobDiverged(ctx context.Context, job FSJob, targetHash string, stat FileStat) error {
	return s.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         job.StreamID,
		EntryID:          job.EntryID,
		MaterializedPath: normalizeStateRelPath(job.TargetPath),
		ProjectedStateID: job.TargetStateID,
		ProjectedHash:    targetHash,
		Stat:             stat,
		Dirty:            true,
	})
}

func (fs *WorkspaceFS) targetAlreadyHasHash(ctx context.Context, rel string, targetHash string) (bool, FileStat, error) {
	if fs == nil || targetHash == "" {
		return false, FileStat{}, nil
	}
	stat, err := fs.Stat(ctx, rel)
	if err != nil {
		return false, FileStat{}, err
	}
	if !stat.Exists || stat.Kind != FileKindFile || stat.SizeBytes > MaxSinglePendingCreateBytes {
		return false, stat, nil
	}
	read, ok, err := fs.ReadBytesStable(ctx, rel, StableReadOptions{
		ExpectedStat: &stat,
		MaxBytes:     MaxSinglePendingCreateBytes,
	})
	if err != nil || !ok {
		return false, stat, err
	}
	if contentSHA256(read.Bytes) != targetHash {
		return false, read.FinalStat, nil
	}
	return true, read.FinalStat, nil
}

func (s *WorkspaceStateDB) UpdateManifestCleanHash(ctx context.Context, entryID string, hash string, stat FileStat) error {
	if s == nil || s.db == nil {
		return errors.New("state db is required")
	}
	if strings.TrimSpace(entryID) == "" {
		return errors.New("entry id is required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE manifest_projection
		   SET last_clean_hash = ?,
		       file_key = ?,
		       size_bytes = ?,
		       mode = ?,
		       mtime_ns = ?,
		       ctime_ns = ?,
		       stat_valid = ?,
		       updated_at = ?
		 WHERE entry_id = ?`,
		nullString(hash),
		nullString(stat.FileKey),
		nullInt64AllowZero(stat.SizeBytes, stat.StatValid),
		nullInt64AllowZero(int64(stat.Mode), stat.StatValid),
		nullInt64AllowZero(stat.MTimeNS, stat.StatValid),
		nullInt64AllowZero(stat.CTimeNS, stat.StatValid),
		boolInt(stat.StatValid),
		time.Now().UTC().Format(time.RFC3339Nano),
		entryID,
	)
	return err
}

func (s *WorkspaceStateDB) nextPendingFSJob(ctx context.Context) (FSJob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, job_key, kind, entry_id, stream_id, source_path, target_path,
		       expected_hash, target_hash, target_state_id, status, attempts, last_error, created_at, updated_at
		  FROM fs_jobs
		 WHERE status = 'pending'
		 ORDER BY id ASC
		 LIMIT 1`)
	return scanFSJob(row)
}

func (s *WorkspaceStateDB) getFSJobByKey(ctx context.Context, key string) (FSJob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, job_key, kind, entry_id, stream_id, source_path, target_path,
		       expected_hash, target_hash, target_state_id, status, attempts, last_error, created_at, updated_at
		  FROM fs_jobs
		 WHERE job_key = ?`, key)
	return scanFSJob(row)
}

func (s *WorkspaceStateDB) markFSJobRunning(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fs_jobs SET status = 'running', attempts = attempts + 1, updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *WorkspaceStateDB) markFSJobDone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fs_jobs SET status = 'done', last_error = NULL, updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *WorkspaceStateDB) markFSJobFailed(ctx context.Context, id int64, runErr error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fs_jobs SET status = 'failed', last_error = ?, updated_at = ? WHERE id = ?`, runErr.Error(), time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func scanManifestProjectionRow(scanner interface{ Scan(...any) error }) (ManifestProjectionRow, error) {
	var row ManifestProjectionRow
	var contentStreamID sql.NullString
	var fileKey sql.NullString
	var size sql.NullInt64
	var mode sql.NullInt64
	var mtime sql.NullInt64
	var ctime sql.NullInt64
	var statValid int
	var lastCleanHash sql.NullString
	var tombstoned int
	var pendingCreate int
	if err := scanner.Scan(
		&row.EntryID,
		&row.Kind,
		&contentStreamID,
		&row.DesiredPath,
		&row.MaterializedPath,
		&fileKey,
		&size,
		&mode,
		&mtime,
		&ctime,
		&statValid,
		&lastCleanHash,
		&row.RootProjectedStateID,
		&tombstoned,
		&pendingCreate,
		&row.UpdatedAt,
	); err != nil {
		return ManifestProjectionRow{}, err
	}
	row.ContentStreamID = contentStreamID.String
	row.LastCleanHash = lastCleanHash.String
	row.Tombstoned = tombstoned != 0
	row.PendingCreate = pendingCreate != 0
	row.Stat = FileStat{
		Path:      row.MaterializedPath,
		Kind:      FileKind(row.Kind),
		Exists:    statValid != 0,
		FileKey:   fileKey.String,
		SizeBytes: size.Int64,
		Mode:      uint32(mode.Int64),
		MTimeNS:   mtime.Int64,
		CTimeNS:   ctime.Int64,
		StatValid: statValid != 0,
	}
	return row, nil
}

func scanContentProjectionRow(scanner interface{ Scan(...any) error }) (ContentProjectionRow, error) {
	var row ContentProjectionRow
	var projectedHash sql.NullString
	var fileKey sql.NullString
	var size sql.NullInt64
	var mode sql.NullInt64
	var mtime sql.NullInt64
	var ctime sql.NullInt64
	var statValid int
	var dirty int
	if err := scanner.Scan(
		&row.StreamID,
		&row.EntryID,
		&row.MaterializedPath,
		&row.ProjectedStateID,
		&projectedHash,
		&fileKey,
		&size,
		&mode,
		&mtime,
		&ctime,
		&statValid,
		&dirty,
		&row.UpdatedAt,
	); err != nil {
		return ContentProjectionRow{}, err
	}
	row.ProjectedHash = projectedHash.String
	row.Dirty = dirty != 0
	row.Stat = FileStat{
		Path:      row.MaterializedPath,
		Kind:      FileKindFile,
		Exists:    statValid != 0,
		FileKey:   fileKey.String,
		SizeBytes: size.Int64,
		Mode:      uint32(mode.Int64),
		MTimeNS:   mtime.Int64,
		CTimeNS:   ctime.Int64,
		StatValid: statValid != 0,
	}
	return row, nil
}

func scanFSJob(scanner interface{ Scan(...any) error }) (FSJob, error) {
	var job FSJob
	var entryID sql.NullString
	var streamID sql.NullString
	var sourcePath sql.NullString
	var targetPath sql.NullString
	var expectedHash sql.NullString
	var targetHash sql.NullString
	if err := scanner.Scan(
		&job.ID,
		&job.JobKey,
		&job.Kind,
		&entryID,
		&streamID,
		&sourcePath,
		&targetPath,
		&expectedHash,
		&targetHash,
		&job.TargetStateID,
		&job.Status,
		&job.Attempts,
		&job.LastError,
		&job.CreatedAt,
		&job.UpdatedAt,
	); err != nil {
		return FSJob{}, err
	}
	job.EntryID = entryID.String
	job.StreamID = streamID.String
	job.SourcePath = sourcePath.String
	job.TargetPath = targetPath.String
	job.ExpectedHash = expectedHash.String
	job.TargetHash = targetHash.String
	return job, nil
}

func contentSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nullInt64AllowZero(value int64, valid bool) any {
	if !valid {
		return nil
	}
	return value
}

func nullInt64FromSQL(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}
