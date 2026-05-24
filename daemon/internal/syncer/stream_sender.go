package syncer

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
			if isUnreferencedContentStreamError(err, *row) {
				if dropErr := s.State.MarkOutboxDropped(ctx, row.ID, "backend-stream-not-referenced", s.now()); dropErr != nil {
					return dropErr
				}
				continue
			}
			return err
		}
		if err := s.State.MarkOutboxAcked(ctx, row.ID, ack.UpdateID, s.now()); err != nil {
			return err
		}
		if s.OnAck != nil {
			s.OnAck(row.StreamID)
			dependents, err := s.State.DependentOutboxStreamIDs(ctx, row.ID)
			if err != nil {
				return err
			}
			for _, streamID := range dependents {
				s.OnAck(streamID)
			}
		}
	}
}

func (s *StreamSender) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func isUnreferencedContentStreamError(err error, row StreamOutboxRow) bool {
	if !isContentOutboxRow(row) {
		return false
	}
	var statusErr *backendStatusError
	if !errors.As(err, &statusErr) || statusErr == nil {
		return false
	}
	return statusErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(statusErr.Body, "not referenced by root manifest")
}

func isContentOutboxRow(row StreamOutboxRow) bool {
	kind := strings.TrimSpace(row.KindHint.String)
	if row.KindHint.Valid && strings.EqualFold(kind, "content") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(row.MutationKey), "content:")
}
