package notty

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"notty/internal/yproto"
)

const maxRawDocumentUpdateBytes = 32 << 20

type postDocumentUpdateResponse struct {
	Accepted bool  `json:"accepted"`
	Applied  bool  `json:"applied"`
	UpdateID int64 `json:"updateId"`
}

func (s *Server) handleDocumentByPath(w http.ResponseWriter, r *http.Request) {
	store := s.requestStore(r)
	document, err := store.GetStreamDocumentMetadataByPath(r.URL.Query().Get("path"))
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
	document, err := s.requestStore(r).CreateStreamDocument(req, meta)
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
	document, oldPath, err := s.requestStore(r).MoveStreamDocument(chi.URLParam(r, "id"), req.Path, meta)
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
	document, err := s.requestStore(r).DeleteStreamDocument(chi.URLParam(r, "id"), meta)
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

func (s *Server) handlePostDocumentUpdate(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "id")
	store := s.requestStore(r)
	if !store.HasStreamDocument(documentID) {
		writeError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRawDocumentUpdateBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "document update is too large")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(payload) == 0 {
		writeError(w, http.StatusBadRequest, "document update is required")
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "document-update", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	meta.Source = "http"
	meta.Trigger = "daemon outgoing update"
	meta.Confidence = "high"
	room := s.rooms.ForDocument(s.requestWorkspaceID(r) + ":" + documentID)
	result, err := s.applyAndPublishDocumentUpdate(r, room, nil, documentID, payload, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, postDocumentUpdateResponse{
		Accepted: true,
		Applied:  result.Applied,
		UpdateID: result.Document.UpdateID,
	})
}

func (s *Server) handleDocumentWebsocket(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "id")
	store := s.requestStore(r)
	if !store.HasStreamDocument(documentID) {
		writeError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	s.handleStreamWebsocketForID(w, r, documentID)
}

func (s *Server) handleDocumentProtocolMessage(room *DocumentRoom, session *DocumentConn, documentID string, payload []byte, meta OperationMeta) error {
	return s.handleStreamProtocolMessageWithStore(s.store, s.subscribers, room, session, documentID, payload, meta)
}

func (s *Server) applyAndPublishDocumentUpdate(r *http.Request, room *DocumentRoom, exclude *DocumentConn, documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error) {
	return applyAndPublishDocumentUpdate(s.requestStore(r), s.requestBroker(r), room, exclude, documentID, update, meta)
}

func applyAndPublishDocumentUpdate(store *Store, broker *Broker, room *DocumentRoom, exclude *DocumentConn, documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error) {
	result, err := applyAndPublishStreamUpdate(store, broker, room, exclude, documentID, update, meta)
	if err != nil {
		return nil, err
	}
	document, err := store.GetStreamDocument(documentID)
	if err != nil {
		return nil, err
	}
	return &ApplyCRDTUpdateResult{
		Document: document,
		Applied:  result.Applied,
	}, nil
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
