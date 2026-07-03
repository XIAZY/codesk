package notty

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	crdt "notty/internal/ycrdt"
)

const UUIDMigrationSnapshotVersion = 1

var (
	prefixedProductIDPattern       = regexp.MustCompile(`\b(?:account|account_email_token|ws|workspace|user|daemon|doc|agent|run|thread|message|invite|event)_[A-Za-z0-9][A-Za-z0-9_-]*\b`)
	uuidMigrationIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)
)

type UUIDMigrationSnapshot struct {
	Version       int                                 `json:"version"`
	CapturedAt    time.Time                           `json:"capturedAt"`
	RowCounts     map[string]int64                    `json:"rowCounts"`
	EntityIDs     map[string][]string                 `json:"entityIds"`
	Documents     []UUIDMigrationDocumentSnapshot     `json:"documents"`
	RootDocuments []UUIDMigrationRootDocumentSnapshot `json:"rootDocuments"`
}

type UUIDMigrationDocumentSnapshot struct {
	WorkspaceID string `json:"workspaceId"`
	DocumentID  string `json:"documentId"`
	Path        string `json:"path"`
	Title       string `json:"title"`
	Hidden      bool   `json:"hidden"`
	UpdateID    int64  `json:"updateId"`
	ContentHash string `json:"contentHash"`
}

type UUIDMigrationRootDocumentSnapshot struct {
	WorkspaceID string                           `json:"workspaceId"`
	DocumentID  string                           `json:"documentId"`
	Entries     []UUIDMigrationRootEntrySnapshot `json:"entries"`
}

type UUIDMigrationRootEntrySnapshot struct {
	EntryID           string `json:"entryId"`
	ContentDocumentID string `json:"contentDocumentId"`
	DesiredPath       string `json:"desiredPath"`
	Deleted           bool   `json:"deleted"`
}

type UUIDMigrationMapping struct {
	Entity string `json:"entity"`
	OldID  string `json:"oldId"`
	NewID  string `json:"newId"`
}

type UUIDMigrationMappingFile struct {
	Mappings []UUIDMigrationMapping `json:"mappings"`
}

type UUIDMigrationIssue struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Table   string `json:"table,omitempty"`
	Column  string `json:"column,omitempty"`
	ID      string `json:"id,omitempty"`
}

type uuidMigrationEntitySpec struct {
	Entity string
	Table  string
	Column string
}

type uuidMigrationReferenceSpec struct {
	Table        string
	Column       string
	TargetTable  string
	TargetColumn string
	Optional     bool
}

type uuidMigrationPolymorphicReferenceSpec struct {
	Table      string
	Column     string
	TypeColumn string
	Optional   bool
}

type uuidMigrationJSONSpec struct {
	Table  string
	Column string
}

var uuidMigrationEntitySpecs = []uuidMigrationEntitySpec{
	{Entity: "account", Table: "accounts", Column: "id"},
	{Entity: "account_email_token", Table: "account_email_tokens", Column: "id"},
	{Entity: "workspace", Table: "workspaces", Column: "id"},
	{Entity: "workspace_invite", Table: "workspace_invites", Column: "id"},
	{Entity: "daemon", Table: "daemons", Column: "id"},
	{Entity: "document", Table: "documents", Column: "id"},
	{Entity: "user", Table: "users", Column: "id"},
	{Entity: "agent", Table: "agents", Column: "id"},
	{Entity: "agent_run", Table: "agent_runs", Column: "id"},
	{Entity: "thread", Table: "threads", Column: "id"},
	{Entity: "thread_message", Table: "thread_messages", Column: "id"},
	{Entity: "agent_event", Table: "agent_events", Column: "id"},
}

var uuidMigrationRowCountTables = []string{
	"accounts",
	"account_email_tokens",
	"workspaces",
	"workspace_members",
	"workspace_invites",
	"daemons",
	"documents",
	"document_heads",
	"document_updates",
	"document_checkpoints",
	"users",
	"agents",
	"agent_runs",
	"threads",
	"thread_messages",
	"thread_participants",
	"presences",
	"activities",
	"agent_events",
	"agent_document_views",
}

