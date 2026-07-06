package notty

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The six canonical daemon lifecycle states the backend actually emits. Each is produced through
// the real applyDaemonLiveness path (softDeleted carries a "deleted" status flag / DeletedAt before
// the call). This golden test pins the exact wire shape those states serialize to: if
// lastSeenAgeSeconds ever gains omitempty (dropping it for never-seen daemons) or a status string
// changes, the frontend liveness logic breaks silently — and this test fails immediately instead.
const (
	daemonGoldenWorkspaceID = "11111111-1111-1111-1111-111111111111"
	daemonStatesGoldenFile  = "testdata/daemon_states.json"
)

// daemonGoldenReference is the fixed "now" every state is constructed against.
func daemonGoldenReference() time.Time {
	return time.Date(2026, time.July, 6, 0, 0, 0, 0, time.UTC)
}

// daemonGoldenCreatedAt is the fixed CreatedAt shared by every constructed state.
func daemonGoldenCreatedAt() time.Time {
	return time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
}

// daemonGoldenState builds a single daemon and runs it through the real liveness path at the
// reference time, exactly as the store does before emitting it on the wire.
func daemonGoldenState(id, name, status string, lastSeenAt, deletedAt time.Time) *Daemon {
	daemon := &Daemon{
		ID:          id,
		WorkspaceID: daemonGoldenWorkspaceID,
		Name:        name,
		Status:      status,
		LastSeenAt:  lastSeenAt,
		CreatedAt:   daemonGoldenCreatedAt(),
		DeletedAt:   deletedAt,
	}
	applyDaemonLiveness(daemon, daemonGoldenReference())
	return daemon
}

// daemonGoldenStates returns the six canonical states keyed by name.
func daemonGoldenStates() map[string]*Daemon {
	ref := daemonGoldenReference()
	return map[string]*Daemon{
		// active, never checked in: zero lastSeenAt -> disconnected, age 0 (root of bug 1).
		"neverSeen": daemonGoldenState("daemon-neverseen", "Never Seen", "active", time.Time{}, time.Time{}),
		// checked in at the reference instant -> age 0, online.
		"justSeen": daemonGoldenState("daemon-justseen", "Just Seen", "active", ref, time.Time{}),
		// 25s since check-in -> age 25, online.
		"aging": daemonGoldenState("daemon-aging", "Aging", "active", ref.Add(-25*time.Second), time.Time{}),
		// 60s since check-in -> age 60, stale.
		"stale": daemonGoldenState("daemon-stale", "Stale", "active", ref.Add(-60*time.Second), time.Time{}),
		// 200s since check-in -> age 200, disconnected.
		"dead": daemonGoldenState("daemon-dead", "Dead", "active", ref.Add(-200*time.Second), time.Time{}),
		// soft-deleted: status flag + DeletedAt; liveness early-returns disconnected/age 0 (root of bug 2).
		"softDeleted": daemonGoldenState("daemon-softdeleted", "Soft Deleted", "deleted", ref, ref),
	}
}

// marshalDaemonStatesGolden serializes the keyed states with stable (alphabetical) key order via a
// map[string]json.RawMessage, then pretty-prints so the committed golden file is diff-friendly.
func marshalDaemonStatesGolden(t *testing.T) []byte {
	t.Helper()
	object := make(map[string]json.RawMessage, 6)
	for name, daemon := range daemonGoldenStates() {
		raw, err := json.Marshal(daemon)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		object[name] = raw
	}
	encoded, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden object: %v", err)
	}
	return append(encoded, '\n')
}

func TestDaemonStatesGoldenContract(t *testing.T) {
	path := filepath.FromSlash(daemonStatesGoldenFile)
	got := marshalDaemonStatesGolden(t)

	if os.Getenv("NOTTY_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden file: %v", err)
		}
		t.Logf("regenerated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file %s: %v (re-run with NOTTY_UPDATE_GOLDEN=1 to generate it)", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("daemon wire format drifted from %s.\nIf the wire format legitimately changed, re-run with NOTTY_UPDATE_GOLDEN=1 to regenerate the golden file.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
