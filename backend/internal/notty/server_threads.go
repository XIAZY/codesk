package notty

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	var req CreateThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "thread-create",
	}
	thread, message, err := s.store.CreateThread(req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "thread.created", Data: thread})
	s.subscribers.Publish(EventEnvelope{Type: "thread.message.created", Data: message})
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"thread":  thread,
		"message": message,
	})
}

func (s *Server) handleReplyThread(w http.ResponseWriter, r *http.Request) {
	var req ReplyThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "thread-reply",
	}
	thread, message, err := s.store.ReplyThread(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "thread.updated", Data: thread})
	s.subscribers.Publish(EventEnvelope{Type: "thread.message.created", Data: message})
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"thread":  thread,
		"message": message,
	})
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	thread, err := s.store.GetThread(chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": thread})
}