var uuidMigrationReferenceSpecs = []uuidMigrationReferenceSpec{
	{Table: "accounts", Column: "last_accessed_workspace_id", TargetTable: "workspaces", TargetColumn: "id", Optional: true},
	{Table: "account_email_tokens", Column: "account_id", TargetTable: "accounts", TargetColumn: "id"},
	{Table: "workspace_members", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "workspace_members", Column: "account_id", TargetTable: "accounts", TargetColumn: "id"},
	{Table: "workspace_members", Column: "user_id", TargetTable: "users", TargetColumn: "id"},
	{Table: "workspace_members", Column: "invited_by", TargetTable: "users", TargetColumn: "id", Optional: true},
	{Table: "workspace_members", Column: "last_accessed_document_id", TargetTable: "documents", TargetColumn: "id", Optional: true},
	{Table: "workspace_invites", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "workspace_invites", Column: "created_by_user_id", TargetTable: "users", TargetColumn: "id"},
	{Table: "daemons", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "documents", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "document_heads", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "document_heads", Column: "document_id", TargetTable: "documents", TargetColumn: "id"},
	{Table: "document_updates", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "document_updates", Column: "document_id", TargetTable: "documents", TargetColumn: "id"},
	{Table: "document_checkpoints", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "document_checkpoints", Column: "document_id", TargetTable: "documents", TargetColumn: "id"},
	{Table: "users", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "agents", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "agents", Column: "daemon_id", TargetTable: "daemons", TargetColumn: "id", Optional: true},
	{Table: "agents", Column: "current_run_id", TargetTable: "agent_runs", TargetColumn: "id", Optional: true},
	{Table: "agent_runs", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "agent_runs", Column: "agent_id", TargetTable: "agents", TargetColumn: "id"},
	{Table: "threads", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "threads", Column: "document_id", TargetTable: "documents", TargetColumn: "id"},
	{Table: "thread_messages", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "thread_messages", Column: "thread_id", TargetTable: "threads", TargetColumn: "id"},
	{Table: "thread_participants", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "thread_participants", Column: "thread_id", TargetTable: "threads", TargetColumn: "id"},
	{Table: "presences", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "presences", Column: "document_id", TargetTable: "documents", TargetColumn: "id"},
	{Table: "activities", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "activities", Column: "document_id", TargetTable: "documents", TargetColumn: "id", Optional: true},
	{Table: "activities", Column: "comment_id", TargetTable: "thread_messages", TargetColumn: "id", Optional: true},
	{Table: "agent_events", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "agent_events", Column: "agent_id", TargetTable: "agents", TargetColumn: "id"},
	{Table: "agent_events", Column: "document_id", TargetTable: "documents", TargetColumn: "id", Optional: true},
	{Table: "agent_events", Column: "thread_id", TargetTable: "threads", TargetColumn: "id", Optional: true},
	{Table: "agent_events", Column: "thread_message_id", TargetTable: "thread_messages", TargetColumn: "id", Optional: true},
	{Table: "agent_events", Column: "run_id", TargetTable: "agent_runs", TargetColumn: "id", Optional: true},
	{Table: "agent_document_views", Column: "workspace_id", TargetTable: "workspaces", TargetColumn: "id"},
	{Table: "agent_document_views", Column: "agent_id", TargetTable: "agents", TargetColumn: "id"},
	{Table: "agent_document_views", Column: "document_id", TargetTable: "documents", TargetColumn: "id"},
}

var uuidMigrationPolymorphicReferenceSpecs = []uuidMigrationPolymorphicReferenceSpec{
	{Table: "document_updates", Column: "actor_id", TypeColumn: "actor_type", Optional: true},
	{Table: "threads", Column: "created_by_id", TypeColumn: "created_by_type"},
	{Table: "thread_messages", Column: "author_id", TypeColumn: "author_type"},
	{Table: "presences", Column: "actor_id", TypeColumn: "actor_type"},
	{Table: "activities", Column: "actor_id", TypeColumn: "actor_type"},
	{Table: "activities", Column: "provenance_actor_id", TypeColumn: "provenance_actor_type", Optional: true},
}

var uuidMigrationAnyPrincipalReferenceSpecs = []uuidMigrationReferenceSpec{
	{Table: "thread_participants", Column: "participant_id", Optional: false},
}

var uuidMigrationJSONSpecs = []uuidMigrationJSONSpec{
	{Table: "daemons", Column: "runtime_detections"},
	{Table: "agent_runs", Column: "log_tail"},
}

