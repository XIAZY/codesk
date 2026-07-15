package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"notty/daemon/internal/desktop/handoff"
)

type memSecretStore struct {
	mu      sync.Mutex
	data    map[string][]byte
	saveErr error
}

func newMemSecretStore() *memSecretStore {
	return &memSecretStore{data: make(map[string][]byte)}
}

func (m *memSecretStore) Save(key string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.data[key] = append([]byte{}, secret...)
	return nil
}

func (m *memSecretStore) Load(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key], nil
}

func (m *memSecretStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

type captureOpener struct {
	mu      sync.Mutex
	urls    []string
	openErr error
	opened  chan string
}

func newCaptureOpener() *captureOpener {
	return &captureOpener{opened: make(chan string, 1)}
}

func (c *captureOpener) Open(rawURL string) error {
	c.mu.Lock()
	c.urls = append(c.urls, rawURL)
	openErr := c.openErr
	c.mu.Unlock()
	select {
	case c.opened <- rawURL:
	default:
	}
	return openErr
}

func (c *captureOpener) lastURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.urls) == 0 {
		return ""
	}
	return c.urls[len(c.urls)-1]
}

var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func postHandoff(t *testing.T, callbackURL string, fields url.Values) *http.Response {
	t.Helper()
	resp, err := noRedirectClient.Post(callbackURL, "application/x-www-form-urlencoded", strings.NewReader(fields.Encode()))
	if err != nil {
		t.Fatalf("POST to callback: %v", err)
	}
	return resp
}

func validHandoffFields() url.Values {
	return url.Values{
		"daemon_id":      {"d-123"},
		"token":          {"nottyd_secret_token_value"},
		"workspace_id":   {"ws-456"},
		"workspace_name": {"Test Workspace"},
		"workspace_slug": {"test-workspace"},
		"workspace_url":  {"https://app.getcodesk.com/w/test-workspace"},
	}
}

func extractCallbackURL(t *testing.T, connectURL string) string {
	t.Helper()
	parsed, err := url.Parse(connectURL)
	if err != nil {
		t.Fatalf("parse connect URL: %v", err)
	}
	callback := parsed.Query().Get("callback")
	if callback == "" {
		t.Fatal("connect URL has no callback query parameter")
	}
	return callback
}

const testOrigin = "https://app.getcodesk.com"

func TestConnectHappyPath(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var payload handoff.Payload
	var connectErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		payload, connectErr = Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	callbackURL := extractCallbackURL(t, connectURL)

	resp := postHandoff(t, callbackURL, validHandoffFields())
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}

	<-done
	if connectErr != nil {
		t.Fatalf("Connect() error: %v", connectErr)
	}

	if payload.DaemonID != "d-123" {
		t.Errorf("DaemonID = %q, want %q", payload.DaemonID, "d-123")
	}
	if payload.WorkspaceID != "ws-456" {
		t.Errorf("WorkspaceID = %q, want %q", payload.WorkspaceID, "ws-456")
	}
	if payload.WorkspaceName != "Test Workspace" {
		t.Errorf("WorkspaceName = %q, want %q", payload.WorkspaceName, "Test Workspace")
	}
	if payload.WorkspaceSlug != "test-workspace" {
		t.Errorf("WorkspaceSlug = %q, want %q", payload.WorkspaceSlug, "test-workspace")
	}
	if payload.WorkspaceURL != "https://app.getcodesk.com/w/test-workspace" {
		t.Errorf("WorkspaceURL = %q, want %q", payload.WorkspaceURL, "https://app.getcodesk.com/w/test-workspace")
	}
	if payload.Token() != "nottyd_secret_token_value" {
		t.Errorf("Token() = %q, want %q", payload.Token(), "nottyd_secret_token_value")
	}

	stored, _ := secrets.Load(SecretKeyDaemonToken)
	if string(stored) != "nottyd_secret_token_value" {
		t.Errorf("stored token = %q, want %q", stored, "nottyd_secret_token_value")
	}
}

func TestConnectPersistsBeforeReturning(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	callbackURL := extractCallbackURL(t, connectURL)
	resp := postHandoff(t, callbackURL, validHandoffFields())
	resp.Body.Close()

	<-done

	stored, _ := secrets.Load(SecretKeyDaemonToken)
	if len(stored) == 0 {
		t.Fatal("token was not persisted before Connect returned")
	}
}

