package notty

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleClaimAgentEvent(w http.ResponseWriter, r *http.Request) {
	var req ClaimAgentEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		req.AgentID = actorFromRequest(r, "")
	}
	event, err := s.store.ClaimAgentEvent(req)
	if err != nil {
		if err == ErrNotFound {
			writeJSON(w, http.StatusOK, map[string]any{"event": nil})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.event.updated", Data: event})
	writeJSON(w, http.StatusOK, map[string]any{"event": event})
}

func (s *Server) handleUpdateAgentEvent(w http.ResponseWriter, r *http.Request) {
	var req UpdateAgentEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "daemon_agent"),
		ActorType: actorTypeFromRequest(r, "agent"),
		Source:    "api",
		Tool:      "agent-event-update",
	}
	event, err := s.store.UpdateAgentEvent(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.event.updated", Data: event})
	writeJSON(w, http.StatusOK, event)
}

func (s *Server) handleAgentNotifications(w http.ResponseWriter, r *http.Request) {
	statuses := r.URL.Query()["status"]
	notifications, err := s.store.ListAgentNotifications(chi.URLParam(r, "id"), statuses...)
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
	statuses := r.URL.Query()["status"]
	box := r.URL.Query().Get("box")
	items, err := s.store.ListAgentInbox(chi.URLParam(r, "id"), box, statuses...)
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
	notification, err := s.store.GetAgentNotification(chi.URLParam(r, "id"))
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
	item, err := s.store.GetAgentInboxItem(chi.URLParam(r, "id"))
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
	var req UpdateAgentNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "daemon_agent"),
		ActorType: actorTypeFromRequest(r, "agent"),
		Source:    "api",
		Tool:      "agent-notification-update",
	}
	notification, err := s.store.UpdateAgentNotification(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.event.updated", Data: notification})
	writeJSON(w, http.StatusOK, map[string]any{"notification": notification})
}

func (s *Server) handleUpdateAgentInboxItem(w http.ResponseWriter, r *http.Request) {
	var req UpdateAgentNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "daemon_agent"),
		ActorType: actorTypeFromRequest(r, "agent"),
		Source:    "api",
		Tool:      "agent-inbox-update",
	}
	item, err := s.store.UpdateAgentInboxItem(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.event.updated", Data: item})
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (s *Server) handleAgentDocumentDiff(w http.ResponseWriter, r *http.Request) {
	diff, err := s.store.DiffDocument(
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
	var req MarkDocumentViewedRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	view, err := s.store.MarkDocumentViewed(chi.URLParam(r, "id"), chi.URLParam(r, "documentID"), req)
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
	var req CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "agent-create",
	}
	agent, err := s.store.CreateAgent(req, meta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.created", Data: agent})
	writeJSON(w, http.StatusCreated, agent)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	var req UpdateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "agent-update",
	}
	agent, err := s.store.UpdateAgent(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleUpdateAgentSession(w http.ResponseWriter, r *http.Request) {
	var req UpdateAgentSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "daemon_agent"),
		ActorType: actorTypeFromRequest(r, "agent"),
		Source:    "api",
		Tool:      "agent-session-update",
	}
	agent, err := s.store.UpdateAgentSession(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "agent-delete",
	}
	agent, err := s.store.DeleteAgent(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.deleted", Data: agent})
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
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "agent-run-create",
		Trigger:   "agent launch",
	}
	agent, run, err := s.store.StartAgentRun(req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	s.subscribers.Publish(EventEnvelope{Type: "agent.run.updated", Data: cloneAgentRunForWorkspace(run)})
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"agent": agent,
		"run":   run,
	})
}

func (s *Server) handleUpdateAgentRun(w http.ResponseWriter, r *http.Request) {
	var req UpdateAgentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "daemon"),
		ActorType: actorTypeFromRequest(r, "agent"),
		Source:    "daemon",
		Tool:      "agent-supervisor",
		Trigger:   "process status",
	}
	run, agent, err := s.store.UpdateAgentRun(chi.URLParam(r, "id"), req, meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.updated", Data: agent})
	s.subscribers.Publish(EventEnvelope{Type: "agent.run.updated", Data: cloneAgentRunForWorkspace(run)})
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleStopAgentRun(w http.ResponseWriter, r *http.Request) {
	meta := OperationMeta{
		ActorID:   actorFromRequest(r, "owner"),
		ActorType: actorTypeFromRequest(r, "human"),
		Source:    "api",
		Tool:      "agent-run-stop",
		Trigger:   "user stop",
	}
	run, err := s.store.StopAgentRun(chi.URLParam(r, "id"), meta)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.subscribers.Publish(EventEnvelope{Type: "agent.run.updated", Data: cloneAgentRunForWorkspace(run)})
	writeJSON(w, http.StatusOK, run)
}
