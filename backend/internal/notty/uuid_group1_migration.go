package notty

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

const uuidMigrationMapTable = "uuid_migration_map"

const (
	uuidGroup1AdvisoryLockNamespace = 581914713
	uuidGroup1AdvisoryLockID        = 1
)

var errWorkspaceMigrated = errors.New("workspace migrated; reinstall daemon")

type UUIDGroup1ColumnBucket string

const (
	UUIDGroup1EntityID        UUIDGroup1ColumnBucket = "entity_id"
	UUIDGroup1RequiredRef     UUIDGroup1ColumnBucket = "required_ref"
	UUIDGroup1OptionalRef     UUIDGroup1ColumnBucket = "optional_ref"
	UUIDGroup1PolymorphicRef  UUIDGroup1ColumnBucket = "polymorphic_ref"
	UUIDGroup1DocumentID      UUIDGroup1ColumnBucket = "document_id_group2"
	UUIDGroup1OpaqueText      UUIDGroup1ColumnBucket = "opaque_text"
	UUIDGroup1HumanText       UUIDGroup1ColumnBucket = "human_text"
	UUIDGroup1OperationalText UUIDGroup1ColumnBucket = "operational_text"
)

type UUIDGroup1Column struct {
	Table  string
	Column string
	Bucket UUIDGroup1ColumnBucket
	Note   string
}

type uuidEntitySpec struct {
	entity string
	table  string
	column string
	prefix string
}

type uuidReferenceSpec struct {
	table    string
	column   string
	entity   string
	nullable bool
}

type uuidUnionReferenceSpec struct {
	table    string
	column   string
	entities []string
	nullable bool
	nullText map[string]struct{}
}

type uuidPolymorphicReferenceSpec struct {
	table      string
	column     string
	typeColumn string
	typeEntity map[string]string
	nullTypes  map[string]struct{}
	nullable   bool
}

func uuidGroup1Entities() []uuidEntitySpec {
	return []uuidEntitySpec{
		{entity: "workspaces", table: "workspaces", column: "id", prefix: "ws_"},
		{entity: "accounts", table: "accounts", column: "id", prefix: "account_"},
		{entity: "account_email_tokens", table: "account_email_tokens", column: "id", prefix: "account_email_token_"},
		{entity: "workspace_invites", table: "workspace_invites", column: "id", prefix: "invite_"},
		{entity: "daemons", table: "daemons", column: "id", prefix: "daemon_"},
		{entity: "users", table: "users", column: "id", prefix: "user_"},
		{entity: "agents", table: "agents", column: "id", prefix: "agent_"},
		{entity: "agent_runs", table: "agent_runs", column: "id", prefix: "run_"},
		{entity: "threads", table: "threads", column: "id", prefix: "thread_"},
		{entity: "thread_messages", table: "thread_messages", column: "id", prefix: "threadmsg_"},
		{entity: "agent_events", table: "agent_events", column: "id", prefix: "aevt_"},
	}
}

