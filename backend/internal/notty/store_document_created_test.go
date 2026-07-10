package notty

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Task #3 (AlphaToad ruling): a NEW document is a rare, high-signal event that DOES push — a one-shot
// `document.created` card to every workspace agent except the creator, in the general box. This is the
// deliberate exception to muted-by-default (edits stay muted unless subscribed). These tests are written
// red-first: they assert the desired create-push behavior, so they fail on a tree without the emission and
// turn green only once CreateDocument emits the cards.
//
// The cards are asserted by reading `agent_events` directly, because they ride the SAME poll+maturity class
// as subscribed edits (available_at = now + window, no doorbell) — so ListAgentInbox deliberately hides them
// until they mature, and the whole point of these tests is the persisted row's existence and shape.

func mustCreateInboxTestAgent(t *testing.T, store *Store, user *User, handle string) *Agent {
	t.Helper()
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: handle, Name: handle, Role: "task #3 subject", Kind: "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent %q: %v", handle, err)
	}
	return agent
}

// documentCreatedCards reads persisted document.created rows for an agent directly, bypassing ListAgentInbox's
// maturity filter (a fresh card is dated now+window and is hidden until it matures).
func documentCreatedCards(t *testing.T, store *Store, agentID string) []*AgentEvent {
	t.Helper()
	rows, err := store.db.Query(
		`SELECT id::text, box, dedup_key, from_update_id, to_update_id, summary, available_at, created_at
		   FROM agent_events
		  WHERE workspace_id = $1::uuid AND agent_id = $2::uuid AND type = 'document.created'
		  ORDER BY created_at, dedup_key`,
		store.state.WorkspaceID, agentID)
	if err != nil {
		t.Fatalf("query created cards: %v", err)
	}
	defer rows.Close()
	var out []*AgentEvent
	for rows.Next() {
		event := &AgentEvent{Type: "document.created", AgentID: agentID}
		if err := rows.Scan(&event.ID, &event.Box, &event.DedupKey, &event.FromUpdateID, &event.ToUpdateID, &event.Summary, &event.AvailableAt, &event.CreatedAt); err != nil {
			t.Fatalf("scan created card: %v", err)
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("created cards rows: %v", err)
	}
	return out
}

func TestDocumentCreatedPushesGeneralCardToNonCreators(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agentA := mustCreateInboxTestAgent(t, store, user, "reviewer-a")
	agentB := mustCreateInboxTestAgent(t, store, user, "reviewer-b")
	store.DrainAgentInboxChanges()

	doc, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	for _, agentID := range []string{agentA.ID, agentB.ID} {
		cards := documentCreatedCards(t, store, agentID)
		if len(cards) != 1 {
			t.Fatalf("agent %s: want exactly 1 document.created card, got %d", agentID, len(cards))
		}
		card := cards[0]
		if normalizeInboxBox(card.Box) != "general" {
			t.Fatalf("document.created must land in general, got %q", card.Box)
		}
		if want := "document-created:" + doc.ID + ":" + agentID; card.DedupKey != want {
			t.Fatalf("dedup key = %q, want %q", card.DedupKey, want)
		}
		if card.ToUpdateID != doc.UpdateID {
			t.Fatalf("to_update_id = %d, want the created version %d (watermark advance depends on it)", card.ToUpdateID, doc.UpdateID)
		}
		// Instant delivery (AlphaToad's ruling): available immediately, no quiescence window.
		if gap := card.AvailableAt.Sub(card.CreatedAt); gap > time.Second {
			t.Fatalf("created card should be available instantly, got a %v delay", gap)
		}
	}

	// Instant wake: creation rings exactly one document.created doorbell per carded agent (general box).
	doorbells := map[string]int{}
	for _, change := range store.DrainAgentInboxChanges() {
		if change.NotificationType != "document.created" {
			continue
		}
		if normalizeInboxBox(change.Box) != "general" {
			t.Fatalf("document.created doorbell must be general, got %q", change.Box)
		}
		doorbells[change.AgentID]++
	}
	for _, agentID := range []string{agentA.ID, agentB.ID} {
		if doorbells[agentID] != 1 {
			t.Fatalf("agent %s: want exactly 1 document.created doorbell, got %d", agentID, doorbells[agentID])
		}
	}
}

func TestDocumentCreatedExcludesTheCreator(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	creator := mustCreateInboxTestAgent(t, store, user, "creator")
	bystander := mustCreateInboxTestAgent(t, store, user, "bystander")

	// The document is created BY an agent, via the tool surface (ActorType "agent").
	doc, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: creator.ID, ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	if cards := documentCreatedCards(t, store, creator.ID); len(cards) != 0 {
		t.Fatalf("the creating agent must not card itself, got %d cards", len(cards))
	}
	if cards := documentCreatedCards(t, store, bystander.ID); len(cards) != 1 {
		t.Fatalf("a non-creator agent must be carded once, got %d", len(cards))
	}
	// The doorbell follows the card: the creator is never woken, the bystander is woken once.
	doorbells := map[string]int{}
	for _, change := range store.DrainAgentInboxChanges() {
		if change.NotificationType == "document.created" {
			doorbells[change.AgentID]++
		}
	}
	if doorbells[creator.ID] != 0 {
		t.Fatalf("the creator must not be woken, got %d doorbells", doorbells[creator.ID])
	}
	if doorbells[bystander.ID] != 1 {
		t.Fatalf("the bystander must be woken once, got %d doorbells", doorbells[bystander.ID])
	}
	_ = doc
}

func TestDocumentCreatedIsIdempotentOnReplay(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")

	req := CreateDocumentRequest{DocumentID: uuid.NewString(), ClientOperationID: "create-op-1"}
	first, err := store.CreateDocument(req, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Drain the first create's doorbell so the replay assertion sees only the replay's effect.
	store.DrainAgentInboxChanges()
	// Same document id + client operation → the replay guard returns the existing doc before any emission.
	second, err := store.CreateDocument(req, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("replay create: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay must return the same document, got %s vs %s", second.ID, first.ID)
	}
	if cards := documentCreatedCards(t, store, agent.ID); len(cards) != 1 {
		t.Fatalf("idempotent replay must not double-card: want 1, got %d", len(cards))
	}
	// A replay re-cards no one and so wakes no one.
	for _, change := range store.DrainAgentInboxChanges() {
		if change.NotificationType == "document.created" {
			t.Fatalf("idempotent replay must ring no doorbell, got %#v", change)
		}
	}
}

func TestDocumentCreatedHiddenEmitsNothing(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")

	// A hidden document must never notify — CreateDocument can't produce one, so pin the guard directly.
	hidden := &Document{ID: uuid.NewString(), Hidden: true, UpdateID: 7}
	events := store.enqueueDocumentCreatedInboxEventsLocked(store.db, hidden, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if len(events) != 0 {
		t.Fatalf("hidden document must emit zero created cards, got %d", len(events))
	}
	if cards := documentCreatedCards(t, store, agent.ID); len(cards) != 0 {
		t.Fatalf("hidden document must leave no rows, got %d", len(cards))
	}
}

func TestDocumentCreatedEmissionFailureIsBestEffort(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	_ = mustCreateInboxTestAgent(t, store, user, "reviewer")

	// A closed connection stands in for the emission's backend being unavailable right after the doc commits.
	// The contract: emission is best-effort, so this must neither panic nor return an error (the helper's
	// signature carries none) — which is exactly why CreateDocument, ignoring the result, always returns the
	// committed document even when card writes fail. The un-carded agent falls back to its muted version gap.
	broken, err := sql.Open("pgx", postgresTestDSN(t))
	if err != nil {
		t.Fatalf("open broken db: %v", err)
	}
	if err := broken.Close(); err != nil {
		t.Fatalf("close broken db: %v", err)
	}

	events := store.enqueueDocumentCreatedInboxEventsLocked(broken, &Document{ID: uuid.NewString(), UpdateID: 5}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if len(events) != 0 {
		t.Fatalf("a failed emission must produce no cards, got %d", len(events))
	}
}

func TestDocumentCreatedBurstRingsPerCreationDoorbells(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")
	store.DrainAgentInboxChanges()

	// A directory push / initial sync: N documents created in a burst.
	const burst = 3
	for i := 0; i < burst; i++ {
		if _, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
			t.Fatalf("create document %d: %v", i, err)
		}
	}

	if cards := documentCreatedCards(t, store, agent.ID); len(cards) != burst {
		t.Fatalf("burst of %d creations must yield %d rows, got %d", burst, burst, len(cards))
	}
	// Instant delivery (AlphaToad's ruling): the store rings one doorbell per creation. The burst is bounded
	// to one TURN per agent downstream by the daemon's busy-path one-slot follow-up (and the wake prompt's
	// top-3+overflow cap) — that collapse is a supervisor property, not the store's; the store fires N.
	doorbells := 0
	for _, change := range store.DrainAgentInboxChanges() {
		if change.NotificationType == "document.created" && change.AgentID == agent.ID {
			doorbells++
		}
	}
	if doorbells != burst {
		t.Fatalf("a burst of %d creations must ring %d instant doorbells, got %d", burst, burst, doorbells)
	}
}

func TestDocumentCreatedCompletionAdvancesWatermark(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")

	doc, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	cards := documentCreatedCards(t, store, agent.ID)
	if len(cards) != 1 {
		t.Fatalf("want 1 created card, got %d", len(cards))
	}

	// Completing the created card advances the document watermark (shouldMarkDocumentViewedForEvent matches
	// document.*), so handling it also clears the document's muted gap.
	if _, err := store.UpdateAgentEvent(cards[0].ID, UpdateAgentEventRequest{Status: "completed"}, OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("complete created card: %v", err)
	}
	view, err := getAgentDocumentViewPostgres(store.db, store.state.WorkspaceID, agent.ID, doc.ID)
	if err != nil {
		t.Fatalf("get document view: %v", err)
	}
	if view == nil || view.UpdateID != doc.UpdateID {
		t.Fatalf("completing the created card must advance the watermark to %d, got %#v", doc.UpdateID, view)
	}
}

func TestDocumentCreatedDoesNotReopenMutedEditPush(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")

	doc, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	// The agent is NOT subscribed. A subsequent edit must stay muted — creation is the only push, edits are
	// still subscription-gated (the core task #2 invariant is intact).
	if _, _, err := store.ReplaceDocumentText(doc.ID, "edited after creation\n", OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("edit document: %v", err)
	}
	rows, err := store.db.Query(
		`SELECT count(*) FROM agent_events
		  WHERE workspace_id = $1::uuid AND agent_id = $2::uuid AND type = 'document.updated' AND box = 'general'`,
		store.state.WorkspaceID, agent.ID)
	if err != nil {
		t.Fatalf("count updated cards: %v", err)
	}
	defer rows.Close()
	var count int
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			t.Fatalf("scan count: %v", err)
		}
	}
	if count != 0 {
		t.Fatalf("an edit to an unsubscribed document must push no general document.updated card, got %d", count)
	}
	// The creation card itself still stands.
	if cards := documentCreatedCards(t, store, agent.ID); len(cards) != 1 {
		t.Fatalf("the document.created card must remain, got %d", len(cards))
	}
}