func CaptureUUIDMigrationSnapshot(ctx context.Context, db *sql.DB) (*UUIDMigrationSnapshot, error) {
	snapshot := &UUIDMigrationSnapshot{
		Version:    UUIDMigrationSnapshotVersion,
		CapturedAt: time.Now().UTC(),
		RowCounts:  map[string]int64{},
		EntityIDs:  map[string][]string{},
	}
	for _, table := range uuidMigrationRowCountTables {
		count, err := countRows(ctx, db, table)
		if err != nil {
			return nil, err
		}
		snapshot.RowCounts[table] = count
	}
	for _, spec := range uuidMigrationEntitySpecs {
		ids, err := queryTextColumn(ctx, db, spec.Table, spec.Column)
		if err != nil {
			return nil, err
		}
		snapshot.EntityIDs[spec.Entity] = ids
	}
	documents, roots, err := captureUUIDMigrationDocumentSnapshots(ctx, db)
	if err != nil {
		return nil, err
	}
	snapshot.Documents = documents
	snapshot.RootDocuments = roots
	return snapshot, nil
}

func LoadUUIDMigrationMappingsFromTable(ctx context.Context, db *sql.DB, table string) ([]UUIDMigrationMapping, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return nil, fmt.Errorf("mapping table is required")
	}
	if !uuidMigrationIdentifierPattern.MatchString(table) {
		return nil, fmt.Errorf("invalid mapping table name %q", table)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT entity_type::text, old_id::text, new_id::text FROM %s ORDER BY entity_type::text, old_id::text`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mappings := []UUIDMigrationMapping{}
	for rows.Next() {
		var mapping UUIDMigrationMapping
		if err := rows.Scan(&mapping.Entity, &mapping.OldID, &mapping.NewID); err != nil {
			return nil, err
		}
		mappings = append(mappings, normalizeUUIDMigrationMapping(mapping))
	}
	return mappings, rows.Err()
}

func VerifyUUIDMigrationDatabase(ctx context.Context, db *sql.DB) ([]UUIDMigrationIssue, error) {
	issues := []UUIDMigrationIssue{}
	shape, err := VerifyUUIDMigrationIDShape(ctx, db)
	if err != nil {
		return nil, err
	}
	issues = append(issues, shape...)
	references, err := VerifyUUIDMigrationReferences(ctx, db)
	if err != nil {
		return nil, err
	}
	issues = append(issues, references...)
	return issues, nil
}

func VerifyUUIDMigrationIDShape(ctx context.Context, db *sql.DB) ([]UUIDMigrationIssue, error) {
	issues := []UUIDMigrationIssue{}
	for _, spec := range uuidMigrationEntitySpecs {
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT %s::text FROM %s ORDER BY %s::text`, spec.Column, spec.Table, spec.Column))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if _, err := uuid.Parse(id); err != nil {
				issues = append(issues, UUIDMigrationIssue{
					Kind:    "invalid_uuid",
					Message: fmt.Sprintf("%s.%s is not a bare UUID: %q", spec.Table, spec.Column, id),
					Table:   spec.Table,
					Column:  spec.Column,
					ID:      id,
				})
			}
			if prefixedProductIDPattern.MatchString(id) {
				issues = append(issues, UUIDMigrationIssue{
					Kind:    "prefixed_id",
					Message: fmt.Sprintf("%s.%s still has a prefixed product ID: %q", spec.Table, spec.Column, id),
					Table:   spec.Table,
					Column:  spec.Column,
					ID:      id,
				})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return issues, nil
}

