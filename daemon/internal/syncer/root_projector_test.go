package syncer

import (
	"context"
	"database/sql"
	"errors"
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

func TestRootManifestProjectorDoesNotTombstoneTrackedEntryWithLocalContentOutbox(t *testing.T) {
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
	if _, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "doc_a",
		KindHint:    "content",
		MutationKey: "content:edit:doc_a:test",
		UpdateBytes: BuildInitialContentUpdate([]byte("alpha\nlocal\n")),
		Reason:      "content-local-edit",
	}); err != nil {
		t.Fatalf("seed content outbox: %v", err)
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
		t.Fatalf("local content outbox should block root tombstone, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_a"].Tombstone != nil {
		t.Fatalf("entry with local content outbox was tombstoned: %#v", manifest.EntriesByID["doc_a"])
	}
	if len(queued) != 1 || queued[0] != "doc_a" {
		t.Fatalf("expected content stream requeued, got %#v", queued)
	}
}

func TestRootManifestProjectorDoesNotTombstoneTrackedEntryWithUnprojectedContentStream(t *testing.T) {
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
	remote := contentDoc(t, "doc_a", "alpha\nremote\n")
	defer remote.Close()
	if _, err := state.persistLatestStreamDocFixture(ctx, "doc_a", remote, contentSHA256([]byte("alpha\nremote\n"))); err != nil {
		t.Fatalf("persist remote content: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_a",
		EntryID:          "doc_a",
		MaterializedPath: "a.md",
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_duplicate",
		EntryID:          "doc_duplicate",
		MaterializedPath: "a.md",
	}); err != nil {
		t.Fatalf("seed duplicate content projection: %v", err)
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
		t.Fatalf("unprojected content stream should block root tombstone, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_a"].Tombstone != nil {
		t.Fatalf("entry with unprojected content was tombstoned: %#v", manifest.EntriesByID["doc_a"])
	}
	if len(queued) != 1 || queued[0] != "doc_a" {
		t.Fatalf("expected content stream requeued, got %#v", queued)
	}
	projection, err := state.GetContentProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("load content projection: %v", err)
	}
	if projection == nil || projection.MaterializedPath != "a.md" {
		t.Fatalf("expected content projection path restored, got %#v", projection)
	}
}

