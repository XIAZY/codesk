package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPStreamTransportPostsWorkspaceScopedUpdateAndAuth(t *testing.T) {
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/workspaces/workspace-1/streams/root-stream/updates" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer daemon-token" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		if got := r.Header.Get("X-Notty-Acting-Agent-ID"); got != "agent-1" {
			t.Fatalf("unexpected acting agent header %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Fatalf("unexpected content type %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "update-bytes" {
			t.Fatalf("unexpected body %q", string(body))
		}
		_ = json.NewEncoder(w).Encode(postStreamUpdateResponse{Accepted: true, Applied: true, UpdateID: 42})
	}))
	defer server.Close()

	transport := HTTPStreamTransport{
		Config: Config{
			BackendURL:  server.URL,
			WorkspaceID: "workspace-1",
			DaemonToken: "daemon-token",
		},
		Client: server.Client(),
	}
	ack, err := transport.PostStreamUpdate(context.Background(), StreamOutboxRow{
		StreamID:    "root-stream",
		UpdateBytes: []byte("update-bytes"),
		ActorID:     "agent-1",
		ActorType:   "agent",
	})
	if err != nil {
		t.Fatalf("post stream update: %v", err)
	}
	if !sawRequest {
		t.Fatal("expected request")
	}
	if ack.UpdateID != 42 {
		t.Fatalf("unexpected ack %#v", ack)
	}
}

func TestHTTPStreamTransportAddsLegacyActorQueryWithoutDaemonToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/streams/content-stream/updates" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("actor"); got != "owner" {
			t.Fatalf("unexpected actor query %q", got)
		}
		if got := r.URL.Query().Get("actor_type"); got != "human" {
			t.Fatalf("unexpected actor_type query %q", got)
		}
		_ = json.NewEncoder(w).Encode(postStreamUpdateResponse{Accepted: true, UpdateID: 7})
	}))
	defer server.Close()

	ack, err := (HTTPStreamTransport{
		Config: Config{BackendURL: server.URL, AgentID: "daemon-agent"},
		Client: server.Client(),
	}).PostStreamUpdate(context.Background(), StreamOutboxRow{
		StreamID:    "content-stream",
		UpdateBytes: []byte("bytes"),
		ActorID:     "owner",
		ActorType:   "human",
	})
	if err != nil {
		t.Fatalf("post stream update: %v", err)
	}
	if ack.UpdateID != 7 {
		t.Fatalf("unexpected ack %#v", ack)
	}
}

func TestHTTPStreamTransportReturnsBackendStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := (HTTPStreamTransport{
		Config: Config{BackendURL: server.URL},
		Client: server.Client(),
	}).PostStreamUpdate(context.Background(), StreamOutboxRow{
		StreamID:    "root-stream",
		UpdateBytes: []byte("bytes"),
	})
	var statusErr *backendStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected backendStatusError, got %T %[1]v", err)
	}
	if statusErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected status error %#v", statusErr)
	}
}

func TestHTTPStreamTransportRejectsUnacceptedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(postStreamUpdateResponse{Accepted: false})
	}))
	defer server.Close()

	_, err := (HTTPStreamTransport{
		Config: Config{BackendURL: server.URL},
		Client: server.Client(),
	}).PostStreamUpdate(context.Background(), StreamOutboxRow{
		StreamID:    "root-stream",
		UpdateBytes: []byte("bytes"),
	})
	if err == nil || err.Error() != "stream update was not accepted" {
		t.Fatalf("expected unaccepted error, got %v", err)
	}
}