func VerifyUUIDMigrationReferences(ctx context.Context, db *sql.DB) ([]UUIDMigrationIssue, error) {
	issues := []UUIDMigrationIssue{}
	for _, spec := range uuidMigrationReferenceSpecs {
		missing, err := queryMissingRequiredReference(ctx, db, spec.Table, spec.Column, spec.Optional, "")
		if err != nil {
			return nil, err
		}
		issues = append(issues, missing...)
		query := fmt.Sprintf(
			`SELECT source.%s::text
			   FROM %s source
			  WHERE %s
			    AND NOT EXISTS (
			        SELECT 1 FROM %s target
			         WHERE target.%s::text = source.%s::text
			    )
			  ORDER BY source.%s::text
			  LIMIT 20`,
			spec.Column,
			spec.Table,
			referencePresencePredicate("source", spec.Column, spec.Optional),
			spec.TargetTable,
			spec.TargetColumn,
			spec.Column,
			spec.Column,
		)
		unresolved, err := querySingleTextColumn(ctx, db, query)
		if err != nil {
			return nil, err
		}
		for _, id := range unresolved {
			issues = append(issues, UUIDMigrationIssue{
				Kind:    "unresolved_reference",
				Message: fmt.Sprintf("%s.%s references missing %s.%s: %q", spec.Table, spec.Column, spec.TargetTable, spec.TargetColumn, id),
				Table:   spec.Table,
				Column:  spec.Column,
				ID:      id,
			})
		}
	}
	for _, spec := range uuidMigrationAnyPrincipalReferenceSpecs {
		missing, err := queryMissingRequiredReference(ctx, db, spec.Table, spec.Column, spec.Optional, "")
		if err != nil {
			return nil, err
		}
		issues = append(issues, missing...)
		query := fmt.Sprintf(
			`SELECT source.%s::text
			   FROM %s source
			  WHERE %s
			    AND NOT EXISTS (SELECT 1 FROM users target WHERE target.id::text = source.%s::text)
			    AND NOT EXISTS (SELECT 1 FROM agents target WHERE target.id::text = source.%s::text)
			  ORDER BY source.%s::text
			  LIMIT 20`,
			spec.Column,
			spec.Table,
			referencePresencePredicate("source", spec.Column, spec.Optional),
			spec.Column,
			spec.Column,
			spec.Column,
		)
		unresolved, err := querySingleTextColumn(ctx, db, query)
		if err != nil {
			return nil, err
		}
		for _, id := range unresolved {
			issues = append(issues, UUIDMigrationIssue{
				Kind:    "unresolved_principal_reference",
				Message: fmt.Sprintf("%s.%s references neither users nor agents: %q", spec.Table, spec.Column, id),
				Table:   spec.Table,
				Column:  spec.Column,
				ID:      id,
			})
		}
	}
	for _, spec := range uuidMigrationPolymorphicReferenceSpecs {
		missing, err := queryMissingRequiredReference(ctx, db, spec.Table, spec.Column, spec.Optional, fmt.Sprintf("lower(%s::text) <> 'system'", spec.TypeColumn))
		if err != nil {
			return nil, err
		}
		issues = append(issues, missing...)
		polyIssues, err := verifyPolymorphicUUIDReference(ctx, db, spec)
		if err != nil {
			return nil, err
		}
		issues = append(issues, polyIssues...)
	}
	return issues, nil
}

func queryMissingRequiredReference(ctx context.Context, db *sql.DB, table, column string, optional bool, extraPredicate string) ([]UUIDMigrationIssue, error) {
	if optional {
		return nil, nil
	}
	predicate := fmt.Sprintf("COALESCE(%s::text, '') = ''", column)
	if strings.TrimSpace(extraPredicate) != "" {
		predicate = fmt.Sprintf("%s AND %s", predicate, extraPredicate)
	}
	query := fmt.Sprintf(
		`SELECT COALESCE(%s::text, '')
		   FROM %s
		  WHERE %s
		  LIMIT 20`,
		column,
		table,
		predicate,
	)
	missing, err := querySingleTextColumn(ctx, db, query)
	if err != nil {
		return nil, err
	}
	issues := []UUIDMigrationIssue{}
	for range missing {
		issues = append(issues, UUIDMigrationIssue{
			Kind:    "missing_required_reference",
			Message: fmt.Sprintf("%s.%s is required but empty", table, column),
			Table:   table,
			Column:  column,
		})
	}
	return issues, nil
}

