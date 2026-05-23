package syncer

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

type streamSync struct {
	cfg      Config
	state    *WorkspaceStateDB
	streamID string
	kind     string
	queue    func(string)

	mu sync.Mutex
}

var streamSyncReconnectInterval = 30 * time.Second

func newStreamSync(cfg Config, state *WorkspaceStateDB, streamID string, kind string, queue func(string)) *streamSync {
	return &streamSync{
		cfg:      cfg,
		state:    state,
		streamID: strings.TrimSpace(streamID),
		kind:     strings.TrimSpace(kind),
		queue:    queue,
	}
}

func (s *streamSync) update(streamID string, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamID = strings.TrimSpace(streamID)
	s.kind = strings.TrimSpace(kind)
}

func (s *streamSync) current() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamID, s.kind
}

func (s *streamSync) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			streamID, _ := s.current()
			log.Printf("stream sync error stream=%s err=%v", streamID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (s *streamSync) runOnce(ctx context.Context) error {
	streamID, _ := s.current()
	if streamID == "" {
		return nil
	}
	paceDocumentConnect()
	clientID := nextAwarenessClientID()
	query := url.Values{
		"client_id":  {fmt.Sprintf("%d", clientID)},
		"actor_id":   {s.cfg.AgentID},
		"actor_type": {"daemon"},
	}
	conn, _, err := dialWorkspaceWebsocket(ctx, s.cfg, "/ws/streams/"+streamID, query, "")
	if err != nil {
		return err
	}
	runCtx := ctx
	cancel := func() {}
	if streamSyncReconnectInterval > 0 {
		runCtx, cancel = context.WithTimeout(ctx, streamSyncReconnectInterval)
	}
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	reconnectClose := context.AfterFunc(runCtx, func() {
		_ = conn.Close()
	})
	defer stopClose()
	defer reconnectClose()
	defer conn.Close()
	if streamSyncReconnectInterval > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(streamSyncReconnectInterval))
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, s.initialSyncStep(runCtx, streamID)); err != nil {
		return err
	}
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				return nil
			}
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		if err := s.handleMessageWithConn(runCtx, payload, conn); err != nil {
			log.Printf("stream sync message error stream=%s err=%v", streamID, err)
		}
	}
}

func (s *streamSync) initialSyncStep(ctx context.Context, streamID string) []byte {
	if s.state != nil {
		if record, err := s.state.GetStream(ctx, streamID); err == nil && len(record.LatestStateVector) > 0 {
			return yproto.BuildSyncStep1FromStateVector(record.LatestStateVector)
		}
	}
	return yproto.BuildSyncStep1FromStateVector(nil)
}

func (s *streamSync) handleMessage(ctx context.Context, payload []byte) error {
	return s.handleMessageWithConn(ctx, payload, nil)
}

func (s *streamSync) handleMessageWithConn(ctx context.Context, payload []byte, conn *websocket.Conn) error {
	topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		return err
	}
	if topLevel != yproto.MessageSync {
		return nil
	}
	syncType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		return err
	}
	streamID, kind := s.current()
	if streamID == "" || len(data) == 0 {
		return nil
	}
	switch syncType {
	case yproto.SyncStep1:
		if conn == nil || s.state == nil {
			return nil
		}
		update, err := s.localUpdateForStateVector(ctx, streamID, kind, data)
		if err != nil || len(update) == 0 {
			return err
		}
		return conn.WriteMessage(websocket.BinaryMessage, yproto.BuildSyncStep2FromUpdate(update))
	case yproto.SyncStep2, yproto.SyncUpdate:
		if s.state == nil {
			return nil
		}
		if err := s.state.EnsureLocalStream(ctx, streamID, firstNonEmptyText(kind, "unknown")); err != nil {
			return err
		}
		_, inserted, err := s.state.InsertInboxUpdate(ctx, streamID, data, 0)
		if err != nil {
			return err
		}
		if inserted && s.queue != nil {
			s.queue(streamID)
		}
	}
	return nil
}

func (s *streamSync) localUpdateForStateVector(ctx context.Context, streamID string, kind string, stateVector []byte) ([]byte, error) {
	if s == nil || s.state == nil || strings.TrimSpace(streamID) == "" {
		return nil, nil
	}
	doc, _, err := s.state.LoadLatestStreamDoc(ctx, streamID, firstNonEmptyText(kind, "unknown"))
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	return doc.EncodeStateAsUpdateV1(stateVector)
}
