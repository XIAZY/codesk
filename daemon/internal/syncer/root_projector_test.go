package syncer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"notty/internal/rootmanifest"
	crdt "notty/internal/ycrdt"
)

func TestRootManifestProjectorLocalCreateIsStatOnlyAndCreatesPendingContent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	ids := map[string]string{"dir:docs": "dir_docs", "file:docs/a.md": "doc_a"}

	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		Capabilities: ScanCapabilities{FileKeyReliable: true, CTimeReliable: true, DirectoryMTimeReliable: true},
		NewID: func(kind string, relPath string) string {
			return ids[kind+":"+relPath]
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected one root mutation, got %#v", mutations)
	}
	if mutations[0].StreamID != "root-stream" || mutations[0].KindHint != "root" {
		t.Fatalf("unexpected root mutation %#v", mutations[0])
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if entry := manifest.EntriesByID["doc_a"]; entry.ID != "doc_a" || entry.Kind != rootmanifest.EntryKindFile || entry.ContentStreamID != "doc_a" || entry.Loc.ParentID != "dir_docs" {
		t.Fatalf("unexpected created file entry %#v", entry)
	}
	if entry := manifest.EntriesByID["dir_docs"]; entry.ID != "dir_docs" || entry.Kind != rootmanifest.EntryKindDir || entry.Loc.ParentID != rootmanifest.RootEntryID {
		t.Fatalf("unexpected created dir entry %#v", entry)
	}
	var pendingPath string
	if err := state.DB().QueryRow(`SELECT materialized_path FROM pending_content_creates WHERE entry_id = 'doc_a' AND status = 'needs_bytes'`).Scan(&pendingPath); err != nil {
		t.Fatalf("pending content create missing: %v", err)
	}
	if pendingPath != "docs/a.md" {
		t.Fatalf("unexpected pending path %q", pendingPath)
	}
	var outboxCount int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM stream_outbox`).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("root scan should not read bytes or create content outbox, got %d outbox rows", outboxCount)
	}
}

func TestRootManifestProjectorLocalDeleteTombstonesTrackedEntry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "a.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "a.md",
		MaterializedPath:     "a.md",
		LastCleanHash:        contentSHA256([]byte("alpha")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}

	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture delete: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected tombstone mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_a"].Tombstone == nil {
		t.Fatalf("expected doc_a tombstoned, got %#v", manifest.EntriesByID["doc_a"])
	}
}

func TestRootManifestProjectorDoesNotTombstoneUnprojectedRemoteCreate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-dir",
		Entry: rootmanifest.Entry{
			ID:   "dir_docs",
			Kind: rootmanifest.EntryKindDir,
			Loc:  rootmanifest.NewLocation(rootmanifest.RootEntryID, "docs"),
		},
	}, {
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation("dir_docs", "a.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "dir_docs",
		Kind:                 rootmanifest.EntryKindDir,
		DesiredPath:          "docs",
		MaterializedPath:     "docs",
		RootProjectedStateID: 1,
		PendingCreate:        false,
	}); err != nil {
		t.Fatalf("seed dir projection: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "docs/a.md",
		MaterializedPath:     "docs/a.md",
		RootProjectedStateID: 1,
		PendingCreate:        false,
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}

	queued := []string{}
	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("unprojected remote create should not tombstone, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_a"].Tombstone != nil {
		t.Fatalf("unprojected remote create was tombstoned: %#v", manifest.EntriesByID["doc_a"])
	}
	if manifest.EntriesByID["dir_docs"].Tombstone != nil {
		t.Fatalf("unprojected remote directory was tombstoned: %#v", manifest.EntriesByID["dir_docs"])
	}
	if len(queued) != 1 || queued[0] != "doc_a" {
		t.Fatalf("expected content stream requeued, got %#v", queued)
	}
	job, err := state.getFSJobByKey(ctx, "root:mkdir:dir_docs:"+hashKey("docs"))
	if err != nil {
		t.Fatalf("expected mkdir job for unprojected remote directory: %v", err)
	}
	if job.Kind != "mkdir" || job.TargetPath != "docs" {
		t.Fatalf("unexpected mkdir job %#v", job)
	}
}

func TestRootManifestProjectorPreservesUntrackedLocalFileCollidingWithRemoteEntry(t *testing.T) {
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
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_remote",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "README.md"),
			ContentStreamID: "doc_remote",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}

	queued := []string{}
	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		NewID: func(kind string, relPath string) string {
			if kind == "file" && relPath == "README.md" {
				return "doc_local"
			}
			return kind + "_unexpected"
		},
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected local-create mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_remote"].Tombstone != nil {
		t.Fatalf("remote entry should remain live: %#v", manifest.EntriesByID["doc_remote"])
	}
	if entry := manifest.EntriesByID["doc_local"]; entry.ID != "doc_local" || entry.Loc == nil || entry.Loc.Name != "README.md" {
		t.Fatalf("expected local colliding entry, got %#v", entry)
	}
	if len(queued) != 1 || queued[0] != "doc_remote" {
		t.Fatalf("expected remote content stream requeued, got %#v", queued)
	}
	var pendingPath string
	if err := state.DB().QueryRow(`SELECT materialized_path FROM pending_content_creates WHERE entry_id = 'doc_local'`).Scan(&pendingPath); err != nil {
		t.Fatalf("pending local create missing: %v", err)
	}
	if pendingPath != "README.md" {
		t.Fatalf("unexpected pending path %q", pendingPath)
	}
}

func TestRootManifestProjectorPlanRemoteCreateTracksContentProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "a.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}

	queued := []string{}
	if err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).PlanApplyMerged(ctx, doc, 3); err != nil {
		t.Fatalf("plan remote root: %v", err)
	}
	projection, err := state.GetContentProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if projection == nil || projection.EntryID != "doc_a" || projection.MaterializedPath != "a.md" || projection.ProjectedStateID.Valid {
		t.Fatalf("unexpected content projection %#v", projection)
	}
	record, err := state.GetStream(ctx, "doc_a")
	if err != nil {
		t.Fatalf("get content stream: %v", err)
	}
	if record.Kind != "content" {
		t.Fatalf("expected content stream kind, got %#v", record)
	}
	var rootStateID int64
	if err := state.DB().QueryRow(`SELECT root_projected_state_id FROM manifest_projection WHERE entry_id = 'doc_a'`).Scan(&rootStateID); err != nil {
		t.Fatalf("read manifest projection: %v", err)
	}
	if rootStateID != 3 {
		t.Fatalf("unexpected root projected state %d", rootStateID)
	}
	if len(queued) != 1 || queued[0] != "doc_a" {
		t.Fatalf("expected content stream queued after remote create, got %#v", queued)
	}
}

func TestRootManifestProjectorPlanRemoteRenameSchedulesMove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "old.md",
		MaterializedPath:     "old.md",
		LastCleanHash:        contentSHA256([]byte("alpha")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_a",
		EntryID:          "doc_a",
		MaterializedPath: "old.md",
		ProjectedHash:    contentSHA256([]byte("alpha")),
		ProjectedStateID: sql.NullInt64{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "new.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := (RootManifestProjector{State: state, FS: NewWorkspaceFS(root), RootStreamID: "root-stream"}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan rename: %v", err)
	}
	var kind, source, target string
	if err := state.DB().QueryRow(`SELECT kind, source_path, target_path FROM fs_jobs WHERE entry_id = 'doc_a'`).Scan(&kind, &source, &target); err != nil {
		t.Fatalf("read fs job: %v", err)
	}
	if kind != "move-entry" || source != "old.md" || target != "new.md" {
		t.Fatalf("unexpected move job kind=%q source=%q target=%q", kind, source, target)
	}
}

func TestRootManifestProjectorDetectsCleanHashMoveWithoutFileKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "new.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write moved file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "old.md",
		MaterializedPath:     "old.md",
		LastCleanHash:        contentSHA256([]byte("alpha")),
		RootProjectedStateID: 1,
		Stat: FileStat{
			Path:      "old.md",
			Kind:      FileKindFile,
			Exists:    true,
			SizeBytes: int64(len("alpha")),
			StatValid: true,
		},
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "old.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		Capabilities: ScanCapabilities{FileKeyReliable: false},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected move mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	entry := manifest.EntriesByID["doc_a"]
	if entry.Tombstone != nil {
		t.Fatalf("clean hash move should not tombstone entry: %#v", entry)
	}
	if entry.Loc == nil || entry.Loc.Name != "new.md" {
		t.Fatalf("expected loc updated to new.md, got %#v", entry)
	}
}

func TestRootManifestProjectorRemoteDeletePreservesDirtyBytes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "a.md",
		MaterializedPath:     "a.md",
		LastCleanHash:        contentSHA256([]byte("clean")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "a.md"),
			ContentStreamID: "doc_a",
		},
	}, {
		Type:      "tombstone",
		EntryID:   "doc_a",
		Tombstone: &rootmanifest.Tombstone{ActorID: "peer", ActorType: "daemon", At: "2026-05-23T00:00:00Z"},
	}}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	if err := (RootManifestProjector{State: state, FS: NewWorkspaceFS(root), RootStreamID: "root-stream"}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan tombstone: %v", err)
	}
	var jobs int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM fs_jobs WHERE kind = 'delete-clean-entry'`).Scan(&jobs); err != nil {
		t.Fatalf("count delete jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("dirty remote delete must not schedule delete job, got %d", jobs)
	}
	content, err := os.ReadFile(filepath.Join(root, "a.md"))
	if err != nil {
		t.Fatalf("read dirty file: %v", err)
	}
	if string(content) != "dirty" {
		t.Fatalf("dirty file changed to %q", string(content))
	}
}

