package notty

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type workspaceView struct {
	WorkspaceID    string
	Name           string
	RootDocumentID string
	UpdatedAt      time.Time
	Users          []*User
	Daemons        []*Daemon
	Agents         []*Agent
	AgentRuns      []*AgentRun
	Threads        []*Thread
	AgentEvents    []*AgentEvent
	Presences      []*Presence
	Activities     []*ActivityEvent
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	currentUserID := ""
	currentDaemonID := ""
	currentMembershipRole := ""
	auth, _ := authFromContext(r.Context())
	if auth != nil {
		currentUserID = auth.UserID
		currentDaemonID = auth.DaemonID
		currentMembershipRole = auth.MembershipRole
	}
	view, err := loadWorkspaceView(r.Context(), s.sqlDB(), s.requestWorkspaceID(r), auth)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workspaceId":           view.WorkspaceID,
		"rootDocumentId":        view.RootDocumentID,
		"currentUserId":         currentUserID,
		"currentDaemonId":       currentDaemonID,
		"currentMembershipRole": currentMembershipRole,
		"name":                  view.Name,
		"users":                 view.Users,
		"daemons":               view.Daemons,
		"agents":                view.Agents,
		"agentRuns":             view.AgentRuns,
		"threads":               view.Threads,
		"agentEvents":           view.AgentEvents,
		"presences":             view.Presences,
		"activities":            view.Activities,
		"updatedAt":             view.UpdatedAt,
	})
}

func loadWorkspaceView(ctx context.Context, db *sql.DB, workspaceID string, auth *AuthContext) (*workspaceView, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	view := &workspaceView{}
	if err := tx.QueryRow(
		`SELECT id::text, name, root_document_id::text, updated_at
		   FROM workspaces
		  WHERE id = $1::uuid`,
		workspaceID,
	).Scan(&view.WorkspaceID, &view.Name, &view.RootDocumentID, &view.UpdatedAt); err != nil {
		return nil, err
	}
	if view.Users, err = listUsersPostgres(tx, workspaceID); err != nil {
		return nil, err
	}
	if view.Daemons, err = listDaemons(tx, workspaceID); err != nil {
		return nil, err
	}
	agents, err := listAgentsPostgres(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	view.Agents = visibleAgentsForAuth(agents, auth)
	agentRuns, err := listAgentRunsPostgres(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	view.AgentRuns = make([]*AgentRun, 0, len(agentRuns))
	for _, run := range agentRuns {
		view.AgentRuns = append(view.AgentRuns, cloneAgentRunForWorkspace(run))
	}
	if view.Threads, err = listThreadsPostgres(tx, workspaceID); err != nil {
		return nil, err
	}
	if view.AgentEvents, err = listAllAgentEventsPostgres(tx, workspaceID); err != nil {
		return nil, err
	}
	if view.Presences, err = listPresencesPostgres(tx, workspaceID); err != nil {
		return nil, err
	}
	if view.Activities, err = listActivitiesPostgres(tx, workspaceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return view, nil
}

func visibleAgentsForAuth(agents []*Agent, auth *AuthContext) []*Agent {
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

func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	channel, unsubscribe := s.requestBroker(r).Subscribe()
	defer unsubscribe()

	auth, _ := authFromContext(r.Context())
	currentMembershipRole := ""
	if auth != nil {
		currentMembershipRole = auth.MembershipRole
	}
	view, err := loadWorkspaceView(r.Context(), s.sqlDB(), s.requestWorkspaceID(r), auth)
	if err != nil {
		log.Printf("load workspace websocket snapshot: %v", err)
		return
	}
	if err := conn.WriteJSON(EventEnvelope{
		Type: "workspace.snapshot",
		Data: map[string]interface{}{
			"rootDocumentId":        view.RootDocumentID,
			"currentMembershipRole": currentMembershipRole,
			"users":                 view.Users,
			"daemons":               view.Daemons,
			"agents":                view.Agents,
			"agentRuns":             view.AgentRuns,
			"threads":               view.Threads,
			"agentEvents":           view.AgentEvents,
			"presences":             view.Presences,
			"activities":            view.Activities,
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
