package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"notty/internal/rootmanifest"
	crdt "notty/internal/ycrdt"
)

func TestWorkspaceSyncLoopLocalCreateOrdersRootBeforeContentInit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	queued := []string{}
	loop := &WorkspaceSyncLoop{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
		Capabilities: ScanCapabilities{FileKeyReliable: true, CTimeReliable: true, DirectoryMTimeReliable: true},
		NewID: func(kind string, relPath string) string {
			if kind == "file" && relPath == "a.md" {
				return "doc_a"
			}
			return "dir_unused"
		},
		Queue: func(streamID string) { queued = append(queued, streamID) },
	}
	if err := loop.ReconcileOne(ctx, "root-stream"); err != nil {
		t.Fatalf("reconcile root: %v", err)
	}
	rows, err := state.ReadyLocalOutbox(ctx, "doc_a", 10)
	if err != nil {
		t.Fatalf("ready content before root ack: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("content init must wait for root ack, got %#v", rows)
	}
	rootOutbox, err := state.NextSendableOutboxRow(ctx)
	if err != nil {
		t.Fatalf("next sendable: %v", err)
	}
	if rootOutbox == nil || rootOutbox.StreamID != "root-stream" {
		t.Fatalf("expected root outbox sendable first, got %#v", rootOutbox)
	}
	if err := state.MarkOutboxAcked(ctx, rootOutbox.ID, 1, time.Now()); err != nil {
		t.Fatalf("ack root: %v", err)
	}
	if err := loop.ReconcileOne(ctx, "doc_a"); err != nil {
		t.Fatalf("reconcile content: %v", err)
	}
	projection, err := state.GetContentProjection(ctx, "doc_a")
	if err != nil {
		t.Fatalf("get projection: %v", err)
	}
	if projection == nil || !projection.ProjectedStateID.Valid || projection.ProjectedHash != contentSHA256([]byte("alpha")) {
		t.Fatalf("expected initialized content projection, got %#v", projection)
	}
	contentOutbox, err := state.NextSendableOutboxRow(ctx)
	if err != nil {
		t.Fatalf("next content sendable: %v", err)
	}
	if contentOutbox == nil || contentOutbox.StreamID != "doc_a" {
		t.Fatalf("expected content outbox sendable after local projection, got %#v", contentOutbox)
	}
	if err := state.MarkOutboxAcked(ctx, contentOutbox.ID, 2, time.Now()); err != nil {
		t.Fatalf("ack content: %v", err)
	}
	if err := loop.ReconcileOne(ctx, "doc_a"); err != nil {
		t.Fatalf("reconcile content after ack: %v", err)
	}
	var pendingStatus string
	if err := state.DB().QueryRow(`SELECT status FROM pending_content_creates WHERE entry_id = 'doc_a'`).Scan(&pendingStatus); err != nil {
		t.Fatalf("read pending create: %v", err)
	}
	if pendingStatus != "completed" {
		t.Fatalf("expected pending create completed, got %q", pendingStatus)
	}
	if len(queued) == 0 {
		t.Fatal("expected loop to queue follow-up work")
	}
}

