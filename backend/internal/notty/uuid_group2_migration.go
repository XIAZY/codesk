package notty

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
)

const (
	uuidGroup2AdvisoryLockID = 2
	uuidGroup2DocumentEntity = "documents"
)

const (
	uuidGroup2DocRefRequired   = "required"
	uuidGroup2DocRefOptional   = "optional"
	uuidGroup2DocRefDisposable = "disposable"
)

const (
	migrationRootEntryKindFile = "file"
	migrationRootDeletedTrue   = "true"
	migrationRootDeletedFalse  = "false"
)

var uuidTextPattern = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

type uuidGroup2DocumentRefSpec struct {
	table  string
	column string
	policy string
}

type uuidGroup2RootEntry struct {
	EntryID           string
	Kind              string
	ContentDocumentID string
	ParentID          string
	Name              string
	Deleted           bool
}

type uuidGroup2RootLoc struct {
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

type uuidGroup2RootPlan struct {
	workspaceID string
	oldRootID   string
	newRootID   string
	clientID    uint64
	headID      int64
	entries     []uuidGroup2RootEntry
}

func uuidGroup2DocumentRefs() []uuidGroup2DocumentRefSpec {
	return []uuidGroup2DocumentRefSpec{
		{table: "workspaces", column: "root_document_id", policy: uuidGroup2DocRefRequired},
		{table: "document_heads", column: "document_id", policy: uuidGroup2DocRefRequired},
		{table: "document_updates", column: "document_id", policy: uuidGroup2DocRefRequired},
		{table: "document_checkpoints", column: "document_id", policy: uuidGroup2DocRefRequired},
		{table: "threads", column: "document_id", policy: uuidGroup2DocRefRequired},
		{table: "presences", column: "document_id", policy: uuidGroup2DocRefDisposable},
		{table: "activities", column: "document_id", policy: uuidGroup2DocRefOptional},
		{table: "agent_events", column: "document_id", policy: uuidGroup2DocRefOptional},
		{table: "agent_document_views", column: "document_id", policy: uuidGroup2DocRefDisposable},
		{table: "workspace_members", column: "last_accessed_document_id", policy: uuidGroup2DocRefOptional},
	}
}

func uuidGroup2DocumentColumnInventory() []uuidGroup2DocumentRefSpec {
	columns := []uuidGroup2DocumentRefSpec{{table: "documents", column: "id", policy: uuidGroup2DocRefRequired}}
	columns = append(columns, uuidGroup2DocumentRefs()...)
	return columns
}

func RunUUIDGroup2Migration(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, uuidGroup1AdvisoryLockNamespace, uuidGroup2AdvisoryLockID); err != nil {
		return err
	}

	migrated, err := uuidGroup2AlreadyMigrated(ctx, tx)
	if err != nil {
		return err
	}
	if migrated {
		if err = verifyUUIDGroup2BootShapeTx(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}

	if err = ensureUUIDMigrationMap(ctx, tx); err != nil {
		return fmt.Errorf("ensure uuid migration map: %w", err)
	}
	if err = ensureUUIDGroup2LegacyRootDocuments(ctx, tx); err != nil {
		return fmt.Errorf("ensure legacy root documents: %w", err)
	}
	if err = populateUUIDGroup2DocumentMigrationMap(ctx, tx); err != nil {
		return fmt.Errorf("populate document uuid migration map: %w", err)
	}
	if err = normalizeUUIDGroup2BlankDocumentRefs(ctx, tx); err != nil {
		return fmt.Errorf("normalize blank document refs: %w", err)
	}
	if err = deleteUUIDGroup2DeletedParentThreadSubtrees(ctx, tx); err != nil {
		return fmt.Errorf("delete deleted-parent thread subtrees: %w", err)
	}
	if err = resolveUUIDGroup2DisposableRefs(ctx, tx); err != nil {
		return fmt.Errorf("resolve disposable document refs: %w", err)
	}
	if err = ensureUUIDGroup2DataBearingRefsMapped(ctx, tx); err != nil {
		return fmt.Errorf("verify document refs mapped: %w", err)
	}
	if err = regenerateUUIDGroup2RootDocuments(ctx, tx); err != nil {
		return fmt.Errorf("regenerate root documents: %w", err)
	}
	if err = rewriteUUIDGroup2DocumentRefs(ctx, tx); err != nil {
		return fmt.Errorf("rewrite document refs: %w", err)
	}
	if err = rewriteUUIDGroup2DocumentEntityIDs(ctx, tx); err != nil {
		return fmt.Errorf("rewrite document ids: %w", err)
	}
	if err = clearUUIDGroup2OptionalWorkspaceMemberOrphans(ctx, tx); err != nil {
		return fmt.Errorf("clear optional document refs: %w", err)
	}
	if err = resolveUUIDGroup2DisposableRefs(ctx, tx); err != nil {
		return fmt.Errorf("resolve rewritten disposable document refs: %w", err)
	}
	if err = convertUUIDGroup2DocumentColumns(ctx, tx); err != nil {
		return fmt.Errorf("convert document columns: %w", err)
	}
	if err = verifyUUIDGroup2DeepTx(ctx, tx); err != nil {
		return fmt.Errorf("verify uuid group2 deep: %w", err)
	}
	return tx.Commit()
}

func VerifyUUIDGroup2BootShape(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database is required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return verifyUUIDGroup2BootShapeTx(ctx, tx)
}

func VerifyUUIDGroup2Deep(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database is required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return verifyUUIDGroup2DeepTx(ctx, tx)
}

func uuidGroup2AlreadyMigrated(ctx context.Context, tx *sql.Tx) (bool, error) {
	for _, column := range uuidGroup2DocumentColumnInventory() {
		dataType, err := columnDataType(ctx, tx, column.table, column.column)
		if err != nil {
			return false, err
		}
		if dataType != "uuid" {
			return false, nil
		}
	}
	return true, nil
}

func ensureUUIDGroup2LegacyRootDocuments(ctx context.Context, tx *sql.Tx) error {
	rootType, err := columnDataType(ctx, tx, "workspaces", "root_document_id")
	if err != nil {
		return err
	}
	if rootType == "uuid" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET root_document_id = 'doc_root_' || id::text WHERE root_document_id = ''`,
	); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT w.id::text,
		        w.root_document_id::text,
		        COALESCE(MAX(d.client_id_seed), 1000) + 1 AS next_client_id
		   FROM workspaces AS w
		   LEFT JOIN documents AS root_doc
		     ON root_doc.workspace_id = w.id
		    AND root_doc.id = w.root_document_id
		   LEFT JOIN documents AS d
		     ON d.workspace_id = w.id
		  WHERE root_doc.id IS NULL
		  GROUP BY w.id::text, w.root_document_id::text
		  ORDER BY w.id::text`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type missingRoot struct {
		workspaceID string
		rootID      string
		clientID    uint64
	}
	missing := []missingRoot{}
	for rows.Next() {
		var root missingRoot
		var clientID int64
		if err := rows.Scan(&root.workspaceID, &root.rootID, &clientID); err != nil {
			return err
		}
		root.rootID = strings.TrimSpace(root.rootID)
		if root.rootID == "" {
			return fmt.Errorf("workspace %s has blank root document id", root.workspaceID)
		}
		if !strings.HasPrefix(root.rootID, "doc_root_") {
			return fmt.Errorf("workspace %s root document %q does not resolve", root.workspaceID, root.rootID)
		}
		root.clientID = uint64(clientID)
		missing = append(missing, root)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, root := range missing {
		update, crdtState, stateVector, err := buildUUIDGroup2RootUpdate(root.clientID, nil)
		if err != nil {
			return fmt.Errorf("workspace %s empty root: %w", root.workspaceID, err)
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, updated_at)
			 VALUES ($1, $2, $3, $4, TRUE, $5, $6)`,
			root.workspaceID, root.rootID, legacyRootDocumentPath, legacyRootDocumentTitle, int64(root.clientID), now,
		); err != nil {
			return err
		}
		var updateID int64
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, NULL, 'system', $4)
			 RETURNING id`,
			root.workspaceID, root.rootID, update, now,
		).Scan(&updateID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			root.workspaceID, root.rootID, stateVector, updateID, now,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			root.workspaceID, root.rootID, updateID, crdtState, stateVector, now,
		); err != nil {
			return err
		}
	}
	return nil
}

func populateUUIDGroup2DocumentMigrationMap(ctx context.Context, tx *sql.Tx) error {
	rootIDs, err := uuidGroup2WorkspaceRootIDs(ctx, tx)
	if err != nil {
		return err
	}
	docType, err := columnDataType(ctx, tx, "documents", "id")
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text FROM documents ORDER BY id::text`)
	if err != nil {
		return err
	}
	currentIDs := []string{}
	for rows.Next() {
		var currentID string
		if err := rows.Scan(&currentID); err != nil {
			rows.Close()
			return err
		}
		currentIDs = append(currentIDs, currentID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, currentID := range currentIDs {
		currentID = strings.TrimSpace(currentID)
		if currentID == "" {
			return errors.New("documents.id has blank document id")
		}
		if _, isRoot := rootIDs[currentID]; isRoot {
			if _, err := ensureUUIDGroup2RootMapping(ctx, tx, currentID); err != nil {
				return err
			}
			continue
		}
		if docType == "uuid" {
			if err := insertUUIDGroup2DocumentMapping(ctx, tx, "doc_"+currentID, currentID); err != nil {
				return err
			}
			continue
		}
		if isUUIDString(currentID) {
			if err := insertUUIDGroup2DocumentMapping(ctx, tx, "doc_"+currentID, currentID); err != nil {
				return err
			}
			continue
		}
		newID, err := stripPrefixedUUID(currentID, "doc_")
		if err != nil {
			return fmt.Errorf("documents.id %q: %w", currentID, err)
		}
		if err := insertUUIDGroup2DocumentMapping(ctx, tx, currentID, newID); err != nil {
			return err
		}
	}
	for rootID := range rootIDs {
		if _, err := ensureUUIDGroup2RootMapping(ctx, tx, rootID); err != nil {
			return err
		}
	}
	return nil
}

func uuidGroup2WorkspaceRootIDs(ctx context.Context, tx *sql.Tx) (map[string]struct{}, error) {
	roots := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx, `SELECT root_document_id::text FROM workspaces ORDER BY id::text`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rootID string
		if err := rows.Scan(&rootID); err != nil {
			return nil, err
		}
		rootID = strings.TrimSpace(rootID)
		if rootID == "" {
			return nil, errors.New("workspaces.root_document_id has blank root document id")
		}
		if err := validateUUIDGroup2RootDocumentID(rootID); err != nil {
			return nil, fmt.Errorf("workspaces.root_document_id %q: %w", rootID, err)
		}
		roots[rootID] = struct{}{}
	}
	return roots, rows.Err()
}

func validateUUIDGroup2RootDocumentID(rootID string) error {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return errors.New("root document id is required")
	}
	if strings.HasPrefix(rootID, "doc_root_") {
		if strings.TrimSpace(strings.TrimPrefix(rootID, "doc_root_")) == "" {
			return fmt.Errorf("malformed root document id %q", rootID)
		}
		return nil
	}
	if strings.HasPrefix(rootID, "doc_") {
		_, err := stripPrefixedUUID(rootID, "doc_")
		return err
	}
	if isUUIDString(rootID) {
		return nil
	}
	return fmt.Errorf("malformed root document id %q", rootID)
}

func ensureUUIDGroup2RootMapping(ctx context.Context, tx *sql.Tx, oldRootID string) (string, error) {
	oldRootID = strings.TrimSpace(oldRootID)
	if oldRootID == "" {
		return "", errors.New("root document id is required")
	}
	if mapped, ok, err := lookupUUIDGroup2DocumentMapping(ctx, tx, oldRootID); err != nil || ok {
		return mapped, err
	}
	newID := uuid.NewString()
	if err := insertUUIDGroup2DocumentMapping(ctx, tx, oldRootID, newID); err != nil {
		return "", err
	}
	return newID, nil
}

func insertUUIDGroup2DocumentMapping(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" {
		return errors.New("document mapping old id is required")
	}
	parsed, err := uuid.Parse(newID)
	if err != nil {
		return fmt.Errorf("document mapping %q -> %q has invalid UUID: %w", oldID, newID, err)
	}
	newID = parsed.String()
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT new_id::text FROM uuid_migration_map WHERE entity_type = $1 AND old_id = $2`,
		uuidGroup2DocumentEntity, oldID,
	).Scan(&existing)
	if err == nil {
		if existing != newID {
			return fmt.Errorf("document mapping %q already points to %s, not %s", oldID, existing, newID)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO uuid_migration_map (entity_type, old_id, new_id) VALUES ($1, $2, $3)`,
		uuidGroup2DocumentEntity, oldID, newID,
	)
	return err
}

func lookupUUIDGroup2DocumentMapping(ctx context.Context, tx *sql.Tx, oldID string) (string, bool, error) {
	oldID = strings.TrimSpace(oldID)
	if oldID == "" {
		return "", false, nil
	}
	var newID string
	err := tx.QueryRowContext(ctx,
		`SELECT new_id::text FROM uuid_migration_map WHERE entity_type = $1 AND old_id = $2`,
		uuidGroup2DocumentEntity, oldID,
	).Scan(&newID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return newID, true, nil
}

func normalizeUUIDGroup2BlankDocumentRefs(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup2DocumentRefs() {
		dataType, err := columnDataType(ctx, tx, ref.table, ref.column)
		if err != nil {
			return err
		}
		if dataType == "uuid" {
			continue
		}
		switch ref.policy {
		case uuidGroup2DocRefOptional:
			continue
		case uuidGroup2DocRefDisposable:
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(
				`DELETE FROM %s WHERE %s IS NULL OR %s = ''`,
				quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column),
			)); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveUUIDGroup2DisposableRefs(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup2DocumentRefs() {
		if ref.policy != uuidGroup2DocRefDisposable {
			continue
		}
		dataType, err := columnDataType(ctx, tx, ref.table, ref.column)
		if err != nil {
			return err
		}
		if dataType == "uuid" {
			_, err = tx.ExecContext(ctx, fmt.Sprintf(
				`DELETE FROM %s AS target
				  WHERE target.%s IS NULL
				     OR NOT EXISTS (
					 SELECT 1 FROM documents AS doc
					  WHERE doc.workspace_id = target.workspace_id
					    AND doc.id = target.%s
				     )`,
				quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column),
			))
			if err != nil {
				return err
			}
			continue
		}
		if ref.table == "agent_document_views" && ref.column == "document_id" {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM agent_document_views AS old_view
				  USING uuid_migration_map AS map
				 WHERE map.entity_type = $1
				   AND old_view.document_id = map.old_id
				   AND EXISTS (
					SELECT 1
					  FROM agent_document_views AS new_view
					 WHERE new_view.workspace_id = old_view.workspace_id
					   AND new_view.agent_id = old_view.agent_id
					   AND new_view.document_id = map.new_id::text
				   )`,
				uuidGroup2DocumentEntity,
			); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s AS target
			  WHERE target.%s IS NULL
			     OR target.%s = ''
			     OR (
				 target.%s !~* $1
				 AND NOT EXISTS (
					 SELECT 1 FROM uuid_migration_map AS map
					  WHERE map.entity_type = $2
					    AND map.old_id = target.%s
				 )
			     )
			     OR (
				 target.%s ~* $1
				 AND NOT EXISTS (
					 SELECT 1 FROM documents AS doc
					  WHERE doc.workspace_id = target.workspace_id
					    AND doc.id::text = target.%s
				 )
				 AND NOT EXISTS (
					 SELECT 1 FROM uuid_migration_map AS map
					  WHERE map.entity_type = $2
					    AND map.new_id::text = LOWER(target.%s::text)
				 )
			     )`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column),
			quoteIdent(ref.column), quoteIdent(ref.column),
			quoteIdent(ref.column), quoteIdent(ref.column), quoteIdent(ref.column),
		), uuidTextPattern, uuidGroup2DocumentEntity)
		if err != nil {
			return err
		}
	}
	return nil
}

type uuidGroup2DeletedParentThread struct {
	workspaceID      string
	threadID         string
	documentID       string
	title            string
	messageCount     int
	participantCount int
}

func deleteUUIDGroup2DeletedParentThreadSubtrees(ctx context.Context, tx *sql.Tx) error {
	dataType, err := columnDataType(ctx, tx, "threads", "document_id")
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	documentIDType, err := columnDataType(ctx, tx, "documents", "id")
	if err != nil {
		return err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT t.workspace_id::text,
		        t.id::text,
		        t.document_id::text,
		        t.title,
		        (SELECT COUNT(*)
		           FROM thread_messages AS m
		          WHERE m.workspace_id::text = t.workspace_id::text
		            AND m.thread_id::text = t.id::text) AS message_count,
		        (SELECT COUNT(*)
		           FROM thread_participants AS p
		          WHERE p.workspace_id::text = t.workspace_id::text
		            AND p.thread_id::text = t.id::text) AS participant_count
		   FROM threads AS t
		  WHERE t.document_id IS NOT NULL
		    AND t.document_id <> ''
		  ORDER BY t.workspace_id::text, t.id::text`)
	if err != nil {
		return err
	}
	threads := []uuidGroup2DeletedParentThread{}
	for rows.Next() {
		var thread uuidGroup2DeletedParentThread
		if err := rows.Scan(&thread.workspaceID, &thread.threadID, &thread.documentID, &thread.title, &thread.messageCount, &thread.participantCount); err != nil {
			rows.Close()
			return err
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, thread := range threads {
		exists, err := uuidGroup2DocumentRowExistsForRef(ctx, tx, documentIDType, thread.workspaceID, thread.documentID)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		log.Printf(
			"uuid group2 deleting deleted-parent thread workspace_id=%s thread_id=%s document_id=%s title=%q message_count=%d participant_count=%d",
			thread.workspaceID, thread.threadID, thread.documentID, thread.title, thread.messageCount, thread.participantCount,
		)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM thread_messages
			  WHERE workspace_id::text = $1
			    AND thread_id::text = $2`,
			thread.workspaceID, thread.threadID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM thread_participants
			  WHERE workspace_id::text = $1
			    AND thread_id::text = $2`,
			thread.workspaceID, thread.threadID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM threads
			  WHERE workspace_id::text = $1
			    AND id::text = $2`,
			thread.workspaceID, thread.threadID,
		); err != nil {
			return err
		}
	}
	return nil
}

func uuidGroup2DocumentRowExistsForRef(ctx context.Context, tx *sql.Tx, documentsIDType, workspaceID, documentID string) (bool, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	documentID = strings.TrimSpace(documentID)
	if workspaceID == "" || documentID == "" {
		return false, nil
	}
	if documentsIDType == "uuid" {
		if stripped, err := stripPrefixedUUID(documentID, "doc_"); err == nil {
			documentID = stripped
		}
	}

	var exists bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1
			  FROM documents AS d
			 WHERE d.workspace_id::text = $1
			   AND d.id::text = $2
		)`,
		workspaceID, documentID,
	).Scan(&exists)
	return exists, err
}

func ensureUUIDGroup2DataBearingRefsMapped(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup2DocumentRefs() {
		if ref.policy == uuidGroup2DocRefDisposable {
			continue
		}
		if ref.table == "workspace_members" && ref.column == "last_accessed_document_id" {
			continue
		}
		if err := ensureUUIDGroup2DocumentRefMapped(ctx, tx, ref.table, ref.column); err != nil {
			return err
		}
	}
	return nil
}

func ensureUUIDGroup2DocumentRefMapped(ctx context.Context, tx *sql.Tx, table, column string) error {
	dataType, err := columnDataType(ctx, tx, table, column)
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT target.%s::text, COUNT(*)
		   FROM %s AS target
		   LEFT JOIN uuid_migration_map AS map
		     ON map.entity_type = $1
		    AND target.%s = map.old_id
		  WHERE target.%s IS NOT NULL
		    AND target.%s <> ''
		    AND target.%s !~* $2
		    AND map.old_id IS NULL
		  GROUP BY target.%s::text
		  ORDER BY target.%s::text
		  LIMIT 5`,
		quoteIdent(column), quoteIdent(table), quoteIdent(column),
		quoteIdent(column), quoteIdent(column), quoteIdent(column),
		quoteIdent(column), quoteIdent(column),
	), uuidGroup2DocumentEntity, uuidTextPattern)
	if err != nil {
		return err
	}
	defer rows.Close()
	missing := []string{}
	for rows.Next() {
		var value string
		var count int
		if err := rows.Scan(&value, &count); err != nil {
			return err
		}
		missing = append(missing, fmt.Sprintf("%q (%d)", value, count))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s.%s has unmapped document references: %s", table, column, strings.Join(missing, ", "))
	}
	return nil
}

func regenerateUUIDGroup2RootDocuments(ctx context.Context, tx *sql.Tx) error {
	plans, err := buildUUIDGroup2RootPlans(ctx, tx)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		update, crdtState, stateVector, err := buildUUIDGroup2RootUpdate(plan.clientID, plan.entries)
		if err != nil {
			return fmt.Errorf("workspace %s root %s: %w", plan.workspaceID, plan.oldRootID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, plan.workspaceID, plan.oldRootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_updates WHERE workspace_id = $1 AND document_id = $2`, plan.workspaceID, plan.oldRootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_heads WHERE workspace_id = $1 AND document_id = $2`, plan.workspaceID, plan.oldRootID); err != nil {
			return err
		}
		var updateID int64
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, NULL, 'system', $4)
			 RETURNING id`,
			plan.workspaceID, plan.newRootID, update, time.Now().UTC(),
		).Scan(&updateID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			plan.workspaceID, plan.newRootID, stateVector, updateID, time.Now().UTC(),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			plan.workspaceID, plan.newRootID, updateID, crdtState, stateVector, time.Now().UTC(),
		); err != nil {
			return err
		}
	}
	return nil
}

