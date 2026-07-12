package notty

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// knownAgentRuntimes mirrors the runtime drivers the daemon ships (see
// daemon/internal/syncer). A workspace's default runtime is a UI default for
// new agents; "" means no default.
var knownAgentRuntimes = map[string]struct{}{
	"codex":  {},
	"claude": {},
}

func validateDefaultRuntime(value string) (string, error) {
	runtime := strings.TrimSpace(value)
	if runtime == "" {
		return "", nil
	}
	if _, ok := knownAgentRuntimes[runtime]; !ok {
		return "", fmt.Errorf("Unknown default runtime %q.", runtime)
	}
	return runtime, nil
}

// updateWorkspaceSettings applies a partial update to the workspace summary
// fields (name and defaultRuntime). Workspace slugs are immutable after creation.
func updateWorkspaceSettings(db *sql.DB, workspaceID string, req UpdateWorkspaceRequest) (*Workspace, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	if req.Name == nil && req.DefaultRuntime == nil {
		return nil, errors.New("At least one of name or defaultRuntime is required.")
	}
	workspace, err := getWorkspace(db, workspaceID)
	if err != nil {
		return nil, err
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, errors.New("Workspace name is required.")
		}
		workspace.Name = name
	}
	if req.DefaultRuntime != nil {
		runtime, err := validateDefaultRuntime(*req.DefaultRuntime)
		if err != nil {
			return nil, err
		}
		workspace.DefaultRuntime = runtime
	}
	workspace.UpdatedAt = time.Now().UTC()
	if _, err := db.Exec(
		`UPDATE workspaces SET name = $1, default_runtime = $2, updated_at = $3 WHERE id = $4::uuid`,
		workspace.Name, workspace.DefaultRuntime, workspace.UpdatedAt, workspace.ID,
	); err != nil {
		return nil, err
	}
	return workspace, nil
}

// deleteWorkspaceHard permanently deletes a workspace and, through the schema's
// ON DELETE CASCADE graph, every row that belongs to it (documents, updates,
// checkpoints, threads, messages, participants, agents, runs, events,
// activities, presences, daemons, members, invites, users, views).
// accounts.last_accessed_workspace_id is nulled by its ON DELETE SET NULL.
//
// Deleted means deleted: there is no soft-delete state and no application-level
// undelete. Recovery from a mistaken deletion is a database-backup operation.
// Caller-facing protection is the server-enforced name-confirmation contract in
// the DELETE handler.
func deleteWorkspaceHard(db *sql.DB, workspaceID string) error {
	if db == nil {
		return errors.New("database is required")
	}
	if !isUUIDString(workspaceID) {
		return ErrNotFound
	}
	result, err := db.Exec(`DELETE FROM workspaces WHERE id = $1::uuid`, workspaceID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
