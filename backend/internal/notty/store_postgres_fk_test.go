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
		constraintName string
		table          string
		referencedTable string
		onDelete       string
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
		table          string
		referencedTable string
		onDelete       string
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

// fkTestFixture holds the IDs and database for a single FK behavior test.
type fkTestFixture struct {
	db          *sql.DB
	workspaceID string
	rootDocID   string
	now         time.Time
}

func newFKTestFixture(t *testing.T) *fkTestFixture {
	t.Helper()
	database := newPostgresTestDatabase(t)
	db := database.DB
	now := time.Now().UTC()
	workspaceID := uuid.NewString()
	rootDocID := uuid.NewString()

	mustExecFK(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, "fk-test-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8],
		"FK Test", rootDocID, now, now)

	mustExecFK(t, db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, '', '', TRUE, 1000, $3)`,
		workspaceID, rootDocID, now)

	return &fkTestFixture{db: db, workspaceID: workspaceID, rootDocID: rootDocID, now: now}
}

func (f *fkTestFixture) insertDocument(t *testing.T, docID string) {
	t.Helper()
	mustExecFK(t, f.db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, 'test/path', 'Test', FALSE, 1001, $3)`,
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
		comment_id, presence_ref)
		VALUES ($1, 'test', $2, $3, $4, 'test', $5,
		'', '', '', '', FALSE, '', '', '', '', '', '', '')
		RETURNING id`,
		f.workspaceID, uuidStringOrNil(docID), actorID, actorType, f.now).Scan(&id)
	if err != nil {
		t.Fatalf("insert activity: %v", err)
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
		"documents", "document_heads", "document_updates", "document_checkpoints",
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
	mustExecFK(t, f.db, `INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		wsB, "ws-b-"+wsB[:8], "Workspace B", rootDocB, f.now, f.now)
	mustExecFK(t, f.db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, '', '', TRUE, 1000, $3)`,
		wsB, rootDocB, f.now)
	docB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, 'b/doc', 'Doc B', FALSE, 1001, $3)`,
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
				comment_id, presence_ref)
			 VALUES ($1, 'test', $2, 'system', '', $3,
				'', '', '', '', FALSE, '', '', '', '', '', '', '')`,
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

	// For every FK in the schema, if it references a composite-parent table,
	// assert the FK includes workspace_id in its source columns.
	fkRows, err := db.Query(`
		SELECT
			tc.constraint_name,
			tc.table_name,
			ccu.table_name AS ref_table,
			string_agg(kcu.column_name, ',' ORDER BY kcu.ordinal_position) AS source_cols
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_name = tc.constraint_name
			AND kcu.constraint_schema = tc.constraint_schema
		JOIN information_schema.constraint_column_usage ccu
			ON ccu.constraint_name = tc.constraint_name
			AND ccu.constraint_schema = tc.constraint_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
			AND tc.table_schema = 'public'
		GROUP BY tc.constraint_name, tc.table_name, ccu.table_name
		ORDER BY tc.constraint_name`)
	if err != nil {
		t.Fatalf("query all FKs: %v", err)
	}
	defer fkRows.Close()

	checked := 0
	for fkRows.Next() {
		var name, table, refTable, sourceColsStr string
		if err := fkRows.Scan(&name, &table, &refTable, &sourceColsStr); err != nil {
			t.Fatalf("scan FK: %v", err)
		}
		if nonWorkspaceTables[refTable] || !compositeParents[refTable] {
			continue
		}
		sourceCols := strings.Split(sourceColsStr, ",")
		hasWorkspaceID := false
		for _, col := range sourceCols {
			if col == "workspace_id" {
				hasWorkspaceID = true
				break
			}
		}
		if !hasWorkspaceID {
			t.Errorf("FK %s on %s -> %s uses simple ref [%s]; must include workspace_id for same-workspace enforcement",
				name, table, refTable, sourceColsStr)
		}
		checked++
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("iterate FKs: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected to check at least one workspace-scoped FK")
	}
	t.Logf("verified %d workspace-scoped FKs use composite (workspace_id, ref) columns", checked)
}

func TestPolymorphicTriggerRejectsCrossWorkspaceRef(t *testing.T) {
	f := newFKTestFixture(t)

	// Create a second workspace with its own user.
	wsB := uuid.NewString()
	rootDocB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		wsB, "ws-b-"+wsB[:8], "Workspace B", rootDocB, f.now, f.now)
	mustExecFK(t, f.db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, updated_at)
		VALUES ($1, $2, '', '', TRUE, 1000, $3)`,
		wsB, rootDocB, f.now)
	userB := uuid.NewString()
	mustExecFK(t, f.db, `INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'member', 'human', 'active', $5, $6)`,
		wsB, userB, "user-b", "User B", f.now, f.now)

	// Insert a document in workspace A.
	docA := uuid.NewString()
	f.insertDocument(t, docA)

	// Attempt to insert a document_update in workspace A referencing user from workspace B.
	_, err := f.db.Exec(`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		f.workspaceID, docA, []byte{0}, userB, "human", f.now)
	if err == nil {
		t.Fatal("expected cross-workspace actor ref to be rejected")
	}
	if !strings.Contains(err.Error(), "references missing user in workspace") {
		t.Fatalf("unexpected error: %v", err)
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

	// claimed_by = daemonB (unrelated daemon) should fail.
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
