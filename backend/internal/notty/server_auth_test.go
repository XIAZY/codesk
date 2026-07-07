package notty

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestJWTIssuerUsesCodeskAndRejectsLegacyNottyIssuer(t *testing.T) {
	secret := "issuer-secret"
	account := &Account{ID: "acct_issuer", Email: "issuer@example.com", DisplayName: "Issuer"}
	token, err := issueJWT(secret, account, time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	claims, err := verifyJWT(secret, token)
	if err != nil {
		t.Fatalf("verify codesk jwt: %v", err)
	}
	if claims.Issuer != "codesk" {
		t.Fatalf("expected codesk issuer, got %q", claims.Issuer)
	}

	legacyToken := signedAuthTestJWT(t, secret, jwtClaims{
		Subject:     account.ID,
		Email:       account.Email,
		DisplayName: account.DisplayName,
		IssuedAt:    time.Now().UTC().Unix(),
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Unix(),
		Issuer:      "notty",
	})
	_, err = verifyJWT(secret, legacyToken)
	if err == nil || !strings.Contains(err.Error(), "invalid jwt issuer") {
		t.Fatalf("expected legacy issuer rejection, got %v", err)
	}
}

func TestRegisterCreatesUnverifiedAccountAndRequiresEmailVerification(t *testing.T) {
	_, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)

	var register AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       "verify-flow@example.com",
		Password:    "verify-pass",
		DisplayName: "Verify Flow",
	}, http.StatusCreated, &register)
	if register.Token != "" {
		t.Fatalf("register returned token for unverified account: %q", register.Token)
	}
	if register.Account == nil || register.Account.EmailVerified {
		t.Fatalf("expected unverified account, got %#v", register.Account)
	}
	messages := authTestWaitForEmailCount(t, sender, 1)
	if len(messages) != 1 {
		t.Fatalf("registration emails sent = %d, want 1", len(messages))
	}
	if messages[0].Subject != "Verify your email to finish signing up" {
		t.Fatalf("registration email should verify the account, got subject %q", messages[0].Subject)
	}

	var loginError map[string]string
	authTestJSON(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    "verify-flow@example.com",
		Password: "verify-pass",
	}, http.StatusForbidden, &loginError)
	if loginError["error"] != errEmailNotVerified.Error() {
		t.Fatalf("login error = %#v, want email_not_verified", loginError)
	}

	verifyToken := authTestEmailToken(t, messages[0], "/account/verify-email")
	var verifyResponse struct {
		Account *Account `json:"account"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusOK, &verifyResponse)
	if verifyResponse.Account == nil || !verifyResponse.Account.EmailVerified {
		t.Fatalf("expected verified account, got %#v", verifyResponse.Account)
	}
	messages = authTestWaitForEmailCount(t, sender, 2)
	if len(messages) != 2 {
		t.Fatalf("verification should send welcome email, got %d messages", len(messages))
	}
	if messages[1].Subject != "Welcome to codesk" {
		t.Fatalf("post-verification email should welcome the user, got subject %q", messages[1].Subject)
	}
	if strings.Contains(messages[1].Text, "token=") {
		t.Fatalf("welcome email should not carry an account token: %q", messages[1].Text)
	}
	authTestErrorContains(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusBadRequest, "email_already_verified")
	messages = sender.snapshot()
	if len(messages) != 2 {
		t.Fatalf("verification replay should not send another welcome email, got %d messages", len(messages))
	}

	var login AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    "verify-flow@example.com",
		Password: "verify-pass",
	}, http.StatusOK, &login)
	if login.Token == "" || login.Account == nil || !login.Account.EmailVerified {
		t.Fatalf("expected verified login response, got %#v", login)
	}
}

func TestVerifyEmailDoesNotBlockOnWelcomeEmailFailure(t *testing.T) {
	_, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)
	sender.failBySubject = map[string]error{
		"Welcome to codesk": errors.New("welcome email failed"),
	}
	logs := authTestCaptureLogs(t)

	var register AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       "welcome-failure@example.com",
		Password:    "verify-pass",
		DisplayName: "Welcome Failure",
	}, http.StatusCreated, &register)
	if register.Token != "" {
		t.Fatalf("register returned token for unverified account: %q", register.Token)
	}
	if register.Account == nil || register.Account.EmailVerified {
		t.Fatalf("expected unverified account, got %#v", register.Account)
	}
	messages := authTestWaitForEmailCount(t, sender, 1)
	if len(messages) != 1 {
		t.Fatalf("registration should send verification email only, got %d messages", len(messages))
	}
	verifyToken := authTestEmailToken(t, messages[0], "/account/verify-email")
	var verifyResponse struct {
		Account *Account `json:"account"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusOK, &verifyResponse)
	if verifyResponse.Account == nil || !verifyResponse.Account.EmailVerified {
		t.Fatalf("expected verified account despite welcome failure, got %#v", verifyResponse.Account)
	}
	_ = authTestWaitForEmailFailure(t, sender, "Welcome to codesk")
	messages = sender.snapshot()
	if len(messages) != 1 {
		t.Fatalf("failed welcome email should not be retained by fake sender, got %d messages", len(messages))
	}
	if got := logs.waitContains(t, "account email send failed type=welcome", register.Account.ID); strings.Contains(got, "token=") {
		t.Fatalf("welcome failure log leaked account token/link: %q", got)
	}
}

func TestRegisterStartsVerificationEmailAsyncAndIgnoresSendFailure(t *testing.T) {
	_, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)
	release := make(chan struct{})
	released := false
	closeRelease := func() {
		if !released {
			close(release)
			released = true
		}
	}
	t.Cleanup(closeRelease)
	sender.blockUntil = release
	sender.failBySubject = map[string]error{
		"Verify your email to finish signing up": errors.New("mailgun unavailable"),
	}
	logs := authTestCaptureLogs(t)

	done := make(chan authTestHTTPResult, 1)
	go func() {
		var register AuthResponse
		result := authTestServeJSONNoFatal(router, http.MethodPost, "/api/auth/register", RegisterRequest{
			Email:       "async-register@example.com",
			Password:    "verify-pass",
			DisplayName: "Async Register",
		}, &register)
		done <- result
	}()

	var register AuthResponse
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("decode register response: %v body=%s", result.err, result.body)
		}
		if result.code != http.StatusCreated {
			t.Fatalf("register expected status %d, got %d body=%s", http.StatusCreated, result.code, result.body)
		}
		if err := json.Unmarshal([]byte(result.body), &register); err != nil {
			t.Fatalf("decode register response: %v body=%s", err, result.body)
		}
	case <-time.After(time.Second):
		closeRelease()
		t.Fatalf("register response waited for email send completion")
	}
	if register.Token != "" || register.Account == nil || register.Account.EmailVerified {
		t.Fatalf("expected unverified registration response without token, got %#v", register)
	}
	attempt := authTestWaitForEmailAttempt(t, sender, "Verify your email to finish signing up")
	authTestAssertAsyncEmailContext(t, attempt)
	if messages := sender.snapshot(); len(messages) != 0 {
		t.Fatalf("blocked async email should not be recorded yet, got %#v", messages)
	}
	closeRelease()
	failure := authTestWaitForEmailFailure(t, sender, "Verify your email to finish signing up")
	if failure.Err == nil || !strings.Contains(failure.Err.Error(), "mailgun unavailable") {
		t.Fatalf("expected async verification send failure, got %#v", failure)
	}
	if got := logs.waitContains(t, "account email send failed type=verification", register.Account.ID); strings.Contains(got, "token=") || strings.Contains(got, "/account/verify-email") {
		t.Fatalf("verification failure log leaked account token/link: %q", got)
	}
}

func TestForgotPasswordStartsResetEmailAsyncAndIgnoresSendFailure(t *testing.T) {
	_, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)
	auth := authTestRegister(t, router, "async-reset@example.com", "old-pass", "Async Reset")
	initialMessages := sender.count()

	release := make(chan struct{})
	released := false
	closeRelease := func() {
		if !released {
			close(release)
			released = true
		}
	}
	t.Cleanup(closeRelease)
	sender.blockUntil = release
	sender.failBySubject = map[string]error{
		"Reset your codesk password": errors.New("mailgun unavailable"),
	}
	logs := authTestCaptureLogs(t)

	done := make(chan authTestHTTPResult, 1)
	go func() {
		done <- authTestServeJSONNoFatal(router, http.MethodPost, "/api/auth/forgot-password", ForgotPasswordRequest{
			Email: "async-reset@example.com",
		}, nil)
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("forgot-password request failed: %v body=%s", result.err, result.body)
		}
		if result.code != http.StatusOK {
			t.Fatalf("forgot-password expected status %d, got %d body=%s", http.StatusOK, result.code, result.body)
		}
	case <-time.After(time.Second):
		closeRelease()
		t.Fatalf("forgot-password response waited for email send completion")
	}
	attempt := authTestWaitForEmailAttempt(t, sender, "Reset your codesk password")
	authTestAssertAsyncEmailContext(t, attempt)
	if messages := sender.snapshot(); len(messages) != initialMessages {
		t.Fatalf("blocked async reset email should not be recorded yet, got %d messages, want %d", len(messages), initialMessages)
	}
	closeRelease()
	failure := authTestWaitForEmailFailure(t, sender, "Reset your codesk password")
	if failure.Err == nil || !strings.Contains(failure.Err.Error(), "mailgun unavailable") {
		t.Fatalf("expected async reset send failure, got %#v", failure)
	}
	if got := logs.waitContains(t, "account email send failed type=password_reset", auth.Account.ID); strings.Contains(got, "token=") || strings.Contains(got, "/account/reset-password") {
		t.Fatalf("reset failure log leaked account token/link: %q", got)
	}
}

