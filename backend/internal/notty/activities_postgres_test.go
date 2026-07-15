package notty

import (
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestActivitiesAPISeamThroughHandlers exercises both activity write paths — the
// walk-buffered path (document/agent/thread creation) and the direct-write path
// (agent session updates) — then asserts the activities surface through the real
// GET /workspace handler: Postgres-backed, newest first, with the expected shape.
func TestActivitiesAPISeamThroughHandlers(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	store := fixture.store
	router := fixture.router
	token := fixture.token
	seedCodexDaemonRuntime(t, store)
	meta := OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "seam-activity-agent",
		Name:   "Seam Activity Agent",
		Role:   "Generates activities",
		Kind:   "codex",
	}, meta)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/activity.md", "start\n")
	if _, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID:    documentID,
		Title:         "Activity thread",
		Body:          "Hey @seam-activity-agent take a look",
		RelativeStart: "start",
		RelativeEnd:   "end",
	}, meta); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Direct-write path: session updates insert their activity directly.
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:    "working",
		SessionID: "sess-1",
	}, OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("update agent session: %v", err)
	}

	var payload map[string]any
	authTestJSON(t, router, http.MethodGet, fixture.workspaceAPIPath("/workspace"), token, nil, http.StatusOK, &payload)
	raw, ok := payload["activities"].([]any)
	if !ok {
		t.Fatalf("expected activities array in workspace response, got %T", payload["activities"])
	}
	if len(raw) == 0 {
		t.Fatal("expected activities in workspace response")
	}

	types := map[string]bool{}
	first := true
	var prev time.Time
	for i, item := range raw {
		evt, _ := item.(map[string]any)
		if evt == nil {
			t.Fatalf("activity %d is not an object: %T", i, item)
		}
		typ, _ := evt["type"].(string)
		if typ == "" {
			t.Fatalf("activity %d missing type: %#v", i, evt)
		}
		if _, ok := evt["actorType"].(string); !ok {
			t.Fatalf("activity %d missing actorType: %#v", i, evt)
		}
		if _, ok := evt["summary"].(string); !ok {
			t.Fatalf("activity %d missing summary: %#v", i, evt)
		}
		occurredRaw, _ := evt["occurredAt"].(string)
		occurred, perr := time.Parse(time.RFC3339, occurredRaw)
		if perr != nil {
			t.Fatalf("activity %d has unparseable occurredAt %q: %v", i, occurredRaw, perr)
		}
		if !first && occurred.After(prev) {
			t.Fatalf("activities not newest-first: %s after %s", occurred, prev)
		}
		prev = occurred
		first = false
		types[typ] = true
	}
	for _, want := range []string{"document.created", "thread.created", "agent.session.updated"} {
		if !types[want] {
			t.Fatalf("expected %q activity in workspace response, got types %v", want, types)
		}
	}
}

// TestActivitiesSurviveStoreRestartPostgres proves the thread-loss class cannot
// apply to activities: they are written through the real product path, the store
// is reopened, and a subsequent write that triggers the full persist walk must
// not clobber the pre-restart activities.
func TestActivitiesSurviveStoreRestartPostgres(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	meta := OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}
	workspaceID := store.WorkspaceID()

	agent, err := store.CreateAgent(CreateAgentRequest{Handle: "restart-agent", Name: "Restart Agent", Role: "durable", Kind: "codex"}, meta)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/restart.md", "start\n")
	if _, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID:    documentID,
		Title:         "Restart thread",
		Body:          "Hey @restart-agent",
		RelativeStart: "s",
		RelativeEnd:   "e",
	}, meta); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{Status: "working"}, OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("update session: %v", err)
	}

	before, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list before restart: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("expected activities before restart")
	}
	beforeTypes := activityTypeCounts(before)

	reloaded, err := NewWorkspaceStore(database, workspaceID, store.WorkspaceName())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}

	after, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list after restart: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("expected %d activities after restart, got %d", len(before), len(after))
	}

	// A post-restart write that runs the full persist walk must leave the
	// pre-restart activities intact and only add its own.
	if _, err := reloaded.CreateAgent(CreateAgentRequest{Handle: "restart-agent-2", Name: "Restart Agent 2", Role: "durable", Kind: "codex"}, meta); err != nil {
		t.Fatalf("create agent after restart: %v", err)
	}
	final, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list after post-restart write: %v", err)
	}
	if len(final) != len(before)+1 {
		t.Fatalf("expected %d activities after post-restart write, got %d", len(before)+1, len(final))
	}
	finalTypes := activityTypeCounts(final)
	for typ, n := range beforeTypes {
		if finalTypes[typ] < n {
			t.Fatalf("activity type %q lost across restart: before %d after %d", typ, n, finalTypes[typ])
		}
	}
}

