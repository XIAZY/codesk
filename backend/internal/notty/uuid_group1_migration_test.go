package notty

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStripPrefixedUUIDKeepsOriginalUUID(t *testing.T) {
	got, err := stripPrefixedUUID("agent_12345678-1234-1234-1234-123456789abc", "agent_")
	if err != nil {
		t.Fatalf("strip prefixed UUID: %v", err)
	}
	if got != "12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("stripped UUID = %q", got)
	}
	if _, err := stripPrefixedUUID("agent_not-a-uuid", "agent_"); err == nil {
		t.Fatal("expected invalid UUID suffix to fail")
	}
	if _, err := stripPrefixedUUID("user_12345678-1234-1234-1234-123456789abc", "agent_"); err == nil {
		t.Fatal("expected missing prefix to fail")
	}
}

func TestUUIDGroup1ColumnInventoryHasNoDuplicateClassifications(t *testing.T) {
	seen := map[string]UUIDGroup1ColumnBucket{}
	for _, column := range UUIDGroup1ColumnInventory() {
		key := column.Table + "." + column.Column
		if previous, ok := seen[key]; ok {
			t.Fatalf("%s classified twice: %s and %s", key, previous, column.Bucket)
		}
		seen[key] = column.Bucket
	}
	for _, key := range []string{
		"documents.id",
		"workspaces.root_document_id",
		"document_updates.actor_id",
		"activities.provenance_requested_by",
		"agent_events.claimed_by",
	} {
		if _, ok := seen[key]; !ok {
			t.Fatalf("expected %s to be classified", key)
		}
	}
}