func VerifyUUIDMigrationJSONPayloads(ctx context.Context, db *sql.DB) ([]UUIDMigrationIssue, error) {
	issues := []UUIDMigrationIssue{}
	for _, spec := range uuidMigrationJSONSpecs {
		query := fmt.Sprintf(
			`SELECT %s::text
			   FROM %s
			  WHERE %s::text ~ $1
			  LIMIT 20`,
			spec.Column,
			spec.Table,
			spec.Column,
		)
		rows, err := db.QueryContext(ctx, query, prefixedProductIDPattern.String())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return nil, err
			}
			match := prefixedProductIDPattern.FindString(payload)
			issues = append(issues, UUIDMigrationIssue{
				Kind:    "prefixed_id_in_json",
				Message: fmt.Sprintf("%s.%s JSON still contains prefixed product ID %q", spec.Table, spec.Column, match),
				Table:   spec.Table,
				Column:  spec.Column,
				ID:      match,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return issues, nil
}

func VerifyUUIDMigrationSnapshots(before, after *UUIDMigrationSnapshot, mappings []UUIDMigrationMapping) []UUIDMigrationIssue {
	issues := []UUIDMigrationIssue{}
	if before == nil {
		return []UUIDMigrationIssue{{Kind: "missing_before_snapshot", Message: "before snapshot is required"}}
	}
	if after == nil {
		return []UUIDMigrationIssue{{Kind: "missing_after_snapshot", Message: "after snapshot is required"}}
	}
	byEntity := mappingByEntity(mappings)
	for table, beforeCount := range before.RowCounts {
		if afterCount, ok := after.RowCounts[table]; !ok {
			issues = append(issues, UUIDMigrationIssue{Kind: "missing_row_count", Table: table, Message: fmt.Sprintf("after snapshot is missing row count for %s", table)})
		} else if beforeCount != afterCount {
			issues = append(issues, UUIDMigrationIssue{Kind: "row_count_changed", Table: table, Message: fmt.Sprintf("%s row count changed from %d to %d", table, beforeCount, afterCount)})
		}
	}
	for entity, ids := range before.EntityIDs {
		entityMap := byEntity[entity]
		afterIDs := setOf(after.EntityIDs[entity])
		for _, oldID := range ids {
			newID := entityMap[oldID]
			if strings.TrimSpace(newID) == "" {
				issues = append(issues, UUIDMigrationIssue{Kind: "missing_mapping", ID: oldID, Message: fmt.Sprintf("missing %s mapping for %q", entity, oldID)})
				continue
			}
			if _, err := uuid.Parse(newID); err != nil {
				issues = append(issues, UUIDMigrationIssue{Kind: "mapped_id_not_uuid", ID: newID, Message: fmt.Sprintf("%s mapping for %q points to non-UUID %q", entity, oldID, newID)})
			}
			if !afterIDs[newID] {
				issues = append(issues, UUIDMigrationIssue{Kind: "mapped_id_missing_after", ID: newID, Message: fmt.Sprintf("%s mapping for %q points to absent after ID %q", entity, oldID, newID)})
			}
		}
	}
	documentMap := byEntity["document"]
	beforeDocs := map[string]UUIDMigrationDocumentSnapshot{}
	for _, doc := range before.Documents {
		beforeDocs[doc.DocumentID] = doc
	}
	afterDocs := map[string]UUIDMigrationDocumentSnapshot{}
	for _, doc := range after.Documents {
		afterDocs[doc.DocumentID] = doc
	}
	for oldID, beforeDoc := range beforeDocs {
		newID := documentMap[oldID]
		if newID == "" {
			continue
		}
		afterDoc, ok := afterDocs[newID]
		if !ok {
			continue
		}
		if beforeDoc.ContentHash != afterDoc.ContentHash {
			issues = append(issues, UUIDMigrationIssue{Kind: "document_content_changed", ID: oldID, Message: fmt.Sprintf("document content changed for %q -> %q", oldID, newID)})
		}
	}
	issues = append(issues, verifyRootSnapshots(before.RootDocuments, after.RootDocuments, byEntity)...)
	return issues
}

func captureUUIDMigrationDocumentSnapshots(ctx context.Context, db *sql.DB) ([]UUIDMigrationDocumentSnapshot, []UUIDMigrationRootDocumentSnapshot, error) {
	rootDocumentIDs, err := loadUUIDMigrationWorkspaceRootDocumentIDs(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.workspace_id::text, d.id::text, d.path, d.title, d.hidden, d.client_id_seed, h.update_id
		  FROM documents d
		  JOIN document_heads h
		    ON h.workspace_id::text = d.workspace_id::text
		   AND h.document_id::text = d.id::text
		 ORDER BY d.workspace_id::text, d.id::text`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	documents := []UUIDMigrationDocumentSnapshot{}
	roots := []UUIDMigrationRootDocumentSnapshot{}
	for rows.Next() {
		var workspaceID, documentID, path, title string
		var hidden bool
		var clientIDSeed uint64
		var updateID int64
		if err := rows.Scan(&workspaceID, &documentID, &path, &title, &hidden, &clientIDSeed, &updateID); err != nil {
			return nil, nil, err
		}
		document := &Document{ID: documentID, ClientIDSeed: clientIDSeed}
		doc, err := documentDocAtUpdatePostgres(ctx, db, workspaceID, document, updateID)
		if err != nil {
			return nil, nil, fmt.Errorf("capture document %s: %w", documentID, err)
		}
		content := doc.GetText("content").ToString()
		documents = append(documents, UUIDMigrationDocumentSnapshot{
			WorkspaceID: workspaceID,
			DocumentID:  documentID,
			Path:        path,
			Title:       title,
			Hidden:      hidden,
			UpdateID:    updateID,
			ContentHash: uuidMigrationContentHash(content),
		})
		if isUUIDMigrationRootDocument(workspaceID, documentID, path, title, hidden, rootDocumentIDs) {
			entries, err := decodeUUIDMigrationRootEntries(doc)
			if err != nil {
				return nil, nil, fmt.Errorf("decode root document %s: %w", documentID, err)
			}
			roots = append(roots, UUIDMigrationRootDocumentSnapshot{
				WorkspaceID: workspaceID,
				DocumentID:  documentID,
				Entries:     entries,
			})
		}
		doc.Close()
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return documents, roots, nil
}

func uuidMigrationContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func loadUUIDMigrationWorkspaceRootDocumentIDs(ctx context.Context, db *sql.DB) (map[string]string, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
		    SELECT 1
		      FROM information_schema.columns
		     WHERE table_schema = current_schema()
		       AND table_name = 'workspaces'
		       AND column_name = 'root_document_id'
		)`,
	).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id::text, root_document_id::text FROM workspaces WHERE COALESCE(root_document_id::text, '') <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := map[string]string{}
	for rows.Next() {
		var workspaceID, rootDocumentID string
		if err := rows.Scan(&workspaceID, &rootDocumentID); err != nil {
			return nil, err
		}
		roots[workspaceID] = rootDocumentID
	}
	return roots, rows.Err()
}

func documentDocAtUpdatePostgres(ctx context.Context, db *sql.DB, workspaceID string, document *Document, updateID int64) (*crdt.Doc, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(document.ClientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	err := db.QueryRowContext(ctx,
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id::text = $1 AND document_id::text = $2 AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		workspaceID,
		document.ID,
		updateID,
	).Scan(&appliedThrough, &checkpointState)
	if err != nil && err != sql.ErrNoRows {
		doc.Close()
		return nil, err
	}
	if err == nil && checkpointState != "" {
		checkpointUpdate, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
		if decodeErr != nil {
			doc.Close()
			return nil, decodeErr
		}
		if applyErr := crdt.ApplyUpdateV1(doc, checkpointUpdate, "checkpoint"); applyErr != nil {
			doc.Close()
			return nil, applyErr
		}
	}
	rows, err := db.QueryContext(ctx,
		`SELECT update
		   FROM document_updates
		  WHERE workspace_id::text = $1
		    AND document_id::text = $2
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		workspaceID,
		document.ID,
		appliedThrough,
		updateID,
	)
	if err != nil {
		doc.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			doc.Close()
			return nil, err
		}
		if err := crdt.ApplyUpdateV1(doc, update, "history"); err != nil {
			doc.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		doc.Close()
		return nil, err
	}
	return doc, nil
}

func decodeUUIDMigrationRootEntries(doc *crdt.Doc) ([]UUIDMigrationRootEntrySnapshot, error) {
	entries := []UUIDMigrationRootEntrySnapshot{}
	root := doc.GetMap("root")
	if err := doc.Read(func(txn *crdt.Transaction) error {
		entriesMap, ok, err := root.GetMap(txn, "entriesById")
		if err != nil || !ok {
			return err
		}
		items, err := entriesMap.Entries(txn)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.ValueKind != crdt.YMapEntryMap || item.MapValue == nil {
				continue
			}
			entry, ok, err := decodeUUIDMigrationRootEntry(txn, item.Key, item.MapValue)
			if err != nil {
				return err
			}
			if ok {
				entries = append(entries, entry)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ContentDocumentID == entries[j].ContentDocumentID {
			return entries[i].DesiredPath < entries[j].DesiredPath
		}
		return entries[i].ContentDocumentID < entries[j].ContentDocumentID
	})
	return entries, nil
}

func decodeUUIDMigrationRootEntry(txn *crdt.Transaction, entryID string, entryMap *crdt.YMap) (UUIDMigrationRootEntrySnapshot, bool, error) {
	var entry UUIDMigrationRootEntrySnapshot
	entry.EntryID = strings.TrimSpace(entryID)
	kind, _, err := entryMap.GetString(txn, "kind")
	if err != nil {
		return entry, false, err
	}
	if kind != "file" {
		return entry, false, nil
	}
	contentDocumentID, _, err := entryMap.GetString(txn, "contentDocumentId")
	if err != nil {
		return entry, false, err
	}
	entry.ContentDocumentID = strings.TrimSpace(contentDocumentID)
	if entry.ContentDocumentID == "" {
		return entry, false, nil
	}
	loc, _, err := entryMap.GetString(txn, "loc")
	if err != nil {
		return entry, false, err
	}
	var parsedLoc struct {
		ParentID string `json:"parentId"`
		Name     string `json:"name"`
	}
	if loc != "" {
		if err := json.Unmarshal([]byte(loc), &parsedLoc); err != nil {
			return entry, false, err
		}
	}
	entry.DesiredPath = strings.TrimSpace(parsedLoc.Name)
	if strings.TrimSpace(parsedLoc.ParentID) != "" {
		entry.DesiredPath = strings.Trim(strings.TrimSpace(parsedLoc.ParentID)+"/"+entry.DesiredPath, "/")
	}
	deleted, _, err := entryMap.GetString(txn, "deleted")
	if err != nil {
		return entry, false, err
	}
	entry.Deleted = strings.EqualFold(strings.TrimSpace(deleted), "true")
	return entry, true, nil
}

func isUUIDMigrationRootDocument(workspaceID, documentID, path, title string, hidden bool, explicitRootDocumentIDs map[string]string) bool {
	if explicitRootDocumentIDs != nil && strings.TrimSpace(explicitRootDocumentIDs[workspaceID]) == strings.TrimSpace(documentID) {
		return true
	}
	return strings.TrimSpace(path) == legacyRootDocumentPath ||
		strings.TrimSpace(title) == legacyRootDocumentTitle ||
		strings.TrimSpace(documentID) == rootDocumentID(workspaceID) ||
		(hidden && strings.Contains(strings.TrimSpace(documentID), "root"))
}

func verifyRootSnapshots(beforeRoots, afterRoots []UUIDMigrationRootDocumentSnapshot, byEntity map[string]map[string]string) []UUIDMigrationIssue {
	issues := []UUIDMigrationIssue{}
	documentMap := byEntity["document"]
	workspaceMap := byEntity["workspace"]
	afterByWorkspace := map[string]UUIDMigrationRootDocumentSnapshot{}
	for _, root := range afterRoots {
		afterByWorkspace[root.WorkspaceID] = root
	}
	for _, beforeRoot := range beforeRoots {
		newWorkspaceID := workspaceMap[beforeRoot.WorkspaceID]
		if newWorkspaceID == "" {
			issues = append(issues, UUIDMigrationIssue{Kind: "root_workspace_mapping_missing", ID: beforeRoot.WorkspaceID, Message: fmt.Sprintf("missing workspace mapping for root document %q", beforeRoot.DocumentID)})
			continue
		}
		afterRoot, ok := afterByWorkspace[newWorkspaceID]
		if !ok {
			issues = append(issues, UUIDMigrationIssue{Kind: "root_missing_after", ID: beforeRoot.DocumentID, Message: fmt.Sprintf("missing root document after migration for workspace %q -> %q", beforeRoot.WorkspaceID, newWorkspaceID)})
			continue
		}
		afterEntries := map[string]UUIDMigrationRootEntrySnapshot{}
		for _, entry := range afterRoot.Entries {
			afterEntries[entry.ContentDocumentID] = entry
		}
		for _, beforeEntry := range beforeRoot.Entries {
			newDocumentID := documentMap[beforeEntry.ContentDocumentID]
			if newDocumentID == "" {
				issues = append(issues, UUIDMigrationIssue{Kind: "root_entry_mapping_missing", ID: beforeEntry.ContentDocumentID, Message: fmt.Sprintf("missing document mapping for root entry %q", beforeEntry.ContentDocumentID)})
				continue
			}
			afterEntry, ok := afterEntries[newDocumentID]
			if !ok {
				issues = append(issues, UUIDMigrationIssue{Kind: "root_entry_missing_after", ID: beforeEntry.ContentDocumentID, Message: fmt.Sprintf("missing root entry after migration for document %q -> %q", beforeEntry.ContentDocumentID, newDocumentID)})
				continue
			}
			if afterEntry.EntryID != newDocumentID {
				issues = append(issues, UUIDMigrationIssue{Kind: "root_entry_key_changed", ID: beforeEntry.ContentDocumentID, Message: fmt.Sprintf("root entry key for document %q is %q, want mapped ID %q", beforeEntry.ContentDocumentID, afterEntry.EntryID, newDocumentID)})
			}
			if beforeEntry.DesiredPath != afterEntry.DesiredPath || beforeEntry.Deleted != afterEntry.Deleted {
				issues = append(issues, UUIDMigrationIssue{Kind: "root_entry_changed", ID: beforeEntry.ContentDocumentID, Message: fmt.Sprintf("root entry changed for document %q -> %q", beforeEntry.ContentDocumentID, newDocumentID)})
			}
		}
	}
	return issues
}

func verifyPolymorphicUUIDReference(ctx context.Context, db *sql.DB, spec uuidMigrationPolymorphicReferenceSpec) ([]UUIDMigrationIssue, error) {
	query := fmt.Sprintf(
		`SELECT source.%s::text, source.%s::text
		   FROM %s source
		  WHERE %s
		    AND CASE
		          WHEN lower(source.%s::text) IN ('human', 'user') THEN NOT EXISTS (SELECT 1 FROM users target WHERE target.id::text = source.%s::text)
		          WHEN lower(source.%s::text) = 'agent' THEN NOT EXISTS (SELECT 1 FROM agents target WHERE target.id::text = source.%s::text)
		          WHEN lower(source.%s::text) = 'daemon' THEN NOT EXISTS (SELECT 1 FROM daemons target WHERE target.id::text = source.%s::text)
		          WHEN lower(source.%s::text) = 'system' THEN false
		          ELSE source.%s::text <> ''
		        END
		  ORDER BY source.%s::text
		  LIMIT 20`,
		spec.Column,
		spec.TypeColumn,
		spec.Table,
		referencePresencePredicate("source", spec.Column, spec.Optional),
		spec.TypeColumn, spec.Column,
		spec.TypeColumn, spec.Column,
		spec.TypeColumn, spec.Column,
		spec.TypeColumn,
		spec.Column,
		spec.Column,
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []UUIDMigrationIssue{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		issues = append(issues, UUIDMigrationIssue{
			Kind:    "unresolved_polymorphic_reference",
			Message: fmt.Sprintf("%s.%s references missing %s principal %q", spec.Table, spec.Column, kind, id),
			Table:   spec.Table,
			Column:  spec.Column,
			ID:      id,
		})
	}
	return issues, rows.Err()
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func queryTextColumn(ctx context.Context, db *sql.DB, table, column string) ([]string, error) {
	return querySingleTextColumn(ctx, db, fmt.Sprintf(`SELECT %s::text FROM %s ORDER BY %s::text`, column, table, column))
}

func querySingleTextColumn(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func referencePresencePredicate(alias, column string, optional bool) string {
	expr := fmt.Sprintf("COALESCE(%s.%s::text, '') <> ''", alias, column)
	if optional {
		return expr
	}
	return expr
}

func mappingByEntity(mappings []UUIDMigrationMapping) map[string]map[string]string {
	result := map[string]map[string]string{}
	for _, mapping := range mappings {
		mapping = normalizeUUIDMigrationMapping(mapping)
		if mapping.Entity == "" || mapping.OldID == "" || mapping.NewID == "" {
			continue
		}
		if result[mapping.Entity] == nil {
			result[mapping.Entity] = map[string]string{}
		}
		result[mapping.Entity][mapping.OldID] = mapping.NewID
	}
	return result
}

func normalizeUUIDMigrationMapping(mapping UUIDMigrationMapping) UUIDMigrationMapping {
	mapping.Entity = strings.TrimSpace(mapping.Entity)
	mapping.OldID = strings.TrimSpace(mapping.OldID)
	mapping.NewID = strings.TrimSpace(mapping.NewID)
	return mapping
}

func setOf(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
