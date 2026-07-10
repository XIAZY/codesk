package notty

import (
	"errors"
	"testing"
	"time"
)

// Task #6: document subscribers are notified of thread activity. A new thread is a one-shot fact (general,
// instant, doorbell); replies are a recurring stream to a watcher (general, coalesced, NO doorbell); being
// subscribed by someone else is a directed fact (for_me, instant, doorbell). Author + already-mentioned/
// participant recipients are excluded so no one is double-carded. These are red-first: they assert the
// desired routing, failing on a tree without the collects.

// agentEventsByType reads persisted agent_events of a type for an agent directly, bypassing the maturity
// filter (watcher reply cards are future-dated and would be hidden by ListAgentInbox until they mature).
func agentEventsByType(t *testing.T, store *Store, agentID, eventType string) []*AgentEvent {
	t.Helper()
	rows, err := store.db.Query(
		`SELECT id::text, box, dedup_key, COALESCE(thread_id::text, ''), summary, available_at, created_at
		   FROM agent_events
		  WHERE workspace_id = $1::uuid AND agent_id = $2::uuid AND type = $3
		  ORDER BY created_at, dedup_key`,
		store.state.WorkspaceID, agentID, eventType)
	if err != nil {
		t.Fatalf("query %s events: %v", eventType, err)
	}
	defer rows.Close()
	var out []*AgentEvent
	for rows.Next() {
		event := &AgentEvent{Type: eventType, AgentID: agentID}
		if err := rows.Scan(&event.ID, &event.Box, &event.DedupKey, &event.ThreadID, &event.Summary, &event.AvailableAt, &event.CreatedAt); err != nil {
			t.Fatalf("scan %s: %v", eventType, err)
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", eventType, err)
	}
	return out
}

func doorbellsByType(store *Store, agentID, eventType string) int {
	n := 0
	for _, change := range store.DrainAgentInboxChanges() {
		if change.AgentID == agentID && change.NotificationType == eventType {
			n++
		}
	}
	return n
}

func humanMeta(user *User) OperationMeta {
	return OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}
}
func agentMeta(agent *Agent) OperationMeta {
	return OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"}
}

func TestThreadCreatedCardsSubscribersExcludingAuthorAndMentioned(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	author := mustCreateInboxTestAgent(t, store, user, "author")
	watcher := mustCreateInboxTestAgent(t, store, user, "watcher")
	mentioned := mustCreateInboxTestAgent(t, store, user, "mentioned")
	bystander := mustCreateInboxTestAgent(t, store, user, "bystander")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	for _, a := range []*Agent{author, watcher, mentioned} {
		if err := store.SubscribeAgentDocument(a.ID, doc); err != nil {
			t.Fatalf("subscribe %s: %v", a.Handle, err)
		}
	}
	store.DrainAgentInboxChanges()

	// The subscribed agent "author" creates a thread mentioning subscribed agent "mentioned".
	if _, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: doc, Title: "Design", Body: "kickoff @mentioned please review", RelativeStart: "s", RelativeEnd: "e",
	}, agentMeta(author)); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// watcher: one general thread.created card + one doorbell.
	if cards := agentEventsByType(t, store, watcher.ID, "thread.created"); len(cards) != 1 || normalizeInboxBox(cards[0].Box) != "general" {
		t.Fatalf("watcher: want 1 general thread.created, got %#v", cards)
	}
	// author excluded (it wrote the thread); non-subscriber excluded.
	if cards := agentEventsByType(t, store, author.ID, "thread.created"); len(cards) != 0 {
		t.Fatalf("author must not be carded, got %d", len(cards))
	}
	if cards := agentEventsByType(t, store, bystander.ID, "thread.created"); len(cards) != 0 {
		t.Fatalf("non-subscriber must not be carded, got %d", len(cards))
	}
	// mentioned subscriber gets ONE card, and it's the for_me mention (mention wins), not a general watcher card.
	if cards := agentEventsByType(t, store, mentioned.ID, "thread.created"); len(cards) != 0 {
		t.Fatalf("mentioned subscriber must not also get a thread.created card, got %d", len(cards))
	}
	if cards := agentEventsByType(t, store, mentioned.ID, "thread.mentioned"); len(cards) != 1 || normalizeInboxBox(cards[0].Box) != "for_me" {
		t.Fatalf("mentioned subscriber must get the for_me mention, got %#v", cards)
	}
	// Doorbells: watcher rung once for the instant thread.created; author never.
	changes := map[string]int{}
	for _, change := range store.DrainAgentInboxChanges() {
		if change.NotificationType == "thread.created" {
			changes[change.AgentID]++
		}
	}
	if changes[watcher.ID] != 1 {
		t.Fatalf("watcher must be rung once for thread.created, got %d", changes[watcher.ID])
	}
	if changes[author.ID] != 0 || changes[bystander.ID] != 0 {
		t.Fatalf("author/bystander must not be rung, got %d/%d", changes[author.ID], changes[bystander.ID])
	}
}

