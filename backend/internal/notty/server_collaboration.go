package notty

import (
	"encoding/json"
	"net/http"
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
