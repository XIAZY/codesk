package notty

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	state := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workspaceId": state.WorkspaceID,
		"name":        state.Name,
		"documents":   SortedSyncDocuments(state),
		"users":       SortedUsers(state),
		"agents":      SortedAgents(state),
		"agentRuns":   SortedWorkspaceAgentRuns(state),
		"threads":     SortedThreads(state),
		"agentEvents": SortedAgentEvents(state),
		"presences":   state.Presences,
		"proposals":   state.Proposals,
		"activities":  state.Activities,
		"updatedAt":   state.UpdatedAt,
	})
}

func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	channel, unsubscribe := s.subscribers.Subscribe()
	defer unsubscribe()

	snapshot := s.store.Snapshot()
	if err := conn.WriteJSON(EventEnvelope{
		Type: "workspace.snapshot",
		Data: map[string]interface{}{
			"documents":   SortedSyncDocuments(snapshot),
			"users":       SortedUsers(snapshot),
			"agents":      SortedAgents(snapshot),
			"agentRuns":   SortedWorkspaceAgentRuns(snapshot),
			"threads":     SortedThreads(snapshot),
			"agentEvents": SortedAgentEvents(snapshot),
			"presences":   snapshot.Presences,
			"proposals":   snapshot.Proposals,
			"activities":  snapshot.Activities,
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