func TestForgotAndResetPasswordRequireVerifiedAccountAndKeepExistingJWTStateless(t *testing.T) {
	_, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)
	unverified := "reset-unverified@example.com"
	verified := "reset-verified@example.com"

	var register AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       unverified,
		Password:    "old-pass",
		DisplayName: "Reset Unverified",
	}, http.StatusCreated, &register)
	authTestWaitForEmailCount(t, sender, 1)
	initialMessages := sender.count()
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: unverified,
	}, http.StatusOK, nil)
	if sender.count() != initialMessages {
		t.Fatalf("forgot-password sent reset email for unverified account")
	}

	auth := authTestRegister(t, router, verified, "old-pass", "Reset Verified")
	initialMessages = sender.count()
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: verified,
	}, http.StatusOK, nil)
	messages := authTestWaitForEmailCount(t, sender, initialMessages+1)
	if len(messages) != initialMessages+1 {
		t.Fatalf("forgot-password emails sent = %d, want %d", len(messages), initialMessages+1)
	}
	resetToken := authTestEmailToken(t, messages[len(messages)-1], "/account/reset-password")
	authTestJSON(t, router, http.MethodPost, "/api/auth/reset-password", "", ResetPasswordRequest{
		Token:    resetToken,
		Password: "new-pass",
	}, http.StatusOK, nil)
	authTestStatus(t, router, http.MethodPost, "/api/auth/reset-password", "", ResetPasswordRequest{
		Token:    resetToken,
		Password: "second-pass",
	}, http.StatusBadRequest)

	authTestStatus(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    verified,
		Password: "old-pass",
	}, http.StatusUnauthorized)
	authTestStatus(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    verified,
		Password: "second-pass",
	}, http.StatusUnauthorized)
	var login AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    verified,
		Password: "new-pass",
	}, http.StatusOK, &login)
	if login.Token == "" {
		t.Fatalf("expected login token after password reset")
	}
	var oldTokenMe AuthResponse
	authTestJSON(t, router, http.MethodGet, "/api/auth/me", auth.Token, nil, http.StatusOK, &oldTokenMe)
	if oldTokenMe.Account == nil || oldTokenMe.Account.Email != verified {
		t.Fatalf("expected existing stateless token to remain valid after reset, got %#v", oldTokenMe)
	}
	var me AuthResponse
	authTestJSON(t, router, http.MethodGet, "/api/auth/me", login.Token, nil, http.StatusOK, &me)
	if me.Account == nil || me.Account.Email != verified {
		t.Fatalf("expected new token to authenticate reset account, got %#v", me)
	}
}

func TestAccountEmailTokensArePurposeScopedAndSingleUse(t *testing.T) {
	_, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)

	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       "token-scope@example.com",
		Password:    "token-pass",
		DisplayName: "Token Scope",
	}, http.StatusCreated, nil)
	messages := authTestWaitForEmailCount(t, sender, 1)
	verifyToken := authTestEmailToken(t, messages[0], "/account/verify-email")

	authTestJSON(t, router, http.MethodPost, "/api/auth/reset-password", "", ResetPasswordRequest{
		Token:    verifyToken,
		Password: "new-pass",
	}, http.StatusBadRequest, nil)
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusOK, nil)
	authTestErrorContains(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusBadRequest, "email_already_verified")
}

func TestAccountEmailTokensRejectExpiredVerificationAndResetTokens(t *testing.T) {
	server, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)

	var register AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       "expired-verify@example.com",
		Password:    "verify-pass",
		DisplayName: "Expired Verify",
	}, http.StatusCreated, &register)
	if register.Account == nil || register.Account.EmailVerified {
		t.Fatalf("expected unverified account response, got %#v", register.Account)
	}
	messages := authTestWaitForEmailCount(t, sender, 1)
	verifyToken := authTestLatestEmailToken(t, messages, 0, "/account/verify-email")
	authTestExpireAccountEmailToken(t, server.sqlDB(), verifyToken, accountEmailTokenPurposeVerifyEmail)
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusBadRequest, nil)
	messages = sender.snapshot()
	if len(messages) != 1 {
		t.Fatalf("expired verification should not send welcome email, got %d messages", len(messages))
	}
	authTestStatus(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    "expired-verify@example.com",
		Password: "verify-pass",
	}, http.StatusForbidden)

	authTestRegister(t, router, "expired-reset@example.com", "old-pass", "Expired Reset")
	initialMessages := sender.count()
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "expired-reset@example.com",
	}, http.StatusOK, nil)
	messages = authTestWaitForEmailCount(t, sender, initialMessages+1)
	if len(messages) != initialMessages+1 {
		t.Fatalf("forgot-password emails sent = %d, want %d", len(messages), initialMessages+1)
	}
	resetToken := authTestEmailToken(t, messages[len(messages)-1], "/account/reset-password")
	authTestExpireAccountEmailToken(t, server.sqlDB(), resetToken, accountEmailTokenPurposeResetPassword)
	authTestJSON(t, router, http.MethodPost, "/api/auth/reset-password", "", ResetPasswordRequest{
		Token:    resetToken,
		Password: "new-pass",
	}, http.StatusBadRequest, nil)
}

func TestVerificationAndPasswordResetRequestsRespectNoopAndCooldownStates(t *testing.T) {
	server, router := newAuthTestServer(t)
	sender := authTestEmailSenderForRouter(t, router)

	var register AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       "resend-state@example.com",
		Password:    "verify-pass",
		DisplayName: "Resend State",
	}, http.StatusCreated, &register)
	if register.Account == nil || register.Account.EmailVerified {
		t.Fatalf("expected unverified account response, got %#v", register.Account)
	}
	messages := authTestWaitForEmailCount(t, sender, 1)
	if len(messages) != 1 {
		t.Fatalf("registration emails sent = %d, want 1", len(messages))
	}
	firstVerifyToken := authTestEmailToken(t, messages[0], "/account/verify-email")

	authTestJSON(t, router, http.MethodPost, "/api/auth/resend-verification", "", ResendVerificationRequest{
		Email: "resend-state@example.com",
	}, http.StatusOK, nil)
	messages = sender.snapshot()
	if len(messages) != 1 {
		t.Fatalf("resend inside cooldown sent email count = %d, want 1", len(messages))
	}

	authTestBackdateAccountEmailToken(t, server.sqlDB(), firstVerifyToken, accountEmailTokenPurposeVerifyEmail, accountEmailTokenCooldown+time.Minute)
	authTestJSON(t, router, http.MethodPost, "/api/auth/resend-verification", "", ResendVerificationRequest{
		Email: "resend-state@example.com",
	}, http.StatusOK, nil)
	messages = authTestWaitForEmailCount(t, sender, 2)
	if len(messages) != 2 {
		t.Fatalf("resend after cooldown sent email count = %d, want 2", len(messages))
	}
	secondVerifyToken := authTestEmailToken(t, messages[1], "/account/verify-email")
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: firstVerifyToken,
	}, http.StatusBadRequest, nil)
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: secondVerifyToken,
	}, http.StatusOK, nil)
	messages = authTestWaitForEmailCount(t, sender, 3)
	if len(messages) != 3 {
		t.Fatalf("successful verification should send welcome email count = %d, want 3", len(messages))
	}
	if messages[2].Subject != "Welcome to codesk" {
		t.Fatalf("successful verification should send welcome email, got subject %q", messages[2].Subject)
	}

	authTestJSON(t, router, http.MethodPost, "/api/auth/resend-verification", "", ResendVerificationRequest{
		Email: "resend-state@example.com",
	}, http.StatusOK, nil)
	messages = sender.snapshot()
	if len(messages) != 3 {
		t.Fatalf("resend for verified account sent email count = %d, want 3", len(messages))
	}

	authTestRegister(t, router, "reset-cooldown@example.com", "old-pass", "Reset Cooldown")
	initialMessages := sender.count()
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "reset-cooldown@example.com",
	}, http.StatusOK, nil)
	messages = authTestWaitForEmailCount(t, sender, initialMessages+1)
	if len(messages) != initialMessages+1 {
		t.Fatalf("forgot-password emails sent = %d, want %d", len(messages), initialMessages+1)
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "reset-cooldown@example.com",
	}, http.StatusOK, nil)
	if sender.count() != initialMessages+1 {
		t.Fatalf("forgot-password inside cooldown sent email count = %d, want %d", sender.count(), initialMessages+1)
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "missing-reset@example.com",
	}, http.StatusOK, nil)
	if sender.count() != initialMessages+1 {
		t.Fatalf("forgot-password for nonexistent account sent email count = %d, want %d", sender.count(), initialMessages+1)
	}
}

func TestAuthenticateHumanRequestRejectsUnverifiedAccountJWT(t *testing.T) {
	_, router := newAuthTestServer(t)

	var register AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       "unverified-jwt@example.com",
		Password:    "jwt-pass",
		DisplayName: "Unverified JWT",
	}, http.StatusCreated, &register)
	if register.Account == nil || register.Account.EmailVerified {
		t.Fatalf("expected unverified account response, got %#v", register.Account)
	}
	token, err := issueJWT("test-secret", register.Account, time.Hour)
	if err != nil {
		t.Fatalf("issue unverified account jwt: %v", err)
	}
	authTestErrorContains(t, router, http.MethodGet, "/api/auth/me", token, nil, http.StatusUnauthorized, errEmailNotVerified.Error())
}

func TestEmailVerifiedColumnDefaultIsFalse(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	now := time.Now().UTC()
	accountID := uuid.NewString()
	if _, err := db.Exec(
		`INSERT INTO accounts (id, email, display_name, password_hash, password_updated_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5, $5)`,
		accountID,
		"future-default-unverified@example.com",
		"Future Default",
		"hash",
		now,
	); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var verified bool
	if err := db.QueryRow(`SELECT email_verified FROM accounts WHERE id = $1`, accountID).Scan(&verified); err != nil {
		t.Fatalf("select email_verified: %v", err)
	}
	if verified {
		t.Fatalf("email_verified defaulted to true")
	}
}

func signedAuthTestJWT(t *testing.T, secret string, claims jwtClaims) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + signJWT([]byte(secret), unsigned)
}