func buildUUIDGroup2RootPlans(ctx context.Context, tx *sql.Tx) ([]uuidGroup2RootPlan, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT w.id::text,
		        w.root_document_id::text,
		        d.client_id_seed,
		        COALESCE(h.update_id, 0)
		   FROM workspaces AS w
		   JOIN documents AS d
		     ON d.workspace_id = w.id
		    AND d.id::text = w.root_document_id::text
		   LEFT JOIN document_heads AS h
		     ON h.workspace_id = w.id
		    AND h.document_id::text = w.root_document_id::text
		  ORDER BY w.id::text`)
	if err != nil {
		return nil, err
	}
	roots := []uuidGroup2RootPlan{}
	for rows.Next() {
		var root uuidGroup2RootPlan
		var clientID int64
		if err := rows.Scan(&root.workspaceID, &root.oldRootID, &clientID, &root.headID); err != nil {
			rows.Close()
			return nil, err
		}
		root.clientID = uint64(clientID)
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	plans := []uuidGroup2RootPlan{}
	for _, plan := range roots {
		mapped, ok, err := lookupUUIDGroup2DocumentMapping(ctx, tx, plan.oldRootID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("workspace %s root %s has no document mapping", plan.workspaceID, plan.oldRootID)
		}
		plan.newRootID = mapped
		doc, err := restoreUUIDGroup2DocumentDoc(ctx, tx, plan.workspaceID, plan.oldRootID, plan.clientID, plan.headID)
		if err != nil {
			return nil, err
		}
		entries, err := decodeUUIDGroup2RootEntries(doc)
		doc.Close()
		if err != nil {
			return nil, err
		}
		mappedEntries, err := mapUUIDGroup2RootEntries(ctx, tx, plan.workspaceID, entries)
		if err != nil {
			return nil, fmt.Errorf("workspace %s root %s: %w", plan.workspaceID, plan.oldRootID, err)
		}
		plan.entries = mappedEntries
		plans = append(plans, plan)
	}
	var missing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM workspaces AS w
		   LEFT JOIN documents AS d
		     ON d.workspace_id = w.id
		    AND d.id::text = w.root_document_id::text
		  WHERE d.id IS NULL`).Scan(&missing); err != nil {
		return nil, err
	}
	if missing > 0 {
		return nil, fmt.Errorf("%d workspace root document refs do not resolve", missing)
	}
	return plans, nil
}

