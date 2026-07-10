package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

func (s *Service) startToolGateway() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-tools/list-documents", s.handleListDocumentsTool)
	mux.HandleFunc("/agent-tools/get-document-by-path", s.handleGetDocumentByPathTool)
	mux.HandleFunc("/agent-tools/get-thread", s.handleGetThreadTool)
	mux.HandleFunc("/agent-tools/list-threads-for-document", s.handleListThreadsForDocumentTool)
	mux.HandleFunc("/agent-tools/list-notifications", s.handleListNotificationsTool)
	mux.HandleFunc("/agent-tools/get-notification", s.handleGetNotificationTool)
	mux.HandleFunc("/agent-tools/complete-notification", s.handleCompleteNotificationTool)
	mux.HandleFunc("/agent-tools/dismiss-notification", s.handleDismissNotificationTool)
	mux.HandleFunc("/agent-tools/list-inbox", s.handleListInboxTool)
	mux.HandleFunc("/agent-tools/get-inbox-item", s.handleGetInboxItemTool)
	mux.HandleFunc("/agent-tools/complete-inbox-item", s.handleCompleteInboxItemTool)
	mux.HandleFunc("/agent-tools/dismiss-inbox-item", s.handleDismissInboxItemTool)
	mux.HandleFunc("/agent-tools/diff-document", s.handleDiffDocumentTool)
	mux.HandleFunc("/agent-tools/mark-document-viewed", s.handleMarkDocumentViewedTool)
	mux.HandleFunc("/agent-tools/create-thread", s.handleCreateThreadTool)
	mux.HandleFunc("/agent-tools/reply-thread", s.handleReplyThreadTool)
	mux.HandleFunc("/agent-tools/subscribe-document", s.handleSubscribeDocumentTool)
	mux.HandleFunc("/agent-tools/unsubscribe-document", s.handleUnsubscribeDocumentTool)
	mux.HandleFunc("/agent-tools/list-subscriptions", s.handleListDocumentSubscriptionsTool)

	server := &http.Server{Handler: mux}
	parsedAddr := strings.TrimSpace(strings.TrimPrefix(s.cfg.AgentToolBaseURL, "http://"))
	parsedAddr = strings.TrimSpace(strings.TrimPrefix(parsedAddr, "https://"))
	if parsedAddr == "" || strings.Contains(parsedAddr, "/") {
		return nil, errors.New("invalid NOTTY_AGENT_TOOL_BASE_URL")
	}
	listener, err := net.Listen("tcp", parsedAddr)
	if err != nil {
		return nil, err
	}
	go func() {
		_ = server.Serve(listener)
	}()
	return server, nil
}

func (s *Service) handleCreateThreadTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var payload createThreadPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := s.createThreadAsRun(r.Context(), run, payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusCreated, response)
}

func (s *Service) handleListDocumentsTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.listDocumentsForRun(r.Context(), run)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleGetDocumentByPathTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := r.URL.Query().Get("path")
	response, err := s.getDocumentByPathForRun(r.Context(), run, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleGetThreadTool(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeToolRequest(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	threadID := r.URL.Query().Get("thread_id")
	response, err := s.getThreadForRun(r.Context(), threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleListThreadsForDocumentTool(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeToolRequest(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	documentID := r.URL.Query().Get("document_id")
	response, err := s.listThreadsForDocumentForRun(r.Context(), documentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleReplyThreadTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var payload struct {
		ThreadID string `json:"threadId"`
		Body     string `json:"body"`
		Kind     string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := s.replyThreadAsRun(r.Context(), run, payload.ThreadID, replyThreadPayload{
		Body: payload.Body,
		Kind: payload.Kind,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusCreated, response)
}

func (s *Service) handleListNotificationsTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.listNotificationsForRun(r.Context(), run)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleListInboxTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.listInboxForRun(r.Context(), run, r.URL.Query().Get("box"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleGetNotificationTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.getNotificationForRun(r.Context(), run, r.URL.Query().Get("notification_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleGetInboxItemTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.getInboxItemForRun(r.Context(), run, r.URL.Query().Get("item_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleCompleteNotificationTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.updateNotificationStatusForRun(r.Context(), run, r.URL.Query().Get("notification_id"), "completed")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleCompleteInboxItemTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.updateInboxItemStatusForRun(r.Context(), run, r.URL.Query().Get("item_id"), "completed")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleDismissNotificationTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.updateNotificationStatusForRun(r.Context(), run, r.URL.Query().Get("notification_id"), "dismissed")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleDismissInboxItemTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.updateInboxItemStatusForRun(r.Context(), run, r.URL.Query().Get("item_id"), "dismissed")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleDiffDocumentTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.diffDocumentForRun(r.Context(), run, r.URL.Query().Get("document_id"), r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleMarkDocumentViewedTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.markDocumentViewedForRun(r.Context(), run, r.URL.Query().Get("document_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) authorizeToolRequest(r *http.Request) (*agentRun, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		return nil, false
	}
	run := s.sessions.agentByToolToken(token)
	if run == nil {
		return nil, false
	}
	return run, true
}

func writeToolJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func shutdownToolGateway(ctx context.Context, server *http.Server) error {
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}
