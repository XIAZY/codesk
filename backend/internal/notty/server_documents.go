package notty

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

func (s *Server) handleDocumentThreads(w http.ResponseWriter, r *http.Request) {
	threads, err := s.requestStore(r).ListThreadsForDocument(chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"threads": threads,
	})
}

func (s *Server) handleCreateDocument(w http.ResponseWriter, r *http.Request) {
	var req CreateDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "document-create", "system", "system")
	document, err := s.requestStore(r).CreateDocument(req, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.publishActivityChanges(r)
	// document.created cards are instant (task #3): drain the inbox doorbell the create just rang, so every
	// carded agent wakes now. Both create surfaces (REST + the daemon's local-file create) reach this handler.
	s.publishAgentInboxChanges(r)
	writeJSON(w, http.StatusCreated, documentMetadata(document))
}

type documentFrame struct {
	documentID string
	payload    []byte
}

type documentWebsocketConn struct {
	conn            *websocket.Conn
	multiplexed     bool
	fixedDocumentID string
	clientID        uint64
	actorID         string
	actorType       string
	send            chan documentFrame
	done            chan struct{}
	closeOnce       sync.Once
	subscribed      map[string]*DocumentRoom
}

func newRawDocumentWebsocketConn(conn *websocket.Conn, documentID string, clientID uint64, actorID, actorType string) *documentWebsocketConn {
	return &documentWebsocketConn{
		conn:            conn,
		fixedDocumentID: documentID,
		clientID:        clientID,
		actorID:         actorID,
		actorType:       actorType,
		send:            make(chan documentFrame, 64),
		done:            make(chan struct{}),
		subscribed:      map[string]*DocumentRoom{},
	}
}

func newMuxDocumentWebsocketConn(conn *websocket.Conn, clientID uint64, actorID, actorType string) *documentWebsocketConn {
	return &documentWebsocketConn{
		conn:        conn,
		multiplexed: true,
		clientID:    clientID,
		actorID:     actorID,
		actorType:   actorType,
		send:        make(chan documentFrame, 64),
		done:        make(chan struct{}),
		subscribed:  map[string]*DocumentRoom{},
	}
}

func (c *documentWebsocketConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

func (c *documentWebsocketConn) sendDocument(documentID string, payload []byte) bool {
	if c == nil || c.send == nil || len(payload) == 0 {
		return false
	}
	select {
	case <-c.done:
		return false
	case c.send <- documentFrame{documentID: documentID, payload: payload}:
		return true
	}
}

func (c *documentWebsocketConn) trySendDocument(documentID string, payload []byte) bool {
	if c == nil || c.send == nil || len(payload) == 0 {
		return false
	}
	select {
	case <-c.done:
		return false
	case c.send <- documentFrame{documentID: documentID, payload: payload}:
		return true
	default:
		return false
	}
}

func (c *documentWebsocketConn) readDocumentFrame() (documentFrame, error) {
	messageType, payload, err := c.conn.ReadMessage()
	if err != nil {
		return documentFrame{}, err
	}
	if messageType != websocket.BinaryMessage {
		return documentFrame{}, nil
	}
	if !c.multiplexed {
		return documentFrame{documentID: c.fixedDocumentID, payload: payload}, nil
	}
	documentID, documentPayload, err := yproto.DecodeDocumentMessage(payload)
	if err != nil {
		return documentFrame{}, err
	}
	return documentFrame{documentID: documentID, payload: documentPayload}, nil
}

func (c *documentWebsocketConn) writeDocumentFrame(frame documentFrame) error {
	payload := frame.payload
	if c.multiplexed {
		payload = yproto.BuildDocumentMessage(frame.documentID, frame.payload)
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *documentWebsocketConn) writeLoop() {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case frame := <-c.send:
			if err := c.writeDocumentFrame(frame); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				c.close()
				return
			}
		}
	}
}

func (s *Server) handleDocumentWebsocket(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "id")
	if documentID == "" {
		writeError(w, http.StatusBadRequest, "document id is required")
		return
	}
	store := s.requestStore(r)
	if !store.HasDocument(documentID) {
		writeError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	clientID, _ := strconv.ParseUint(r.URL.Query().Get("client_id"), 10, 64)
	actorID, actorType := documentWebsocketActor(r)
	session := newRawDocumentWebsocketConn(conn, documentID, clientID, actorID, actorType)
	s.serveDocumentWebsocket(r, session)
}