func uuidGroup1References() []uuidReferenceSpec {
	workspaceTables := []string{
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
	refs := make([]uuidReferenceSpec, 0, len(workspaceTables)+12)
	for _, table := range workspaceTables {
		refs = append(refs, uuidReferenceSpec{table: table, column: "workspace_id", entity: "workspaces"})
	}
	refs = append(refs,
		uuidReferenceSpec{table: "account_email_tokens", column: "account_id", entity: "accounts"},
		uuidReferenceSpec{table: "workspace_members", column: "account_id", entity: "accounts"},
		uuidReferenceSpec{table: "workspace_members", column: "user_id", entity: "users"},
		uuidReferenceSpec{table: "workspace_invites", column: "created_by_user_id", entity: "users"},
		uuidReferenceSpec{table: "agent_runs", column: "agent_id", entity: "agents"},
		uuidReferenceSpec{table: "agent_events", column: "agent_id", entity: "agents"},
		uuidReferenceSpec{table: "agent_document_views", column: "agent_id", entity: "agents"},
		uuidReferenceSpec{table: "thread_messages", column: "thread_id", entity: "threads"},
		uuidReferenceSpec{table: "thread_participants", column: "thread_id", entity: "threads"},
		uuidReferenceSpec{table: "accounts", column: "last_accessed_workspace_id", entity: "workspaces", nullable: true},
		uuidReferenceSpec{table: "workspace_members", column: "invited_by", entity: "users", nullable: true},
		uuidReferenceSpec{table: "agents", column: "daemon_id", entity: "daemons", nullable: true},
		uuidReferenceSpec{table: "agents", column: "current_run_id", entity: "agent_runs", nullable: true},
		uuidReferenceSpec{table: "agent_events", column: "thread_id", entity: "threads", nullable: true},
		uuidReferenceSpec{table: "agent_events", column: "thread_message_id", entity: "thread_messages", nullable: true},
		uuidReferenceSpec{table: "agent_events", column: "run_id", entity: "agent_runs", nullable: true},
	)
	return refs
}

func uuidGroup1UnionReferences() []uuidUnionReferenceSpec {
	return []uuidUnionReferenceSpec{
		{table: "thread_participants", column: "participant_id", entities: []string{"users", "agents"}},
		{table: "agent_events", column: "claimed_by", entities: []string{"daemons", "agents"}, nullable: true, nullText: setOf("", "daemon", "system")},
	}
}

func uuidGroup1PolymorphicReferences() []uuidPolymorphicReferenceSpec {
	actorTypes := map[string]string{"human": "users", "agent": "agents", "daemon": "daemons"}
	nullSystem := setOf("", "system")
	return []uuidPolymorphicReferenceSpec{
		{table: "document_updates", column: "actor_id", typeColumn: "actor_type", typeEntity: actorTypes, nullTypes: nullSystem, nullable: true},
		{table: "threads", column: "created_by_id", typeColumn: "created_by_type", typeEntity: map[string]string{"human": "users", "agent": "agents"}, nullable: true},
		{table: "thread_messages", column: "author_id", typeColumn: "author_type", typeEntity: map[string]string{"human": "users", "agent": "agents"}, nullable: true},
		{table: "presences", column: "actor_id", typeColumn: "actor_type", typeEntity: actorTypes},
		{table: "activities", column: "actor_id", typeColumn: "actor_type", typeEntity: actorTypes, nullTypes: nullSystem, nullable: true},
		{table: "activities", column: "provenance_actor_id", typeColumn: "provenance_actor_type", typeEntity: actorTypes, nullTypes: nullSystem, nullable: true},
	}
}

func UUIDGroup1ColumnInventory() []UUIDGroup1Column {
	columns := []UUIDGroup1Column{}
	add := func(table, column string, bucket UUIDGroup1ColumnBucket, note string) {
		columns = append(columns, UUIDGroup1Column{Table: table, Column: column, Bucket: bucket, Note: note})
	}
	for _, spec := range uuidGroup1Entities() {
		add(spec.table, spec.column, UUIDGroup1EntityID, spec.entity)
	}
	for _, spec := range uuidGroup1References() {
		bucket := UUIDGroup1RequiredRef
		if spec.nullable {
			bucket = UUIDGroup1OptionalRef
		}
		add(spec.table, spec.column, bucket, spec.entity)
	}
	for _, spec := range uuidGroup1UnionReferences() {
		bucket := UUIDGroup1RequiredRef
		if spec.nullable {
			bucket = UUIDGroup1OptionalRef
		}
		add(spec.table, spec.column, bucket, strings.Join(spec.entities, "|"))
	}
	for _, spec := range uuidGroup1PolymorphicReferences() {
		bucket := UUIDGroup1PolymorphicRef
		if spec.nullable {
			bucket = UUIDGroup1OptionalRef
		}
		add(spec.table, spec.column, bucket, spec.typeColumn)
	}

	for _, item := range []struct {
		table  string
		column string
	}{
		{"documents", "id"},
		{"workspaces", "root_document_id"},
		{"document_heads", "document_id"},
		{"document_updates", "document_id"},
		{"document_checkpoints", "document_id"},
		{"threads", "document_id"},
		{"presences", "document_id"},
		{"activities", "document_id"},
		{"agent_events", "document_id"},
		{"agent_document_views", "document_id"},
		{"workspace_members", "last_accessed_document_id"},
	} {
		add(item.table, item.column, UUIDGroup1DocumentID, "Group 2 document identity")
	}

	for _, item := range []struct {
		table  string
		column string
		bucket UUIDGroup1ColumnBucket
	}{
		{"workspaces", "slug", UUIDGroup1OperationalText},
		{"workspaces", "name", UUIDGroup1HumanText},
		{"accounts", "email", UUIDGroup1OperationalText},
		{"accounts", "display_name", UUIDGroup1HumanText},
		{"accounts", "password_hash", UUIDGroup1OperationalText},
		{"account_email_tokens", "purpose", UUIDGroup1OperationalText},
		{"account_email_tokens", "token_hash", UUIDGroup1OperationalText},
		{"workspace_members", "membership_role", UUIDGroup1OperationalText},
		{"workspace_members", "status", UUIDGroup1OperationalText},
		{"workspace_invites", "token_hash", UUIDGroup1OperationalText},
		{"daemons", "name", UUIDGroup1HumanText},
		{"daemons", "token_hash", UUIDGroup1OperationalText},
		{"daemons", "status", UUIDGroup1OperationalText},
		{"daemons", "daemon_version", UUIDGroup1OperationalText},
		{"daemons", "os", UUIDGroup1OperationalText},
		{"daemons", "arch", UUIDGroup1OperationalText},
		{"documents", "path", UUIDGroup1OperationalText},
		{"documents", "title", UUIDGroup1HumanText},
		{"documents", "create_client_operation_id", UUIDGroup1OperationalText},
		{"document_heads", "state_vector", UUIDGroup1OperationalText},
		{"document_checkpoints", "crdt_state", UUIDGroup1OperationalText},
		{"document_checkpoints", "state_vector", UUIDGroup1OperationalText},
		{"document_updates", "actor_type", UUIDGroup1OperationalText},
		{"users", "handle", UUIDGroup1OperationalText},
		{"users", "name", UUIDGroup1HumanText},
		{"users", "role", UUIDGroup1HumanText},
		{"users", "kind", UUIDGroup1OperationalText},
		{"users", "status", UUIDGroup1OperationalText},
		{"agents", "handle", UUIDGroup1OperationalText},
		{"agents", "name", UUIDGroup1HumanText},
		{"agents", "role", UUIDGroup1HumanText},
		{"agents", "kind", UUIDGroup1OperationalText},
		{"agents", "system_prompt", UUIDGroup1HumanText},
		{"agents", "workspace_root", UUIDGroup1OperationalText},
		{"agents", "current_turn_id", UUIDGroup1OperationalText},
		{"agents", "session_id", UUIDGroup1OperationalText},
		{"agents", "status", UUIDGroup1OperationalText},
		{"agents", "current_task", UUIDGroup1HumanText},
		{"agents", "current_activity", UUIDGroup1HumanText},
		{"agent_runs", "agent_handle", UUIDGroup1OperationalText},
		{"agent_runs", "agent_name", UUIDGroup1HumanText},
		{"agent_runs", "agent_kind", UUIDGroup1OperationalText},
		{"agent_runs", "system_prompt", UUIDGroup1HumanText},
		{"agent_runs", "session_id", UUIDGroup1OperationalText},
		{"agent_runs", "workspace_root", UUIDGroup1OperationalText},
		{"agent_runs", "working_dir", UUIDGroup1OperationalText},
		{"agent_runs", "prompt", UUIDGroup1HumanText},
		{"agent_runs", "status", UUIDGroup1OperationalText},
		{"agent_runs", "desired_status", UUIDGroup1OperationalText},
		{"agent_runs", "last_message", UUIDGroup1HumanText},
		{"agent_runs", "error", UUIDGroup1HumanText},
		{"agent_runs", "assigned_task_ref", UUIDGroup1OperationalText},
		{"threads", "client_operation_id", UUIDGroup1OperationalText},
		{"threads", "title", UUIDGroup1HumanText},
		{"threads", "status", UUIDGroup1OperationalText},
		{"threads", "anchor_relative_start", UUIDGroup1OperationalText},
		{"threads", "anchor_relative_end", UUIDGroup1OperationalText},
		{"threads", "anchor_kind", UUIDGroup1OperationalText},
		{"threads", "anchor_excerpt", UUIDGroup1HumanText},
		{"threads", "created_by_type", UUIDGroup1OperationalText},
		{"threads", "created_by_handle", UUIDGroup1OperationalText},
		{"threads", "created_by_name", UUIDGroup1HumanText},
		{"thread_messages", "author_type", UUIDGroup1OperationalText},
		{"thread_messages", "author_handle", UUIDGroup1OperationalText},
		{"thread_messages", "author_name", UUIDGroup1HumanText},
		{"thread_messages", "body", UUIDGroup1HumanText},
		{"thread_messages", "kind", UUIDGroup1OperationalText},
		{"presences", "actor_type", UUIDGroup1OperationalText},
		{"presences", "file_path", UUIDGroup1OperationalText},
		{"presences", "mode", UUIDGroup1OperationalText},
		{"presences", "activity", UUIDGroup1HumanText},
		{"activities", "type", UUIDGroup1OperationalText},
		{"activities", "actor_type", UUIDGroup1OperationalText},
		{"activities", "summary", UUIDGroup1HumanText},
		{"activities", "provenance_actor_type", UUIDGroup1OperationalText},
		{"activities", "provenance_execution_id", UUIDGroup1OpaqueText},
		{"activities", "provenance_tool", UUIDGroup1OperationalText},
		{"activities", "provenance_trigger", UUIDGroup1OperationalText},
		{"activities", "provenance_confidence", UUIDGroup1OperationalText},
		{"activities", "provenance_requested_by", UUIDGroup1OpaqueText},
		{"activities", "provenance_source", UUIDGroup1OperationalText},
		{"activities", "provenance_intended_scope", UUIDGroup1HumanText},
		{"activities", "provenance_read_set_summary", UUIDGroup1HumanText},
		{"activities", "comment_id", UUIDGroup1OpaqueText},
		{"activities", "presence_ref", UUIDGroup1OpaqueText},
		{"agent_events", "agent_handle", UUIDGroup1OperationalText},
		{"agent_events", "type", UUIDGroup1OperationalText},
		{"agent_events", "box", UUIDGroup1OperationalText},
		{"agent_events", "status", UUIDGroup1OperationalText},
		{"agent_events", "summary", UUIDGroup1HumanText},
		{"agent_events", "prompt", UUIDGroup1HumanText},
		{"agent_events", "dedup_key", UUIDGroup1OperationalText},
		{"agent_events", "last_error", UUIDGroup1HumanText},
		{"agent_document_views", "state_vector", UUIDGroup1OperationalText},
	} {
		add(item.table, item.column, item.bucket, "")
	}
	sort.Slice(columns, func(i, j int) bool {
		if columns[i].Table == columns[j].Table {
			return columns[i].Column < columns[j].Column
		}
		return columns[i].Table < columns[j].Table
	})
	return columns
}

func RunUUIDGroup1Migration(ctx context.Context, db *sql.DB) error {
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

	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, uuidGroup1AdvisoryLockNamespace, uuidGroup1AdvisoryLockID); err != nil {
		return err
	}

	migrated, err := uuidGroup1AlreadyMigrated(ctx, tx)
	if err != nil {
		return err
	}
	if migrated {
		if err = verifyUUIDGroup1BootShapeTx(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}

	if err = ensureUUIDMigrationMap(ctx, tx); err != nil {
		return fmt.Errorf("ensure uuid migration map: %w", err)
	}
	if err = populateUUIDMigrationMap(ctx, tx); err != nil {
		return fmt.Errorf("populate uuid migration map: %w", err)
	}
	if err = adoptOrphanedReferences(ctx, tx); err != nil {
		return fmt.Errorf("adopt orphaned references: %w", err)
	}
	if err = rewriteUUIDGroup1References(ctx, tx); err != nil {
		return fmt.Errorf("rewrite uuid group1 references: %w", err)
	}
	if err = rewriteUUIDGroup1EntityIDs(ctx, tx); err != nil {
		return fmt.Errorf("rewrite uuid group1 entity ids: %w", err)
	}
	if err = resolveOrphanedReferences(ctx, tx); err != nil {
		return fmt.Errorf("resolve orphaned references: %w", err)
	}
	if err = convertUUIDGroup1Columns(ctx, tx); err != nil {
		return fmt.Errorf("convert uuid group1 columns: %w", err)
	}
	if err = verifyUUIDGroup1DeepTx(ctx, tx); err != nil {
		return fmt.Errorf("verify uuid group1 deep: %w", err)
	}
	return tx.Commit()
}

func VerifyUUIDGroup1BootShape(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database is required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return verifyUUIDGroup1BootShapeTx(ctx, tx)
}

func VerifyUUIDGroup1Deep(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database is required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return verifyUUIDGroup1DeepTx(ctx, tx)
}

func verifyUUIDGroup1BootShapeTx(ctx context.Context, tx *sql.Tx) error {
	for _, spec := range appendUUIDEntityAndReferenceColumns() {
		dataType, err := columnDataType(ctx, tx, spec.table, spec.column)
		if err != nil {
			return err
		}
		if dataType != "uuid" {
			return fmt.Errorf("%s.%s type = %s, want uuid", spec.table, spec.column, dataType)
		}
	}
	if err := verifyDocumentColumnsRemainText(ctx, tx); err != nil {
		return err
	}
	if err := verifyNoPrefixedValuesInUUIDColumns(ctx, tx); err != nil {
		return err
	}
	return nil
}

func verifyUUIDGroup1DeepTx(ctx context.Context, tx *sql.Tx) error {
	if err := verifyUUIDGroup1BootShapeTx(ctx, tx); err != nil {
		return err
	}
	if err := verifyUUIDGroup1ReferencesResolve(ctx, tx); err != nil {
		return err
	}
	return verifyTextColumnInventoryClassified(ctx, tx)
}

func appendUUIDEntityAndReferenceColumns() []uuidReferenceSpec {
	columns := []uuidReferenceSpec{}
	for _, entity := range uuidGroup1Entities() {
		columns = append(columns, uuidReferenceSpec{table: entity.table, column: entity.column, entity: entity.entity})
	}
	columns = append(columns, uuidGroup1References()...)
	for _, ref := range uuidGroup1UnionReferences() {
		columns = append(columns, uuidReferenceSpec{table: ref.table, column: ref.column})
	}
	for _, ref := range uuidGroup1PolymorphicReferences() {
		columns = append(columns, uuidReferenceSpec{table: ref.table, column: ref.column})
	}
	return columns
}

func uuidGroup1AlreadyMigrated(ctx context.Context, tx *sql.Tx) (bool, error) {
	for _, spec := range appendUUIDEntityAndReferenceColumns() {
		dataType, err := columnDataType(ctx, tx, spec.table, spec.column)
		if err != nil {
			return false, err
		}
		if dataType != "uuid" {
			return false, nil
		}
	}
	return true, nil
}

func ensureUUIDMigrationMap(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS uuid_migration_map (
			entity_type TEXT NOT NULL,
			old_id TEXT NOT NULL,
			new_id UUID NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (entity_type, old_id),
			UNIQUE (new_id)
		)
	`)
	return err
}

func populateUUIDMigrationMap(ctx context.Context, tx *sql.Tx) error {
	for _, spec := range uuidGroup1Entities() {
		dataType, err := columnDataType(ctx, tx, spec.table, spec.column)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s::text FROM %s ORDER BY %s::text`,
			quoteIdent(spec.column), quoteIdent(spec.table), quoteIdent(spec.column),
		))
		if err != nil {
			return err
		}
		mappings := []struct {
			oldID string
			newID string
		}{}
		for rows.Next() {
			var oldID string
			if err := rows.Scan(&oldID); err != nil {
				rows.Close()
				return err
			}
			newID := oldID
			if dataType == "uuid" {
				oldID = spec.prefix + oldID
			} else {
				newID, err = stripPrefixedUUID(oldID, spec.prefix)
				if err != nil {
					rows.Close()
					return fmt.Errorf("%s.%s %q: %w", spec.table, spec.column, oldID, err)
				}
			}
			mappings = append(mappings, struct {
				oldID string
				newID string
			}{oldID: oldID, newID: newID})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, mapping := range mappings {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO uuid_migration_map (entity_type, old_id, new_id)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (entity_type, old_id)
				 DO UPDATE SET new_id = EXCLUDED.new_id`,
				spec.entity, mapping.oldID, mapping.newID,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func entityPrefixMap() map[string]string {
	m := map[string]string{}
	for _, spec := range uuidGroup1Entities() {
		m[spec.entity] = spec.prefix
	}
	return m
}

func adoptOrphanedReferences(ctx context.Context, tx *sql.Tx) error {
	prefixes := entityPrefixMap()

	adoptColumn := func(table, column, entity string) error {
		dataType, err := columnDataType(ctx, tx, table, column)
		if err != nil {
			return err
		}
		if dataType == "uuid" {
			return nil
		}
		prefix, ok := prefixes[entity]
		if !ok {
			return nil
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT DISTINCT target.%s::text
			   FROM %s AS target
			   LEFT JOIN uuid_migration_map AS map
			     ON map.entity_type = $1
			    AND target.%s = map.old_id
			  WHERE target.%s IS NOT NULL
			    AND target.%s <> ''
			    AND target.%s LIKE $2
			    AND map.old_id IS NULL`,
			quoteIdent(column), quoteIdent(table), quoteIdent(column),
			quoteIdent(column), quoteIdent(column), quoteIdent(column),
		), entity, prefix+"%")
		if err != nil {
			return err
		}
		var orphans []string
		for rows.Next() {
			var oldID string
			if err := rows.Scan(&oldID); err != nil {
				rows.Close()
				return err
			}
			orphans = append(orphans, oldID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		var adopted int
		for _, oldID := range orphans {
			newID, err := stripPrefixedUUID(oldID, prefix)
			if err != nil {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO uuid_migration_map (entity_type, old_id, new_id)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (entity_type, old_id) DO NOTHING`,
				entity, oldID, newID); err != nil {
				return err
			}
			adopted++
		}
		if adopted > 0 {
			log.Printf("uuid migration: adopted %d orphaned %s refs in %s.%s", adopted, entity, table, column)
		}
		return nil
	}

	for _, ref := range uuidGroup1References() {
		if err := adoptColumn(ref.table, ref.column, ref.entity); err != nil {
			return fmt.Errorf("adopt %s.%s: %w", ref.table, ref.column, err)
		}
	}
	for _, ref := range uuidGroup1UnionReferences() {
		for _, entity := range ref.entities {
			if err := adoptColumn(ref.table, ref.column, entity); err != nil {
				return fmt.Errorf("adopt %s.%s: %w", ref.table, ref.column, err)
			}
		}
	}
	for _, ref := range uuidGroup1PolymorphicReferences() {
		for _, entity := range ref.typeEntity {
			if err := adoptColumn(ref.table, ref.column, entity); err != nil {
				return fmt.Errorf("adopt %s.%s: %w", ref.table, ref.column, err)
			}
		}
	}
	return nil
}

var disposableTables = map[string]bool{
	"agent_document_views": true,
	"thread_participants":  true,
	"presences":            true,
}

func resolveOrphanedReferences(ctx context.Context, tx *sql.Tx) error {
	canResolve := func(table, column string) bool {
		refType, err := columnDataType(ctx, tx, table, column)
		if err != nil || refType == "uuid" {
			return false
		}
		return true
	}

	typesMatch := func(table, column, targetTable string) bool {
		refType, _ := columnDataType(ctx, tx, table, column)
		targetType, _ := columnDataType(ctx, tx, targetTable, "id")
		return refType == targetType
	}

	execResolve := func(query string, args ...any) (int64, error) {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		n, _ := result.RowsAffected()
		return n, nil
	}

	// Build NOT EXISTS clauses for union refs (value must not exist in ANY target).
	unionNotExists := func(column string, entities []string) string {
		clauses := make([]string, 0, len(entities))
		for _, entity := range entities {
			target := entityTable(entity)
			if target == "" {
				continue
			}
			clauses = append(clauses, fmt.Sprintf(
				"NOT EXISTS (SELECT 1 FROM %s AS e WHERE e.id = t.%s)",
				quoteIdent(target), quoteIdent(column)))
		}
		return strings.Join(clauses, " AND ")
	}

	const maxPasses = 5
	for pass := 0; pass < maxPasses; pass++ {
		var totalAffected int64

		// Direct references: one entity per column.
		for _, ref := range uuidGroup1References() {
			if !canResolve(ref.table, ref.column) {
				continue
			}
			target := entityTable(ref.entity)
			if target == "" || !typesMatch(ref.table, ref.column, target) {
				continue
			}
			if ref.nullable {
				n, err := execResolve(fmt.Sprintf(
					`UPDATE %s AS t SET %s = ''
					  WHERE t.%s <> ''
					    AND NOT EXISTS (SELECT 1 FROM %s AS e WHERE e.id = t.%s)`,
					quoteIdent(ref.table), quoteIdent(ref.column),
					quoteIdent(ref.column), quoteIdent(target), quoteIdent(ref.column)))
				if err != nil {
					return fmt.Errorf("resolve %s.%s: %w", ref.table, ref.column, err)
				}
				if n > 0 {
					log.Printf("uuid migration: nulled %d orphaned %s refs in %s.%s", n, ref.entity, ref.table, ref.column)
				}
				totalAffected += n
			} else if disposableTables[ref.table] {
				n, err := execResolve(fmt.Sprintf(
					`DELETE FROM %s AS t
					  WHERE NOT EXISTS (SELECT 1 FROM %s AS e WHERE e.id = t.%s)`,
					quoteIdent(ref.table), quoteIdent(target), quoteIdent(ref.column)))
				if err != nil {
					return fmt.Errorf("resolve %s.%s: %w", ref.table, ref.column, err)
				}
				if n > 0 {
					log.Printf("uuid migration: deleted %d orphaned rows from %s (missing %s in %s)", n, ref.table, ref.column, target)
				}
				totalAffected += n
			}
		}

		// Union references: orphaned only if value exists in NONE of the targets.
		for _, ref := range uuidGroup1UnionReferences() {
			if !canResolve(ref.table, ref.column) {
				continue
			}
			notExists := unionNotExists(ref.column, ref.entities)
			if notExists == "" {
				continue
			}
			if ref.nullable {
				n, err := execResolve(fmt.Sprintf(
					`UPDATE %s AS t SET %s = '' WHERE t.%s <> '' AND %s`,
					quoteIdent(ref.table), quoteIdent(ref.column),
					quoteIdent(ref.column), notExists))
				if err != nil {
					return fmt.Errorf("resolve %s.%s: %w", ref.table, ref.column, err)
				}
				if n > 0 {
					log.Printf("uuid migration: nulled %d orphaned union refs in %s.%s", n, ref.table, ref.column)
				}
				totalAffected += n
			} else if disposableTables[ref.table] {
				n, err := execResolve(fmt.Sprintf(
					`DELETE FROM %s AS t WHERE %s`,
					quoteIdent(ref.table), notExists))
				if err != nil {
					return fmt.Errorf("resolve %s.%s: %w", ref.table, ref.column, err)
				}
				if n > 0 {
					log.Printf("uuid migration: deleted %d orphaned rows from %s (unresolved %s)", n, ref.table, ref.column)
				}
				totalAffected += n
			}
		}

		// Polymorphic references: per-entity with type filter.
		for _, ref := range uuidGroup1PolymorphicReferences() {
			if !canResolve(ref.table, ref.column) {
				continue
			}
			for actorType, entity := range ref.typeEntity {
				target := entityTable(entity)
				if target == "" || !typesMatch(ref.table, ref.column, target) {
					continue
				}
				if ref.nullable {
					n, err := execResolve(fmt.Sprintf(
						`UPDATE %s AS t SET %s = ''
						  WHERE t.%s <> ''
						    AND t.%s = $1
						    AND NOT EXISTS (SELECT 1 FROM %s AS e WHERE e.id = t.%s)`,
						quoteIdent(ref.table), quoteIdent(ref.column),
						quoteIdent(ref.column), quoteIdent(ref.typeColumn),
						quoteIdent(target), quoteIdent(ref.column)),
						actorType)
					if err != nil {
						return fmt.Errorf("resolve %s.%s: %w", ref.table, ref.column, err)
					}
					if n > 0 {
						log.Printf("uuid migration: nulled %d orphaned %s/%s refs in %s.%s", n, actorType, entity, ref.table, ref.column)
					}
					totalAffected += n
				} else if disposableTables[ref.table] {
					n, err := execResolve(fmt.Sprintf(
						`DELETE FROM %s AS t
						  WHERE t.%s = $1
						    AND NOT EXISTS (SELECT 1 FROM %s AS e WHERE e.id = t.%s)`,
						quoteIdent(ref.table), quoteIdent(ref.typeColumn),
						quoteIdent(target), quoteIdent(ref.column)),
						actorType)
					if err != nil {
						return fmt.Errorf("resolve %s.%s: %w", ref.table, ref.column, err)
					}
					if n > 0 {
						log.Printf("uuid migration: deleted %d orphaned %s rows from %s (missing %s in %s)", n, actorType, ref.table, ref.column, target)
					}
					totalAffected += n
				}
			}
		}

		if totalAffected == 0 {
			return nil
		}
	}
	return fmt.Errorf("orphaned reference resolution did not converge after %d passes", maxPasses)
}

func rewriteUUIDGroup1References(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup1References() {
		if err := rewriteSingleReference(ctx, tx, ref); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1UnionReferences() {
		if err := rewriteUnionReference(ctx, tx, ref); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1PolymorphicReferences() {
		if err := rewritePolymorphicReference(ctx, tx, ref); err != nil {
			return err
		}
	}
	return nil
}

func rewriteSingleReference(ctx context.Context, tx *sql.Tx, ref uuidReferenceSpec) error {
	dataType, err := columnDataType(ctx, tx, ref.table, ref.column)
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	if ref.nullable {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET %s = '' WHERE %s IS NULL`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column),
		)); err != nil {
			return err
		}
	}
	if err := ensureReferenceMapped(ctx, tx, ref.table, ref.column, []string{ref.entity}, nil); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s AS target
		    SET %s = map.new_id::text
		   FROM uuid_migration_map AS map
		  WHERE map.entity_type = $1
		    AND target.%s = map.old_id`,
		quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column),
	), ref.entity)
	return err
}

func rewriteUnionReference(ctx context.Context, tx *sql.Tx, ref uuidUnionReferenceSpec) error {
	dataType, err := columnDataType(ctx, tx, ref.table, ref.column)
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	if ref.nullable {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET %s = '' WHERE %s IS NULL OR %s IN (%s)`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.column), quoteIdent(ref.column), quotedLiterals(keys(ref.nullText)),
		)); err != nil {
			return err
		}
	}
	if err := ensureReferenceMapped(ctx, tx, ref.table, ref.column, ref.entities, ref.nullText); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s AS target
		    SET %s = map.new_id::text
		   FROM uuid_migration_map AS map
		  WHERE map.entity_type IN (%s)
		    AND target.%s = map.old_id`,
		quoteIdent(ref.table), quoteIdent(ref.column), quotedLiterals(ref.entities), quoteIdent(ref.column),
	))
	return err
}

func rewritePolymorphicReference(ctx context.Context, tx *sql.Tx, ref uuidPolymorphicReferenceSpec) error {
	dataType, err := columnDataType(ctx, tx, ref.table, ref.column)
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	if len(ref.nullTypes) > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET %s = '' WHERE %s IS NULL OR %s IN (%s)`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.typeColumn), quoteIdent(ref.typeColumn), quotedLiterals(keys(ref.nullTypes)),
		)); err != nil {
			return err
		}
	}
	for actorType, entity := range ref.typeEntity {
		if err := ensureTypedReferenceMapped(ctx, tx, ref.table, ref.column, ref.typeColumn, actorType, entity); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s AS target
			    SET %s = map.new_id::text
			   FROM uuid_migration_map AS map
			  WHERE map.entity_type = $1
			    AND target.%s = $2
			    AND target.%s = map.old_id`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.typeColumn), quoteIdent(ref.column),
		), entity, actorType)
		if err != nil {
			return err
		}
	}
	return ensureNoUnknownPolymorphicReferences(ctx, tx, ref)
}

func rewriteUUIDGroup1EntityIDs(ctx context.Context, tx *sql.Tx) error {
	for _, spec := range uuidGroup1Entities() {
		dataType, err := columnDataType(ctx, tx, spec.table, spec.column)
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
			quoteIdent(spec.table), quoteIdent(spec.column), quoteIdent(spec.column),
		), spec.entity)
		if err != nil {
			return err
		}
	}
	return nil
}

func convertUUIDGroup1Columns(ctx context.Context, tx *sql.Tx) error {
	for _, entity := range uuidGroup1Entities() {
		if err := convertColumnToUUID(ctx, tx, entity.table, entity.column, false); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1References() {
		if err := convertColumnToUUID(ctx, tx, ref.table, ref.column, ref.nullable); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1UnionReferences() {
		if err := convertColumnToUUID(ctx, tx, ref.table, ref.column, ref.nullable); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1PolymorphicReferences() {
		if err := convertColumnToUUID(ctx, tx, ref.table, ref.column, ref.nullable); err != nil {
			return err
		}
	}
	return nil
}

func convertColumnToUUID(ctx context.Context, tx *sql.Tx, table, column string, nullable bool) error {
	dataType, err := columnDataType(ctx, tx, table, column)
	if err != nil {
		return err
	}
	if dataType == "uuid" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT`,
		quoteIdent(table), quoteIdent(column),
	)); err != nil {
		return err
	}
	if nullable {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL`,
			quoteIdent(table), quoteIdent(column),
		)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET %s = NULL WHERE %s = ''`,
			quoteIdent(table), quoteIdent(column), quoteIdent(column),
		)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ALTER COLUMN %s TYPE uuid USING NULLIF(%s, '')::uuid`,
			quoteIdent(table), quoteIdent(column), quoteIdent(column),
		)); err != nil {
			return err
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN %s TYPE uuid USING %s::uuid`,
		quoteIdent(table), quoteIdent(column), quoteIdent(column),
	)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN %s SET NOT NULL`,
		quoteIdent(table), quoteIdent(column),
	))
	return err
}

func ensureReferenceMapped(ctx context.Context, tx *sql.Tx, table, column string, entities []string, nullText map[string]struct{}) error {
	query := fmt.Sprintf(
		`SELECT %s::text, COUNT(*)
		   FROM %s AS target
		   LEFT JOIN uuid_migration_map AS map
		     ON map.entity_type IN (%s)
		    AND target.%s = map.old_id
		  WHERE target.%s IS NOT NULL
		    AND target.%s <> ''
		    AND map.old_id IS NULL
		  GROUP BY %s::text
		  ORDER BY %s::text
		  LIMIT 5`,
		quoteIdent(column), quoteIdent(table), quotedLiterals(entities), quoteIdent(column),
		quoteIdent(column), quoteIdent(column), quoteIdent(column), quoteIdent(column),
	)
	rows, err := tx.QueryContext(ctx, query)
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
		if _, ok := nullText[value]; ok {
			continue
		}
		missing = append(missing, fmt.Sprintf("%q (%d)", value, count))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s.%s has unmapped references: %s", table, column, strings.Join(missing, ", "))
	}
	return nil
}

func ensureTypedReferenceMapped(ctx context.Context, tx *sql.Tx, table, column, typeColumn, actorType, entity string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT target.%s::text, COUNT(*)
		   FROM %s AS target
		   LEFT JOIN uuid_migration_map AS map
		     ON map.entity_type = $1
		    AND target.%s = map.old_id
		  WHERE target.%s = $2
		    AND target.%s IS NOT NULL
		    AND target.%s <> ''
		    AND map.old_id IS NULL
		  GROUP BY target.%s::text
		  ORDER BY target.%s::text
		  LIMIT 5`,
		quoteIdent(column), quoteIdent(table), quoteIdent(column), quoteIdent(typeColumn),
		quoteIdent(column), quoteIdent(column), quoteIdent(column), quoteIdent(column),
	), entity, actorType)
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
		return fmt.Errorf("%s.%s has unmapped %s=%s references: %s", table, column, typeColumn, actorType, strings.Join(missing, ", "))
	}
	return nil
}