func TestRootManifestProjectorAgentCopyModeDoesNotTombstoneMissingTrackedEntry(t *testing.T) {
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

	queued := []string{}
	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "agent_1",
		ActorType:    "agent",
		Mode:         ProjectionAgentCopy,
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("agent copy projection should not tombstone missing tracked file, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_a"].Tombstone != nil {
		t.Fatalf("agent copy projection tombstoned tracked file: %#v", manifest.EntriesByID["doc_a"])
	}
	if len(queued) != 1 || queued[0] != "doc_a" {
		t.Fatalf("expected content stream requeued, got %#v", queued)
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

func TestRootManifestProjectorTombstonesLocallyDeletedDirectoryTree(t *testing.T) {
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
		t.Fatalf("stat dir: %v", err)
	}
	fileStat, err := fs.Stat(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("stat file: %v", err)
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
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatalf("remove docs: %v", err)
	}

	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           fs,
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected one root mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["dir_docs"].Tombstone == nil {
		t.Fatalf("expected deleted directory tombstoned: %#v", manifest.EntriesByID["dir_docs"])
	}
	if manifest.EntriesByID["doc_a"].Tombstone == nil {
		t.Fatalf("expected deleted child tombstoned: %#v", manifest.EntriesByID["doc_a"])
	}
	var mkdirJobs int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM fs_jobs WHERE kind = 'mkdir' AND entry_id = 'dir_docs'`).Scan(&mkdirJobs); err != nil {
		t.Fatalf("count mkdir jobs: %v", err)
	}
	if mkdirJobs != 0 {
		t.Fatalf("local directory delete should not schedule mkdir retry, got %d", mkdirJobs)
	}
}

func TestRootManifestProjectorDoesNotTombstoneUntrackedRemoteCreate(t *testing.T) {
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
			ID:              "doc_remote",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "remote.md"),
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
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("untracked remote create should not tombstone, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.EntriesByID["doc_remote"].Tombstone != nil {
		t.Fatalf("untracked remote create was tombstoned: %#v", manifest.EntriesByID["doc_remote"])
	}
	if len(queued) != 1 || queued[0] != "doc_remote" {
		t.Fatalf("expected content stream requeued, got %#v", queued)
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

func TestRootManifestProjectorClaimsUntrackedRemoteFileWhenBytesMatchLatestStream(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("remote\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	remote := contentDoc(t, "doc_remote", "remote\n")
	defer remote.Close()
	if _, err := state.persistLatestStreamDocFixture(ctx, "doc_remote", remote, contentSHA256([]byte("remote\n"))); err != nil {
		t.Fatalf("persist remote stream: %v", err)
	}
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
	if len(mutations) != 0 {
		t.Fatalf("matching remote projection should not create local duplicate, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, ok := manifest.EntriesByID["doc_local"]; ok {
		t.Fatalf("unexpected local duplicate entry in manifest: %#v", manifest.EntriesByID["doc_local"])
	}
	if len(queued) != 1 || queued[0] != "doc_remote" {
		t.Fatalf("expected remote content stream requeued, got %#v", queued)
	}
	projection, err := state.LoadManifestProjection(ctx)
	if err != nil {
		t.Fatalf("load manifest projection: %v", err)
	}
	row := projection["doc_remote"]
	if row.EntryID != "doc_remote" || row.MaterializedPath != "README.md" || row.PendingCreate {
		t.Fatalf("expected remote path claimed cleanly, got %#v", row)
	}
	var pendingCount int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM pending_content_creates`).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending content creates: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("expected no pending local create, got %d", pendingCount)
	}
}

