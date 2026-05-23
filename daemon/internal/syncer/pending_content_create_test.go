package syncer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	crdt "notty/internal/ycrdt"
)

func TestPendingContentCreateProcessorCreatesDependentContentInit(t *testing.T) {
	withPendingCreateStabilityDelay(t, 0)
	ctx := context.Background()
	root := t.TempDir()
	writePendingCreateFile(t, root, "docs/a.md", "alpha")
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	rootOutbox, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc_a",
		UpdateBytes: []byte("root-update"),
		ActorID:     "daemon_1",
		ActorType:   "daemon",
		Reason:      "local-create",
	})
	if err != nil {
		t.Fatalf("root outbox: %v", err)
	}
	stat, err := fs.Stat(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("stat pending file: %v", err)
	}
	caps := ScanCapabilities{FileKeyReliable: true, CTimeReliable: true}
	if err := state.UpsertPendingContentCreate(ctx, PendingContentCreate{
		EntryID:          "doc_a",
		ContentStreamID:  "doc_a",
		MaterializedPath: "docs/a.md",
		RootMutationKey:  "root:create:doc_a",
		ObservedStat:     stat,
	}, caps); err != nil {
		t.Fatalf("upsert pending create: %v", err)
	}

	more, err := (PendingContentCreateProcessor{
		State:        state,
		FS:           fs,
		Capabilities: caps,
		ActorID:      "daemon_1",
		ActorType:    "daemon",
	}).Process(ctx, PendingCreateLimits{MaxRows: 1, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatalf("process pending create: %v", err)
	}
	if more {
		t.Fatal("did not expect more pending creates")
	}

	var status string
	var outboxID sql.NullInt64
	if err := state.DB().QueryRow(`SELECT status, content_outbox_id FROM pending_content_creates WHERE entry_id = 'doc_a'`).Scan(&status, &outboxID); err != nil {
		t.Fatalf("query pending create: %v", err)
	}
	if status != "outbox_created" || !outboxID.Valid {
		t.Fatalf("expected content outbox created, status=%q outbox=%#v", status, outboxID)
	}
	ready, err := state.ReadyLocalOutbox(ctx, "doc_a", 10)
	if err != nil {
		t.Fatalf("ready content before root ack: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("content init should wait for root ack before local apply, got %#v", ready)
	}
	if err := state.MarkOutboxAcked(ctx, rootOutbox.ID, 1, time.Now()); err != nil {
		t.Fatalf("ack root outbox: %v", err)
	}
	ready, err = state.ReadyLocalOutbox(ctx, "doc_a", 10)
	if err != nil {
		t.Fatalf("ready content after root ack: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != outboxID.Int64 {
		t.Fatalf("expected content init ready after root ack, got %#v", ready)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, ready[0].UpdateBytes, "test"); err != nil {
		t.Fatalf("apply content init: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("unexpected content init text %q", got)
	}
}

func TestPendingContentCreateProcessorHonorsRowBudget(t *testing.T) {
	withPendingCreateStabilityDelay(t, 0)
	ctx := context.Background()
	root := t.TempDir()
	writePendingCreateFile(t, root, "a.md", "alpha")
	writePendingCreateFile(t, root, "b.md", "bravo")
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	for _, id := range []string{"a", "b"} {
		if _, err := state.UpsertOutbox(ctx, StreamMutation{
			StreamID:    "root",
			KindHint:    "root",
			MutationKey: "root:create:" + id,
			UpdateBytes: []byte("root-" + id),
		}); err != nil {
			t.Fatalf("root outbox %s: %v", id, err)
		}
		stat, err := fs.Stat(ctx, id+".md")
		if err != nil {
			t.Fatalf("stat %s: %v", id, err)
		}
		if err := state.UpsertPendingContentCreate(ctx, PendingContentCreate{
			EntryID:          id,
			ContentStreamID:  id,
			MaterializedPath: id + ".md",
			RootMutationKey:  "root:create:" + id,
			ObservedStat:     stat,
		}, ScanCapabilities{FileKeyReliable: true}); err != nil {
			t.Fatalf("pending %s: %v", id, err)
		}
	}
	more, err := (PendingContentCreateProcessor{
		State:        state,
		FS:           fs,
		Capabilities: ScanCapabilities{FileKeyReliable: true},
	}).Process(ctx, PendingCreateLimits{MaxRows: 1, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatalf("process pending creates: %v", err)
	}
	if !more {
		t.Fatal("expected more pending rows after one-row budget")
	}
	var needsBytes int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM pending_content_creates WHERE status = 'needs_bytes'`).Scan(&needsBytes); err != nil {
		t.Fatalf("query needs_bytes: %v", err)
	}
	if needsBytes != 1 {
		t.Fatalf("expected one row left as needs_bytes, got %d", needsBytes)
	}
}

func TestPendingContentCreateProcessorReadsLatestStableBytesAfterDiscoveryStatChanges(t *testing.T) {
	withPendingCreateStabilityDelay(t, 0)
	ctx := context.Background()
	root := t.TempDir()
	writePendingCreateFile(t, root, "doc.md", "alpha")
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	if _, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc",
		UpdateBytes: []byte("root-doc"),
	}); err != nil {
		t.Fatalf("root outbox: %v", err)
	}
	stat, err := fs.Stat(ctx, "doc.md")
	if err != nil {
		t.Fatalf("stat doc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("beta"), 0o644); err != nil {
		t.Fatalf("rewrite doc: %v", err)
	}
	if err := state.UpsertPendingContentCreate(ctx, PendingContentCreate{
		EntryID:          "doc",
		ContentStreamID:  "doc",
		MaterializedPath: "doc.md",
		RootMutationKey:  "root:create:doc",
		ObservedStat:     stat,
	}, ScanCapabilities{FileKeyReliable: true}); err != nil {
		t.Fatalf("pending doc: %v", err)
	}
	if _, err := (PendingContentCreateProcessor{
		State:        state,
		FS:           fs,
		Capabilities: ScanCapabilities{FileKeyReliable: true},
	}).Process(ctx, PendingCreateLimits{MaxRows: 1, MaxBytes: 64 << 20}); err != nil {
		t.Fatalf("process pending create: %v", err)
	}
	ready, err := state.ReadyLocalOutbox(ctx, "doc", 10)
	if err != nil {
		t.Fatalf("ready content: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("content should still wait for root ack, got %#v", ready)
	}
	var contentUpdate []byte
	if err := state.DB().QueryRow(`SELECT update_bytes FROM stream_outbox WHERE stream_id = 'doc' AND mutation_key = 'content:init:doc'`).Scan(&contentUpdate); err != nil {
		t.Fatalf("read content outbox: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, contentUpdate, "test"); err != nil {
		t.Fatalf("apply content init: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "beta" {
		t.Fatalf("expected latest stable content, got %q", got)
	}
}

func TestPendingContentCreateProcessorCancelsChangedPath(t *testing.T) {
	withPendingCreateStabilityDelay(t, 0)
	ctx := context.Background()
	root := t.TempDir()
	writePendingCreateFile(t, root, "doc.md", "alpha")
	fs := NewWorkspaceFS(root)
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	if _, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc",
		UpdateBytes: []byte("root-doc"),
	}); err != nil {
		t.Fatalf("root outbox: %v", err)
	}
	stat, err := fs.Stat(ctx, "doc.md")
	if err != nil {
		t.Fatalf("stat doc: %v", err)
	}
	if err := state.UpsertPendingContentCreate(ctx, PendingContentCreate{
		EntryID:          "doc",
		ContentStreamID:  "doc",
		MaterializedPath: "doc.md",
		RootMutationKey:  "root:create:doc",
		ObservedStat:     stat,
	}, ScanCapabilities{FileKeyReliable: true}); err != nil {
		t.Fatalf("pending doc: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "doc.md")); err != nil {
		t.Fatalf("remove doc: %v", err)
	}
	if _, err := (PendingContentCreateProcessor{
		State:        state,
		FS:           fs,
		Capabilities: ScanCapabilities{FileKeyReliable: true},
	}).Process(ctx, PendingCreateLimits{MaxRows: 1, MaxBytes: 64 << 20}); err != nil {
		t.Fatalf("process removed pending create: %v", err)
	}
	var status string
	if err := state.DB().QueryRow(`SELECT status FROM pending_content_creates WHERE entry_id = 'doc'`).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("expected cancelled pending create, got %q", status)
	}
	var hints int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM scan_hints WHERE kind = 'path' AND path = 'doc.md'`).Scan(&hints); err != nil {
		t.Fatalf("query scan hints: %v", err)
	}
	if hints != 1 {
		t.Fatalf("expected path scan hint after cancellation, got %d", hints)
	}
}

func withPendingCreateStabilityDelay(t *testing.T, delay time.Duration) {
	t.Helper()
	previous := pendingContentCreateStabilityDelay
	pendingContentCreateStabilityDelay = delay
	t.Cleanup(func() {
		pendingContentCreateStabilityDelay = previous
	})
}

func writePendingCreateFile(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