func TestConnectSecretStoreFailure(t *testing.T) {
	secrets := newMemSecretStore()
	secrets.saveErr = errors.New("disk full")
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var connectErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, connectErr = Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	callbackURL := extractCallbackURL(t, connectURL)
	resp := postHandoff(t, callbackURL, validHandoffFields())
	resp.Body.Close()

	<-done
	if connectErr == nil {
		t.Fatal("Connect() should fail when SecretStore.Save fails")
	}
	if !strings.Contains(connectErr.Error(), "persist") {
		t.Errorf("error should mention 'persist', got: %v", connectErr)
	}
	if strings.Contains(connectErr.Error(), "nottyd_secret_token_value") {
		t.Error("error message must not contain the token value")
	}
}

func TestConnectContextCancelled(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithCancel(context.Background())

	var connectErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, connectErr = Connect(ctx, testOrigin, secrets, opener)
	}()

	<-opener.opened
	cancel()

	<-done
	if connectErr == nil {
		t.Fatal("Connect() should fail when context is cancelled")
	}
	if !errors.Is(connectErr, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", connectErr)
	}

	stored, _ := secrets.Load(SecretKeyDaemonToken)
	if len(stored) > 0 {
		t.Error("token should not be persisted when context is cancelled")
	}
}

func TestConnectOpenURLFailure(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()
	opener.openErr = errors.New("no browser available")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, testOrigin, secrets, opener)
	if err == nil {
		t.Fatal("Connect() should fail when OpenURL fails")
	}
	if !strings.Contains(err.Error(), "open browser") {
		t.Errorf("error should mention 'open browser', got: %v", err)
	}

	stored, _ := secrets.Load(SecretKeyDaemonToken)
	if len(stored) > 0 {
		t.Error("token should not be persisted when browser open fails")
	}
}

func TestConnectInvalidOrigin(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, "", secrets, opener)
	if err == nil {
		t.Fatal("Connect() should fail with empty origin")
	}

	if opener.lastURL() != "" {
		t.Error("browser should not be opened for invalid origin")
	}
}

func TestConnectCallbackURLIsLoopback(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	callbackURL := extractCallbackURL(t, connectURL)
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatalf("parse callback URL: %v", err)
	}
	if parsed.Hostname() != "127.0.0.1" {
		t.Errorf("callback host = %q, want 127.0.0.1", parsed.Hostname())
	}
	if parsed.Scheme != "http" {
		t.Errorf("callback scheme = %q, want http", parsed.Scheme)
	}

	cancel()
	<-done
}

func TestConnectTokenNotInConnectURL(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	if strings.Contains(connectURL, "nottyd_") {
		t.Error("connect URL must not contain daemon token")
	}

	cancel()
	<-done
}

func TestConnectDuplicatePost(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	callbackURL := extractCallbackURL(t, connectURL)

	resp1 := postHandoff(t, callbackURL, validHandoffFields())
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first POST: expected 303, got %d", resp1.StatusCode)
	}

	<-done

	resp2, err := noRedirectClient.Post(callbackURL, "application/x-www-form-urlencoded",
		strings.NewReader(validHandoffFields().Encode()))
	if err == nil {
		resp2.Body.Close()
		if resp2.StatusCode == http.StatusSeeOther {
			t.Error("duplicate POST should not be accepted")
		}
	}
}

func TestConnectCompletionRedirectURL(t *testing.T) {
	secrets := newMemSecretStore()
	opener := newCaptureOpener()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Connect(ctx, testOrigin, secrets, opener)
	}()

	connectURL := <-opener.opened
	callbackURL := extractCallbackURL(t, connectURL)

	resp, err := noRedirectClient.Post(callbackURL, "application/x-www-form-urlencoded",
		strings.NewReader(validHandoffFields().Encode()))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	location := resp.Header.Get("Location")
	if location != testOrigin+"/desktop/connect/complete" {
		t.Errorf("redirect Location = %q, want %q", location, testOrigin+"/desktop/connect/complete")
	}
	if strings.Contains(location, "nottyd_") {
		t.Error("redirect URL must not contain token")
	}

	<-done
}
