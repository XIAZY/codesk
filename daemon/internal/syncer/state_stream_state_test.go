package syncer

import (
	"context"
	"errors"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestApplyStreamQueueAtomicallyRollsBackOutboxMarkersWithState(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	local := contentDoc(t, "doc_a", "local")
	defer local.Close()
	outbox, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "doc_a",
		KindHint:    "content",
		MutationKey: "content:edit:doc_a:local",
		UpdateBytes: local.EncodeStateAsUpdate(),
		ActorID:     "daemon",
		ActorType:   "daemon",
		Reason:      "test",
	})
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	doc := crdt.New(crdt.WithGUID("doc_a"))
	defer doc.Close()
	_, err = state.applyStreamQueueAtomically(ctx, "doc_a", "content", doc, contentSHA256([]byte("local")), streamQueueApplyOptions{
		BeforeCommit: func() error { return errors.New("forced crash point") },
	})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	assertOutboxLocalApplied(t, state, outbox.ID, false)
	assertStreamHasNoLatestState(t, state, "doc_a")

	retryDoc := crdt.New(crdt.WithGUID("doc_a"))
	defer retryDoc.Close()
	result, err := state.ApplyStreamQueueAtomically(ctx, "doc_a", "content", retryDoc, contentSHA256([]byte("local")))
	if err != nil {
		t.Fatalf("retry atomic apply: %v", err)
	}
	if result.StateID <= 0 || len(result.LocalOutbox) != 1 || result.LocalOutbox[0].ID != outbox.ID {
		t.Fatalf("expected outbox row applied and state persisted, got %#v", result)
	}
	assertOutboxLocalApplied(t, state, outbox.ID, true)
	loaded, stream, err := state.LoadLatestStreamDoc(ctx, "doc_a", "content")
	if err != nil {
		t.Fatalf("load latest: %v", err)
	}
	defer loaded.Close()
	if !stream.LatestStateID.Valid || loaded.GetText("content").ToString() != "local" {
		t.Fatalf("expected durable local content, stream=%#v text=%q", stream, loaded.GetText("content").ToString())
	}
}

func TestApplyStreamQueueAtomicallyRollsBackInboxMarkersWithState(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	remote := contentDoc(t, "doc_a", "remote")
	defer remote.Close()
	inbox, inserted, err := state.InsertInboxUpdate(ctx, "doc_a", remote.EncodeStateAsUpdate(), 7)
	if err != nil {
		t.Fatalf("seed inbox: %v", err)
	}
	if !inserted {
		t.Fatal("expected inbox insert")
	}

	doc := crdt.New(crdt.WithGUID("doc_a"))
	defer doc.Close()
	_, err = state.applyStreamQueueAtomically(ctx, "doc_a", "content", doc, contentSHA256([]byte("remote")), streamQueueApplyOptions{
		BeforeCommit: func() error { return errors.New("forced crash point") },
	})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	assertInboxApplied(t, state, inbox.ID, false)
	assertStreamHasNoLatestState(t, state, "doc_a")

	retryDoc := crdt.New(crdt.WithGUID("doc_a"))
	defer retryDoc.Close()
	result, err := state.ApplyStreamQueueAtomically(ctx, "doc_a", "content", retryDoc, contentSHA256([]byte("remote")))
	if err != nil {
		t.Fatalf("retry atomic apply: %v", err)
	}
	if result.StateID <= 0 || len(result.Inbox) != 1 || result.Inbox[0].ID != inbox.ID {
		t.Fatalf("expected inbox row applied and state persisted, got %#v", result)
	}
	assertInboxApplied(t, state, inbox.ID, true)
	loaded, stream, err := state.LoadLatestStreamDoc(ctx, "doc_a", "content")
	if err != nil {
		t.Fatalf("load latest: %v", err)
	}
	defer loaded.Close()
	if !stream.LatestStateID.Valid || loaded.GetText("content").ToString() != "remote" {
		t.Fatalf("expected durable remote content, stream=%#v text=%q", stream, loaded.GetText("content").ToString())
	}
}

func assertOutboxLocalApplied(t *testing.T, state *WorkspaceStateDB, outboxID int64, want bool) {
	t.Helper()
	var applied string
	err := state.DB().QueryRow(`SELECT COALESCE(local_applied_at, '') FROM stream_outbox WHERE id = ?`, outboxID).Scan(&applied)
	if err != nil {
		t.Fatalf("read outbox applied marker: %v", err)
	}
	if (applied != "") != want {
		t.Fatalf("outbox applied=%v, want %v", applied != "", want)
	}
}

func assertInboxApplied(t *testing.T, state *WorkspaceStateDB, inboxID int64, want bool) {
	t.Helper()
	var applied string
	err := state.DB().QueryRow(`SELECT COALESCE(applied_at, '') FROM stream_inbox WHERE id = ?`, inboxID).Scan(&applied)
	if err != nil {
		t.Fatalf("read inbox applied marker: %v", err)
	}
	if (applied != "") != want {
		t.Fatalf("inbox applied=%v, want %v", applied != "", want)
	}
}

func assertStreamHasNoLatestState(t *testing.T, state *WorkspaceStateDB, streamID string) {
	t.Helper()
	stream, err := state.GetStream(context.Background(), streamID)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if stream.LatestStateID.Valid {
		t.Fatalf("expected no latest stream state, got %#v", stream)
	}
	var states int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM stream_states WHERE stream_id = ?`, streamID).Scan(&states); err != nil {
		t.Fatalf("count stream states: %v", err)
	}
	if states != 0 {
		t.Fatalf("expected no stream state rows, got %d", states)
	}
}
