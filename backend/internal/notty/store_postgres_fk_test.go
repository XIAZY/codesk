package notty

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPostgresSchemaForeignKeyConstraints verifies that all expected FK
// constraints exist with the correct ON DELETE actions after schema init.
func TestPostgresSchemaForeignKeyConstraints(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	type fkExpectation struct {
		constraintName  string
		table           string
		referencedTable string
		onDelete        string
	}

	expected := []fkExpectation{
		// Workspace ownership CASCADE
		{"fk_workspace_members_workspace", "workspace_members", "workspaces", "CASCADE"},
		{"fk_workspace_invites_workspace", "workspace_invites", "workspaces", "CASCADE"},
		{"fk_daemons_workspace", "daemons", "workspaces", "CASCADE"},
		{"fk_documents_workspace", "documents", "workspaces", "CASCADE"},
		{"fk_document_heads_workspace", "document_heads", "workspaces", "CASCADE"},
		{"fk_document_updates_workspace", "document_updates", "workspaces", "CASCADE"},
		{"fk_document_checkpoints_workspace", "document_checkpoints", "workspaces", "CASCADE"},
		{"fk_document_reverse_window_workspace", "document_reverse_windows", "workspaces", "CASCADE"},
		{"fk_users_workspace", "users", "workspaces", "CASCADE"},
		{"fk_agents_workspace", "agents", "workspaces", "CASCADE"},
		{"fk_agent_runs_workspace", "agent_runs", "workspaces", "CASCADE"},
		{"fk_threads_workspace", "threads", "workspaces", "CASCADE"},
		{"fk_thread_messages_workspace", "thread_messages", "workspaces", "CASCADE"},
		{"fk_thread_participants_workspace", "thread_participants", "workspaces", "CASCADE"},
		{"fk_presences_workspace", "presences", "workspaces", "CASCADE"},
		{"fk_activities_workspace", "activities", "workspaces", "CASCADE"},
		{"fk_agent_events_workspace", "agent_events", "workspaces", "CASCADE"},
		{"fk_agent_document_views_workspace", "agent_document_views", "workspaces", "CASCADE"},

		// Account/auth
		{"fk_account_email_tokens_account", "account_email_tokens", "accounts", "CASCADE"},
		{"fk_accounts_last_workspace", "accounts", "workspaces", "SET NULL"},
		{"fk_workspace_members_account", "workspace_members", "accounts", "CASCADE"},

		// Membership/invite
		{"fk_workspace_members_user", "workspace_members", "users", "CASCADE"},
		{"fk_workspace_members_invited_by", "workspace_members", "users", "SET NULL"},
		{"fk_workspace_invites_created_by", "workspace_invites", "users", "CASCADE"},

		// Daemon/agent/run
		{"fk_agents_daemon", "agents", "daemons", "SET NULL"},
		{"fk_agent_runs_agent", "agent_runs", "agents", "CASCADE"},
		{"fk_agents_current_run", "agents", "agent_runs", "SET NULL"},
		{"fk_agent_events_agent", "agent_events", "agents", "CASCADE"},
		{"fk_agent_events_run", "agent_events", "agent_runs", "SET NULL"},
		{"fk_agent_document_views_agent", "agent_document_views", "agents", "CASCADE"},

		// Documents (composite same-workspace enforcement for CASCADE,
		// simple FK for SET NULL to avoid nulling workspace_id)
		{"fk_document_heads_document", "document_heads", "documents", "CASCADE"},
		{"fk_document_updates_document", "document_updates", "documents", "CASCADE"},
		{"fk_document_checkpoints_document", "document_checkpoints", "documents", "CASCADE"},
		{"fk_threads_document", "threads", "documents", "CASCADE"},
		{"fk_presences_document", "presences", "documents", "CASCADE"},
		{"fk_agent_document_views_document", "agent_document_views", "documents", "CASCADE"},
		{"fk_document_reverse_window_document", "document_reverse_windows", "documents", "CASCADE"},
		{"fk_document_reverse_window_root", "document_reverse_windows", "documents", "CASCADE"},
		{"fk_document_reverse_window_daemon", "document_reverse_windows", "daemons", "CASCADE"},
		{"fk_workspace_members_last_doc", "workspace_members", "documents", "SET NULL"},
		{"fk_activities_document", "activities", "documents", "SET NULL"},
		{"fk_agent_events_document", "agent_events", "documents", "SET NULL"},

		// Threads/messages
		{"fk_thread_messages_thread", "thread_messages", "threads", "CASCADE"},
		{"fk_thread_participants_thread", "thread_participants", "threads", "CASCADE"},
		{"fk_agent_events_thread", "agent_events", "threads", "SET NULL"},
		{"fk_agent_events_thread_message", "agent_events", "thread_messages", "SET NULL"},
	}

	// Query all FK constraints from information_schema.
	rows, err := db.Query(`
		SELECT
			tc.constraint_name,
			tc.table_name,
			ccu.table_name AS referenced_table,
			rc.delete_rule
		FROM information_schema.table_constraints tc
		JOIN information_schema.referential_constraints rc
			ON rc.constraint_name = tc.constraint_name
			AND rc.constraint_schema = tc.constraint_schema
		JOIN information_schema.constraint_column_usage ccu
			ON ccu.constraint_name = tc.constraint_name
			AND ccu.constraint_schema = tc.constraint_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
			AND tc.table_schema = 'public'
		ORDER BY tc.constraint_name`)
	if err != nil {
		t.Fatalf("query FK constraints: %v", err)
	}
	defer rows.Close()

	type foundFK struct {
		table           string
		referencedTable string
		onDelete        string
	}
	found := map[string]foundFK{}
	for rows.Next() {
		var name, table, refTable, deleteRule string
		if err := rows.Scan(&name, &table, &refTable, &deleteRule); err != nil {
			t.Fatalf("scan FK row: %v", err)
		}
		found[name] = foundFK{table: table, referencedTable: refTable, onDelete: deleteRule}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate FK rows: %v", err)
	}

	for _, exp := range expected {
		fk, ok := found[exp.constraintName]
		if !ok {
			t.Errorf("missing FK constraint %s", exp.constraintName)
			continue
		}
		if fk.table != exp.table {
			t.Errorf("FK %s: table = %q, want %q", exp.constraintName, fk.table, exp.table)
		}
		if fk.referencedTable != exp.referencedTable {
			t.Errorf("FK %s: referenced_table = %q, want %q", exp.constraintName, fk.referencedTable, exp.referencedTable)
		}
		if fk.onDelete != exp.onDelete {
			t.Errorf("FK %s: on_delete = %q, want %q", exp.constraintName, fk.onDelete, exp.onDelete)
		}
	}
}

func TestPostgresSchemaInitIsIdempotentOnNativeDatabase(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("first repeated schema init: %v", err)
	}
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("second repeated schema init: %v", err)
	}
}

func TestPostgresSchemaOmitsLegacyDocumentPathTitleColumns(t *testing.T) {
	database := newPostgresTestDatabase(t)

	assertNoDocumentPathTitleColumns(t, database.DB)
}

func TestPostgresSchemaForeignKeyConstraintsAreValidated(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	rows, err := db.Query(`
		SELECT conname
		  FROM pg_constraint
		 WHERE connamespace = 'public'::regnamespace
		   AND contype = 'f'
		   AND NOT convalidated
		 ORDER BY conname`)
	if err != nil {
		t.Fatalf("query unvalidated FK constraints: %v", err)
	}
	defer rows.Close()

	var unvalidated []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan unvalidated FK constraint: %v", err)
		}
		unvalidated = append(unvalidated, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate unvalidated FK constraints: %v", err)
	}
	if len(unvalidated) != 0 {
		t.Fatalf("found unvalidated FK constraints: %v", unvalidated)
	}
}

// fkTestFixture holds the IDs and database for a single FK behavior test.
type fkTestFixture struct {
	db          *sql.DB
	workspaceID string
	rootDocID   string
	now         time.Time
}

type fkActorIDs struct {
	userID   string
	daemonID string
	agentID  string
}

// seedWorkspaceRowFK inserts a workspace and its root document row in one
// transaction, satisfying the deferred fk_workspaces_root_document constraint
// (a workspace can never exist without its root document row).
func seedWorkspaceRowFK(t *testing.T, db *sql.DB, workspaceID, slug, name, rootDocID string, now time.Time) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin workspace seed: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, slug, name, rootDocID, now, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO documents (workspace_id, id, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, TRUE, 1000, $3)`,
		workspaceID, rootDocID, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert root document: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit workspace seed: %v", err)
	}
}

func newFKTestFixture(t *testing.T) *fkTestFixture {
	t.Helper()
	database := newPostgresTestDatabase(t)
	db := database.DB
	now := time.Now().UTC()
	workspaceID := uuid.NewString()
	rootDocID := uuid.NewString()

	seedWorkspaceRowFK(t, db, workspaceID, "fk-test-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8], "FK Test", rootDocID, now)

	return &fkTestFixture{db: db, workspaceID: workspaceID, rootDocID: rootDocID, now: now}
}

func (f *fkTestFixture) insertDocument(t *testing.T, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO documents (workspace_id, id, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, FALSE, 1001, $3)`,
		f.workspaceID, docID, f.now)
}

func (f *fkTestFixture) insertDocumentHead(t *testing.T, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
		VALUES ($1, $2, '', 0, $3)`,
		f.workspaceID, docID, f.now)
}

func (f *fkTestFixture) insertDocumentUpdate(t *testing.T, docID string, actorID *string, actorType string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		f.workspaceID, docID, []byte{0, 0}, actorID, actorType, f.now)
}

func (f *fkTestFixture) insertDocumentCheckpoint(t *testing.T, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
		VALUES ($1, $2, 0, '', '', $3)`,
		f.workspaceID, docID, f.now)
}

func (f *fkTestFixture) insertDocumentReverseWindow(t *testing.T, docID, daemonID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO document_reverse_windows (
		document_id, workspace_id, root_document_id, entry_id, desired_path,
		origin_daemon_id, origin_scope, tombstone_operation_id,
		tombstone_request_fingerprint, opened_at, reverse_until, created_at, updated_at
	) VALUES ($1, $2, $3, $4, 'docs/a.md', $5, 'primary', $6,
		'fingerprint', $7, $8, $7, $7)`,
		docID, f.workspaceID, f.rootDocID, docID, daemonID, uuid.NewString(), f.now, f.now.Add(time.Minute))
}

func (f *fkTestFixture) insertUser(t *testing.T, userID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'member', 'human', 'active', $5, $6)`,
		f.workspaceID, userID, "user-"+userID[:8], "Test User", f.now, f.now)
}

