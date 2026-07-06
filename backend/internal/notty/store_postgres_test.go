package notty

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

func TestPostgresPersistsNormalizedEntitiesAcrossReload(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "pg-reviewer",
		Name:         "PG Reviewer",
		Role:         "Reviews threaded work",
		Kind:         "codex",
		SystemPrompt: "Stay attached to thread context.",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	assertSharedAgentPrompt(t, agent.SystemPrompt, "PG Reviewer", "pg-reviewer", "Reviews threaded work")
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# notty\n\n")
	thread, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID:    documentID,
		Title:         "Persistence check",
		Body:          "Looks durable. Please review this @pg-reviewer",
		RelativeStart: "pg-relative-start",
		RelativeEnd:   "pg-relative-end",
		Excerpt:       "# notty",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
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
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
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
	workspace := store.Snapshot()
	reloaded, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}

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
	if got, err := getAgentEventPostgres(db, snapshot.WorkspaceID, claimed.ID); err != nil || got == nil || got.Status != "completed" || got.RunID != run.ID {
		t.Fatalf("expected completed event after reload, got %#v (err: %v)", got, err)
	}
	if presences, err := listPresencesPostgres(db, snapshot.WorkspaceID); err != nil {
		t.Fatalf("list presences: %v", err)
	} else if len(presences) == 0 {
		t.Fatal("expected presences in Postgres after reload")
	} else {
		var found bool
		for _, p := range presences {
			if p.ActorID == user.ID && p.FilePath == "docs/spec.md" && len(p.Selection) == 2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected presence for user %s with docs/spec.md, got %d presences", user.ID, len(presences))
		}
	}
	if activities, err := listActivitiesPostgres(db, snapshot.WorkspaceID); err != nil || len(activities) == 0 {
		t.Fatalf("expected activities after reload, got %d (err: %v)", len(activities), err)
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

func TestPostgresThreadPersistPreservesDatabaseOnlyThreads(t *testing.T) {
	database := newPostgresTestDatabase(t)
	staleStore := newPostgresTestWorkspaceStore(t, database)
	user := seedTestUser(t, staleStore)
	documentID := mustCreateTestDocument(t, staleStore, "docs/thread-preserve.md", "keep the thread\n")
	workspace := staleStore.Snapshot()

	freshStore, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("new fresh workspace store: %v", err)
	}
	thread, _, _, err := freshStore.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Preserve me",
		Body:       "This row only exists in the fresher store snapshot.",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread from fresh store: %v", err)
	}

	staleStore.mu.Lock()
	staleStore.ensureMaps()
	if _, ok := staleStore.state.Threads[thread.ID]; ok {
		staleStore.mu.Unlock()
		t.Fatalf("stale store unexpectedly contains thread %s", thread.ID)
	}
	err = staleStore.persistLocked()
	staleStore.mu.Unlock()
	if err != nil {
		t.Fatalf("persist stale store: %v", err)
	}

	reloaded, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload workspace store: %v", err)
	}
	got := reloaded.Snapshot().Threads[thread.ID]
	if got == nil || len(got.Messages) != 1 {
		t.Fatalf("expected database-only thread and message to survive stale persist, got %#v", got)
	}
}

