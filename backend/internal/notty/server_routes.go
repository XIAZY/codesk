package notty

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func (s *Server) Routes() http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)
	router.Use(cors)

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	router.Post("/api/auth/register", s.handleRegister)
	router.Post("/api/auth/login", s.handleLogin)
	router.Group(func(router chi.Router) {
		router.Use(s.requireHuman)
		router.Get("/api/auth/me", s.handleMe)
		router.Get("/api/workspaces", s.handleListWorkspaces)
		router.Post("/api/workspaces", s.handleCreateWorkspace)
	})

	router.Route("/api/workspaces/{workspaceID}", func(router chi.Router) {
		router.Use(s.requireWorkspace)
		router.Get("/workspace", s.handleWorkspace)
		router.Get("/members", s.handleListWorkspaceMembers)
		router.Post("/members", s.handleAddWorkspaceMember)
		router.Get("/daemons", s.handleListDaemons)
		router.Post("/daemons", s.handleCreateDaemon)
		router.Delete("/daemons/{daemonID}", s.handleDeleteDaemon)
		router.Post("/daemons/{daemonID}/agents", s.handleCreateDaemonAgent)
		router.Get("/documents/by-path", s.handleDocumentByPath)
		router.Post("/documents", s.handleCreateDocument)
		router.Get("/documents/{id}/threads", s.handleDocumentThreads)
		router.Patch("/documents/{id}", s.handleMoveDocument)
		router.Delete("/documents/{id}", s.handleDeleteDocument)
		router.Post("/users", s.handleCreateUser)
		router.Patch("/users/{id}", s.handleUpdateUser)
		router.Delete("/users/{id}", s.handleDeleteUser)
		router.Post("/agents", s.handleCreateAgent)
		router.Patch("/agents/{id}", s.handleUpdateAgent)
		router.Patch("/agents/{id}/session", s.handleUpdateAgentSession)
		router.Delete("/agents/{id}", s.handleDeleteAgent)
		router.Post("/agents/{id}/runs", s.handleStartAgentRunForAgent)
		router.Post("/threads", s.handleCreateThread)
		router.Get("/threads/{id}", s.handleThread)
		router.Post("/threads/{id}/messages", s.handleReplyThread)
		router.Post("/presence", s.handlePresence)
		router.Post("/agent-runs", s.handleStartAgentRun)
		router.Patch("/agent-runs/{id}", s.handleUpdateAgentRun)
		router.Post("/agent-runs/{id}/stop", s.handleStopAgentRun)
		router.Post("/agent-events/claim", s.handleClaimAgentEvent)
		router.Patch("/agent-events/{id}", s.handleUpdateAgentEvent)
		router.Get("/agents/{id}/notifications", s.handleAgentNotifications)
		router.Get("/agent-notifications/{id}", s.handleAgentNotification)
		router.Patch("/agent-notifications/{id}", s.handleUpdateAgentNotification)
		router.Get("/agents/{id}/inbox", s.handleAgentInbox)
		router.Get("/agent-inbox/{id}", s.handleAgentInboxItem)
		router.Patch("/agent-inbox/{id}", s.handleUpdateAgentInboxItem)
		router.Get("/agents/{id}/documents/{documentID}/diff", s.handleAgentDocumentDiff)
		router.Post("/agents/{id}/documents/{documentID}/viewed", s.handleMarkAgentDocumentViewed)
	})
	router.Group(func(router chi.Router) {
		router.Use(s.requireWorkspace)
		router.Get("/ws/workspaces/{workspaceID}", s.handleWebsocket)
		router.Get("/ws/workspaces/{workspaceID}/documents/{id}", s.handleDocumentWebsocket)
	})

	if !s.authEnabled() {
		router.Get("/api/workspace", s.handleWorkspace)
		router.Get("/api/documents/by-path", s.handleDocumentByPath)
		router.Post("/api/documents", s.handleCreateDocument)
		router.Get("/api/documents/{id}/threads", s.handleDocumentThreads)
		router.Patch("/api/documents/{id}", s.handleMoveDocument)
		router.Delete("/api/documents/{id}", s.handleDeleteDocument)
		router.Post("/api/users", s.handleCreateUser)
		router.Patch("/api/users/{id}", s.handleUpdateUser)
		router.Delete("/api/users/{id}", s.handleDeleteUser)
		router.Post("/api/agents", s.handleCreateAgent)
		router.Patch("/api/agents/{id}", s.handleUpdateAgent)
		router.Patch("/api/agents/{id}/session", s.handleUpdateAgentSession)
		router.Delete("/api/agents/{id}", s.handleDeleteAgent)
		router.Post("/api/agents/{id}/runs", s.handleStartAgentRunForAgent)
		router.Post("/api/threads", s.handleCreateThread)
		router.Get("/api/threads/{id}", s.handleThread)
		router.Post("/api/threads/{id}/messages", s.handleReplyThread)
		router.Post("/api/presence", s.handlePresence)
		router.Post("/api/agent-runs", s.handleStartAgentRun)
		router.Patch("/api/agent-runs/{id}", s.handleUpdateAgentRun)
		router.Post("/api/agent-runs/{id}/stop", s.handleStopAgentRun)
		router.Post("/api/agent-events/claim", s.handleClaimAgentEvent)
		router.Patch("/api/agent-events/{id}", s.handleUpdateAgentEvent)
		router.Get("/api/agents/{id}/notifications", s.handleAgentNotifications)
		router.Get("/api/agent-notifications/{id}", s.handleAgentNotification)
		router.Patch("/api/agent-notifications/{id}", s.handleUpdateAgentNotification)
		router.Get("/api/agents/{id}/inbox", s.handleAgentInbox)
		router.Get("/api/agent-inbox/{id}", s.handleAgentInboxItem)
		router.Patch("/api/agent-inbox/{id}", s.handleUpdateAgentInboxItem)
		router.Get("/api/agents/{id}/documents/{documentID}/diff", s.handleAgentDocumentDiff)
		router.Post("/api/agents/{id}/documents/{documentID}/viewed", s.handleMarkAgentDocumentViewed)
		router.Get("/ws", s.handleWebsocket)
		router.Get("/ws/documents/{id}", s.handleDocumentWebsocket)
	}

	return router
}
