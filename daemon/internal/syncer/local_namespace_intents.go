package syncer

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	localNamespaceIntentKindCreate = "local_create"

	localNamespaceIntentPending  = "pending"
	localNamespaceIntentResolved = "resolved"
	localNamespaceIntentFailed   = "failed"
)

type localNamespaceIntent struct {
	ID                    string
	WorkspaceRelativePath string
	Kind                  string
	Status                string
	DocumentID            string
	ClientOperationID     string
	ObservedFileIdentity  string
	ObservedContentHash   string
	ObservedMTime         int64
	ActorID               string
	ActorType             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type localNamespaceClaim struct {
	Path              string
	DocumentID        string
	ClientOperationID string
	Kind              string
	Status            string
}

func newLocalCreateIntent(relativePath, actorID, actorType, observedHash string, observedMTime int64) localNamespaceIntent {
	id := "nsintent_" + uuid.NewString()
	documentID := "doc_" + uuid.NewString()
	return localNamespaceIntent{
		ID:                    id,
		WorkspaceRelativePath: relativePath,
		Kind:                  localNamespaceIntentKindCreate,
		Status:                localNamespaceIntentPending,
		DocumentID:            documentID,
		ClientOperationID:     id,
		ObservedContentHash:   observedHash,
		ObservedMTime:         observedMTime,
		ActorID:               actorID,
		ActorType:             actorType,
	}
}

func (c *workspaceStore) storeLocalNamespaceIntent(intent localNamespaceIntent) error {
	if c == nil || intent.ID == "" || intent.WorkspaceRelativePath == "" || intent.DocumentID == "" || intent.ClientOperationID == "" {
		return nil
	}
	now := time.Now().UTC()
	if intent.Kind == "" {
		intent.Kind = localNamespaceIntentKindCreate
	}
	if intent.Status == "" {
		intent.Status = localNamespaceIntentPending
	}
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	intent.UpdatedAt = now
	var existing int
	if err := c.db.QueryRow(`select count(*) from local_namespace_intents where status = ? and workspace_relative_path = ?`, localNamespaceIntentPending, intent.WorkspaceRelativePath).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	_, err := c.db.Exec(`insert or ignore into local_namespace_intents (
			id, workspace_relative_path, kind, status, document_id, client_operation_id,
			observed_file_identity, observed_content_hash, observed_mtime,
			actor_id, actor_type, created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.ID,
		intent.WorkspaceRelativePath,
		intent.Kind,
		intent.Status,
		intent.DocumentID,
		intent.ClientOperationID,
		intent.ObservedFileIdentity,
		intent.ObservedContentHash,
		intent.ObservedMTime,
		intent.ActorID,
		intent.ActorType,
		unixNano(intent.CreatedAt),
		unixNano(intent.UpdatedAt),
	)
	return err
}

func (c *workspaceStore) loadPendingLocalNamespaceIntents() ([]localNamespaceIntent, error) {
	return c.loadLocalNamespaceIntentsByStatus(localNamespaceIntentPending)
}

func scanLocalNamespaceIntents(rows *sql.Rows) ([]localNamespaceIntent, error) {
	var intents []localNamespaceIntent
	for rows.Next() {
		var intent localNamespaceIntent
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&intent.ID,
			&intent.WorkspaceRelativePath,
			&intent.Kind,
			&intent.Status,
			&intent.DocumentID,
			&intent.ClientOperationID,
			&intent.ObservedFileIdentity,
			&intent.ObservedContentHash,
			&intent.ObservedMTime,
			&intent.ActorID,
			&intent.ActorType,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		intent.CreatedAt = time.Unix(0, createdAt).UTC()
		intent.UpdatedAt = time.Unix(0, updatedAt).UTC()
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (c *workspaceStore) loadLocalNamespaceProjectionClaims() ([]localNamespaceClaim, error) {
	intents, err := c.loadLocalNamespaceIntentsByStatus(localNamespaceIntentPending, localNamespaceIntentResolved)
	if err != nil {
		return nil, err
	}
	claims := make([]localNamespaceClaim, 0, len(intents))
	for _, intent := range intents {
		claims = append(claims, localNamespaceClaim{
			Path:              intent.WorkspaceRelativePath,
			DocumentID:        intent.DocumentID,
			ClientOperationID: intent.ClientOperationID,
			Kind:              intent.Kind,
			Status:            intent.Status,
		})
	}
	return claims, nil
}

func (c *workspaceStore) loadLocalNamespaceIntentsByStatus(statuses ...string) ([]localNamespaceIntent, error) {
	if c == nil || len(statuses) == 0 {
		return nil, nil
	}
	placeholders := make([]string, 0, len(statuses))
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		if status == "" {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, status)
	}
	if len(args) == 0 {
		return nil, nil
	}
	query := `select
			id, workspace_relative_path, kind, status, document_id, client_operation_id,
			observed_file_identity, observed_content_hash, observed_mtime,
			actor_id, actor_type, created_at, updated_at
		from local_namespace_intents
		where status in (` + strings.Join(placeholders, ",") + `)
		order by created_at, id`
	rows, err := c.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLocalNamespaceIntents(rows)
}

func (c *workspaceStore) updateLocalNamespaceIntentStatus(id, status string) error {
	if c == nil || id == "" || status == "" {
		return nil
	}
	res, err := c.db.Exec(`update local_namespace_intents set status = ?, updated_at = ? where id = ?`, status, unixNano(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
