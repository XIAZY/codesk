package syncer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamProjectionSeedsDocumentPathWhenTargetAbsent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	projection := &streamProjection{state: state, fs: NewWorkspaceFS(root), rootDir: root}
	if err := projection.ensureDocumentProjectionPath(ctx, &document{ID: "doc_remote", Path: "docs/a.md"}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}

	row, err := state.GetContentProjection(ctx, "doc_remote")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if row == nil || row.EntryID != "doc_remote" || row.MaterializedPath != "docs/a.md" {
		t.Fatalf("unexpected seeded projection %#v", row)
	}
}

func TestStreamProjectionDoesNotClaimExistingUntrackedPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	projection := &streamProjection{state: state, fs: NewWorkspaceFS(root), rootDir: root}
	if err := projection.ensureDocumentProjectionPath(ctx, &document{ID: "doc_remote", Path: "README.md"}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}

	row, err := state.GetContentProjection(ctx, "doc_remote")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if row != nil {
		t.Fatalf("existing untracked file should not be claimed, got %#v", row)
	}
}

func TestStreamProjectionRestoresEmptyProjectionPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if _, err := state.DB().ExecContext(ctx, `
		INSERT INTO content_projection(
			stream_id, entry_id, materialized_path, projected_state_id, projected_hash,
			file_key, size_bytes, mode, mtime_ns, ctime_ns, stat_valid, dirty, updated_at
		) VALUES ('doc_remote', 'doc_remote', '', 7, 'hash-before', NULL, NULL, NULL, NULL, NULL, 0, 1, '2026-05-23T00:00:00Z')`); err != nil {
		t.Fatalf("seed empty content projection: %v", err)
	}

	projection := &streamProjection{state: state, fs: NewWorkspaceFS(root), rootDir: root}
	if err := projection.ensureDocumentProjectionPath(ctx, &document{ID: "doc_remote", Path: "README.md"}); err != nil {
		t.Fatalf("restore projection: %v", err)
	}

	row, err := state.GetContentProjection(ctx, "doc_remote")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if row == nil || row.MaterializedPath != "README.md" || !row.ProjectedStateID.Valid || row.ProjectedStateID.Int64 != 7 || row.ProjectedHash != "hash-before" || !row.Dirty {
		t.Fatalf("unexpected restored projection %#v", row)
	}
	var stateID sql.NullInt64
	if err := state.DB().QueryRow(`SELECT projected_state_id FROM content_projection WHERE stream_id = 'doc_remote'`).Scan(&stateID); err != nil {
		t.Fatalf("read projected state: %v", err)
	}
	if !stateID.Valid || stateID.Int64 != 7 {
		t.Fatalf("projected state was not preserved: %#v", stateID)
	}
}
