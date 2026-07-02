package notty

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
	if len(sender.messages) != 1 {
		t.Fatalf("verification emails sent = %d, want 1", len(sender.messages))
	}

	var loginError map[string]string
	authTestJSON(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    "verify-flow@example.com",
		Password: "verify-pass",
	}, http.StatusForbidden, &loginError)
	if loginError["error"] != errEmailNotVerified.Error() {
		t.Fatalf("login error = %#v, want email_not_verified", loginError)
	}

	verifyToken := authTestEmailToken(t, sender.messages[0], "/account/verify-email")
	var verifyResponse struct {
		Account *Account `json:"account"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusOK, &verifyResponse)
	if verifyResponse.Account == nil || !verifyResponse.Account.EmailVerified {
		t.Fatalf("expected verified account, got %#v", verifyResponse.Account)
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
	initialMessages := len(sender.messages)
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: unverified,
	}, http.StatusOK, nil)
	if len(sender.messages) != initialMessages {
		t.Fatalf("forgot-password sent reset email for unverified account")
	}

	auth := authTestRegister(t, router, verified, "old-pass", "Reset Verified")
	initialMessages = len(sender.messages)
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: verified,
	}, http.StatusOK, nil)
	if len(sender.messages) != initialMessages+1 {
		t.Fatalf("forgot-password emails sent = %d, want %d", len(sender.messages), initialMessages+1)
	}
	resetToken := authTestEmailToken(t, sender.messages[len(sender.messages)-1], "/account/reset-password")
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
	verifyToken := authTestEmailToken(t, sender.messages[0], "/account/verify-email")

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
	verifyToken := authTestEmailToken(t, sender.messages[len(sender.messages)-1], "/account/verify-email")
	authTestExpireAccountEmailToken(t, server.store.db, verifyToken, accountEmailTokenPurposeVerifyEmail)
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusBadRequest, nil)
	authTestStatus(t, router, http.MethodPost, "/api/auth/login", "", LoginRequest{
		Email:    "expired-verify@example.com",
		Password: "verify-pass",
	}, http.StatusForbidden)

	authTestRegister(t, router, "expired-reset@example.com", "old-pass", "Expired Reset")
	initialMessages := len(sender.messages)
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "expired-reset@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != initialMessages+1 {
		t.Fatalf("forgot-password emails sent = %d, want %d", len(sender.messages), initialMessages+1)
	}
	resetToken := authTestEmailToken(t, sender.messages[len(sender.messages)-1], "/account/reset-password")
	authTestExpireAccountEmailToken(t, server.store.db, resetToken, accountEmailTokenPurposeResetPassword)
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
	if len(sender.messages) != 1 {
		t.Fatalf("verification emails sent = %d, want 1", len(sender.messages))
	}
	firstVerifyToken := authTestEmailToken(t, sender.messages[0], "/account/verify-email")

	authTestJSON(t, router, http.MethodPost, "/api/auth/resend-verification", "", ResendVerificationRequest{
		Email: "resend-state@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != 1 {
		t.Fatalf("resend inside cooldown sent email count = %d, want 1", len(sender.messages))
	}

	authTestBackdateAccountEmailToken(t, server.store.db, firstVerifyToken, accountEmailTokenPurposeVerifyEmail, accountEmailTokenCooldown+time.Minute)
	authTestJSON(t, router, http.MethodPost, "/api/auth/resend-verification", "", ResendVerificationRequest{
		Email: "resend-state@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != 2 {
		t.Fatalf("resend after cooldown sent email count = %d, want 2", len(sender.messages))
	}
	secondVerifyToken := authTestEmailToken(t, sender.messages[1], "/account/verify-email")
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: firstVerifyToken,
	}, http.StatusBadRequest, nil)
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: secondVerifyToken,
	}, http.StatusOK, nil)

	authTestJSON(t, router, http.MethodPost, "/api/auth/resend-verification", "", ResendVerificationRequest{
		Email: "resend-state@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != 2 {
		t.Fatalf("resend for verified account sent email count = %d, want 2", len(sender.messages))
	}

	authTestRegister(t, router, "reset-cooldown@example.com", "old-pass", "Reset Cooldown")
	initialMessages := len(sender.messages)
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "reset-cooldown@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != initialMessages+1 {
		t.Fatalf("forgot-password emails sent = %d, want %d", len(sender.messages), initialMessages+1)
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "reset-cooldown@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != initialMessages+1 {
		t.Fatalf("forgot-password inside cooldown sent email count = %d, want %d", len(sender.messages), initialMessages+1)
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/forgot-password", "", ForgotPasswordRequest{
		Email: "missing-reset@example.com",
	}, http.StatusOK, nil)
	if len(sender.messages) != initialMessages+1 {
		t.Fatalf("forgot-password for nonexistent account sent email count = %d, want %d", len(sender.messages), initialMessages+1)
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

func TestEmailVerifiedMigrationBackfillsExistingAccountsButFutureDefaultIsFalse(t *testing.T) {
	db, err := sql.Open("pgx", postgresTestDSN(t))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE IF EXISTS account_email_tokens`); err != nil {
		t.Fatalf("drop email tokens: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS accounts`); err != nil {
		t.Fatalf("drop accounts: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			last_accessed_workspace_id TEXT NOT NULL DEFAULT '',
			password_updated_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`); err != nil {
		t.Fatalf("create legacy accounts: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO accounts (id, email, display_name, password_hash, last_accessed_workspace_id, password_updated_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, '', $5, $5, $5)`,
		"account_legacy",
		"legacy-verified@example.com",
		"Legacy",
		"hash",
		now,
	); err != nil {
		t.Fatalf("insert legacy account: %v", err)
	}
	if err := initPostgresSchemaTables(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	var existingVerified bool
	if err := db.QueryRow(`SELECT email_verified FROM accounts WHERE id = $1`, "account_legacy").Scan(&existingVerified); err != nil {
		t.Fatalf("select existing email_verified: %v", err)
	}
	if !existingVerified {
		t.Fatalf("existing account was not backfilled as verified")
	}
	if _, err := db.Exec(
		`INSERT INTO accounts (id, email, display_name, password_hash, last_accessed_workspace_id, password_updated_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, '', $5, $5, $5)`,
		"account_future",
		"future-unverified@example.com",
		"Future",
		"hash",
		now,
	); err != nil {
		t.Fatalf("insert future account: %v", err)
	}
	var futureVerified bool
	if err := db.QueryRow(`SELECT email_verified FROM accounts WHERE id = $1`, "account_future").Scan(&futureVerified); err != nil {
		t.Fatalf("select future email_verified: %v", err)
	}
	if futureVerified {
		t.Fatalf("future account defaulted to verified")
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
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&workspaceCount); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	var memberCount int
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM workspace_members`).Scan(&memberCount); err != nil {
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
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = $1`, workspace.ID).Scan(&userCount); err != nil {
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
	router := newAuthTestRouter(t)

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

	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+workspace.ID+"/agents/"+agent.ID+"/session", daemonResponse.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "thread-1"}, http.StatusOK)
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
	store, err := server.workspaceStore(workspace.ID)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	if _, ok := store.Snapshot().Users[workspace.OwnerUserID]; !ok {
		t.Fatalf("expected owner user in loaded workspace store")
	}

	invite, token := authTestCreateInvite(t, router, owner.Token, workspace.ID)
	if invite.Invite == nil || invite.Invite.ID == "" || invite.Invite.WorkspaceID != workspace.ID {
		t.Fatalf("expected workspace invite response, got %#v", invite)
	}

	var tokenHashCount int
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM workspace_invites WHERE token_hash = $1`, tokenHash(token)).Scan(&tokenHashCount); err != nil {
		t.Fatalf("count token hash: %v", err)
	}
	if tokenHashCount != 1 {
		t.Fatalf("expected one hashed invite token row, got %d", tokenHashCount)
	}
	var rawTokenCount int
	if err := server.store.db.QueryRow(`SELECT COUNT(*) FROM workspace_invites WHERE token_hash = $1`, token).Scan(&rawTokenCount); err != nil {
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
	if err := server.store.db.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(membership_role), ''), COALESCE(MAX(user_id), '')
		   FROM workspace_members
		  WHERE workspace_id = $1 AND account_id = $2 AND status = 'active'`,
		workspace.ID, joiner.Account.ID,
	).Scan(&memberCount, &role, &joinedUserID); err != nil {
		t.Fatalf("count joined membership: %v", err)
	}
	if memberCount != 1 || role != MembershipRoleMember {
		t.Fatalf("expected one active member role, got count=%d role=%q", memberCount, role)
	}
	if user := store.Snapshot().Users[joinedUserID]; user == nil || user.Handle != "joiner" {
		t.Fatalf("accept should reload workspace store with joined user, got userID=%q snapshot=%#v", joinedUserID, store.Snapshot().Users)
	}
	var userCount int
	if err := server.store.db.QueryRow(
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
	if err := server.store.db.QueryRow(
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
	if _, err := server.store.db.Exec(
		`UPDATE workspace_members SET status = 'removed', membership_role = $1 WHERE workspace_id = $2 AND account_id = $3`,
		MembershipRoleAdmin, workspace.ID, member.Account.ID,
	); err != nil {
		t.Fatalf("mark membership removed: %v", err)
	}
	if _, err := server.store.db.Exec(
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
	if err := server.store.db.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(m.user_id), ''), COALESCE(MAX(m.membership_role), ''), COALESCE(MAX(m.status), ''), COALESCE(MAX(u.handle), ''), COALESCE(MAX(u.status), '')
		   FROM workspace_members m
		   JOIN users u ON u.workspace_id = m.workspace_id AND u.id = m.user_id
		  WHERE m.workspace_id = $1 AND m.account_id = $2`,
		workspace.ID, member.Account.ID,
	).Scan(&memberCount, &userID, &membershipRole, &memberStatus, &userHandle, &userStatus); err != nil {
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
	if _, err := server.store.db.Exec(
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
	if _, err := server.store.db.Exec(`UPDATE workspace_invites SET expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, invite.Invite.ID); err != nil {
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
	if _, err := server.store.db.Exec(
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
	if _, err := server.store.db.Exec(
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
	ID          string
	OwnerUserID string
}

type authTestEmailSender struct {
	messages []EmailMessage
}

type authTestRouter struct {
	http.Handler
	emailSender *authTestEmailSender
}

func (s *authTestEmailSender) SendEmail(_ context.Context, message EmailMessage) error {
	s.messages = append(s.messages, message)
	return nil
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
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := initPostgresSchema(db); err != nil {
		_ = db.Close()
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		_ = db.Close()
		t.Fatalf("clear tables: %v", err)
	}
	_ = db.Close()

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	server := NewServer(Config{DatabaseURL: dsn, JWTSecret: "test-secret"}, store)
	emailSender := &authTestEmailSender{}
	server.emailSender = emailSender
	return server, &authTestRouter{
		Handler:     server.Routes(),
		emailSender: emailSender,
	}
}

func authTestRegister(t *testing.T, router http.Handler, email string, password string, name string) AuthResponse {
	t.Helper()
	sender := authTestEmailSenderForRouter(t, router)
	initialMessages := len(sender.messages)
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
	if len(sender.messages) <= initialMessages {
		t.Fatalf("register did not send verification email")
	}
	verifyToken := authTestEmailToken(t, sender.messages[len(sender.messages)-1], "/account/verify-email")
	var verifyResponse struct {
		Account *Account `json:"account"`
	}
	authTestJSON(t, router, http.MethodPost, "/api/auth/verify-email", "", VerifyEmailRequest{
		Token: verifyToken,
	}, http.StatusOK, &verifyResponse)
	if verifyResponse.Account == nil || !verifyResponse.Account.EmailVerified {
		t.Fatalf("expected verified auth test account, got %#v", verifyResponse.Account)
	}
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
	return authTestWorkspace{ID: response.Workspace.ID, OwnerUserID: response.Member.UserID}
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