func (f *fkTestFixture) insertAccount(t *testing.T, accountID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO accounts (id, email, display_name, password_hash, created_at, updated_at)
		VALUES ($1, $2, 'Test', 'hash', $3, $4)`,
		accountID, "test-"+accountID[:8]+"@example.com", f.now, f.now)
}

func (f *fkTestFixture) insertDaemon(t *testing.T, daemonID string) {
	t.Helper()
	runtimes, _ := json.Marshal([]RuntimeDetection{{Kind: "codex", Available: true}})
	mustExecFK(t, f.db, `INSERT INTO daemons (id, workspace_id, name, token_hash, status, runtime_detections, created_at)
		VALUES ($1, $2, 'Test Daemon', $3, 'active', $4, $5)`,
		daemonID, f.workspaceID, "token_"+daemonID[:8], runtimes, f.now)
}

func (f *fkTestFixture) insertAgent(t *testing.T, agentID, daemonID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO agents (workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
		current_turn_id, session_id, status, current_task, current_activity, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'assistant', 'codex', '', '', '', '', 'idle', '', '', $6)`,
		f.workspaceID, agentID, daemonID, "agent-"+agentID[:8], "Test Agent", f.now)
}

func (f *fkTestFixture) insertActors(t *testing.T) fkActorIDs {
	t.Helper()
	actors := fkActorIDs{
		userID:   uuid.NewString(),
		daemonID: uuid.NewString(),
		agentID:  uuid.NewString(),
	}
	f.insertUser(t, actors.userID)
	f.insertDaemon(t, actors.daemonID)
	f.insertAgent(t, actors.agentID, actors.daemonID)
	return actors
}

func (f *fkTestFixture) insertForeignActors(t *testing.T) fkActorIDs {
	t.Helper()
	wsID := uuid.NewString()
	rootDocID := uuid.NewString()
	seedWorkspaceRowFK(t, f.db, wsID, "foreign-"+wsID[:8], "Foreign Workspace", rootDocID, f.now)

	actors := fkActorIDs{
		userID:   uuid.NewString(),
		daemonID: uuid.NewString(),
		agentID:  uuid.NewString(),
	}
	mustExecFK(t, f.db, `INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'member', 'human', 'active', $5, $6)`,
		wsID, actors.userID, "user-"+actors.userID[:8], "Foreign User", f.now, f.now)
	runtimes, _ := json.Marshal([]RuntimeDetection{{Kind: "codex", Available: true}})
	mustExecFK(t, f.db, `INSERT INTO daemons (id, workspace_id, name, token_hash, status, runtime_detections, created_at)
		VALUES ($1, $2, 'Foreign Daemon', $3, 'active', $4, $5)`,
		actors.daemonID, wsID, "token_"+actors.daemonID[:8], runtimes, f.now)
	mustExecFK(t, f.db, `INSERT INTO agents (workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
		current_turn_id, session_id, status, current_task, current_activity, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'assistant', 'codex', '', '', '', '', 'idle', '', '', $6)`,
		wsID, actors.agentID, actors.daemonID, "agent-"+actors.agentID[:8], "Foreign Agent", f.now)
	return actors
}

func (f *fkTestFixture) insertAgentRun(t *testing.T, runID, agentID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO agent_runs (workspace_id, id, agent_id, agent_handle, agent_name, agent_kind,
		system_prompt, workspace_root, working_dir, prompt, status, desired_status,
		last_message, log_tail, error, assigned_task_ref, updated_at)
		VALUES ($1, $2, $3, $4, 'Agent', 'codex', '', '', '.', 'test prompt', 'running', 'running',
		'', '[]'::jsonb, '', '', $5)`,
		f.workspaceID, runID, agentID, "agent-"+agentID[:8], f.now)
}

func (f *fkTestFixture) insertThread(t *testing.T, threadID, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
		created_by_type, created_by_handle, created_by_name, created_at, updated_at)
		VALUES ($1, $2, $3, 'Test Thread', 'open', 'system', '', '', $4, $5)`,
		f.workspaceID, threadID, docID, f.now, f.now)
}

func (f *fkTestFixture) insertThreadMessage(t *testing.T, msgID, threadID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO thread_messages (workspace_id, id, thread_id, author_type,
		author_handle, author_name, body, kind, created_at)
		VALUES ($1, $2, $3, 'system', '', '', 'test body', 'message', $4)`,
		f.workspaceID, msgID, threadID, f.now)
}

func (f *fkTestFixture) insertThreadMessageByUser(t *testing.T, msgID, threadID, userID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type,
		author_handle, author_name, body, kind, created_at)
		VALUES ($1, $2, $3, $4, 'human', $5, 'Test User', 'test body', 'message', $6)`,
		f.workspaceID, msgID, threadID, userID, "user-"+userID[:8], f.now)
}

func (f *fkTestFixture) insertThreadMessageByAgent(t *testing.T, msgID, threadID, agentID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type,
		author_handle, author_name, body, kind, created_at)
		VALUES ($1, $2, $3, $4, 'agent', $5, 'Test Agent', 'test body', 'message', $6)`,
		f.workspaceID, msgID, threadID, agentID, "agent-"+agentID[:8], f.now)
}

func (f *fkTestFixture) insertPresence(t *testing.T, actorID, actorType, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
		VALUES ($1, $2, $3, $4, '', 'editing', '', $5)`,
		f.workspaceID, actorID, actorType, docID, f.now)
}

func (f *fkTestFixture) insertActivity(t *testing.T, actorID *string, actorType, docID string) int64 {
	t.Helper()
	var id int64
	err := f.db.QueryRow(`INSERT INTO activities (workspace_id, type, document_id, actor_id, actor_type, summary, occurred_at,
		provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
		provenance_autonomous, provenance_confidence, provenance_requested_by,
		provenance_source, provenance_intended_scope, provenance_read_set_summary,
		presence_ref)
		VALUES ($1, 'test', $2, $3, $4, 'test', $5,
		'', '', '', '', FALSE, '', '', '', '', '', '')
		RETURNING id`,
		f.workspaceID, uuidStringOrNil(docID), actorID, actorType, f.now).Scan(&id)
	if err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	return id
}

func (f *fkTestFixture) insertActivityProvenance(t *testing.T, actorID string, actorType, docID string) int64 {
	t.Helper()
	var id int64
	err := f.db.QueryRow(`INSERT INTO activities (workspace_id, type, document_id, actor_type, summary, occurred_at,
		provenance_actor_id, provenance_actor_type,
		provenance_execution_id, provenance_tool, provenance_trigger,
		provenance_autonomous, provenance_confidence, provenance_requested_by,
		provenance_source, provenance_intended_scope, provenance_read_set_summary,
		presence_ref)
		VALUES ($1, 'test', $2, 'system', '', $3,
		$4, $5,
		'', '', '', FALSE, '', '', '', '', '', '')
		RETURNING id`,
		f.workspaceID, docID, f.now, actorID, actorType).Scan(&id)
	if err != nil {
		t.Fatalf("insert activity provenance: %v", err)
	}
	return id
}

func (f *fkTestFixture) insertAgentEvent(t *testing.T, eventID, agentID string, docID, threadID, threadMsgID, runID *string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
		document_id, thread_id, thread_message_id, summary, prompt, dedup_key,
		run_id, last_error, attempt_count, available_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'test', 'pending', $5, $6, $7, '', '', '', $8, '', 0, $9, $10, $11)`,
		f.workspaceID, eventID, agentID, "agent-"+agentID[:8],
		docID, threadID, threadMsgID, runID, f.now, f.now, f.now)
}

func (f *fkTestFixture) insertAgentDocumentView(t *testing.T, agentID, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector, viewed_at)
		VALUES ($1, $2, $3, 0, '', $4)`,
		f.workspaceID, agentID, docID, f.now)
}

func (f *fkTestFixture) insertThreadParticipant(t *testing.T, threadID, participantID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		VALUES ($1, $2, $3)`,
		f.workspaceID, threadID, participantID)
}

func (f *fkTestFixture) insertWorkspaceMember(t *testing.T, accountID, userID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO workspace_members (workspace_id, account_id, user_id, membership_role, status, created_at)
		VALUES ($1, $2, $3, 'member', 'active', $4)`,
		f.workspaceID, accountID, userID, f.now)
}

func (f *fkTestFixture) insertWorkspaceInvite(t *testing.T, inviteID, userID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO workspace_invites (id, workspace_id, token_hash, created_by_user_id, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		inviteID, f.workspaceID, "hash_"+inviteID[:8], userID, f.now.Add(24*time.Hour), f.now)
}

func (f *fkTestFixture) countRows(t *testing.T, table string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE workspace_id = $1`, table), f.workspaceID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func (f *fkTestFixture) countRowsNoWS(t *testing.T, table, whereColumn, whereValue string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1`, table, whereColumn), whereValue).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func (f *fkTestFixture) activityDocumentID(t *testing.T, activityID int64) *string {
	t.Helper()
	var docID *string
	if err := f.db.QueryRow(`SELECT document_id::text FROM activities WHERE id = $1`, activityID).Scan(&docID); err != nil {
		t.Fatalf("query activity document_id: %v", err)
	}
	return docID
}

func (f *fkTestFixture) agentEventDocumentID(t *testing.T, eventID string) *string {
	t.Helper()
	var docID *string
	if err := f.db.QueryRow(`SELECT document_id::text FROM agent_events WHERE id = $1`, eventID).Scan(&docID); err != nil {
		t.Fatalf("query agent_event document_id: %v", err)
	}
	return docID
}

func (f *fkTestFixture) threadCreatedByID(t *testing.T, threadID string) *string {
	t.Helper()
	var id *string
	if err := f.db.QueryRow(`SELECT created_by_id::text FROM threads WHERE id = $1`, threadID).Scan(&id); err != nil {
		t.Fatalf("query thread created_by_id: %v", err)
	}
	return id
}