// TestUpdateAgentSessionAcceptsFailedStatusPostgres proves the daemon's item-#3
// terminal classification can publish `failed` through the real store write path
// (not just the driver unit) — the status is accepted, carries the provider's
// line, and persists across a re-read.
func TestUpdateAgentSessionAcceptsFailedStatusPostgres(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	meta := OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}

	agent, err := store.CreateAgent(CreateAgentRequest{Handle: "failing-agent", Name: "Failing Agent", Role: "durable", Kind: "codex"}, meta)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agentMeta := OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}

	updated, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:          "failed",
		CurrentActivity: "The model is no longer available.",
	}, agentMeta)
	if err != nil {
		t.Fatalf("update session to failed: %v", err)
	}
	if updated.Status != "failed" {
		t.Fatalf("status = %q, want failed", updated.Status)
	}
	if updated.CurrentActivity != "The model is no longer available." {
		t.Fatalf("activity = %q, want the published provider line", updated.CurrentActivity)
	}

	// A second update re-reads current DB state (getAgentForUpdatePostgres), so a
	// bare heartbeat proves `failed` persisted rather than only round-tripping.
	reloaded, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, agentMeta)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if reloaded.Status != "failed" {
		t.Fatalf("reloaded status = %q, want failed to persist", reloaded.Status)
	}
}

// TestUpdateAgentSessionAcceptsStalledStatusPostgres proves the daemon's item #5
// stalled-liveness status is accepted through the real publish/store path (not
// bounced as an unsupported status) and persists, carrying its diagnostic.
func TestUpdateAgentSessionAcceptsStalledStatusPostgres(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	meta := OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}

	agent, err := store.CreateAgent(CreateAgentRequest{Handle: "stalled-agent", Name: "Stalled Agent", Role: "durable", Kind: "codex"}, meta)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agentMeta := OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}

	updated, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:          "stalled",
		CurrentActivity: "Stalled: no runtime activity for 15m0s during turn turn_1",
	}, agentMeta)
	if err != nil {
		t.Fatalf("update session to stalled: %v", err)
	}
	if updated.Status != "stalled" {
		t.Fatalf("status = %q, want stalled", updated.Status)
	}
	if updated.CurrentActivity != "Stalled: no runtime activity for 15m0s during turn turn_1" {
		t.Fatalf("activity = %q, want the published stall diagnostic", updated.CurrentActivity)
	}

	// A second update re-reads current DB state, so a bare heartbeat proves
	// `stalled` persisted rather than only round-tripping.
	reloaded, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, agentMeta)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if reloaded.Status != "stalled" {
		t.Fatalf("reloaded status = %q, want stalled to persist", reloaded.Status)
	}
}

// TestUpdateAgentSessionHeartbeatOnlyProducesNoActivityAndPreservesFields proves
// item #5 blockers 6/15/16: the daemon's 60s heartbeat republish (which advances
// LastHeartbeatAt but changes no semantic field) must NOT create an activity row —
// activities are never trimmed — and a heartbeat-only update must PRESERVE the
// status, turn, and stalled diagnostic; a real transition still emits exactly one.
func TestUpdateAgentSessionHeartbeatOnlyProducesNoActivityAndPreservesFields(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	workspaceID := store.WorkspaceID()
	meta := OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}

	agent, err := store.CreateAgent(CreateAgentRequest{Handle: "hb-agent", Name: "HB Agent", Role: "durable", Kind: "codex"}, meta)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agentMeta := OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}
	diagnostic := "Stalled: no runtime activity for 15m0s during turn turn_1"
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{Status: "stalled", CurrentTurnID: "turn_1", CurrentActivity: diagnostic}, agentMeta); err != nil {
		t.Fatalf("stall transition: %v", err)
	}
	baseline, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list baseline: %v", err)
	}

	// Two heartbeat-only updates (LastHeartbeatAt only): no status/turn/activity.
	for i := 0; i < 2; i++ {
		updated, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano)}, agentMeta)
		if err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		if updated.Status != "stalled" || updated.CurrentTurnID != "turn_1" || updated.CurrentActivity != diagnostic {
			t.Fatalf("heartbeat-only must preserve status/turn/diagnostic, got status=%q turn=%q activity=%q", updated.Status, updated.CurrentTurnID, updated.CurrentActivity)
		}
	}
	afterHeartbeats, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list after heartbeats: %v", err)
	}
	if len(afterHeartbeats) != len(baseline) {
		t.Fatalf("heartbeat-only updates must create no activity rows: baseline=%d after=%d", len(baseline), len(afterHeartbeats))
	}

	// Blocker 20: the same-status path (the daemon re-sending Status:"stalled" with
	// omitted turn/activity, as the stalled heartbeat does) is NOT a transition — it
	// must preserve the turn + diagnostic and create no activity row.
	for i := 0; i < 2; i++ {
		updated, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{Status: "stalled", LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano)}, agentMeta)
		if err != nil {
			t.Fatalf("same-status stalled heartbeat %d: %v", i, err)
		}
		if updated.Status != "stalled" || updated.CurrentTurnID != "turn_1" || updated.CurrentActivity != diagnostic {
			t.Fatalf("same-status stalled heartbeat must preserve turn/diagnostic, got status=%q turn=%q activity=%q", updated.Status, updated.CurrentTurnID, updated.CurrentActivity)
		}
	}
	afterSameStatus, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list after same-status: %v", err)
	}
	if len(afterSameStatus) != len(baseline) {
		t.Fatalf("same-status stalled heartbeats must create no activity rows: baseline=%d after=%d", len(baseline), len(afterSameStatus))
	}

	// A real status transition still emits exactly one activity.
	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{Status: "idle"}, agentMeta); err != nil {
		t.Fatalf("idle transition: %v", err)
	}
	afterTransition, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list after transition: %v", err)
	}
	if len(afterTransition) != len(baseline)+1 {
		t.Fatalf("a real transition must emit exactly one activity: baseline=%d after=%d", len(baseline), len(afterTransition))
	}
}