func TestPostgresSnapshotPersistPreservesDatabaseOnlyRows(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	staleStore := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, staleStore)
	user := seedTestUser(t, staleStore)
	agent, err := staleStore.CreateAgent(CreateAgentRequest{
		Handle: "db-only-agent",
		Name:   "DB Only Agent",
		Role:   "Checks stale snapshots",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, staleStore, "docs/db-only.md", "keep rows\n")
	document, err := staleStore.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	workspace := staleStore.Snapshot()
	now := time.Now().UTC()
	eventID := uuid.NewString()

	mustExec(t, db, `INSERT INTO presences (
			workspace_id, actor_id, actor_type, document_id, file_path, mode,
			selection_start, selection_end, activity, updated_at
		) VALUES ($1, $2, 'human', $3, 'docs/db-only.md', 'editing', 0, 4, 'db-only', $4)`,
		workspace.WorkspaceID, user.ID, documentID, now)
	var activityID int64
	if err := db.QueryRow(`INSERT INTO activities (
			workspace_id, type, document_id, actor_id, actor_type, summary, occurred_at,
			provenance_actor_type, provenance_execution_id, provenance_tool, provenance_trigger,
			provenance_autonomous, provenance_confidence, provenance_requested_by,
			provenance_source, provenance_intended_scope, provenance_read_set_summary,
			comment_id, presence_ref
		) VALUES (
			$1, 'db_only', $2, $3, 'human', 'db-only activity', $4,
			'', '', '', '', FALSE, '', '', '', '', '', '', ''
		) RETURNING id`,
		workspace.WorkspaceID, documentID, user.ID, now).Scan(&activityID); err != nil {
		t.Fatalf("insert database-only activity: %v", err)
	}
	mustExec(t, db, `INSERT INTO agent_events (
			workspace_id, id, agent_id, agent_handle, type, box, status, document_id,
			from_update_id, to_update_id, summary, prompt, dedup_key, last_error,
			attempt_count, available_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, 'db.only', 'for_me', 'pending', $5,
			0, 0, 'db-only event', '', $6, '', 0, $7, $7, $7
		)`,
		workspace.WorkspaceID, eventID, agent.ID, agent.Handle, documentID, "db-only-"+eventID, now)
	mustExec(t, db, `INSERT INTO agent_document_views (
			workspace_id, agent_id, document_id, update_id, state_vector, viewed_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		workspace.WorkspaceID, agent.ID, documentID, document.UpdateID, document.StateVector, now)

	staleStore.mu.Lock()
	staleStore.ensureMaps()
	if staleStore.state.AgentDocumentViews[agentDocumentViewKey(agent.ID, documentID)] != nil {
		staleStore.mu.Unlock()
		t.Fatalf("stale store unexpectedly contains database-only agent document view")
	}
	err = staleStore.persistLocked()
	staleStore.mu.Unlock()
	if err != nil {
		t.Fatalf("persist stale store: %v", err)
	}

	assertRowCount := func(table string, where string, args ...any) {
		t.Helper()
		var count int
		query := "SELECT COUNT(*) FROM " + table + " WHERE " + where
		if err := db.QueryRow(query, args...).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected one %s row matching %q, got %d", table, where, count)
		}
	}
	assertRowCount("presences", "workspace_id = $1 AND actor_id = $2", workspace.WorkspaceID, user.ID)
	assertRowCount("activities", "id = $1", activityID)
	assertRowCount("agent_events", "workspace_id = $1 AND id = $2", workspace.WorkspaceID, eventID)
	assertRowCount("agent_document_views", "workspace_id = $1 AND agent_id = $2 AND document_id = $3", workspace.WorkspaceID, agent.ID, documentID)
}

func TestPostgresThreadSurvivesUUIDTurnSessionPersist(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "thread-session-agent",
		Name:   "Thread Session Agent",
		Role:   "Keeps thread writes durable",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/thread-session.md", "durable\n")
	turnID := uuid.NewString()
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:          "working",
		SessionID:       "session-thread-survival",
		CurrentTurnID:   turnID,
		CurrentActivity: "Writing a thread",
	}, OperationMeta{ActorID: "daemon_agent", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("update agent session: %v", err)
	}
	thread, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Survive restart",
		Body:       "This thread must survive after a UUID-shaped turn id session update.",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	store.mu.Lock()
	err = store.persistLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("persist store after session update and thread create: %v", err)
	}

	var currentRunID sql.NullString
	if err := db.QueryRow(`SELECT current_run_id::text FROM agents WHERE workspace_id = $1 AND id = $2`, store.workspaceID, agent.ID).Scan(&currentRunID); err != nil {
		t.Fatalf("select current_run_id: %v", err)
	}
	if currentRunID.Valid {
		t.Fatalf("current_run_id was poisoned by turn id: %q", currentRunID.String)
	}

	workspace := store.Snapshot()
	reloaded, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	snapshot := reloaded.Snapshot()
	gotAgent := snapshot.Agents[agent.ID]
	if gotAgent == nil || gotAgent.CurrentTurnID != turnID || gotAgent.CurrentRunID != "" {
		t.Fatalf("expected turn id without run-id mirror after reload, got %#v", gotAgent)
	}
	gotThread := snapshot.Threads[thread.ID]
	if gotThread == nil || len(gotThread.Messages) != 1 || !containsString(gotThread.ParticipantIDs, user.ID) {
		t.Fatalf("expected thread/message/participant after reload, got %#v", gotThread)
	}
}

func TestPostgresCreatesWorkspaceRootDocumentFromStoredRootID(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	workspaceID := "11111111-1111-1111-1111-111111111111"
	rootDocumentID := "22222222-2222-4222-8222-222222222222"
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID,
		"legacy-root",
		"Legacy Root",
		rootDocumentID,
		now,
		now,
	); err != nil {
		t.Fatalf("insert workspace row: %v", err)
	}

	store, err := NewWorkspaceStore(database, workspaceID, "fallback name")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	snapshot := store.Snapshot()
	if snapshot.Name != "Legacy Root" {
		t.Fatalf("workspace name = %q, want row name", snapshot.Name)
	}
	if snapshot.RootDocumentID != rootDocumentID {
		t.Fatalf("root document ID = %q, want stored %q", snapshot.RootDocumentID, rootDocumentID)
	}
	if !store.HasDocument(rootDocumentID) {
		t.Fatalf("stored root document %q is not syncable", rootDocumentID)
	}

	var storedRootID string
	if err := db.QueryRow(`SELECT root_document_id::text FROM workspaces WHERE id = $1`, workspaceID).Scan(&storedRootID); err != nil {
		t.Fatalf("select stored root document ID: %v", err)
	}
	if storedRootID != rootDocumentID {
		t.Fatalf("stored root document ID = %q, want %q", storedRootID, rootDocumentID)
	}
}

func TestPostgresLoadRegeneratesMissingCheckpointBeforeSync(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
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
	workspace := store.Snapshot()
	if _, err := db.Exec(`DELETE FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, workspace.WorkspaceID, documentID); err != nil {
		t.Fatalf("delete checkpoints: %v", err)
	}
	if _, err := db.Exec(`UPDATE document_heads SET state_vector = '' WHERE workspace_id = $1 AND document_id = $2`, workspace.WorkspaceID, documentID); err != nil {

		t.Fatalf("clear head state vector: %v", err)
	}

	reloaded, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	reloadedDocument, err := reloaded.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get reloaded document: %v", err)
	}
	if reloadedDocument.StateVector == "" {
		t.Fatal("expected reload to regenerate and advertise a checkpoint state vector")
	}

	var checkpointCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2 AND update_id = $3`, workspace.WorkspaceID, documentID, reloadedDocument.UpdateID).Scan(&checkpointCount); err != nil {

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
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
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
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
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
	turnID := uuid.NewString()
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:          "working",
		SessionID:       "thread_pg",
		CurrentTurnID:   turnID,
		CurrentActivity: "Handling notifications",
	}, OperationMeta{ActorID: "daemon_agent", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("update agent session: %v", err)
	}
	workspace := store.Snapshot()
	reloaded, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	got := reloaded.Snapshot().Agents[agent.ID]
	if got == nil || got.Status != "working" || got.SessionID != "thread_pg" || got.CurrentTurnID != turnID || got.CurrentRunID != "" {
		t.Fatalf("expected targeted session fields to persist, got %#v", got)
	}
}

func TestPostgresDocumentUpdatesPersistWithoutWorkspaceSnapshot(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	workspaceID := store.Snapshot().WorkspaceID

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
	if got, err := documentContentAtUpdatePostgres(db, workspaceID, updated, updated.UpdateID); err != nil || got != "# after\n" {

		t.Fatalf("unexpected reconstructed updated content: %q err=%v", got, err)
	}

	var headUpdateID int64
	var headStateVector string
	if err := db.QueryRow(`SELECT update_id, state_vector FROM document_heads WHERE workspace_id = $1 AND document_id = $2`, workspaceID, documentID).Scan(&headUpdateID, &headStateVector); err != nil {

		t.Fatalf("query document head: %v", err)
	}
	if headUpdateID == 0 || headStateVector == "" {
		t.Fatalf("expected versioned document head, updateID=%d stateVector=%q", headUpdateID, headStateVector)
	}
	assertNoDocumentHeadMaterializationColumns(t, db)

	var updateCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_updates WHERE workspace_id = $1 AND document_id = $2`, workspaceID, documentID).Scan(&updateCount); err != nil {

		t.Fatalf("query document updates: %v", err)
	}
	if updateCount == 0 {
		t.Fatal("expected document update log entry")
	}
	var checkpointCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, workspaceID, documentID).Scan(&checkpointCount); err != nil {

		t.Fatalf("query document checkpoints: %v", err)
	}
	if checkpointCount == 0 {
		t.Fatal("expected document checkpoint")
	}
}

