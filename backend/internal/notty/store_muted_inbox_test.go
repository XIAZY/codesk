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

// Bare list-inbox = ALL boxes (AlphaToad's ruling): an empty box query returns items from for-me, general,
// AND muted in one list; a specific --box still filters to that box. The muted item being reachable via the
// bare list is the point — it stays queryable, just not pushed.
func TestBareListInboxReturnsAllBoxesIncludingMuted(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	mutedDoc := mustCreateTestDocument(t, store, "docs/muted.md", "start\n")
	subscribedDoc := mustCreateTestDocument(t, store, "docs/subscribed.md", "start\n")

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "watcher", Name: "Watcher", Role: "sees all three boxes", Kind: "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// general source: a subscribed document, edited.
	if err := store.SubscribeAgentDocument(agent.ID, subscribedDoc); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, _, err := store.ReplaceDocumentText(subscribedDoc, "start\nsub-edit\n", OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("edit subscribed doc: %v", err)
	}
	// muted source: an unsubscribed document, edited.
	if _, _, err := store.ReplaceDocumentText(mutedDoc, "start\nmuted-edit\n", OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("edit muted doc: %v", err)
	}
	// for-me source: a thread that mentions the agent.
	if _, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: subscribedDoc, Title: "Look", Body: "please review @watcher", RelativeStart: "s", RelativeEnd: "e",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Bare list (empty box) → all three boxes present.
	all, err := store.ListAgentInbox(agent.ID, "", "pending")
	if err != nil {
		t.Fatalf("bare list inbox: %v", err)
	}
	boxes := map[string]bool{}
	for _, e := range all {
		boxes[normalizeInboxBox(e.Box)] = true
	}
	for _, want := range []string{"for_me", "general", "muted"} {
		if !boxes[want] {
			t.Fatalf("bare list-inbox must include the %q box; got boxes %v", want, boxes)
		}
	}

	// A specific --box still filters to exactly that box.
	for _, box := range []string{"for_me", "general", "muted"} {
		items, err := store.ListAgentInbox(agent.ID, box, "pending")
		if err != nil {
			t.Fatalf("list --box %q: %v", box, err)
		}
		if len(items) == 0 {
			t.Fatalf("--box %q should return its items", box)
		}
		for _, e := range items {
			if normalizeInboxBox(e.Box) != normalizeInboxBox(box) {
				t.Fatalf("--box %q returned a %q item", box, e.Box)
			}
		}
	}
}

// Cascade guard: a subscription joins the FK cascade graph, so deleting the agent removes its
// subscriptions (the same ON DELETE CASCADE FK covers document deletion). Extends the #83 cascade sweep.
func TestDocumentSubscriptionsCascadeOnAgentDelete(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/cascade.md", "x\n")

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "cascade-agent", Name: "Cascade", Role: "subscribes then is deleted", Kind: "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := store.SubscribeAgentDocument(agent.ID, documentID); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subs, err := listDocumentSubscriberAgentIDsPostgres(store.db, store.WorkspaceID(), documentID); err != nil || len(subs) != 1 {
		t.Fatalf("expected exactly one subscriber before delete (err=%v), got %d", err, len(subs))
	}

	if _, err := store.DeleteAgent(agent.ID, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("delete agent: %v", err)
	}

	subs, err := listDocumentSubscriberAgentIDsPostgres(store.db, store.WorkspaceID(), documentID)
	if err != nil {
		t.Fatalf("list subscribers after delete: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("deleting the agent must cascade-remove its subscriptions, got %d", len(subs))
	}
}

// Load-bearing thread-doorbell guard (Tom's ruling 964e6eaa): task #2 removes the DOCUMENT doorbell, and
// the protocol-test flip deleted the only positive doorbell-fires assertion — so this is now the ONLY pin
// that a thread event still rings the inbox doorbell. It asserts the FULL positive shape (exactly one
// agent.inbox.changed for the agent, right type + for_me box) on BOTH thread create and reply. A regression
// that silently killed thread doorbells (e.g. an over-eager deletion of the fan-out) reds here or nowhere.
func TestThreadDoorbellFiresForMentionOnCreateAndReply(t *testing.T) {
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	seedCodexDaemonRuntime(t, store)
	user := seedTestUser(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/thread.md", "start\n")

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Gets mentioned",
		Kind:   "codex",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	assertExactlyOneMentionDoorbell := func(label string) {
		t.Helper()
		count := 0
		for _, c := range store.DrainAgentInboxChanges() {
			if c.AgentID != agent.ID {
				continue
			}
			count++
			if c.NotificationType != "thread.mentioned" {
				t.Fatalf("%s: doorbell type = %q, want thread.mentioned", label, c.NotificationType)
			}
			if normalizeInboxBox(c.Box) != "for_me" {
				t.Fatalf("%s: doorbell box = %q, want for_me", label, c.Box)
			}
		}
		if count != 1 {
			t.Fatalf("%s: want exactly one inbox doorbell for the agent, got %d", label, count)
		}
	}

	store.DrainAgentInboxChanges()
	thread, _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID, Title: "Question", Body: "please review @reviewer",
		RelativeStart: "s", RelativeEnd: "e",
	}, OperationMeta{ActorID: user.ID, ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	assertExactlyOneMentionDoorbell("thread create mention")

	if _, _, err := store.ReplyThread(thread.ID, ReplyThreadRequest{Body: "following up @reviewer"}, OperationMeta{
		ActorID: user.ID, ActorType: "human", Source: "test",
	}); err != nil {
		t.Fatalf("reply thread: %v", err)
	}
	assertExactlyOneMentionDoorbell("thread reply mention")
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
