package handoff

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"notty/daemon/internal/desktopurl"
)

const (
	callbackPrefix = "/desktop/connect/"
	completionPath = "/desktop/connect/complete"
	maxBodyBytes   = 16 << 10
	maxTokenBytes  = 4 << 10
	rejectionBody  = "desktop handoff rejected\n"
)

var (
	// ErrClosed reports that the session was closed before a valid handoff arrived.
	ErrClosed = errors.New("desktop handoff session closed")

	formFields = []string{
		"daemon_id",
		"token",
		"workspace_id",
		"workspace_name",
		"workspace_slug",
		"workspace_url",
	}
)

// Payload is the one-time desktop credential handoff returned by the web app.
type Payload struct {
	DaemonID      string
	WorkspaceID   string
	WorkspaceName string
	WorkspaceSlug string
	WorkspaceURL  string
	token         string
}

// Token returns the opaque daemon credential received from the web app.
func (p Payload) Token() string {
	return p.token
}

// String prevents fmt diagnostics from disclosing the token.
func (p Payload) String() string {
	return fmt.Sprintf("desktop handoff payload{daemon_id=%q workspace_id=%q token=<redacted>}", p.DaemonID, p.WorkspaceID)
}

// GoString prevents %#v diagnostics from bypassing String's redaction.
func (p Payload) GoString() string {
	return p.String()
}

// Session owns a single loopback-only callback listener.
type Session struct {
	listener      net.Listener
	server        *http.Server
	callbackURL   string
	completionURL string
	host          string
	path          string

	stateMu  sync.RWMutex
	claimed  bool
	accepted bool
	closed   bool
	result   Payload

	acceptedCh chan struct{}
	closedCh   chan struct{}
	serveErrCh chan error
	closeOnce  sync.Once
}

// NewSession validates the Codesk completion origin, pre-binds an ephemeral
// IPv4 loopback port, generates a 256-bit callback nonce, and starts the
// one-shot HTTP receiver.
func NewSession(codeskOrigin string) (*Session, error) {
	completionURL, err := completionURLForOrigin(codeskOrigin)
	if err != nil {
		return nil, fmt.Errorf("validate desktop handoff completion origin: %w", err)
	}

	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("generate desktop handoff nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind desktop handoff listener: %w", err)
	}

	host := listener.Addr().String()
	path := callbackPrefix + nonce
	session := &Session{
		listener:      listener,
		callbackURL:   "http://" + host + path,
		completionURL: completionURL,
		host:          host,
		path:          path,
		acceptedCh:    make(chan struct{}),
		closedCh:      make(chan struct{}),
		serveErrCh:    make(chan error, 1),
	}
	session.server = &http.Server{
		Handler:           session,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	go func() {
		serveErr := session.server.Serve(listener)
		if serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) || errors.Is(serveErr, net.ErrClosed) {
			return
		}
		select {
		case session.serveErrCh <- serveErr:
		default:
		}
	}()

	return session, nil
}

// CallbackURL returns the exact nonce-bearing loopback URL the browser must
// submit to. It never contains the daemon token.
func (s *Session) CallbackURL() string {
	if s == nil {
		return ""
	}
	return s.callbackURL
}

// Wait blocks until the first valid handoff arrives, the context ends, the
// listener fails, or Close is called. It closes the listener before returning.
func (s *Session) Wait(ctx context.Context) (Payload, error) {
	if s == nil {
		return Payload{}, ErrClosed
	}
	if payload, ok := s.acceptedPayload(); ok {
		_ = s.Close()
		return payload, nil
	}

	select {
	case <-s.acceptedCh:
		payload, _ := s.acceptedPayload()
		_ = s.Close()
		return payload, nil
	case err := <-s.serveErrCh:
		if payload, ok, _ := s.closeAndWaitForClaimedPayload(); ok {
			return payload, nil
		}
		return Payload{}, fmt.Errorf("serve desktop handoff callback: %w", err)
	case <-s.closedCh:
		if payload, ok, _ := s.closeAndWaitForClaimedPayload(); ok {
			return payload, nil
		}
		return Payload{}, ErrClosed
	case <-ctx.Done():
		if payload, ok, _ := s.closeAndWaitForClaimedPayload(); ok {
			return payload, nil
		}
		return Payload{}, ctx.Err()
	}
}

// Close fences new claims and releases the callback listener. If a valid POST
// already claimed the session, Close lets its redirect response finish first.
// It is safe to call more than once.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	_, _, closeErr := s.closeAndWaitForClaimedPayload()
	return closeErr
}

func (s *Session) closeServer() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.closedCh)
		closeErr = s.server.Close()
		if errors.Is(closeErr, http.ErrServerClosed) || errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
	})
	return closeErr
}