func TestPostgresDocumentProtocolColdBootstrapStreamsCheckpointAndTail(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	workspaceID := store.Snapshot().WorkspaceID

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
	headContent, err := documentContentAtUpdatePostgres(db, workspaceID, head, head.UpdateID)

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
		workspaceID,

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
		workspaceID,

		documentID,
		checkpointUpdateID,
		head.UpdateID,
	).Scan(&tailCount); err != nil {
		t.Fatalf("query tail updates: %v", err)
	}
	if tailCount == 0 {
		t.Fatalf("test setup expected tail updates after checkpoint, checkpoint=%d head=%d", checkpointUpdateID, head.UpdateID)
	}
	workspace := store.Snapshot()
	store, err = NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload store for cold bootstrap: %v", err)
	}
	server := NewServer(Config{}, database)
	broker := server.workspaceBroker(workspaceID)
	room := server.rooms.ForDocument(workspaceID + ":" + documentID)

	clientDoc := crdt.New()
	conn := &DocumentConn{send: make(chan []byte, 128)}
	if err := server.handleDocumentProtocolMessageWithStore(store, broker, room, conn, documentID, buildSyncStep1ForTest(clientDoc), OperationMeta{
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
				if err := server.handleDocumentProtocolMessageWithStore(store, broker, room, conn, documentID, reply, OperationMeta{
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
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	workspaceID := store.Snapshot().WorkspaceID

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
		workspaceID,

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
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	workspaceID := store.Snapshot().WorkspaceID

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
	if got, err := documentContentAtUpdatePostgres(db, workspaceID, updated, updated.UpdateID); err != nil || got != "# before\nmore\n" {

		t.Fatalf("unexpected reconstructed updated content: %q err=%v", got, err)
	}

	var headUpdateID int64
	if err := db.QueryRow(`SELECT update_id FROM document_heads WHERE workspace_id = $1 AND document_id = $2`, workspaceID, documentID).Scan(&headUpdateID); err != nil {

		t.Fatalf("query document head: %v", err)
	}
	if headUpdateID == 0 {
		t.Fatal("expected document head update id")
	}
	assertNoDocumentHeadMaterializationColumns(t, db)

	var updateCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_updates WHERE workspace_id = $1 AND document_id = $2`, workspaceID, documentID).Scan(&updateCount); err != nil {

		t.Fatalf("query document updates: %v", err)
	}
	if updateCount == 0 {
		t.Fatal("expected document update log entry")
	}
}

func TestPostgresAgentInboxTracksDocumentUpdatesAndThreadMentions(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)

	logDocumentID := mustCreateTestDocument(t, store, "agent.log", "start\n")
	normalDocumentID := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
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
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
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

	// The document.updated event also lives in Postgres. Drain it first so
	// the next claim returns the thread.mentioned event.
	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{AgentID: agent.ID, ClaimedBy: "daemon"})
	if err != nil {
		t.Fatalf("claim first event: %v", err)
	}
	if claimed.Type == "document.updated" {
		if _, err := store.UpdateAgentEvent(claimed.ID, UpdateAgentEventRequest{Status: "completed"}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"}); err != nil {
			t.Fatalf("complete document.updated: %v", err)
		}
		claimed, err = store.ClaimAgentEvent(ClaimAgentEventRequest{AgentID: agent.ID, ClaimedBy: "daemon"})
		if err != nil {
			t.Fatalf("claim thread mention after draining document.updated: %v", err)
		}
	}
	if claimed.Type != "thread.mentioned" || claimed.ThreadID != thread.ID || claimed.ThreadMessageID != message.ID {
		t.Fatalf("unexpected claimed log thread mention: %#v", claimed)
	}
	if _, err := store.UpdateAgentEvent(claimed.ID, UpdateAgentEventRequest{Status: "completed"}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("complete claimed log thread mention: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(normalDocumentID, "start\nsemantic update\n", OperationMeta{
		ActorID:   user.ID,
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
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
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
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
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

func TestPostgresPendingAgentEventsSurviveFailedCommit(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	workspaceID := store.Snapshot().WorkspaceID
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "watcher",
		Name:   "Watcher",
		Role:   "Watches documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	dedupKey := "test-survive-failed-commit:" + agent.ID
	now := time.Now().UTC()

	store.mu.Lock()
	store.pendingAgentEvents = []*AgentEvent{{
		ID:          uuid.NewString(),
		AgentID:     agent.ID,
		AgentHandle: agent.Handle,
		Type:        "thread.mentioned",
		Box:         "for_me",
		Status:      "pending",
		DocumentID:  documentID,
		DedupKey:    dedupKey,
		CreatedAt:   now,
		UpdatedAt:   now,
		AvailableAt: now,
	}}
	// Inject an invalid AgentDocumentView to cause upsertAgentDocumentViewsPostgresLocked
	// to fail AFTER flushPendingAgentEventsPostgresLocked succeeds, rolling back the tx.
	bogusViewKey := agentDocumentViewKey(agent.ID, "00000000-0000-0000-0000-000000000000")
	store.state.AgentDocumentViews[bogusViewKey] = &AgentDocumentView{
		AgentID:    agent.ID,
		DocumentID: "00000000-0000-0000-0000-000000000000",
		UpdateID:   1,
		ViewedAt:   now,
	}
	err = store.persistLocked()
	store.mu.Unlock()
	if err == nil {
		t.Fatal("expected persist to fail due to invalid document view FK")
	}

	// Buffer must survive the failed transaction
	store.mu.Lock()
	remaining := len(store.pendingAgentEvents)
	store.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("expected pending event to survive failed persist, got %d", remaining)
	}

	// Postgres must have zero rows for the dedup key (transaction rolled back)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE workspace_id = $1 AND dedup_key = $2`, workspaceID, dedupKey).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 events in Postgres after rollback, got %d", count)
	}

	// Remove the invalid view, retry persist
	store.mu.Lock()
	delete(store.state.AgentDocumentViews, bogusViewKey)
	err = store.persistLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("retry persist: %v", err)
	}

	// Buffer must be cleared after successful commit
	store.mu.Lock()
	remaining = len(store.pendingAgentEvents)
	store.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected buffer cleared after successful retry, got %d pending", remaining)
	}

	// Event must exist exactly once in Postgres
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE workspace_id = $1 AND dedup_key = $2`, workspaceID, dedupKey).Scan(&count); err != nil {
		t.Fatalf("count events after retry: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 event after retry, got %d", count)
	}
}

func TestPostgresDiffDocumentReconstructsAcrossCheckpointsAfterReload(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	workspaceID := store.Snapshot().WorkspaceID
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
	finalContent, err := documentContentAtUpdatePostgres(db, workspaceID, final, finalVersion)

	if err != nil {
		t.Fatalf("reconstruct final document: %v", err)
	}

	workspace := store.Snapshot()
	reloaded, err := NewWorkspaceStore(database, workspace.WorkspaceID, workspace.Name)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}

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
	if err := db.QueryRow(`SELECT COUNT(*) FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, workspaceID, documentID).Scan(&checkpointCount); err != nil {

		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpointCount < 2 {
		t.Fatalf("expected multiple checkpoints after long edit history, got %d", checkpointCount)
	}
}