func (f *fkTestFixture) threadMessageAuthorID(t *testing.T, msgID string) *string {
	t.Helper()
	var id *string
	if err := f.db.QueryRow(`SELECT author_id::text FROM thread_messages WHERE id = $1`, msgID).Scan(&id); err != nil {
		t.Fatalf("query thread_message author_id: %v", err)
	}
	return id
}

func mustExecFK(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v\nquery: %s\nargs: %v", err, query, args)
	}
}

func expectExecErrorFK(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err == nil {
		t.Fatalf("expected exec error\nquery: %s\nargs: %v", query, args)
	}
}

func countMatchingFK(t *testing.T, db *sql.DB, table, where string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, table, where), args...).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}

func strPtr(s string) *string { return &s }

// TestWorkspaceDeleteCascadesAllChildren creates a workspace with documents,
// threads, messages, users, agents, etc., deletes the workspace, and asserts
// all children are gone.
func TestWorkspaceDeleteCascadesAllChildren(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	userID := uuid.NewString()
	daemonID := uuid.NewString()
	agentID := uuid.NewString()
	runID := uuid.NewString()
	threadID := uuid.NewString()
	msgID := uuid.NewString()
	eventID := uuid.NewString()
	accountID := uuid.NewString()

	f.insertDocument(t, docID)
	f.insertDocumentHead(t, docID)
	f.insertDocumentUpdate(t, docID, nil, "system")
	f.insertDocumentCheckpoint(t, docID)
	f.insertUser(t, userID)
	f.insertDaemon(t, daemonID)
	f.insertDocumentReverseWindow(t, docID, daemonID)
	f.insertAgent(t, agentID, daemonID)
	f.insertAgentRun(t, runID, agentID)
	f.insertThread(t, threadID, docID)
	f.insertThreadMessage(t, msgID, threadID)
	f.insertThreadParticipant(t, threadID, userID)
	f.insertPresence(t, userID, "human", docID)
	f.insertActivity(t, strPtr(userID), "human", docID)
	f.insertAgentEvent(t, eventID, agentID, strPtr(docID), strPtr(threadID), strPtr(msgID), strPtr(runID))
	f.insertAgentDocumentView(t, agentID, docID)

	f.insertAccount(t, accountID)
	f.insertWorkspaceMember(t, accountID, userID)

	// Verify setup.
	if c := f.countRows(t, "documents"); c < 2 { // root + content doc
		t.Fatalf("expected at least 2 documents, got %d", c)
	}

	// Delete the workspace.
	mustExecFK(t, f.db, `DELETE FROM workspaces WHERE id = $1`, f.workspaceID)

	// Assert all children are gone.
	for _, table := range []string{
		"documents", "document_heads", "document_updates", "document_checkpoints", "document_reverse_windows",
		"users", "daemons", "agents", "agent_runs",
		"threads", "thread_messages", "thread_participants",
		"presences", "activities", "agent_events", "agent_document_views",
		"workspace_members", "workspace_invites",
	} {
		if c := f.countRows(t, table); c != 0 {
			t.Errorf("%s: expected 0 rows after workspace delete, got %d", table, c)
		}
	}
}

// TestDocumentDeleteCascadesStorageAndNullsAttribution creates a document
// with heads, updates, checkpoints, threads, presences, and views. Deletes
// the document and asserts storage/threads/presences/views are gone while
// activities and events have document_id nulled.
func TestDocumentDeleteCascadesStorageAndNullsAttribution(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	userID := uuid.NewString()
	daemonID := uuid.NewString()
	agentID := uuid.NewString()
	threadID := uuid.NewString()
	msgID := uuid.NewString()
	eventID := uuid.NewString()

	f.insertDocument(t, docID)
	f.insertDocumentHead(t, docID)
	f.insertDocumentUpdate(t, docID, nil, "system")
	f.insertDocumentCheckpoint(t, docID)
	f.insertUser(t, userID)
	f.insertDaemon(t, daemonID)
	f.insertDocumentReverseWindow(t, docID, daemonID)
	f.insertAgent(t, agentID, daemonID)
	f.insertThread(t, threadID, docID)
	f.insertThreadMessage(t, msgID, threadID)
	f.insertPresence(t, userID, "human", docID)
	f.insertAgentDocumentView(t, agentID, docID)

	activityID := f.insertActivity(t, strPtr(userID), "human", docID)
	f.insertAgentEvent(t, eventID, agentID, strPtr(docID), strPtr(threadID), strPtr(msgID), nil)

	// Delete the document.
	mustExecFK(t, f.db, `DELETE FROM documents WHERE id = $1`, docID)

	// Storage should be gone.
	if c := f.countRowsNoWS(t, "document_heads", "document_id", docID); c != 0 {
		t.Errorf("document_heads: expected 0 after doc delete, got %d", c)
	}
	if c := f.countRowsNoWS(t, "document_reverse_windows", "document_id", docID); c != 0 {
		t.Errorf("document_reverse_windows: expected 0 after doc delete, got %d", c)
	}

	// Threads should be gone (CASCADE via document).
	if c := f.countRowsNoWS(t, "threads", "id", threadID); c != 0 {
		t.Errorf("threads: expected 0 after doc delete, got %d", c)
	}
	if c := f.countRowsNoWS(t, "thread_messages", "id", msgID); c != 0 {
		t.Errorf("thread_messages: expected 0 after doc delete, got %d", c)
	}

	// Presences should be gone (CASCADE via document).
	if c := f.countRows(t, "presences"); c != 0 {
		t.Errorf("presences: expected 0 after doc delete, got %d", c)
	}

	// Agent document views should be gone (CASCADE via document).
	if c := f.countRows(t, "agent_document_views"); c != 0 {
		t.Errorf("agent_document_views: expected 0 after doc delete, got %d", c)
	}

	// Activity document_id should be nulled (SET NULL).
	if docRef := f.activityDocumentID(t, activityID); docRef != nil {
		t.Errorf("activity document_id should be NULL after doc delete, got %q", *docRef)
	}

	// Agent event document_id should be nulled (SET NULL).
	if docRef := f.agentEventDocumentID(t, eventID); docRef != nil {
		t.Errorf("agent_event document_id should be NULL after doc delete, got %q", *docRef)
	}
}

// TestAgentDeleteCascadesRunsAndEventsAndCleansParticipants creates an agent
// with runs, events, document views, and thread participation. Deletes the
// agent and asserts runs/events/views are gone, participants deleted, and
// thread attribution nulled.
func TestAgentDeleteCascadesRunsAndEventsAndCleansParticipants(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	daemonID := uuid.NewString()
	agentID := uuid.NewString()
	runID := uuid.NewString()
	threadID := uuid.NewString()
	msgID := uuid.NewString()
	eventID := uuid.NewString()

	f.insertDocument(t, docID)
	f.insertDaemon(t, daemonID)
	f.insertAgent(t, agentID, daemonID)
	f.insertAgentRun(t, runID, agentID)
	f.insertThread(t, threadID, docID)
	f.insertThreadMessageByAgent(t, msgID, threadID, agentID)
	f.insertThreadParticipant(t, threadID, agentID)
	f.insertPresence(t, agentID, "agent", docID)
	f.insertAgentDocumentView(t, agentID, docID)
	f.insertAgentEvent(t, eventID, agentID, strPtr(docID), strPtr(threadID), strPtr(msgID), strPtr(runID))

	// Delete the agent.
	mustExecFK(t, f.db, `DELETE FROM agents WHERE id = $1`, agentID)

	// Runs should be gone (CASCADE via agent).
	if c := f.countRowsNoWS(t, "agent_runs", "id", runID); c != 0 {
		t.Errorf("agent_runs: expected 0 after agent delete, got %d", c)
	}

	// Events should be gone (CASCADE via agent).
	if c := f.countRowsNoWS(t, "agent_events", "id", eventID); c != 0 {
		t.Errorf("agent_events: expected 0 after agent delete, got %d", c)
	}

	// Document views should be gone (CASCADE via agent).
	if c := f.countRows(t, "agent_document_views"); c != 0 {
		t.Errorf("agent_document_views: expected 0 after agent delete, got %d", c)
	}

	// Participants should be deleted by trigger.
	if c := f.countRows(t, "thread_participants"); c != 0 {
		t.Errorf("thread_participants: expected 0 after agent delete, got %d", c)
	}

	// Presences should be deleted by trigger.
	if c := f.countRows(t, "presences"); c != 0 {
		t.Errorf("presences: expected 0 after agent delete, got %d", c)
	}

	// Thread message author_id should be nulled by trigger.
	if authorID := f.threadMessageAuthorID(t, msgID); authorID != nil {
		t.Errorf("thread_message author_id should be NULL after agent delete, got %q", *authorID)
	}
}