func TestAuthenticatedWorkspaceRoutesIsolateTenantsAndIgnoreSpoofedActor(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "owner-auth@example.com", "owner-pass", "Owner")
	if len(owner.Workspaces) != 0 {
		t.Fatalf("registration should not create implicit workspaces, got %#v", owner.Workspaces)
	}
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Tenant One")

	authTestStatus(t, router, http.MethodGet, "/api/workspace", owner.Token, nil, http.StatusNotFound)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", "", nil, http.StatusUnauthorized)

	outsider := authTestRegister(t, router, "outsider-auth@example.com", "outsider-pass", "Outsider")
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", outsider.Token, nil, http.StatusForbidden)

	member := authTestAddMember(t, router, owner.Token, workspace.ID, "outsider-auth@example.com", "outsider")
	var outsiderWorkspace struct {
		WorkspaceID   string `json:"workspaceId"`
		CurrentUserID string `json:"currentUserId"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", outsider.Token, nil, http.StatusOK, &outsiderWorkspace)
	if outsiderWorkspace.WorkspaceID != workspace.ID || outsiderWorkspace.CurrentUserID != member.UserID {
		t.Fatalf("expected outsider membership user in workspace response, got %#v member=%#v", outsiderWorkspace, member)
	}

	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/auth.md", "# Auth\n")
	var threadResponse struct {
		Thread Thread `json:"thread"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads?actor=spoof&actor_type=agent", owner.Token, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Auth provenance",
		Body:       "This should be owned by the authenticated workspace user.",
		Excerpt:    "# Auth",
	}, http.StatusCreated, &threadResponse)
	if threadResponse.Thread.CreatedByID != workspace.OwnerUserID || threadResponse.Thread.CreatedByType != "human" {
		t.Fatalf("expected authenticated author, got id=%q type=%q owner=%q", threadResponse.Thread.CreatedByID, threadResponse.Thread.CreatedByType, workspace.OwnerUserID)
	}
}

func TestRegisterAccountCreatesNoImplicitWorkspaceRows(t *testing.T) {
	server, router := newAuthTestServer(t)

	auth := authTestRegister(t, router, "zero-workspace@example.com", "owner-pass", "Zero Workspace")
	if len(auth.Workspaces) != 0 {
		t.Fatalf("registration should return zero workspaces, got %#v", auth.Workspaces)
	}

	var workspaceCount int
	if err := server.sqlDB().QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&workspaceCount); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	var memberCount int
	if err := server.sqlDB().QueryRow(`SELECT COUNT(*) FROM workspace_members`).Scan(&memberCount); err != nil {
		t.Fatalf("count workspace members: %v", err)
	}
	if workspaceCount != 0 || memberCount != 0 {
		t.Fatalf("registration should not create workspace rows, workspaces=%d members=%d", workspaceCount, memberCount)
	}

	var list struct {
		Workspaces []*Workspace `json:"workspaces"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces", auth.Token, nil, http.StatusOK, &list)
	if len(list.Workspaces) != 0 {
		t.Fatalf("workspace list should be empty for zero-workspace account, got %#v", list.Workspaces)
	}
}

func TestCreateWorkspaceStoresIndependentRootDocumentID(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "root-id-owner@example.com", "owner-pass", "Root ID Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Root ID Tenant")
	if workspace.RootDocumentID == "" {
		t.Fatalf("expected workspace creation to return root document ID")
	}
	if _, err := uuid.Parse(workspace.RootDocumentID); err != nil {
		t.Fatalf("root document ID must be a stored UUID, got %q: %v", workspace.RootDocumentID, err)
	}

	var storedRootID string
	if err := server.sqlDB().QueryRow(`SELECT root_document_id::text FROM workspaces WHERE id = $1`, workspace.ID).Scan(&storedRootID); err != nil {
		t.Fatalf("load stored root document ID: %v", err)
	}
	if storedRootID != workspace.RootDocumentID {
		t.Fatalf("stored root document ID = %q, want %q", storedRootID, workspace.RootDocumentID)
	}

	store, err := server.workspaceStore(workspace.ID)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	if store.RootDocumentID() != workspace.RootDocumentID {
		t.Fatalf("workspace store root document ID = %q, want %q", store.RootDocumentID(), workspace.RootDocumentID)
	}
	if !store.HasDocument(workspace.RootDocumentID) {
		t.Fatalf("stored root document %q is not syncable", workspace.RootDocumentID)
	}
}

func TestLastAccessedWorkspaceAndDocumentPersistPerAccountMembership(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "last-access-owner@example.com", "owner-pass", "Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Last Access Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/route.md", "# Route\n")
	otherDocument := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/other.md", "# Other\n")
	memberAuth := authTestRegister(t, router, "last-access-member@example.com", "member-pass", "Member")
	_ = authTestAddMember(t, router, owner.Token, workspace.ID, "last-access-member@example.com", "member")

	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/last-accessed", owner.Token, UpdateLastAccessedRequest{
		DocumentID: document.ID,
	}, http.StatusOK)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/last-accessed", memberAuth.Token, UpdateLastAccessedRequest{
		DocumentID: otherDocument.ID,
	}, http.StatusOK)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/last-accessed", owner.Token, UpdateLastAccessedRequest{
		DocumentID: "doc_missing",
	}, http.StatusNotFound)

	var response AuthResponse
	authTestJSON(t, router, http.MethodGet, "/api/auth/me", owner.Token, nil, http.StatusOK, &response)
	if response.Account == nil || response.Account.LastAccessedWorkspaceID != workspace.ID {
		t.Fatalf("expected last accessed workspace %q, got %#v", workspace.ID, response.Account)
	}
	if len(response.Workspaces) != 1 || response.Workspaces[0].LastAccessedDocumentID != document.ID {
		t.Fatalf("expected last accessed document %q, got %#v", document.ID, response.Workspaces)
	}
	var memberResponse AuthResponse
	authTestJSON(t, router, http.MethodGet, "/api/auth/me", memberAuth.Token, nil, http.StatusOK, &memberResponse)
	if memberResponse.Account == nil || memberResponse.Account.LastAccessedWorkspaceID != workspace.ID {
		t.Fatalf("expected member last accessed workspace %q, got %#v", workspace.ID, memberResponse.Account)
	}
	if len(memberResponse.Workspaces) != 1 || memberResponse.Workspaces[0].LastAccessedDocumentID != otherDocument.ID {
		t.Fatalf("expected member last accessed document %q, got %#v", otherDocument.ID, memberResponse.Workspaces)
	}
}

func TestCreateWorkspaceRequiresExplicitValidSlugAndHandle(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "workspace-slug-one@example.com", "owner-pass", "Owner One")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Handle: "owner-one",
	}, http.StatusBadRequest, "Workspace slug is required.")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "Product Workspace",
		Handle: "owner-one",
	}, http.StatusBadRequest, "Workspace slug can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   " product-workspace ",
		Handle: "owner-one",
	}, http.StatusBadRequest, "Workspace slug can only contain lowercase letters, numbers, underscores, and dashes.")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name: "Product Workspace",
		Slug: "product-workspace",
	}, http.StatusBadRequest, "Handle is required.")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: "Owner One",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: " owner-one ",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")

	var first struct {
		Workspace Workspace `json:"workspace"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", owner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: "owner-one",
	}, http.StatusCreated, &first)
	if first.Workspace.Slug != "product-workspace" {
		t.Fatalf("expected exact submitted slug, got %#v", first.Workspace)
	}

	secondOwner := authTestRegister(t, router, "workspace-slug-two@example.com", "owner-pass", "Owner Two")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces", secondOwner.Token, CreateWorkspaceRequest{
		Name:   "Product Workspace",
		Slug:   "product-workspace",
		Handle: "owner-two",
	}, http.StatusBadRequest, "Workspace slug is already taken.")
}

func TestWorkspaceMemberAndAgentIdentifiersAreValidatedAndImmutable(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "identifier-owner@example.com", "owner-pass", "Identifier Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Identifier Tenant")
	_ = authTestRegister(t, router, "identifier-member@example.com", "member-pass", "Identifier Member")

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email: "identifier-member@example.com",
	}, http.StatusBadRequest, "Handle is required.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-member@example.com",
		Handle: "Member One",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-member@example.com",
		Handle: " member_one ",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")

	member := authTestAddMember(t, router, owner.Token, workspace.ID, "identifier-member@example.com", "member_one")
	var repeatedAdd struct {
		Member WorkspaceMember `json:"member"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-member@example.com",
		Handle: "other_member",
	}, http.StatusCreated, &repeatedAdd)
	if repeatedAdd.Member.UserID != member.UserID || repeatedAdd.Member.UserHandle != member.UserHandle {
		t.Fatalf("re-adding existing member should return original user identity, first=%#v repeated=%#v", member, repeatedAdd.Member)
	}
	var userCount int
	if err := server.sqlDB().QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = $1`, workspace.ID).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 2 {
		t.Fatalf("re-adding existing member should not create a replacement user, got %d users", userCount)
	}

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Runtime daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "member_one",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusBadRequest, "Handle is already taken.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "Agent One",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: " agent_one ",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusBadRequest, "Handle can only contain lowercase letters, numbers, underscores, and dashes.")

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "agent_one",
		Name:   "Agent One",
		Role:   "Review changes",
		Kind:   "codex",
	}, http.StatusCreated, &agent)

	_ = authTestRegister(t, router, "identifier-other-member@example.com", "member-pass", "Identifier Other Member")
	authTestErrorContains(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  "identifier-other-member@example.com",
		Handle: "agent_one",
	}, http.StatusBadRequest, "Handle is already taken.")

	var updatedAgent Agent
	authTestJSON(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+agent.ID, owner.Token, UpdateAgentRequest{
		Handle: "renamed_agent",
		Name:   "Renamed Agent",
		Role:   "Still reviews changes",
	}, http.StatusOK, &updatedAgent)
	if updatedAgent.Handle != "agent_one" || updatedAgent.Name != "Renamed Agent" {
		t.Fatalf("agent update should mutate profile fields but keep handle, got %#v", updatedAgent)
	}
}

func TestAuthenticatedWorkspaceUserMutationEndpointsAreUnavailable(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "no-user-endpoints@example.com", "owner-pass", "Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "No User Endpoints Tenant")

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/users", owner.Token, map[string]string{
		"name":   "Blocked User",
		"handle": "blocked_user",
		"role":   "Blocked",
	}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/users/user-blocked", owner.Token, map[string]string{
		"name": "Blocked User",
	}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/users/user-blocked", owner.Token, nil, http.StatusNotFound)
}

