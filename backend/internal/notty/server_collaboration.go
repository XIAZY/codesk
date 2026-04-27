package notty

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"notty/internal/yproto"
)

func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var req UpsertPresenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	presence, err := s.store.UpsertPresence(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "presence.updated", Data: presence})
	writeJSON(w, http.StatusOK, presence)
}

func (s *Server) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	var req CreateProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proposal, err := s.store.CreateProposal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "proposal.created", Data: proposal})
	writeJSON(w, http.StatusCreated, proposal)
}

func (s *Server) handleMergeProposal(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		actor = "owner"
	}
	document, update, err := s.store.MergeProposal(chi.URLParam(r, "id"), actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(update) > 0 {
		s.rooms.ForDocument(document.ID).Broadcast(yproto.BuildSyncUpdate(update), nil)
		s.subscribers.Publish(EventEnvelope{Type: "document.updated", Data: DocumentUpdateEvent{
			DocumentID: document.ID,
			Update:     base64.StdEncoding.EncodeToString(update),
			Path:       document.Path,
			UpdatedAt:  document.UpdatedAt,
			ActorID:    actor,
		}})
	}
	s.subscribers.Publish(EventEnvelope{Type: "proposal.merged", Data: document})
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "user-create",
	}
	user, err := s.store.CreateUser(req, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "user.created", Data: user})
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "user-update",
	}
	user, err := s.store.UpdateUser(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "user.updated", Data: user})
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "user-delete",
	}
	user, err := s.store.DeleteUser(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "user.deleted", Data: user})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
