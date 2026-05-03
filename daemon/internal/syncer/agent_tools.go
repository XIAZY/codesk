package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type toolThreadMutationResponse struct {
	Thread  *thread        `json:"thread"`
	Message *threadMessage `json:"message"`
}

type toolDocumentsResponse struct {
	Documents []*document `json:"documents"`
}

type toolDocumentResponse struct {
	Document *document `json:"document"`
}

type toolThreadResponse struct {
	Thread *thread `json:"thread"`
}

type toolThreadsResponse struct {
	Threads []*thread `json:"threads"`
}

type toolNotificationResponse struct {
	Notification *agentEvent `json:"notification"`
}

type toolNotificationsResponse struct {
	Notifications []*agentEvent `json:"notifications"`
}

type toolInboxResponse struct {
	Items []*agentEvent `json:"items"`
}

type toolInboxItemResponse struct {
	Item *agentEvent `json:"item"`
}

type documentDiff struct {
	DocumentID   string             `json:"documentId"`
	FromUpdateID int64              `json:"fromUpdateId"`
	ToUpdateID   int64              `json:"toUpdateId"`
	FromContent  string             `json:"fromContent"`
	ToContent    string             `json:"toContent"`
	Unified      string             `json:"unified"`
	Hunks        []documentDiffHunk `json:"hunks"`
}

type documentDiffHunk struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

type toolDocumentDiffResponse struct {
	Diff *documentDiff `json:"diff"`
}

type toolDocumentViewResponse struct {
	View any `json:"view"`
}

func (s *Service) listDocumentsForRun(ctx context.Context) (*toolDocumentsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/workspace", nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("list documents failed: %s", res.Status)
	}
	var response toolDocumentsResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) getDocumentByPathForRun(ctx context.Context, path string) (*toolDocumentResponse, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("document path is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/documents/by-path?path="+url.QueryEscape(trimmed), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("get document by path failed: %s", res.Status)
	}
	var response toolDocumentResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) getThreadForRun(ctx context.Context, threadID string) (*toolThreadResponse, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("thread id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/threads/"+url.PathEscape(threadID), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("get thread failed: %s", res.Status)
	}
	var response toolThreadResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) listThreadsForDocumentForRun(ctx context.Context, documentID string) (*toolThreadsResponse, error) {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/documents/"+url.PathEscape(documentID)+"/threads", nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("list threads for document failed: %s", res.Status)
	}
	var response toolThreadsResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) listNotificationsForRun(ctx context.Context, run *agentRun) (*toolNotificationsResponse, error) {
	inbox, err := s.listInboxForRun(ctx, run, "for_me")
	if err != nil {
		return nil, err
	}
	return &toolNotificationsResponse{Notifications: inbox.Items}, nil
}

func (s *Service) listInboxForRun(ctx context.Context, run *agentRun, box string) (*toolInboxResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	return s.listInboxForAgent(ctx, run.AgentID, box)
}

func (s *Service) fetchPendingInboxForAgent(ctx context.Context, agentID string) ([]*agentEvent, []*agentEvent, error) {
	forMe, err := s.listInboxForAgent(ctx, agentID, "for_me")
	if err != nil {
		return nil, nil, err
	}
	general, err := s.listInboxForAgent(ctx, agentID, "general")
	if err != nil {
		return nil, nil, err
	}
	return forMe.Items, general.Items, nil
}

func (s *Service) listInboxForAgent(ctx context.Context, agentID string, box string) (*toolInboxResponse, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, fmt.Errorf("agent id is required")
	}
	box = strings.TrimSpace(box)
	if box == "" {
		box = "for_me"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/agents/"+url.PathEscape(agentID)+"/inbox?status=pending&box="+url.QueryEscape(box), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("list inbox failed: %s", res.Status)
	}
	var response toolInboxResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) getNotificationForRun(ctx context.Context, run *agentRun, notificationID string) (*toolNotificationResponse, error) {
	item, err := s.getInboxItemForRun(ctx, run, notificationID)
	if err != nil {
		return nil, err
	}
	return &toolNotificationResponse{Notification: item.Item}, nil
}

func (s *Service) getInboxItemForRun(ctx context.Context, run *agentRun, itemID string) (*toolInboxItemResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil, fmt.Errorf("item id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BackendURL+"/api/agent-inbox/"+url.PathEscape(itemID), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("get inbox item failed: %s", res.Status)
	}
	var response toolInboxItemResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	if response.Item == nil || response.Item.AgentID != run.AgentID {
		return nil, fmt.Errorf("inbox item does not belong to this agent")
	}
	return &response, nil
}

func (s *Service) updateNotificationStatusForRun(ctx context.Context, run *agentRun, notificationID string, status string) (*toolNotificationResponse, error) {
	item, err := s.updateInboxItemStatusForRun(ctx, run, notificationID, status)
	if err != nil {
		return nil, err
	}
	return &toolNotificationResponse{Notification: item.Item}, nil
}

func (s *Service) updateInboxItemStatusForRun(ctx context.Context, run *agentRun, itemID string, status string) (*toolInboxItemResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil, fmt.Errorf("item id is required")
	}
	status = strings.TrimSpace(status)
	if status == "" {
		return nil, fmt.Errorf("inbox item status is required")
	}
	body, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, s.cfg.BackendURL+"/api/agent-inbox/"+url.PathEscape(itemID)+"?actor="+url.QueryEscape(run.AgentID)+"&actor_type=agent", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("update inbox item failed: %s", res.Status)
	}
	var response toolInboxItemResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) diffDocumentForRun(ctx context.Context, run *agentRun, documentID string, from string, to string) (*toolDocumentDiffResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	query := url.Values{}
	if strings.TrimSpace(from) != "" {
		query.Set("from", strings.TrimSpace(from))
	}
	if strings.TrimSpace(to) != "" {
		query.Set("to", strings.TrimSpace(to))
	}
	endpoint := s.cfg.BackendURL + "/api/agents/" + url.PathEscape(run.AgentID) + "/documents/" + url.PathEscape(documentID) + "/diff"
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("diff document failed: %s", res.Status)
	}
	var response toolDocumentDiffResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) markDocumentViewedForRun(ctx context.Context, run *agentRun, documentID string) (*toolDocumentViewResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BackendURL+"/api/agents/"+url.PathEscape(run.AgentID)+"/documents/"+url.PathEscape(documentID)+"/viewed", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("mark document viewed failed: %s", res.Status)
	}
	var response toolDocumentViewResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) createThreadAsRun(ctx context.Context, run *agentRun, payload createThreadPayload) (*toolThreadMutationResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	prepared, err := s.prepareCreateThreadPayload(ctx, payload)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(prepared)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BackendURL+"/api/threads?actor="+url.QueryEscape(run.AgentID)+"&actor_type=agent", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("create thread failed: %s", res.Status)
	}
	var response toolThreadMutationResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (s *Service) replyThreadAsRun(ctx context.Context, run *agentRun, threadID string, payload replyThreadPayload) (*toolThreadMutationResponse, error) {
	if run == nil {
		return nil, fmt.Errorf("missing agent run context")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("thread id is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BackendURL+"/api/threads/"+threadID+"/messages?actor="+url.QueryEscape(run.AgentID)+"&actor_type=agent", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("reply thread failed: %s", res.Status)
	}
	var response toolThreadMutationResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}