func TestPostgresCreateAgentValidatesDaemonRuntimeReport(t *testing.T) {
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
			database := newPostgresTestDatabase(t)
			db := database.DB
			store := newPostgresTestWorkspaceStore(t, database)
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

func newPostgresTestDatabase(t *testing.T) *Database {
	t.Helper()
	dsn := postgresTestDSN(t)
	// Truncate stale data before OpenDatabase runs migrations, which fail on leftover rows.
	rawDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw db for pre-clean: %v", err)
	}
	_ = clearNottyTables(rawDB)
	rawDB.Close()

	database, err := OpenDatabase(dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := clearNottyTables(database.DB); err != nil {
		_ = database.Close()
		t.Fatalf("clear tables: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
	})
	return database
}

func insertPostgresTestWorkspace(t *testing.T, database *Database, name string) *Workspace {
	t.Helper()
	if database == nil || database.DB == nil {
		t.Fatal("database is required")
	}
	now := time.Now().UTC()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	workspace := &Workspace{
		ID:             uuid.NewString(),
		Slug:           "test-" + suffix,
		Name:           strings.TrimSpace(name),
		RootDocumentID: newRootDocumentID(),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if workspace.Name == "" {
		workspace.Name = "Test Workspace"
	}
	if _, err := database.DB.Exec(
		`INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		workspace.ID,
		workspace.Slug,
		workspace.Name,
		workspace.RootDocumentID,
		workspace.CreatedAt,
		workspace.UpdatedAt,
	); err != nil {
		t.Fatalf("insert test workspace: %v", err)
	}
	return workspace
}

func newPostgresTestWorkspaceStore(t *testing.T, database *Database) *Store {
	t.Helper()
	workspace := insertPostgresTestWorkspace(t, database, "Test Workspace")
	store, err := NewWorkspaceStore(database, workspace.ID, workspace.Name)
	if err != nil {
		t.Fatalf("new workspace store: %v", err)
	}
	return store
}

func seedTestUser(t *testing.T, store *Store) *User {
	t.Helper()
	now := time.Now().UTC()
	user := &User{
		ID:        uuid.NewString(),
		Handle:    "owner",
		Name:      "Test Owner",
		Role:      "owner",
		Kind:      "human",
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}
	store.mu.Lock()
	store.ensureMaps()
	store.state.Users[user.ID] = user
	err := store.persistLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("persist test user: %v", err)
	}
	return user
}

func seedStoreDaemonRuntime(t *testing.T, store *Store, daemonID string, runtimes ...RuntimeDetection) string {
	t.Helper()
	if store == nil {
		t.Fatal("store is required")
	}
	daemonID = strings.TrimSpace(daemonID)
	if _, err := uuid.Parse(daemonID); err != nil {
		daemonID = uuid.NewString()
	}
	now := time.Now().UTC()
	runtimesJSON, _ := json.Marshal(runtimes)
	tokenHash := "test_token_" + daemonID
	if _, err := store.db.Exec(
		`INSERT INTO daemons (id, workspace_id, name, token_hash, status, daemon_version, os, arch, runtime_detections, last_seen_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, '', '', '', $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			runtime_detections = EXCLUDED.runtime_detections,
			last_seen_at = EXCLUDED.last_seen_at`,
		daemonID, store.workspaceID, "Test daemon", tokenHash, "active", runtimesJSON, now, now,
	); err != nil {
		t.Fatalf("insert test daemon: %v", err)
	}
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
	store.mu.Unlock()
	return daemonID
}

func seedCodexDaemonRuntime(t *testing.T, store *Store) string {
	t.Helper()
	return seedStoreDaemonRuntime(t, store, "", RuntimeDetection{Kind: "codex", Available: true, Version: "codex test"})
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func clearNottyTables(db *sql.DB) error {
	if _, err := db.Exec(`TRUNCATE TABLE
		agent_document_views,
		document_checkpoints,
		document_updates,
		document_heads,
		documents,
		thread_messages,
		thread_participants,
		threads,
		agent_events,
		agent_runs,
		presences,
		activities,
		agents,
		account_email_tokens,
		workspace_invites,
		users,
		daemons,
		workspace_members,
		accounts,
		workspaces
		CASCADE`); err != nil {
		return err
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

func assertNoDocumentPathTitleColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	if columns := documentPathTitleColumns(t, db); len(columns) != 0 {
		t.Fatalf("documents must not retain legacy file-path columns, found: %v", columns)
	}
}

func documentPathTitleColumns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT column_name
		   FROM information_schema.columns
		  WHERE table_name = 'documents'
		    AND column_name IN ('path', 'title')
		  ORDER BY column_name`,
	)
	if err != nil {
		t.Fatalf("query documents legacy columns: %v", err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan documents legacy column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate documents legacy columns: %v", err)
	}
	return columns
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

func TestNewWorkspaceStartsWithNoSyntheticUsers(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	snapshot := store.Snapshot()
	for id, user := range snapshot.Users {
		if id == "user_owner" || (user != nil && user.Handle == "owner" && user.Name == "Workspace Owner") {
			t.Fatalf("workspace should not contain synthetic user_owner, found %q: %+v", id, user)
		}
	}
}

func TestCreateAgentRequiresDaemon(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	_, err := store.CreateAgent(CreateAgentRequest{
		Handle: "test-agent",
		Name:   "Test Agent",
		Role:   "test",
		Kind:   "codex",
	}, OperationMeta{ActorID: "test", ActorType: "human", Source: "test"})
	if err == nil {
		t.Fatalf("expected error when creating agent without any daemons")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("expected daemon-related error, got: %v", err)
	}
	snapshot := store.Snapshot()
	if _, exists := snapshot.Daemons["daemon_local"]; exists {
		t.Fatalf("daemon_local should not be synthesized")
	}
}
