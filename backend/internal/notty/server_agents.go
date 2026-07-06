package notty

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) requireHumanPrincipal(w http.ResponseWriter, r *http.Request) bool {
	auth, ok := authFromContext(r.Context())
	if ok && auth.PrincipalKind == "human" {
		return true
	}
	writeError(w, http.StatusForbidden, "human authentication is required")
	return false
}

func (s *Server) requireAgentEndpointAccess(w http.ResponseWriter, r *http.Request, agentID string) bool {
	auth, ok := authFromContext(r.Context())
	if !ok || auth == nil || auth.PrincipalKind == "human" {
		return true
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "agent id is required")
		return false
	}
	if auth.PrincipalKind == "agent" {
		if auth.ActingAgentID == agentID {
			return true
		}
		writeError(w, http.StatusForbidden, "agent access denied")
		return false
	}
	if auth.PrincipalKind == "daemon" {
		snapshot := s.requestStore(r).Snapshot()
		agent := snapshot.Agents[agentID]
		if agent != nil && agent.DaemonID == auth.DaemonID {
			return true
		}
		writeError(w, http.StatusForbidden, "daemon cannot access this agent")
		return false
	}
	writeError(w, http.StatusForbidden, "agent access denied")
	return false
}

func (s *Server) requireAgentEventEndpointAccess(w http.ResponseWriter, r *http.Request, eventID string) bool {
	auth, ok := authFromContext(r.Context())
	if !ok || auth == nil || auth.PrincipalKind == "human" {
		return true
	}
	eventID = strings.TrimSpace(eventID)
	store := s.requestStore(r)
	if event, err := getAgentEventPostgres(store.db, store.state.WorkspaceID, eventID); err == nil && event != nil {
		return s.requireAgentEndpointAccess(w, r, event.AgentID)
	}
	if spec, ok := parseSyntheticDocumentInboxID(eventID); ok {
		return s.requireAgentEndpointAccess(w, r, spec.AgentID)
	}
	return true
}

func (s *Server) requireAgentRunEndpointAccess(w http.ResponseWriter, r *http.Request, runID string) bool {
	auth, ok := authFromContext(r.Context())
	if !ok || auth == nil || auth.PrincipalKind == "human" {
		return true
	}
	runID = strings.TrimSpace(runID)
	snapshot := s.requestStore(r).Snapshot()
	if run := snapshot.AgentRuns[runID]; run != nil {
		return s.requireAgentEndpointAccess(w, r, run.AgentID)
	}
	return true
}