func restoreUUIDGroup2DocumentDoc(ctx context.Context, tx *sql.Tx, workspaceID, documentID string, clientID uint64, headUpdateID int64) (*crdt.Doc, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	appliedThrough := int64(0)
	var checkpointState string
	var checkpointUpdateID int64
	err := tx.QueryRowContext(ctx,
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id::text = $1 AND document_id::text = $2 AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		workspaceID, documentID, headUpdateID,
	).Scan(&checkpointUpdateID, &checkpointState)
	if err != nil && err != sql.ErrNoRows {
		doc.Close()
		return nil, err
	}
	if err == nil && checkpointState != "" {
		update, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
		if decodeErr != nil {
			doc.Close()
			return nil, decodeErr
		}
		if applyErr := crdt.ApplyUpdateV1(doc, update, "uuid-group2-checkpoint"); applyErr != nil {
			doc.Close()
			return nil, applyErr
		}
		appliedThrough = checkpointUpdateID
	}
	if appliedThrough > headUpdateID {
		appliedThrough = 0
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT update
		   FROM document_updates
		  WHERE workspace_id::text = $1
		    AND document_id::text = $2
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		workspaceID, documentID, appliedThrough, headUpdateID,
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
		if err := crdt.ApplyUpdateV1(doc, update, "uuid-group2-history"); err != nil {
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

func decodeUUIDGroup2RootEntries(doc *crdt.Doc) ([]uuidGroup2RootEntry, error) {
	if doc == nil {
		return nil, nil
	}
	entries := []uuidGroup2RootEntry{}
	root := doc.GetMap(rootMapName)
	if err := doc.Read(func(txn *crdt.Transaction) error {
		entriesMap, ok, err := root.GetMap(txn, rootEntriesMapName)
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
			entry, ok, err := decodeUUIDGroup2RootEntryMap(txn, item.Key, item.MapValue)
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntryID < entries[j].EntryID })
	return entries, nil
}

func decodeUUIDGroup2RootEntryMap(txn *crdt.Transaction, entryID string, entryMap *crdt.YMap) (uuidGroup2RootEntry, bool, error) {
	entry := uuidGroup2RootEntry{EntryID: strings.TrimSpace(entryID)}
	if entry.EntryID == "" || entryMap == nil {
		return uuidGroup2RootEntry{}, false, nil
	}
	if value, ok, err := entryMap.GetString(txn, "kind"); err != nil {
		return uuidGroup2RootEntry{}, false, err
	} else if ok {
		entry.Kind = strings.TrimSpace(value)
	}
	if value, ok, err := entryMap.GetString(txn, "contentDocumentId"); err != nil {
		return uuidGroup2RootEntry{}, false, err
	} else if ok {
		entry.ContentDocumentID = strings.TrimSpace(value)
	}
	if value, ok, err := entryMap.GetString(txn, "loc"); err != nil {
		return uuidGroup2RootEntry{}, false, err
	} else if ok {
		var loc uuidGroup2RootLoc
		if err := json.Unmarshal([]byte(value), &loc); err == nil {
			entry.ParentID = normalizeUUIDGroup2RootPath(loc.ParentID)
			entry.Name = normalizeUUIDGroup2RootPath(loc.Name)
		}
	}
	if value, ok, err := entryMap.GetString(txn, "deleted"); err != nil {
		return uuidGroup2RootEntry{}, false, err
	} else if ok {
		entry.Deleted = strings.EqualFold(strings.TrimSpace(value), migrationRootDeletedTrue)
	}
	if entry.Kind == "" {
		entry.Kind = migrationRootEntryKindFile
	}
	if entry.Kind != migrationRootEntryKindFile || entry.ContentDocumentID == "" {
		return uuidGroup2RootEntry{}, false, nil
	}
	return entry, true, nil
}

func mapUUIDGroup2RootEntries(ctx context.Context, tx *sql.Tx, workspaceID string, entries []uuidGroup2RootEntry) ([]uuidGroup2RootEntry, error) {
	byMappedID := map[string]uuidGroup2RootEntry{}
	for _, entry := range entries {
		if err := verifyUUIDGroup2RootEntryTargetsContentDocument(ctx, tx, workspaceID, entry.ContentDocumentID); err != nil {
			return nil, fmt.Errorf("root entry %s content document %q: %w", entry.EntryID, entry.ContentDocumentID, err)
		}
		mappedID, ok, err := lookupUUIDGroup2DocumentMapping(ctx, tx, entry.ContentDocumentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			if isUUIDString(entry.ContentDocumentID) {
				mappedID = uuid.MustParse(entry.ContentDocumentID).String()
			} else {
				return nil, fmt.Errorf("root entry %s content document %q has no mapping", entry.EntryID, entry.ContentDocumentID)
			}
		}
		entry.EntryID = mappedID
		entry.ContentDocumentID = mappedID
		if existing, exists := byMappedID[mappedID]; exists && !uuidGroup2RootEntriesEqual(existing, entry) {
			return nil, fmt.Errorf("root entry collision after mapping for %s", mappedID)
		}
		byMappedID[mappedID] = entry
	}
	mapped := make([]uuidGroup2RootEntry, 0, len(byMappedID))
	for _, entry := range byMappedID {
		mapped = append(mapped, entry)
	}
	sort.Slice(mapped, func(i, j int) bool { return mapped[i].EntryID < mapped[j].EntryID })
	return mapped, nil
}

func verifyUUIDGroup2RootEntryTargetsContentDocument(ctx context.Context, tx *sql.Tx, workspaceID, documentID string) error {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return errors.New("content document id is required")
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT d.hidden
		   FROM documents AS d
		  WHERE d.workspace_id::text = $1
		    AND d.id::text = $2
		  UNION ALL
		 SELECT d.hidden
		   FROM documents AS d
		   JOIN uuid_migration_map AS map
		     ON map.entity_type = $3
		    AND map.old_id = d.id::text
		  WHERE d.workspace_id::text = $1
		    AND map.new_id::text = LOWER($2)
		  LIMIT 1`,
		workspaceID, documentID, uuidGroup2DocumentEntity,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return errors.New("does not resolve to a document")
	}
	var hidden bool
	if err := rows.Scan(&hidden); err != nil {
		return err
	}
	if hidden {
		return errors.New("resolves to a root or hidden document")
	}
	return rows.Err()
}

func uuidGroup2RootEntriesEqual(a, b uuidGroup2RootEntry) bool {
	return a.EntryID == b.EntryID &&
		a.Kind == b.Kind &&
		a.ContentDocumentID == b.ContentDocumentID &&
		a.ParentID == b.ParentID &&
		a.Name == b.Name &&
		a.Deleted == b.Deleted
}

func buildUUIDGroup2RootUpdate(clientID uint64, entries []uuidGroup2RootEntry) ([]byte, string, string, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	defer doc.Close()
	root := doc.GetMap(rootMapName)
	update, err := doc.Update(func(txn *crdt.Transaction) error {
		entriesMap, err := ensureUUIDGroup2RootEntriesMap(txn, root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := setUUIDGroup2RootFileEntry(txn, entriesMap, entry); err != nil {
				return err
			}
		}
		return nil
	}, "uuid-group2-root-regeneration")
	if err != nil {
		return nil, "", "", err
	}
	crdtState := base64.StdEncoding.EncodeToString(doc.EncodeStateAsUpdate())
	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	return update, crdtState, stateVector, nil
}

func ensureUUIDGroup2RootEntriesMap(txn *crdt.Transaction, root *crdt.YMap) (*crdt.YMap, error) {
	entriesMap, ok, err := root.GetMap(txn, rootEntriesMapName)
	if err != nil {
		return nil, err
	}
	if ok {
		return entriesMap, nil
	}
	return root.SetMap(txn, rootEntriesMapName)
}

func setUUIDGroup2RootFileEntry(txn *crdt.Transaction, entriesMap *crdt.YMap, entry uuidGroup2RootEntry) error {
	entry.EntryID = strings.TrimSpace(entry.EntryID)
	entry.ContentDocumentID = strings.TrimSpace(entry.ContentDocumentID)
	if entry.EntryID == "" || entry.ContentDocumentID == "" {
		return errors.New("root entry requires entry and content document IDs")
	}
	entryMap, ok, err := entriesMap.GetMap(txn, entry.EntryID)
	if err != nil {
		return err
	}
	if !ok {
		entryMap, err = entriesMap.SetMap(txn, entry.EntryID)
		if err != nil {
			return err
		}
	}
	if err := entryMap.SetString(txn, "kind", migrationRootEntryKindFile); err != nil {
		return err
	}
	if err := entryMap.SetString(txn, "contentDocumentId", entry.ContentDocumentID); err != nil {
		return err
	}
	if err := entryMap.SetString(txn, "loc", uuidGroup2RootEntryPathLoc(entry.desiredPath())); err != nil {
		return err
	}
	if entry.Deleted {
		return entryMap.SetString(txn, "deleted", migrationRootDeletedTrue)
	}
	return entryMap.SetString(txn, "deleted", migrationRootDeletedFalse)
}

func (e uuidGroup2RootEntry) desiredPath() string {
	name := normalizeUUIDGroup2RootPath(e.Name)
	parent := normalizeUUIDGroup2RootPath(e.ParentID)
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return normalizeUUIDGroup2RootPath(parent + "/" + name)
}

func uuidGroup2RootEntryPathLoc(path string) string {
	encoded, _ := json.Marshal(uuidGroup2RootLoc{Name: normalizeUUIDGroup2RootPath(path)})
	return string(encoded)
}

func normalizeUUIDGroup2RootPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." {
		return ""
	}
	return clean
}

func rewriteUUIDGroup2DocumentRefs(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup2DocumentRefs() {
		dataType, err := columnDataType(ctx, tx, ref.table, ref.column)
		if err != nil {
			return err
		}
		if dataType == "uuid" {
			continue
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s AS target
			    SET %s = map.new_id::text
			   FROM uuid_migration_map AS map
			  WHERE map.entity_type = $1
			    AND target.%s = map.old_id`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column),
		), uuidGroup2DocumentEntity)
		if err != nil {
			return err
		}
	}
	return nil
}

