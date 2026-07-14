package handoff

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "nottyd_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func TestNewSessionUsesIPv4LoopbackAnd256BitNonce(t *testing.T) {
	session := newTestSession(t)
	callback, err := url.Parse(session.CallbackURL())
	if err != nil {
		t.Fatalf("parse callback URL: %v", err)
	}
	if callback.Scheme != "http" || callback.Hostname() != "127.0.0.1" || callback.Port() == "" {
		t.Fatalf("callback is not an explicit IPv4 loopback URL: %q", callback)
	}
	if callback.RawQuery != "" || callback.Fragment != "" {
		t.Fatalf("callback URL unexpectedly contains query or fragment: %q", callback)
	}
	nonce := strings.TrimPrefix(callback.Path, callbackPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("nonce has %d bytes, want 32", len(decoded))
	}
	if strings.Contains(session.CallbackURL(), testToken) {
		t.Fatal("callback URL contains daemon token")
	}
}

func TestSessionAcceptsOneValidFormPost(t *testing.T) {
	session := newTestSession(t)
	response := postForm(t, session.CallbackURL(), validForm())
	body := readBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", response.StatusCode, body)
	}
	if strings.Contains(body, testToken) {
		t.Fatal("success response contains daemon token")
	}
	for header, want := range map[string]string{
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	} {
		if got := response.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if response.ContentLength != int64(len(body)) {
		t.Errorf("content length = %d, want %d", response.ContentLength, len(body))
	}
	assertSecondRequestRejected(t, session.CallbackURL())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload, err := session.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	want := Payload{
		DaemonID:      "daemon-123",
		WorkspaceID:   "workspace-123",
		WorkspaceName: "Desktop QA",
		WorkspaceSlug: "desktop-qa",
		WorkspaceURL:  "https://app.example.test/w/desktop-qa",
		token:         testToken,
	}
	if payload.DaemonID != want.DaemonID || payload.WorkspaceID != want.WorkspaceID ||
		payload.WorkspaceName != want.WorkspaceName || payload.WorkspaceSlug != want.WorkspaceSlug ||
		payload.WorkspaceURL != want.WorkspaceURL {
		t.Fatal("accepted payload metadata does not match the submitted values")
	}
	if payload.Token() != testToken {
		t.Fatal("accepted payload token does not match the submitted token")
	}
}

