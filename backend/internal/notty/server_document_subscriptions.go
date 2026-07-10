package notty

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Document subscription endpoints (task #2). Muted-by-default routing means an agent hears about a
// document's edits only if it has subscribed. These endpoints are agent-driven: a daemon calls them with
// its token + the X-Notty-Acting-Agent-ID header, and requireAgentEndpointAccess enforces the 4.2 boundary
// (a daemon may act only for an agent it owns — a cross-daemon or unknown agent is 403). Subscribe and
// unsubscribe are idempotent and ring no broadcast.
//
// Human-principal policy (Tom's ruling): these are agent-driven, but a human principal passes the SAME
// shared boundary every sibling agent endpoint uses — we deliberately do NOT special-case a human rejection
// here. AlphaToad's "agent-only for now" means no human UI is built on this, not that the API rejects
// humans; keeping the shared boundary avoids extra special-case code. An explicit owner-subscribe test row
// pins this as a chosen behavior.

// SubscribeDocumentRequest is the POST body for subscribing an agent to a document's updates.
type SubscribeDocumentRequest struct {
	DocumentID string `json:"documentId"`
}

func (s *Server) handleSubscribeAgentDocument(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	if !s.requireAgentEndpointAccess(w, r, agentID) {
		return
	}
	var req SubscribeDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DocumentID) == "" {
		writeError(w, http.StatusBadRequest, "documentId is required")
		return
	}
	if err := s.requestStore(r).SubscribeAgentDocument(agentID, req.DocumentID); err != nil {
		writeError(w, subscriptionErrorStatus(err), err.Error())
		return
	}
	s.writeAgentDocumentSubscriptions(w, r, agentID)
}

func (s *Server) handleUnsubscribeAgentDocument(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	if !s.requireAgentEndpointAccess(w, r, agentID) {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	if _, err := s.requestStore(r).UnsubscribeAgentDocument(agentID, documentID); err != nil {
		writeError(w, subscriptionErrorStatus(err), err.Error())
		return
	}
	s.writeAgentDocumentSubscriptions(w, r, agentID)
}

func (s *Server) handleListAgentDocumentSubscriptions(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	if !s.requireAgentEndpointAccess(w, r, agentID) {
		return
	}
	s.writeAgentDocumentSubscriptions(w, r, agentID)
}

// writeAgentDocumentSubscriptions writes the agent's current subscription list. All three endpoints return
// the same {documentIds:[…]} body (Tom's ruling) — subscribe/unsubscribe return the POST-CHANGE list, which
// is self-verifying (the caller sees the effect, not a bare ack) and saves a follow-up list call.
func (s *Server) writeAgentDocumentSubscriptions(w http.ResponseWriter, r *http.Request, agentID string) {
	documentIDs, err := s.requestStore(r).ListAgentDocumentSubscriptions(agentID)
	if err != nil {
		writeError(w, subscriptionErrorStatus(err), err.Error())
		return
	}
	if documentIDs == nil {
		documentIDs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"documentIds": documentIDs})
}

func subscriptionErrorStatus(err error) int {
	if err == ErrNotFound {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}