func TestRouteTableMatchesAuthenticatedWorkspaceAllowlist(t *testing.T) {
	server := NewServer(Config{JWTSecret: "test-secret"}, nil)
	routes, ok := server.Routes().(chi.Routes)
	if !ok {
		t.Fatal("server routes should expose chi route table")
	}

	public := map[string]bool{
		"GET /healthz":                       true,
		"POST /api/auth/register":            true,
		"POST /api/auth/login":               true,
		"POST /api/auth/verify-email":        true,
		"POST /api/auth/resend-verification": true,
		"POST /api/auth/forgot-password":     true,
		"POST /api/auth/reset-password":      true,
		"GET /api/invites/{token}":           true,
	}
	human := map[string]bool{
		"GET /api/auth/me":                 true,
		"GET /api/workspaces":              true,
		"POST /api/workspaces":             true,
		"POST /api/invites/{token}/accept": true,
	}
	workspaceAPI := map[string]bool{
		"GET /api/workspaces/{workspaceID}/workspace":                                  true,
		"PATCH /api/workspaces/{workspaceID}/workspace":                                true,
		"DELETE /api/workspaces/{workspaceID}/":                                        true,
		"PATCH /api/workspaces/{workspaceID}/last-accessed":                            true,
		"GET /api/workspaces/{workspaceID}/members":                                    true,
		"POST /api/workspaces/{workspaceID}/members":                                   true,
		"POST /api/workspaces/{workspaceID}/invites":                                   true,
		"GET /api/workspaces/{workspaceID}/daemons":                                    true,
		"POST /api/workspaces/{workspaceID}/daemons":                                   true,
		"PATCH /api/workspaces/{workspaceID}/daemon/status":                            true,
		"POST /api/workspaces/{workspaceID}/daemons/{daemonID}/reinstall-token":        true,
		"DELETE /api/workspaces/{workspaceID}/daemons/{daemonID}":                      true,
		"POST /api/workspaces/{workspaceID}/daemons/{daemonID}/agents":                 true,
		"POST /api/workspaces/{workspaceID}/documents":                                 true,
		"GET /api/workspaces/{workspaceID}/documents/{id}/threads":                     true,
		"POST /api/workspaces/{workspaceID}/agents":                                    true,
		"PATCH /api/workspaces/{workspaceID}/agents/{id}":                              true,
		"PATCH /api/workspaces/{workspaceID}/agents/{id}/session":                      true,
		"DELETE /api/workspaces/{workspaceID}/agents/{id}":                             true,
		"POST /api/workspaces/{workspaceID}/agents/{id}/runs":                          true,
		"POST /api/workspaces/{workspaceID}/threads":                                   true,
		"GET /api/workspaces/{workspaceID}/threads/{id}":                               true,
		"POST /api/workspaces/{workspaceID}/threads/{id}/messages":                     true,
		"POST /api/workspaces/{workspaceID}/presence":                                  true,
		"POST /api/workspaces/{workspaceID}/agent-runs":                                true,
		"PATCH /api/workspaces/{workspaceID}/agent-runs/{id}":                          true,
		"POST /api/workspaces/{workspaceID}/agent-runs/{id}/stop":                      true,
		"POST /api/workspaces/{workspaceID}/agent-events/claim":                        true,
		"PATCH /api/workspaces/{workspaceID}/agent-events/{id}":                        true,
		"GET /api/workspaces/{workspaceID}/agents/{id}/notifications":                  true,
		"GET /api/workspaces/{workspaceID}/agent-notifications/{id}":                   true,
		"PATCH /api/workspaces/{workspaceID}/agent-notifications/{id}":                 true,
		"GET /api/workspaces/{workspaceID}/agents/{id}/inbox":                          true,
		"GET /api/workspaces/{workspaceID}/agent-inbox/{id}":                           true,
		"PATCH /api/workspaces/{workspaceID}/agent-inbox/{id}":                         true,
		"GET /api/workspaces/{workspaceID}/agents/{id}/documents/{documentID}/diff":    true,
		"POST /api/workspaces/{workspaceID}/agents/{id}/documents/{documentID}/viewed": true,
	}
	workspaceWS := map[string]bool{
		"GET /ws/workspaces/{workspaceID}":                true,
		"GET /ws/workspaces/{workspaceID}/documents/{id}": true,
		"GET /ws/workspaces/{workspaceID}/documents-sync": true,
	}
	expected := map[string]string{}
	addExpectedRoutes := func(kind string, routes map[string]bool) {
		for route := range routes {
			expected[route] = kind
		}
	}
	addExpectedRoutes("public", public)
	addExpectedRoutes("human", human)
	addExpectedRoutes("workspace API", workspaceAPI)
	addExpectedRoutes("workspace websocket", workspaceWS)

	seen := map[string]string{}
	if err := chi.Walk(routes, func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		kind, ok := expected[key]
		if !ok {
			t.Fatalf("unexpected registered route %s", key)
		}
		switch kind {
		case "public":
			if !public[key] {
				t.Fatalf("public route allowlist mismatch for %s", key)
			}
		case "human":
			if !human[key] || strings.HasPrefix(route, "/api/workspaces/{workspaceID}") {
				t.Fatalf("human-auth route allowlist mismatch for %s", key)
			}
		case "workspace API":
			if !strings.HasPrefix(route, "/api/workspaces/{workspaceID}/") {
				t.Fatalf("workspace API route must be workspace scoped, got %s", key)
			}
		case "workspace websocket":
			if !strings.HasPrefix(route, "/ws/workspaces/{workspaceID}") {
				t.Fatalf("websocket route must be workspace scoped, got %s", key)
			}
		default:
			t.Fatalf("unclassified expected route %s kind %q", key, kind)
		}
		seen[key] = kind
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	for key := range expected {
		if _, ok := seen[key]; !ok {
			t.Fatalf("expected route %s was not registered", key)
		}
	}
}

func TestLegacyNoAuthRoutesAreNotRegistered(t *testing.T) {
	router := newAuthTestRouter(t)

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodGet, path: "/api/workspace"},
		{method: http.MethodPost, path: "/api/documents", body: map[string]string{}},
		{method: http.MethodPost, path: "/api/users", body: map[string]string{"name": "Blocked", "handle": "blocked"}},
		{method: http.MethodPost, path: "/api/agents", body: map[string]string{"handle": "blocked", "name": "Blocked", "kind": "codex"}},
		{method: http.MethodPost, path: "/api/threads", body: map[string]string{"documentId": "doc_missing"}},
		{method: http.MethodPost, path: "/api/presence", body: map[string]string{"actorId": "user_missing"}},
		{method: http.MethodPost, path: "/api/agent-runs", body: map[string]string{"agentId": "agent_missing"}},
		{method: http.MethodGet, path: "/ws"},
		{method: http.MethodGet, path: "/ws/documents/doc_missing"},
		{method: http.MethodGet, path: "/ws/documents-sync"},
	}

	for _, tc := range cases {
		authTestStatus(t, router, tc.method, tc.path, "", tc.body, http.StatusNotFound)
	}
}

