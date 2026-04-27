package notty

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	document, err := s.store.GetDocument(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"documents": s.store.ListDocuments(),
	})
}

func (s *Server) handleDocumentByPath(w http.ResponseWriter, r *http.Request) {
	document, err := s.store.GetDocumentByPath(r.URL.Query().Get("path"))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDocumentThreads(w http.ResponseWriter, r *http.Request) {
	threads, err := s.store.ListThreadsForDocument(chi.URLParam(r, "id"))
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
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "document-create",
	}
	document, err := s.store.CreateDocument(req, meta)
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
	s.subscribers.Publish(event)
	writeJSON(w, http.StatusCreated, document)
}

func (s *Server) handleMoveDocument(w http.ResponseWriter, r *http.Request) {
	var req UpdateDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "document-move",
	}
	document, oldPath, err := s.store.MoveDocument(chi.URLParam(r, "id"), req.Path, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "document.moved", Data: DocumentLifecycleEvent{
		DocumentID: document.ID,
		Path:       document.Path,
		OldPath:    oldPath,
		Title:      document.Title,
		UpdatedAt:  document.UpdatedAt,
		ActorID:    meta.ActorID,
	}})
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "document-delete",
	}
	document, err := s.store.DeleteDocument(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "document.deleted", Data: DocumentLifecycleEvent{
		DocumentID: document.ID,
		Path:       document.Path,
		Title:      document.Title,
		UpdatedAt:  time.Now().UTC(),
		ActorID:    meta.ActorID,
	}})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	var req ApplyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	update, err := base64.StdEncoding.DecodeString(req.Update)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.store.ApplyCRDTUpdateWithResult(chi.URLParam(r, "id"), update, req.Meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	document := result.Document

	event := EventEnvelope{Type: "document.updated", Data: DocumentUpdateEvent{
		DocumentID:  document.ID,
		UpdateID:    document.UpdateID,
		StateVector: document.StateVector,
		Update:      req.Update,
		Path:        document.Path,
		UpdatedAt:   document.UpdatedAt,
		ActorID:     req.Meta.ActorID,
	}}
	s.subscribers.Publish(event)
	if result.MentionsChanged {
		s.subscribers.Publish(documentMentionsUpdatedEvent(document, req.Meta.ActorID))
	}
	s.rooms.ForDocument(document.ID).Broadcast(yproto.BuildSyncUpdate(update), nil)
	writeJSON(w, http.StatusOK, ApplyUpdateResponse{Document: document})
}

func (s *Server) handleDocumentSync(w http.ResponseWriter, r *http.Request) {
	var req DocumentSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var stateVector []byte
	if strings.TrimSpace(req.StateVector) != "" {
		var err error
		stateVector, err = base64.StdEncoding.DecodeString(req.StateVector)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	document, update, err := s.store.EncodeDocumentUpdate(chi.URLParam(r, "id"), stateVector)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, DocumentSyncResponse{
		Document: document,
		Update:   base64.StdEncoding.EncodeToString(update),
	})
}

func (s *Server) handleDocumentWebsocket(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "id")
	document, err := s.store.GetLiveDocument(documentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	room := s.rooms.ForDocument(documentID)
	session := &DocumentConn{send: make(chan []byte, 64)}
	room.Add(session)
	defer room.Remove(session)

	clientID, _ := strconv.ParseUint(r.URL.Query().Get("client_id"), 10, 64)
	actorID := r.URL.Query().Get("actor_id")
	actorType := r.URL.Query().Get("actor_type")
	log.Printf("document ws open doc=%s actor=%s client=%d", documentID, actorID, clientID)

	if awareness := room.SnapshotAwareness(); len(awareness) > 0 {
		clients := make([]uint64, 0, len(awareness))
		for current := range awareness {
			clients = append(clients, current)
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, yproto.BuildAwarenessUpdate(awareness, clients))
	}

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
			if err := s.handleDocumentProtocolMessage(room, session, document, payload, OperationMeta{
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
				room.Broadcast(message, nil)
			}
			log.Printf("document ws closed doc=%s actor=%s client=%d", documentID, actorID, clientID)
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

func (s *Server) handleDocumentProtocolMessage(room *DocumentRoom, session *DocumentConn, document *Document, payload []byte, meta OperationMeta) error {
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
			reply, err := yproto.BuildSyncStep2(document.Doc, data)
			if err != nil {
				return err
			}
			session.send <- reply
			session.send <- yproto.BuildSyncStep1(document.Doc)
		case yproto.SyncStep2, yproto.SyncUpdate:
			result, err := s.store.ApplyCRDTUpdateWithResult(document.ID, data, meta)
			if err != nil {
				return err
			}
			updated := result.Document
			document.Content = updated.Content
			document.CRDTState = updated.CRDTState
			document.StateVector = updated.StateVector
			document.UpdateID = updated.UpdateID
			document.UpdatedAt = updated.UpdatedAt
			if result.MentionsChanged {
				s.subscribers.Publish(documentMentionsUpdatedEvent(updated, meta.ActorID))
			}
			room.Broadcast(yproto.BuildSyncUpdate(data), session)
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
		room.Broadcast(broadcast, session)
	}
	return nil
}

func documentMentionsUpdatedEvent(document *Document, actorID string) EventEnvelope {
	return EventEnvelope{Type: "document.mentions.updated", Data: DocumentLifecycleEvent{
		DocumentID: document.ID,
		Path:       document.Path,
		Title:      document.Title,
		UpdatedAt:  document.UpdatedAt,
		ActorID:    actorID,
	}}
}