func ensureNoUnknownPolymorphicReferences(ctx context.Context, tx *sql.Tx, ref uuidPolymorphicReferenceSpec) error {
	known := map[string]struct{}{}
	for actorType := range ref.typeEntity {
		known[actorType] = struct{}{}
	}
	for actorType := range ref.nullTypes {
		known[actorType] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s, COUNT(*)
		   FROM %s
		  WHERE %s IS NOT NULL
		    AND %s <> ''
		  GROUP BY %s
		  ORDER BY %s`,
		quoteIdent(ref.typeColumn), quoteIdent(ref.table), quoteIdent(ref.column),
		quoteIdent(ref.column), quoteIdent(ref.typeColumn), quoteIdent(ref.typeColumn),
	))
	if err != nil {
		return err
	}
	defer rows.Close()
	unknown := []string{}
	for rows.Next() {
		var actorType string
		var count int
		if err := rows.Scan(&actorType, &count); err != nil {
			return err
		}
		if _, ok := known[actorType]; !ok {
			unknown = append(unknown, fmt.Sprintf("%q (%d)", actorType, count))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%s.%s has unknown %s values: %s", ref.table, ref.column, ref.typeColumn, strings.Join(unknown, ", "))
	}
	return nil
}

func verifyDocumentColumnsRemainText(ctx context.Context, tx *sql.Tx) error {
	for _, column := range UUIDGroup1ColumnInventory() {
		if column.Bucket != UUIDGroup1DocumentID {
			continue
		}
		dataType, err := columnDataType(ctx, tx, column.Table, column.Column)
		if err != nil {
			return err
		}
		if dataType != "text" && dataType != "uuid" {
			return fmt.Errorf("%s.%s type = %s, want text before Group 2 or uuid after Group 2", column.Table, column.Column, dataType)
		}
	}
	return nil
}

func verifyNoPrefixedValuesInUUIDColumns(ctx context.Context, tx *sql.Tx) error {
	prefixPattern := regexp.MustCompile(`^(account|account_email_token|ws|user|invite|daemon|agent|run|thread|threadmsg|aevt)_`)
	for _, column := range appendUUIDEntityAndReferenceColumns() {
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
			if prefixPattern.MatchString(value) {
				bad = append(bad, value)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(bad) > 0 {
			return fmt.Errorf("%s.%s still has prefixed UUID values: %s", column.table, column.column, strings.Join(bad, ", "))
		}
	}
	return nil
}

func verifyUUIDGroup1ReferencesResolve(ctx context.Context, tx *sql.Tx) error {
	for _, ref := range uuidGroup1References() {
		if err := verifyReferenceExists(ctx, tx, ref.table, ref.column, ref.entity, ref.nullable); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1UnionReferences() {
		if err := verifyUnionReferenceExists(ctx, tx, ref); err != nil {
			return err
		}
	}
	for _, ref := range uuidGroup1PolymorphicReferences() {
		if err := verifyPolymorphicReferenceExists(ctx, tx, ref); err != nil {
			return err
		}
	}
	return nil
}

func verifyReferenceExists(ctx context.Context, tx *sql.Tx, table, column, entity string, nullable bool) error {
	target := entityTable(entity)
	if target == "" {
		return nil
	}
	condition := "target." + quoteIdent(column) + " IS NOT NULL"
	if !nullable {
		condition = "target." + quoteIdent(column) + " IS NOT NULL"
	}
	var count int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*)
		   FROM %s AS target
		   LEFT JOIN %s AS ref ON ref.id = target.%s
		  WHERE %s
		    AND ref.id IS NULL`,
		quoteIdent(table), quoteIdent(target), quoteIdent(column), condition,
	)).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%s.%s has %d unresolved references to %s", table, column, count, target)
	}
	return nil
}