// TestUserDeleteNullsAttributionAndDeletesMembership creates a user with
// thread authorship, membership, and participation. Deletes the user and
// asserts attribution is nulled and membership/participation are deleted.
func TestUserDeleteNullsAttributionAndDeletesMembership(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	userID := uuid.NewString()
	accountID := uuid.NewString()
	threadID := uuid.NewString()
	msgID := uuid.NewString()
	inviteID := uuid.NewString()

	f.insertDocument(t, docID)
	f.insertUser(t, userID)
	f.insertAccount(t, accountID)
	f.insertWorkspaceMember(t, accountID, userID)
	f.insertWorkspaceInvite(t, inviteID, userID)

	// Create thread authored by user.
	mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
		created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
		VALUES ($1, $2, $3, 'User Thread', 'open', $4, 'human', $5, 'Test User', $6, $7)`,
		f.workspaceID, threadID, docID, userID, "user-"+userID[:8], f.now, f.now)

	f.insertThreadMessageByUser(t, msgID, threadID, userID)
	f.insertThreadParticipant(t, threadID, userID)
	f.insertPresence(t, userID, "human", docID)

	// Verify setup.
	if c := f.countRows(t, "workspace_members"); c != 1 {
		t.Fatalf("expected 1 workspace_member, got %d", c)
	}

	// Delete the user.
	mustExecFK(t, f.db, `DELETE FROM users WHERE id = $1`, userID)

	// Workspace members should be deleted (CASCADE on fk_workspace_members_user).
	if c := f.countRows(t, "workspace_members"); c != 0 {
		t.Errorf("workspace_members: expected 0 after user delete, got %d", c)
	}

	// Workspace invites should be deleted (CASCADE on fk_workspace_invites_created_by).
	if c := f.countRows(t, "workspace_invites"); c != 0 {
		t.Errorf("workspace_invites: expected 0 after user delete, got %d", c)
	}

	// Thread participants should be deleted by trigger.
	if c := f.countRows(t, "thread_participants"); c != 0 {
		t.Errorf("thread_participants: expected 0 after user delete, got %d", c)
	}

	// Presences should be deleted by trigger.
	if c := f.countRows(t, "presences"); c != 0 {
		t.Errorf("presences: expected 0 after user delete, got %d", c)
	}

	// Thread created_by_id should be nulled by trigger.
	if createdBy := f.threadCreatedByID(t, threadID); createdBy != nil {
		t.Errorf("thread created_by_id should be NULL after user delete, got %q", *createdBy)
	}

	// Thread message author_id should be nulled by trigger.
	if authorID := f.threadMessageAuthorID(t, msgID); authorID != nil {
		t.Errorf("thread_message author_id should be NULL after user delete, got %q", *authorID)
	}
}

// TestCompositeUniqueIndexesExist verifies the composite unique indexes
// for same-workspace enforcement.
func TestCompositeUniqueIndexesExist(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	expectedIndexes := []string{
		"uq_documents_workspace_id",
		"uq_users_workspace_id",
		"uq_agents_workspace_id",
		"uq_daemons_workspace_id",
		"uq_threads_workspace_id",
		"uq_thread_messages_workspace_id",
		"uq_agent_runs_workspace_id",
		"uq_document_reverse_window_restore_operation",
		"uq_document_reverse_window_tombstone_operation",
	}

	rows, err := db.Query(`
		SELECT indexname
		FROM pg_indexes
		WHERE schemaname = 'public'
		  AND indexname LIKE 'uq_%'
		ORDER BY indexname`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}

	sort.Strings(found)
	sort.Strings(expectedIndexes)

	if len(found) != len(expectedIndexes) {
		t.Fatalf("expected %d composite unique indexes, got %d: %v", len(expectedIndexes), len(found), found)
	}
	for i, name := range expectedIndexes {
		if found[i] != name {
			t.Errorf("index %d: got %q, want %q", i, found[i], name)
		}
	}
}

// TestConstraintTriggersExist verifies that the polymorphic constraint
// triggers and cleanup triggers are installed.
func TestConstraintTriggersExist(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	expectedTriggers := []struct {
		table   string
		trigger string
	}{
		{"document_updates", "trg_document_updates_actor_ref"},
		{"presences", "trg_presences_actor_ref"},
		{"activities", "trg_activities_actor_ref"},
		{"activities", "trg_activities_provenance_ref"},
		{"threads", "trg_threads_author_ref"},
		{"thread_messages", "trg_thread_messages_author_ref"},
		{"thread_participants", "trg_thread_participants_ref"},
		{"agent_events", "trg_agent_events_claimed_by"},
		{"users", "trg_user_delete_cleanup"},
		{"agents", "trg_agent_delete_cleanup"},
		{"daemons", "trg_daemon_delete_cleanup"},
	}

	for _, exp := range expectedTriggers {
		var count int
		err := db.QueryRow(`
			SELECT COUNT(*)
			FROM information_schema.triggers
			WHERE trigger_schema = 'public'
			  AND event_object_table = $1
			  AND trigger_name = $2`,
			exp.table, exp.trigger).Scan(&count)
		if err != nil {
			t.Fatalf("query trigger %s on %s: %v", exp.trigger, exp.table, err)
		}
		if count == 0 {
			t.Errorf("missing trigger %s on table %s", exp.trigger, exp.table)
		}
	}
}

func TestFKConstraintsRejectCrossWorkspaceRefs(t *testing.T) {
	f := newFKTestFixture(t)

	// Create workspace B with a full entity graph.
	wsB := uuid.NewString()
	rootDocB := uuid.NewString()
	seedWorkspaceRowFK(t, f.db, wsB, "ws-b-"+wsB[:8], "Workspace B", rootDocB, f.now)
	docB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO documents (workspace_id, id, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, FALSE, 1001, $3)`,
		wsB, docB, f.now)
	userB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'member', 'human', 'active', $5, $6)`,
		wsB, userB, "user-b", "User B", f.now, f.now)
	daemonB := uuid.NewString()
	runtimesB, _ := json.Marshal([]RuntimeDetection{{Kind: "codex", Available: true}})
	mustExecFK(t, f.db, `INSERT INTO daemons (id, workspace_id, name, token_hash, status, runtime_detections, created_at)
		VALUES ($1, $2, 'Daemon B', $3, 'active', $4, $5)`,
		daemonB, wsB, "token_b", runtimesB, f.now)
	agentB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO agents (workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
		current_turn_id, session_id, status, current_task, current_activity, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'assistant', 'codex', '', '', '', '', 'idle', '', '', $6)`,
		wsB, agentB, daemonB, "agent-b", "Agent B", f.now)
	runB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO agent_runs (workspace_id, id, agent_id, agent_handle, agent_name, agent_kind,
		system_prompt, workspace_root, working_dir, prompt, status, desired_status,
		last_message, log_tail, error, assigned_task_ref, updated_at)
		VALUES ($1, $2, $3, $4, 'Agent B', 'codex', '', '', '.', 'test', 'running', 'running',
		'', '[]'::jsonb, '', '', $5)`,
		wsB, runB, agentB, "agent-b", f.now)
	threadB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
		created_by_type, created_by_handle, created_by_name, created_at, updated_at)
		VALUES ($1, $2, $3, 'Thread B', 'open', 'system', '', '', $4, $5)`,
		wsB, threadB, docB, f.now, f.now)
	msgB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO thread_messages (workspace_id, id, thread_id, author_type,
		author_handle, author_name, body, kind, created_at)
		VALUES ($1, $2, $3, 'system', '', '', 'body', 'message', $4)`,
		wsB, msgB, threadB, f.now)

	// Create workspace A entities for FK sources.
	accountA := uuid.NewString()
	f.insertAccount(t, accountA)
	userA := uuid.NewString()
	f.insertUser(t, userA)
	daemonA := uuid.NewString()
	f.insertDaemon(t, daemonA)
	agentA := uuid.NewString()
	f.insertAgent(t, agentA, daemonA)
	runA := uuid.NewString()
	f.insertAgentRun(t, runA, agentA)
	docA := uuid.NewString()
	f.insertDocument(t, docA)
	threadA := uuid.NewString()
	f.insertThread(t, threadA, docA)
	msgA := uuid.NewString()
	f.insertThreadMessage(t, msgA, threadA)

	tests := []struct {
		name  string
		query string
		args  []any
	}{
		// ── Membership/invite refs ──
		{
			"workspace_members.user_id",
			`INSERT INTO workspace_members (workspace_id, account_id, user_id, membership_role, status, created_at)
			 VALUES ($1, $2, $3, 'member', 'active', $4)`,
			[]any{f.workspaceID, accountA, userB, f.now},
		},
		{
			"workspace_members.invited_by",
			`INSERT INTO workspace_members (workspace_id, account_id, user_id, invited_by, membership_role, status, created_at)
			 VALUES ($1, $2, $3, $4, 'member', 'active', $5)`,
			[]any{f.workspaceID, accountA, userA, userB, f.now},
		},
		{
			"workspace_invites.created_by_user_id",
			`INSERT INTO workspace_invites (id, workspace_id, token_hash, created_by_user_id, expires_at, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			[]any{uuid.NewString(), f.workspaceID, "hash_x", userB, f.now.Add(24 * time.Hour), f.now},
		},
		// ── Daemon/agent/run refs ──
		{
			"agents.daemon_id",
			`INSERT INTO agents (workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
				current_turn_id, session_id, status, current_task, current_activity, updated_at)
			 VALUES ($1, $2, $3, $4, $5, 'assistant', 'codex', '', '', '', '', 'idle', '', '', $6)`,
			[]any{f.workspaceID, uuid.NewString(), daemonB, "agent-cross", "Cross Agent", f.now},
		},
		{
			"agent_runs.agent_id",
			`INSERT INTO agent_runs (workspace_id, id, agent_id, agent_handle, agent_name, agent_kind,
				system_prompt, workspace_root, working_dir, prompt, status, desired_status,
				last_message, log_tail, error, assigned_task_ref, updated_at)
			 VALUES ($1, $2, $3, $4, 'Agent', 'codex', '', '', '.', 'test', 'running', 'running',
				'', '[]'::jsonb, '', '', $5)`,
			[]any{f.workspaceID, uuid.NewString(), agentB, "agent-b", f.now},
		},
		{
			"agents.current_run_id",
			`UPDATE agents SET current_run_id = $1 WHERE id = $2`,
			[]any{runB, agentA},
		},
		{
			"agent_events.agent_id",
			`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
				summary, prompt, dedup_key, last_error, attempt_count, available_at, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'test', 'pending', '', '', '', '', 0, $5, $6, $7)`,
			[]any{f.workspaceID, uuid.NewString(), agentB, "agent-b", f.now, f.now, f.now},
		},
		{
			"agent_events.run_id",
			`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
				summary, prompt, dedup_key, last_error, attempt_count, run_id, available_at, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'test', 'pending', '', '', '', '', 0, $5, $6, $7, $8)`,
			[]any{f.workspaceID, uuid.NewString(), agentA, "agent-a", runB, f.now, f.now, f.now},
		},
		{
			"agent_document_views.agent_id",
			`INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector, viewed_at)
			 VALUES ($1, $2, $3, 0, '', $4)`,
			[]any{f.workspaceID, agentB, f.rootDocID, f.now},
		},
		// ── Document refs ──
		{
			"document_heads.document_id",
			`INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
			 VALUES ($1, $2, '', 0, $3)`,
			[]any{f.workspaceID, docB, f.now},
		},
		{
			"document_updates.document_id",
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_type, created_at)
			 VALUES ($1, $2, $3, 'system', $4)`,
			[]any{f.workspaceID, docB, []byte{0}, f.now},
		},
		{
			"document_checkpoints.document_id",
			`INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
			 VALUES ($1, $2, 0, '', '', $3)`,
			[]any{f.workspaceID, docB, f.now},
		},
		{
			"threads.document_id",
			`INSERT INTO threads (workspace_id, id, document_id, title, status,
				created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			 VALUES ($1, $2, $3, 'Cross Thread', 'open', 'system', '', '', $4, $5)`,
			[]any{f.workspaceID, uuid.NewString(), docB, f.now, f.now},
		},
		{
			"presences.document_id",
			`INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
			 VALUES ($1, $2, 'human', $3, '', 'editing', '', $4)`,
			[]any{f.workspaceID, userA, docB, f.now},
		},
		{
			"agent_document_views.document_id",
			`INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector, viewed_at)
			 VALUES ($1, $2, $3, 0, '', $4)`,
			[]any{f.workspaceID, agentA, docB, f.now},
		},
		{
			"activities.document_id",
			`INSERT INTO activities (workspace_id, type, document_id, actor_type, summary, occurred_at,
				provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', $2, 'system', '', $3,
				'', '', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, docB, f.now},
		},
		{
			"agent_events.document_id",
			`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
				document_id, summary, prompt, dedup_key, last_error, attempt_count, available_at, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'test', 'pending', $5, '', '', '', '', 0, $6, $7, $8)`,
			[]any{f.workspaceID, uuid.NewString(), agentA, "agent-a", docB, f.now, f.now, f.now},
		},
		// ── Thread/message refs ──
		{
			"thread_messages.thread_id",
			`INSERT INTO thread_messages (workspace_id, id, thread_id, author_type,
				author_handle, author_name, body, kind, created_at)
			 VALUES ($1, $2, $3, 'system', '', '', 'body', 'message', $4)`,
			[]any{f.workspaceID, uuid.NewString(), threadB, f.now},
		},
		{
			"thread_participants.thread_id",
			`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
			 VALUES ($1, $2, $3)`,
			[]any{f.workspaceID, threadB, userA},
		},
		{
			"agent_events.thread_id",
			`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
				thread_id, summary, prompt, dedup_key, last_error, attempt_count, available_at, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'test', 'pending', $5, '', '', '', '', 0, $6, $7, $8)`,
			[]any{f.workspaceID, uuid.NewString(), agentA, "agent-a", threadB, f.now, f.now, f.now},
		},
		{
			"agent_events.thread_message_id",
			`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
				thread_message_id, summary, prompt, dedup_key, last_error, attempt_count, available_at, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'test', 'pending', $5, '', '', '', '', 0, $6, $7, $8)`,
			[]any{f.workspaceID, uuid.NewString(), agentA, "agent-a", msgB, f.now, f.now, f.now},
		},
		{
			"workspace_members.last_accessed_document_id",
			`INSERT INTO workspace_members (workspace_id, account_id, user_id, last_accessed_document_id, membership_role, status, created_at)
			 VALUES ($1, $2, $3, $4, 'member', 'active', $5)`,
			[]any{f.workspaceID, accountA, userA, docB, f.now},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.db.Exec(tc.query, tc.args...)
			if err == nil {
				t.Fatalf("expected cross-workspace ref to be rejected")
			}
		})
	}
}

