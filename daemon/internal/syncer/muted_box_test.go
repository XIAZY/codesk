package syncer

import "testing"

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
