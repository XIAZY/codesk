package syncer

import (
	"context"
	"errors"
	"sync"
	"time"
)

type StreamAck struct {
	UpdateID int64
}

type StreamTransport interface {
	PostStreamUpdate(ctx context.Context, row StreamOutboxRow) (StreamAck, error)
}

type StreamSender struct {
	State     *WorkspaceStateDB
	Transport StreamTransport
	Now       func() time.Time
	OnAck     func(streamID string)

	mu sync.Mutex
}

func (s *StreamSender) SendPending(ctx context.Context) error {
	if s == nil || s.State == nil {
		return errors.New("state db is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Transport == nil {
		return errors.New("stream transport is required")
	}
	for {
		row, err := s.State.NextSendableOutboxRow(ctx)
		if err != nil || row == nil {
			return err
		}
		now := s.now()
		if err := s.State.MarkOutboxSent(ctx, row.ID, now); err != nil {
			return err
		}
		ack, err := s.Transport.PostStreamUpdate(ctx, *row)
		if err != nil {
			return err
		}
		if err := s.State.MarkOutboxAcked(ctx, row.ID, ack.UpdateID, s.now()); err != nil {
			return err
		}
		if s.OnAck != nil {
			s.OnAck(row.StreamID)
		}
	}
}

func (s *StreamSender) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