func rewriteUUIDGroup2DocumentEntityIDs(ctx context.Context, tx *sql.Tx) error {
	dataType, err := columnDataType(ctx, tx, "documents", "id")
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE documents AS target
		    SET id = map.new_id::text
		   FROM uuid_migration_map AS map
		  WHERE map.entity_type = $1
		    AND target.id = map.old_id`,
		uuidGroup2DocumentEntity,
	)
	return err
}

func clearUUIDGroup2OptionalWorkspaceMemberOrphans(ctx context.Context, tx *sql.Tx) error {
	dataType, err := columnDataType(ctx, tx, "workspace_members", "last_accessed_document_id")
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		_, err = tx.ExecContext(ctx,
			`UPDATE workspace_members AS m
			    SET last_accessed_document_id = NULL
			  WHERE last_accessed_document_id IS NOT NULL
			    AND NOT EXISTS (
				SELECT 1 FROM documents AS d
				 WHERE d.workspace_id = m.workspace_id
				   AND d.id = m.last_accessed_document_id
			    )`)
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE workspace_members AS m
		    SET last_accessed_document_id = NULL
		  WHERE last_accessed_document_id IS NOT NULL
		    AND last_accessed_document_id <> ''
		    AND NOT EXISTS (
			SELECT 1 FROM documents AS d
			 WHERE d.workspace_id = m.workspace_id
			   AND d.id::text = m.last_accessed_document_id
		    )`)
	return err
}