// TestCompositeKeyIntrospection auto-discovers every FK referencing a table that
// has a (workspace_id, id) composite unique index, and asserts the FK itself uses
// the composite column pair. A future plain REFERENCES table(id) FK fails this
// automatically without anyone adding it to a list.
func TestCompositeKeyIntrospection(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	// Tables that are NOT workspace-scoped (no workspace_id column or no composite unique).
	// FKs referencing these are allowed to be simple single-column.
	nonWorkspaceTables := map[string]bool{
		"accounts":   true,
		"workspaces": true,
	}

	// Find all tables that have a composite unique index on (workspace_id, id).
	compositeParents := map[string]bool{}
	rows, err := db.Query(`
		SELECT t.relname
		FROM pg_index i
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = 'public'
		  AND i.indisunique
		  AND array_length(i.indkey, 1) = 2
		  AND EXISTS (
			SELECT 1 FROM pg_attribute a
			WHERE a.attrelid = t.oid AND a.attnum = i.indkey[0] AND a.attname = 'workspace_id'
		  )
		  AND EXISTS (
			SELECT 1 FROM pg_attribute a
			WHERE a.attrelid = t.oid AND a.attnum = i.indkey[1] AND a.attname = 'id'
		  )`)
	if err != nil {
		t.Fatalf("query composite parents: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan composite parent: %v", err)
		}
		compositeParents[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate composite parents: %v", err)
	}
	if len(compositeParents) == 0 {
		t.Fatal("expected at least one table with (workspace_id, id) composite unique")
	}

	// For every FK referencing a composite-parent table, assert exact column pairs:
	// source columns = (workspace_id, <ref>), referenced columns = (workspace_id, id).
	// Uses pg_constraint + pg_attribute for positional column access.
	fkRows, err := db.Query(`
		SELECT
			c.conname,
			src.relname AS source_table,
			ref.relname AS ref_table,
			(SELECT string_agg(a.attname, ',' ORDER BY ord.n)
			 FROM unnest(c.conkey) WITH ORDINALITY AS ord(attnum, n)
			 JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ord.attnum
			) AS source_cols,
			(SELECT string_agg(a.attname, ',' ORDER BY ord.n)
			 FROM unnest(c.confkey) WITH ORDINALITY AS ord(attnum, n)
			 JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = ord.attnum
			) AS ref_cols
		FROM pg_constraint c
		JOIN pg_class src ON src.oid = c.conrelid
		JOIN pg_class ref ON ref.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = src.relnamespace
		WHERE c.contype = 'f'
		  AND n.nspname = 'public'
		ORDER BY c.conname`)
	if err != nil {
		t.Fatalf("query all FKs: %v", err)
	}
	defer fkRows.Close()

	checked := 0
	for fkRows.Next() {
		var name, table, refTable, sourceColsStr, refColsStr string
		if err := fkRows.Scan(&name, &table, &refTable, &sourceColsStr, &refColsStr); err != nil {
			t.Fatalf("scan FK: %v", err)
		}
		if nonWorkspaceTables[refTable] || !compositeParents[refTable] {
			continue
		}
		sourceCols := strings.Split(sourceColsStr, ",")
		refCols := strings.Split(refColsStr, ",")

		// Source must be exactly (<workspace-scope col>, <ref>). Every
		// workspace-scoped table names that scope column workspace_id, except
		// workspaces itself, whose own id is the workspace scope.
		wantScopeCol := "workspace_id"
		if table == "workspaces" {
			wantScopeCol = "id"
		}
		if len(sourceCols) != 2 || sourceCols[0] != wantScopeCol {
			t.Errorf("FK %s on %s -> %s: source columns [%s] must be (%s, <ref>)",
				name, table, refTable, sourceColsStr, wantScopeCol)
		}
		// Referenced must be exactly (workspace_id, id).
		if len(refCols) != 2 || refCols[0] != "workspace_id" || refCols[1] != "id" {
			t.Errorf("FK %s on %s -> %s: referenced columns [%s] must be (workspace_id, id)",
				name, table, refTable, refColsStr)
		}
		checked++
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("iterate FKs: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected to check at least one workspace-scoped FK")
	}
	t.Logf("verified %d workspace-scoped FKs use exact (workspace_id, ref) -> (workspace_id, id) column pairs", checked)
}