func (s *Server) handleClaimAgentEvent(w http.ResponseWriter, r *http.Request) {
	var req ClaimAgentEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		req.AgentID = r.URL.Query().Get("actor")
		if auth, ok := authFromContext(r.Context()); ok && auth.ActingAgentID != "" {
			req.AgentID = auth.ActingAgentID
		}
	}
	if strings.TrimSpace(req.ClaimedBy) == "" {
		if auth, ok := authFromContext(r.Context()); ok {
			if auth.PrincipalKind == "agent" || auth.PrincipalKind == "daemon" {
				req.ClaimedBy = auth.PrincipalID
			}
		}
	}
	if !s.requireAgentEndpointAccess(w, r, req.AgentID) {
		return
	}
	event, err := s.requestStore(r).ClaimAgentEvent(req)
	if err != nil {
		if err == ErrNotFound {
			writeJSON(w, http.StatusOK, map[string]any{"event": nil})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.event.updated", Data: event})
	writeJSON(w, http.StatusOK, map[string]any{"event": event})
}

func (s *Server) handleUpdateAgentEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEventEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	var req UpdateAgentEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-event-update", "system", "system")
	event, err := s.requestStore(r).UpdateAgentEvent(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.event.updated", Data: event})
	writeJSON(w, http.StatusOK, event)
}

func (s *Server) handleAgentNotifications(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	statuses := r.URL.Query()["status"]
	notifications, err := s.requestStore(r).ListAgentNotifications(chi.URLParam(r, "id"), statuses...)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": notifications})
}

func (s *Server) handleAgentInbox(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	statuses := r.URL.Query()["status"]
	box := r.URL.Query().Get("box")
	items, err := s.requestStore(r).ListAgentInbox(chi.URLParam(r, "id"), box, statuses...)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleAgentNotification(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEventEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	notification, err := s.requestStore(r).GetAgentNotification(chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notification": notification})
}

func (s *Server) handleAgentInboxItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEventEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	item, err := s.requestStore(r).GetAgentInboxItem(chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (s *Server) handleUpdateAgentNotification(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEventEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	var req UpdateAgentNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-notification-update", "system", "system")
	notification, err := s.requestStore(r).UpdateAgentNotification(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.event.updated", Data: notification})
	writeJSON(w, http.StatusOK, map[string]any{"notification": notification})
}

func (s *Server) handleUpdateAgentInboxItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEventEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	var req UpdateAgentNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-inbox-update", "system", "system")
	item, err := s.requestStore(r).UpdateAgentInboxItem(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.event.updated", Data: item})
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (s *Server) handleAgentDocumentDiff(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	diff, err := s.requestStore(r).DiffDocument(
		chi.URLParam(r, "id"),
		chi.URLParam(r, "documentID"),
		r.URL.Query().Get("from"),
		r.URL.Query().Get("to"),
	)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		} else if errors.Is(err, ErrDocumentDiffTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": diff})
}

func (s *Server) handleMarkAgentDocumentViewed(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	var req MarkDocumentViewedRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	view, err := s.requestStore(r).MarkDocumentViewed(chi.URLParam(r, "id"), chi.URLParam(r, "documentID"), req)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"view": view})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	if err := requirePermission(auth, ActionManageAgents); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var req CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DaemonID) == "" {
		writeError(w, http.StatusBadRequest, "daemon id is required")
		return
	}
	meta := operationMetaFromAuth(auth, "agent-create", "system", "system")
	agent, err := s.requestStore(r).CreateAgent(req, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.created", Data: agent})
	writeJSON(w, http.StatusCreated, agent)
}

func (s *Server) handleCreateDaemonAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	if err := requirePermission(auth, ActionManageAgents); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var req CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.DaemonID = chi.URLParam(r, "daemonID")
	meta := operationMetaFromAuth(auth, "daemon-agent-create", "system", "system")
	agent, err := s.requestStore(r).CreateAgent(req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.created", Data: agent})
	writeJSON(w, http.StatusCreated, agent)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	if err := requirePermission(auth, ActionManageAgents); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var req UpdateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := operationMetaFromAuth(auth, "agent-update", "system", "system")
	agent, err := s.requestStore(r).UpdateAgent(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleUpdateAgentSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	var req UpdateAgentSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-session-update", "system", "system")
	agent, err := s.requestStore(r).UpdateAgentSession(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	if err := requirePermission(auth, ActionManageAgents); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	meta := operationMetaFromAuth(auth, "agent-delete", "system", "system")
	agent, err := s.requestStore(r).DeleteAgent(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.deleted", Data: agent})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleStartAgentRunForAgent(w http.ResponseWriter, r *http.Request) {
	var req StartAgentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.AgentID = chi.URLParam(r, "id")
	s.handleStartAgentRunRequest(w, r, req)
}

func (s *Server) handleStartAgentRun(w http.ResponseWriter, r *http.Request) {
	var req StartAgentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.handleStartAgentRunRequest(w, r, req)
}

func (s *Server) handleStartAgentRunRequest(w http.ResponseWriter, r *http.Request, req StartAgentRunRequest) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-run-create", "system", "system")
	meta.Trigger = "agent launch"
	agent, run, err := s.requestStore(r).StartAgentRun(req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.run.updated", Data: cloneAgentRunForWorkspace(run)})
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"agent": agent,
		"run":   run,
	})
}

func (s *Server) handleUpdateAgentRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentRunEndpointAccess(w, r, chi.URLParam(r, "id")) {
		return
	}
	var req UpdateAgentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-supervisor", "system", "system")
	meta.Source = "daemon"
	meta.Trigger = "process status"
	run, agent, err := s.requestStore(r).UpdateAgentRun(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.run.updated", Data: cloneAgentRunForWorkspace(run)})
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleStopAgentRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	meta := operationMetaFromAuth(auth, "agent-run-stop", "system", "system")
	meta.Trigger = "user stop"
	run, err := s.requestStore(r).StopAgentRun(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "agent.run.updated", Data: cloneAgentRunForWorkspace(run)})
	writeJSON(w, http.StatusOK, run)
}
