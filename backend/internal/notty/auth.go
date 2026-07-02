package notty

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type authContextKey struct{}
type requestStoreKey struct{}
type requestBrokerKey struct{}
type requestWorkspaceIDKey struct{}

type AuthContext struct {
	WorkspaceID    string
	PrincipalID    string
	PrincipalKind  string
	AccountID      string
	UserID         string
	UserHandle     string
	UserName       string
	MembershipRole string
	DaemonID       string
	ActingAgentID  string
	ActingAgentRef string
}

type jwtClaims struct {
	Subject     string `json:"sub"`
	Email       string `json:"email"`
	DisplayName string `json:"name"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
	Issuer      string `json:"iss"`
}

var (
	errInvalidInvite       = errors.New("Invalid invite link.")
	errExpiredInvite       = errors.New("This invite link has expired. Ask the workspace admin for a new one.")
	errEmailNotVerified    = errors.New("email_not_verified")
	errInvalidAccountToken = errors.New("invalid or expired token")
)

const (
	accountEmailTokenPurposeVerifyEmail   = "verify_email"
	accountEmailTokenPurposeResetPassword = "reset_password"
	accountEmailTokenTTL                  = time.Hour
	accountEmailTokenCooldown             = 2 * time.Minute
)

func authFromContext(ctx context.Context) (*AuthContext, bool) {
	auth, ok := ctx.Value(authContextKey{}).(*AuthContext)
	return auth, ok && auth != nil
}

func contextWithAuth(ctx context.Context, auth *AuthContext) context.Context {
	return context.WithValue(ctx, authContextKey{}, auth)
}

func contextWithRequestStore(ctx context.Context, store *Store) context.Context {
	return context.WithValue(ctx, requestStoreKey{}, store)
}

func requestStoreFromContext(ctx context.Context) (*Store, bool) {
	store, ok := ctx.Value(requestStoreKey{}).(*Store)
	return store, ok && store != nil
}

func contextWithRequestBroker(ctx context.Context, broker *Broker) context.Context {
	return context.WithValue(ctx, requestBrokerKey{}, broker)
}

func requestBrokerFromContext(ctx context.Context) (*Broker, bool) {
	broker, ok := ctx.Value(requestBrokerKey{}).(*Broker)
	return broker, ok && broker != nil
}

func contextWithWorkspaceID(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, requestWorkspaceIDKey{}, workspaceID)
}

func workspaceIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(requestWorkspaceIDKey{}).(string)
	return strings.TrimSpace(value)
}

func operationMetaFromAuth(auth *AuthContext, tool string, fallbackID string, fallbackType string) OperationMeta {
	if auth == nil {
		return OperationMeta{ActorID: fallbackID, ActorType: fallbackType, Source: "api", Tool: tool}
	}
	actorID := auth.UserID
	actorType := "human"
	if auth.PrincipalKind == "daemon" {
		actorID = auth.DaemonID
		actorType = "daemon"
	}
	if auth.PrincipalKind == "agent" {
		actorID = firstNonEmptyString(auth.ActingAgentID, auth.ActingAgentRef)
		actorType = "agent"
	}
	if actorID == "" {
		actorID = auth.PrincipalID
	}
	return OperationMeta{
		ActorID:   actorID,
		ActorType: actorType,
		Source:    "api",
		Tool:      tool,
	}
}

func hashPassword(password string) (string, error) {
	password = strings.TrimSpace(password)
	if len(password) < 6 {
		return "", errors.New("password must be at least 6 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func verifyPassword(hash string, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func issueJWT(secret string, account *Account, ttl time.Duration) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("jwt secret is not configured")
	}
	if account == nil {
		return "", errors.New("account is required")
	}
	now := time.Now().UTC()
	claims := jwtClaims{
		Subject:     account.ID,
		Email:       account.Email,
		DisplayName: account.DisplayName,
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(ttl).Unix(),
		Issuer:      "codesk",
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	unsigned := encodedHeader + "." + encodedPayload
	signature := signJWT([]byte(secret), unsigned)
	return unsigned + "." + signature, nil
}

func verifyJWT(secret string, token string) (*jwtClaims, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("jwt secret is not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt")
	}
	unsigned := parts[0] + "." + parts[1]
	expected := signJWT([]byte(secret), unsigned)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return nil, errors.New("invalid jwt signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	if claims.Issuer != "codesk" {
		return nil, errors.New("invalid jwt issuer")
	}
	if claims.Subject == "" || claims.ExpiresAt <= time.Now().UTC().Unix() {
		return nil, errors.New("jwt expired")
	}
	return &claims, nil
}

func signJWT(secret []byte, unsigned string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(unsigned))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomToken(prefix string) (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func accountFromRow(row *sql.Row) (*Account, error) {
	account := &Account{}
	if err := row.Scan(
		&account.ID,
		&account.Email,
		&account.DisplayName,
		&account.EmailVerified,
		&account.LastAccessedWorkspaceID,
		&account.CreatedAt,
		&account.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return account, nil
}

func registerAccount(db *sql.DB, req RegisterRequest) (*Account, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	email := normalizeEmail(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		return nil, errors.New("valid email is required")
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = email
	}
	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	account := &Account{
		ID:                      "account_" + uuid.NewString(),
		Email:                   email,
		DisplayName:             displayName,
		EmailVerified:           false,
		LastAccessedWorkspaceID: "",
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	_, err = db.Exec(
		`INSERT INTO accounts (id, email, display_name, password_hash, email_verified, last_accessed_workspace_id, password_updated_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		account.ID,
		account.Email,
		account.DisplayName,
		passwordHash,
		account.EmailVerified,
		account.LastAccessedWorkspaceID,
		now,
		now,
		now,
	)
	if err != nil {
		return nil, err
	}
	return account, nil
}