func TestUUIDGroup1MigrationStripsPrefixesAndLeavesDocumentIDs(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)

	ids := map[string]string{
		"workspace": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"account":   "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"token":     "cccccccc-cccc-cccc-cccc-cccccccccccc",
		"invite":    "dddddddd-dddd-dddd-dddd-dddddddddddd",
		"daemon":    "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		"user":      "11111111-1111-1111-1111-111111111111",
		"agent":     "22222222-2222-2222-2222-222222222222",
		"run":       "33333333-3333-3333-3333-333333333333",
		"thread":    "44444444-4444-4444-4444-444444444444",
		"message":   "55555555-5555-5555-5555-555555555555",
		"event":     "66666666-6666-6666-6666-666666666666",
	}
	old := map[string]string{
		"workspace": "ws_" + ids["workspace"],
		"account":   "account_" + ids["account"],
		"token":     "account_email_token_" + ids["token"],
		"invite":    "invite_" + ids["invite"],
		"daemon":    "daemon_" + ids["daemon"],
		"user":      "user_" + ids["user"],
		"agent":     "agent_" + ids["agent"],
		"run":       "run_" + ids["run"],
		"thread":    "thread_" + ids["thread"],
		"message":   "threadmsg_" + ids["message"],
		"event":     "aevt_" + ids["event"],
	}
	documentID := "doc_77777777-7777-7777-7777-777777777777"
	rootDocumentID := "doc_root_" + old["workspace"]
	updateBytes := []byte{0x01, 0x02, 0x03, 0xff}

	mustExec(t, db, `INSERT INTO workspaces (id, root_document_id) VALUES ($1, $2)`, old["workspace"], rootDocumentID)
	mustExec(t, db, `INSERT INTO accounts (id, last_accessed_workspace_id) VALUES ($1, $2)`, old["account"], old["workspace"])
	mustExec(t, db, `INSERT INTO account_email_tokens (id, account_id) VALUES ($1, $2)`, old["token"], old["account"])
	mustExec(t, db, `INSERT INTO workspace_invites (id, workspace_id, created_by_user_id) VALUES ($1, $2, $3)`, old["invite"], old["workspace"], old["user"])
	mustExec(t, db, `INSERT INTO daemons (id, workspace_id) VALUES ($1, $2)`, old["daemon"], old["workspace"])
	mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title) VALUES ($1, $2, $3, $4)`, old["workspace"], documentID, "docs/spec.md", "Spec")
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector) VALUES ($1, $2, $3)`, old["workspace"], documentID, "sv")
	mustExec(t, db, `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], documentID, updateBytes, "system", "system")
	mustExec(t, db, `INSERT INTO document_checkpoints (workspace_id, document_id, crdt_state, state_vector) VALUES ($1, $2, $3, $4)`, old["workspace"], documentID, "checkpoint", "csv")
	mustExec(t, db, `INSERT INTO users (workspace_id, id) VALUES ($1, $2)`, old["workspace"], old["user"])
	mustExec(t, db, `INSERT INTO agents (workspace_id, id, daemon_id, current_run_id) VALUES ($1, $2, $3, $4)`, old["workspace"], old["agent"], old["daemon"], old["run"])
	mustExec(t, db, `INSERT INTO agent_runs (workspace_id, id, agent_id) VALUES ($1, $2, $3)`, old["workspace"], old["run"], old["agent"])
	mustExec(t, db, `INSERT INTO threads (workspace_id, id, document_id, created_by_id, created_by_type) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], old["thread"], documentID, old["user"], "human")
	mustExec(t, db, `INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], old["message"], old["thread"], old["user"], "human")
	mustExec(t, db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id) VALUES ($1, $2, $3), ($1, $2, $4)`, old["workspace"], old["thread"], old["user"], old["agent"])
	mustExec(t, db, `INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], old["daemon"], "daemon", documentID, "docs/spec.md")
	mustExec(t, db, `INSERT INTO activities (workspace_id, document_id, actor_id, actor_type, provenance_actor_id, provenance_actor_type, provenance_requested_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`, old["workspace"], documentID, old["user"], "human", old["agent"], "agent", "opaque-requester")
	mustExec(t, db, `INSERT INTO agent_events (workspace_id, id, agent_id, document_id, thread_id, thread_message_id, claimed_by, run_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, old["workspace"], old["event"], old["agent"], documentID, old["thread"], old["message"], old["daemon"], old["run"])
	mustExec(t, db, `INSERT INTO agent_document_views (workspace_id, agent_id, document_id, state_vector) VALUES ($1, $2, $3, $4)`, old["workspace"], old["agent"], documentID, "view-sv")
	mustExec(t, db, `INSERT INTO workspace_members (workspace_id, account_id, user_id, invited_by, last_accessed_document_id) VALUES ($1, $2, $3, $3, $4)`, old["workspace"], old["account"], old["user"], documentID)

	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run migration: %v", err)
	}

	assertColumnType(t, db, "workspaces", "id", "uuid")
	assertColumnType(t, db, "documents", "id", "text")
	assertColumnType(t, db, "workspaces", "root_document_id", "text")
	assertScalar(t, db, `SELECT id::text FROM workspaces`, ids["workspace"])
	assertScalar(t, db, `SELECT workspace_id::text FROM documents`, ids["workspace"])
	assertScalar(t, db, `SELECT id FROM documents`, documentID)
	assertScalar(t, db, `SELECT root_document_id FROM workspaces`, rootDocumentID)
	assertScalar(t, db, `SELECT actor_id IS NULL FROM document_updates`, true)
	assertBytes(t, db, `SELECT update FROM document_updates`, updateBytes)

	for entity, oldID := range map[string]string{
		"workspaces":           old["workspace"],
		"accounts":             old["account"],
		"account_email_tokens": old["token"],
		"workspace_invites":    old["invite"],
		"daemons":              old["daemon"],
		"users":                old["user"],
		"agents":               old["agent"],
		"agent_runs":           old["run"],
		"threads":              old["thread"],
		"thread_messages":      old["message"],
		"agent_events":         old["event"],
	} {
		want := strings.TrimPrefix(oldID, prefixForOldID(oldID))
		assertScalar(t, db, `SELECT new_id::text FROM uuid_migration_map WHERE entity_type = $1 AND old_id = $2`, want, entity, oldID)
	}
	if err := VerifyUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("verify migration: %v", err)
	}
	if !isMigratedWorkspaceID(t.Context(), db, old["workspace"]) {
		t.Fatalf("expected old workspace ID %q to be recognized as migrated", old["workspace"])
	}
}

