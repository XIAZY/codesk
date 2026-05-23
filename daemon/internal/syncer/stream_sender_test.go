package syncer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStreamSenderSendsOnlyLocallyAppliedDependencyReadyRows(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	root, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:create:doc",
		UpdateBytes: []byte("root"),
	})
	if err != nil {
		t.Fatalf("root outbox: %v", err)
	}
	content, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:             "doc",
		KindHint:             "content",
		MutationKey:          "content:init:doc",
		UpdateBytes:          []byte("content"),
		DependsOnStreamID:    "root",
		DependsOnMutationKey: "root:create:doc",
	})
	if err != nil {
		t.Fatalf("content outbox: %v", err)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, root.ID, time.Now()); err != nil {
		t.Fatalf("mark root local: %v", err)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, content.ID, time.Now()); err != nil {
		t.Fatalf("mark content local: %v", err)
	}

	transport := &recordingStreamTransport{}
	acked := []string{}
	sender := &StreamSender{
		State:     state,
		Transport: transport,
		OnAck: func(streamID string) {
			acked = append(acked, streamID)
		},
	}
	if err := sender.SendPending(ctx); err != nil {
		t.Fatalf("send pending: %v", err)
	}
	if got := transport.sentKeys(); len(got) != 2 || got[0] != "root:create:doc" || got[1] != "content:init:doc" {
		t.Fatalf("expected root before content send order, got %#v", got)
	}
	if len(acked) != 2 || acked[0] != "root" || acked[1] != "doc" {
		t.Fatalf("expected ack callbacks for both streams, got %#v", acked)
	}
}

func TestStreamSenderLeavesRowUnackedOnTransportError(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	row, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "root",
		KindHint:    "root",
		MutationKey: "root:update",
		UpdateBytes: []byte("root"),
	})
	if err != nil {
		t.Fatalf("root outbox: %v", err)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, row.ID, time.Now()); err != nil {
		t.Fatalf("mark local: %v", err)
	}

	transportErr := errors.New("network down")
	err = (&StreamSender{
		State:     state,
		Transport: &recordingStreamTransport{err: transportErr},
	}).SendPending(ctx)
	if !errors.Is(err, transportErr) {
		t.Fatalf("expected transport error, got %v", err)
	}
	next, err := state.NextSendableOutboxRow(ctx)
	if err != nil {
		t.Fatalf("next sendable after error: %v", err)
	}
	if next == nil || next.ID != row.ID || next.SentAt.String == "" || next.AckedAt.Valid {
		t.Fatalf("expected row sent but unacked for retry, got %#v", next)
	}
}

type recordingStreamTransport struct {
	rows []StreamOutboxRow
	err  error
}

func (t *recordingStreamTransport) PostStreamUpdate(ctx context.Context, row StreamOutboxRow) (StreamAck, error) {
	_ = ctx
	t.rows = append(t.rows, row)
	if t.err != nil {
		return StreamAck{}, t.err
	}
	return StreamAck{UpdateID: int64(len(t.rows))}, nil
}

func (t *recordingStreamTransport) sentKeys() []string {
	keys := make([]string, 0, len(t.rows))
	for _, row := range t.rows {
		keys = append(keys, row.MutationKey)
	}
	return keys
}
