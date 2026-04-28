package notty

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/reearth/ygo/crdt"
	"notty/internal/yproto"
)

func TestPostgresPersistsNormalizedEntitiesAcrossReload(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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
		DocumentID: documentID,
		Title:      "Persistence check",
		Body:       "Looks durable. Please review this @pg-reviewer",
		Start:      0,
		End:        6,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.Anchor.RelativeStart == "" || thread.Anchor.RelativeEnd == "" {
		t.Fatalf("expected persisted thread to use relative positions, got %#v", thread.Anchor)
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
	proposal, err := store.CreateProposal(CreateProposalRequest{
		DocumentID:   documentID,
		Title:        "Persist it",
		Author:       agent.ID,
		ProposedText: "# notty\n\nPostgres-backed.\n",
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
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
	if got := snapshot.Documents[documentID]; got == nil || got.Path != "docs/spec.md" || got.Content != "# notty\n\n" {
		t.Fatalf("expected document after reload, got %#v", got)
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
	if got := snapshot.Threads[thread.ID]; got == nil || len(got.Messages) != 1 || got.Anchor.RelativeStart == "" || got.Anchor.RelativeEnd == "" {
		t.Fatalf("expected thread messages after reload, got %#v", got)
	}
	if got := snapshot.AgentRuns[run.ID]; got == nil || got.Status != "completed" {
		t.Fatalf("expected completed run after reload, got %#v", got)
	}
	if got := snapshot.AgentRuns[run.ID]; got == nil || got.SessionID != "session_pg_123" {
		t.Fatalf("expected persisted run session id after reload, got %#v", got)
	}
	if got := snapshot.AgentEvents[claimed.ID]; got == nil || got.Status != "completed" || got.RunID != run.ID {
		t.Fatalf("expected completed event after reload, got %#v", got)
	}
	if got := snapshot.Presences[user.ID]; got == nil || got.FilePath != "docs/spec.md" || len(got.Selection) != 2 {
		t.Fatalf("expected presence after reload, got %#v", got)
	}
	if got := snapshot.Proposals[proposal.ID]; got == nil || got.Title != "Persist it" {
		t.Fatalf("expected proposal after reload, got %#v", got)
	}
	if len(snapshot.Activities) == 0 {
		t.Fatal("expected activities after reload")
	}
	assertNoActivityMaterializedContentColumn(t, db)

	var snapshotTable sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.workspace_snapshots')`).Scan(&snapshotTable); err != nil {
		t.Fatalf("query snapshot table existence: %v", err)
	}
	if snapshotTable.Valid {
		t.Fatalf("expected workspace_snapshots table to be removed, got %q", snapshotTable.String)
	}
}

func TestPostgresDocumentUpdateHotPathKeepsHeadSnapshotless(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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

	documentID := mustCreateTestDocument(t, store, "docs/hot.md", "")
	initial, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get initial document: %v", err)
	}

	var initialHeadUpdateID int64
	var initialHeadStateVector string
	if err := db.QueryRow(
		`SELECT update_id, state_vector FROM document_heads WHERE workspace_id = $1 AND document_id = $2`,
		"ws_notty",
		documentID,
	).Scan(&initialHeadUpdateID, &initialHeadStateVector); err != nil {
		t.Fatalf("query initial head: %v", err)
	}
	if initialHeadUpdateID <= 0 || initialHeadStateVector == "" {
		t.Fatalf("expected initialized lightweight head, updateID=%d stateVector=%q", initialHeadUpdateID, initialHeadStateVector)
	}
	assertNoDocumentHeadMaterializationColumns(t, db)
	var initialCheckpointState string
	if err := db.QueryRow(
		`SELECT crdt_state FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2 AND update_id = $3`,
		"ws_notty",
		documentID,
		initialHeadUpdateID,
	).Scan(&initialCheckpointState); err != nil {
		t.Fatalf("query initial checkpoint: %v", err)
	}
	if initialCheckpointState == "" {
		t.Fatal("expected initial checkpoint to hold the full CRDT state")
	}

	peer, err := decodeCRDTState(initial.CRDTState, 77)
	if err != nil {
		t.Fatalf("decode peer document: %v", err)
	}
	text := peer.GetText("content")
	for _, value := range []string{"a", "b"} {
		update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
			text.Insert(txn, text.Len(), value, nil)
		})
		if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
			ActorID:   "peer",
			ActorType: "human",
			Source:    "test",
		}); err != nil {
			t.Fatalf("apply update %q: %v", value, err)
		}
	}

	var storedUpdateID int64
	var storedStateVector string
	if err := db.QueryRow(
		`SELECT update_id, state_vector FROM document_heads WHERE workspace_id = $1 AND document_id = $2`,
		"ws_notty",
		documentID,
	).Scan(&storedUpdateID, &storedStateVector); err != nil {
		t.Fatalf("query updated head: %v", err)
	}
	if storedUpdateID <= initialHeadUpdateID || storedStateVector == "" {
		t.Fatalf("expected hot path to update only lightweight head metadata, updateID=%d initial=%d stateVector=%q", storedUpdateID, initialHeadUpdateID, storedStateVector)
	}

	current, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get current document: %v", err)
	}
	currentDoc, err := decodeCRDTState(current.CRDTState, 88)
	if err != nil {
		t.Fatalf("decode current crdt state: %v", err)
	}
	if got := currentDoc.GetText("content").ToString(); got != "ab" {
		t.Fatalf("expected on-demand document state to include hot-path updates, got %q", got)
	}

	if _, err := db.Exec(`DELETE FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID); err != nil {
		t.Fatalf("delete checkpoints: %v", err)
	}
	reloaded, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()
	reloadedDocument, err := reloaded.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get reloaded document: %v", err)
	}
	if reloadedDocument.Content != "ab" {
		t.Fatalf("expected reload to replay update log without a head snapshot, got %q", reloadedDocument.Content)
	}
}

func TestPostgresDocumentAtHandleTextDoesNotCreateMentionEvent(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	peer, err := decodeCRDTState(document.CRDTState, 77)
	if err != nil {
		t.Fatalf("decode document: %v", err)
	}
	text := peer.GetText("content")
	update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), "Please review @codex-agent.\n", nil)
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
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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
	got := reloaded.Snapshot().Agents[agent.ID]
	if got == nil || got.Status != "working" || got.CodexThreadID != "thread_pg" || got.CurrentTurnID != "turn_pg" {
		t.Fatalf("expected targeted session fields to persist, got %#v", got)
	}
}

func TestPostgresDocumentUpdatesPersistWithoutWorkspaceSnapshot(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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

	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# before\n")

	updated, update, err := store.ReplaceDocumentText(documentID, "# after\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected incremental document update bytes")
	}
	if updated.Content != "# after\n" {
		t.Fatalf("unexpected updated content: %q", updated.Content)
	}

	var headUpdateID int64
	var headStateVector string
	if err := db.QueryRow(`SELECT update_id, state_vector FROM document_heads WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID).Scan(&headUpdateID, &headStateVector); err != nil {
		t.Fatalf("query document head: %v", err)
	}
	if headUpdateID == 0 || headStateVector == "" {
		t.Fatalf("expected versioned document head, updateID=%d stateVector=%q", headUpdateID, headStateVector)
	}
	assertNoDocumentHeadMaterializationColumns(t, db)

	var updateCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_updates WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID).Scan(&updateCount); err != nil {
		t.Fatalf("query document updates: %v", err)
	}
	if updateCount == 0 {
		t.Fatal("expected document update log entry")
	}
	var checkpointCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID).Scan(&checkpointCount); err != nil {
		t.Fatalf("query document checkpoints: %v", err)
	}
	if checkpointCount == 0 {
		t.Fatal("expected document checkpoint")
	}
}

