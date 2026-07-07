package notty

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	var req CreateThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ClientOperationID == "" {
		req.ClientOperationID = r.Header.Get("X-Notty-Idempotency-Key")
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "thread-create", "system", "system")
	thread, message, created, err := s.requestStore(r).CreateThread(req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if created {
		s.requestBroker(r).Publish(EventEnvelope{Type: "thread.created", Data: thread})
		s.requestBroker(r).Publish(EventEnvelope{Type: "thread.message.created", Data: message})
		s.publishActivityChanges(r)
		s.publishAgentInboxChanges(r)
	}
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
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "thread-reply", "system", "system")
	thread, message, err := s.requestStore(r).ReplyThread(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "thread.updated", Data: thread})
	s.requestBroker(r).Publish(EventEnvelope{Type: "thread.message.created", Data: message})
	s.publishActivityChanges(r)
	s.publishAgentInboxChanges(r)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"thread":  thread,
		"message": message,
	})
}

// handleUpdateThreadStatus is PATCH /threads/{id}/status — the minimal
// resolve endpoint. Human workspace members only (any role: resolving is a
// judgment call, not an admin action); opening it to agent tooling is a
// deliberate later decision, so agent/daemon principals get 403 today.
// Idempotent no-ops return the unchanged thread and skip the broadcast.
func (s *Server) handleUpdateThreadStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	var req UpdateThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	thread, changed, err := s.requestStore(r).UpdateThreadStatus(chi.URLParam(r, "id"), req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if changed {
		s.requestBroker(r).Publish(EventEnvelope{Type: "thread.updated", Data: thread})
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": thread})
}

// handleUpdateThreadAnchor is PATCH /threads/{id}/anchor — re-anchor a thread (e.g. an orphan whose
// original text was deleted) to a new position. AuthZ mirrors /status: human workspace members only,
// so agent/daemon principals get 403. Idempotent no-ops return the unchanged thread and skip the
// broadcast; a real change emits exactly one thread.updated.
func (s *Server) handleUpdateThreadAnchor(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	var req UpdateThreadAnchorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	thread, changed, err := s.requestStore(r).UpdateThreadAnchor(chi.URLParam(r, "id"), req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if changed {
		s.requestBroker(r).Publish(EventEnvelope{Type: "thread.updated", Data: thread})
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": thread})
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	thread, err := s.requestStore(r).GetThread(chi.URLParam(r, "id"))
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