func authenticateAccount(db *sql.DB, req LoginRequest) (*Account, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	email := normalizeEmail(req.Email)
	var account Account
	var passwordHash string
	err := db.QueryRow(
		`SELECT id, email, display_name, password_hash, email_verified, last_accessed_workspace_id, created_at, updated_at
		   FROM accounts
		  WHERE email = $1`,
		email,
	).Scan(
		&account.ID,
		&account.Email,
		&account.DisplayName,
		&passwordHash,
		&account.EmailVerified,
		&account.LastAccessedWorkspaceID,
		&account.CreatedAt,
		&account.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, errors.New("invalid email or password")
	}
	if err != nil {
		return nil, err
	}
	if !verifyPassword(passwordHash, req.Password) {
		return nil, errors.New("invalid email or password")
	}
	return &account, nil
}

func getAccountByID(db *sql.DB, accountID string) (*Account, error) {
	return accountFromRow(db.QueryRow(
		`SELECT id, email, display_name, email_verified, last_accessed_workspace_id, created_at, updated_at FROM accounts WHERE id = $1`,
		strings.TrimSpace(accountID),
	))
}

func createAccountEmailToken(db *sql.DB, accountID string, purpose string, ttl time.Duration, cooldown time.Duration) (string, bool, error) {
	if db == nil {
		return "", false, errors.New("database is required")
	}
	accountID = strings.TrimSpace(accountID)
	purpose = strings.TrimSpace(purpose)
	if accountID == "" || purpose == "" {
		return "", false, errors.New("account and token purpose are required")
	}
	now := time.Now().UTC()
	if cooldown > 0 {
		var createdAt time.Time
		err := db.QueryRow(
			`SELECT created_at
			   FROM account_email_tokens
			  WHERE account_id = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > $3
			  ORDER BY created_at DESC
			  LIMIT 1`,
			accountID,
			purpose,
			now,
		).Scan(&createdAt)
		if err != nil && err != sql.ErrNoRows {
			return "", false, err
		}
		if err == nil && now.Sub(createdAt) < cooldown {
			return "", false, nil
		}
	}
	token, err := randomToken("")
	if err != nil {
		return "", false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(
		`UPDATE account_email_tokens
		    SET consumed_at = $1
		  WHERE account_id = $2 AND purpose = $3 AND consumed_at IS NULL`,
		now,
		accountID,
		purpose,
	); err != nil {
		return "", false, err
	}
	if _, err = tx.Exec(
		`INSERT INTO account_email_tokens (id, account_id, purpose, token_hash, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		"account_email_token_"+uuid.NewString(),
		accountID,
		purpose,
		tokenHash(token),
		now.Add(ttl),
		now,
	); err != nil {
		return "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return token, true, nil
}

func verifyAccountEmailWithToken(db *sql.DB, rawToken string) (*Account, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	accountID, err := consumeAccountEmailTokenTx(tx, rawToken, accountEmailTokenPurposeVerifyEmail)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(
		`UPDATE accounts
		    SET email_verified = TRUE, updated_at = $1
		  WHERE id = $2`,
		now,
		accountID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return getAccountByID(db, accountID)
}

func resetAccountPasswordWithToken(db *sql.DB, rawToken string, password string) error {
	passwordHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accountID, err := consumeAccountEmailTokenTx(tx, rawToken, accountEmailTokenPurposeResetPassword)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := tx.Exec(
		`UPDATE accounts
		    SET password_hash = $1, password_updated_at = $2, updated_at = $2
		  WHERE id = $3 AND email_verified = TRUE`,
		passwordHash,
		now,
		accountID,
	)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count == 0 {
		return errInvalidAccountToken
	}
	return tx.Commit()
}

func consumeAccountEmailToken(db *sql.DB, rawToken string, purpose string) (string, error) {
	if db == nil {
		return "", errors.New("database is required")
	}
	rawToken = strings.TrimSpace(rawToken)
	purpose = strings.TrimSpace(purpose)
	if rawToken == "" || purpose == "" {
		return "", errInvalidAccountToken
	}
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	accountID, err := consumeAccountEmailTokenTx(tx, rawToken, purpose)
	if err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return accountID, nil
}

func consumeAccountEmailTokenTx(tx *sql.Tx, rawToken string, purpose string) (string, error) {
	now := time.Now().UTC()
	var id string
	var accountID string
	var expiresAt time.Time
	var consumedAt sql.NullTime
	err := tx.QueryRow(
		`SELECT id, account_id, expires_at, consumed_at
		   FROM account_email_tokens
		  WHERE token_hash = $1 AND purpose = $2
		  FOR UPDATE`,
		tokenHash(rawToken),
		purpose,
	).Scan(&id, &accountID, &expiresAt, &consumedAt)
	if err == sql.ErrNoRows {
		return "", errInvalidAccountToken
	}
	if err != nil {
		return "", err
	}
	if consumedAt.Valid || !expiresAt.After(now) {
		return "", errInvalidAccountToken
	}
	if _, err = tx.Exec(
		`UPDATE account_email_tokens SET consumed_at = $1 WHERE id = $2`,
		now,
		id,
	); err != nil {
		return "", err
	}
	return accountID, nil
}

func requestEmailVerification(db *sql.DB, account *Account, cooldown time.Duration) (string, bool, error) {
	if account == nil || account.EmailVerified {
		return "", false, nil
	}
	return createAccountEmailToken(db, account.ID, accountEmailTokenPurposeVerifyEmail, accountEmailTokenTTL, cooldown)
}

func requestPasswordReset(db *sql.DB, account *Account, cooldown time.Duration) (string, bool, error) {
	if account == nil || !account.EmailVerified {
		return "", false, nil
	}
	return createAccountEmailToken(db, account.ID, accountEmailTokenPurposeResetPassword, accountEmailTokenTTL, cooldown)
}

func listWorkspacesForAccount(db *sql.DB, accountID string) ([]*Workspace, error) {
	rows, err := db.Query(
		`SELECT w.id, w.slug, w.name, m.last_accessed_document_id, w.created_at, w.updated_at
		   FROM workspaces w
		   JOIN workspace_members m ON m.workspace_id = w.id
		  WHERE m.account_id = $1 AND m.status = 'active'
		  ORDER BY w.updated_at DESC, w.name ASC`,
		accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workspaces := []*Workspace{}
	for rows.Next() {
		workspace := &Workspace{}
		if err := rows.Scan(
			&workspace.ID,
			&workspace.Slug,
			&workspace.Name,
			&workspace.LastAccessedDocumentID,
			&workspace.CreatedAt,
			&workspace.UpdatedAt,
		); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, rows.Err()
}

func updateLastAccessedWorkspace(db *sql.DB, accountID string, workspaceID string, documentID string) error {
	accountID = strings.TrimSpace(accountID)
	workspaceID = strings.TrimSpace(workspaceID)
	documentID = strings.TrimSpace(documentID)
	if db == nil {
		return errors.New("database is required")
	}
	if accountID == "" || workspaceID == "" {
		return errors.New("account and workspace are required")
	}
	now := time.Now().UTC()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(
		`UPDATE accounts SET last_accessed_workspace_id = $1, updated_at = $2 WHERE id = $3`,
		workspaceID, now, accountID,
	); err != nil {
		return err
	}
	if documentID != "" {
		if _, err = tx.Exec(
			`UPDATE workspace_members
			    SET last_accessed_document_id = $1
			  WHERE workspace_id = $2 AND account_id = $3 AND status = 'active'`,
			documentID, workspaceID, accountID,
		); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func getWorkspace(db *sql.DB, workspaceID string) (*Workspace, error) {
	workspace := &Workspace{}
	err := db.QueryRow(
		`SELECT id, slug, name, created_at, updated_at FROM workspaces WHERE id = $1`,
		strings.TrimSpace(workspaceID),
	).Scan(&workspace.ID, &workspace.Slug, &workspace.Name, &workspace.CreatedAt, &workspace.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return workspace, nil
}

func createWorkspaceForAccount(db *sql.DB, account *Account, req CreateWorkspaceRequest) (*Workspace, *WorkspaceMember, error) {
	if db == nil || account == nil {
		return nil, nil, errors.New("account is required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, nil, errors.New("Workspace name is required.")
	}
	slug, err := validateWorkspaceSlug(req.Slug)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureWorkspaceSlugAvailable(db, slug); err != nil {
		return nil, nil, err
	}
	handle, err := validateHandle(req.Handle)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	workspace := &Workspace{
		ID:        "ws_" + uuid.NewString(),
		Slug:      slug,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	user := &User{
		ID:        "user_" + uuid.NewString(),
		Handle:    handle,
		Name:      firstNonEmptyString(account.DisplayName, account.Email),
		Role:      "Workspace owner",
		Kind:      "human",
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(
		`INSERT INTO workspaces (id, slug, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		workspace.ID, workspace.Slug, workspace.Name, workspace.CreatedAt, workspace.UpdatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, nil, errors.New("Workspace slug is already taken.")
		}
		return nil, nil, err
	}
	if _, err = tx.Exec(
		`INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		workspace.ID, user.ID, user.Handle, user.Name, user.Role, user.Kind, user.Status, user.CreatedAt, user.UpdatedAt,
	); err != nil {
		return nil, nil, err
	}
	member := &WorkspaceMember{
		WorkspaceID:    workspace.ID,
		AccountID:      account.ID,
		UserID:         user.ID,
		UserHandle:     user.Handle,
		UserName:       user.Name,
		MembershipRole: MembershipRoleOwner,
		Status:         "active",
		CreatedAt:      now,
		AcceptedAt:     now,
	}
	if _, err = tx.Exec(
		`INSERT INTO workspace_members (workspace_id, account_id, user_id, membership_role, status, invited_by, created_at, accepted_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		member.WorkspaceID, member.AccountID, member.UserID, member.MembershipRole, member.Status, member.InvitedBy, member.CreatedAt, member.AcceptedAt,
	); err != nil {
		return nil, nil, err
	}
	if _, err = tx.Exec(
		`UPDATE accounts SET last_accessed_workspace_id = $1, updated_at = $2 WHERE id = $3`,
		workspace.ID, now, account.ID,
	); err != nil {
		return nil, nil, err
	}
	account.LastAccessedWorkspaceID = workspace.ID
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return workspace, member, nil
}

func ensureWorkspaceSlugAvailable(db *sql.DB, slug string) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE slug = $1`, slug).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("Workspace slug is already taken.")
	}
	return nil
}

func addWorkspaceMember(db *sql.DB, workspaceID string, req AddWorkspaceMemberRequest, invitedBy string) (*WorkspaceMember, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	email := normalizeEmail(req.Email)
	if email == "" {
		return nil, errors.New("email is required")
	}
	account, err := getAccountByEmail(db, email)
	if err != nil {
		return nil, err
	}
	if _, err := getWorkspace(db, workspaceID); err != nil {
		return nil, err
	}
	if existing, err := workspaceMemberForAccount(db, workspaceID, account.ID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	handle, err := validateHandle(req.Handle)
	if err != nil {
		return nil, err
	}
	if err := ensurePostgresWorkspaceHandleAvailable(db, workspaceID, handle); err != nil {
		return nil, err
	}
	name := firstNonEmptyString(req.DisplayName, account.DisplayName, account.Email)
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "Workspace member"
	}
	user := &User{
		ID:        "user_" + uuid.NewString(),
		Handle:    handle,
		Name:      name,
		Role:      role,
		Kind:      "human",
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(
		`INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		workspaceID, user.ID, user.Handle, user.Name, user.Role, user.Kind, user.Status, user.CreatedAt, user.UpdatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, errors.New("Handle is already taken.")
		}
		return nil, err
	}
	member := &WorkspaceMember{
		WorkspaceID:    workspaceID,
		AccountID:      account.ID,
		UserID:         user.ID,
		UserHandle:     user.Handle,
		UserName:       user.Name,
		MembershipRole: MembershipRoleMember,
		Status:         "active",
		InvitedBy:      strings.TrimSpace(invitedBy),
		CreatedAt:      now,
		AcceptedAt:     now,
	}
	if _, err = tx.Exec(
		`INSERT INTO workspace_members (workspace_id, account_id, user_id, membership_role, status, invited_by, created_at, accepted_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		member.WorkspaceID, member.AccountID, member.UserID, member.MembershipRole, member.Status, member.InvitedBy, member.CreatedAt, member.AcceptedAt,
	); err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback()
			err = nil
			if existing, existingErr := workspaceMemberForAccount(db, workspaceID, account.ID); existingErr == nil {
				return existing, nil
			}
			return nil, errors.New("Account is already a workspace member.")
		}
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return member, nil
}

func createWorkspaceInvite(db *sql.DB, workspaceID string, createdByUserID string) (*WorkspaceInvite, string, error) {
	if db == nil {
		return nil, "", errors.New("database is required")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	createdByUserID = strings.TrimSpace(createdByUserID)
	if workspaceID == "" || createdByUserID == "" {
		return nil, "", errors.New("workspace and creator are required")
	}
	token, err := randomToken("")
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	invite := &WorkspaceInvite{
		ID:              "invite_" + uuid.NewString(),
		WorkspaceID:     workspaceID,
		CreatedByUserID: createdByUserID,
		ExpiresAt:       now.Add(7 * 24 * time.Hour),
		CreatedAt:       now,
	}
	if _, err := db.Exec(
		`INSERT INTO workspace_invites (id, workspace_id, token_hash, created_by_user_id, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		invite.ID, invite.WorkspaceID, tokenHash(token), invite.CreatedByUserID, invite.ExpiresAt, invite.CreatedAt,
	); err != nil {
		return nil, "", err
	}
	return invite, token, nil
}

