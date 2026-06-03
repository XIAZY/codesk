package syncer

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

type workspaceDocumentSocket struct {
	runtime *workspaceRuntime

	mu        sync.Mutex
	desired   map[string]*document
	needsSync map[string]struct{}
	synced    map[string]struct{}

	wake chan struct{}
	send chan outboundDocumentMessage
}

type outboundDocumentMessage struct {
	documentID string
	payload    []byte
	result     chan error
}

const documentSocketSendTimeout = 30 * time.Second

func newWorkspaceDocumentSocket(runtime *workspaceRuntime) *workspaceDocumentSocket {
	return &workspaceDocumentSocket{
		runtime:   runtime,
		desired:   map[string]*document{},
		needsSync: map[string]struct{}{},
		synced:    map[string]struct{}{},
		wake:      make(chan struct{}, 1),
		send:      make(chan outboundDocumentMessage, 64),
	}
}

func (s *workspaceDocumentSocket) SetDesiredDocuments(documents []*document) {
	if s == nil {
		return
	}
	next := map[string]*document{}
	for _, document := range documents {
		if document == nil || document.ID == "" {
			continue
		}
		if s.runtime == nil || document.ID != s.runtime.rootDocumentID {
			if isIgnoredDocumentPath(document.Path) {
				continue
			}
		}
		next[document.ID] = cloneSyncDocument(document)
	}

	s.mu.Lock()
	for documentID, document := range next {
		current := s.desired[documentID]
		if current == nil || current.Path != document.Path {
			s.needsSync[documentID] = struct{}{}
			delete(s.synced, documentID)
		}
		s.desired[documentID] = document
	}
	for documentID := range s.desired {
		if _, ok := next[documentID]; !ok {
			delete(s.desired, documentID)
			delete(s.needsSync, documentID)
			delete(s.synced, documentID)
		}
	}
	s.mu.Unlock()
	s.wakeWriter()
}

func (s *workspaceDocumentSocket) Document(documentID string) *document {
	if s == nil || documentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSyncDocument(s.desired[documentID])
}

func (s *workspaceDocumentSocket) wakeWriter() {
	if s == nil || s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *workspaceDocumentSocket) SendSyncUpdate(ctx context.Context, documentID string, update []byte) error {
	if s == nil || documentID == "" || len(update) == 0 {
		return nil
	}
	return s.SendProtocolMessage(ctx, documentID, yproto.BuildSyncUpdate(update))
}

func (s *workspaceDocumentSocket) SendProtocolMessage(ctx context.Context, documentID string, payload []byte) error {
	if s == nil || documentID == "" || len(payload) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sendCtx, cancel := context.WithTimeout(ctx, documentSocketSendTimeout)
	defer cancel()
	result := make(chan error, 1)
	msg := outboundDocumentMessage{documentID: documentID, payload: payload, result: result}
	select {
	case s.send <- msg:
		s.wakeWriter()
	case <-sendCtx.Done():
		return sendCtx.Err()
	}
	select {
	case err := <-result:
		return err
	case <-sendCtx.Done():
		return sendCtx.Err()
	}
}

func (s *workspaceDocumentSocket) QueueProtocolMessage(documentID string, payload []byte) error {
	if s == nil || documentID == "" || len(payload) == 0 {
		return nil
	}
	msg := outboundDocumentMessage{documentID: documentID, payload: payload}
	select {
	case s.send <- msg:
		s.wakeWriter()
		return nil
	default:
		return fmt.Errorf("document socket send queue is full for %s", documentID)
	}
}

func (s *workspaceDocumentSocket) hasDesiredDocuments() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.desired) > 0
}

func (s *workspaceDocumentSocket) resetConnectionSyncState() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.synced = map[string]struct{}{}
	for documentID := range s.desired {
		s.needsSync[documentID] = struct{}{}
	}
	s.mu.Unlock()
	s.wakeWriter()
}

func (s *workspaceDocumentSocket) pendingSyncDocuments() []*document {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.needsSync))
	for documentID := range s.needsSync {
		if _, already := s.synced[documentID]; already {
			continue
		}
		if s.desired[documentID] != nil {
			ids = append(ids, documentID)
		}
	}
	sort.Strings(ids)
	documents := make([]*document, 0, len(ids))
	for _, documentID := range ids {
		documents = append(documents, cloneSyncDocument(s.desired[documentID]))
	}
	return documents
}

func (s *workspaceDocumentSocket) markSynced(documentID string) {
	if s == nil || documentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.desired[documentID] == nil {
		return
	}
	s.synced[documentID] = struct{}{}
	delete(s.needsSync, documentID)
}