func TestWorkspaceSyncLoopSkipsUninitializedRemoteContentProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_remote",
		EntryID:          "doc_remote",
		MaterializedPath: "remote.md",
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
	loop := &WorkspaceSyncLoop{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
	}
	if err := loop.ReconcileOne(ctx, "doc_remote"); err != nil {
		t.Fatalf("reconcile uninitialized content: %v", err)
	}
	stream, err := state.GetStream(ctx, "doc_remote")
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	if stream.LatestStateID.Valid {
		t.Fatalf("uninitialized remote content should not persist empty state: %#v", stream)
	}
	var jobs int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM fs_jobs WHERE kind = 'write-content'`).Scan(&jobs); err != nil {
		t.Fatalf("count fs jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("uninitialized remote content should not schedule empty write, got %d jobs", jobs)
	}
	if _, err := os.Stat(filepath.Join(root, "remote.md")); !os.IsNotExist(err) {
		t.Fatalf("remote.md should not have been materialized, stat err=%v", err)
	}
}

func TestWorkspaceSyncLoopSkipsFirstEmptyRemoteContentState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := state.UpsertContentProjection(ctx, ContentProjectionRow{
		StreamID:         "doc_remote",
		EntryID:          "doc_remote",
		MaterializedPath: "remote.md",
	}); err != nil {
		t.Fatalf("seed content projection: %v", err)
	}
	empty := contentDoc(t, "doc_remote", "")
	defer empty.Close()
	if _, _, err := state.InsertInboxUpdate(ctx, "doc_remote", empty.EncodeStateAsUpdate(), 1); err != nil {
		t.Fatalf("seed empty inbox: %v", err)
	}
	loop := &WorkspaceSyncLoop{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
	}
	if err := loop.ReconcileOne(ctx, "doc_remote"); err != nil {
		t.Fatalf("reconcile empty remote content: %v", err)
	}
	stream, err := state.GetStream(ctx, "doc_remote")
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	if stream.LatestStateID.Valid {
		t.Fatalf("first empty remote content should not persist latest state: %#v", stream)
	}
	var jobs int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM fs_jobs WHERE kind = 'write-content'`).Scan(&jobs); err != nil {
		t.Fatalf("count fs jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("first empty remote content should not schedule empty write, got %d jobs", jobs)
	}
	if _, err := os.Stat(filepath.Join(root, "remote.md")); !os.IsNotExist(err) {
		t.Fatalf("remote.md should not have been materialized, stat err=%v", err)
	}
}

func TestWorkspaceSyncLoopDropsLocalOutboxForTombstonedContentStream(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	rootDoc := crdt.New(crdt.WithGUID("root-stream"))
	defer rootDoc.Close()
	if _, err := rootmanifest.ApplyIntents(rootDoc, []rootmanifest.Intent{{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              "doc_old",
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(rootmanifest.RootEntryID, "old.md"),
			ContentStreamID: "doc_old",
		},
	}, {
		Type:    "tombstone",
		EntryID: "doc_old",
		Tombstone: &rootmanifest.Tombstone{
			ActorID:   "remote",
			ActorType: "human",
			At:        time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}); err != nil {
		t.Fatalf("build root manifest: %v", err)
	}
	if err := state.EnsureLocalStream(ctx, "root-stream", "root"); err != nil {
		t.Fatalf("ensure root stream: %v", err)
	}
	rootStateID, err := state.InsertStreamState(ctx, "root-stream", rootDoc.EncodeStateAsUpdate(), crdt.EncodeStateVectorV1(rootDoc), "")
	if err != nil {
		t.Fatalf("insert root state: %v", err)
	}
	if err := state.UpdateLatestStreamState(ctx, "root-stream", rootStateID, crdt.EncodeStateVectorV1(rootDoc)); err != nil {
		t.Fatalf("update root latest: %v", err)
	}

	local := contentDoc(t, "doc_old", "dirty local")
	defer local.Close()
	outbox, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "doc_old",
		KindHint:    "content",
		MutationKey: "content:edit:doc_old:dirty",
		UpdateBytes: local.EncodeStateAsUpdate(),
	})
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	loop := &WorkspaceSyncLoop{
		State:        state,
		FS:           NewWorkspaceFS(root),
		RootStreamID: "root-stream",
		ActorID:      "daemon",
		ActorType:    "daemon",
	}
	if err := loop.ReconcileOne(ctx, "doc_old"); err != nil {
		t.Fatalf("reconcile tombstoned content: %v", err)
	}

	var localApplied, droppedAt string
	if err := state.DB().QueryRow(`
		SELECT COALESCE(local_applied_at, ''), COALESCE(dropped_at, '')
		  FROM stream_outbox WHERE id = ?`, outbox.ID).Scan(&localApplied, &droppedAt); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if localApplied != "" || droppedAt == "" {
		t.Fatalf("expected tombstoned content outbox dropped before local apply, local_applied=%q dropped=%q", localApplied, droppedAt)
	}
	stream, err := state.GetStream(ctx, "doc_old")
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	if stream.LatestStateID.Valid {
		t.Fatalf("tombstoned content local outbox should not advance stream state: %#v", stream)
	}
	var jobs int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM fs_jobs WHERE stream_id = 'doc_old'`).Scan(&jobs); err != nil {
		t.Fatalf("count fs jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("tombstoned content should not schedule fs jobs, got %d", jobs)
	}
}
