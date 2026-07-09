package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Agent-tool surface for document subscriptions (task #2). This is the RULED subscribe interface: agents
// opt in to a document's updates explicitly via the CLI. Each tool call is proxied to the backend with the
// run's agent identity (daemon token + acting-agent), so the backend's ownership boundary applies. All three
// return the same {documentIds:[…]} shape the backend serves.

type toolDocumentSubscriptionsResponse struct {
	DocumentIDs []string `json:"documentIds"`
}

func (s *Service) subscribeDocumentForRun(ctx context.Context, run *agentRun, documentID string) (*toolDocumentSubscriptionsResponse, error) {
	return s.mutateDocumentSubscriptionForRun(ctx, run, http.MethodPost, documentID)
}

func (s *Service) unsubscribeDocumentForRun(ctx context.Context, run *agentRun, documentID string) (*toolDocumentSubscriptionsResponse, error) {
	return s.mutateDocumentSubscriptionForRun(ctx, run, http.MethodDelete, documentID)
}

func (s *Service) mutateDocumentSubscriptionForRun(ctx context.Context, run *agentRun, method, documentID string) (*toolDocumentSubscriptionsResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	base := "/api/agents/" + url.PathEscape(run.AgentID) + "/document-subscriptions"
	var (
		path string
		body io.Reader
	)
	if method == http.MethodDelete {
		path = base + "/" + url.PathEscape(documentID)
	} else {
		path = base
		payload, err := json.Marshal(map[string]string{"documentId": documentID})
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(payload)
	}
	req, err := s.newAgentBackendRequest(ctx, run, method, path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.doDocumentSubscriptionsRequest(req, "update document subscription")
}

func (s *Service) listDocumentSubscriptionsForRun(ctx context.Context, run *agentRun) (*toolDocumentSubscriptionsResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	req, err := s.newAgentBackendRequest(ctx, run, http.MethodGet, "/api/agents/"+url.PathEscape(run.AgentID)+"/document-subscriptions", nil)
	if err != nil {
		return nil, err
	}
	return s.doDocumentSubscriptionsRequest(req, "list document subscriptions")
}

func (s *Service) doDocumentSubscriptionsRequest(req *http.Request, label string) (*toolDocumentSubscriptionsResponse, error) {
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s failed: %s", label, res.Status)
	}
	var response toolDocumentSubscriptionsResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) handleSubscribeDocumentTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.subscribeDocumentForRun(r.Context(), run, r.URL.Query().Get("document_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleUnsubscribeDocumentTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.unsubscribeDocumentForRun(r.Context(), run, r.URL.Query().Get("document_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}

func (s *Service) handleListDocumentSubscriptionsTool(w http.ResponseWriter, r *http.Request) {
	run, ok := s.authorizeToolRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	response, err := s.listDocumentSubscriptionsForRun(r.Context(), run)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeToolJSON(w, http.StatusOK, response)
}
