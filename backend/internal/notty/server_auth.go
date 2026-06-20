package notty

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.db == nil {
		writeError(w, http.StatusServiceUnavailable, "database auth is not available")
		return
	}
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := registerAccount(s.store.db, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token, err := issueJWT(s.cfg.JWTSecret, account, 7*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	workspaces, _ := listWorkspacesForAccount(s.store.db, account.ID)
	writeJSON(w, http.StatusCreated, AuthResponse{Token: token, Account: account, Workspaces: workspaces})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.db == nil {
		writeError(w, http.StatusServiceUnavailable, "database auth is not available")
		return
	}
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := authenticateAccount(s.store.db, req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	token, err := issueJWT(s.cfg.JWTSecret, account, 7*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	workspaces, _ := listWorkspacesForAccount(s.store.db, account.ID)
	writeJSON(w, http.StatusOK, AuthResponse{Token: token, Account: account, Workspaces: workspaces})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromContext(r.Context())
	if !ok || auth.AccountID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	account, err := getAccountByID(s.store.db, auth.AccountID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	workspaces, _ := listWorkspacesForAccount(s.store.db, account.ID)
	writeJSON(w, http.StatusOK, map[string]any{"account": account, "workspaces": workspaces})
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromContext(r.Context())
	if !ok || auth.AccountID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	workspaces, err := listWorkspacesForAccount(s.store.db, auth.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": workspaces})
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromContext(r.Context())
	if !ok || auth.AccountID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	account, err := getAccountByID(s.store.db, auth.AccountID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req CreateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	workspace, member, err := createWorkspaceForAccount(s.store.db, account, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": workspace, "member": member})
}

func (s *Server) handleListWorkspaceMembers(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	members, err := listWorkspaceMembers(s.store.db, s.requestWorkspaceID(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) handleAddWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	var req AddWorkspaceMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromContext(r.Context())
	invitedBy := ""
	if auth != nil {
		invitedBy = auth.UserID
	}
	member, err := addWorkspaceMember(s.store.db, s.requestWorkspaceID(r), req, invitedBy)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if store, err := s.workspaceStore(s.requestWorkspaceID(r)); err == nil && store != nil {
		_ = store.Reload()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"member": member})
}

func (s *Server) handleListDaemons(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	daemons, err := listDaemons(s.store.db, s.requestWorkspaceID(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"daemons": daemons})
}

func (s *Server) handleCreateDaemon(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	var req CreateDaemonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	daemon, token, err := createDaemon(s.store.db, s.requestWorkspaceID(r), req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if store, err := s.workspaceStore(s.requestWorkspaceID(r)); err == nil && store != nil {
		_ = store.Reload()
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "daemon.created", Data: daemon})
	writeJSON(w, http.StatusCreated, CreateDaemonResponse{Daemon: daemon, Token: token})
}

func (s *Server) handleUpdateDaemonStatus(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromContext(r.Context())
	if !ok || auth == nil || auth.PrincipalKind != "daemon" || strings.TrimSpace(auth.DaemonID) == "" {
		writeError(w, http.StatusForbidden, "daemon authentication is required")
		return
	}
	var req UpdateDaemonStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	daemon, err := updateDaemonStatus(s.store.db, s.requestWorkspaceID(r), auth.DaemonID, req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if store, err := s.workspaceStore(s.requestWorkspaceID(r)); err == nil && store != nil {
		_ = store.Reload()
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "daemon.updated", Data: daemon})
	writeJSON(w, http.StatusOK, map[string]any{"daemon": daemon})
}

func (s *Server) handleCreateDaemonReinstallToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	daemon, token, err := createDaemonReinstallToken(s.store.db, s.requestWorkspaceID(r), chi.URLParam(r, "daemonID"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CreateDaemonResponse{Daemon: daemon, Token: token})
}

func (s *Server) handleDeleteDaemon(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	daemon, err := deleteDaemon(s.store.db, s.requestWorkspaceID(r), chi.URLParam(r, "daemonID"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if store, err := s.workspaceStore(s.requestWorkspaceID(r)); err == nil && store != nil {
		_ = store.Reload()
	}
	s.requestBroker(r).Publish(EventEnvelope{Type: "daemon.deleted", Data: daemon})
	writeJSON(w, http.StatusOK, map[string]any{"daemon": daemon})
}

func (s *Server) requireHuman(next http.Handler) http.Handler {
	if !s.authEnabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, err := s.authenticateHumanRequest(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(contextWithAuth(r.Context(), auth)))
	})
}

func (s *Server) requireWorkspace(next http.Handler) http.Handler {
	if !s.authEnabled() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			workspaceID := firstNonEmptyString(chi.URLParam(r, "workspaceID"), s.store.Snapshot().WorkspaceID)
			store, err := s.workspaceStore(workspaceID)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			ctx := contextWithWorkspaceID(r.Context(), workspaceID)
			ctx = contextWithRequestStore(ctx, store)
			ctx = contextWithRequestBroker(ctx, s.workspaceBroker(workspaceID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workspaceID := strings.TrimSpace(chi.URLParam(r, "workspaceID"))
		if workspaceID == "" {
			writeError(w, http.StatusBadRequest, "workspace id is required")
			return
		}
		auth, err := s.authenticateWorkspaceRequest(r, workspaceID)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, ErrNotFound) {
				status = http.StatusForbidden
			}
			writeError(w, status, err.Error())
			return
		}
		store, err := s.workspaceStore(workspaceID)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		ctx := contextWithWorkspaceID(r.Context(), workspaceID)
		ctx = contextWithAuth(ctx, auth)
		ctx = contextWithRequestStore(ctx, store)
		ctx = contextWithRequestBroker(ctx, s.workspaceBroker(workspaceID))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authenticateHumanRequest(r *http.Request) (*AuthContext, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errors.New("missing bearer token")
	}
	claims, err := verifyJWT(s.cfg.JWTSecret, token)
	if err != nil {
		return nil, err
	}
	account, err := getAccountByID(s.store.db, claims.Subject)
	if err != nil {
		return nil, err
	}
	return &AuthContext{
		PrincipalID:   account.ID,
		PrincipalKind: "human",
		AccountID:     account.ID,
	}, nil
}

func (s *Server) authenticateWorkspaceRequest(r *http.Request, workspaceID string) (*AuthContext, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errors.New("missing bearer token")
	}
	if isLikelyJWT(token) {
		base, err := s.authenticateHumanRequest(r)
		if err != nil {
			return nil, err
		}
		member, err := workspaceMemberForAccount(s.store.db, workspaceID, base.AccountID)
		if err != nil {
			return nil, err
		}
		base.WorkspaceID = workspaceID
		base.UserID = member.UserID
		base.UserHandle = member.UserHandle
		base.UserName = member.UserName
		base.PrincipalID = member.UserID
		return base, nil
	}
	daemon, err := authenticateDaemonToken(s.store.db, token, workspaceID)
	if err != nil {
		return nil, err
	}
	auth := &AuthContext{
		WorkspaceID:   workspaceID,
		PrincipalID:   daemon.ID,
		PrincipalKind: "daemon",
		DaemonID:      daemon.ID,
	}
	actingAgentID := strings.TrimSpace(r.Header.Get("X-Notty-Acting-Agent-ID"))
	if actingAgentID == "" {
		return auth, nil
	}
	store, err := s.workspaceStore(workspaceID)
	if err != nil {
		return nil, err
	}
	snapshot := store.Snapshot()
	actingAgent, ok := snapshot.Agents[actingAgentID]
	if !ok || actingAgent == nil {
		return nil, ErrNotFound
	}
	if actingAgent.DaemonID != daemon.ID {
		return nil, ErrNotFound
	}
	auth.PrincipalKind = "agent"
	auth.PrincipalID = actingAgent.ID
	auth.ActingAgentID = actingAgent.ID
	auth.ActingAgentRef = actingAgent.ID
	return auth, nil
}