// TestPolymorphicTriggerRejectsCrossWorkspaceRef is table-driven over all 8
// trigger-guarded polymorphic surfaces, each asserting a workspace-A row with
// a workspace-B principal rejects.
func TestPolymorphicTriggerRejectsCrossWorkspaceRef(t *testing.T) {
	f := newFKTestFixture(t)

	// Create workspace B entities.
	wsB := uuid.NewString()
	rootDocB := uuid.NewString()
	seedWorkspaceRowFK(t, f.db, wsB, "ws-b-"+wsB[:8], "Workspace B", rootDocB, f.now)
	userB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'member', 'human', 'active', $5, $6)`,
		wsB, userB, "user-b", "User B", f.now, f.now)
	daemonB := uuid.NewString()
	runtimes2, _ := json.Marshal([]RuntimeDetection{{Kind: "codex", Available: true}})
	mustExecFK(t, f.db, `INSERT INTO daemons (id, workspace_id, name, token_hash, status, runtime_detections, created_at)
		VALUES ($1, $2, 'Daemon B', $3, 'active', $4, $5)`,
		daemonB, wsB, "token_b2", runtimes2, f.now)
	agentB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO agents (workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
		current_turn_id, session_id, status, current_task, current_activity, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'assistant', 'codex', '', '', '', '', 'idle', '', '', $6)`,
		wsB, agentB, daemonB, "agent-b2", "Agent B", f.now)

	// Workspace A entities.
	docA := uuid.NewString()
	f.insertDocument(t, docA)
	userA := uuid.NewString()
	f.insertUser(t, userA)
	daemonA := uuid.NewString()
	f.insertDaemon(t, daemonA)
	agentA := uuid.NewString()
	f.insertAgent(t, agentA, daemonA)
	threadA := uuid.NewString()
	f.insertThread(t, threadA, docA)

	tests := []struct {
		name  string
		query string
		args  []any
	}{
		// document_updates.actor_id — human
		{
			"document_updates.actor_id/human",
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, $4, 'human', $5)`,
			[]any{f.workspaceID, docA, []byte{0}, userB, f.now},
		},
		// document_updates.actor_id — agent
		{
			"document_updates.actor_id/agent",
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, $4, 'agent', $5)`,
			[]any{f.workspaceID, docA, []byte{0}, agentB, f.now},
		},
		// document_updates.actor_id — daemon
		{
			"document_updates.actor_id/daemon",
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, $4, 'daemon', $5)`,
			[]any{f.workspaceID, docA, []byte{0}, daemonB, f.now},
		},
		// threads.created_by_id — human
		{
			"threads.created_by_id/human",
			`INSERT INTO threads (workspace_id, id, document_id, title, status,
				created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			 VALUES ($1, $2, $3, 'Test', 'open', $4, 'human', 'u', 'U', $5, $6)`,
			[]any{f.workspaceID, uuid.NewString(), docA, userB, f.now, f.now},
		},
		// threads.created_by_id — agent
		{
			"threads.created_by_id/agent",
			`INSERT INTO threads (workspace_id, id, document_id, title, status,
				created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			 VALUES ($1, $2, $3, 'Test', 'open', $4, 'agent', 'a', 'A', $5, $6)`,
			[]any{f.workspaceID, uuid.NewString(), docA, agentB, f.now, f.now},
		},
		// thread_messages.author_id — human
		{
			"thread_messages.author_id/human",
			`INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type,
				author_handle, author_name, body, kind, created_at)
			 VALUES ($1, $2, $3, $4, 'human', 'u', 'U', 'body', 'message', $5)`,
			[]any{f.workspaceID, uuid.NewString(), threadA, userB, f.now},
		},
		// thread_messages.author_id — agent
		{
			"thread_messages.author_id/agent",
			`INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type,
				author_handle, author_name, body, kind, created_at)
			 VALUES ($1, $2, $3, $4, 'agent', 'a', 'A', 'body', 'message', $5)`,
			[]any{f.workspaceID, uuid.NewString(), threadA, agentB, f.now},
		},
		// thread_participants.participant_id — user from workspace B
		{
			"thread_participants.participant_id/user",
			`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
			 VALUES ($1, $2, $3)`,
			[]any{f.workspaceID, threadA, userB},
		},
		// thread_participants.participant_id — agent from workspace B
		{
			"thread_participants.participant_id/agent",
			`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
			 VALUES ($1, $2, $3)`,
			[]any{f.workspaceID, threadA, agentB},
		},
		// presences.actor_id — human
		{
			"presences.actor_id/human",
			`INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
			 VALUES ($1, $2, 'human', $3, '', 'editing', '', $4)`,
			[]any{f.workspaceID, userB, docA, f.now},
		},
		// presences.actor_id — agent
		{
			"presences.actor_id/agent",
			`INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
			 VALUES ($1, $2, 'agent', $3, '', 'editing', '', $4)`,
			[]any{f.workspaceID, agentB, docA, f.now},
		},
		// presences.actor_id — daemon
		{
			"presences.actor_id/daemon",
			`INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
			 VALUES ($1, $2, 'daemon', $3, '', 'editing', '', $4)`,
			[]any{f.workspaceID, daemonB, docA, f.now},
		},
		// activities.actor_id — human
		{
			"activities.actor_id/human",
			`INSERT INTO activities (workspace_id, type, actor_id, actor_type, summary, occurred_at,
				provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', $2, 'human', '', $3,
				'', '', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, userB, f.now},
		},
		// activities.actor_id — agent
		{
			"activities.actor_id/agent",
			`INSERT INTO activities (workspace_id, type, actor_id, actor_type, summary, occurred_at,
				provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', $2, 'agent', '', $3,
				'', '', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, agentB, f.now},
		},
		// activities.provenance_actor_id — human
		{
			"activities.provenance_actor_id/human",
			`INSERT INTO activities (workspace_id, type, actor_type, summary, occurred_at,
				provenance_actor_id, provenance_actor_type,
				provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', 'system', '', $2,
				$3, 'human',
				'', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, f.now, userB},
		},
		// activities.provenance_actor_id — agent
		{
			"activities.provenance_actor_id/agent",
			`INSERT INTO activities (workspace_id, type, actor_type, summary, occurred_at,
				provenance_actor_id, provenance_actor_type,
				provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', 'system', '', $2,
				$3, 'agent',
				'', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, f.now, agentB},
		},
		// activities.provenance_actor_id — daemon
		{
			"activities.provenance_actor_id/daemon",
			`INSERT INTO activities (workspace_id, type, actor_type, summary, occurred_at,
				provenance_actor_id, provenance_actor_type,
				provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', 'system', '', $2,
				$3, 'daemon',
				'', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, f.now, daemonB},
		},
		// agent_events.claimed_by — cross-workspace daemon
		{
			"agent_events.claimed_by/cross-workspace",
			`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
				summary, prompt, dedup_key, last_error, attempt_count, claimed_by, available_at, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'test', 'claimed', '', '', '', '', 0, $5, $6, $7, $8)`,
			[]any{f.workspaceID, uuid.NewString(), agentA, "agent-a", daemonB, f.now, f.now, f.now},
		},
		// unknown type — must fail closed on every type-dispatched surface
		{
			"document_updates.actor_type/unknown_rejects",
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, $4, 'banana', $5)`,
			[]any{f.workspaceID, docA, []byte{0}, userA, f.now},
		},
		{
			"threads.created_by_type/unknown_rejects",
			`INSERT INTO threads (workspace_id, id, document_id, title, status,
				created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			 VALUES ($1, $2, $3, 'Test', 'open', $4, 'banana', 'u', 'U', $5, $6)`,
			[]any{f.workspaceID, uuid.NewString(), docA, userA, f.now, f.now},
		},
		{
			"thread_messages.author_type/unknown_rejects",
			`INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type,
				author_handle, author_name, body, kind, created_at)
			 VALUES ($1, $2, $3, $4, 'banana', 'u', 'U', 'body', 'message', $5)`,
			[]any{f.workspaceID, uuid.NewString(), threadA, userA, f.now},
		},
		{
			"presences.actor_type/unknown_rejects",
			`INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
			 VALUES ($1, $2, 'banana', $3, '', 'editing', '', $4)`,
			[]any{f.workspaceID, userA, docA, f.now},
		},
		{
			"activities.actor_type/unknown_rejects",
			`INSERT INTO activities (workspace_id, type, actor_id, actor_type, summary, occurred_at,
				provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', $2, 'banana', '', $3,
				'', '', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, userA, f.now},
		},
		{
			"activities.provenance_actor_type/unknown_rejects",
			`INSERT INTO activities (workspace_id, type, actor_type, summary, occurred_at,
				provenance_actor_id, provenance_actor_type,
				provenance_execution_id, provenance_tool, provenance_trigger,
				provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref)
			 VALUES ($1, 'test', 'system', '', $2,
				$3, 'banana',
				'', '', '', FALSE, '', '', '', '', '', '')`,
			[]any{f.workspaceID, f.now, userA},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.db.Exec(tc.query, tc.args...)
			if err == nil {
				t.Fatalf("expected cross-workspace polymorphic ref to be rejected")
			}
		})
	}
}

func TestAgentEventClaimedByRejectsUnrelatedDaemon(t *testing.T) {
	f := newFKTestFixture(t)

	daemonA := uuid.NewString()
	daemonB := uuid.NewString()
	agentID := uuid.NewString()

	f.insertDaemon(t, daemonA)
	f.insertDaemon(t, daemonB)
	f.insertAgent(t, agentID, daemonA)

	// claimed_by = daemonA (agent's daemon) should succeed.
	eventOK := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
		summary, prompt, dedup_key, last_error, attempt_count, claimed_by, available_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'test', 'claimed', '', '', '', '', 0, $5, $6, $7, $8)`,
		f.workspaceID, eventOK, agentID, "agent-"+agentID[:8], daemonA, f.now, f.now, f.now)

	// claimed_by = daemonB (unrelated same-workspace daemon) should fail.
	eventBad := uuid.NewString()
	_, err := f.db.Exec(`INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
		summary, prompt, dedup_key, last_error, attempt_count, claimed_by, available_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'test', 'claimed', '', '', '', '', 0, $5, $6, $7, $8)`,
		f.workspaceID, eventBad, agentID, "agent-"+agentID[:8], daemonB, f.now, f.now, f.now)
	if err == nil {
		t.Fatal("expected unrelated daemon claimed_by to be rejected")
	}
	if !strings.Contains(err.Error(), "is not the event agent") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolymorphicWorkspaceRefStateMachine(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	f.insertDocument(t, docID)
	threadID := uuid.NewString()
	f.insertThread(t, threadID, docID)
	actors := f.insertActors(t)
	foreign := f.insertForeignActors(t)

	sameWorkspaceID := map[string]string{
		"human":  actors.userID,
		"agent":  actors.agentID,
		"daemon": actors.daemonID,
	}
	foreignID := map[string]string{
		"human":  foreign.userID,
		"agent":  foreign.agentID,
		"daemon": foreign.daemonID,
	}

	type polymorphicSurface struct {
		name            string
		configuredTypes []string
		nullableID      bool
		insert          func(actorID any, actorType string) (string, []any)
	}

	surfaces := []polymorphicSurface{
		{
			name:            "document_updates.actor_id",
			configuredTypes: []string{"human", "agent", "daemon"},
			nullableID:      true,
			insert: func(actorID any, actorType string) (string, []any) {
				return `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
					VALUES ($1, $2, $3, $4, $5, $6)`,
					[]any{f.workspaceID, docID, []byte{0}, actorID, actorType, f.now}
			},
		},
		{
			name:            "threads.created_by_id",
			configuredTypes: []string{"human", "agent"},
			nullableID:      true,
			insert: func(actorID any, actorType string) (string, []any) {
				return `INSERT INTO threads (workspace_id, id, document_id, title, status,
					created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
					VALUES ($1, $2, $3, 'State Thread', 'open', $4, $5, 'h', 'n', $6, $7)`,
					[]any{f.workspaceID, uuid.NewString(), docID, actorID, actorType, f.now, f.now}
			},
		},
		{
			name:            "thread_messages.author_id",
			configuredTypes: []string{"human", "agent"},
			nullableID:      true,
			insert: func(actorID any, actorType string) (string, []any) {
				return `INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type,
					author_handle, author_name, body, kind, created_at)
					VALUES ($1, $2, $3, $4, $5, 'h', 'n', 'body', 'message', $6)`,
					[]any{f.workspaceID, uuid.NewString(), threadID, actorID, actorType, f.now}
			},
		},
		{
			name:            "presences.actor_id",
			configuredTypes: []string{"human", "agent", "daemon"},
			insert: func(actorID any, actorType string) (string, []any) {
				return `INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity, updated_at)
					VALUES ($1, $2, $3, $4, '', 'editing', '', $5)`,
					[]any{f.workspaceID, actorID, actorType, docID, f.now}
			},
		},
		{
			name:            "activities.actor_id",
			configuredTypes: []string{"human", "agent", "daemon"},
			nullableID:      true,
			insert: func(actorID any, actorType string) (string, []any) {
				return `INSERT INTO activities (workspace_id, type, document_id, actor_id, actor_type, summary, occurred_at,
					provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
					provenance_autonomous, provenance_confidence, provenance_requested_by,
					provenance_source, provenance_intended_scope, provenance_read_set_summary,
					presence_ref)
					VALUES ($1, 'test', $2, $3, $4, '', $5,
					'', '', '', '', FALSE, '', '', '', '', '', '')`,
					[]any{f.workspaceID, docID, actorID, actorType, f.now}
			},
		},
		{
			name:            "activities.provenance_actor_id",
			configuredTypes: []string{"human", "agent", "daemon"},
			nullableID:      true,
			insert: func(actorID any, actorType string) (string, []any) {
				return `INSERT INTO activities (workspace_id, type, document_id, actor_type, summary, occurred_at,
					provenance_actor_id, provenance_actor_type,
					provenance_execution_id, provenance_tool, provenance_trigger,
					provenance_autonomous, provenance_confidence, provenance_requested_by,
					provenance_source, provenance_intended_scope, provenance_read_set_summary,
					presence_ref)
					VALUES ($1, 'test', $2, 'system', '', $3, $4, $5,
					'', '', '', FALSE, '', '', '', '', '', '')`,
					[]any{f.workspaceID, docID, f.now, actorID, actorType}
			},
		},
	}

	for _, surface := range surfaces {
		surface := surface
		t.Run(surface.name, func(t *testing.T) {
			if surface.nullableID {
				query, args := surface.insert(nil, surface.configuredTypes[0])
				mustExecFK(t, f.db, query, args...)
			}

			for _, sentinelType := range []string{"", "system"} {
				query, args := surface.insert(uuid.NewString(), sentinelType)
				mustExecFK(t, f.db, query, args...)
			}

			for _, actorType := range surface.configuredTypes {
				t.Run(actorType+"/same_workspace_accepts", func(t *testing.T) {
					query, args := surface.insert(sameWorkspaceID[actorType], actorType)
					mustExecFK(t, f.db, query, args...)
				})
				t.Run(actorType+"/missing_rejects", func(t *testing.T) {
					query, args := surface.insert(uuid.NewString(), actorType)
					expectExecErrorFK(t, f.db, query, args...)
				})
				t.Run(actorType+"/cross_workspace_rejects", func(t *testing.T) {
					query, args := surface.insert(foreignID[actorType], actorType)
					expectExecErrorFK(t, f.db, query, args...)
				})
			}

			query, args := surface.insert(sameWorkspaceID[surface.configuredTypes[0]], "banana")
			expectExecErrorFK(t, f.db, query, args...)

			if !containsStringFK(surface.configuredTypes, "daemon") {
				query, args := surface.insert(sameWorkspaceID["daemon"], "daemon")
				expectExecErrorFK(t, f.db, query, args...)
			}
		})
	}
}

func TestPolymorphicWorkspaceRefRejectsCrossWorkspaceUpdate(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	f.insertDocument(t, docID)
	actors := f.insertActors(t)
	foreign := f.insertForeignActors(t)

	activityID := f.insertActivity(t, strPtr(actors.userID), "human", docID)
	expectExecErrorFK(t, f.db,
		`UPDATE activities SET actor_id = $1, actor_type = 'agent' WHERE id = $2`,
		foreign.agentID, activityID)

	threadID := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
		created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
		VALUES ($1, $2, $3, 'State Thread', 'open', $4, 'human', 'h', 'n', $5, $6)`,
		f.workspaceID, threadID, docID, actors.userID, f.now, f.now)
	expectExecErrorFK(t, f.db,
		`UPDATE threads SET created_by_id = $1, created_by_type = 'daemon' WHERE id = $2`,
		actors.daemonID, threadID)
}

