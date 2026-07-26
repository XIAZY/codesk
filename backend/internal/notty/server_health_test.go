package notty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"notty/backend/internal/buildinfo"
)

func TestHealthzReportsBuildIdentity(t *testing.T) {
	server := NewServer(Config{JWTSecret: "test-secret"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	server.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want \"ok\"", body["status"])
	}
	// commit + builtAt must be present so a deploy is verifiable with one curl.
	// Under `go test` (no ldflags) they carry the "dev"/"unknown" fallback — never
	// empty, so the field is always meaningful.
	if body["commit"] == "" || body["commit"] != buildinfo.Commit {
		t.Errorf("commit = %q, want %q (buildinfo.Commit)", body["commit"], buildinfo.Commit)
	}
	if body["builtAt"] == "" || body["builtAt"] != buildinfo.Time {
		t.Errorf("builtAt = %q, want %q (buildinfo.Time)", body["builtAt"], buildinfo.Time)
	}
}
