package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type postStreamUpdateResponse struct {
	Accepted    bool   `json:"accepted"`
	Applied     bool   `json:"applied"`
	UpdateID    int64  `json:"updateId"`
	StateVector string `json:"stateVector"`
}

type HTTPStreamTransport struct {
	Config Config
	Client *http.Client
}

func (t HTTPStreamTransport) PostStreamUpdate(ctx context.Context, row StreamOutboxRow) (StreamAck, error) {
	if len(row.UpdateBytes) == 0 {
		return StreamAck{}, errors.New("stream update is required")
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	path := "/api/streams/" + url.PathEscape(strings.TrimSpace(row.StreamID)) + "/updates"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Config.BackendURL+t.Config.workspaceAPIPath(path), bytes.NewReader(row.UpdateBytes))
	if err != nil {
		return StreamAck{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if strings.TrimSpace(row.ActorType) == "agent" && strings.TrimSpace(row.ActorID) != "" {
		applyBackendAuth(req.Header, t.Config, row.ActorID)
	} else {
		applyBackendAuth(req.Header, t.Config, "")
	}
	if strings.TrimSpace(t.Config.DaemonToken) == "" {
		query := req.URL.Query()
		query.Set("actor", firstNonEmptyText(row.ActorID, t.Config.AgentID))
		query.Set("actor_type", firstNonEmptyText(row.ActorType, "daemon"))
		req.URL.RawQuery = query.Encode()
	}

	res, err := client.Do(req)
	if err != nil {
		return StreamAck{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return StreamAck{}, &backendStatusError{
			Method:     req.Method,
			URL:        req.URL.String(),
			StatusCode: res.StatusCode,
			Body:       string(body),
		}
	}
	var response postStreamUpdateResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return StreamAck{}, err
	}
	if !response.Accepted {
		return StreamAck{}, errors.New("stream update was not accepted")
	}
	return StreamAck{UpdateID: response.UpdateID}, nil
}