func TestSessionRejectsInvalidRequestsWithoutConsumingNonce(t *testing.T) {
	tests := []struct {
		name       string
		request    func(*Session) *http.Request
		wantStatus int
	}{
		{
			name: "wrong path",
			request: func(session *Session) *http.Request {
				return formRequest(t, strings.Replace(session.CallbackURL(), callbackPrefix, "/wrong/", 1), validForm())
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "wrong nonce",
			request: func(session *Session) *http.Request {
				wrongURL := session.CallbackURL()
				replacement := "A"
				if strings.HasSuffix(wrongURL, replacement) {
					replacement = "E"
				}
				return formRequest(t, wrongURL[:len(wrongURL)-1]+replacement, validForm())
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "wrong host",
			request: func(session *Session) *http.Request {
				request := formRequest(t, session.CallbackURL(), validForm())
				request.Host = "localhost:" + request.URL.Port()
				return request
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "query",
			request: func(session *Session) *http.Request {
				request := formRequest(t, session.CallbackURL()+"?token=forbidden", validForm())
				return request
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "get",
			request: func(session *Session) *http.Request {
				request, err := http.NewRequest(http.MethodGet, session.CallbackURL(), nil)
				if err != nil {
					t.Fatalf("new GET request: %v", err)
				}
				return request
			},
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name: "json content type",
			request: func(session *Session) *http.Request {
				request, err := http.NewRequest(http.MethodPost, session.CallbackURL(), strings.NewReader("{}"))
				if err != nil {
					t.Fatalf("new JSON request: %v", err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "malformed encoding",
			request: func(session *Session) *http.Request {
				request, err := http.NewRequest(http.MethodPost, session.CallbackURL(), strings.NewReader("daemon_id=%ZZ"))
				if err != nil {
					t.Fatalf("new malformed request: %v", err)
				}
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return request
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "missing field",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Del("workspace_name")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "duplicate field",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Add("daemon_id", "daemon-456")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "unknown field",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("extra", "value")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "empty token",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("token", "")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "token control character",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("token", "opaque\ntoken")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "oversized token",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("token", strings.Repeat("x", maxTokenBytes+1))
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid UTF-8 metadata",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Del("workspace_name")
				request, err := http.NewRequest(http.MethodPost, session.CallbackURL(), strings.NewReader(form.Encode()+"&workspace_name=%FF"))
				if err != nil {
					t.Fatalf("new invalid UTF-8 request: %v", err)
				}
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return request
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "workspace URL with credentials",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("workspace_url", "https://user:pass@app.example.test/w/desktop-qa")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "workspace URL with empty query",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("workspace_url", "https://app.example.test/w/desktop-qa?")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "workspace URL with empty fragment",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("workspace_url", "https://app.example.test/w/desktop-qa#")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "control character",
			request: func(session *Session) *http.Request {
				form := validForm()
				form.Set("workspace_name", "Desktop\nQA")
				return formRequest(t, session.CallbackURL(), form)
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newTestSession(t)
			response, err := http.DefaultClient.Do(test.request(session))
			if err != nil {
				t.Fatalf("invalid request: %v", err)
			}
			body := readBody(t, response)
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if strings.Contains(body, testToken) {
				t.Fatal("rejection response contains daemon token")
			}

			accepted := postForm(t, session.CallbackURL(), validForm())
			_ = readBody(t, accepted)
			if accepted.StatusCode != http.StatusOK {
				t.Fatalf("valid request after rejection = %d, want 200", accepted.StatusCode)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := session.Wait(ctx); err != nil {
				t.Fatalf("wait after valid request: %v", err)
			}
		})
	}
}

func TestSessionRejectsOversizedBody(t *testing.T) {
	session := newTestSession(t)
	body := "daemon_id=" + strings.Repeat("x", maxBodyBytes)
	request, err := http.NewRequest(http.MethodPost, session.CallbackURL(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new oversized request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("oversized request: %v", err)
	}
	_ = readBody(t, response)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
	accepted := postForm(t, session.CallbackURL(), validForm())
	_ = readBody(t, accepted)
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("valid request after oversized body = %d, want 200", accepted.StatusCode)
	}
}

func TestSessionOnlyOneConcurrentPostWins(t *testing.T) {
	session := newTestSession(t)
	const requests = 12
	statuses := make(chan int, requests)
	errorsCh := make(chan error, requests)
	start := make(chan struct{})
	var group sync.WaitGroup
	prepared := make([]*http.Request, 0, requests)
	for range requests {
		prepared = append(prepared, formRequest(t, session.CallbackURL(), validForm()))
	}
	for _, request := range prepared {
		group.Add(1)
		go func(request *http.Request) {
			defer group.Done()
			<-start
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				errorsCh <- err
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			statuses <- response.StatusCode
		}(request)
	}
	close(start)
	group.Wait()
	close(statuses)
	close(errorsCh)

	okCount := 0
	rejectedCount := 0
	for status := range statuses {
		if status == http.StatusOK {
			okCount++
			continue
		}
		if status != http.StatusConflict {
			t.Errorf("losing request status = %d, want 409", status)
			continue
		}
		rejectedCount++
	}
	if okCount != 1 {
		t.Fatalf("successful requests = %d, want 1", okCount)
	}
	for range errorsCh {
		// The listener closes at the winner's claim boundary. Requests that have
		// not connected by then are rejected at the TCP boundary.
		rejectedCount++
	}
	if rejectedCount != requests-1 {
		t.Fatalf("rejected requests = %d, want %d", rejectedCount, requests-1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestSessionValidClaimWinsCancellationRace(t *testing.T) {
	session := newTestSession(t)
	writer := &blockingFlushWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flushStarted:     make(chan struct{}),
		releaseFlush:     make(chan struct{}),
	}
	request := formRequest(t, session.CallbackURL(), validForm())
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		session.ServeHTTP(writer, request)
	}()
	<-writer.flushStarted
	assertListenerRefusesConnection(t, session.CallbackURL())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	type waitResult struct {
		payload Payload
		err     error
	}
	waitDone := make(chan waitResult, 1)
	go func() {
		payload, err := session.Wait(ctx)
		waitDone <- waitResult{payload: payload, err: err}
	}()

	select {
	case result := <-waitDone:
		t.Fatalf("Wait returned before the claimed response flushed: %v", result.err)
	case <-time.After(10 * time.Millisecond):
	}

	close(writer.releaseFlush)
	result := <-waitDone
	if result.err != nil {
		t.Fatalf("wait after valid claim: %v", result.err)
	}
	if result.payload.Token() != testToken {
		t.Fatal("valid claimed token was not preserved")
	}
	<-handlerDone
}

func TestSessionCancellationFencesLateValidClaim(t *testing.T) {
	session := newTestSession(t)
	encoded := validForm().Encode()
	body := &blockingRequestBody{
		reader:      strings.NewReader(encoded),
		readStarted: make(chan struct{}),
		releaseRead: make(chan struct{}),
	}
	request, err := http.NewRequest(http.MethodPost, session.CallbackURL(), body)
	if err != nil {
		t.Fatalf("new form request: %v", err)
	}
	request.ContentLength = int64(len(encoded))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	writer := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		session.ServeHTTP(writer, request)
	}()
	<-body.readStarted

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload, waitErr := session.Wait(ctx)
	close(body.releaseRead)
	<-handlerDone

	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("wait error = %v, want canceled", waitErr)
	}
	if payload != (Payload{}) {
		t.Fatalf("canceled wait returned payload: %v", payload)
	}
	if writer.Code != http.StatusConflict {
		t.Fatalf("late valid request status = %d, want 409", writer.Code)
	}
	if _, ok := session.acceptedPayload(); ok {
		t.Fatal("late valid request claimed a canceled session")
	}
}

func TestSessionCloseWaitsForClaimedResponse(t *testing.T) {
	session := newTestSession(t)
	writer := &blockingFlushWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flushStarted:     make(chan struct{}),
		releaseFlush:     make(chan struct{}),
	}
	request := formRequest(t, session.CallbackURL(), validForm())
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		session.ServeHTTP(writer, request)
	}()
	<-writer.flushStarted
	assertListenerRefusesConnection(t, session.CallbackURL())

	closeDone := make(chan error, 1)
	go func() { closeDone <- session.Close() }()
	released := false
	defer func() {
		if !released {
			close(writer.releaseFlush)
		}
	}()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close returned before the claimed response flushed: %v", closeErr)
	case <-time.After(10 * time.Millisecond):
	}

	close(writer.releaseFlush)
	released = true
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatalf("close after valid claim: %v", closeErr)
	}
	<-handlerDone
	payload, waitErr := session.Wait(context.Background())
	if waitErr != nil {
		t.Fatalf("wait after close preserved claim: %v", waitErr)
	}
	if payload.Token() != testToken {
		t.Fatal("Close discarded the valid claimed token")
	}
}

func TestSessionWaitTimeoutAndCancelCloseListener(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		session := newTestSession(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if _, err := session.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait error = %v, want deadline exceeded", err)
		}
		assertListenerClosed(t, session.CallbackURL())
	})

	t.Run("cancel", func(t *testing.T) {
		session := newTestSession(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := session.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want canceled", err)
		}
		assertListenerClosed(t, session.CallbackURL())
	})
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	session := newTestSession(t)
	if err := session.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := session.Wait(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("wait error = %v, want ErrClosed", err)
	}
}

func TestRejectionsDoNotLeakToken(t *testing.T) {
	session := newTestSession(t)
	form := validForm()
	leaked := testToken + "\ninvalid"
	form.Set("token", leaked)
	response := postForm(t, session.CallbackURL(), form)
	body := readBody(t, response)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	if strings.Contains(body, leaked) || strings.Contains(body, testToken) {
		t.Fatal("response leaked submitted token")
	}
	if strings.Contains(session.CallbackURL(), leaked) || strings.Contains(session.CallbackURL(), testToken) {
		t.Fatal("callback URL leaked submitted token")
	}
}

func TestSessionTreatsTokenAsOpaque(t *testing.T) {
	session := newTestSession(t)
	form := validForm()
	const opaqueToken = "future.credential-format:v2/with spaces"
	form.Set("token", opaqueToken)
	response := postForm(t, session.CallbackURL(), form)
	_ = readBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	payload, err := session.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if payload.Token() != opaqueToken {
		t.Fatal("opaque token changed during handoff")
	}
}

func TestPayloadFormattingRedactsToken(t *testing.T) {
	payload := Payload{DaemonID: "daemon-123", WorkspaceID: "workspace-123", token: testToken}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		formatted := fmt.Sprintf(format, payload)
		if strings.Contains(formatted, testToken) {
			t.Errorf("format %q contains daemon token", format)
		}
		if !strings.Contains(formatted, "<redacted>") {
			t.Errorf("format %q does not identify the redacted field", format)
		}
	}
}

func newTestSession(t *testing.T) *Session {
	t.Helper()
	session, err := NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})
	return session
}

