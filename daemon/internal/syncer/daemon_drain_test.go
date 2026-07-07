package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// D1: a rejected credential (401/403) or a drained/deprovisioned daemon (410 Gone) is terminal —
// the daemon must stop rather than retry forever. Anything else stays retryable.
func TestIsTerminalAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unauthorized", &backendStatusError{StatusCode: http.StatusUnauthorized}, true},
		{"forbidden", &backendStatusError{StatusCode: http.StatusForbidden}, true},
		{"gone (drained)", &backendStatusError{StatusCode: http.StatusGone}, true},
		{"wrapped gone", fmt.Errorf("refresh: %w", &backendStatusError{StatusCode: http.StatusGone}), true},
		{"server error is transient", &backendStatusError{StatusCode: http.StatusInternalServerError}, false},
		{"not found is transient", &backendStatusError{StatusCode: http.StatusNotFound}, false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalAuthError(tc.err); got != tc.want {
				t.Fatalf("isTerminalAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// Initial refresh must also die on 410 Gone (a daemon drained before it ever started), not just 401/403.
func TestFatalInitializationErrorIncludesDrainStatuses(t *testing.T) {
	if !isFatalInitializationError(&backendStatusError{StatusCode: http.StatusGone}) {
		t.Fatal("410 Gone must be fatal on initial refresh (daemon already drained)")
	}
	if isFatalInitializationError(&backendStatusError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("500 must remain retryable on initial refresh")
	}
}

// D1: the workspace event stream surfaces a rejected handshake's status so the reconnect loop can
// exit on a terminal auth/drain status instead of reconnecting forever. The status lived in the
// dial response, which the stream previously discarded.
func TestRunWorkspaceEventStreamClassifiesHandshakeStatus(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		terminal bool
	}{
		{"gone drains", http.StatusGone, true},
		{"unauthorized drains", http.StatusUnauthorized, true},
		{"server error retries", http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			s := &Service{cfg: Config{BackendURL: server.URL, WorkspaceID: "ws_test"}}
			err := s.runWorkspaceEventStream(context.Background())
			if err == nil {
				t.Fatal("expected an error from a rejected websocket handshake")
			}
			if got := isTerminalAuthError(err); got != tc.terminal {
				t.Fatalf("isTerminalAuthError = %v, want %v (err=%v)", got, tc.terminal, err)
			}
		})
	}
}

type closeTrackingBody struct {
	io.Reader
	closed *int32
}

func (b *closeTrackingBody) Close() error {
	atomic.AddInt32(b.closed, 1)
	return nil
}

type presenceRoundTripper struct {
	closed *int32
}

func (rt *presenceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       &closeTrackingBody{Reader: strings.NewReader("{}"), closed: rt.closed},
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// D2: sendPresence runs every 60s per runtime; it must close the response body or leak an fd each tick.
func TestSendPresenceClosesResponseBody(t *testing.T) {
	var closed int32
	r := &workspaceRuntime{
		cfg:    Config{BackendURL: "http://presence.test", AgentID: "daemon_agent"},
		client: &http.Client{Transport: &presenceRoundTripper{closed: &closed}},
	}
	if err := r.sendPresence(context.Background()); err != nil {
		t.Fatalf("sendPresence: %v", err)
	}
	if got := atomic.LoadInt32(&closed); got != 1 {
		t.Fatalf("presence response body closed %d times, want exactly 1", got)
	}
}
