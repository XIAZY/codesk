package notty

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// updateWorkspaceSettings applies a partial update to the workspace name.
// Workspace slugs and default runtime are immutable after creation.
func updateWorkspaceSettings(db *sql.DB, workspaceID string, req UpdateWorkspaceRequest) (*Workspace, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	if req.Name == nil {
		return nil, errors.New("Name is required.")
	}
	workspace, err := getWorkspace(db, workspaceID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(*req.Name)
	if name == "" {
		return nil, errors.New("Workspace name is required.")
	}
	workspace.Name = name
	workspace.UpdatedAt = time.Now().UTC()
	if _, err := db.Exec(
		`UPDATE workspaces SET name = $1, updated_at = $2 WHERE id = $3::uuid`,
		workspace.Name, workspace.UpdatedAt, workspace.ID,
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
