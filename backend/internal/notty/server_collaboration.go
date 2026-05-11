package notty

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var req UpsertPresenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if auth, ok := authFromContext(r.Context()); ok {
		meta := operationMetaFromAuth(auth, "presence", req.ActorID, req.ActorType)
		req.ActorID = meta.ActorID
		req.ActorType = meta.ActorType
	}
	presence, err := s.requestStore(r).UpsertPresence(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "presence.updated", Data: presence})
	writeJSON(w, http.StatusOK, presence)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "user-create", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	user, err := s.requestStore(r).CreateUser(req, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "user.created", Data: user})
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "user-update", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	user, err := s.requestStore(r).UpdateUser(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "user.updated", Data: user})
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "user-delete", actorFromRequest(r, "owner"), actorTypeFromRequest(r, "human"))
	user, err := s.requestStore(r).DeleteUser(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "user.deleted", Data: user})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