func TestParticipantRefStateMachine(t *testing.T) {
	f := newFKTestFixture(t)

	docID := uuid.NewString()
	f.insertDocument(t, docID)
	threadID := uuid.NewString()
	f.insertThread(t, threadID, docID)
	actors := f.insertActors(t)
	foreign := f.insertForeignActors(t)

	mustExecFK(t, f.db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		VALUES ($1, $2, $3)`, f.workspaceID, threadID, actors.userID)
	mustExecFK(t, f.db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		VALUES ($1, $2, $3)`, f.workspaceID, threadID, actors.agentID)

	expectExecErrorFK(t, f.db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		VALUES ($1, $2, $3)`, f.workspaceID, threadID, uuid.NewString())
	expectExecErrorFK(t, f.db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		VALUES ($1, $2, $3)`, f.workspaceID, threadID, foreign.userID)
	expectExecErrorFK(t, f.db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		VALUES ($1, $2, $3)`, f.workspaceID, threadID, foreign.agentID)
	expectExecErrorFK(t, f.db, `UPDATE thread_participants
		SET participant_id = $1
		WHERE workspace_id = $2 AND thread_id = $3 AND participant_id = $4`,
		foreign.agentID, f.workspaceID, threadID, actors.userID)
}

func TestAgentEventClaimedByStateMachine(t *testing.T) {
	f := newFKTestFixture(t)

	daemonID := uuid.NewString()
	unrelatedDaemonID := uuid.NewString()
	agentID := uuid.NewString()
	f.insertDaemon(t, daemonID)
	f.insertDaemon(t, unrelatedDaemonID)
	f.insertAgent(t, agentID, daemonID)
	foreign := f.insertForeignActors(t)

	insertEvent := func(t *testing.T, claimedBy any) {
		t.Helper()
		eventID := uuid.NewString()
		mustExecFK(t, f.db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
			summary, prompt, dedup_key, last_error, attempt_count, claimed_by, available_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'test', 'claimed', '', '', $5, '', 0, $6, $7, $8, $9)`,
			f.workspaceID, eventID, agentID, "agent-"+agentID[:8], "test-"+eventID, claimedBy, f.now, f.now, f.now)
	}
	expectEventError := func(t *testing.T, claimedBy any) {
		t.Helper()
		eventID := uuid.NewString()
		expectExecErrorFK(t, f.db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
			summary, prompt, dedup_key, last_error, attempt_count, claimed_by, available_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'test', 'claimed', '', '', $5, '', 0, $6, $7, $8, $9)`,
			f.workspaceID, eventID, agentID, "agent-"+agentID[:8], "test-"+eventID, claimedBy, f.now, f.now, f.now)
	}

	t.Run("null_accepts", func(t *testing.T) { insertEvent(t, nil) })
	t.Run("event_agent_accepts", func(t *testing.T) { insertEvent(t, agentID) })
	t.Run("agent_daemon_accepts", func(t *testing.T) { insertEvent(t, daemonID) })
	t.Run("unrelated_same_workspace_daemon_rejects", func(t *testing.T) { expectEventError(t, unrelatedDaemonID) })
	t.Run("cross_workspace_daemon_rejects", func(t *testing.T) { expectEventError(t, foreign.daemonID) })
	t.Run("unknown_claimant_rejects", func(t *testing.T) { expectEventError(t, uuid.NewString()) })
	t.Run("update_rejects", func(t *testing.T) {
		eventID := uuid.NewString()
		mustExecFK(t, f.db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status,
			summary, prompt, dedup_key, last_error, attempt_count, available_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'test', 'pending', '', '', $5, '', 0, $6, $7, $8)`,
			f.workspaceID, eventID, agentID, "agent-"+agentID[:8], "test-"+eventID, f.now, f.now, f.now)
		expectExecErrorFK(t, f.db, `UPDATE agent_events SET claimed_by = $1 WHERE id = $2`, unrelatedDaemonID, eventID)
	})
}