func workspaceInvitePreview(db *sql.DB, token string) (*WorkspaceInvitePreviewResponse, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errInvalidInvite
	}
	var workspace WorkspaceInvitePreview
	var expiresAt time.Time
	err := db.QueryRow(
		`SELECT w.name, w.slug, i.expires_at
		   FROM workspace_invites i
		   JOIN workspaces w ON w.id = i.workspace_id
		  WHERE i.token_hash = $1`,
		tokenHash(token),
	).Scan(&workspace.Name, &workspace.Slug, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, errInvalidInvite
	}
	if err != nil {
		return nil, err
	}
	if !expiresAt.After(time.Now().UTC()) {
		return nil, errExpiredInvite
	}
	return &WorkspaceInvitePreviewResponse{Workspace: &workspace, ExpiresAt: expiresAt}, nil
}

func acceptWorkspaceInvite(db *sql.DB, token string, accountID string, req AcceptWorkspaceInviteRequest) (*Workspace, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	token = strings.TrimSpace(token)
	accountID = strings.TrimSpace(accountID)
	if token == "" {
		return nil, errInvalidInvite
	}
	if accountID == "" {
		return nil, errors.New("account is required")
	}
	now := time.Now().UTC()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var workspace Workspace
	var createdByUserID string
	var expiresAt time.Time
	err = tx.QueryRow(
		`SELECT w.id, w.slug, w.name, w.created_at, w.updated_at, i.created_by_user_id, i.expires_at
		   FROM workspace_invites i
		   JOIN workspaces w ON w.id = i.workspace_id
		  WHERE i.token_hash = $1
		  FOR UPDATE OF i`,
		tokenHash(token),
	).Scan(&workspace.ID, &workspace.Slug, &workspace.Name, &workspace.CreatedAt, &workspace.UpdatedAt, &createdByUserID, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, errInvalidInvite
	}
	if err != nil {
		return nil, err
	}
	if !expiresAt.After(now) {
		return nil, errExpiredInvite
	}

	var existingUserID string
	var existingStatus string
	err = tx.QueryRow(
		`SELECT user_id, status
		   FROM workspace_members
		  WHERE workspace_id = $1 AND account_id = $2
		  FOR UPDATE`,
		workspace.ID, accountID,
	).Scan(&existingUserID, &existingStatus)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil && existingStatus == "active" {
		if _, err = tx.Exec(
			`UPDATE accounts SET last_accessed_workspace_id = $1, updated_at = $2 WHERE id = $3`,
			workspace.ID, now, accountID,
		); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		committed = true
		return &workspace, nil
	}
	if err == nil {
		if err := reactivateWorkspaceInviteMemberTx(tx, workspace.ID, accountID, existingUserID, strings.TrimSpace(createdByUserID), now); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(
			`UPDATE accounts SET last_accessed_workspace_id = $1, updated_at = $2 WHERE id = $3`,
			workspace.ID, now, accountID,
		); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		committed = true
		return &workspace, nil
	}

	handle, err := validateHandle(req.Handle)
	if err != nil {
		return nil, err
	}
	if err := ensureWorkspaceHandleAvailableTx(tx, workspace.ID, handle); err != nil {
		return nil, err
	}

	var account Account
	if err = tx.QueryRow(
		`SELECT id, email, display_name, last_accessed_workspace_id, created_at, updated_at
		   FROM accounts
		  WHERE id = $1`,
		accountID,
	).Scan(&account.ID, &account.Email, &account.DisplayName, &account.LastAccessedWorkspaceID, &account.CreatedAt, &account.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}

	user := &User{
		ID:        "user_" + uuid.NewString(),
		Handle:    handle,
		Name:      firstNonEmptyString(account.DisplayName, account.Email),
		Role:      "Workspace member",
		Kind:      "human",
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err = tx.Exec(
		`INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		workspace.ID, user.ID, user.Handle, user.Name, user.Role, user.Kind, user.Status, user.CreatedAt, user.UpdatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, errors.New("Handle is already taken.")
		}
		return nil, err
	}
	if _, err = tx.Exec(
		`INSERT INTO workspace_members (workspace_id, account_id, user_id, membership_role, status, invited_by, created_at, accepted_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		workspace.ID, accountID, user.ID, MembershipRoleMember, "active", strings.TrimSpace(createdByUserID), now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, errors.New("Account is already a workspace member.")
		}
		return nil, err
	}
	if _, err = tx.Exec(
		`UPDATE accounts SET last_accessed_workspace_id = $1, updated_at = $2 WHERE id = $3`,
		workspace.ID, now, accountID,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return &workspace, nil
}

func reactivateWorkspaceInviteMemberTx(tx *sql.Tx, workspaceID string, accountID string, existingUserID string, invitedBy string, now time.Time) error {
	existingUserID = strings.TrimSpace(existingUserID)
	if existingUserID == "" {
		return ErrNotFound
	}

	result, err := tx.Exec(
		`UPDATE users
		    SET status = 'active', updated_at = $1
		  WHERE workspace_id = $2 AND id = $3`,
		now, workspaceID, existingUserID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return ErrNotFound
	}

	result, err = tx.Exec(
		`UPDATE workspace_members
		    SET user_id = $1,
		        membership_role = $2,
		        status = 'active',
		        invited_by = $3,
		        accepted_at = $4
		  WHERE workspace_id = $5 AND account_id = $6`,
		existingUserID, MembershipRoleMember, invitedBy, now, workspaceID, accountID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func ensurePostgresWorkspaceHandleAvailable(db *sql.DB, workspaceID string, handle string) error {
	var count int
	if err := db.QueryRow(
		`SELECT
			(SELECT COUNT(*) FROM users WHERE workspace_id = $1 AND handle = $2) +
			(SELECT COUNT(*) FROM agents WHERE workspace_id = $1 AND handle = $2)`,
		strings.TrimSpace(workspaceID),
		handle,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("Handle is already taken.")
	}
	return nil
}

func ensureWorkspaceHandleAvailableTx(tx *sql.Tx, workspaceID string, handle string) error {
	var count int
	if err := tx.QueryRow(
		`SELECT
			(SELECT COUNT(*) FROM users WHERE workspace_id = $1 AND handle = $2) +
			(SELECT COUNT(*) FROM agents WHERE workspace_id = $1 AND handle = $2)`,
		strings.TrimSpace(workspaceID),
		handle,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("Handle is already taken.")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "duplicate key") || strings.Contains(message, "SQLSTATE 23505")
}

func getAccountByEmail(db *sql.DB, email string) (*Account, error) {
	account, err := accountFromRow(db.QueryRow(
		`SELECT id, email, display_name, email_verified, last_accessed_workspace_id, created_at, updated_at FROM accounts WHERE email = $1`,
		normalizeEmail(email),
	))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return account, nil
}

func workspaceMemberForAccount(db *sql.DB, workspaceID string, accountID string) (*WorkspaceMember, error) {
	member := &WorkspaceMember{}
	err := db.QueryRow(
		`SELECT m.workspace_id, m.account_id, m.user_id, u.handle, u.name, m.membership_role, m.status, m.invited_by, m.created_at, COALESCE(m.accepted_at, '0001-01-01T00:00:00Z'::timestamptz)
		   FROM workspace_members m
		   JOIN users u ON u.workspace_id = m.workspace_id AND u.id = m.user_id
		  WHERE m.workspace_id = $1 AND m.account_id = $2 AND m.status = 'active'`,
		workspaceID,
		accountID,
	).Scan(&member.WorkspaceID, &member.AccountID, &member.UserID, &member.UserHandle, &member.UserName, &member.MembershipRole, &member.Status, &member.InvitedBy, &member.CreatedAt, &member.AcceptedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return member, nil
}

func listWorkspaceMembers(db *sql.DB, workspaceID string) ([]*WorkspaceMember, error) {
	rows, err := db.Query(
		`SELECT m.workspace_id, m.account_id, m.user_id, u.handle, u.name, m.membership_role, m.status, m.invited_by, m.created_at, COALESCE(m.accepted_at, '0001-01-01T00:00:00Z'::timestamptz)
		   FROM workspace_members m
		   JOIN users u ON u.workspace_id = m.workspace_id AND u.id = m.user_id
		  WHERE m.workspace_id = $1
		  ORDER BY u.handle ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []*WorkspaceMember{}
	for rows.Next() {
		member := &WorkspaceMember{}
		if err := rows.Scan(&member.WorkspaceID, &member.AccountID, &member.UserID, &member.UserHandle, &member.UserName, &member.MembershipRole, &member.Status, &member.InvitedBy, &member.CreatedAt, &member.AcceptedAt); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func createDaemon(db *sql.DB, workspaceID string, name string) (*Daemon, string, error) {
	if db == nil {
		return nil, "", errors.New("database is required")
	}
	if _, err := getWorkspace(db, workspaceID); err != nil {
		return nil, "", err
	}
	token, err := randomToken("nottyd_")
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	daemon := &Daemon{
		ID:          "daemon_" + uuid.NewString(),
		WorkspaceID: workspaceID,
		Name:        firstNonEmptyString(strings.TrimSpace(name), "Daemon"),
		Status:      "active",
		CreatedAt:   now,
	}
	if _, err := db.Exec(
		`INSERT INTO daemons (id, workspace_id, name, token_hash, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		daemon.ID, daemon.WorkspaceID, daemon.Name, tokenHash(token), daemon.Status, daemon.CreatedAt,
	); err != nil {
		return nil, "", err
	}
	applyDaemonLiveness(daemon, now)
	return daemon, token, nil
}

func createDaemonReinstallToken(db *sql.DB, workspaceID string, daemonID string) (*Daemon, string, error) {
	if db == nil {
		return nil, "", errors.New("database is required")
	}
	token, err := randomToken("nottyd_")
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	daemon := &Daemon{}
	err = db.QueryRow(
		`UPDATE daemons
		    SET token_hash = $1
		  WHERE id = $2
		    AND workspace_id = $3
		    AND status = 'active'
		    AND deleted_at IS NULL
		  RETURNING id, workspace_id, name, status, daemon_version, os, arch, runtime_detections::text, COALESCE(last_seen_at, '0001-01-01T00:00:00Z'::timestamptz), created_at, COALESCE(deleted_at, '0001-01-01T00:00:00Z'::timestamptz)`,
		tokenHash(token),
		strings.TrimSpace(daemonID),
		strings.TrimSpace(workspaceID),
	).Scan(&daemon.ID, &daemon.WorkspaceID, &daemon.Name, &daemon.Status, &daemon.Version, &daemon.OS, &daemon.Arch, runtimeDetectionsScanner(&daemon.Runtimes), &daemon.LastSeenAt, &daemon.CreatedAt, &daemon.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	applyDaemonLiveness(daemon, now)
	return daemon, token, nil
}

func listDaemons(db *sql.DB, workspaceID string) ([]*Daemon, error) {
	now := time.Now().UTC()
	rows, err := db.Query(
		`SELECT id, workspace_id, name, status, daemon_version, os, arch, runtime_detections::text, last_seen_at, created_at, deleted_at
		   FROM daemons
		  WHERE workspace_id = $1
		    AND status <> 'deleted'
		  ORDER BY created_at ASC, id ASC`,
		strings.TrimSpace(workspaceID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	daemons := []*Daemon{}
	for rows.Next() {
		daemon := &Daemon{}
		var lastSeen sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(&daemon.ID, &daemon.WorkspaceID, &daemon.Name, &daemon.Status, &daemon.Version, &daemon.OS, &daemon.Arch, runtimeDetectionsScanner(&daemon.Runtimes), &lastSeen, &daemon.CreatedAt, &deletedAt); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			daemon.LastSeenAt = lastSeen.Time
		}
		if deletedAt.Valid {
			daemon.DeletedAt = deletedAt.Time
		}
		applyDaemonLiveness(daemon, now)
		daemons = append(daemons, daemon)
	}
	return daemons, rows.Err()
}

type runtimeDetectionsScanTarget struct {
	value *[]RuntimeDetection
}

func runtimeDetectionsScanner(value *[]RuntimeDetection) *runtimeDetectionsScanTarget {
	return &runtimeDetectionsScanTarget{value: value}
}

func (t *runtimeDetectionsScanTarget) Scan(src any) error {
	if t == nil || t.value == nil {
		return nil
	}
	*t.value = nil
	switch value := src.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return json.Unmarshal([]byte(value), t.value)
	case []byte:
		if len(value) == 0 {
			return nil
		}
		return json.Unmarshal(value, t.value)
	default:
		return fmt.Errorf("unsupported runtime detections type %T", src)
	}
}

func updateDaemonStatus(db *sql.DB, workspaceID string, daemonID string, req UpdateDaemonStatusRequest) (*Daemon, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	runtimeJSON, err := json.Marshal(req.Runtimes)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	daemon := &Daemon{}
	err = db.QueryRow(
		`UPDATE daemons
		    SET last_seen_at = $1,
		        daemon_version = $2,
		        os = $3,
		        arch = $4,
		        runtime_detections = $5::jsonb
		  WHERE id = $6
		    AND workspace_id = $7
		    AND status = 'active'
		    AND deleted_at IS NULL
		  RETURNING id, workspace_id, name, status, daemon_version, os, arch, runtime_detections::text, COALESCE(last_seen_at, '0001-01-01T00:00:00Z'::timestamptz), created_at, COALESCE(deleted_at, '0001-01-01T00:00:00Z'::timestamptz)`,
		now,
		strings.TrimSpace(req.Version),
		strings.TrimSpace(req.OS),
		strings.TrimSpace(req.Arch),
		string(runtimeJSON),
		strings.TrimSpace(daemonID),
		strings.TrimSpace(workspaceID),
	).Scan(&daemon.ID, &daemon.WorkspaceID, &daemon.Name, &daemon.Status, &daemon.Version, &daemon.OS, &daemon.Arch, runtimeDetectionsScanner(&daemon.Runtimes), &daemon.LastSeenAt, &daemon.CreatedAt, &daemon.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyDaemonLiveness(daemon, now)
	return daemon, nil
}

func deleteDaemon(db *sql.DB, workspaceID string, daemonID string) (*Daemon, error) {
	now := time.Now().UTC()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	daemon := &Daemon{}
	err = tx.QueryRow(
		`UPDATE daemons
		    SET status = 'deleted',
		        deleted_at = $1
		  WHERE id = $2
		    AND workspace_id = $3
		    AND status <> 'deleted'
		  RETURNING id, workspace_id, name, status, daemon_version, os, arch, runtime_detections::text, COALESCE(last_seen_at, '0001-01-01T00:00:00Z'::timestamptz), created_at, COALESCE(deleted_at, '0001-01-01T00:00:00Z'::timestamptz)`,
		now,
		strings.TrimSpace(daemonID),
		strings.TrimSpace(workspaceID),
	).Scan(&daemon.ID, &daemon.WorkspaceID, &daemon.Name, &daemon.Status, &daemon.Version, &daemon.OS, &daemon.Arch, runtimeDetectionsScanner(&daemon.Runtimes), &daemon.LastSeenAt, &daemon.CreatedAt, &daemon.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		`UPDATE agents
		    SET status = 'disconnected',
		        current_activity = 'Daemon deleted',
		        current_turn_id = '',
		        current_run_id = '',
		        updated_at = $1
		  WHERE workspace_id = $2
		    AND daemon_id = $3`,
		now,
		strings.TrimSpace(workspaceID),
		daemon.ID,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	applyDaemonLiveness(daemon, now)
	return daemon, nil
}

func authenticateDaemonToken(db *sql.DB, token string, workspaceID string) (*Daemon, error) {
	now := time.Now().UTC()
	daemon := &Daemon{}
	err := db.QueryRow(
		`UPDATE daemons
		    SET last_seen_at = $1
		  WHERE token_hash = $2
		    AND workspace_id = $3
		    AND status = 'active'
		    AND deleted_at IS NULL
		  RETURNING id, workspace_id, name, status, daemon_version, os, arch, runtime_detections::text, COALESCE(last_seen_at, '0001-01-01T00:00:00Z'::timestamptz), created_at, COALESCE(deleted_at, '0001-01-01T00:00:00Z'::timestamptz)`,
		now,
		tokenHash(token),
		workspaceID,
	).Scan(&daemon.ID, &daemon.WorkspaceID, &daemon.Name, &daemon.Status, &daemon.Version, &daemon.OS, &daemon.Arch, runtimeDetectionsScanner(&daemon.Runtimes), &daemon.LastSeenAt, &daemon.CreatedAt, &daemon.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyDaemonLiveness(daemon, now)
	return daemon, nil
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[len("Bearer "):])
	}
	if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		return token
	}
	return ""
}

func isLikelyJWT(token string) bool {
	return strings.Count(token, ".") == 2
}

func authError(status int, message string) error {
	return fmt.Errorf("%d:%s", status, message)
}