func TestPostgresDocumentProtocolColdBootstrapStreamsCheckpointAndTail(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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
	server := NewServer(Config{}, store)

	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "version 0\n")
	for index := 1; index <= 105; index++ {
		if _, _, err := store.ReplaceDocumentText(documentID, "version "+strconv.Itoa(index)+"\n", OperationMeta{
			ActorID:   "owner",
			ActorType: "human",
			Source:    "test",
		}); err != nil {
			t.Fatalf("replace document text %d: %v", index, err)
		}
	}

	head, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document head: %v", err)
	}
	var checkpointUpdateID int64
	if err := db.QueryRow(
		`SELECT update_id
		   FROM document_checkpoints
		  WHERE workspace_id = $1 AND document_id = $2 AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		"ws_notty",
		documentID,
		head.UpdateID,
	).Scan(&checkpointUpdateID); err != nil {
		t.Fatalf("query latest checkpoint: %v", err)
	}
	var tailCount int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		   FROM document_updates
		  WHERE workspace_id = $1 AND document_id = $2 AND id > $3 AND id <= $4`,
		"ws_notty",
		documentID,
		checkpointUpdateID,
		head.UpdateID,
	).Scan(&tailCount); err != nil {
		t.Fatalf("query tail updates: %v", err)
	}
	if tailCount == 0 {
		t.Fatalf("test setup expected tail updates after checkpoint, checkpoint=%d head=%d", checkpointUpdateID, head.UpdateID)
	}

	clientDoc := crdt.New()
	conn := &DocumentConn{send: make(chan []byte, 128)}
	if err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), conn, documentID, yproto.BuildSyncStep1(clientDoc), OperationMeta{
		ActorID:   "client",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("start cold sync: %v", err)
	}

	var syncTypes []uint64
	for {
		select {
		case payload := <-conn.send:
			syncTypes = append(syncTypes, decodeSyncTypeForTest(t, payload))
			reply := applySyncPayloadToDoc(t, clientDoc, payload, "server-cold-bootstrap")
			if len(reply) > 0 {
				if err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), conn, documentID, reply, OperationMeta{
					ActorID:   "client",
					ActorType: "human",
					Source:    "test",
				}); err != nil {
					t.Fatalf("apply cold sync reply: %v", err)
				}
			}
		default:
			if got := clientDoc.GetText("content").ToString(); got != head.Content {
				t.Fatalf("cold bootstrap content diverged: got %q want %q", got, head.Content)
			}
			if len(syncTypes) != tailCount+2 {
				t.Fatalf("expected checkpoint, %d tail updates, and server sync step1; got sync types %v", tailCount, syncTypes)
			}
			if syncTypes[0] != yproto.SyncStep2 {
				t.Fatalf("expected checkpoint as sync step2, got sync types %v", syncTypes)
			}
			for index := 1; index <= tailCount; index++ {
				if syncTypes[index] != yproto.SyncUpdate {
					t.Fatalf("expected tail update at message %d, got sync types %v", index, syncTypes)
				}
			}
			if syncTypes[len(syncTypes)-1] != yproto.SyncStep1 {
				t.Fatalf("expected final server sync step1, got sync types %v", syncTypes)
			}
			return
		}
	}
}

