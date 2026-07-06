package notty

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	state := s.requestStore(r).Snapshot()
	currentUserID := ""
	currentDaemonID := ""
	currentMembershipRole := ""
	auth, _ := authFromContext(r.Context())
	if auth != nil {
		currentUserID = auth.UserID
		currentDaemonID = auth.DaemonID
		currentMembershipRole = auth.MembershipRole
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workspaceId":           state.WorkspaceID,
		"rootDocumentId":        state.RootDocumentID,
		"currentUserId":         currentUserID,
		"currentDaemonId":       currentDaemonID,
		"currentMembershipRole": currentMembershipRole,
		"name":                  state.Name,
		"users":                 SortedUsers(state),
		"daemons":               s.daemonsForWorkspace(r, state),
		"agents":                visibleAgentsForAuth(state, auth),
		"agentRuns":             SortedWorkspaceAgentRuns(state),
		"threads":               SortedThreads(state),
		"agentEvents":           s.agentEventsForWorkspace(r, state),
		"presences":             s.presencesForWorkspace(r, state),
		"activities":            state.Activities,
		"updatedAt":             state.UpdatedAt,
	})
}

func visibleAgentsForAuth(state WorkspaceState, auth *AuthContext) []*Agent {
	agents := SortedAgents(state)
	if auth == nil || auth.PrincipalKind != "daemon" || auth.DaemonID == "" {
		return agents
	}
	filtered := make([]*Agent, 0, len(agents))
	for _, agent := range agents {
		if agent != nil && agent.DaemonID == auth.DaemonID {
			filtered = append(filtered, agent)
		}
	}
	return filtered
}

func (s *Server) agentEventsForWorkspace(r *http.Request, state WorkspaceState) []*AgentEvent {
	if s.sqlDB() != nil {
		if events, err := listAllAgentEventsPostgres(s.sqlDB(), state.WorkspaceID); err == nil {
			return events
		}
	}
	return nil
}

func (s *Server) presencesForWorkspace(r *http.Request, state WorkspaceState) []*Presence {
	if s.sqlDB() != nil {
		if presences := listPresencesPostgres(s.sqlDB(), state.WorkspaceID); presences != nil {
			return presences
		}
	}
	return nil
}

func (s *Server) daemonsForWorkspace(r *http.Request, state WorkspaceState) []*Daemon {
	if s.sqlDB() != nil {
		if daemons, err := listDaemons(s.sqlDB(), s.requestWorkspaceID(r)); err == nil {
			return daemons
		}
	}
	return SortedDaemons(state)
}

func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	channel, unsubscribe := s.requestBroker(r).Subscribe()
	defer unsubscribe()

	snapshot := s.requestStore(r).Snapshot()
	auth, _ := authFromContext(r.Context())
	currentMembershipRole := ""
	if auth != nil {
		currentMembershipRole = auth.MembershipRole
	}
	if err := conn.WriteJSON(EventEnvelope{
		Type: "workspace.snapshot",
		Data: map[string]interface{}{
			"rootDocumentId":        snapshot.RootDocumentID,
			"currentMembershipRole": currentMembershipRole,
			"users":                 SortedUsers(snapshot),
			"daemons":               s.daemonsForWorkspace(r, snapshot),
			"agents":                visibleAgentsForAuth(snapshot, auth),
			"agentRuns":             SortedWorkspaceAgentRuns(snapshot),
			"threads":               SortedThreads(snapshot),
			"agentEvents":           s.agentEventsForWorkspace(r, snapshot),
			"presences":             s.presencesForWorkspace(r, snapshot),
			"activities":            snapshot.Activities,
		},
	}); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case event := <-channel:
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}