func validForm() url.Values {
	return url.Values{
		"daemon_id":      {"daemon-123"},
		"token":          {testToken},
		"workspace_id":   {"workspace-123"},
		"workspace_name": {"Desktop QA"},
		"workspace_slug": {"desktop-qa"},
		"workspace_url":  {"https://app.example.test/w/desktop-qa"},
	}
}

func formRequest(t *testing.T, endpoint string, form url.Values) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new form request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

func postForm(t *testing.T, endpoint string, form url.Values) *http.Response {
	t.Helper()
	response, err := http.DefaultClient.Do(formRequest(t, endpoint, form))
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(body)
}

func assertListenerClosed(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		request := formRequest(t, endpoint, validForm())
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return
		}
		_ = readBody(t, response)
		if time.Now().After(deadline) {
			t.Fatal("listener remained reachable after session close")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertListenerRefusesConnection(t *testing.T, endpoint string) {
	t.Helper()
	callback, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse callback URL: %v", err)
	}
	connection, err := net.DialTimeout("tcp4", callback.Host, 100*time.Millisecond)
	if err != nil {
		return
	}
	_ = connection.Close()
	t.Fatal("accept socket remained reachable after the valid request claimed the session")
}

func assertSecondRequestRejected(t *testing.T, endpoint string) {
	t.Helper()
	request := formRequest(t, endpoint, validForm())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return
	}
	_ = readBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("second request status = %d, want 409 or closed listener", response.StatusCode)
	}
	assertListenerClosed(t, endpoint)
}

type blockingFlushWriter struct {
	*httptest.ResponseRecorder
	flushStarted chan struct{}
	releaseFlush chan struct{}
	flushOnce    sync.Once
}

type blockingRequestBody struct {
	reader      *strings.Reader
	readStarted chan struct{}
	releaseRead chan struct{}
	startOnce   sync.Once
}

func (b *blockingRequestBody) Read(buffer []byte) (int, error) {
	b.startOnce.Do(func() {
		close(b.readStarted)
		<-b.releaseRead
	})
	return b.reader.Read(buffer)
}

func (b *blockingRequestBody) Close() error {
	return nil
}

func (w *blockingFlushWriter) Flush() {
	w.flushOnce.Do(func() { close(w.flushStarted) })
	<-w.releaseFlush
	w.ResponseRecorder.Flush()
}