func verifyUnionReferenceExists(ctx context.Context, tx *sql.Tx, ref uuidUnionReferenceSpec) error {
	joins := []string{}
	nullChecks := []string{}
	for index, entity := range ref.entities {
		alias := fmt.Sprintf("r%d", index)
		joins = append(joins, fmt.Sprintf("LEFT JOIN %s AS %s ON %s.id = target.%s", quoteIdent(entityTable(entity)), alias, alias, quoteIdent(ref.column)))
		nullChecks = append(nullChecks, alias+".id IS NULL")
	}
	var count int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*)
		   FROM %s AS target
		   %s
		  WHERE target.%s IS NOT NULL
		    AND %s`,
		quoteIdent(ref.table), strings.Join(joins, "\n"), quoteIdent(ref.column), strings.Join(nullChecks, " AND "),
	)).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%s.%s has %d unresolved union references", ref.table, ref.column, count)
	}
	return nil
}

func verifyPolymorphicReferenceExists(ctx context.Context, tx *sql.Tx, ref uuidPolymorphicReferenceSpec) error {
	for actorType, entity := range ref.typeEntity {
		target := entityTable(entity)
		var count int
		err := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COUNT(*)
			   FROM %s AS target
			   LEFT JOIN %s AS ref ON ref.id = target.%s
			  WHERE target.%s = $1
			    AND target.%s IS NOT NULL
			    AND ref.id IS NULL`,
			quoteIdent(ref.table), quoteIdent(target), quoteIdent(ref.column),
			quoteIdent(ref.typeColumn), quoteIdent(ref.column),
		), actorType).Scan(&count)
		if err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%s.%s has %d unresolved %s references", ref.table, ref.column, count, actorType)
		}
	}
	return nil
}