func TestThreadCreatedReplayDoesNotDoubleCard(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	watcher := mustCreateInboxTestAgent(t, store, user, "watcher")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	if err := store.SubscribeAgentDocument(watcher.ID, doc); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	store.DrainAgentInboxChanges()

	// Same client operation id → the second create is a replay of the first, not a new thread.
	req := CreateThreadRequest{DocumentID: doc, Title: "T", Body: "hello", RelativeStart: "s", RelativeEnd: "e", ClientOperationID: "create-op-1"}
	if _, _, created, err := store.CreateThread(req, humanMeta(user)); err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	if _, _, created, err := store.CreateThread(req, humanMeta(user)); err != nil || created {
		t.Fatalf("replay create must be a no-op: created=%v err=%v", created, err)
	}

	// Exactly one card and one doorbell total — asserted against the counters, not absence.
	if cards := agentEventsByType(t, store, watcher.ID, "thread.created"); len(cards) != 1 {
		t.Fatalf("replay must not double-card: want 1, got %d", len(cards))
	}
	if n := doorbellsByType(store, watcher.ID, "thread.created"); n != 1 {
		t.Fatalf("replay must ring exactly one doorbell total, got %d", n)
	}
}

func TestThreadRepliedWatcherSlidesAvailableAtForward(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	watcher := mustCreateInboxTestAgent(t, store, user, "watcher")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	if err := store.SubscribeAgentDocument(watcher.ID, doc); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	thread, _, _, err := store.CreateThread(CreateThreadRequest{DocumentID: doc, Title: "Q", Body: "?", RelativeStart: "s", RelativeEnd: "e"}, humanMeta(user))
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	store.DrainAgentInboxChanges()

	// First reply establishes the watcher card; capture its availability.
	if _, _, err := store.ReplyThread(thread.ID, ReplyThreadRequest{Body: "one"}, humanMeta(user)); err != nil {
		t.Fatalf("reply one: %v", err)
	}
	first := agentEventsByType(t, store, watcher.ID, "thread.replied")
	if len(first) != 1 {
		t.Fatalf("want 1 watcher card after reply one, got %d", len(first))
	}
	firstAvailable := first[0].AvailableAt

	// Second reply must SLIDE the same row's availability forward (coalescing), not add a row or ring.
	if _, _, err := store.ReplyThread(thread.ID, ReplyThreadRequest{Body: "two"}, humanMeta(user)); err != nil {
		t.Fatalf("reply two: %v", err)
	}
	second := agentEventsByType(t, store, watcher.ID, "thread.replied")
	if len(second) != 1 {
		t.Fatalf("second reply must coalesce onto one row, got %d", len(second))
	}
	if !second[0].AvailableAt.After(firstAvailable) {
		t.Fatalf("second reply must slide available_at forward: first=%v second=%v", firstAvailable, second[0].AvailableAt)
	}
	if n := doorbellsByType(store, watcher.ID, "thread.replied"); n != 0 {
		t.Fatalf("coalesced watcher replies must ring zero doorbells, got %d", n)
	}
}

func TestSubscriptionAddedCardFailureRollsBackSubscription(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")

	// Poison the card upsert to simulate a transient failure mid-transaction.
	testHookSubscriptionCardUpsert = func(querier, string, *AgentEvent) (*AgentEvent, error) {
		return nil, errors.New("poisoned card write")
	}
	defer func() { testHookSubscriptionCardUpsert = nil }()

	if _, err := store.SubscribeAgentDocumentAndNotify(agent.ID, doc, humanMeta(user)); err == nil {
		t.Fatalf("expected the card failure to surface an error")
	}

	// All-or-nothing: the subscription must NOT have persisted (rolled back with the card), and no card exists.
	ids, err := listAgentDocumentSubscriptionsPostgres(store.db, store.state.WorkspaceID, agent.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a card-write failure must leave no orphaned subscription, got %v", ids)
	}
	if cards := agentEventsByType(t, store, agent.ID, "subscription.added"); len(cards) != 0 {
		t.Fatalf("no card should persist on failure, got %d", len(cards))
	}

	// Recovery: with the seam cleared, a retry now subscribes and cards cleanly.
	testHookSubscriptionCardUpsert = nil
	if inserted, err := store.SubscribeAgentDocumentAndNotify(agent.ID, doc, humanMeta(user)); err != nil || !inserted {
		t.Fatalf("retry after the transient failure must succeed: inserted=%v err=%v", inserted, err)
	}
	if cards := agentEventsByType(t, store, agent.ID, "subscription.added"); len(cards) != 1 {
		t.Fatalf("retry must card once, got %d", len(cards))
	}
}