// TestActivitiesConcurrentWritesPostgres fires many activity-producing
// operations concurrently and asserts each lands exactly one row — no lost or
// duplicated activities from the shared pending buffer and post-commit clear.
func TestActivitiesConcurrentWritesPostgres(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	user := seedTestUser(t, store)
	workspaceID := store.WorkspaceID()

	before := activityCount(t, db, workspaceID)

	const goroutines = 12
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			// CreateDocument buffers its document.created activity and flushes it
			// inside the same persist transaction; each op must land exactly one.
			if _, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create document: %v", err)
		}
	}

	if got := activityCount(t, db, workspaceID) - before; got != goroutines {
		t.Fatalf("expected %d new activities from %d concurrent ops, got %d", goroutines, goroutines, got)
	}
	if got := countActivityType(t, db, workspaceID, "document.created"); got != goroutines {
		t.Fatalf("expected exactly %d document.created activities, got %d", goroutines, got)
	}
}

// TestActivitiesReadWindowPreservedPostgres is the task #16 window guarantee:
// with more than the read window of activities in the table, the read returns
// exactly the newest 100 (ordered newest-first) while every row stays in
// Postgres — the memory-window cap never deletes anything.
func TestActivitiesReadWindowPreservedPostgres(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	workspaceID := store.WorkspaceID()

	agent, err := store.CreateAgent(CreateAgentRequest{Handle: "window-agent", Name: "Window Agent", Role: "noisy", Kind: "codex"}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	baseline := activityCount(t, db, workspaceID)
	const extra = 120
	for i := 0; i < extra; i++ {
		// Each update carries a DISTINCT semantic field (CurrentActivity) so it is a
		// real session change that produces one activity — a heartbeat-only or
		// unchanged repeat is intentionally suppressed now (item #5 blocker 6).
		if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{Status: "working", CurrentActivity: fmt.Sprintf("Working step %d", i)}, OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}); err != nil {
			t.Fatalf("update session %d: %v", i, err)
		}
	}

	total := activityCount(t, db, workspaceID)
	if got := total - baseline; got != extra {
		t.Fatalf("expected %d activities inserted with no trimming, got %d", extra, got)
	}
	if total <= 100 {
		t.Fatalf("test needs >100 activities to prove windowing, got %d", total)
	}

	window, err := listActivitiesPostgres(db, workspaceID)
	if err != nil {
		t.Fatalf("list activities: %v", err)
	}
	if len(window) != 100 {
		t.Fatalf("expected read window of 100, got %d (of %d rows)", len(window), total)
	}
	for i := 1; i < len(window); i++ {
		if window[i-1].ID <= window[i].ID {
			t.Fatalf("activities not newest-first at index %d: id %d then %d", i, window[i-1].ID, window[i].ID)
		}
	}
	var maxID int64
	if err := db.QueryRow(`SELECT max(id) FROM activities WHERE workspace_id = $1::uuid`, workspaceID).Scan(&maxID); err != nil {
		t.Fatalf("max id: %v", err)
	}
	if window[0].ID != maxID {
		t.Fatalf("expected window to start at newest id %d, got %d", maxID, window[0].ID)
	}
}

func activityCount(t *testing.T, db *sql.DB, workspaceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM activities WHERE workspace_id = $1::uuid`, workspaceID).Scan(&n); err != nil {
		t.Fatalf("count activities: %v", err)
	}
	return n
}

func countActivityType(t *testing.T, db *sql.DB, workspaceID, typ string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM activities WHERE workspace_id = $1::uuid AND type = $2`, workspaceID, typ).Scan(&n); err != nil {
		t.Fatalf("count activities of type %s: %v", typ, err)
	}
	return n
}

func activityTypeCounts(activities []*ActivityEvent) map[string]int {
	counts := map[string]int{}
	for _, activity := range activities {
		counts[activity.Type]++
	}
	return counts
}