func TestActorDeleteCleanupStateMachine(t *testing.T) {
	t.Run("user_delete_only_cleans_human_refs", func(t *testing.T) {
		f := newFKTestFixture(t)
		docID := uuid.NewString()
		f.insertDocument(t, docID)
		actors := f.insertActors(t)

		humanThreadID := uuid.NewString()
		agentThreadID := uuid.NewString()
		humanMsgID := uuid.NewString()
		agentMsgID := uuid.NewString()
		mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
			created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			VALUES ($1, $2, $3, 'Human Thread', 'open', $4, 'human', 'h', 'Human', $5, $6)`,
			f.workspaceID, humanThreadID, docID, actors.userID, f.now, f.now)
		mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
			created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			VALUES ($1, $2, $3, 'Agent Thread', 'open', $4, 'agent', 'a', 'Agent', $5, $6)`,
			f.workspaceID, agentThreadID, docID, actors.agentID, f.now, f.now)
		f.insertThreadMessageByUser(t, humanMsgID, humanThreadID, actors.userID)
		f.insertThreadMessageByAgent(t, agentMsgID, agentThreadID, actors.agentID)
		f.insertThreadParticipant(t, humanThreadID, actors.userID)
		f.insertThreadParticipant(t, agentThreadID, actors.agentID)
		f.insertPresence(t, actors.userID, "human", docID)
		f.insertPresence(t, actors.agentID, "agent", docID)
		f.insertDocumentUpdate(t, docID, strPtr(actors.userID), "human")
		f.insertDocumentUpdate(t, docID, strPtr(actors.agentID), "agent")
		humanActivityID := f.insertActivity(t, strPtr(actors.userID), "human", docID)
		agentActivityID := f.insertActivity(t, strPtr(actors.agentID), "agent", docID)
		humanProvenanceID := f.insertActivityProvenance(t, actors.userID, "human", docID)
		agentProvenanceID := f.insertActivityProvenance(t, actors.agentID, "agent", docID)

		mustExecFK(t, f.db, `DELETE FROM users WHERE id = $1`, actors.userID)

		if got := countMatchingFK(t, f.db, "document_updates", `workspace_id = $1 AND actor_type = 'human' AND actor_id IS NULL`, f.workspaceID); got != 1 {
			t.Fatalf("human document_updates nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "document_updates", `workspace_id = $1 AND actor_type = 'agent' AND actor_id = $2`, f.workspaceID, actors.agentID); got != 1 {
			t.Fatalf("agent document_updates preserved = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND actor_type = 'human' AND actor_id IS NULL`, humanActivityID); got != 1 {
			t.Fatalf("human activity actor ref nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND actor_type = 'agent' AND actor_id = $2`, agentActivityID, actors.agentID); got != 1 {
			t.Fatalf("agent activity actor ref preserved = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND provenance_actor_type = 'human' AND provenance_actor_id IS NULL`, humanProvenanceID); got != 1 {
			t.Fatalf("human activity provenance ref nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND provenance_actor_type = 'agent' AND provenance_actor_id = $2`, agentProvenanceID, actors.agentID); got != 1 {
			t.Fatalf("agent activity provenance ref preserved = %d, want 1", got)
		}
		if f.threadCreatedByID(t, humanThreadID) != nil {
			t.Fatal("human thread created_by_id should be NULL")
		}
		if got := f.threadCreatedByID(t, agentThreadID); got == nil || *got != actors.agentID {
			t.Fatalf("agent thread created_by_id = %v, want %s", got, actors.agentID)
		}
		if f.threadMessageAuthorID(t, humanMsgID) != nil {
			t.Fatal("human thread_message author_id should be NULL")
		}
		if got := f.threadMessageAuthorID(t, agentMsgID); got == nil || *got != actors.agentID {
			t.Fatalf("agent thread_message author_id = %v, want %s", got, actors.agentID)
		}
		if got := countMatchingFK(t, f.db, "presences", `workspace_id = $1 AND actor_id = $2`, f.workspaceID, actors.userID); got != 0 {
			t.Fatalf("human presence count = %d, want 0", got)
		}
		if got := countMatchingFK(t, f.db, "presences", `workspace_id = $1 AND actor_id = $2`, f.workspaceID, actors.agentID); got != 1 {
			t.Fatalf("agent presence count = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "thread_participants", `workspace_id = $1 AND participant_id = $2`, f.workspaceID, actors.userID); got != 0 {
			t.Fatalf("human participant count = %d, want 0", got)
		}
		if got := countMatchingFK(t, f.db, "thread_participants", `workspace_id = $1 AND participant_id = $2`, f.workspaceID, actors.agentID); got != 1 {
			t.Fatalf("agent participant count = %d, want 1", got)
		}
	})

	t.Run("agent_delete_only_cleans_agent_refs", func(t *testing.T) {
		f := newFKTestFixture(t)
		docID := uuid.NewString()
		f.insertDocument(t, docID)
		actors := f.insertActors(t)

		humanThreadID := uuid.NewString()
		agentThreadID := uuid.NewString()
		humanMsgID := uuid.NewString()
		agentMsgID := uuid.NewString()
		mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
			created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			VALUES ($1, $2, $3, 'Human Thread', 'open', $4, 'human', 'h', 'Human', $5, $6)`,
			f.workspaceID, humanThreadID, docID, actors.userID, f.now, f.now)
		mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
			created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			VALUES ($1, $2, $3, 'Agent Thread', 'open', $4, 'agent', 'a', 'Agent', $5, $6)`,
			f.workspaceID, agentThreadID, docID, actors.agentID, f.now, f.now)
		f.insertThreadMessageByUser(t, humanMsgID, humanThreadID, actors.userID)
		f.insertThreadMessageByAgent(t, agentMsgID, agentThreadID, actors.agentID)
		f.insertThreadParticipant(t, humanThreadID, actors.userID)
		f.insertThreadParticipant(t, agentThreadID, actors.agentID)
		f.insertPresence(t, actors.userID, "human", docID)
		f.insertPresence(t, actors.agentID, "agent", docID)
		f.insertDocumentUpdate(t, docID, strPtr(actors.userID), "human")
		f.insertDocumentUpdate(t, docID, strPtr(actors.agentID), "agent")
		humanActivityID := f.insertActivity(t, strPtr(actors.userID), "human", docID)
		agentActivityID := f.insertActivity(t, strPtr(actors.agentID), "agent", docID)
		humanProvenanceID := f.insertActivityProvenance(t, actors.userID, "human", docID)
		agentProvenanceID := f.insertActivityProvenance(t, actors.agentID, "agent", docID)

		mustExecFK(t, f.db, `DELETE FROM agents WHERE id = $1`, actors.agentID)

		if got := countMatchingFK(t, f.db, "document_updates", `workspace_id = $1 AND actor_type = 'agent' AND actor_id IS NULL`, f.workspaceID); got != 1 {
			t.Fatalf("agent document_updates nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "document_updates", `workspace_id = $1 AND actor_type = 'human' AND actor_id = $2`, f.workspaceID, actors.userID); got != 1 {
			t.Fatalf("human document_updates preserved = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND actor_type = 'agent' AND actor_id IS NULL`, agentActivityID); got != 1 {
			t.Fatalf("agent activity actor ref nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND actor_type = 'human' AND actor_id = $2`, humanActivityID, actors.userID); got != 1 {
			t.Fatalf("human activity actor ref preserved = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND provenance_actor_type = 'agent' AND provenance_actor_id IS NULL`, agentProvenanceID); got != 1 {
			t.Fatalf("agent activity provenance ref nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `id = $1 AND provenance_actor_type = 'human' AND provenance_actor_id = $2`, humanProvenanceID, actors.userID); got != 1 {
			t.Fatalf("human activity provenance ref preserved = %d, want 1", got)
		}
		if got := f.threadCreatedByID(t, humanThreadID); got == nil || *got != actors.userID {
			t.Fatalf("human thread created_by_id = %v, want %s", got, actors.userID)
		}
		if f.threadCreatedByID(t, agentThreadID) != nil {
			t.Fatal("agent thread created_by_id should be NULL")
		}
		if got := f.threadMessageAuthorID(t, humanMsgID); got == nil || *got != actors.userID {
			t.Fatalf("human thread_message author_id = %v, want %s", got, actors.userID)
		}
		if f.threadMessageAuthorID(t, agentMsgID) != nil {
			t.Fatal("agent thread_message author_id should be NULL")
		}
		if got := countMatchingFK(t, f.db, "presences", `workspace_id = $1 AND actor_id = $2`, f.workspaceID, actors.agentID); got != 0 {
			t.Fatalf("agent presence count = %d, want 0", got)
		}
		if got := countMatchingFK(t, f.db, "presences", `workspace_id = $1 AND actor_id = $2`, f.workspaceID, actors.userID); got != 1 {
			t.Fatalf("human presence count = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "thread_participants", `workspace_id = $1 AND participant_id = $2`, f.workspaceID, actors.agentID); got != 0 {
			t.Fatalf("agent participant count = %d, want 0", got)
		}
		if got := countMatchingFK(t, f.db, "thread_participants", `workspace_id = $1 AND participant_id = $2`, f.workspaceID, actors.userID); got != 1 {
			t.Fatalf("human participant count = %d, want 1", got)
		}
	})

	t.Run("daemon_delete_only_cleans_daemon_refs", func(t *testing.T) {
		f := newFKTestFixture(t)
		docID := uuid.NewString()
		f.insertDocument(t, docID)
		actors := f.insertActors(t)
		f.insertDocumentReverseWindow(t, docID, actors.daemonID)
		threadID := uuid.NewString()
		msgID := uuid.NewString()
		mustExecFK(t, f.db, `INSERT INTO threads (workspace_id, id, document_id, title, status,
			created_by_id, created_by_type, created_by_handle, created_by_name, created_at, updated_at)
			VALUES ($1, $2, $3, 'Agent Thread', 'open', $4, 'agent', 'a', 'Agent', $5, $6)`,
			f.workspaceID, threadID, docID, actors.agentID, f.now, f.now)
		f.insertThreadMessageByAgent(t, msgID, threadID, actors.agentID)
		f.insertThreadParticipant(t, threadID, actors.agentID)
		f.insertPresence(t, actors.daemonID, "daemon", docID)
		f.insertDocumentUpdate(t, docID, strPtr(actors.daemonID), "daemon")
		f.insertActivity(t, strPtr(actors.daemonID), "daemon", docID)
		mustExecFK(t, f.db, `INSERT INTO activities (workspace_id, type, document_id, actor_type, summary, occurred_at,
			provenance_actor_id, provenance_actor_type,
			provenance_execution_id, provenance_tool, provenance_trigger,
			provenance_autonomous, provenance_confidence, provenance_requested_by,
			provenance_source, provenance_intended_scope, provenance_read_set_summary,
			presence_ref)
			VALUES ($1, 'test', $2, 'system', '', $3, $4, 'daemon',
			'', '', '', FALSE, '', '', '', '', '', '')`,
			f.workspaceID, docID, f.now, actors.daemonID)

		mustExecFK(t, f.db, `DELETE FROM daemons WHERE id = $1`, actors.daemonID)

		if got := countMatchingFK(t, f.db, "document_reverse_windows", `workspace_id = $1`, f.workspaceID); got != 0 {
			t.Fatalf("daemon reverse-window count = %d, want 0", got)
		}

		if got := countMatchingFK(t, f.db, "document_updates", `workspace_id = $1 AND actor_type = 'daemon' AND actor_id IS NULL`, f.workspaceID); got != 1 {
			t.Fatalf("daemon document_updates nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `workspace_id = $1 AND actor_type = 'daemon' AND actor_id IS NULL`, f.workspaceID); got != 1 {
			t.Fatalf("daemon activity actor refs nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "activities", `workspace_id = $1 AND provenance_actor_type = 'daemon' AND provenance_actor_id IS NULL`, f.workspaceID); got != 1 {
			t.Fatalf("daemon activity provenance refs nulled = %d, want 1", got)
		}
		if got := countMatchingFK(t, f.db, "presences", `workspace_id = $1 AND actor_id = $2`, f.workspaceID, actors.daemonID); got != 0 {
			t.Fatalf("daemon presence count = %d, want 0", got)
		}
		if got := f.threadCreatedByID(t, threadID); got == nil || *got != actors.agentID {
			t.Fatalf("agent thread created_by_id = %v, want %s", got, actors.agentID)
		}
		if got := f.threadMessageAuthorID(t, msgID); got == nil || *got != actors.agentID {
			t.Fatalf("agent thread_message author_id = %v, want %s", got, actors.agentID)
		}
		if got := countMatchingFK(t, f.db, "thread_participants", `workspace_id = $1 AND participant_id = $2`, f.workspaceID, actors.agentID); got != 1 {
			t.Fatalf("agent participant count = %d, want 1", got)
		}
	})
}

func containsStringFK(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
