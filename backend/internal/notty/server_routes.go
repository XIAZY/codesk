package notty

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"notty/backend/internal/buildinfo"
)

func (s *Server) Routes() http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)
	router.Use(cors)

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Build identity (commit + build time) so a deploy can be verified with a
		// single unauthenticated curl instead of route-fingerprinting forensics.
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"commit":  buildinfo.Commit,
			"builtAt": buildinfo.Time,
		})
	})

	router.Post("/api/auth/register", s.handleRegister)
	router.Post("/api/auth/login", s.handleLogin)
	router.Post("/api/auth/verify-email", s.handleVerifyEmail)
	router.Post("/api/auth/resend-verification", s.handleResendVerification)
	router.Post("/api/auth/forgot-password", s.handleForgotPassword)
	router.Post("/api/auth/reset-password", s.handleResetPassword)
	router.Get("/api/invites/{token}", s.handleWorkspaceInvitePreview)
	router.Group(func(router chi.Router) {
		router.Use(s.requireHuman)
		router.Get("/api/auth/me", s.handleMe)
		router.Get("/api/workspaces", s.handleListWorkspaces)
		router.Post("/api/workspaces", s.handleCreateWorkspace)
		router.Post("/api/invites/{token}/accept", s.handleAcceptWorkspaceInvite)
	})

	router.Route("/api/workspaces/{workspaceID}", func(router chi.Router) {
		router.Use(s.requireWorkspace)
		router.Get("/workspace", s.handleWorkspace)
		router.Patch("/workspace", s.handleUpdateWorkspace)
		router.Delete("/", s.handleDeleteWorkspace)
		router.Patch("/last-accessed", s.handleUpdateLastAccessed)
		router.Get("/members", s.handleListWorkspaceMembers)
		router.Post("/members", s.handleAddWorkspaceMember)
		router.Post("/invites", s.handleCreateWorkspaceInvite)
		router.Get("/daemons", s.handleListDaemons)
		router.Post("/daemons", s.handleCreateDaemon)
		router.Patch("/daemon/status", s.handleUpdateDaemonStatus)
		router.Post("/daemons/{daemonID}/reinstall-token", s.handleCreateDaemonReinstallToken)
		router.Delete("/daemons/{daemonID}", s.handleDeleteDaemon)
		router.Post("/daemons/{daemonID}/agents", s.handleCreateDaemonAgent)
		router.Post("/documents", s.handleCreateDocument)
		router.Get("/documents/{id}/threads", s.handleDocumentThreads)
		router.Get("/documents/{id}/subscribers", s.handleDocumentSubscribers)
		router.Post("/agents", s.handleCreateAgent)
		router.Patch("/agents/{id}", s.handleUpdateAgent)
		router.Patch("/agents/{id}/session", s.handleUpdateAgentSession)
		router.Delete("/agents/{id}", s.handleDeleteAgent)
		router.Post("/agents/{id}/runs", s.handleStartAgentRunForAgent)
		router.Post("/threads", s.handleCreateThread)
		router.Get("/threads/{id}", s.handleThread)
		router.Patch("/threads/{id}/status", s.handleUpdateThreadStatus)
		router.Patch("/threads/{id}/anchor", s.handleUpdateThreadAnchor)
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
		router.Get("/agents/{id}/document-subscriptions", s.handleListAgentDocumentSubscriptions)
		router.Post("/agents/{id}/document-subscriptions", s.handleSubscribeAgentDocument)
		router.Delete("/agents/{id}/document-subscriptions/{documentID}", s.handleUnsubscribeAgentDocument)
	})
	router.Group(func(router chi.Router) {
		router.Use(s.requireWorkspace)
		router.Get("/ws/workspaces/{workspaceID}", s.handleWebsocket)
		router.Get("/ws/workspaces/{workspaceID}/documents/{id}", s.handleDocumentWebsocket)
		router.Get("/ws/workspaces/{workspaceID}/documents-sync", s.handleWorkspaceDocumentSyncWebsocket)
	})

	return router
}
