package notty

import (
	"testing"
	"time"
)

// Boundary #1 (task #2, the feature's entire point): a document edit with NO subscribers must be muted —
// it produces zero inbox rows and rings zero doorbells for any non-subscribing agent. This is the exact
// reproduction of what #agent-notification-efficiency root-caused: today every keystroke batch upserts an
// inbox row per agent and broadcasts an inbox-changed doorbell per agent, waking everyone.
//
// Written red-first per the house rule: it asserts the DESIRED muted behavior, so it FAILS on current main
// (which pushes to every agent) and turns green only once the routing change makes document delivery
// subscription-gated. Broker emission-count is captured off DrainAgentInboxChanges (the store's inbox-change
// fan-out that becomes the per-agent broadcast). The daemon-supervisor "no turn" half of this boundary is a
// separate seam test.
func TestMutedDocumentUpdateDoesNotPushToNonSubscriber(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/muted.md", "start\n")

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "watcher",
		Name:   "Watcher",
		Role:   "Should hear nothing about unsubscribed documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// Clear any inbox-change fan-out produced by setup so the assertion sees only the edit's effect.
	store.DrainAgentInboxChanges()

	// A keystroke batch: a human edits the document. The agent is NOT subscribed to it.
	if _, _, err := store.ReplaceDocumentText(documentID, "start\nedited\n", OperationMeta{
		ActorID:   user.ID,
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	// DESIRED (muted): no inbox row lands in any pushed box for the non-subscriber.
	for _, box := range []string{"general", "for-me"} {
		items, err := store.ListAgentInbox(agent.ID, box, "pending")
		if err != nil {
			t.Fatalf("list inbox box %q: %v", box, err)
		}
		if ev := findAgentEventByType(items, "document.updated"); ev != nil {
			t.Fatalf("muted boundary: a document edit must create no inbox row for a non-subscriber, got %#v in box %q", ev, box)
		}
	}

	// DESIRED (muted): no inbox-changed doorbell fires for the non-subscriber (nothing to wake it).
	changes := store.DrainAgentInboxChanges()
	for _, c := range changes {
		if c.AgentID == agent.ID && c.NotificationType == "document.updated" {
			t.Fatalf("muted boundary: a document edit must ring no inbox doorbell for a non-subscriber, got change %#v", c)
		}
	}
}

// The subscription boundary: subscribing is the ONLY document→push path, and it respects quiescence. A
// subscribed edit persists exactly one general-box card whose available_at sits in the future (~60s) and
// SLIDES forward on the next keystroke batch — so the card matures one window after typing stops, not once
// per batch — and documents still ring no doorbell even for subscribers (delivery is by poll once mature).
func TestSubscribedDocumentUpdatePushesToGeneralWithQuiescence(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/watched.md", "start\n")

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "subscriber",
		Name:   "Subscriber",
		Role:   "Explicitly watches a document",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := store.SubscribeAgentDocument(agent.ID, documentID); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	store.DrainAgentInboxChanges()

	edit := func(text string) {
		if _, _, err := store.ReplaceDocumentText(documentID, text, OperationMeta{
			ActorID: user.ID, ActorType: "human", Source: "test",
		}); err != nil {
			t.Fatalf("edit: %v", err)
		}
	}
	// The persisted card is immature (available_at in the future), so it is hidden from the maturity-filtered
	// listing — read it directly to assert box + the sliding available_at.
	persistedCard := func() *AgentEvent {
		all, err := listAllAgentEventsPostgres(store.db, store.WorkspaceID())
		if err != nil {
			t.Fatalf("list all events: %v", err)
		}
		var found *AgentEvent
		count := 0
		for _, e := range all {
			if e.AgentID == agent.ID && e.Type == "document.updated" && e.DocumentID == documentID {
				found = e
				count++
			}
		}
		if count > 1 {
			t.Fatalf("subscribed doc must have exactly one persisted card (deduped), got %d", count)
		}
		return found
	}

	edit("start\nfirst\n")
	card := persistedCard()
	if card == nil {
		t.Fatalf("a subscribed edit must persist a document card")
	}
	if card.Box != "general" {
		t.Fatalf("a subscribed document card must be in general, got %q", card.Box)
	}
	if !card.AvailableAt.After(time.Now().UTC()) {
		t.Fatalf("a subscribed card must be immature (available_at ~60s out), got %v", card.AvailableAt)
	}
	firstAvailable := card.AvailableAt

	edit("start\nfirst\nsecond\n")
	slid := persistedCard()
	if slid == nil {
		t.Fatalf("the card must survive a second edit")
	}
	if !slid.AvailableAt.After(firstAvailable) {
		t.Fatalf("quiescence: a second edit must slide available_at forward (was %v, now %v)", firstAvailable, slid.AvailableAt)
	}

	// Even for a subscriber, documents ring no doorbell — the mature card is found by the poll.
	for _, c := range store.DrainAgentInboxChanges() {
		if c.NotificationType == "document.updated" {
			t.Fatalf("documents must ring no inbox doorbell even for subscribers, got %#v", c)
		}
	}
}
