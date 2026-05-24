package notty

import (
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

const maxRawStreamUpdateBytes = 32 << 20

type postStreamUpdateResponse struct {
	Accepted    bool   `json:"accepted"`
	Applied     bool   `json:"applied"`
	UpdateID    int64  `json:"updateId"`
	StateVector string `json:"stateVector"`
}

func (s *Server) handleWorkspaceBootstrap(w http.ResponseWriter, r *http.Request) {
	rootStreamID, err := s.requestStore(r).BootstrapWorkspaceStreams()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"workspaceId":  s.requestWorkspaceID(r),
		"rootStreamId": rootStreamID,
	})
}

func (s *Server) handlePostStreamUpdate(w http.ResponseWriter, r *http.Request) {
	s.handlePostStreamUpdateForID(w, r, chi.URLParam(r, "streamID"))
}

func (s *Server) handlePostStreamUpdateForID(w http.ResponseWriter, r *http.Request, streamID string) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRawStreamUpdateBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "stream update is too large")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(payload) == 0 {
		writeError(w, http.StatusBadRequest, "stream update is required")
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "stream-update", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	meta.Source = "http"
	meta.Trigger = "generic stream update"
	meta.Confidence = "high"
	room := s.rooms.ForDocument(streamRoomID(s.requestWorkspaceID(r), streamID))
	result, err := s.applyAndPublishStreamUpdate(r, room, nil, streamID, payload, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, postStreamUpdateResponse{
		Accepted:    result.Accepted,
		Applied:     result.Applied,
		UpdateID:    result.UpdateID,
		StateVector: base64.StdEncoding.EncodeToString(result.StateVector),
	})
}

func (s *Server) handleStreamWebsocket(w http.ResponseWriter, r *http.Request) {
	s.handleStreamWebsocketForID(w, r, chi.URLParam(r, "streamID"))
}

func (s *Server) handleStreamWebsocketForID(w http.ResponseWriter, r *http.Request, streamID string) {
	if _, err := s.requestStore(r).AuthorizeStreamAccess(streamID); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	workspaceID := s.requestWorkspaceID(r)
	room := s.rooms.ForDocument(streamRoomID(workspaceID, streamID))
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
		meta := operationMetaFromAuth(auth, "stream-y-protocol", actorID, actorType)
		actorID = meta.ActorID
		actorType = meta.ActorType
	}
	log.Printf("stream ws open stream=%s actor=%s client=%d", streamID, actorID, clientID)

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
				log.Printf("stream ws read close stream=%s actor=%s err=%v", streamID, actorID, err)
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			if err := s.handleStreamProtocolMessageWithStore(s.requestStore(r), s.requestBroker(r), room, session, streamID, payload, OperationMeta{
				ActorID:     actorID,
				ActorType:   actorType,
				ExecutionID: "ws-session",
				Tool:        "y-protocol",
				Trigger:     "websocket sync",
				Source:      "ws",
				Confidence:  "high",
			}); err != nil {
				log.Printf("stream ws protocol error stream=%s actor=%s err=%v", streamID, actorID, err)
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
			log.Printf("stream ws closed stream=%s actor=%s client=%d", streamID, actorID, clientID)
			return
		case <-session.Done():
			return
		case message := <-session.send:
			if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
				log.Printf("stream ws write error stream=%s actor=%s err=%v", streamID, actorID, err)
				return
			}
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleStreamProtocolMessageWithStore(store *Store, broker *Broker, room *DocumentRoom, session *DocumentConn, streamID string, payload []byte, meta OperationMeta) error {
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
			head, updates, err := store.EncodeStreamSyncUpdates(streamID, data)
			if err != nil {
				return err
			}
			for index, update := range updates {
				if index == 0 {
					if !session.Enqueue(yproto.BuildSyncStep2FromUpdate(update)) {
						return nil
					}
				} else if !session.Enqueue(yproto.BuildSyncUpdate(update)) {
					return nil
				}
			}
			if len(head.StateVector) > 0 {
				if !session.Enqueue(yproto.BuildSyncStep1FromStateVector(head.StateVector)) {
					return nil
				}
			}
		case yproto.SyncStep2, yproto.SyncUpdate:
			if len(data) == 0 {
				return nil
			}
			result, err := applyAndPublishStreamUpdate(store, broker, room, session, streamID, data, meta)
			if err != nil {
				return err
			}
			log.Printf("stream ws apply result stream=%s actor=%s actor_type=%s sync_type=%s bytes=%d applied=%t update_id=%d", streamID, meta.ActorID, meta.ActorType, documentSyncTypeLabel(syncType), len(data), result.Applied, result.UpdateID)
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

func (s *Server) applyAndPublishStreamUpdate(r *http.Request, room *DocumentRoom, exclude *DocumentConn, streamID string, update []byte, meta OperationMeta) (*ApplyStreamUpdateResult, error) {
	return applyAndPublishStreamUpdate(s.requestStore(r), s.requestBroker(r), room, exclude, streamID, update, meta)
}

func applyAndPublishStreamUpdate(store *Store, broker *Broker, room *DocumentRoom, exclude *DocumentConn, streamID string, update []byte, meta OperationMeta) (*ApplyStreamUpdateResult, error) {
	if isCanonicalEmptyYjsUpdate(update) {
		head, err := store.GetAuthorizedStreamHead(streamID)
		if err != nil {
			if err == ErrNotFound {
				return &ApplyStreamUpdateResult{Accepted: true, Applied: false}, nil
			}
			return nil, err
		}
		return &ApplyStreamUpdateResult{Accepted: true, Applied: false, UpdateID: head.UpdateID, StateVector: head.StateVector}, nil
	}
	result, err := store.ApplyStreamUpdate(streamID, update, meta)
	if err != nil {
		return nil, err
	}
	if !result.Applied {
		return &result, nil
	}
	if err := store.RefreshStreamDocumentCache(); err != nil {
		return nil, err
	}
	if room != nil {
		room.BroadcastSyncUpdate(yproto.BuildSyncUpdate(update), exclude)
	}
	if broker != nil {
		broker.Publish(EventEnvelope{Type: "stream.updated", Data: map[string]any{
			"streamId":  streamID,
			"kind":      result.Kind,
			"updateId":  result.UpdateID,
			"actorId":   meta.ActorID,
			"actorType": meta.ActorType,
			"updatedAt": time.Now().UTC(),
		}})
		publishAgentInboxChanges(store, broker)
	}
	return &result, nil
}
