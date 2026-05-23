package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