func (s *Session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setResponseHeaders(w)

	if r.Host != s.host || r.URL.Path != s.path || r.URL.EscapedPath() != s.path {
		reject(w, http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		reject(w, http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reject(w, http.StatusMethodNotAllowed)
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		reject(w, http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > maxBodyBytes {
		reject(w, http.StatusRequestEntityTooLarge)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			reject(w, http.StatusRequestEntityTooLarge)
			return
		}
		reject(w, http.StatusBadRequest)
		return
	}
	if len(r.PostForm) != len(formFields) {
		reject(w, http.StatusBadRequest)
		return
	}
	for key := range r.PostForm {
		if !knownFormField(key) {
			reject(w, http.StatusBadRequest)
			return
		}
	}

	payload, ok := parsePayload(r.PostForm)
	if !ok {
		reject(w, http.StatusBadRequest)
		return
	}
	s.stateMu.Lock()
	if s.closed || s.claimed {
		s.stateMu.Unlock()
		reject(w, http.StatusConflict)
		return
	}
	s.claimed = true
	s.result = payload
	s.stateMu.Unlock()
	// The winning connection is already accepted. Close only the listener at the
	// claim boundary so no second connection can enter while the response flushes.
	_ = s.listener.Close()

	w.Header().Set("Location", s.completionURL)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusSeeOther)
	_ = http.NewResponseController(w).Flush()

	s.stateMu.Lock()
	s.accepted = true
	close(s.acceptedCh)
	s.stateMu.Unlock()
}

func completionURLForOrigin(rawOrigin string) (string, error) {
	if rawOrigin == "" || strings.ContainsAny(rawOrigin, "?#") {
		return "", errors.New("invalid Codesk origin")
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Host == "" || origin.Opaque != "" {
		return "", errors.New("invalid Codesk origin")
	}
	if origin.Scheme != "https" && origin.Scheme != "http" {
		return "", errors.New("invalid Codesk origin")
	}
	if origin.Scheme == "http" && origin.Hostname() != "127.0.0.1" && origin.Hostname() != "localhost" {
		return "", errors.New("remote Codesk origin must use HTTPS")
	}
	if !validURLPort(origin) {
		return "", errors.New("Codesk origin contains an invalid port")
	}
	if origin.User != nil || origin.Path != "" || origin.RawPath != "" || origin.RawQuery != "" ||
		origin.ForceQuery || origin.Fragment != "" || origin.String() != rawOrigin {
		return "", errors.New("Codesk origin contains forbidden URL components")
	}
	return rawOrigin + completionPath, nil
}

func validURLPort(value *url.URL) bool {
	port := value.Port()
	if port == "" {
		return !strings.HasSuffix(value.Host, ":")
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535
}

func (s *Session) acceptedPayload() (Payload, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if !s.accepted {
		return Payload{}, false
	}
	payload := s.result
	return payload, true
}

func (s *Session) closeAndWaitForClaimedPayload() (Payload, bool, error) {
	s.stateMu.Lock()
	claimed := s.claimed
	// Closing and claiming share this lock as their linearization boundary. Once
	// closed is set, a parsed request can no longer claim the session.
	s.closed = true
	s.stateMu.Unlock()
	if !claimed {
		return Payload{}, false, s.closeServer()
	}

	// A complete valid form claimed the session before the close fence. Let its
	// tiny redirect response finish before cancellation or Close tears down the
	// server, then return the accepted payload to the waiter.
	<-s.acceptedCh
	payload, _ := s.acceptedPayload()
	return payload, true, s.closeServer()
}

func parsePayload(values url.Values) (Payload, bool) {
	daemonID, ok := singleValue(values, "daemon_id")
	if !ok || !validText(daemonID, 128) {
		return Payload{}, false
	}
	token, ok := singleValue(values, "token")
	if !ok || !validOpaqueToken(token) {
		return Payload{}, false
	}
	workspaceID, ok := singleValue(values, "workspace_id")
	if !ok || !validText(workspaceID, 128) {
		return Payload{}, false
	}
	workspaceName, ok := singleValue(values, "workspace_name")
	if !ok || !validText(workspaceName, 256) {
		return Payload{}, false
	}
	workspaceSlug, ok := singleValue(values, "workspace_slug")
	if !ok || !validText(workspaceSlug, 128) {
		return Payload{}, false
	}
	workspaceURL, ok := singleValue(values, "workspace_url")
	if !ok || !desktopurl.Valid(workspaceURL) {
		return Payload{}, false
	}

	return Payload{
		DaemonID:      daemonID,
		WorkspaceID:   workspaceID,
		WorkspaceName: workspaceName,
		WorkspaceSlug: workspaceSlug,
		WorkspaceURL:  workspaceURL,
		token:         token,
	}, true
}

func singleValue(values url.Values, key string) (string, bool) {
	items, ok := values[key]
	if !ok || len(items) != 1 {
		return "", false
	}
	return items[0], true
}

func knownFormField(candidate string) bool {
	for _, field := range formFields {
		if candidate == field {
			return true
		}
	}
	return false
}

func validOpaqueToken(token string) bool {
	if token == "" || len(token) > maxTokenBytes || !utf8.ValidString(token) {
		return false
	}
	for _, char := range token {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func setResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Connection", "close")
}

func reject(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(rejectionBody)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, rejectionBody)
}