func (s *workspaceDocumentSocket) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if !s.hasDesiredDocuments() {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("%s document socket error: %v", s.runtime.actorID(), err)
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

func (s *workspaceDocumentSocket) runOnce(ctx context.Context) error {
	if s == nil || s.runtime == nil {
		return nil
	}
	paceDocumentConnect()
	clientID := nextAwarenessClientID()
	query := url.Values{
		"client_id":  {fmt.Sprintf("%d", clientID)},
		"actor_id":   {s.runtime.actorID()},
		"actor_type": {s.runtime.actorKind()},
	}
	conn, _, err := dialWorkspaceWebsocket(ctx, s.runtime.cfg, "/ws/documents-sync", query, s.runtime.actingAgentID())
	if err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stopClose()
	defer conn.Close()

	s.resetConnectionSyncState()
	runCtx, cancelRun := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		err := s.writeLoop(runCtx, conn)
		_ = conn.Close()
		errCh <- err
	}()
	defer func() {
		cancelRun()
		_ = conn.Close()
		<-writerDone
	}()

	for {
		select {
		case err := <-errCh:
			return err
		default:
		}
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		documentID, documentPayload, err := yproto.DecodeDocumentMessage(payload)
		if err != nil {
			log.Printf("%s document socket decode error: %v", s.runtime.actorID(), err)
			continue
		}
		if err := s.runtime.handleDocumentSyncMessage(documentID, documentPayload); err != nil {
			log.Printf("%s document socket message error doc=%s err=%v", s.runtime.actorID(), documentID, err)
		}
	}
}

func (s *workspaceDocumentSocket) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		if err := s.flushSyncSteps(conn); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
		case msg := <-s.send:
			if msg.documentID == "" || len(msg.payload) == 0 {
				if msg.result != nil {
					msg.result <- nil
				}
				continue
			}
			err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(msg.documentID, msg.payload))
			if msg.result != nil {
				msg.result <- err
			}
			if err != nil {
				return err
			}
		}
	}
}

func (s *workspaceDocumentSocket) flushSyncSteps(conn *websocket.Conn) error {
	for _, document := range s.pendingSyncDocuments() {
		if document == nil || document.ID == "" {
			continue
		}
		payload := s.initialSyncStep(document)
		if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(document.ID, payload)); err != nil {
			return err
		}
		s.markSynced(document.ID)
	}
	return nil
}

func (s *workspaceDocumentSocket) initialSyncStep(document *document) []byte {
	if s != nil && s.runtime != nil && s.runtime.docCache != nil && document != nil {
		if stateVector := s.runtime.docCache.localStateVector(document.ID); len(stateVector) > 0 {
			return yproto.BuildSyncStep1FromStateVector(stateVector)
		}
	}
	return yproto.BuildSyncStep1FromStateVector(nil)
}

func (r *workspaceRuntime) handleDocumentSyncMessage(documentID string, payload []byte) error {
	if r == nil || r.documentSocket == nil {
		return nil
	}
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
	document := r.documentSocket.Document(documentID)
	if document == nil || document.ID == "" {
		return nil
	}
	switch syncType {
	case yproto.SyncStep1:
		if r.docCache == nil {
			return nil
		}
		diff, err := r.docCache.localStateDiff(document.ID, document.Path, data)
		if err != nil {
			return err
		}
		if isCanonicalEmptyUpdate(diff) {
			return r.docCache.clearOutboxUpdates(document.ID)
		}
		return r.documentSocket.QueueProtocolMessage(document.ID, yproto.BuildSyncStep2FromUpdate(diff))
	case yproto.SyncStep2, yproto.SyncUpdate:
		if len(data) == 0 || r.docCache == nil {
			return nil
		}
		appended, err := r.docCache.appendPendingRemoteUpdate(document.ID, document.Path, data)
		if err != nil {
			return err
		}
		if appended {
			r.markDocumentDirty(document.ID)
		}
	}
	return nil
}

func isCanonicalEmptyUpdate(update []byte) bool {
	return len(update) == 0 || bytes.Equal(update, []byte{0, 0})
}

func cloneSyncDocument(document *document) *document {
	if document == nil {
		return nil
	}
	clone := *document
	return &clone
}

func cloneDocuments(documents []*document) []*document {
	if len(documents) == 0 {
		return nil
	}
	clone := make([]*document, 0, len(documents))
	for _, document := range documents {
		if document == nil {
			continue
		}
		clone = append(clone, cloneSyncDocument(document))
	}
	return clone
}