func TestDaemonTokenIsWorkspaceScopedAndCanActAsWorkspaceAgent(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "daemon-owner@example.com", "owner-pass", "Daemon Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Tenant")
	otherWorkspace := authTestCreateWorkspace(t, router, owner.Token, "Other Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/daemon.md", "# Daemon\n")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Local daemon"}, http.StatusCreated, &daemonResponse)
	if daemonResponse.Token == "" {
		t.Fatal("expected daemon token")
	}
	if daemonResponse.Daemon.ConnectionStatus != "disconnected" {
		t.Fatalf("new daemon should start disconnected until it checks in, got %q", daemonResponse.Daemon.ConnectionStatus)
	}

	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Review workspace changes",
		Kind:   "codex",
	}, http.StatusCreated, &agent)
	if agent.DaemonID != daemonResponse.Daemon.ID {
		t.Fatalf("expected agent daemon id %q, got %q", daemonResponse.Daemon.ID, agent.DaemonID)
	}

	events, unsubscribe := server.workspaceBroker(workspace.ID).Subscribe()
	defer unsubscribe()
	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+agent.ID+"/session", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "thread-1"}, http.StatusOK)
	requireBrokerEventTypes(t, events, "agent.updated", "activity.created")
	activities, err := listActivitiesPostgres(server.sqlDB(), workspace.ID)
	if err != nil {
		t.Fatalf("list activities: %v", err)
	}
	var sessionActivity *ActivityEvent
	for _, activity := range activities {
		if activity.Type == "agent.session.updated" {
			sessionActivity = activity
			break
		}
	}
	if sessionActivity == nil {
		t.Fatalf("expected agent session activity")
	}
	if sessionActivity.ActorID != agent.ID || sessionActivity.ActorType != "agent" {
		t.Fatalf("expected session activity actor to be acting agent %q, got id=%q type=%q", agent.ID, sessionActivity.ActorID, sessionActivity.ActorType)
	}
	if sessionActivity.Provenance.ActorID != agent.ID || sessionActivity.Provenance.ActorType != "agent" {
		t.Fatalf("expected session provenance actor to be acting agent %q, got id=%q type=%q", agent.ID, sessionActivity.Provenance.ActorID, sessionActivity.Provenance.ActorType)
	}
	var daemonList struct {
		Daemons []*Daemon `json:"daemons"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, nil, http.StatusOK, &daemonList)
	if len(daemonList.Daemons) != 1 || daemonList.Daemons[0].ConnectionStatus != "online" {
		t.Fatalf("expected checked-in daemon to be online, got %#v", daemonList.Daemons)
	}
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+otherWorkspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusForbidden)

	var statusResponse struct {
		Daemon Daemon `json:"daemon"`
	}
	authTestJSON(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK, &statusResponse)
	if statusResponse.Daemon.ID != daemonResponse.Daemon.ID || statusResponse.Daemon.Version != "0.62.0" || len(statusResponse.Daemon.Runtimes) != 1 || !statusResponse.Daemon.Runtimes[0].Available {
		t.Fatalf("expected daemon status to update authenticated daemon, got %#v", statusResponse.Daemon)
	}
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", owner.Token, UpdateDaemonStatusRequest{Version: "human"}, http.StatusForbidden)

	var otherDaemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Other daemon"}, http.StatusCreated, &otherDaemonResponse)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", otherDaemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)
	var otherAgent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+otherDaemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "other-agent",
		Name:   "Other Agent",
		Role:   "Owned by another daemon",
		Kind:   "codex",
	}, http.StatusCreated, &otherAgent)
	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+otherAgent.ID+"/session", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": otherAgent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "wrong-daemon"}, http.StatusForbidden)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+otherAgent.ID+"/session", daemonResponse.Token, UpdateAgentSessionRequest{Status: "idle", SessionID: "wrong-daemon-no-header"}, http.StatusForbidden)
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", daemonResponse.Token, CreateDaemonRequest{Name: "daemon-created-daemon"}, http.StatusForbidden)

	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, nil, http.StatusOK, &daemonList)
	if len(daemonList.Daemons) < 1 || daemonList.Daemons[0].Version != "0.62.0" || len(daemonList.Daemons[0].Runtimes) != 1 {
		t.Fatalf("expected daemon list to include runtime status, got %#v", daemonList.Daemons)
	}

	authTestStatusWithHeaders(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.Handle,
	}, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Handle-based agent note",
		Body:       "Handles are display names, not acting identities.",
		Excerpt:    "# Daemon",
	}, http.StatusForbidden)

	var threadResponse struct {
		Thread Thread `json:"thread"`
	}
	authTestJSONWithHeaders(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/threads", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, CreateThreadRequest{
		DocumentID: document.ID,
		Title:      "Agent note",
		Body:       "The daemon token may only speak as a verified workspace agent.",
		Excerpt:    "# Daemon",
	}, http.StatusCreated, &threadResponse)
	if threadResponse.Thread.CreatedByID != agent.ID || threadResponse.Thread.CreatedByType != "agent" {
		t.Fatalf("expected thread authored by acting agent, got id=%q type=%q agent=%q", threadResponse.Thread.CreatedByID, threadResponse.Thread.CreatedByType, agent.ID)
	}

	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID, owner.Token, nil, http.StatusOK)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusForbidden)
}

func TestCreateDaemonAgentValidatesReportedRuntimeKind(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		runtimes    []RuntimeDetection
		wantStatus  int
		wantKind    string
		wantErrText []string
	}{
		{
			name:        "malformed kind",
			kind:        "bad kind",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: true}},
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"invalid agent kind"},
		},
		{
			name:        "no runtime report",
			kind:        "codex",
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"has not reported runtime availability"},
		},
		{
			name:        "runtime unavailable",
			kind:        "codex",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: false, Reason: "codex command not found"}},
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"runtime \"codex\" is unavailable", "codex command not found"},
		},
		{
			name:        "different runtime reported",
			kind:        "claude-code",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: true}},
			wantStatus:  http.StatusBadRequest,
			wantErrText: []string{"runtime \"claude-code\" is not reported"},
		},
		{
			name:       "runtime available",
			kind:       "Claude-Code",
			runtimes:   []RuntimeDetection{{Kind: "claude-code", Available: true, Version: "claude test"}},
			wantStatus: http.StatusCreated,
			wantKind:   "claude-code",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newAuthTestRouter(t)
			emailSuffix := strings.ReplaceAll(tc.name, " ", "-")
			owner := authTestRegister(t, router, "runtime-"+emailSuffix+"@example.com", "owner-pass", "Runtime Owner")
			workspace := authTestCreateWorkspace(t, router, owner.Token, "Runtime Tenant "+tc.name)
			var daemonResponse CreateDaemonResponse
			authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Runtime daemon"}, http.StatusCreated, &daemonResponse)
			if tc.runtimes != nil {
				authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
					Version:  "0.62.0",
					OS:       "linux",
					Arch:     "arm64",
					Runtimes: tc.runtimes,
				}, http.StatusOK)
			}
			recorder := authTestRequest(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, nil, CreateAgentRequest{
				Handle: "runtime-agent",
				Name:   "Runtime Agent",
				Role:   "Exercises runtime validation",
				Kind:   tc.kind,
			})
			if recorder.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d body=%s", tc.wantStatus, recorder.Code, recorder.Body.String())
			}
			if len(tc.wantErrText) > 0 {
				var errorResponse struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &errorResponse); err != nil {
					t.Fatalf("decode error response: %v body=%s", err, recorder.Body.String())
				}
				for _, want := range tc.wantErrText {
					if !strings.Contains(errorResponse.Error, want) {
						t.Fatalf("expected error %q to contain %q", errorResponse.Error, want)
					}
				}
			}
			if tc.wantStatus != http.StatusCreated {
				return
			}
			var agent Agent
			if err := json.Unmarshal(recorder.Body.Bytes(), &agent); err != nil {
				t.Fatalf("decode agent response: %v body=%s", err, recorder.Body.String())
			}
			if agent.Kind != tc.wantKind {
				t.Fatalf("expected agent kind %q, got %#v", tc.wantKind, agent)
			}
		})
	}
}

func TestCreateDaemonAgentAppearsInDaemonWorkspaceSnapshot(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "runtime-snapshot-owner@example.com", "owner-pass", "Runtime Snapshot Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Runtime Snapshot Tenant")
	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Runtime daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex test",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

	var created Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "runtime-agent",
		Name:   "Runtime Agent",
		Role:   "Exercises daemon pickup",
		Kind:   "codex",
	}, http.StatusCreated, &created)

	var snapshot struct {
		CurrentDaemonID string   `json:"currentDaemonId"`
		Agents          []*Agent `json:"agents"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusOK, &snapshot)
	if snapshot.CurrentDaemonID != daemonResponse.Daemon.ID {
		t.Fatalf("expected current daemon %q, got %q", daemonResponse.Daemon.ID, snapshot.CurrentDaemonID)
	}
	if len(snapshot.Agents) != 1 {
		t.Fatalf("expected daemon workspace snapshot to include one agent, got %#v", snapshot.Agents)
	}
	got := snapshot.Agents[0]
	if got.ID != created.ID || got.DaemonID != daemonResponse.Daemon.ID || got.Kind != "codex" {
		t.Fatalf("expected created runtime agent in daemon snapshot, got %#v created=%#v daemon=%#v", got, created, daemonResponse.Daemon)
	}
}

func TestDaemonReinstallTokenRotatesDaemonToken(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "daemon-reinstall-owner@example.com", "owner-pass", "Daemon Reinstall Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Reinstall Tenant")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Local daemon"}, http.StatusCreated, &daemonResponse)
	if daemonResponse.Token == "" || daemonResponse.Daemon == nil {
		t.Fatalf("expected daemon token and daemon, got %#v", daemonResponse)
	}

	var reinstallResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/reinstall-token", owner.Token, nil, http.StatusOK, &reinstallResponse)
	if reinstallResponse.Token == "" {
		t.Fatal("expected reinstall token")
	}
	if reinstallResponse.Token == daemonResponse.Token {
		t.Fatal("expected reinstall token to differ from original token")
	}
	if reinstallResponse.Daemon.ID != daemonResponse.Daemon.ID {
		t.Fatalf("expected reinstall token for same daemon, got %#v", reinstallResponse.Daemon)
	}

	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", reinstallResponse.Token, nil, http.StatusOK)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", daemonResponse.Token, nil, http.StatusForbidden)
	authTestStatus(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", reinstallResponse.Token, nil, http.StatusOK)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/reinstall-token", reinstallResponse.Token, nil, http.StatusForbidden)
}

func TestDaemonTokenDocumentUpdateHTTPRouteRemoved(t *testing.T) {
	_, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "daemon-document-owner@example.com", "owner-pass", "Daemon Document Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Document Tenant")
	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "docs/daemon-update.md", "alpha")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Local daemon"}, http.StatusCreated, &daemonResponse)
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)

	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemonResponse.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "daemon-editor",
		Name:   "Daemon Editor",
		Role:   "Applies local document edits",
		Kind:   "codex",
	}, http.StatusCreated, &agent)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID+"/updates", bytes.NewReader([]byte{0, 0}))
	request.Header.Set("Authorization", "Bearer "+daemonResponse.Token)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Notty-Acting-Agent-ID", agent.ID)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected removed authenticated document update route, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID, owner.Token, map[string]string{"path": "docs/renamed.md"}, http.StatusNotFound)
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID, owner.Token, nil, http.StatusNotFound)
}