func verifyTextColumnInventoryClassified(ctx context.Context, tx *sql.Tx) error {
	classified := map[string]struct{}{}
	for _, column := range UUIDGroup1ColumnInventory() {
		classified[column.Table+"."+column.Column] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT table_name, column_name
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND data_type = 'text'
		   AND table_name IN (
		       'workspaces', 'accounts', 'account_email_tokens', 'workspace_members',
		       'workspace_invites', 'daemons', 'documents', 'document_heads',
		       'document_updates', 'document_checkpoints', 'users', 'agents',
		       'agent_runs', 'threads', 'thread_messages', 'thread_participants',
		       'presences', 'activities', 'agent_events', 'agent_document_views'
		   )
		 ORDER BY table_name, column_name
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	missing := []string{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return err
		}
		key := table + "." + column
		if _, ok := classified[key]; !ok {
			missing = append(missing, key)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("unclassified text columns: %s", strings.Join(missing, ", "))
	}
	return nil
}

func columnDataType(ctx context.Context, tx *sql.Tx, table, column string) (string, error) {
	var dataType string
	err := tx.QueryRowContext(ctx, `
		SELECT data_type
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = $1
		   AND column_name = $2`,
		table, column,
	).Scan(&dataType)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("missing column %s.%s", table, column)
	}
	return dataType, err
}

