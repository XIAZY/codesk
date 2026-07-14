package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"notty/daemon/internal/desktop/handoff"
)

const (
	testCallback = "http://127.0.0.1:49152/desktop/connect/AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	testSecret   = "future.credential-format:v2/with spaces"
)

type fakeSession struct {
	payload handoff.Payload
	err     error
	closed  bool
}

type trackingSession struct {
	*handoff.Session
	closed bool
}

func (s *fakeSession) CallbackURL() string {
	return testCallback
}

func (s *fakeSession) Wait(context.Context) (handoff.Payload, error) {
	return s.payload, s.err
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

func (s *trackingSession) Close() error {
	s.closed = true
	return s.Session.Close()
}

func TestParseConnectPage(t *testing.T) {
	valid, err := parseConnectPage("https://app.example.test/desktop-handoff-spike.html")
	if err != nil {
		t.Fatalf("valid connect page: %v", err)
	}
	if got := connectPageWithCallback(valid, testCallback); got != "https://app.example.test/desktop-handoff-spike.html?callback=http%3A%2F%2F127.0.0.1%3A49152%2Fdesktop%2Fconnect%2FAAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" {
		t.Fatalf("launch URL = %q", got)
	}
}

func TestParseConnectPageRejectsUnsafeURLs(t *testing.T) {
	for _, candidate := range []string{
		"",
		"/desktop-handoff-spike.html",
		"file:///tmp/desktop-handoff-spike.html",
		"http://app.example.test/desktop-handoff-spike.html",
		"https://user@app.example.test/desktop-handoff-spike.html",
		"https://app.example.test/desktop-handoff-spike.html?callback=other",
		"https://app.example.test/desktop-handoff-spike.html#fragment",
		"https://app.example.test/desktop-handoff-spike.html#",
	} {
		if _, err := parseConnectPage(candidate); err == nil {
			t.Fatalf("unsafe connect page was accepted: %q", candidate)
		}
	}
}

func TestRunPrintsOnlyNonsecretAcceptanceFields(t *testing.T) {
	session := acceptedSession(t, testSecret)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"--connect-page", "https://app.example.test/desktop-handoff-spike.html",
	}, &stdout, &stderr, func() (handoffSession, error) {
		return session, nil
	})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr=%q", exitCode, stderr.String())
	}
	if !session.closed {
		t.Fatal("session was not closed")
	}
	output := stdout.String() + stderr.String()
	if strings.Contains(output, testSecret) {
		t.Fatal("command output contains the daemon token")
	}
	for _, expected := range []string{
		"connect_url=https://app.example.test/desktop-handoff-spike.html?callback=",
		"handoff_accepted=true",
		"daemon_id=daemon-123",
		"workspace_id=workspace-123",
		"token_received=true",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("command output does not contain %q", expected)
		}
	}
	launchValue := strings.TrimPrefix(strings.SplitN(stdout.String(), "\n", 2)[0], "connect_url=")
	launchURL, err := url.Parse(launchValue)
	if err != nil {
		t.Fatalf("parse launch URL: %v", err)
	}
	if launchURL.Query().Get("callback") != session.CallbackURL() {
		t.Fatal("launch URL did not preserve the callback")
	}
}

func acceptedSession(t *testing.T, token string) *trackingSession {
	t.Helper()
	session, err := handoff.NewSession()
	if err != nil {
		t.Fatalf("new handoff session: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close handoff session: %v", err)
		}
	})

	form := url.Values{
		"daemon_id":      {"daemon-123"},
		"token":          {token},
		"workspace_id":   {"workspace-123"},
		"workspace_name": {"Desktop QA"},
		"workspace_slug": {"desktop-qa"},
		"workspace_url":  {"https://app.example.test/w/desktop-qa"},
	}
	response, err := http.PostForm(session.CallbackURL(), form)
	if err != nil {
		t.Fatalf("post accepted handoff: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("accepted handoff status = %d, want 200", response.StatusCode)
	}
	return &trackingSession{Session: session}
}

func TestRunRedactsReceiverErrors(t *testing.T) {
	session := &fakeSession{err: errors.New("failure mentioning " + testSecret)}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"--connect-page", "http://127.0.0.1:5173/desktop-handoff-spike.html",
	}, &stdout, &stderr, func() (handoffSession, error) {
		return session, nil
	})
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if strings.Contains(stdout.String()+stderr.String(), testSecret) {
		t.Fatal("command output contains the daemon token")
	}
}

func TestRunRejectsArgumentsBeforeBinding(t *testing.T) {
	called := false
	exitCode := run(context.Background(), []string{"--connect-page", "file:///tmp/spike.html"}, &bytes.Buffer{}, &bytes.Buffer{}, func() (handoffSession, error) {
		called = true
		return nil, errors.New("unexpected")
	})
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if called {
		t.Fatal("session was created before argument validation")
	}
}