func TestWorkspaceInviteLinkCreatePreviewAndAccept(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "invite-link-owner@example.com", "owner-pass", "Invite Link Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Link Workspace")
	ownerUser, err := getUserForTest(server.sqlDB(), workspace.ID, workspace.OwnerUserID)
	if err != nil {
		t.Fatalf("get owner user: %v", err)
	}
	if ownerUser == nil {
		t.Fatalf("expected owner user in workspace users")
	}

	invite, token := authTestCreateInvite(t, router, owner.Token, workspace.ID)
	if invite.Invite == nil || invite.Invite.ID == "" || invite.Invite.WorkspaceID != workspace.ID {
		t.Fatalf("expected workspace invite response, got %#v", invite)
	}

	var tokenHashCount int
	if err := server.sqlDB().QueryRow(`SELECT COUNT(*) FROM workspace_invites WHERE token_hash = $1`, tokenHash(token)).Scan(&tokenHashCount); err != nil {
		t.Fatalf("count token hash: %v", err)
	}
	if tokenHashCount != 1 {
		t.Fatalf("expected one hashed invite token row, got %d", tokenHashCount)
	}
	var rawTokenCount int
	if err := server.sqlDB().QueryRow(`SELECT COUNT(*) FROM workspace_invites WHERE token_hash = $1`, token).Scan(&rawTokenCount); err != nil {
		t.Fatalf("count raw token: %v", err)
	}
	if rawTokenCount != 0 {
		t.Fatalf("raw invite token must not be stored")
	}

	recorder := authTestRequest(t, router, http.MethodGet, "/api/invites/"+token, "", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview expected status %d, got %d body=%s", http.StatusOK, recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), workspace.ID) || strings.Contains(recorder.Body.String(), "workspaceId") {
		t.Fatalf("public invite preview leaked workspace ID: %s", recorder.Body.String())
	}
	var preview WorkspaceInvitePreviewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v body=%s", err, recorder.Body.String())
	}
	if preview.Workspace == nil || preview.Workspace.Name != "Invite Link Workspace" || preview.Workspace.Slug == "" {
		t.Fatalf("expected safe workspace preview, got %#v", preview)
	}

	authTestStatus(t, router, http.MethodPost, "/api/invites/"+token+"/accept", "", AcceptWorkspaceInviteRequest{Handle: "joiner"}, http.StatusUnauthorized)

	joiner := authTestRegister(t, router, "invite-link-joiner@example.com", "join-pass", "Invite Link Joiner")
	var accepted AcceptWorkspaceInviteResponse
	authTestJSON(t, router, http.MethodPost, "/api/invites/"+token+"/accept", joiner.Token, AcceptWorkspaceInviteRequest{Handle: "joiner"}, http.StatusOK, &accepted)
	if accepted.Workspace == nil || accepted.Workspace.ID != workspace.ID {
		t.Fatalf("expected accepted workspace %q, got %#v", workspace.ID, accepted)
	}

	var memberCount int
	var role string
	var joinedUserID string
	err = server.sqlDB().QueryRow(
		`SELECT COUNT(*) OVER (), membership_role, user_id::text
		   FROM workspace_members
		  WHERE workspace_id = $1 AND account_id = $2 AND status = 'active'
		  ORDER BY user_id::text
		  LIMIT 1`,
		workspace.ID, joiner.Account.ID,
	).Scan(&memberCount, &role, &joinedUserID)
	if err == sql.ErrNoRows {
		memberCount = 0
	} else if err != nil {
		t.Fatalf("count joined membership: %v", err)
	}
	if memberCount != 1 || role != MembershipRoleMember {
		t.Fatalf("expected one active member role, got count=%d role=%q", memberCount, role)
	}
	joinedUser, err := getUserForTest(server.sqlDB(), workspace.ID, joinedUserID)
	if err != nil {
		t.Fatalf("get joined user: %v", err)
	}
	if joinedUser == nil || joinedUser.Handle != "joiner" {
		t.Fatalf("accept should persist joined user, got userID=%q user=%#v", joinedUserID, joinedUser)
	}
	var userCount int
	if err := server.sqlDB().QueryRow(
		`SELECT COUNT(*)
		   FROM users u
		   JOIN workspace_members m ON m.user_id = u.id AND m.workspace_id = u.workspace_id
		  WHERE u.workspace_id = $1 AND u.handle = $2 AND m.account_id = $3`,
		workspace.ID, "joiner", joiner.Account.ID,
	).Scan(&userCount); err != nil {
		t.Fatalf("count joined user: %v", err)
	}
	if userCount != 1 {
		t.Fatalf("expected exactly one joined user row, got %d", userCount)
	}

	var me AuthResponse
	authTestJSON(t, router, http.MethodGet, "/api/auth/me", joiner.Token, nil, http.StatusOK, &me)
	if me.Account == nil || me.Account.LastAccessedWorkspaceID != workspace.ID {
		t.Fatalf("expected accepted workspace to become last accessed, got %#v", me.Account)
	}
	if len(me.Workspaces) != 1 || me.Workspaces[0].ID != workspace.ID {
		t.Fatalf("expected accepted workspace in auth state, got %#v", me.Workspaces)
	}

	authTestStatus(t, router, http.MethodPost, "/api/invites/"+token+"/accept", joiner.Token, nil, http.StatusOK)
	if err := server.sqlDB().QueryRow(
		`SELECT COUNT(*)
		   FROM workspace_members
		  WHERE workspace_id = $1 AND account_id = $2`,
		workspace.ID, joiner.Account.ID,
	).Scan(&memberCount); err != nil {
		t.Fatalf("count membership after idempotent accept: %v", err)
	}
	if memberCount != 1 {
		t.Fatalf("idempotent accept should not duplicate memberships, got %d", memberCount)
	}
}

func TestWorkspaceInviteAcceptReactivatesInactiveMembership(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "invite-reactivate-owner@example.com", "owner-pass", "Invite Reactivate Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Reactivate Workspace")
	member := authTestRegister(t, router, "invite-reactivate-member@example.com", "member-pass", "Invite Reactivate Member")
	added := authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "reactivate-member")
	if _, err := server.sqlDB().Exec(
		`UPDATE workspace_members SET status = 'removed', membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, member.Account.ID,
	); err != nil {
		t.Fatalf("mark membership removed: %v", err)
	}
	if _, err := server.sqlDB().Exec(
		`UPDATE users SET status = 'removed' WHERE workspace_id = $1 AND id = $2`,
		workspace.ID, added.UserID,
	); err != nil {
		t.Fatalf("mark user removed: %v", err)
	}

	_, token := authTestCreateInvite(t, router, owner.Token, workspace.ID)
	var accepted AcceptWorkspaceInviteResponse
	authTestJSON(t, router, http.MethodPost, "/api/invites/"+token+"/accept", member.Token, AcceptWorkspaceInviteRequest{Handle: "ignored-new-handle"}, http.StatusOK, &accepted)
	if accepted.Workspace == nil || accepted.Workspace.ID != workspace.ID {
		t.Fatalf("expected accepted workspace %q, got %#v", workspace.ID, accepted)
	}

	var memberCount int
	var userID, membershipRole, memberStatus, userHandle, userStatus string
	err := server.sqlDB().QueryRow(
		`SELECT COUNT(*) OVER (), m.user_id::text, m.membership_role, m.status, u.handle, u.status
		   FROM workspace_members m
		   JOIN users u ON u.workspace_id = m.workspace_id AND u.id = m.user_id
		  WHERE m.workspace_id = $1 AND m.account_id = $2
		  ORDER BY m.user_id::text
		  LIMIT 1`,
		workspace.ID, member.Account.ID,
	).Scan(&memberCount, &userID, &membershipRole, &memberStatus, &userHandle, &userStatus)
	if err == sql.ErrNoRows {
		memberCount = 0
	} else if err != nil {
		t.Fatalf("load reactivated member: %v", err)
	}
	if memberCount != 1 {
		t.Fatalf("reactivated invite should keep one membership row, got %d", memberCount)
	}
	if userID != added.UserID || userHandle != added.UserHandle {
		t.Fatalf("reactivated invite should preserve existing user identity, got userID=%q handle=%q want userID=%q handle=%q", userID, userHandle, added.UserID, added.UserHandle)
	}
	if membershipRole != MembershipRoleMember || memberStatus != "active" || userStatus != "active" {
		t.Fatalf("expected active member reactivation, got role=%q memberStatus=%q userStatus=%q", membershipRole, memberStatus, userStatus)
	}

	var me AuthResponse
	authTestJSON(t, router, http.MethodGet, "/api/auth/me", member.Token, nil, http.StatusOK, &me)
	if me.Account == nil || me.Account.LastAccessedWorkspaceID != workspace.ID || len(me.Workspaces) != 1 || me.Workspaces[0].ID != workspace.ID {
		t.Fatalf("reactivated invite should restore workspace auth state, got account=%#v workspaces=%#v", me.Account, me.Workspaces)
	}
}

func TestWorkspaceInviteCreateRequiresInvitePermission(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "invite-permission-owner@example.com", "owner-pass", "Invite Permission Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Permission Workspace")

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/invites", "", nil, http.StatusUnauthorized)
	_, _ = authTestCreateInvite(t, router, owner.Token, workspace.ID)

	member := authTestRegister(t, router, "invite-permission-member@example.com", "owner-pass", "Invite Permission Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "invite-member")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/invites", member.Token, nil, http.StatusForbidden)

	outsider := authTestRegister(t, router, "invite-permission-outsider@example.com", "owner-pass", "Invite Permission Outsider")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/invites", outsider.Token, nil, http.StatusForbidden)

	admin := authTestRegister(t, router, "invite-permission-admin@example.com", "owner-pass", "Invite Permission Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "invite-admin")
	if _, err := server.sqlDB().Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}
	_, _ = authTestCreateInvite(t, router, admin.Token, workspace.ID)
}

func TestWorkspaceInvitePreviewAndAcceptRejectInvalidExpiredTokens(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "invite-expired-owner@example.com", "owner-pass", "Invite Expired Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Expired Workspace")
	joiner := authTestRegister(t, router, "invite-expired-joiner@example.com", "join-pass", "Invite Expired Joiner")

	authTestErrorContains(t, router, http.MethodGet, "/api/invites/not-a-real-token", "", nil, http.StatusNotFound, "Invalid invite link.")
	authTestErrorContains(t, router, http.MethodPost, "/api/invites/not-a-real-token/accept", joiner.Token, AcceptWorkspaceInviteRequest{Handle: "joiner"}, http.StatusNotFound, "Invalid invite link.")

	invite, token := authTestCreateInvite(t, router, owner.Token, workspace.ID)
	if _, err := server.sqlDB().Exec(`UPDATE workspace_invites SET expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, invite.Invite.ID); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	previewRecorder := authTestRequest(t, router, http.MethodGet, "/api/invites/"+token, "", nil, nil)
	if previewRecorder.Code != http.StatusGone {
		t.Fatalf("expired preview expected status %d, got %d body=%s", http.StatusGone, previewRecorder.Code, previewRecorder.Body.String())
	}
	if strings.Contains(previewRecorder.Body.String(), workspace.ID) || strings.Contains(previewRecorder.Body.String(), "Invite Expired Workspace") {
		t.Fatalf("expired invite preview leaked workspace metadata: %s", previewRecorder.Body.String())
	}
	authTestErrorContains(t, router, http.MethodPost, "/api/invites/"+token+"/accept", joiner.Token, AcceptWorkspaceInviteRequest{Handle: "joiner"}, http.StatusGone, "expired")
}

func TestWorkspaceInviteAcceptValidatesHandle(t *testing.T) {
	router := newAuthTestRouter(t)

	owner := authTestRegister(t, router, "invite-handle-owner@example.com", "owner-pass", "Invite Handle Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Handle Workspace")
	existing := authTestRegister(t, router, "invite-handle-existing@example.com", "member-pass", "Invite Existing Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, existing.Account.Email, "taken-handle")
	joiner := authTestRegister(t, router, "invite-handle-joiner@example.com", "join-pass", "Invite Handle Joiner")
	_, token := authTestCreateInvite(t, router, owner.Token, workspace.ID)

	authTestErrorContains(t, router, http.MethodPost, "/api/invites/"+token+"/accept", joiner.Token, nil, http.StatusBadRequest, "Handle is required.")
	authTestErrorContains(t, router, http.MethodPost, "/api/invites/"+token+"/accept", joiner.Token, AcceptWorkspaceInviteRequest{Handle: "Bad Handle"}, http.StatusBadRequest, "Handle can only contain lowercase letters")
	authTestErrorContains(t, router, http.MethodPost, "/api/invites/"+token+"/accept", joiner.Token, AcceptWorkspaceInviteRequest{Handle: "taken-handle"}, http.StatusBadRequest, "Handle is already taken.")
}

func TestWorkspaceInviteRouteRequiresManagementRole(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "invite-owner@example.com", "owner-pass", "Invite Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Invite Workspace")

	member := authTestRegister(t, router, "invite-member@example.com", "owner-pass", "Invite Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "member-handle")

	invitedByMember := authTestRegister(t, router, "invite-member-target@example.com", "owner-pass", "Invite Member Target")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", member.Token, AddWorkspaceMemberRequest{
		Email:  invitedByMember.Account.Email,
		Handle: "blocked-member",
	}, http.StatusForbidden)

	admin := authTestRegister(t, router, "invite-admin@example.com", "owner-pass", "Invite Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "admin-handle")
	if _, err := server.sqlDB().Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}
	adminInviteTarget := authTestRegister(t, router, "invite-admin-target@example.com", "owner-pass", "Invite Admin Target")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", admin.Token, AddWorkspaceMemberRequest{
		Email:  adminInviteTarget.Account.Email,
		Handle: "admin-invite",
	}, http.StatusCreated)

	ownerInviteTarget := authTestRegister(t, router, "invite-owner-target@example.com", "owner-pass", "Invite Owner Target")
	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/members", owner.Token, AddWorkspaceMemberRequest{
		Email:  ownerInviteTarget.Account.Email,
		Handle: "owner-invite",
	}, http.StatusCreated)
}

