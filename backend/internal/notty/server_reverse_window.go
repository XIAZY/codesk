package notty

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"notty/internal/yproto"
)

func (s *Server) handleOpenReverseWindow(w http.ResponseWriter, r *http.Request) {
	daemonID, scope, ok := reverseWindowOriginFromRequest(r)
	if !ok {
		writeError(w, http.StatusForbidden, "daemon or acting-agent authentication is required")
		return
	}
	var request OpenReverseWindowRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.OriginDaemonID = daemonID
	request.OriginScope = scope
	result, err := s.requestStore(r).OpenOrReplaceReverseWindow(r.Context(), request)
	if err != nil {
		writeError(w, reverseWindowErrorStatus(err), err.Error())
		return
	}
	s.broadcastReverseWindowRootUpdate(r, result.RootDocumentID, result.RootUpdate)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleConsumeReverseWindow(w http.ResponseWriter, r *http.Request) {
	daemonID, scope, ok := reverseWindowOriginFromRequest(r)
	if !ok {
		writeError(w, http.StatusForbidden, "daemon or acting-agent authentication is required")
		return
	}
	var request ConsumeReverseWindowRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.OriginDaemonID = daemonID
	request.OriginScope = scope
	result, err := s.requestStore(r).ConsumeReverseWindow(r.Context(), request)
	if err != nil {
		writeError(w, reverseWindowErrorStatus(err), err.Error())
		return
	}
	s.broadcastReverseWindowRootUpdate(r, result.RootDocumentID, result.RootUpdate)
	writeJSON(w, http.StatusOK, result)
}

func reverseWindowOriginFromRequest(r *http.Request) (string, string, bool) {
	auth, ok := authFromContext(r.Context())
	if !ok || strings.TrimSpace(auth.DaemonID) == "" {
		return "", "", false
	}
	switch auth.PrincipalKind {
	case "daemon":
		return auth.DaemonID, "primary", true
	case "agent":
		agentID := strings.TrimSpace(auth.ActingAgentID)
		if agentID == "" {
			return "", "", false
		}
		return auth.DaemonID, "agent:" + agentID, true
	default:
		return "", "", false
	}
}

func (s *Server) broadcastReverseWindowRootUpdate(r *http.Request, rootDocumentID string, update []byte) {
	if len(update) == 0 || strings.TrimSpace(rootDocumentID) == "" {
		return
	}
	room := s.rooms.ForDocument(s.requestWorkspaceID(r) + ":" + rootDocumentID)
	room.BroadcastSyncUpdate(rootDocumentID, yproto.BuildSyncUpdate(update), nil)
}

func reverseWindowErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrReverseWindowInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrReverseWindowExpired):
		return http.StatusGone
	case errors.Is(err, ErrReverseWindowFrontierNotReached):
		return http.StatusPreconditionFailed
	case errors.Is(err, ErrReverseWindowIdentityMismatch),
		errors.Is(err, ErrReverseWindowRootMismatch),
		errors.Is(err, ErrReverseWindowPathClaimed):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
