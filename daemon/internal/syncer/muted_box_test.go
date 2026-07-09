package syncer

import "testing"

// Supervisor "no turn on muted" row (task #2). The daemon schedules a notification turn only when the
// pushed boxes (for_me/general) are non-empty (automation.go: `if len(forMe)==0 && len(general)==0 { return }`).
// A muted event must therefore never appear in a pushed box — proven here at the decision function so a
// muted-only inbox schedules no turn, without needing the fake-runtime supervisor plumbing. (The "mature
// subscribed card schedules exactly one turn" half rides the backend maturity filter — the daemon only
// fetches mature+pending cards — which is pinned store-side.)
func TestMutedEventNeverEntersPushedNotificationBoxes(t *testing.T) {
	const agentID = "11111111-1111-1111-1111-111111111111"
	muted := &agentEvent{ID: "m", AgentID: agentID, Type: "document.updated", Box: "muted", Status: "pending"}
	general := &agentEvent{ID: "g", AgentID: agentID, Type: "document.updated", Box: "general", Status: "pending"}
	forme := &agentEvent{ID: "f", AgentID: agentID, Type: "thread.mentioned", Box: "for_me", Status: "pending"}
	events := []*agentEvent{muted, general, forme}

	for _, box := range []string{"for_me", "general"} {
		for _, e := range pendingNotificationsForAgent(events, agentID, box) {
			if e.ID == muted.ID {
				t.Fatalf("a muted event must never enter the %q notification box", box)
			}
		}
	}

	// Muted-only inbox → both pushed boxes empty → the supervisor gate schedules no turn.
	mutedOnly := []*agentEvent{muted}
	if n := len(pendingNotificationsForAgent(mutedOnly, agentID, "for_me")); n != 0 {
		t.Fatalf("muted-only for_me must be empty, got %d", n)
	}
	if n := len(pendingNotificationsForAgent(mutedOnly, agentID, "general")); n != 0 {
		t.Fatalf("muted-only general must be empty (no turn), got %d", n)
	}

	// Sanity: genuinely-pushed events still route to their boxes.
	if got := pendingNotificationsForAgent(events, agentID, "general"); len(got) != 1 || got[0].ID != general.ID {
		t.Fatalf("general box should carry exactly the general event, got %v", got)
	}
	if got := pendingNotificationsForAgent(events, agentID, "for_me"); len(got) != 1 || got[0].ID != forme.ID {
		t.Fatalf("for_me box should carry exactly the mention, got %v", got)
	}
}

// Task #2 boundary #2 (daemon copy): a muted box value must NEVER normalize to a pushed box. The daemon
// has its own normalizeInboxBox copy; if it falls through to the for_me default (as it did before this task),
// a muted document item would be surfaced as an actionable notification and pushed to the runtime — the
// exact ambient-push the feature removes. This is the adversarial row that keeps the two normalizer copies
// (backend store.go + this one) in agreement.
func TestNormalizeInboxBoxMutedNeverRoutesToPushedBox(t *testing.T) {
	for _, in := range []string{"muted", "MUTED", " muted ", "Muted", "mUtEd"} {
		if got := normalizeInboxBox(in); got != "muted" {
			t.Fatalf("normalizeInboxBox(%q) = %q, want \"muted\" — a muted item must never fall through to a pushed box", in, got)
		}
	}
	// The pushed boxes and the unknown->for_me default are unchanged.
	if got := normalizeInboxBox("general"); got != "general" {
		t.Fatalf("general regressed: %q", got)
	}
	if got := normalizeInboxBox("for-me"); got != "for_me" {
		t.Fatalf("for-me regressed: %q", got)
	}
	if got := normalizeInboxBox("bogus"); got != "for_me" {
		t.Fatalf("an unknown box should still default to for_me, got %q", got)
	}
}