func TestWorkspaceAdminOnlyActionsRejectMembersAndAllowAdmins(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "admin-only-owner@example.com", "owner-pass", "Admin Only Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Admin Only Workspace")

	admin := authTestRegister(t, router, "admin-only-admin@example.com", "owner-pass", "Admin Only Admin")
	authTestAddMember(t, router, owner.Token, workspace.ID, admin.Account.Email, "admin-handle")
	if _, err := server.sqlDB().Exec(
		`UPDATE workspace_members SET membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, admin.Account.ID,
	); err != nil {
		t.Fatalf("promote member to admin: %v", err)
	}

	member := authTestRegister(t, router, "admin-only-member@example.com", "owner-pass", "Admin Only Member")
	authTestAddMember(t, router, owner.Token, workspace.ID, member.Account.Email, "member-handle")

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", member.Token, CreateDaemonRequest{
		Name: "member-daemon",
	}, http.StatusForbidden)

	var ownerDaemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{
		Name: "owner-daemon",
	}, http.StatusCreated, &ownerDaemon)
	authTestReportCodexRuntime(t, router, workspace.ID, ownerDaemon.Token)

	var ownerAgent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+ownerDaemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "owner-agent",
		Name:   "Owner Agent",
		Role:   "Owner runs admin-only actions",
		Kind:   "codex",
	}, http.StatusCreated, &ownerAgent)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+ownerDaemon.Daemon.ID+"/agents", member.Token, CreateAgentRequest{
		Handle: "member-agent",
		Name:   "Member Agent",
		Role:   "Blocked",
		Kind:   "codex",
	}, http.StatusForbidden)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/agents", member.Token, CreateAgentRequest{
		DaemonID: ownerDaemon.Daemon.ID,
		Handle:   "member-agent-2",
		Name:     "Member Agent 2",
		Role:     "Blocked",
		Kind:     "codex",
	}, http.StatusForbidden)

	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+ownerAgent.ID, member.Token, UpdateAgentRequest{
		Name: "Blocked patch",
	}, http.StatusForbidden)

	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/agents/"+ownerAgent.ID, member.Token, nil, http.StatusForbidden)
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+workspace.ID+"/daemons/"+ownerDaemon.Daemon.ID, member.Token, nil, http.StatusForbidden)

	var adminDaemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", admin.Token, CreateDaemonRequest{
		Name: "admin-daemon",
	}, http.StatusCreated, &adminDaemon)
	authTestReportCodexRuntime(t, router, workspace.ID, adminDaemon.Token)

	authTestStatus(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+adminDaemon.Daemon.ID+"/agents", admin.Token, CreateAgentRequest{
		Handle: "admin-agent",
		Name:   "Admin Agent",
		Role:   "Allowed",
		Kind:   "codex",
	}, http.StatusCreated)
}

type authTestWorkspace struct {
	ID             string
	RootDocumentID string
	OwnerUserID    string
}

type authTestEmailSender struct {
	mu            sync.Mutex
	messages      []EmailMessage
	failBySubject map[string]error
	blockUntil    <-chan struct{}
	attempts      chan authTestEmailAttempt
	sent          chan EmailMessage
	failures      chan authTestEmailFailure
}

type authTestEmailAttempt struct {
	Message     EmailMessage
	HasDeadline bool
	Deadline    time.Time
	ErrAtStart  error
}

type authTestEmailFailure struct {
	Message EmailMessage
	Err     error
}

type authTestRouter struct {
	http.Handler
	emailSender *authTestEmailSender
}

func newAuthTestEmailSender() *authTestEmailSender {
	return &authTestEmailSender{
		attempts: make(chan authTestEmailAttempt, 1024),
		sent:     make(chan EmailMessage, 1024),
		failures: make(chan authTestEmailFailure, 1024),
	}
}

func (s *authTestEmailSender) SendEmail(ctx context.Context, message EmailMessage) error {
	deadline, hasDeadline := ctx.Deadline()
	s.attempts <- authTestEmailAttempt{
		Message:     message,
		HasDeadline: hasDeadline,
		Deadline:    deadline,
		ErrAtStart:  ctx.Err(),
	}
	if s.blockUntil != nil {
		<-s.blockUntil
	}
	s.mu.Lock()
	err := s.failBySubject[message.Subject]
	if err == nil {
		s.messages = append(s.messages, message)
	}
	s.mu.Unlock()
	if err != nil {
		s.failures <- authTestEmailFailure{Message: message, Err: err}
		return err
	}
	s.sent <- message
	return nil
}

func (s *authTestEmailSender) snapshot() []EmailMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]EmailMessage(nil), s.messages...)
}

func (s *authTestEmailSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

func authTestWaitForEmailCount(t *testing.T, sender *authTestEmailSender, want int) []EmailMessage {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		messages := sender.snapshot()
		if len(messages) >= want {
			return messages
		}
		select {
		case <-sender.sent:
		case <-deadline.C:
			t.Fatalf("email messages sent = %d, want at least %d", len(messages), want)
		}
	}
}

func authTestWaitForEmailAttempt(t *testing.T, sender *authTestEmailSender, subject string) authTestEmailAttempt {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case attempt := <-sender.attempts:
			if attempt.Message.Subject == subject {
				return attempt
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for email attempt with subject %q", subject)
		}
	}
}

func authTestWaitForEmailFailure(t *testing.T, sender *authTestEmailSender, subject string) authTestEmailFailure {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case failure := <-sender.failures:
			if failure.Message.Subject == subject {
				return failure
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for email failure with subject %q", subject)
		}
	}
}

func authTestAssertAsyncEmailContext(t *testing.T, attempt authTestEmailAttempt) {
	t.Helper()
	if attempt.ErrAtStart != nil {
		t.Fatalf("email context was already canceled before send: %v", attempt.ErrAtStart)
	}
	if !attempt.HasDeadline {
		t.Fatalf("email send context has no deadline")
	}
	if remaining := time.Until(attempt.Deadline); remaining <= 0 || remaining > 11*time.Second {
		t.Fatalf("email send deadline remaining = %v, want fresh bounded timeout", remaining)
	}
}

type authTestLogBuffer struct {
	mu     sync.Mutex
	text   strings.Builder
	writes chan struct{}
}

func authTestCaptureLogs(t *testing.T) *authTestLogBuffer {
	t.Helper()
	buffer := &authTestLogBuffer{writes: make(chan struct{}, 128)}
	previousLogOutput := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() {
		log.SetOutput(previousLogOutput)
	})
	return buffer
}

func (b *authTestLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	_, _ = b.text.Write(p)
	b.mu.Unlock()
	select {
	case b.writes <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (b *authTestLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

func (b *authTestLogBuffer) waitContains(t *testing.T, want ...string) string {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		got := b.String()
		all := true
		for _, value := range want {
			if !strings.Contains(got, value) {
				all = false
				break
			}
		}
		if all {
			return got
		}
		select {
		case <-b.writes:
		case <-deadline.C:
			t.Fatalf("timed out waiting for log fields %v; got %q", want, got)
		}
	}
}

func authTestEmailToken(t *testing.T, message EmailMessage, wantPath ...string) string {
	t.Helper()
	if len(wantPath) > 1 {
		t.Fatalf("authTestEmailToken received %d expected paths, want at most 1", len(wantPath))
	}
	for _, line := range strings.Split(message.Text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "token=") {
			continue
		}
		parsed, err := url.Parse(line)
		if err != nil {
			t.Fatalf("parse email link %q: %v", line, err)
		}
		if len(wantPath) == 1 && parsed.Path != wantPath[0] {
			t.Fatalf("email link path = %q, want %q in %q", parsed.Path, wantPath[0], line)
		}
		token := parsed.Query().Get("token")
		if token == "" {
			t.Fatalf("email link %q did not include token", line)
		}
		return token
	}
	t.Fatalf("email message did not include token link: %#v", message)
	return ""
}

func authTestLatestEmailToken(t *testing.T, messages []EmailMessage, start int, wantPath string) string {
	t.Helper()
	if start < 0 || start > len(messages) {
		t.Fatalf("invalid email message start index %d for %d messages", start, len(messages))
	}
	for i := len(messages) - 1; i >= start; i-- {
		token, ok := authTestEmailTokenForPath(t, messages[i], wantPath)
		if ok {
			return token
		}
	}
	t.Fatalf("email messages[%d:] did not include token link for path %q: %#v", start, wantPath, messages[start:])
	return ""
}

func authTestEmailTokenForPath(t *testing.T, message EmailMessage, wantPath string) (string, bool) {
	t.Helper()
	for _, line := range strings.Split(message.Text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "token=") {
			continue
		}
		parsed, err := url.Parse(line)
		if err != nil {
			t.Fatalf("parse email link %q: %v", line, err)
		}
		if parsed.Path != wantPath {
			continue
		}
		token := parsed.Query().Get("token")
		if token == "" {
			t.Fatalf("email link %q did not include token", line)
		}
		return token, true
	}
	return "", false
}

func authTestExpireAccountEmailToken(t *testing.T, db *sql.DB, rawToken string, purpose string) {
	t.Helper()
	result, err := db.Exec(
		`UPDATE account_email_tokens SET expires_at = NOW() - INTERVAL '1 hour' WHERE token_hash = $1 AND purpose = $2`,
		tokenHash(rawToken),
		purpose,
	)
	if err != nil {
		t.Fatalf("expire account email token: %v", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("expired token rows = %d, err = %v; want 1 nil", count, err)
	}
}

func authTestBackdateAccountEmailToken(t *testing.T, db *sql.DB, rawToken string, purpose string, age time.Duration) {
	t.Helper()
	result, err := db.Exec(
		`UPDATE account_email_tokens SET created_at = NOW() - make_interval(secs => $1) WHERE token_hash = $2 AND purpose = $3`,
		int64(age/time.Second),
		tokenHash(rawToken),
		purpose,
	)
	if err != nil {
		t.Fatalf("backdate account email token: %v", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("backdated token rows = %d, err = %v; want 1 nil", count, err)
	}
}

func authTestEmailSenderForRouter(t *testing.T, router http.Handler) *authTestEmailSender {
	t.Helper()
	testRouter, ok := router.(*authTestRouter)
	if !ok || testRouter.emailSender == nil {
		t.Fatalf("expected auth test router with email sender")
	}
	return testRouter.emailSender
}

func newAuthTestRouter(t *testing.T) http.Handler {
	t.Helper()
	_, router := newAuthTestServer(t)
	return router
}

func newAuthTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	database := newPostgresTestDatabase(t)
	server := NewServer(Config{DatabaseURL: database.URL, JWTSecret: "test-secret"}, database)
	emailSender := newAuthTestEmailSender()
	server.emailSender = emailSender
	return server, &authTestRouter{
		Handler:     server.Routes(),
		emailSender: emailSender,
	}
}

func authTestRegister(t *testing.T, router http.Handler, email string, password string, name string) AuthResponse {
	t.Helper()
	sender := authTestEmailSenderForRouter(t, router)
	initialMessages := sender.count()
	var response AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/register", "", RegisterRequest{
		Email:       email,
		Password:    password,
		DisplayName: name,
	}, http.StatusCreated, &response)
	if response.Account == nil {
		t.Fatalf("expected register account response, got %#v", response)
	}
	if response.Token != "" {
		return response
	}
	messages := authTestWaitForEmailCount(t, sender, initialMessages+1)
	if len(messages) <= initialMessages {
		t.Fatalf("register did not send verification email")
	}
	verifyToken := authTestLatestEmailToken(t, messages, initialMessages, "/account/verify-email")
	var verifyResponse struct {
		Account *Account `json:"account"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusOK, &verifyResponse)
	if verifyResponse.Account == nil || !verifyResponse.Account.EmailVerified {
		t.Fatalf("expected verified auth test account, got %#v", verifyResponse.Account)
	}
	authTestWaitForEmailCount(t, sender, initialMessages+2)
	var loginResponse AuthResponse
	authTestJSON(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    email,
		Password: password,
	}, http.StatusOK, &loginResponse)
	if loginResponse.Token == "" || loginResponse.Account == nil {
		t.Fatalf("expected login auth response, got %#v", loginResponse)
	}
	return loginResponse
}

