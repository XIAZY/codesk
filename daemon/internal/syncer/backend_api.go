package syncer

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

func (c Config) workspaceAPIPath(path string) string {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.TrimSpace(c.WorkspaceID) == "" {
		return path
	}
	if strings.HasPrefix(path, "/api/") {
		return "/api/workspaces/" + url.PathEscape(c.WorkspaceID) + strings.TrimPrefix(path, "/api")
	}
	return path
}

func (c Config) workspaceWSPath(path string) string {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.TrimSpace(c.WorkspaceID) == "" {
		return path
	}
	if strings.HasPrefix(path, "/ws/documents/") {
		return "/ws/workspaces/" + url.PathEscape(c.WorkspaceID) + "/documents/" + strings.TrimPrefix(path, "/ws/documents/")
	}
	if path == "/ws" {
		return "/ws/workspaces/" + url.PathEscape(c.WorkspaceID)
	}
	return path
}

func (s *Service) newBackendRequest(ctx context.Context, method string, path string, body io.Reader) (*http.Request, error) {
	return newBackendRequestForConfig(ctx, s.cfg, method, path, body)
}

func (r *workspaceRuntime) newBackendRequest(ctx context.Context, method string, path string, body io.Reader) (*http.Request, error) {
	return newBackendRequestForConfig(ctx, r.cfg, method, path, body)
}

func newBackendRequestForConfig(ctx context.Context, cfg Config, method string, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, cfg.BackendURL+cfg.workspaceAPIPath(path), body)
	if err != nil {
		return nil, err
	}
	applyBackendAuth(req.Header, cfg, "")
	return req, nil
}

func (s *Service) newAgentBackendRequest(ctx context.Context, run *agentRun, method string, path string, body io.Reader) (*http.Request, error) {
	req, err := s.newBackendRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if run != nil {
		applyBackendAuth(req.Header, s.cfg, run.AgentID)
		if strings.TrimSpace(s.cfg.WorkspaceID) == "" {
			query := req.URL.Query()
			query.Set("actor", run.AgentID)
			query.Set("actor_type", "agent")
			req.URL.RawQuery = query.Encode()
		}
	}
	return req, nil
}

func (r *workspaceRuntime) newAgentBackendRequest(ctx context.Context, run *agentRun, method string, path string, body io.Reader) (*http.Request, error) {
	req, err := r.newBackendRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if run != nil {
		applyBackendAuth(req.Header, r.cfg, run.AgentID)
		if strings.TrimSpace(r.cfg.WorkspaceID) == "" {
			query := req.URL.Query()
			query.Set("actor", run.AgentID)
			query.Set("actor_type", "agent")
			req.URL.RawQuery = query.Encode()
		}
	}
	return req, nil
}

func applyBackendAuth(header http.Header, cfg Config, actingAgentID string) {
	if strings.TrimSpace(cfg.DaemonToken) != "" {
		header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.DaemonToken))
	}
	if strings.TrimSpace(actingAgentID) != "" {
		header.Set("X-Notty-Acting-Agent-ID", strings.TrimSpace(actingAgentID))
	}
}

func dialWorkspaceWebsocket(ctx context.Context, cfg Config, path string, query url.Values, actingAgentID string) (*websocket.Conn, *http.Response, error) {
	wsURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		return nil, nil, err
	}
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = cfg.workspaceWSPath(path)
	wsURL.RawQuery = query.Encode()
	headers := http.Header{}
	applyBackendAuth(headers, cfg, actingAgentID)
	return websocket.DefaultDialer.DialContext(ctx, wsURL.String(), headers)
}
