package notty

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// handleUpdateWorkspace is PATCH /api/workspaces/{id}/workspace.
// name and defaultRuntime require manage_workspace (owner or admin); slug is
// owner-only because changing it breaks existing links — old-slug URLs 404 by
// design (no redirect table at this scale).
func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	if err := requirePermission(auth, ActionManageWorkspace); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var req UpdateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Slug != nil && auth.MembershipRole != MembershipRoleOwner {
		writeError(w, http.StatusForbidden, "changing the workspace slug requires the owner role")
		return
	}
	workspace, err := updateWorkspaceSettings(s.sqlDB(), s.requestWorkspaceID(r), req)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, errSlugTaken):
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	// Refresh the in-memory store (workspace name lives in its state) and let
	// connected clients pick up the new summary.
	if store := s.requestStore(r); store != nil {
		_ = store.Reload()
	}
	if broker := s.requestBroker(r); broker != nil {
		broker.Publish(EventEnvelope{Type: "workspace.updated", Data: workspace})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": workspace})
}

// handleDeleteWorkspace is DELETE /api/workspaces/{id}.
// Owner-only (delete_workspace), human-only (daemons must never pass the
// permission bypass into a destructive op), and gated on a server-enforced
// confirmation: the request body must echo the workspace's exact name.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanPrincipal(w, r) {
		return
	}
	auth, _ := authFromContext(r.Context())
	if err := requirePermission(auth, ActionDeleteWorkspace); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var req DeleteWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	workspaceID := s.requestWorkspaceID(r)
	workspace, err := getWorkspace(s.sqlDB(), workspaceID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if strings.TrimSpace(req.ConfirmName) != workspace.Name {
		writeError(w, http.StatusBadRequest, "confirmName must match the workspace name exactly")
		return
	}
	// Tell connected clients before the workspace disappears so open UIs can
	// navigate away; their next request will fail authentication.
	if broker := s.requestBroker(r); broker != nil {
		broker.Publish(EventEnvelope{Type: "workspace.deleted", Data: map[string]string{"workspaceId": workspaceID}})
	}
	if err := deleteWorkspaceHard(s.sqlDB(), workspaceID); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.evictWorkspace(workspaceID)
	w.WriteHeader(http.StatusNoContent)
}

// evictWorkspace drops the in-process store and broker for a deleted
// workspace. Existing websocket subscribers keep their channels until their
// connections close; new requests re-authenticate and fail because the
// workspace row is gone.
func (s *Server) evictWorkspace(workspaceID string) {
	s.mu.Lock()
	delete(s.workspaceStores, workspaceID)
	delete(s.workspaceBrokers, workspaceID)
	s.mu.Unlock()
}
