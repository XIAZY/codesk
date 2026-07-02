package notty

import (
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	seedCodexDaemonRuntime(t, store)

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
	users := SortedUsers(store.Snapshot())
	if len(users) == 0 {
		t.Fatal("expected seeded workspace user")
	}
	user := users[0]
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# notty\n\n")
	thread, _, _, err := store.CreateThread(CreateThreadRequest{
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
	if got := snapshot.ContentDocuments[documentID]; got == nil {
		t.Fatalf("expected document after reload, got %#v", got)
	}
	if got := snapshot.Users[user.ID]; got == nil || got.Handle != user.Handle {
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
		t.Fatalf("expected persisted run session id after reload, got %#v", got)
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
	assertNoAgentCodexThreadIDColumn(t, db)

	var snapshotTable sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.workspace_snapshots')`).Scan(&snapshotTable); err != nil {
		t.Fatalf("query snapshot table existence: %v", err)
	}
	if snapshotTable.Valid {
		t.Fatalf("expected workspace_snapshots table to be removed, got %q", snapshotTable.String)
	}
}

func TestPostgresLoadRegeneratesMissingCheckpointBeforeSync(t *testing.T) {
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
	documentID := mustCreateTestDocument(t, store, "docs/missing-checkpoint.md", "")
	peer := syncDocumentToDocForTest(t, store, documentID, 77)
	text := peer.GetText("content")
	for _, value := range []string{"1\n", "2\n", "3\n"} {
		update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
			text.Insert(txn, text.LenInTxn(txn), value, nil)
		})
		if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
			ActorID:   "peer",
			ActorType: "human",
			Source:    "test",
		}); err != nil {
			t.Fatalf("apply update %q: %v", value, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID); err != nil {
		t.Fatalf("delete checkpoints: %v", err)
	}
	if _, err := db.Exec(`UPDATE document_heads SET state_vector = '' WHERE workspace_id = $1 AND document_id = $2`, "ws_notty", documentID); err != nil {
		t.Fatalf("clear head state vector: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
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
	if reloadedDocument.StateVector == "" {
		t.Fatal("expected reload to regenerate and advertise a checkpoint state vector")
	}

	var checkpointCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2 AND update_id = $3`, "ws_notty", documentID, reloadedDocument.UpdateID).Scan(&checkpointCount); err != nil {
		t.Fatalf("count regenerated checkpoint: %v", err)
	}
	if checkpointCount != 1 {
		t.Fatalf("expected one regenerated head checkpoint, got %d", checkpointCount)
	}

	_, updates, err := reloaded.EncodeDocumentSyncUpdates(documentID, nil)
	if err != nil {
		t.Fatalf("encode cold sync after regenerated checkpoint: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected cold sync to send one regenerated checkpoint update, got %d updates", len(updates))
	}
	clientDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(clientDoc, updates[0], "checkpoint"); err != nil {
		t.Fatalf("apply regenerated checkpoint: %v", err)
	}
	if got := clientDoc.GetText("content").ToString(); got != "1\n2\n3\n" {
		t.Fatalf("expected regenerated checkpoint to reconstruct document, got %q", got)
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
	seedCodexDaemonRuntime(t, store)

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
	seedCodexDaemonRuntime(t, store)
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
		SessionID:       "thread_pg",
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
	if got == nil || got.Status != "working" || got.SessionID != "thread_pg" || got.CurrentTurnID != "turn_pg" {
		t.Fatalf("expected targeted session fields to persist, got %#v", got)
	}
}

func TestPostgresDocumentUpdatesPersistWithoutWorkspaceSnapshot(t *testing.T) {
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
	if got, err := documentContentAtUpdatePostgres(db, "ws_notty", updated, updated.UpdateID); err != nil || got != "# after\n" {
		t.Fatalf("unexpected reconstructed updated content: %q err=%v", got, err)
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
	headContent, err := documentContentAtUpdatePostgres(db, "ws_notty", head, head.UpdateID)
	if err != nil {
		t.Fatalf("reconstruct document head: %v", err)
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
	if err := store.Close(); err != nil {
		t.Fatalf("close store before cold bootstrap: %v", err)
	}

	store, err = NewStore(dsn)
	if err != nil {
		t.Fatalf("reload store for cold bootstrap: %v", err)
	}
	defer store.Close()
	server := NewServer(Config{}, store)

	clientDoc := crdt.New()
	conn := &DocumentConn{send: make(chan []byte, 128)}
	if err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), conn, documentID, buildSyncStep1ForTest(clientDoc), OperationMeta{
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
			if got := clientDoc.GetText("content").ToString(); got != headContent {
				t.Fatalf("cold bootstrap content diverged: got %q want %q", got, headContent)
			}
			if len(syncTypes) != 2 || syncTypes[0] != yproto.SyncStep2 || syncTypes[1] != yproto.SyncStep1 {
				t.Fatalf("expected one merged sync step2 and final server sync step1; got sync types %v", syncTypes)
			}
			return
		}
	}
}

func TestPostgresApplyCRDTUpdateCreatesPeriodicCheckpointFromHistory(t *testing.T) {
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

	documentID := mustCreateTestDocument(t, store, "docs/periodic-checkpoint.md", "")
	peer := syncDocumentToDocForTest(t, store, documentID, 77)
	text := peer.GetText("content")
	var expected strings.Builder
	for index := 0; index < postgresCheckpointInterval+5; index++ {
		line := strconv.Itoa(index) + "\n"
		expected.WriteString(line)
		update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
			text.Insert(txn, text.LenInTxn(txn), line, nil)
		})
		if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
			ActorID:   "peer",
			ActorType: "human",
			Source:    "test",
		}); err != nil {
			t.Fatalf("apply update %d: %v", index, err)
		}
	}

	head, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document head: %v", err)
	}
	var latestCheckpointUpdateID int64
	if err := db.QueryRow(
		`SELECT update_id
		   FROM document_checkpoints
		  WHERE workspace_id = $1 AND document_id = $2
		  ORDER BY update_id DESC
		  LIMIT 1`,
		"ws_notty",
		documentID,
	).Scan(&latestCheckpointUpdateID); err != nil {
		t.Fatalf("query latest checkpoint: %v", err)
	}
	if tail := head.UpdateID - latestCheckpointUpdateID; tail > 5 {
		t.Fatalf("expected write-side checkpointing to keep tail short, checkpoint=%d head=%d tail=%d", latestCheckpointUpdateID, head.UpdateID, tail)
	}

	_, updates, err := store.EncodeDocumentSyncUpdates(documentID, nil)
	if err != nil {
		t.Fatalf("encode cold bootstrap updates: %v", err)
	}
	if len(updates) > 6 {
		t.Fatalf("expected checkpoint plus short tail, got %d updates", len(updates))
	}
	clientDoc := crdt.New()
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(clientDoc, update, "bootstrap"); err != nil {
			t.Fatalf("apply bootstrap update: %v", err)
		}
	}
	if got := clientDoc.GetText("content").ToString(); got != expected.String() {
		t.Fatalf("checkpoint plus tail content diverged: got %q want %q", got, expected.String())
	}
}

func TestPostgresApplyCRDTUpdatePersistsWithoutWorkspaceSnapshot(t *testing.T) {
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

	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# before\n")

	frontendDoc := syncDocumentToDocForTest(t, store, documentID, 77)
	text := frontendDoc.GetText("content")
	var update []byte
	unsubscribe := frontendDoc.OnUpdate(func(next []byte, origin any) {
		update = append([]byte(nil), next...)
	})
	frontendDoc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), "more\n", nil)
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
	if got, err := documentContentAtUpdatePostgres(db, "ws_notty", updated, updated.UpdateID); err != nil || got != "# before\nmore\n" {
		t.Fatalf("unexpected reconstructed updated content: %q err=%v", got, err)
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

func TestPostgresAgentInboxTracksDocumentUpdatesAndThreadMentions(t *testing.T) {
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
	seedCodexDaemonRuntime(t, store)

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
		t.Fatalf("list inbox after document update: %v", err)
	}
	update := findAgentEventByType(items, "document.updated")
	if update == nil || update.DocumentID != logDocumentID {
		t.Fatalf("expected document update in general inbox, got %#v", items)
	}

	thread, message, _, err := store.CreateThread(CreateThreadRequest{
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

func TestPostgresPersistsUTF8SafeTruncatedAgentEventPrompt(t *testing.T) {
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
	seedCodexDaemonRuntime(t, store)

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	body := strings.Repeat("a", 238) + "可见 @reviewer"
	if _, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID:    documentID,
		Title:         "UTF-8 prompt",
		Body:          body,
		RelativeStart: "test-relative-start",
		RelativeEnd:   "test-relative-end",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create thread with long UTF-8 mention body: %v", err)
	}
	if err := store.load(); err != nil {
		t.Fatalf("reload store after UTF-8 mention prompt: %v", err)
	}
	items, err := store.ListAgentInbox(agent.ID, "for-me", "pending")
	if err != nil {
		t.Fatalf("list inbox: %v", err)
	}
	mention := findAgentEventByType(items, "thread.mentioned")
	if mention == nil {
		t.Fatalf("expected thread mention, got %s", formatAgentEvents(items))
	}
	if !utf8.ValidString(mention.Prompt) {
		t.Fatalf("mention prompt is invalid UTF-8: % x", []byte(mention.Prompt))
	}
	if !strings.Contains(mention.Prompt, "...") {
		t.Fatalf("expected prompt to include truncated body, got %q", mention.Prompt)
	}
}

func TestPostgresDiffDocumentReconstructsAcrossCheckpointsAfterReload(t *testing.T) {
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
	seedCodexDaemonRuntime(t, store)
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

func TestPostgresCreateAgentValidatesDaemonRuntimeReport(t *testing.T) {
	dsn := postgresTestDSN(t)
	cases := []struct {
		name        string
		kind        string
		runtimes    []RuntimeDetection
		wantKind    string
		wantErrText []string
	}{
		{
			name:        "malformed kind",
			kind:        "bad kind",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: true}},
			wantErrText: []string{"invalid agent kind"},
		},
		{
			name:        "no runtime report",
			kind:        "codex",
			wantErrText: []string{"has not reported runtime availability"},
		},
		{
			name:        "runtime unavailable",
			kind:        "codex",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: false, Reason: "codex command not found"}},
			wantErrText: []string{"runtime \"codex\" is unavailable", "codex command not found"},
		},
		{
			name:        "different runtime reported",
			kind:        "claude-code",
			runtimes:    []RuntimeDetection{{Kind: "codex", Available: true}},
			wantErrText: []string{"runtime \"claude-code\" is not reported"},
		},
		{
			name:     "requested runtime available",
			kind:     "Claude-Code",
			runtimes: []RuntimeDetection{{Kind: "claude-code", Available: true, Version: "claude test"}},
			wantKind: "claude-code",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			daemonID := seedStoreDaemonRuntime(t, store, "daemon_runtime_test", tc.runtimes...)

			agent, err := store.CreateAgent(CreateAgentRequest{
				DaemonID: daemonID,
				Handle:   "runtime-agent",
				Name:     "Runtime Agent",
				Role:     "Exercises runtime validation",
				Kind:     tc.kind,
			}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
			if len(tc.wantErrText) > 0 {
				if err == nil {
					t.Fatalf("expected create agent error, got agent %#v", agent)
				}
				for _, want := range tc.wantErrText {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("expected error %q to contain %q", err.Error(), want)
					}
				}
				var agentCount int
				if err := db.QueryRow(`SELECT COUNT(*) FROM agents WHERE workspace_id = $1`, store.state.WorkspaceID).Scan(&agentCount); err != nil {
					t.Fatalf("count agents: %v", err)
				}
				if agentCount != 0 {
					t.Fatalf("failed create should not persist agent rows, got %d", agentCount)
				}
				return
			}
			if err != nil {
				t.Fatalf("create agent: %v", err)
			}
			if agent.Kind != tc.wantKind {
				t.Fatalf("expected persisted kind %q, got %#v", tc.wantKind, agent)
			}
		})
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

func seedStoreDaemonRuntime(t *testing.T, store *Store, daemonID string, runtimes ...RuntimeDetection) string {
	t.Helper()
	if store == nil {
		t.Fatal("store is required")
	}
	daemonID = strings.TrimSpace(daemonID)
	if daemonID == "" {
		daemonID = "daemon_test"
	}
	now := time.Now().UTC()
	store.mu.Lock()
	store.ensureMaps()
	store.state.Daemons[daemonID] = &Daemon{
		ID:          daemonID,
		WorkspaceID: store.state.WorkspaceID,
		Name:        "Test daemon",
		Status:      "active",
		Runtimes:    append([]RuntimeDetection(nil), runtimes...),
		LastSeenAt:  now,
		CreatedAt:   now,
	}
	applyDaemonLiveness(store.state.Daemons[daemonID], now)
	err := store.persistLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("persist test daemon runtime: %v", err)
	}
	return daemonID
}

func seedCodexDaemonRuntime(t *testing.T, store *Store) string {
	t.Helper()
	return seedStoreDaemonRuntime(t, store, "", RuntimeDetection{Kind: "codex", Available: true, Version: "codex test"})
}

func clearNottyTables(db *sql.DB) error {
	statements := []string{
		`DELETE FROM agent_document_views`,
		`DELETE FROM document_checkpoints`,
		`DELETE FROM document_updates`,
		`DELETE FROM document_heads`,
		`DELETE FROM documents`,
		`DELETE FROM thread_messages`,
		`DELETE FROM thread_participants`,
		`DELETE FROM threads`,
		`DELETE FROM agent_events`,
		`DELETE FROM agent_runs`,
		`DELETE FROM presences`,
		`DELETE FROM activities`,
		`DELETE FROM agents`,
		`DELETE FROM account_email_tokens`,
		`DELETE FROM workspace_invites`,
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

func assertNoAgentCodexThreadIDColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		   FROM information_schema.columns
		  WHERE table_name = 'agents'
		    AND column_name = 'codex_thread_id'`,
	).Scan(&count); err != nil {
		t.Fatalf("query agents columns: %v", err)
	}
	if count != 0 {
		t.Fatal("agents must not persist runtime-specific codex_thread_id")
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