func TestInitPostgresSchemaMigratesLegacyDocumentHeadsIntoCheckpoints(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)

	workspaceUUID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	oldWorkspaceID := "ws_" + workspaceUUID
	documentID := "doc_77777777-7777-7777-7777-777777777777"
	mustExec(t, db, `CREATE TABLE workspaces (id TEXT PRIMARY KEY, root_document_id TEXT NOT NULL DEFAULT '')`)
	mustExec(t, db, `CREATE TABLE document_heads (
		workspace_id TEXT NOT NULL,
		document_id TEXT PRIMARY KEY,
		state_vector TEXT NOT NULL DEFAULT '',
		update_id BIGINT NOT NULL DEFAULT 0,
		crdt_state TEXT NOT NULL DEFAULT '',
		crdt_state_update_id BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL
	)`)
	mustExec(t, db, `INSERT INTO workspaces (id) VALUES ($1)`, oldWorkspaceID)
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, crdt_state, crdt_state_update_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`, oldWorkspaceID, documentID, "sv", int64(0), "checkpoint-state", int64(9))

	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init postgres schema: %v", err)
	}

	assertColumnType(t, db, "document_checkpoints", "workspace_id", "uuid")
	assertScalar(t, db, `SELECT workspace_id::text FROM document_heads`, workspaceUUID)
	assertScalar(t, db, `SELECT workspace_id::text FROM document_checkpoints`, workspaceUUID)
	assertScalar(t, db, `SELECT document_id FROM document_checkpoints`, documentID)
	assertScalar(t, db, `SELECT crdt_state FROM document_checkpoints`, "checkpoint-state")
	assertScalar(t, db, `SELECT root_document_id FROM workspaces`, "doc_root_"+oldWorkspaceID)
}

func createLegacyUUIDGroup1Schema(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, root_document_id TEXT NOT NULL DEFAULT '', slug TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE accounts (id TEXT PRIMARY KEY, last_accessed_workspace_id TEXT, email TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '', password_hash TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE account_email_tokens (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, purpose TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE workspace_members (workspace_id TEXT NOT NULL, account_id TEXT NOT NULL, user_id TEXT NOT NULL, invited_by TEXT, last_accessed_document_id TEXT NOT NULL DEFAULT '', membership_role TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE workspace_invites (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, created_by_user_id TEXT NOT NULL, token_hash TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE daemons (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', daemon_version TEXT NOT NULL DEFAULT '', os TEXT NOT NULL DEFAULT '', arch TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE documents (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, path TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', create_client_operation_id TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE document_heads (workspace_id TEXT NOT NULL, document_id TEXT PRIMARY KEY, state_vector TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE document_updates (workspace_id TEXT NOT NULL, document_id TEXT NOT NULL, update BYTEA NOT NULL, actor_id TEXT, actor_type TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE document_checkpoints (workspace_id TEXT NOT NULL, document_id TEXT NOT NULL, crdt_state TEXT NOT NULL DEFAULT '', state_vector TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE users (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, handle TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agents (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, daemon_id TEXT, current_run_id TEXT, handle TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', system_prompt TEXT NOT NULL DEFAULT '', workspace_root TEXT NOT NULL DEFAULT '', current_turn_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', current_task TEXT NOT NULL DEFAULT '', current_activity TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agent_runs (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, agent_handle TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '', agent_kind TEXT NOT NULL DEFAULT '', system_prompt TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', workspace_root TEXT NOT NULL DEFAULT '', working_dir TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', desired_status TEXT NOT NULL DEFAULT '', last_message TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', assigned_task_ref TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE threads (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, document_id TEXT NOT NULL, created_by_id TEXT NOT NULL, created_by_type TEXT NOT NULL DEFAULT '', client_operation_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', anchor_relative_start TEXT NOT NULL DEFAULT '', anchor_relative_end TEXT NOT NULL DEFAULT '', anchor_kind TEXT NOT NULL DEFAULT '', anchor_excerpt TEXT NOT NULL DEFAULT '', created_by_handle TEXT NOT NULL DEFAULT '', created_by_name TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE thread_messages (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, author_id TEXT NOT NULL, author_type TEXT NOT NULL DEFAULT '', author_handle TEXT NOT NULL DEFAULT '', author_name TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE thread_participants (workspace_id TEXT NOT NULL, thread_id TEXT NOT NULL, participant_id TEXT NOT NULL)`,
		`CREATE TABLE presences (workspace_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_type TEXT NOT NULL DEFAULT '', document_id TEXT NOT NULL DEFAULT '', file_path TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT '', activity TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE activities (workspace_id TEXT NOT NULL, document_id TEXT NOT NULL DEFAULT '', actor_id TEXT, actor_type TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', provenance_actor_id TEXT, provenance_actor_type TEXT NOT NULL DEFAULT '', provenance_execution_id TEXT NOT NULL DEFAULT '', provenance_tool TEXT NOT NULL DEFAULT '', provenance_trigger TEXT NOT NULL DEFAULT '', provenance_confidence TEXT NOT NULL DEFAULT '', provenance_requested_by TEXT NOT NULL DEFAULT '', provenance_source TEXT NOT NULL DEFAULT '', provenance_intended_scope TEXT NOT NULL DEFAULT '', provenance_read_set_summary TEXT NOT NULL DEFAULT '', comment_id TEXT NOT NULL DEFAULT '', presence_ref TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agent_events (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, document_id TEXT NOT NULL DEFAULT '', thread_id TEXT, thread_message_id TEXT, claimed_by TEXT, run_id TEXT, agent_handle TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT '', box TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '', dedup_key TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agent_document_views (workspace_id TEXT NOT NULL, agent_id TEXT NOT NULL, document_id TEXT NOT NULL, state_vector TEXT NOT NULL DEFAULT '')`,
	}
	for _, statement := range statements {
		mustExec(t, db, statement)
	}
}

