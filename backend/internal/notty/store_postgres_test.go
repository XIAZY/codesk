package notty

import (
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

func TestPostgresPersistsNormalizedEntitiesAcrossReload(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "pg-reviewer",
		Name:         "PG Reviewer",
		Role:         "Reviews threaded work",
		Kind:         "codex",
		SystemPrompt: "Stay attached to thread context.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	assertSharedAgentPrompt(t, agent.SystemPrompt, "PG Reviewer", "pg-reviewer", "Reviews threaded work")
	user, err := store.CreateUser(CreateUserRequest{
		Name:   "Ada Proof",
		Handle: "adaproof",
		Role:   "Database validation user",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# notty\n\n")
	thread, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID:    documentID,
		Title:         "Persistence check",
		Body:          "Looks durable. Please review this @pg-reviewer",
		RelativeStart: "pg-relative-start",
		RelativeEnd:   "pg-relative-end",
		Excerpt:       "# notty",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.Anchor.RelativeStart != "pg-relative-start" || thread.Anchor.RelativeEnd != "pg-relative-end" {
		t.Fatalf("expected persisted thread to use caller relative positions, got %#v", thread.Anchor)
	}
	if _, err := store.UpsertPresence(UpsertPresenceRequest{
		ActorID:    user.ID,
		ActorType:  "human",
		DocumentID: documentID,
		FilePath:   "docs/spec.md",
		Mode:       "editing",
		Selection:  []int{0, 5},
		Activity:   "Reviewing persistence",
	}); err != nil {
		t.Fatalf("upsert presence: %v", err)
	}
	_, run, err := store.StartAgentRun(StartAgentRunRequest{
		AgentID: agent.ID,
		Prompt:  "Inspect the thread and leave notes.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{
		AgentID:   agent.ID,
		ClaimedBy: "daemon",
	})
	if err != nil {
		t.Fatalf("claim event: %v", err)
	}
	if _, _, err := store.UpdateAgentRun(run.ID, UpdateAgentRunRequest{
		Status:      "completed",
		SessionID:   "session_pg_123",
		LastMessage: "Done",
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	if _, err := store.UpdateAgentEvent(claimed.ID, UpdateAgentEventRequest{
		Status: "completed",
		RunID:  run.ID,
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("complete event: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reloaded, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()

	snapshot := reloaded.Snapshot()
	if got := snapshot.Documents[documentID]; got == nil || got.Path != "docs/spec.md" {
		t.Fatalf("expected stream-derived document after reload, got %#v", got)
	}
	if got := snapshot.Users[user.ID]; got == nil || got.Handle != "adaproof" {
		t.Fatalf("expected user after reload, got %#v", got)
	}
	if got := snapshot.Agents[agent.ID]; got == nil || got.Handle != "pg-reviewer" {
		t.Fatalf("expected agent after reload, got %#v", got)
	}
	if got := snapshot.Agents[agent.ID]; got == nil || got.SessionID != "session_pg_123" {
		t.Fatalf("expected agent session id after reload, got %#v", got)
	}
	if got := snapshot.Threads[thread.ID]; got == nil || len(got.Messages) != 1 || got.Anchor.RelativeStart != "pg-relative-start" || got.Anchor.RelativeEnd != "pg-relative-end" {
		t.Fatalf("expected thread messages after reload, got %#v", got)
	}
	if got := snapshot.AgentRuns[run.ID]; got == nil || got.Status != "completed" {
		t.Fatalf("expected completed run after reload, got %#v", got)
	}
	if got := snapshot.AgentRuns[run.ID]; got == nil || got.SessionID != "session_pg_123" {
		t.Fatalf("expected persisted run session id, got %#v", got)
	}
	if got := snapshot.AgentEvents[claimed.ID]; got == nil || got.Status != "completed" || got.RunID != run.ID {
		t.Fatalf("expected completed event after reload, got %#v", got)
	}
	if got := snapshot.Presences[user.ID]; got == nil || got.FilePath != "docs/spec.md" || len(got.Selection) != 2 {
		t.Fatalf("expected presence after reload, got %#v", got)
	}
	if len(snapshot.Activities) == 0 {
		t.Fatal("expected activities after reload")
	}
	assertNoActivityMaterializedContentColumn(t, db)
	assertNoProposalTable(t, db)

	var snapshotTable sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.workspace_snapshots')`).Scan(&snapshotTable); err != nil {
		t.Fatalf("query snapshot table existence: %v", err)
	}
	if snapshotTable.Valid {
		t.Fatalf("expected workspace_snapshots table to be removed, got %q", snapshotTable.String)
	}
}

func TestPostgresDocumentAtHandleTextDoesNotCreateMentionEvent(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/plain-at-handle.md", "Draft.\n")
	peer := syncDocumentToDocForTest(t, store, documentID, 77)
	text := peer.GetText("content")
	update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), "Please review @codex-agent.\n", nil)
	})
	if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("apply document update: %v", err)
	}

	items, err := store.ListAgentInbox(agent.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list agent inbox: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("document @handle text must not enqueue for-me items, got %#v", items)
	}
}

func TestPostgresUpdateAgentSessionPersistsTargetAgent(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "session-agent",
		Name:   "Session Agent",
		Role:   "Keeps one long-lived Codex thread",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:          "working",
		CodexThreadID:   "thread_pg",
		CurrentTurnID:   "turn_pg",
		CurrentActivity: "Handling notifications",
	}, OperationMeta{ActorID: "daemon_agent", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("update agent session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reloaded, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()
	reloadedAgent := reloaded.Snapshot().Agents[agent.ID]
	if reloadedAgent == nil {
		t.Fatalf("get reloaded agent: not found")
	}
	if reloadedAgent.CodexThreadID != "thread_pg" || reloadedAgent.CurrentTurnID != "turn_pg" || reloadedAgent.Status != "working" {
		t.Fatalf("expected agent session fields after reload, got %#v", reloadedAgent)
	}
}

func TestPostgresAgentInboxSkipsLogDocumentUpdatesButKeepsThreadMentions(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	logDocumentID := mustCreateTestDocument(t, store, "agent.log", "start\n")
	normalDocumentID := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, _, err := store.ReplaceDocumentText(logDocumentID, "start\ntelemetry\n", OperationMeta{
		ActorID:   "daemon",
		ActorType: "agent",
		Source:    "test",
	}); err != nil {
		t.Fatalf("log document update: %v", err)
	}
	items, err := store.ListAgentInbox(agent.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list inbox after log update: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected log document update to stay out of inbox, got %#v", items)
	}

	thread, message, err := store.CreateThread(CreateThreadRequest{
		DocumentID:    logDocumentID,
		Title:         "Log question",
		Body:          "Please inspect @reviewer",
		RelativeStart: "test-relative-start",
		RelativeEnd:   "test-relative-end",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create log thread: %v", err)
	}
	items, err = store.ListAgentInbox(agent.ID, "for-me", "pending")
	if err != nil {
		t.Fatalf("list inbox after log thread mention: %v", err)
	}
	mention := findAgentEventByType(items, "thread.mentioned")
	if mention == nil || mention.ThreadID != thread.ID || mention.ThreadMessageID != message.ID {
		t.Fatalf("expected log thread mention in for-me inbox, got %s", formatAgentEvents(items))
	}

	store.mu.Lock()
	store.state.AgentEvents = map[string]*AgentEvent{}
	if err := store.persistLocked(); err != nil {
		store.mu.Unlock()
		t.Fatalf("persist missing log thread mention: %v", err)
	}
	store.mu.Unlock()
	if err := store.load(); err != nil {
		t.Fatalf("reload store with missing log thread mention: %v", err)
	}
	items, err = store.ListAgentInbox(agent.ID, "for-me", "pending")
	if err != nil {
		t.Fatalf("list reconciled inbox after reload: %v", err)
	}
	mention = findAgentEventByType(items, "thread.mentioned")
	if mention == nil || mention.ThreadID != thread.ID || mention.ThreadMessageID != message.ID {
		t.Fatalf("expected missing log thread mention to reconcile after reload, got %s", formatAgentEvents(items))
	}

	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{AgentID: agent.ID, ClaimedBy: "daemon"})
	if err != nil {
		t.Fatalf("claim log thread mention: %v", err)
	}
	if claimed.Type != "thread.mentioned" || claimed.ThreadID != thread.ID || claimed.ThreadMessageID != message.ID {
		t.Fatalf("unexpected claimed log thread mention: %#v", claimed)
	}
	if _, err := store.UpdateAgentEvent(claimed.ID, UpdateAgentEventRequest{Status: "completed"}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("complete claimed log thread mention: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(normalDocumentID, "start\nsemantic update\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("normal document update: %v", err)
	}
	items, err = store.ListAgentInbox(agent.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list inbox after normal update: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected normal document update in inbox, got %#v", items)
	}
}

func TestPostgresDiffDocumentReconstructsAcrossStreamCheckpointsAfterReload(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/history.md", "v000\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "historian",
		Name:   "Historian",
		Role:   "Reviews historical diffs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	var midVersion int64
	var midContent string
	for index := 1; index <= 220; index++ {
		content := "v" + strconv.FormatInt(int64(index), 10) + "\n"
		updated, _, err := store.ReplaceDocumentText(documentID, content, OperationMeta{
			ActorID:   "owner",
			ActorType: "human",
			Source:    "test",
		})
		if err != nil {
			t.Fatalf("replace document %d: %v", index, err)
		}
		if index == 100 {
			midVersion = updated.UpdateID
			midContent = content
		}
	}
	final, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get final document: %v", err)
	}
	finalVersion := final.UpdateID
	finalContent, err := documentContentAtUpdatePostgres(db, "ws_notty", final, finalVersion)
	if err != nil {
		t.Fatalf("reconstruct final document: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reloaded, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()

	diff, err := reloaded.DiffDocument(agent.ID, documentID, strconv.FormatInt(midVersion, 10), strconv.FormatInt(finalVersion, 10))
	if err != nil {
		t.Fatalf("diff reconstructed document: %v", err)
	}
	if diff.FromContent != midContent || diff.ToContent != finalContent {
		t.Fatalf("unexpected reconstructed diff contents: from=%q want=%q to=%q want=%q", diff.FromContent, midContent, diff.ToContent, finalContent)
	}
	if !strings.Contains(diff.Unified, "-v100\n") || !strings.Contains(diff.Unified, "+v220\n") {
		t.Fatalf("expected reconstructed diff across checkpoints, got %q", diff.Unified)
	}

	var checkpointCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM crdt_stream_checkpoints WHERE workspace_id = $1 AND stream_id = $2`, "ws_notty", documentID).Scan(&checkpointCount); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpointCount == 0 {
		t.Fatal("expected stream checkpoints after long edit history")
	}
}

func postgresTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}
	if os.Getenv("NOTTY_DATABASE_TEST_ISOLATED") != "1" {
		t.Fatalf("refusing to run destructive Postgres tests without NOTTY_DATABASE_TEST_ISOLATED=1; use scripts/test-postgres.sh")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse NOTTY_DATABASE_TEST_URL: %v", err)
	}
	if dbName := strings.TrimPrefix(parsed.Path, "/"); dbName != "notty_test" {
		t.Fatalf("refusing to run destructive Postgres tests against database %q; expected disposable database notty_test", dbName)
	}
	return dsn
}

func clearNottyTables(db *sql.DB) error {
	statements := []string{
		`DELETE FROM crdt_stream_checkpoints`,
		`DELETE FROM crdt_stream_updates`,
		`DELETE FROM crdt_stream_heads`,
		`DELETE FROM agent_document_views`,
		`DELETE FROM thread_messages`,
		`DELETE FROM thread_participants`,
		`DELETE FROM threads`,
		`DELETE FROM agent_events`,
		`DELETE FROM agent_runs`,
		`DELETE FROM presences`,
		`DELETE FROM activities`,
		`DELETE FROM agents`,
		`DELETE FROM users`,
		`DELETE FROM daemons`,
		`DELETE FROM workspace_members`,
		`DELETE FROM accounts`,
		`DELETE FROM workspaces`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func decodeSyncTypeForTest(t *testing.T, payload []byte) uint64 {
	t.Helper()
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol message: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("expected sync message, got top-level type %d", messageType)
	}
	syncType, _, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode sync message: %v", err)
	}
	return syncType
}

func syncDocumentToDocForTest(t *testing.T, store *Store, documentID string, clientID uint64) *crdt.Doc {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	_, updates, err := store.EncodeDocumentSyncUpdates(documentID, nil)
	if err != nil {
		t.Fatalf("encode document sync updates: %v", err)
	}
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update, "test-bootstrap"); err != nil {
			t.Fatalf("apply sync update: %v", err)
		}
	}
	return doc
}

func assertNoActivityMaterializedContentColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		   FROM information_schema.columns
		  WHERE table_name = 'activities'
		    AND column_name = 'new_content'`,
	).Scan(&count); err != nil {
		t.Fatalf("query activities columns: %v", err)
	}
	if count != 0 {
		t.Fatal("activities must not materialize document content in new_content")
	}
}

func assertNoProposalTable(t *testing.T, db *sql.DB) {
	t.Helper()
	var table sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.proposals')`).Scan(&table); err != nil {
		t.Fatalf("query proposals table existence: %v", err)
	}
	if table.Valid {
		t.Fatalf("proposals table should be removed, got %q", table.String)
	}
}