func (s *Server) handleWorkspaceDocumentSyncWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	clientID, _ := strconv.ParseUint(r.URL.Query().Get("client_id"), 10, 64)
	actorID, actorType := documentWebsocketActor(r)
	session := newMuxDocumentWebsocketConn(conn, clientID, actorID, actorType)
	s.serveDocumentWebsocket(r, session)
}

func documentWebsocketActor(r *http.Request) (string, string) {
	actorID := r.URL.Query().Get("actor_id")
	actorType := r.URL.Query().Get("actor_type")
	if auth, ok := authFromContext(r.Context()); ok {
		meta := operationMetaFromAuth(auth, "y-protocol", actorID, actorType)
		actorID = meta.ActorID
		actorType = meta.ActorType
	}
	return actorID, actorType
}

func (s *Server) serveDocumentWebsocket(r *http.Request, session *documentWebsocketConn) {
	defer session.closeAndUnsubscribeAll()
	_ = session.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	session.conn.SetPongHandler(func(string) error {
		return session.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	go session.writeLoop()

	if !session.multiplexed {
		if _, err := s.ensureDocumentSubscribed(r, session, session.fixedDocumentID); err != nil {
			log.Printf("document ws subscribe failed doc=%s actor=%s err=%v", session.fixedDocumentID, session.actorID, err)
			return
		}
	}
	log.Printf("document ws open multiplexed=%t doc=%s actor=%s client=%d", session.multiplexed, session.fixedDocumentID, session.actorID, session.clientID)

	for {
		frame, err := session.readDocumentFrame()
		if err != nil {
			log.Printf("document ws read close multiplexed=%t doc=%s actor=%s err=%v", session.multiplexed, frame.documentID, session.actorID, err)
			return
		}
		if frame.documentID == "" || len(frame.payload) == 0 {
			continue
		}
		room, err := s.ensureDocumentSubscribed(r, session, frame.documentID)
		if err != nil {
			log.Printf("document ws subscribe error doc=%s actor=%s err=%v", frame.documentID, session.actorID, err)
			return
		}
		if err := s.handleDocumentProtocolMessageWithStore(s.requestStore(r), s.requestBroker(r), room, session, frame.documentID, frame.payload, OperationMeta{
			ActorID:     session.actorID,
			ActorType:   session.actorType,
			ExecutionID: "ws-session",
			Tool:        "y-protocol",
			Trigger:     "websocket sync",
			Source:      "ws",
			Confidence:  "high",
		}); err != nil {
			log.Printf("document ws protocol error doc=%s actor=%s err=%v", frame.documentID, session.actorID, err)
			return
		}
	}
}

func (s *Server) ensureDocumentSubscribed(r *http.Request, session *documentWebsocketConn, documentID string) (*DocumentRoom, error) {
	if documentID == "" {
		return nil, errors.New("document id is required")
	}
	if room := session.subscribed[documentID]; room != nil {
		return room, nil
	}
	store := s.requestStore(r)
	if !store.HasDocument(documentID) {
		return nil, ErrNotFound
	}
	room := s.rooms.ForDocument(s.requestWorkspaceID(r) + ":" + documentID)
	room.Add(session)
	session.subscribed[documentID] = room
	if awareness := room.SnapshotAwareness(); len(awareness) > 0 {
		clients := make([]uint64, 0, len(awareness))
		for current := range awareness {
			clients = append(clients, current)
		}
		session.trySendDocument(documentID, yproto.BuildAwarenessUpdate(awareness, clients))
	}
	return room, nil
}

func (c *documentWebsocketConn) closeAndUnsubscribeAll() {
	c.close()
	for documentID, room := range c.subscribed {
		room.Remove(c)
		if c.clientID != 0 && room.RemoveAwareness(c.clientID) {
			message := yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
				c.clientID: {Clock: time.Now().Unix(), State: []byte("null")},
			}, []uint64{c.clientID})
			room.BroadcastDocumentBestEffort(documentID, message, nil)
		}
	}
	log.Printf("document ws closed multiplexed=%t doc=%s actor=%s client=%d", c.multiplexed, c.fixedDocumentID, c.actorID, c.clientID)
}