func stripPrefixedUUID(value, prefix string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("missing prefix %q", prefix)
	}
	suffix := strings.TrimPrefix(value, prefix)
	parsed, err := uuid.Parse(suffix)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func isMigratedWorkspaceID(ctx context.Context, db *sql.DB, workspaceID string) bool {
	workspaceID = strings.TrimSpace(workspaceID)
	if db == nil || workspaceID == "" {
		return false
	}
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
		    SELECT 1
		      FROM information_schema.tables
		     WHERE table_schema = 'public'
		       AND table_name = 'uuid_migration_map'
		)`,
	).Scan(&exists)
	if err != nil || !exists {
		return false
	}
	err = db.QueryRowContext(ctx,
		`SELECT EXISTS (
		    SELECT 1
		      FROM uuid_migration_map
		     WHERE entity_type = 'workspaces'
		       AND old_id = $1
		)`,
		workspaceID,
	).Scan(&exists)
	return err == nil && exists
}

func uuidStringOrNil(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !isUUIDString(value) {
		return nil
	}
	return value
}

func isUUIDString(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

func actorUUIDOrNil(actorID, actorType string) any {
	actorID = strings.TrimSpace(actorID)
	switch strings.TrimSpace(actorType) {
	case "", "system":
		return nil
	default:
		return uuidStringOrNil(actorID)
	}
}

func stringFromNull(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func entityTable(entity string) string {
	for _, spec := range uuidGroup1Entities() {
		if spec.entity == entity {
			return spec.table
		}
	}
	return ""
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quotedLiterals(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+strings.ReplaceAll(value, "'", "''")+"'")
	}
	return strings.Join(quoted, ", ")
}

func setOf(values ...string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func keys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
