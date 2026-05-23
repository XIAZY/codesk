package syncer

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestStreamOutboxDependenciesGateLocalApplyAndSend(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	root, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc_1",
		UpdateBytes: []byte("root-update"),
		ActorID:     "daemon_1",
		ActorType:   "daemon",
		Reason:      "local-create",
	})
	if err != nil {
		t.Fatalf("upsert root outbox: %v", err)
	}
	reused, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc_1",
		UpdateBytes: []byte("root-update"),
		ActorID:     "daemon_1",
		ActorType:   "daemon",
		Reason:      "local-create",
	})
	if err != nil {
		t.Fatalf("reuse root outbox: %v", err)
	}
	if reused.ID != root.ID {
		t.Fatalf("expected idempotent upsert to reuse row %d, got %d", root.ID, reused.ID)
	}

	content, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:             "doc_1",
		KindHint:             "content",
		MutationKey:          "content:init:doc_1",
		UpdateBytes:          []byte("content-update"),
		ActorID:              "daemon_1",
		ActorType:            "daemon",
		Reason:               "local-create",
		DependsOnStreamID:    "root",
		DependsOnMutationKey: "root:create:doc_1",
	})
	if err != nil {
		t.Fatalf("upsert dependent content outbox: %v", err)
	}
	if !content.DependsOnID.Valid || content.DependsOnID.Int64 != root.ID {
		t.Fatalf("expected content dependency on root row, got %#v", content.DependsOnID)
	}

	readyContent, err := state.ReadyLocalOutbox(ctx, "doc_1", 10)
	if err != nil {
		t.Fatalf("ready content before ack: %v", err)
	}
	if len(readyContent) != 0 {
		t.Fatalf("content outbox should not be locally applicable before root ack, got %#v", readyContent)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, root.ID, time.Now()); err != nil {
		t.Fatalf("mark root local: %v", err)
	}
	next, err := state.NextSendableOutboxRow(ctx)
	if err != nil {
		t.Fatalf("next sendable root: %v", err)
	}
	if next == nil || next.ID != root.ID {
		t.Fatalf("expected root row to be sendable first, got %#v", next)
	}
	if err := state.MarkOutboxAcked(ctx, root.ID, 42, time.Now()); err != nil {
		t.Fatalf("ack root: %v", err)
	}

	readyContent, err = state.ReadyLocalOutbox(ctx, "doc_1", 10)
	if err != nil {
		t.Fatalf("ready content after ack: %v", err)
	}
	if len(readyContent) != 1 || readyContent[0].ID != content.ID {
		t.Fatalf("expected content row locally applicable after root ack, got %#v", readyContent)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, content.ID, time.Now()); err != nil {
		t.Fatalf("mark content local: %v", err)
	}
	next, err = state.NextSendableOutboxRow(ctx)
	if err != nil {
		t.Fatalf("next sendable content: %v", err)
	}
	if next == nil || next.ID != content.ID {
		t.Fatalf("expected content row sendable after local apply and root ack, got %#v", next)
	}
}

func TestStreamOutboxAllowsReencodedLogicalMutationAfterLocalApply(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	row, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc_1",
		UpdateBytes: []byte("first-encoding"),
		ActorID:     "daemon",
		ActorType:   "daemon",
		Reason:      "local-create",
	})
	if err != nil {
		t.Fatalf("insert outbox: %v", err)
	}
	replaced, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc_1",
		UpdateBytes: []byte("second-encoding"),
		ActorID:     "daemon",
		ActorType:   "daemon",
		Reason:      "local-create",
	})
	if err != nil {
		t.Fatalf("replace unapplied outbox: %v", err)
	}
	if replaced.ID != row.ID || !bytes.Equal(replaced.UpdateBytes, []byte("second-encoding")) {
		t.Fatalf("expected unapplied row replaced in place, got %#v", replaced)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, row.ID, time.Now()); err != nil {
		t.Fatalf("mark local: %v", err)
	}
	reused, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc_1",
		UpdateBytes: []byte("third-encoding"),
		ActorID:     "daemon",
		ActorType:   "daemon",
		Reason:      "local-create",
	})
	if err != nil {
		t.Fatalf("reuse applied outbox: %v", err)
	}
	if reused.ID != row.ID || !bytes.Equal(reused.UpdateBytes, []byte("second-encoding")) {
		t.Fatalf("expected applied row reused without rewrite, got %#v", reused)
	}
}

func TestStreamInboxDedupeAndAppliedState(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	update := []byte("remote-update")
	first, inserted, err := state.InsertInboxUpdate(ctx, "root", update, 10)
	if err != nil {
		t.Fatalf("insert inbox: %v", err)
	}
	if !inserted {
		t.Fatal("expected first inbox insert to create row")
	}
	second, inserted, err := state.InsertInboxUpdate(ctx, "root", append([]byte(nil), update...), 10)
	if err != nil {
		t.Fatalf("dedupe inbox: %v", err)
	}
	if inserted {
		t.Fatal("expected duplicate inbox update to be deduped")
	}
	if second.ID != first.ID || !bytes.Equal(second.UpdateBytes, update) {
		t.Fatalf("expected duplicate to return original row, got first=%#v second=%#v", first, second)
	}

	unapplied, err := state.UnappliedInbox(ctx, "root", 10)
	if err != nil {
		t.Fatalf("unapplied inbox: %v", err)
	}
	if len(unapplied) != 1 || unapplied[0].ID != first.ID {
		t.Fatalf("expected one unapplied row, got %#v", unapplied)
	}
	if err := state.MarkInboxApplied(ctx, first.ID, time.Now()); err != nil {
		t.Fatalf("mark inbox applied: %v", err)
	}
	unapplied, err = state.UnappliedInbox(ctx, "root", 10)
	if err != nil {
		t.Fatalf("unapplied inbox after mark: %v", err)
	}
	if len(unapplied) != 0 {
		t.Fatalf("expected no unapplied rows after mark, got %#v", unapplied)
	}
}

func TestOutboxDependencyMustExist(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	if _, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:             "doc_1",
		KindHint:             "content",
		MutationKey:          "content:init:doc_1",
		UpdateBytes:          []byte("content-update"),
		DependsOnStreamID:    "root",
		DependsOnMutationKey: "root:create:doc_1",
	}); err == nil {
		t.Fatal("expected missing dependency to reject outbox insertion")
	}
}