func TestThreadRepliedWatcherCoalescesWithoutDoorbell(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	watcher := mustCreateInboxTestAgent(t, store, user, "watcher")
	participant := mustCreateInboxTestAgent(t, store, user, "participant")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	for _, a := range []*Agent{watcher, participant} {
		if err := store.SubscribeAgentDocument(a.ID, doc); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
	}
	// The human opens a thread that makes "participant" a participant (mention); "watcher" stays a pure watcher.
	thread, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: doc, Title: "Q", Body: "@participant thoughts?", RelativeStart: "s", RelativeEnd: "e",
	}, humanMeta(user))
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	store.DrainAgentInboxChanges()

	// Two replies by the human.
	for i := 0; i < 2; i++ {
		if _, _, err := store.ReplyThread(thread.ID, ReplyThreadRequest{Body: "more"}, humanMeta(user)); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}

	// watcher: exactly ONE coalesced general card (dedup per thread+agent), matured ~60s out, NO doorbell.
	cards := agentEventsByType(t, store, watcher.ID, "thread.replied")
	if len(cards) != 1 || normalizeInboxBox(cards[0].Box) != "general" {
		t.Fatalf("watcher: want 1 coalesced general thread.replied, got %#v", cards)
	}
	if gap := cards[0].AvailableAt.Sub(cards[0].CreatedAt); gap < 55*time.Second || gap > 65*time.Second {
		t.Fatalf("watcher card should mature ~60s out (coalesced), got %v", gap)
	}
	if n := doorbellsByType(store, watcher.ID, "thread.replied"); n != 0 {
		t.Fatalf("watcher must get ZERO doorbells for coalesced replies, got %d", n)
	}
	// participant keeps the instant for_me card per reply (existing behavior, unaffected).
	if cards := agentEventsByType(t, store, participant.ID, "thread.replied"); len(cards) == 0 {
		t.Fatalf("participant must still receive instant for_me thread.replied cards")
	} else {
		for _, c := range cards {
			if normalizeInboxBox(c.Box) != "for_me" {
				t.Fatalf("participant reply card must be for_me, got %q", c.Box)
			}
		}
	}
}

func TestSubscriptionAddedCardOnlyWhenAddedByAnotherHuman(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\n")
	store.DrainAgentInboxChanges()

	// A human subscribes the agent (Participants panel) → one for_me card + one doorbell.
	inserted, err := store.SubscribeAgentDocumentAndNotify(agent.ID, doc, humanMeta(user))
	if err != nil || !inserted {
		t.Fatalf("human subscribe: inserted=%v err=%v", inserted, err)
	}
	cards := agentEventsByType(t, store, agent.ID, "subscription.added")
	if len(cards) != 1 || normalizeInboxBox(cards[0].Box) != "for_me" {
		t.Fatalf("want 1 for_me subscription.added card, got %#v", cards)
	}
	if n := doorbellsByType(store, agent.ID, "subscription.added"); n != 1 {
		t.Fatalf("want 1 doorbell, got %d", n)
	}

	// Idempotent re-subscribe → no new card.
	if inserted, err := store.SubscribeAgentDocumentAndNotify(agent.ID, doc, humanMeta(user)); err != nil || inserted {
		t.Fatalf("re-subscribe should be a no-op insert: inserted=%v err=%v", inserted, err)
	}
	if cards := agentEventsByType(t, store, agent.ID, "subscription.added"); len(cards) != 1 {
		t.Fatalf("re-subscribe must not double-card, got %d", len(cards))
	}

	// Self-subscribe via the tool (agent principal) → no card.
	other := mustCreateInboxTestAgent(t, store, user, "self-subscriber")
	if _, err := store.SubscribeAgentDocumentAndNotify(other.ID, doc, agentMeta(other)); err != nil {
		t.Fatalf("self-subscribe: %v", err)
	}
	if cards := agentEventsByType(t, store, other.ID, "subscription.added"); len(cards) != 0 {
		t.Fatalf("self-subscribe must card no one, got %d", len(cards))
	}
}

func TestSubscriptionAddedCompletionDoesNotAdvanceDocumentWatermark(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	agent := mustCreateInboxTestAgent(t, store, user, "reviewer")
	doc := mustCreateTestDocument(t, store, "docs/spec.md", "start\nedit\n")

	if _, err := store.SubscribeAgentDocumentAndNotify(agent.ID, doc, humanMeta(user)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	cards := agentEventsByType(t, store, agent.ID, "subscription.added")
	if len(cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(cards))
	}
	// Completing it must NOT advance the document watermark — being told you're subscribed is not reviewing
	// the doc. The `subscription.added` type escapes the `document.`-prefix watermark rule by name.
	if _, err := store.UpdateAgentEvent(cards[0].ID, UpdateAgentEventRequest{Status: "completed"}, humanMeta(user)); err != nil {
		t.Fatalf("complete card: %v", err)
	}
	view, err := getAgentDocumentViewPostgres(store.db, store.state.WorkspaceID, agent.ID, doc)
	if err != nil && err != ErrNotFound {
		t.Fatalf("get view: %v", err)
	}
	if view != nil && view.UpdateID > 0 {
		t.Fatalf("subscription.added completion must not advance the watermark, got updateId=%d", view.UpdateID)
	}
}