func TestRootManifestProjectorRemoteDeleteSchedulesCleanDelete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("clean"), 0o644); err != nil {
		t.Fatalf("write clean file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "a.md",
		MaterializedPath:     "a.md",
		LastCleanHash:        contentSHA256([]byte("clean")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "a.md"),
			ContentStreamID: "doc_a",
		},
	}, {
		Type:      "tombstone",
		EntryID:   "doc_a",
		Tombstone: &rootmanifest.Tombstone{ActorID: "peer", ActorType: "daemon", At: "2026-05-23T00:00:00Z"},
	}}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	fs := NewWorkspaceFS(root)
	if err := (RootManifestProjector{State: state, FS: fs, RootStreamID: "root-stream"}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan tombstone: %v", err)
	}
	if err := state.RunPendingFSJobs(ctx, fs); err != nil {
		t.Fatalf("run delete job: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.md")); !os.IsNotExist(err) {
		t.Fatalf("expected clean file deleted, stat err=%v", err)
	}
}

func TestRootManifestProjectorLocalDirectoryRenameKeepsChildren(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	dirStat, err := fs.Stat(ctx, "docs")
	if err != nil {
		t.Fatalf("stat docs: %v", err)
	}
	fileStat, err := fs.Stat(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("stat docs/a.md: %v", err)
	}
	if dirStat.FileKey == "" {
		t.Skip("filesystem did not provide directory file key")
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-dir",
		Entry: rootmanifest.Entry{
			ID:   "dir_docs",
			Kind: rootmanifest.EntryKindDir,
			Loc:  rootmanifest.NewLocation(rootmanifest.RootEntryID, "docs"),
		},
	}, {
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation("dir_docs", "a.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "dir_docs",
		Kind:                 rootmanifest.EntryKindDir,
		DesiredPath:          "docs",
		MaterializedPath:     "docs",
		Stat:                 dirStat,
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed dir projection: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "docs/a.md",
		MaterializedPath:     "docs/a.md",
		Stat:                 fileStat,
		LastCleanHash:        contentSHA256([]byte("alpha")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed file projection: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "docs"), filepath.Join(root, "notes")); err != nil {
		t.Fatalf("rename dir: %v", err)
	}
	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           fs,
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		Capabilities: ScanCapabilities{FileKeyReliable: true, CTimeReliable: true, DirectoryMTimeReliable: true},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture rename: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected directory rename mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	dir := manifest.EntriesByID["dir_docs"]
	if dir.Loc == nil || dir.Loc.Name != "notes" {
		t.Fatalf("expected dir renamed to notes, got %#v", dir)
	}
	child := manifest.EntriesByID["doc_a"]
	if child.Tombstone != nil || child.Loc == nil || child.Loc.ParentID != "dir_docs" || child.Loc.Name != "a.md" {
		t.Fatalf("child identity should stay under renamed dir, got %#v", child)
	}
}
