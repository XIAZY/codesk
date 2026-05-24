package syncer

import (
	"context"
	"errors"
	"net/http"
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

func TestStreamSenderQueuesDependentStreamsAfterAck(t *testing.T) {
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
	if _, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:             "doc",
		KindHint:             "content",
		MutationKey:          "content:init:doc",
		UpdateBytes:          []byte("content"),
		DependsOnStreamID:    "root",
		DependsOnMutationKey: "root:create:doc",
	}); err != nil {
		t.Fatalf("content outbox: %v", err)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, root.ID, time.Now()); err != nil {
		t.Fatalf("mark root local: %v", err)
	}

	acked := []string{}
	if err := (&StreamSender{
		State:     state,
		Transport: &recordingStreamTransport{},
		OnAck: func(streamID string) {
			acked = append(acked, streamID)
		},
	}).SendPending(ctx); err != nil {
		t.Fatalf("send pending: %v", err)
	}
	if len(acked) != 2 || acked[0] != "root" || acked[1] != "doc" {
		t.Fatalf("expected root ack to queue dependent content stream, got %#v", acked)
	}
}

func TestStreamSenderDropsUnreferencedContentRowAndContinues(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	oldRow, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "doc_old",
		KindHint:    "content",
		MutationKey: "content:edit:doc_old:dirty",
		UpdateBytes: []byte("old"),
	})
	if err != nil {
		t.Fatalf("old content outbox: %v", err)
	}
	newRow, err := state.UpsertOutbox(ctx, StreamMutation{
		StreamID:    "doc_new",
		KindHint:    "content",
		MutationKey: "content:edit:doc_new:dirty",
		UpdateBytes: []byte("new"),
	})
	if err != nil {
		t.Fatalf("new content outbox: %v", err)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, oldRow.ID, time.Now()); err != nil {
		t.Fatalf("mark old local: %v", err)
	}
	if err := state.MarkOutboxLocallyApplied(ctx, newRow.ID, time.Now()); err != nil {
		t.Fatalf("mark new local: %v", err)
	}

	transport := &recordingStreamTransport{
		errByKey: map[string]error{
			oldRow.MutationKey: &backendStatusError{
				Method:     http.MethodPost,
				URL:        "http://backend/api/streams/doc_old/updates",
				StatusCode: http.StatusBadRequest,
				Body:       `{"error":"stream \"doc_old\" is not referenced by root manifest"}`,
			},
		},
	}
	if err := (&StreamSender{
		State:     state,
		Transport: transport,
	}).SendPending(ctx); err != nil {
		t.Fatalf("send pending: %v", err)
	}
	if got := transport.sentKeys(); len(got) != 2 || got[0] != oldRow.MutationKey || got[1] != newRow.MutationKey {
		t.Fatalf("expected sender to drop old row then send new row, got %#v", got)
	}
	var oldDropped, oldAcked, newDropped, newAcked string
	if err := state.DB().QueryRow(`
		SELECT COALESCE(dropped_at, ''), COALESCE(acked_at, '')
		  FROM stream_outbox WHERE id = ?`, oldRow.ID).Scan(&oldDropped, &oldAcked); err != nil {
		t.Fatalf("read old row: %v", err)
	}
	if err := state.DB().QueryRow(`
		SELECT COALESCE(dropped_at, ''), COALESCE(acked_at, '')
		  FROM stream_outbox WHERE id = ?`, newRow.ID).Scan(&newDropped, &newAcked); err != nil {
		t.Fatalf("read new row: %v", err)
	}
	if oldDropped == "" || oldAcked != "" {
		t.Fatalf("expected old row dropped but not acked, dropped=%q acked=%q", oldDropped, oldAcked)
	}
	if newDropped != "" || newAcked == "" {
		t.Fatalf("expected new row acked and not dropped, dropped=%q acked=%q", newDropped, newAcked)
	}
}

type recordingStreamTransport struct {
	rows     []StreamOutboxRow
	err      error
	errByKey map[string]error
}

func (t *recordingStreamTransport) PostStreamUpdate(ctx context.Context, row StreamOutboxRow) (StreamAck, error) {
	_ = ctx
	t.rows = append(t.rows, row)
	if t.errByKey != nil {
		if err := t.errByKey[row.MutationKey]; err != nil {
			return StreamAck{}, err
		}
	}
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
