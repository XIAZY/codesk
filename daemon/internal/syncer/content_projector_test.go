package syncer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestContentProjectorCapturesLocalEditFromProjectedBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	doc := contentDoc(t, "doc", "alpha")
	stateID, err := state.PersistLatestStreamDoc(ctx, "doc", doc, contentSHA256([]byte("alpha")))
	if err != nil {
		t.Fatalf("persist base: %v", err)
	}
	stat, err := fs.Stat(ctx, "doc.md")
	if err != nil {
		t.Fatalf("stat projected file: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc",
		EntryID:          "doc",
		MaterializedPath: "doc.md",
		ProjectedStateID: sql.NullInt64{Int64: stateID, Valid: true},
		ProjectedHash:    contentSHA256([]byte("alpha")),
		Stat:             stat,
	}); err != nil {
		t.Fatalf("upsert content projection: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("alpha\nlocal\n"), 0o644); err != nil {
		t.Fatalf("edit file: %v", err)
	}

	mutations, err := (ContentProjector{
		State:    state,
		FS:       fs,
		StreamID: "doc",
		ActorID:  "daemon",
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected one local content mutation, got %#v", mutations)
	}
	if mutations[0].StreamID != "doc" || mutations[0].KindHint != "content" {
		t.Fatalf("unexpected mutation metadata %#v", mutations[0])
	}
	if err := crdt.ApplyUpdateV1(doc, mutations[0].UpdateBytes, "local"); err != nil {
		t.Fatalf("apply local mutation: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "alpha\nlocal\n" {
		t.Fatalf("unexpected merged text %q", got)
	}
}

func TestContentProjectorCapturesDirtyAppendButSkipsDirtyRewrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("alpha\nlocal\n"), 0o644); err != nil {
		t.Fatalf("write append file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	base := contentDoc(t, "doc", "alpha\n")
	baseID, err := state.PersistLatestStreamDoc(ctx, "doc", base, contentSHA256([]byte("alpha\n")))
	if err != nil {
		t.Fatalf("persist base: %v", err)
	}
	stat, err := fs.Stat(ctx, "doc.md")
	if err != nil {
		t.Fatalf("stat append file: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc",
		EntryID:          "doc",
		MaterializedPath: "doc.md",
		ProjectedStateID: sql.NullInt64{Int64: baseID, Valid: true},
		ProjectedHash:    contentSHA256([]byte("alpha\n")),
		Stat:             stat,
		Dirty:            true,
	}); err != nil {
		t.Fatalf("upsert dirty projection: %v", err)
	}
	remote := contentDoc(t, "doc", "alpha\nremote\n")
	if _, err := state.PersistLatestStreamDoc(ctx, "doc", remote, contentSHA256([]byte("alpha\nremote\n"))); err != nil {
		t.Fatalf("persist remote: %v", err)
	}
	mutations, err := (ContentProjector{State: state, FS: fs, StreamID: "doc"}).CaptureLocal(ctx, base)
	if err != nil {
		t.Fatalf("capture dirty append: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected dirty append mutation, got %#v", mutations)
	}

	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("replacement\n"), 0o644); err != nil {
		t.Fatalf("write rewrite file: %v", err)
	}
	mutations, err = (ContentProjector{State: state, FS: fs, StreamID: "doc"}).CaptureLocal(ctx, base)
	if err != nil {
		t.Fatalf("capture dirty rewrite: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("expected dirty rewrite to remain local-only, got %#v", mutations)
	}
}

func TestContentProjectorPlansAndRunsSafeRemoteWrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	base := contentDoc(t, "doc", "alpha")
	baseID, err := state.PersistLatestStreamDoc(ctx, "doc", base, contentSHA256([]byte("alpha")))
	if err != nil {
		t.Fatalf("persist base: %v", err)
	}
	stat, err := fs.Stat(ctx, "doc.md")
	if err != nil {
		t.Fatalf("stat projected file: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc",
		EntryID:          "doc",
		MaterializedPath: "doc.md",
		ProjectedStateID: sql.NullInt64{Int64: baseID, Valid: true},
		ProjectedHash:    contentSHA256([]byte("alpha")),
		Stat:             stat,
	}); err != nil {
		t.Fatalf("upsert content projection: %v", err)
	}
	remote := contentDoc(t, "doc", "remote")
	remoteID, err := state.PersistLatestStreamDoc(ctx, "doc", remote, contentSHA256([]byte("remote")))
	if err != nil {
		t.Fatalf("persist remote: %v", err)
	}
	if err := (ContentProjector{State: state, StreamID: "doc"}).PlanApplyMerged(ctx, remote, remoteID); err != nil {
		t.Fatalf("plan remote write: %v", err)
	}
	if err := state.RunPendingFSJobs(ctx, fs); err != nil {
		t.Fatalf("run fs jobs: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "doc.md"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(content) != "remote" {
		t.Fatalf("unexpected projected content %q", string(content))
	}
	projection, err := state.GetContentProjection(ctx, "doc")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if projection == nil || !projection.ProjectedStateID.Valid || projection.ProjectedStateID.Int64 != remoteID || projection.ProjectedHash != contentSHA256([]byte("remote")) || projection.Dirty {
		t.Fatalf("unexpected projection %#v", projection)
	}
}

func TestWriteContentFSJobInfersDoneWhenTargetHashAlreadyPresent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("remote"), 0o644); err != nil {
		t.Fatalf("write already-projected file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	remote := contentDoc(t, "doc", "remote")
	remoteID, err := state.PersistLatestStreamDoc(ctx, "doc", remote, contentSHA256([]byte("remote")))
	if err != nil {
		t.Fatalf("persist remote: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc",
		EntryID:          "doc",
		MaterializedPath: "doc.md",
		ProjectedStateID: sql.NullInt64{Int64: remoteID - 1, Valid: remoteID > 1},
		ProjectedHash:    contentSHA256([]byte("old")),
	}); err != nil {
		t.Fatalf("upsert stale content projection: %v", err)
	}
	if _, err := state.InsertFSJob(ctx, FSJob{
		JobKey:        "content:write:doc:idempotent",
		Kind:          "write-content",
		StreamID:      "doc",
		EntryID:       "doc",
		TargetPath:    "doc.md",
		ExpectedHash:  contentSHA256([]byte("old")),
		TargetHash:    contentSHA256([]byte("remote")),
		TargetStateID: sql.NullInt64{Int64: remoteID, Valid: true},
	}); err != nil {
		t.Fatalf("insert write job: %v", err)
	}
	if err := state.RunPendingFSJobs(ctx, fs); err != nil {
		t.Fatalf("run fs jobs: %v", err)
	}
	projection, err := state.GetContentProjection(ctx, "doc")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if projection == nil || !projection.ProjectedStateID.Valid || projection.ProjectedStateID.Int64 != remoteID || projection.ProjectedHash != contentSHA256([]byte("remote")) || projection.Dirty {
		t.Fatalf("expected idempotent write completion to advance projection, got %#v", projection)
	}
	var status string
	if err := state.DB().QueryRow(`SELECT status FROM fs_jobs WHERE job_key = 'content:write:doc:idempotent'`).Scan(&status); err != nil {
		t.Fatalf("read fs job status: %v", err)
	}
	if status != "done" {
		t.Fatalf("expected idempotent write job marked done, got %q", status)
	}
}

func TestContentProjectorDoesNotOverwriteDirtyRemoteWrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	remote := contentDoc(t, "doc", "remote")
	remoteID, err := state.PersistLatestStreamDoc(ctx, "doc", remote, contentSHA256([]byte("remote")))
	if err != nil {
		t.Fatalf("persist remote: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc",
		EntryID:          "doc",
		MaterializedPath: "doc.md",
		ProjectedStateID: sql.NullInt64{Int64: remoteID, Valid: true},
		ProjectedHash:    contentSHA256([]byte("alpha")),
	}); err != nil {
		t.Fatalf("upsert content projection: %v", err)
	}
	if err := (ContentProjector{State: state, StreamID: "doc"}).PlanApplyMerged(ctx, remote, remoteID); err != nil {
		t.Fatalf("plan remote write: %v", err)
	}
	err = state.RunPendingFSJobs(ctx, fs)
	if !errors.Is(err, ErrDivergedWorkingCopy) {
		t.Fatalf("expected diverged working copy, got %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "doc.md"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(content) != "dirty" {
		t.Fatalf("dirty bytes were overwritten: %q", string(content))
	}
	projection, err := state.GetContentProjection(ctx, "doc")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if projection == nil || !projection.Dirty || !projection.ProjectedStateID.Valid || projection.ProjectedStateID.Int64 != remoteID {
		t.Fatalf("expected dirty projection, got %#v", projection)
	}
}

func TestWriteContentFSJobDoesNotOverwriteExistingFileWithUnknownBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("local still changing"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	remote := contentDoc(t, "doc", "remote snapshot")
	remoteID, err := state.PersistLatestStreamDoc(ctx, "doc", remote, contentSHA256([]byte("remote snapshot")))
	if err != nil {
		t.Fatalf("persist remote: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc",
		EntryID:          "doc",
		MaterializedPath: "doc.md",
	}); err != nil {
		t.Fatalf("upsert content projection: %v", err)
	}
	if _, err := state.InsertFSJob(ctx, FSJob{
		JobKey:        "content:write:doc:unknown-base",
		Kind:          "write-content",
		StreamID:      "doc",
		EntryID:       "doc",
		TargetPath:    "doc.md",
		TargetHash:    contentSHA256([]byte("remote snapshot")),
		TargetStateID: sql.NullInt64{Int64: remoteID, Valid: true},
	}); err != nil {
		t.Fatalf("insert write job: %v", err)
	}
	err = state.RunPendingFSJobs(ctx, fs)
	if !errors.Is(err, ErrDivergedWorkingCopy) {
		t.Fatalf("expected diverged working copy, got %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "doc.md"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(content) != "local still changing" {
		t.Fatalf("local bytes were overwritten: %q", string(content))
	}
	projection, err := state.GetContentProjection(ctx, "doc")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if projection == nil || !projection.Dirty {
		t.Fatalf("expected dirty projection, got %#v", projection)
	}
}

func contentDoc(t *testing.T, guid string, content string) *crdt.Doc {
	t.Helper()
	doc := crdt.New(crdt.WithGUID(guid))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, content, nil)
	}, "test")
	return doc
}