func convertUUIDGroup2DocumentColumns(ctx context.Context, tx *sql.Tx) error {
	if err := convertColumnToUUID(ctx, tx, "documents", "id", false); err != nil {
		return err
	}
	for _, ref := range uuidGroup2DocumentRefs() {
		nullable := ref.policy == uuidGroup2DocRefOptional
		if err := convertColumnToUUID(ctx, tx, ref.table, ref.column, nullable); err != nil {
			return err
		}
	}
	return nil
}

func verifyUUIDGroup2BootShapeTx(ctx context.Context, tx *sql.Tx) error {
	for _, column := range uuidGroup2DocumentColumnInventory() {
		dataType, err := columnDataType(ctx, tx, column.table, column.column)
		if err != nil {
			return err
		}
		if dataType != "uuid" {
			return fmt.Errorf("%s.%s type = %s, want uuid", column.table, column.column, dataType)
		}
	}
	for _, column := range []struct {
		table  string
		column string
	}{
		{"documents", "path"},
		{"documents", "title"},
		{"presences", "file_path"},
	} {
		dataType, err := columnDataType(ctx, tx, column.table, column.column)
		if err != nil {
			return err
		}
		if dataType != "text" {
			return fmt.Errorf("%s.%s type = %s, want text", column.table, column.column, dataType)
		}
	}
	return verifyNoPrefixedValuesInUUIDGroup2Columns(ctx, tx)
}

