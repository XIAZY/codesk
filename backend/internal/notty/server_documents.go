package notty

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

func (s *Server) handleDocumentByPath(w http.ResponseWriter, r *http.Request) {
	document, err := s.requestStore(r).GetDocumentMetadataByPath(r.URL.Query().Get("path"))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": document})
}

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
	meta := operationMetaFromAuth(auth, "document-create", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	document, err := s.requestStore(r).CreateDocument(req, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	event := EventEnvelope{Type: "document.created", Data: DocumentLifecycleEvent{
		DocumentID: document.ID,
		Path:       document.Path,
		Title:      document.Title,
		UpdatedAt:  document.UpdatedAt,
		ActorID:    meta.ActorID,
	}}
	s.requestBroker(r).Publish(event)
	s.publishAgentInboxChanges(r)
	writeJSON(w, http.StatusCreated, documentMetadata(document))
}

func (s *Server) handleMoveDocument(w http.ResponseWriter, r *http.Request) {
	var req UpdateDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "document-move", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	document, oldPath, err := s.requestStore(r).MoveDocument(chi.URLParam(r, "id"), req.Path, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "document.moved", Data: DocumentLifecycleEvent{
		DocumentID: document.ID,
		Path:       document.Path,
		OldPath:    oldPath,
		Title:      document.Title,
		UpdatedAt:  document.UpdatedAt,
		ActorID:    meta.ActorID,
	}})
	writeJSON(w, http.StatusOK, documentMetadata(document))
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "document-delete", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	document, err := s.requestStore(r).DeleteDocument(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "document.deleted", Data: DocumentLifecycleEvent{
		DocumentID: document.ID,
		Path:       document.Path,
		Title:      document.Title,
		UpdatedAt:  time.Now().UTC(),
		ActorID:    meta.ActorID,
	}})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleDocumentWebsocket(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "id")
	store := s.requestStore(r)
	if !store.HasDocument(documentID) {
		writeError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	workspaceID := s.requestWorkspaceID(r)
	room := s.rooms.ForDocument(workspaceID + ":" + documentID)
	session := newDocumentConn(64)
	room.Add(session)
	defer func() {
		session.Close()
		room.Remove(session)
	}()

	clientID, _ := strconv.ParseUint(r.URL.Query().Get("client_id"), 10, 64)
	actorID := r.URL.Query().Get("actor_id")
	actorType := r.URL.Query().Get("actor_type")
	if auth, ok := authFromContext(r.Context()); ok {
		meta := operationMetaFromAuth(auth, "y-protocol", actorID, actorType)
		actorID = meta.ActorID
		actorType = meta.ActorType
	}
	log.Printf("document ws open doc=%s actor=%s client=%d", documentID, actorID, clientID)

	if awareness := room.SnapshotAwareness(); len(awareness) > 0 {
		clients := make([]uint64, 0, len(awareness))
		for current := range awareness {
			clients = append(clients, current)
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, yproto.BuildAwarenessUpdate(awareness, clients))
	}

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				log.Printf("document ws read close doc=%s actor=%s err=%v", documentID, actorID, err)
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			if err := s.handleDocumentProtocolMessageWithStore(store, s.requestBroker(r), room, session, documentID, payload, OperationMeta{
				ActorID:     actorID,
				ActorType:   actorType,
				ExecutionID: "ws-session",
				Tool:        "y-protocol",
				Trigger:     "websocket sync",
				Source:      "ws",
				Confidence:  "high",
			}); err != nil {
				log.Printf("document ws protocol error doc=%s actor=%s err=%v", documentID, actorID, err)
				return
			}
		}
	}()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			if clientID != 0 && room.RemoveAwareness(clientID) {
				message := yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
					clientID: {Clock: time.Now().Unix(), State: []byte("null")},
				}, []uint64{clientID})
				room.BroadcastBestEffort(message, nil)
			}
			log.Printf("document ws closed doc=%s actor=%s client=%d", documentID, actorID, clientID)
			return
		case <-session.Done():
			return
		case message := <-session.send:
			if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
				log.Printf("document ws write error doc=%s actor=%s err=%v", documentID, actorID, err)
				return
			}
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleDocumentProtocolMessage(room *DocumentRoom, session *DocumentConn, documentID string, payload []byte, meta OperationMeta) error {
	return s.handleDocumentProtocolMessageWithStore(s.store, s.subscribers, room, session, documentID, payload, meta)
}

func (s *Server) handleDocumentProtocolMessageWithStore(store *Store, broker *Broker, room *DocumentRoom, session *DocumentConn, documentID string, payload []byte, meta OperationMeta) error {
	if store == nil {
		store = s.store
	}
	if broker == nil {
		broker = s.subscribers
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
					if !session.Enqueue(yproto.BuildSyncStep2FromUpdate(update)) {
						return nil
					}
				} else {
					if !session.Enqueue(yproto.BuildSyncUpdate(update)) {
						return nil
					}
				}
			}
			if document.StateVector != "" {
				stateVector, err := base64.StdEncoding.DecodeString(document.StateVector)
				if err != nil {
					return err
				}
				if !session.Enqueue(yproto.BuildSyncStep1FromStateVector(stateVector)) {
					return nil
				}
			}
		case yproto.SyncStep2, yproto.SyncUpdate:
			if len(data) == 0 {
				return nil
			}
			log.Printf("document ws inbound update doc=%s actor=%s actor_type=%s sync_type=%s bytes=%d canonical_empty_yjs_update=%t", documentID, meta.ActorID, meta.ActorType, documentSyncTypeLabel(syncType), len(data), isCanonicalEmptyYjsUpdate(data))
			if isCanonicalEmptyYjsUpdate(data) {
				return nil
			}
			result, err := store.ApplyCRDTUpdateWithResult(documentID, data, meta)
			if err != nil {
				return err
			}
			log.Printf("document ws apply result doc=%s actor=%s actor_type=%s sync_type=%s bytes=%d applied=%t update_id=%d", documentID, meta.ActorID, meta.ActorType, documentSyncTypeLabel(syncType), len(data), result.Applied, result.Document.UpdateID)
			if !result.Applied {
				return nil
			}
			updated := result.Document
			room.BroadcastSyncUpdate(yproto.BuildSyncUpdate(data), session)
			broker.Publish(EventEnvelope{Type: "document.updated", Data: DocumentUpdateEvent{
				DocumentID: updated.ID,
				UpdateID:   updated.UpdateID,
				Path:       updated.Path,
				UpdatedAt:  updated.UpdatedAt,
				ActorID:    meta.ActorID,
			}})
			publishAgentInboxChanges(store, broker)
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
		room.BroadcastBestEffort(broadcast, session)
	}
	return nil
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
