package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"notty/daemon/internal/buildinfo"
)

type daemonStatusReporter struct {
	cfg    Config
	client *http.Client
}

type daemonStatusUpdate struct {
	Version  string             `json:"version,omitempty"`
	OS       string             `json:"os,omitempty"`
	Arch     string             `json:"arch,omitempty"`
	Runtimes []RuntimeDetection `json:"runtimes,omitempty"`
}

func newDaemonStatusReporter(cfg Config, client *http.Client) *daemonStatusReporter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &daemonStatusReporter{cfg: cfg, client: client}
}

func (r *daemonStatusReporter) Report(ctx context.Context, detections []RuntimeDetection) error {
	if r == nil {
		return nil
	}
	version, err := buildinfo.Require()
	if err != nil {
		return fmt.Errorf("report daemon status: %w", err)
	}
	payload := daemonStatusUpdate{
		Version:  version,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Runtimes: detections,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := newBackendRequestForConfig(ctx, r.cfg, http.MethodPatch, "/api/daemon/status", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("daemon status update failed with HTTP %d", res.StatusCode)
	}
	return nil
}
