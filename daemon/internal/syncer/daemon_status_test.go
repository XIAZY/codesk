package syncer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"testing"
)

func TestDaemonStatusReporterSendsRuntimeDetections(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotPayload daemonStatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotPayload); err != nil {
			t.Fatalf("decode daemon status payload: %v", err)
		}
		if r.Method != http.MethodPatch {
			t.Fatalf("expected PATCH, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		BackendURL:         server.URL,
		WorkspaceID:        "workspace:test",
		DaemonToken:        "daemon_token",
		DaemonVersion:      "0.62.0",
		AgentWorkspaceRoot: t.TempDir(),
	}
	reporter := newDaemonStatusReporter(cfg, server.Client())
	detections := []RuntimeDetection{{
		Kind:      RuntimeCodex,
		Available: true,
		Version:   "codex 0.1.0",
		Path:      "/usr/local/bin/codex",
	}}

	if err := reporter.Report(context.Background(), detections); err != nil {
		t.Fatalf("report daemon status: %v", err)
	}
	wantPath := "/api/workspaces/" + url.PathEscape(cfg.WorkspaceID) + "/daemon/status"
	if gotPath != wantPath {
		t.Fatalf("expected path %s, got %s", wantPath, gotPath)
	}
	if gotAuth != "Bearer daemon_token" {
		t.Fatalf("expected daemon bearer auth, got %q", gotAuth)
	}
	if gotPayload.Version != "0.62.0" || gotPayload.OS != runtime.GOOS || gotPayload.Arch != runtime.GOARCH {
		t.Fatalf("unexpected daemon status payload: %#v", gotPayload)
	}
	if len(gotPayload.Runtimes) != 1 || gotPayload.Runtimes[0].Kind != RuntimeCodex || !gotPayload.Runtimes[0].Available {
		t.Fatalf("unexpected runtime detections: %#v", gotPayload.Runtimes)
	}
}
