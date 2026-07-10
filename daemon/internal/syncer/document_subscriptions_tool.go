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
// run's agent identity (daemon token + acting-agent), so the backend's ownership boundary applies.
//
// The backend serves bare {documentIds:[…]} — it stores the root-namespace CRDT as opaque truth and never
// decodes a document's path (there is no path column). The daemon is the only party that materializes paths,
// from the root-document projection it already holds to sync files to disk. So the gateway enriches here:
// it joins the backend's ids against that projection (zero extra network calls, the actual source of truth)
// and returns {documents:[{id,path}]} to the CLI. An id with no projection entry (a deleted/renamed doc
// mid-sync) degrades honestly to an empty path, which the CLI renders as an id-only line.

type subscribedDocument struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type toolDocumentSubscriptionsResponse struct {
	Documents []subscribedDocument `json:"documents"`
}

// backendDocumentSubscriptionsResponse is the bare id-only shape the backend endpoints serve; the gateway
// enriches it into toolDocumentSubscriptionsResponse before returning it to the CLI.
type backendDocumentSubscriptionsResponse struct {
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
	ids, err := s.doDocumentSubscriptionsRequest(req, "update document subscription")
	if err != nil {
		return nil, err
	}
	return s.enrichDocumentSubscriptions(run, ids), nil
}

func (s *Service) listDocumentSubscriptionsForRun(ctx context.Context, run *agentRun) (*toolDocumentSubscriptionsResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	req, err := s.newAgentBackendRequest(ctx, run, http.MethodGet, "/api/agents/"+url.PathEscape(run.AgentID)+"/document-subscriptions", nil)
	if err != nil {
		return nil, err
	}
	ids, err := s.doDocumentSubscriptionsRequest(req, "list document subscriptions")
	if err != nil {
		return nil, err
	}
	return s.enrichDocumentSubscriptions(run, ids), nil
}

func (s *Service) doDocumentSubscriptionsRequest(req *http.Request, label string) ([]string, error) {
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s failed: %s", label, res.Status)
	}
	var response backendDocumentSubscriptionsResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return response.DocumentIDs, nil
}

// enrichDocumentSubscriptions joins backend document ids against the root-namespace projection the daemon
// already holds, attaching each document's path. An id absent from the projection (deleted/renamed mid-sync)
// keeps an empty path — the CLI renders that as an id-only line rather than erroring or dropping it.
func (s *Service) enrichDocumentSubscriptions(run *agentRun, ids []string) *toolDocumentSubscriptionsResponse {
	return &toolDocumentSubscriptionsResponse{Documents: buildSubscribedDocuments(ids, s.subscriptionPathIndex(run))}
}

// buildSubscribedDocuments joins ids against a path index. An id absent from the index keeps an empty path
// (a deleted/renamed doc the projection no longer has) rather than being dropped — pure so the honesty case
// is unit-testable without a live runtime.
func buildSubscribedDocuments(ids []string, pathByID map[string]string) []subscribedDocument {
	documents := make([]subscribedDocument, 0, len(ids))
	for _, id := range ids {
		documents = append(documents, subscribedDocument{ID: id, Path: pathByID[id]})
	}
	return documents
}

// subscriptionPathIndex materializes an id→path map from the root projection the daemon holds. Best-effort:
// if the runtime or projection is unavailable, it returns an empty map and every line degrades to id-only.
func (s *Service) subscriptionPathIndex(run *agentRun) map[string]string {
	index := map[string]string{}
	runtime := s.runtimeForThreadAnchor(run)
	if runtime == nil {
		return index
	}
	documents, err := runtime.documentsFromRootProjection()
	if err != nil {
		return index
	}
	for _, document := range documents {
		if document != nil && document.ID != "" {
			index[document.ID] = document.Path
		}
	}
	return index
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