func TestPostgresApplyCRDTUpdatePersistsWithoutWorkspaceSnapshot(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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

	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# before\n")
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}

	frontendDoc, err := decodeCRDTState(document.CRDTState, 77)
	if err != nil {
		t.Fatalf("decode initial state: %v", err)
	}
	text := frontendDoc.GetText("content")
	var update []byte
	unsubscribe := frontendDoc.OnUpdate(func(next []byte, origin any) {
		update = append([]byte(nil), next...)
	})
	frontendDoc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), "more\n", nil)
	}, "browser")
	unsubscribe()
	if len(update) == 0 {
		t.Fatal("expected incremental crdt update bytes")
	}

	updated, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("apply crdt update: %v", err)
	}
	if updated.Content != "# before\nmore\n" {
		t.Fatalf("unexpected updated content: %q", updated.Content)
	}

	var headUpdateID int64
	if err := db.QueryRow(`SELECT update_id FROM document_heads WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID).Scan(&headUpdateID); err != nil {
		t.Fatalf("query document head: %v", err)
	}
	if headUpdateID == 0 {
		t.Fatal("expected document head update id")
	}
	assertNoDocumentHeadMaterializationColumns(t, db)

	var updateCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_updates WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID).Scan(&updateCount); err != nil {
		t.Fatalf("query document updates: %v", err)
	}
	if updateCount == 0 {
		t.Fatal("expected document update log entry")
	}
}

func TestPostgresAgentInboxSkipsLogDocumentUpdatesButKeepsThreadMentions(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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
		DocumentID: logDocumentID,
		Title:      "Log question",
		Body:       "Please inspect @reviewer",
		Start:      0,
		End:        5,
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

func TestPostgresDiffDocumentReconstructsAcrossCheckpointsAfterReload(t *testing.T) {
	dsn := os.Getenv("NOTTY_DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("NOTTY_DATABASE_TEST_URL is not set")
	}

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
	for index := 1; index <= 105; index++ {
		content := "v" + strconv.FormatInt(int64(index), 10) + "\n"
		updated, _, err := store.ReplaceDocumentText(documentID, content, OperationMeta{
			ActorID:   "owner",
			ActorType: "human",
			Source:    "test",
		})
		if err != nil {
			t.Fatalf("replace document %d: %v", index, err)
		}
		if index == 50 {
			midVersion = updated.UpdateID
			midContent = updated.Content
		}
	}
	final, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get final document: %v", err)
	}
	finalVersion := final.UpdateID
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
	if diff.FromContent != midContent || diff.ToContent != final.Content {
		t.Fatalf("unexpected reconstructed diff contents: from=%q want=%q to=%q want=%q", diff.FromContent, midContent, diff.ToContent, final.Content)
	}
	if !strings.Contains(diff.Unified, "-v50\n") || !strings.Contains(diff.Unified, "+v105\n") {
		t.Fatalf("expected reconstructed diff across checkpoints, got %q", diff.Unified)
	}

	var checkpointCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID).Scan(&checkpointCount); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpointCount < 2 {
		t.Fatalf("expected multiple checkpoints after long edit history, got %d", checkpointCount)
	}
}

func clearNottyTables(db *sql.DB) error {
	statements := []string{
		`DELETE FROM agent_document_views`,
		`DELETE FROM document_checkpoints`,
		`DELETE FROM document_updates`,
		`DELETE FROM document_heads`,
		`DELETE FROM documents`,
		`DELETE FROM comments`,
		`DELETE FROM thread_messages`,
		`DELETE FROM thread_participants`,
		`DELETE FROM threads`,
		`DELETE FROM agent_events`,
		`DELETE FROM agent_runs`,
		`DELETE FROM proposals`,
		`DELETE FROM presences`,
		`DELETE FROM activities`,
		`DELETE FROM agents`,
		`DELETE FROM users`,
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

func assertNoDocumentHeadMaterializationColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(
		`SELECT column_name
		   FROM information_schema.columns
		  WHERE table_name = 'document_heads'
		    AND column_name IN ('content', 'crdt_state', 'crdt_state_update_id')`,
	)
	if err != nil {
		t.Fatalf("query document_heads columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan document_heads column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate document_heads columns: %v", err)
	}
	if len(columns) != 0 {
		t.Fatalf("document_heads must not materialize document state, found columns: %v", columns)
	}
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