func (s *Server) handleDocumentProtocolMessageWithStore(store *Store, broker *Broker, room *DocumentRoom, session documentSubscriber, documentID string, payload []byte, meta OperationMeta) error {
	if store == nil {
		return errors.New("workspace store is required")
	}
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		return err
	}

	switch messageType {
	case yproto.MessageSync:
		syncType, data, err := yproto.DecodeSyncMessage(reader)
		if err != nil {
			return err
		}
		switch syncType {
		case yproto.SyncStep1:
			document, updates, err := store.EncodeDocumentSyncUpdates(documentID, data)
			if err != nil {
				return err
			}
			for index, update := range updates {
				if index == 0 {
					if !sendDocumentPayload(session, documentID, yproto.BuildSyncStep2FromUpdate(update)) {
						return nil
					}
				} else {
					if !sendDocumentPayload(session, documentID, yproto.BuildSyncUpdate(update)) {
						return nil
					}
				}
			}
			if document.StateVector != "" {
				stateVector, err := base64.StdEncoding.DecodeString(document.StateVector)
				if err != nil {
					return err
				}
				if !sendDocumentPayload(session, documentID, yproto.BuildSyncStep1FromStateVector(stateVector)) {
					return nil
				}
			}
		case yproto.SyncStep2, yproto.SyncUpdate:
			if len(data) == 0 {
				return nil
			}
			log.Printf("document ws inbound update doc=%s actor=%s actor_type=%s sync_type=%s bytes=%d canonical_empty_yjs_update=%t", documentID, meta.ActorID, meta.ActorType, documentSyncTypeLabel(syncType), len(data), isCanonicalEmptyYjsUpdate(data))
			result, err := applyAndPublishDocumentUpdate(store, broker, room, session, documentID, data, meta)
			if err != nil {
				return err
			}
			log.Printf("document ws apply result doc=%s actor=%s actor_type=%s sync_type=%s bytes=%d applied=%t update_id=%d", documentID, meta.ActorID, meta.ActorType, documentSyncTypeLabel(syncType), len(data), result.Applied, result.Document.UpdateID)
			if result.Applied {
				if document, _, err := store.EncodeDocumentSyncUpdates(documentID, nil); err != nil {
					return err
				} else if document.StateVector != "" {
					stateVector, decodeErr := base64.StdEncoding.DecodeString(document.StateVector)
					if decodeErr != nil {
						return decodeErr
					}
					sendDocumentPayload(session, documentID, yproto.BuildSyncStep1FromStateVector(stateVector))
				}
			}
		}
	case yproto.MessageAwareness:
		updates, err := yproto.DecodeAwarenessUpdate(reader)
		if err != nil {
			return err
		}
		changed := room.ApplyAwareness(updates)
		if len(changed) == 0 {
			return nil
		}
		snapshot := room.SnapshotAwareness()
		broadcast := yproto.BuildAwarenessUpdate(snapshot, changed)
		room.BroadcastDocumentBestEffort(documentID, broadcast, session)
	}
	return nil
}

func sendDocumentPayload(session documentSubscriber, documentID string, payload []byte) bool {
	if session == nil {
		return false
	}
	return session.sendDocument(documentID, payload)
}

func (s *Server) applyAndPublishDocumentUpdate(r *http.Request, room *DocumentRoom, exclude documentSubscriber, documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error) {
	return applyAndPublishDocumentUpdate(s.requestStore(r), s.requestBroker(r), room, exclude, documentID, update, meta)
}

func applyAndPublishDocumentUpdate(store *Store, broker *Broker, room *DocumentRoom, exclude documentSubscriber, documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error) {
	if isCanonicalEmptyYjsUpdate(update) {
		document, err := store.GetDocument(documentID)
		if err != nil {
			return nil, err
		}
		return &ApplyCRDTUpdateResult{Document: document, Applied: false}, nil
	}
	result, err := store.ApplyCRDTUpdateWithResult(documentID, update, meta)
	if err != nil {
		return nil, err
	}
	if !result.Applied {
		return result, nil
	}
	updated := result.Document
	if room != nil {
		room.BroadcastSyncUpdate(documentID, yproto.BuildSyncUpdate(update), exclude)
	}
	if broker != nil {
		publishActivityChanges(store, broker)
		if !updated.Hidden {
			publishAgentInboxChanges(store, broker)
		}
	}
	return result, nil
}

func documentSyncTypeLabel(syncType uint64) string {
	switch syncType {
	case yproto.SyncStep1:
		return "sync_step_1"
	case yproto.SyncStep2:
		return "sync_step_2"
	case yproto.SyncUpdate:
		return "sync_update"
	default:
		return "unknown"
	}
}

func isCanonicalEmptyYjsUpdate(update []byte) bool {
	return len(update) == 2 && update[0] == 0 && update[1] == 0
}