func TestRootManifestProjectorClaimsUntrackedCleanProjectionWhenLocalBytesDiffer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("remote\nlocal edit\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	remote := contentDoc(t, "doc_remote", "remote\n")
	defer remote.Close()
	stateID, err := state.persistLatestStreamDocFixture(ctx, "doc_remote", remote, contentSHA256([]byte("remote\n")))
	if err != nil {
		t.Fatalf("persist remote stream: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_remote",
		EntryID:          "doc_remote",
		MaterializedPath: "README.md",
		ProjectedStateID: sql.NullInt64{Int64: stateID, Valid: true},
		ProjectedHash:    contentSHA256([]byte("remote\n")),
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
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
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("clean projected local edit should not create duplicate root mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, ok := manifest.EntriesByID["doc_local"]; ok {
		t.Fatalf("unexpected local duplicate entry in manifest: %#v", manifest.EntriesByID["doc_local"])
	}
	projection, err := state.LoadManifestProjection(ctx)
	if err != nil {
		t.Fatalf("load manifest projection: %v", err)
	}
	row := projection["doc_remote"]
	if row.EntryID != "doc_remote" || row.MaterializedPath != "README.md" || row.PendingCreate {
		t.Fatalf("expected remote path claimed as edited projection, got %#v", row)
	}
}

func TestRootManifestProjectorDoesNotCreateDuplicateForManifestPathWithKnownStream(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("remote\nlocal edit\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	remote := contentDoc(t, "doc_remote", "remote\n")
	defer remote.Close()
	if _, err := state.persistLatestStreamDocFixture(ctx, "doc_remote", remote, contentSHA256([]byte("remote\n"))); err != nil {
		t.Fatalf("persist remote stream: %v", err)
	}
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
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("known manifest stream should block local duplicate, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, ok := manifest.EntriesByID["doc_local"]; ok {
		t.Fatalf("unexpected local duplicate entry in manifest: %#v", manifest.EntriesByID["doc_local"])
	}
}

func TestRootManifestProjectorDoesNotCreateDuplicateForContentProjectedPathBeforeRootArrives(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("remote\nlocal edit\n"), 0o644); err != nil {
		t.Fatalf("write projected file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	remote := contentDoc(t, "doc_remote", "remote\n")
	defer remote.Close()
	stateID, err := state.persistLatestStreamDocFixture(ctx, "doc_remote", remote, contentSHA256([]byte("remote\n")))
	if err != nil {
		t.Fatalf("persist remote stream: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_remote",
		EntryID:          "doc_remote",
		MaterializedPath: "README.md",
		ProjectedStateID: sql.NullInt64{Int64: stateID, Valid: true},
		ProjectedHash:    contentSHA256([]byte("remote\n")),
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()

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
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("content-projected path should not create duplicate root mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, ok := manifest.EntriesByID["doc_local"]; ok {
		t.Fatalf("unexpected local duplicate entry in manifest: %#v", manifest.EntriesByID["doc_local"])
	}
	var pendingCount int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM pending_content_creates`).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending creates: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("expected no pending local create, got %d", pendingCount)
	}
}

func TestRootManifestProjectorAllowsReplacementForTombstonedContentProjectedPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("local replacement\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_deleted",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_deleted",
		DesiredPath:          "README.md",
		MaterializedPath:     "README.md",
		RootProjectedStateID: 1,
		Tombstoned:           true,
	}); err != nil {
		t.Fatalf("seed tombstoned manifest projection: %v", err)
	}
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_deleted",
		EntryID:          "doc_deleted",
		MaterializedPath: "README.md",
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()

	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		NewID: func(kind string, relPath string) string {
			if kind == "file" && relPath == "README.md" {
				return "doc_replacement"
			}
			return kind + "_unexpected"
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("expected replacement root mutation, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if entry := manifest.EntriesByID["doc_replacement"]; entry.ID != "doc_replacement" || entry.Loc == nil || entry.Loc.Name != "README.md" {
		t.Fatalf("expected replacement entry, got %#v", entry)
	}
	var pendingPath string
	if err := state.DB().QueryRow(`SELECT materialized_path FROM pending_content_creates WHERE entry_id = 'doc_replacement'`).Scan(&pendingPath); err != nil {
		t.Fatalf("pending replacement create missing: %v", err)
	}
	if pendingPath != "README.md" {
		t.Fatalf("unexpected pending replacement path %q", pendingPath)
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
	fs := NewWorkspaceFS(root)
	if err := (RootManifestProjector{State: state, FS: fs, RootStreamID: "root-stream"}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan rename: %v", err)
	}
	var kind, source, target string
	if err := state.DB().QueryRow(`SELECT kind, source_path, target_path FROM fs_jobs WHERE entry_id = 'doc_a'`).Scan(&kind, &source, &target); err != nil {
		t.Fatalf("read fs job: %v", err)
	}
	if kind != "move-entry" || source != "old.md" || target != "new.md" {
		t.Fatalf("unexpected move job kind=%q source=%q target=%q", kind, source, target)
	}
	contentProjection, err := state.GetContentProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("read content projection: %v", err)
	}
	if contentProjection == nil || contentProjection.MaterializedPath != "old.md" || !contentProjection.ProjectedStateID.Valid || contentProjection.ProjectedStateID.Int64 != 1 || contentProjection.ProjectedHash != contentSHA256([]byte("alpha")) {
		t.Fatalf("pending rename should preserve projected content base at old path, got %#v", contentProjection)
	}
	manifestProjection, err := state.GetManifestProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("read manifest projection: %v", err)
	}
	if manifestProjection == nil || manifestProjection.DesiredPath != "new.md" || manifestProjection.MaterializedPath != "old.md" {
		t.Fatalf("pending rename should keep old materialized path while recording desired path, got %#v", manifestProjection)
	}
	if err := state.RunPendingFSJobs(ctx, fs); err != nil {
		t.Fatalf("run move job: %v", err)
	}
	contentProjection, err = state.GetContentProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("read content projection after move: %v", err)
	}
	if contentProjection == nil || contentProjection.MaterializedPath != "new.md" {
		t.Fatalf("completed rename should advance content projection path, got %#v", contentProjection)
	}
	manifestProjection, err = state.GetManifestProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("read manifest projection after move: %v", err)
	}
	if manifestProjection == nil || manifestProjection.MaterializedPath != "new.md" {
		t.Fatalf("completed rename should advance manifest projection path, got %#v", manifestProjection)
	}
	if _, err := os.Stat(filepath.Join(root, "old.md")); !os.IsNotExist(err) {
		t.Fatalf("expected old path moved away, stat err=%v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "new.md")); err != nil || string(content) != "alpha" {
		t.Fatalf("expected new path content, content=%q err=%v", string(content), err)
	}
}

func TestRootManifestProjectorOrdersMoveJobsToFreeRenameConflictTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "source.md",
		MaterializedPath:     "source.md",
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed remote projection: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_z",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_z",
		DesiredPath:          "target.md",
		MaterializedPath:     "target.md",
		RootProjectedStateID: 1,
		PendingCreate:        true,
	}); err != nil {
		t.Fatalf("seed local projection: %v", err)
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "target.md"),
			ContentStreamID: "doc_a",
		},
	}, {
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_z",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "target.md"),
			ContentStreamID: "doc_z",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := (RootManifestProjector{State: state, FS: NewWorkspaceFS(root), RootStreamID: "root-stream"}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan rename conflict: %v", err)
	}
	rows, err := state.DB().Query(`SELECT source_path, target_path FROM fs_jobs WHERE kind = 'move-entry' ORDER BY id`)
	if err != nil {
		t.Fatalf("query move jobs: %v", err)
	}
	defer rows.Close()
	got := [][2]string{}
	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			t.Fatalf("scan move job: %v", err)
		}
		got = append(got, [2]string{source, target})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("move rows: %v", err)
	}
	localConflict := rootmanifest.ConflictPath("target.md", "doc_z")
	want := [][2]string{{"target.md", localConflict}, {"source.md", "target.md"}}
	if len(got) != len(want) {
		t.Fatalf("unexpected move jobs %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("move job %d = %#v, want %#v (all jobs %#v)", i, got[i], want[i], got)
		}
	}
}

func TestRootManifestProjectorPlansSwapThroughTempMove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("A"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("B"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	for _, row := range []ManifestProjectionRow{{
		EntryID:              "doc_a",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a",
		DesiredPath:          "a.md",
		MaterializedPath:     "a.md",
		LastCleanHash:        contentSHA256([]byte("A")),
		RootProjectedStateID: 1,
	}, {
		EntryID:              "doc_b",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_b",
		DesiredPath:          "b.md",
		MaterializedPath:     "b.md",
		LastCleanHash:        contentSHA256([]byte("B")),
		RootProjectedStateID: 1,
	}} {
		if err := state.UpsertManifestProjection(ctx, row); err != nil {
			t.Fatalf("seed manifest projection: %v", err)
		}
		if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
			StreamID:         row.ContentStreamID,
			EntryID:          row.EntryID,
			MaterializedPath: row.MaterializedPath,
			ProjectedHash:    row.LastCleanHash,
		}); err != nil {
			t.Fatalf("seed content projection: %v", err)
		}
	}
	doc := crdt.New(crdt.WithGUID("root-stream"))
	defer doc.Close()
	if _, err := rootmanifest.ApplyIntents(doc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_a",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "b.md"),
			ContentStreamID: "doc_a",
		},
	}, {
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_b",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "a.md"),
			ContentStreamID: "doc_b",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := (RootManifestProjector{State: state, FS: fs, RootStreamID: "root-stream"}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan swap: %v", err)
	}
	rows, err := state.DB().Query(`SELECT kind, source_path, target_path FROM fs_jobs ORDER BY id`)
	if err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()
	type moveRow struct{ kind, source, target string }
	moves := []moveRow{}
	for rows.Next() {
		var row moveRow
		if err := rows.Scan(&row.kind, &row.source, &row.target); err != nil {
			t.Fatalf("scan job: %v", err)
		}
		moves = append(moves, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if len(moves) != 3 || moves[0].kind != "move-entry-temp" || moves[1].kind != "move-entry" || moves[2].kind != "move-entry" {
		t.Fatalf("expected temp move followed by two final moves, got %#v", moves)
	}
	if err := state.RunPendingFSJobs(ctx, fs); err != nil {
		t.Fatalf("run swap jobs: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "a.md")); err != nil || string(content) != "B" {
		t.Fatalf("expected b content at a.md, content=%q err=%v", string(content), err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "b.md")); err != nil || string(content) != "A" {
		t.Fatalf("expected a content at b.md, content=%q err=%v", string(content), err)
	}
	projectionA, err := state.GetManifestProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("projection a: %v", err)
	}
	projectionB, err := state.GetManifestProjection(ctx, "doc_b")
	if err != nil {
		t.Fatalf("projection b: %v", err)
	}
	if projectionA == nil || projectionA.MaterializedPath != "b.md" || projectionB == nil || projectionB.MaterializedPath != "a.md" {
		t.Fatalf("expected swapped projection paths, a=%#v b=%#v", projectionA, projectionB)
	}
}

func TestRetryableMoveJobCanBeRevivedAfterCollisionClears(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.md"), []byte("source"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "target.md"), []byte("blocking"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	job := FSJob{
		JobKey:     "root:move:retryable",
		Kind:       "move-entry",
		SourcePath: "source.md",
		TargetPath: "target.md",
	}
	if _, err := state.InsertFSJob(ctx, job); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	err = state.RunPendingFSJobs(ctx, fs)
	if !errors.Is(err, ErrPathCollision) {
		t.Fatalf("expected path collision, got %v", err)
	}
	var status string
	if err := state.DB().QueryRow(`SELECT status FROM fs_jobs WHERE job_key = ?`, job.JobKey).Scan(&status); err != nil {
		t.Fatalf("read retryable status: %v", err)
	}
	if status != "retryable" {
		t.Fatalf("expected retryable job status, got %q", status)
	}
	if err := os.Remove(filepath.Join(root, "target.md")); err != nil {
		t.Fatalf("clear target: %v", err)
	}
	revived, err := state.InsertFSJob(ctx, job)
	if err != nil {
		t.Fatalf("revive job: %v", err)
	}
	if revived.Status != "pending" {
		t.Fatalf("expected revived job pending, got %#v", revived)
	}
	if err := state.RunPendingFSJobs(ctx, fs); err != nil {
		t.Fatalf("retry job: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "target.md")); err != nil || string(content) != "source" {
		t.Fatalf("expected source moved to target, content=%q err=%v", string(content), err)
	}
	if _, err := os.Stat(filepath.Join(root, "source.md")); !os.IsNotExist(err) {
		t.Fatalf("expected source removed, stat err=%v", err)
	}
}

func TestRootMutationKeyIncludesIntentPayload(t *testing.T) {
	locB := rootMutationKey([]rootmanifest.Intent{{
		Type:    "loc",
		EntryID: "doc_a",
		Loc:     rootmanifest.NewLocation(rootmanifest.RootEntryID, "b.md"),
	}})
	locC := rootMutationKey([]rootmanifest.Intent{{
		Type:    "loc",
		EntryID: "doc_a",
		Loc:     rootmanifest.NewLocation(rootmanifest.RootEntryID, "c.md"),
	}})
	if locB == locC {
		t.Fatalf("distinct location assignments must not share a mutation key: %q", locB)
	}

	tombstoneA := rootMutationKey([]rootmanifest.Intent{{
		Type:      "tombstone",
		EntryID:   "doc_a",
		Tombstone: &rootmanifest.Tombstone{ActorID: "actor_a", ActorType: "daemon", At: "2026-05-23T00:00:00Z"},
	}})
	tombstoneB := rootMutationKey([]rootmanifest.Intent{{
		Type:      "tombstone",
		EntryID:   "doc_a",
		Tombstone: &rootmanifest.Tombstone{ActorID: "actor_b", ActorType: "daemon", At: "2026-05-23T00:00:00Z"},
	}})
	if tombstoneA == tombstoneB {
		t.Fatalf("distinct tombstone assignments must not share a mutation key: %q", tombstoneA)
	}
}

func TestRootManifestProjectorDoesNotMoveEntryToOtherConflictMaterializedPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	conflictPath := rootmanifest.ConflictPath("README.md", "doc_z_local")
	if err := os.WriteFile(filepath.Join(root, conflictPath), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("write conflict file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	conflictStat, err := fs.Stat(ctx, conflictPath)
	if err != nil {
		t.Fatalf("stat conflict file: %v", err)
	}
	if conflictStat.FileKey == "" {
		t.Skip("filesystem did not provide file key")
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
			ID:              "doc_a_remote",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "README.md"),
			ContentStreamID: "doc_a_remote",
		},
	}, {
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_z_local",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "README.md"),
			ContentStreamID: "doc_z_local",
		},
	}}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_a_remote",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_a_remote",
		DesiredPath:          "README.md",
		MaterializedPath:     "README.md",
		Stat:                 conflictStat,
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed remote projection: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:              "doc_z_local",
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      "doc_z_local",
		DesiredPath:          "README.md",
		MaterializedPath:     conflictPath,
		Stat:                 conflictStat,
		LastCleanHash:        contentSHA256([]byte("local\n")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed conflict projection: %v", err)
	}

	queued := []string{}
	mutations, err := (RootManifestProjector{
		State:        state,
		FS:           fs,
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		Capabilities: ScanCapabilities{FileKeyReliable: true},
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).CaptureLocal(ctx, doc)
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("generated conflict path should not be captured as a local move, got %#v", mutations)
	}
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if manifest.EntriesByID["doc_a_remote"].Loc == nil || manifest.EntriesByID["doc_a_remote"].Loc.Name != "README.md" {
		t.Fatalf("remote entry moved into conflict path: %#v", manifest.EntriesByID["doc_a_remote"])
	}
	if len(queued) != 1 || queued[0] != "doc_a_remote" {
		t.Fatalf("expected missing remote content stream requeued, got %#v", queued)
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
	queued := []string{}
	if err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan tombstone: %v", err)
	}
	if len(queued) != 1 || queued[0] != "root-stream" {
		t.Fatalf("dirty delete should requeue root scan for replacement capture, got %#v", queued)
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

func TestRootManifestProjectorRemoteDeletePreservesLocalOutboxCreatedAfterTombstone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("local"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
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
		LastCleanHash:        contentSHA256([]byte("local")),
		RootProjectedStateID: 1,
	}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}
	if _, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "doc_a",
		KindHint:    "content",
		MutationKey: "content:edit:doc_a:local",
		UpdateBytes: []byte("local update"),
		ActorID:     "daemon",
		ActorType:   "daemon",
		Reason:      "content-local-edit",
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
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
		Tombstone: &rootmanifest.Tombstone{ActorID: "peer", ActorType: "daemon", At: "2000-01-01T00:00:00Z"},
	}}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	queued := []string{}
	if err := (RootManifestProjector{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		Queue: func(streamID string) {
			queued = append(queued, streamID)
		},
	}).PlanApplyMerged(ctx, doc, 2); err != nil {
		t.Fatalf("plan tombstone: %v", err)
	}
	if len(queued) != 1 || queued[0] != "root-stream" {
		t.Fatalf("local outbox delete should requeue root scan for replacement capture, got %#v", queued)
	}
	var jobs int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM fs_jobs WHERE kind = 'delete-clean-entry'`).Scan(&jobs); err != nil {
		t.Fatalf("count delete jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("locally edited remote delete must not schedule delete job, got %d", jobs)
	}
	content, err := os.ReadFile(filepath.Join(root, "a.md"))
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if string(content) != "local" {
		t.Fatalf("local file changed to %q", string(content))
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