func resetUUIDGroup1MigrationTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{
		"uuid_migration_map",
		"agent_document_views",
		"agent_events",
		"activities",
		"presences",
		"thread_participants",
		"thread_messages",
		"threads",
		"agent_runs",
		"agents",
		"users",
		"document_checkpoints",
		"document_updates",
		"document_heads",
		"documents",
		"daemons",
		"workspace_invites",
		"workspace_members",
		"account_email_tokens",
		"accounts",
		"workspaces",
	} {
		mustExec(t, db, `DROP TABLE IF EXISTS `+table+` CASCADE`)
	}
}

func assertColumnType(t *testing.T, db *sql.DB, table, column, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`
		SELECT data_type
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = $1
		   AND column_name = $2`,
		table, column,
	).Scan(&got); err != nil {
		t.Fatalf("column type %s.%s: %v", table, column, err)
	}
	if got != want {
		t.Fatalf("%s.%s type = %q, want %q", table, column, got, want)
	}
}

func assertScalar[T comparable](t *testing.T, db *sql.DB, query string, want T, args ...any) {
	t.Helper()
	var got T
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("query scalar %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q = %#v, want %#v", query, got, want)
	}
}

func assertBytes(t *testing.T, db *sql.DB, query string, want []byte, args ...any) {
	t.Helper()
	var got []byte
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("query bytes %q: %v", query, err)
	}
	if string(got) != string(want) {
		t.Fatalf("query %q bytes = %v, want %v", query, got, want)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func prefixForOldID(value string) string {
	for _, prefix := range []string{
		"account_email_token_",
		"threadmsg_",
		"account_",
		"invite_",
		"daemon_",
		"agent_",
		"thread_",
		"aevt_",
		"user_",
		"run_",
		"ws_",
	} {
		if strings.HasPrefix(value, prefix) {
			return prefix
		}
	}
	return ""
}

func TestUUIDStringOrNilRejectsNonUUIDOptionalRefs(t *testing.T) {
	if uuidStringOrNil("") != nil {
		t.Fatal("empty optional UUID should map to nil")
	}
	if uuidStringOrNil("daemon") != nil {
		t.Fatal("legacy claimed_by daemon sentinel should map to nil")
	}
	id := uuid.NewString()
	if got := uuidStringOrNil(id); got != id {
		t.Fatalf("uuidStringOrNil(%q) = %#v", id, got)
	}
}