func verifyUUIDGroup2DeepTx(ctx context.Context, tx *sql.Tx) error {
	if err := verifyUUIDGroup2BootShapeTx(ctx, tx); err != nil {
		return fmt.Errorf("boot shape: %w", err)
	}
	if err := verifyUUIDGroup2DocumentRefsResolve(ctx, tx); err != nil {
		return fmt.Errorf("document refs resolve: %w", err)
	}
	if err := verifyUUIDGroup2RootDocuments(ctx, tx); err != nil {
		return fmt.Errorf("root documents: %w", err)
	}
	if err := verifyTextColumnInventoryClassified(ctx, tx); err != nil {
		return fmt.Errorf("text column inventory: %w", err)
	}
	return nil
}

func verifyNoPrefixedValuesInUUIDGroup2Columns(ctx context.Context, tx *sql.Tx) error {
	for _, column := range uuidGroup2DocumentColumnInventory() {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s::text
			   FROM %s
			  WHERE %s IS NOT NULL
			  LIMIT 5`,
			quoteIdent(column.column), quoteIdent(column.table), quoteIdent(column.column),
		))
		if err != nil {
			return err
		}
		bad := []string{}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return err
			}
			if strings.HasPrefix(value, "doc_") || strings.HasPrefix(value, "doc_root_") {
				bad = append(bad, value)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(bad) > 0 {
			return fmt.Errorf("%s.%s still has prefixed document values: %s", column.table, column.column, strings.Join(bad, ", "))
		}
	}
	return nil
}

func verifyUUIDGroup2DocumentRefsResolve(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup2DocumentRefs() {
		condition := fmt.Sprintf("target.%s IS NOT NULL", quoteIdent(ref.column))
		if ref.policy != uuidGroup2DocRefOptional {
			condition = fmt.Sprintf("target.%s IS NOT NULL", quoteIdent(ref.column))
		}
		workspaceJoinColumn := "target.workspace_id"
		if ref.table == "workspaces" && ref.column == "root_document_id" {
			workspaceJoinColumn = "target.id"
		}
		var count int
		err := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COUNT(*)
			   FROM %s AS target
			   LEFT JOIN documents AS doc
			     ON doc.workspace_id::text = (%s)::text
			    AND doc.id::text = target.%s::text
			  WHERE %s
			    AND doc.id IS NULL`,
			quoteIdent(ref.table), workspaceJoinColumn, quoteIdent(ref.column), condition,
		)).Scan(&count)
		if err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%s.%s has %d unresolved document references", ref.table, ref.column, count)
		}
	}
	var badRoots int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM workspaces AS w
		   LEFT JOIN documents AS d
		     ON d.workspace_id::text = w.id::text
		    AND d.id::text = w.root_document_id::text
		  WHERE d.id IS NULL
		     OR d.hidden IS DISTINCT FROM TRUE`).Scan(&badRoots); err != nil {
		return err
	}
	if badRoots > 0 {
		return fmt.Errorf("%d workspaces have missing or non-hidden root documents", badRoots)
	}
	return nil
}

func verifyUUIDGroup2RootDocuments(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT w.id::text,
		        w.root_document_id::text,
		        d.client_id_seed,
		        COALESCE(h.update_id, 0)
		   FROM workspaces AS w
		   JOIN documents AS d
		     ON d.workspace_id::text = w.id::text
		    AND d.id::text = w.root_document_id::text
		   LEFT JOIN document_heads AS h
		     ON h.workspace_id::text = w.id::text
		    AND h.document_id::text = w.root_document_id::text
		  ORDER BY w.id::text`)
	if err != nil {
		return err
	}
	type rootDocumentCheck struct {
		workspaceID string
		rootID      string
		clientID    int64
		headID      int64
	}
	checks := []rootDocumentCheck{}
	for rows.Next() {
		var check rootDocumentCheck
		if err := rows.Scan(&check.workspaceID, &check.rootID, &check.clientID, &check.headID); err != nil {
			rows.Close()
			return err
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, check := range checks {
		doc, err := restoreUUIDGroup2DocumentDoc(ctx, tx, check.workspaceID, check.rootID, uint64(check.clientID), check.headID)
		if err != nil {
			return err
		}
		entries, err := decodeUUIDGroup2RootEntries(doc)
		doc.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !isUUIDString(entry.EntryID) {
				return fmt.Errorf("workspace %s root entry key %q is not a UUID", check.workspaceID, entry.EntryID)
			}
			if !isUUIDString(entry.ContentDocumentID) {
				return fmt.Errorf("workspace %s root entry %s content document %q is not a UUID", check.workspaceID, entry.EntryID, entry.ContentDocumentID)
			}
			if entry.EntryID != entry.ContentDocumentID {
				return fmt.Errorf("workspace %s root entry key %q does not match content document %q", check.workspaceID, entry.EntryID, entry.ContentDocumentID)
			}
			var hidden bool
			if err := tx.QueryRowContext(ctx,
				`SELECT hidden
				   FROM documents
				  WHERE workspace_id::text = $1
				    AND id::text = $2`,
				check.workspaceID, entry.ContentDocumentID,
			).Scan(&hidden); err != nil {
				if err == sql.ErrNoRows {
					return fmt.Errorf("workspace %s root entry %s references missing document %s", check.workspaceID, entry.EntryID, entry.ContentDocumentID)
				}
				return err
			}
			if hidden {
				return fmt.Errorf("workspace %s root entry %s references root or hidden document %s", check.workspaceID, entry.EntryID, entry.ContentDocumentID)
			}
		}
	}
	return nil
}