func authTestCreateWorkspace(t *testing.T, router http.Handler, token string, name string) authTestWorkspace {
	t.Helper()
	var response struct {
		Workspace Workspace       `json:"workspace"`
		Member    WorkspaceMember `json:"member"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces", token, CreateWorkspaceRequest{
		Name:   name,
		Slug:   authTestIdentifierFromName(name, 64),
		Handle: "owner",
	}, http.StatusCreated, &response)
	if response.Workspace.ID == "" || response.Member.UserID == "" {
		t.Fatalf("expected workspace response, got %#v", response)
	}
	return authTestWorkspace{ID: response.Workspace.ID, RootDocumentID: response.Workspace.RootDocumentID, OwnerUserID: response.Member.UserID}
}

func authTestIdentifierFromName(name string, maxLen int) string {
	value := strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	lastSeparator := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastSeparator = false
			continue
		}
		if r == '_' || r == '-' || r == ' ' {
			if !lastSeparator && builder.Len() > 0 {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
	}
	identifier := strings.Trim(builder.String(), "-")
	if len(identifier) > maxLen {
		identifier = strings.Trim(identifier[:maxLen], "-")
	}
	if len(identifier) < 2 {
		return "ws"
	}
	return identifier
}

func authTestAddMember(t *testing.T, router http.Handler, token string, workspaceID string, email string, handle string) WorkspaceMember {
	t.Helper()
	var response struct {
		Member WorkspaceMember `json:"member"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspaceID+"/members", token, AddWorkspaceMemberRequest{
		Email:  email,
		Handle: handle,
	}, http.StatusCreated, &response)
	if response.Member.UserID == "" {
		t.Fatalf("expected member response, got %#v", response)
	}
	return response.Member
}

func authTestCreateInvite(t *testing.T, router http.Handler, token string, workspaceID string) (CreateWorkspaceInviteResponse, string) {
	t.Helper()
	var response CreateWorkspaceInviteResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspaceID+"/invites", token, nil, http.StatusCreated, &response)
	if response.Invite == nil || response.URL == "" {
		t.Fatalf("expected invite response, got %#v", response)
	}
	rawToken := strings.TrimPrefix(response.URL, "/invite/")
	if rawToken == response.URL || rawToken == "" {
		t.Fatalf("expected invite URL with raw token, got %q", response.URL)
	}
	return response, rawToken
}

func authTestReportCodexRuntime(t *testing.T, router http.Handler, workspaceID string, daemonToken string) {
	t.Helper()
	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspaceID+"/daemon/status", daemonToken, UpdateDaemonStatusRequest{
		Version: "0.62.0",
		OS:      "linux",
		Arch:    "arm64",
		Runtimes: []RuntimeDetection{{
			Kind:      "codex",
			Available: true,
			Version:   "codex-cli 0.134.0",
			Path:      "/usr/local/bin/codex",
		}},
	}, http.StatusOK)
}

func authTestCreateDocument(t *testing.T, router http.Handler, token string, workspaceID string, path string, content string) DocumentMetadata {
	t.Helper()
	var document DocumentMetadata
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspaceID+"/documents", token, CreateDocumentRequest{}, http.StatusCreated, &document)
	if document.ID == "" {
		t.Fatalf("expected document response, got %#v", document)
	}
	return document
}

func authTestStatus(t *testing.T, router http.Handler, method string, target string, token string, body any, want int) {
	t.Helper()
	authTestStatusWithHeaders(t, router, method, target, token, nil, body, want)
}

func authTestStatusWithHeaders(t *testing.T, router http.Handler, method string, target string, token string, headers map[string]string, body any, want int) {
	t.Helper()
	recorder := authTestRequest(t, router, method, target, token, headers, body)
	if recorder.Code != want {
		t.Fatalf("%s %s expected status %d, got %d body=%s", method, target, want, recorder.Code, recorder.Body.String())
	}
}

func authTestJSON(t *testing.T, router http.Handler, method string, target string, token string, body any, want int, out any) {
	t.Helper()
	authTestJSONWithHeaders(t, router, method, target, token, nil, body, want, out)
}

func authTestJSONWithHeaders(t *testing.T, router http.Handler, method string, target string, token string, headers map[string]string, body any, want int, out any) {
	t.Helper()
	recorder := authTestRequest(t, router, method, target, token, headers, body)
	if recorder.Code != want {
		t.Fatalf("%s %s expected status %d, got %d body=%s", method, target, want, recorder.Code, recorder.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response for %s %s: %v body=%s", method, target, err, recorder.Body.String())
		}
	}
}

func authTestErrorContains(t *testing.T, router http.Handler, method string, target string, token string, body any, want int, wantError string) {
	t.Helper()
	var response struct {
		Error string `json:"error"`
	}
	authTestJSON(t, router, method, target, token, body, want, &response)
	if !strings.Contains(response.Error, wantError) {
		t.Fatalf("%s %s expected error containing %q, got %q", method, target, wantError, response.Error)
	}
}

type authTestHTTPResult struct {
	code int
	body string
	err  error
}

func authTestServeJSONNoFatal(router http.Handler, method string, target string, body any, out any) authTestHTTPResult {
	payload := []byte(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return authTestHTTPResult{err: err}
		}
		payload = encoded
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	router.ServeHTTP(recorder, request)
	result := authTestHTTPResult{code: recorder.Code, body: recorder.Body.String()}
	if out != nil {
		if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
			result.err = err
		}
	}
	return result
}

func authTestRequest(t *testing.T, router http.Handler, method string, target string, token string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload := []byte(nil)
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

// A daemon check-in must broadcast a daemon.updated event carrying the freshly
// computed connection status. This is the backend half of the "status shows up
// live, no refresh" contract: if the publish is ever dropped, the frontend can
// only recover the status on a full snapshot refetch — the exact bug we fixed.
func TestDaemonStatusCheckInPublishesDaemonUpdatedEvent(t *testing.T) {
	server, router := newAuthTestServer(t)

	owner := authTestRegister(t, router, "daemon-live-status@example.com", "owner-pass", "Daemon Live Status")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Daemon Live Status Tenant")

	var daemonResponse CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Install daemon"}, http.StatusCreated, &daemonResponse)

	// Subscribe after creation so we isolate the status check-in from daemon.created.
	events, unsubscribe := server.workspaceBroker(workspace.ID).Subscribe()
	defer unsubscribe()

	authTestStatus(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/daemon/status", daemonResponse.Token, UpdateDaemonStatusRequest{
		Version: "0.63.0",
		OS:      "linux",
		Arch:    "arm64",
	}, http.StatusOK)

	var updated *Daemon
	for draining := true; draining; {
		select {
		case event := <-events:
			if event.Type == "daemon.updated" {
				daemon, ok := event.Data.(*Daemon)
				if !ok {
					t.Fatalf("daemon.updated event carried %T, want *Daemon", event.Data)
				}
				updated = daemon
			}
		default:
			draining = false
		}
	}

	if updated == nil {
		t.Fatal("PATCH /daemon/status must publish a daemon.updated event so the frontend reflects check-ins without a refresh")
	}
	if updated.ID != daemonResponse.Daemon.ID {
		t.Fatalf("daemon.updated carried daemon %q, want %q", updated.ID, daemonResponse.Daemon.ID)
	}
	if updated.ConnectionStatus != "online" {
		t.Fatalf("daemon.updated must carry the fresh connectionStatus the UI renders, got %q want online", updated.ConnectionStatus)
	}
}
