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

	return router
}
