package notty

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The contract-regression tier diffs a real backend response against a committed golden. Real
// handlers mint server-side UUIDs and live-PG wall-clock timestamps, so raw byte-equality is
// nondeterministic from the first run. canonicalizeContractJSON rewrites those non-deterministic
// values into stable forms while preserving structure:
//   - every UUID-shaped string is replaced by a stable alias (<id-1>, <id-2>, …), assigned in
//     deterministic first-seen order over sorted keys, and the SAME uuid always maps to the SAME
//     alias — so cross-references (thread.id ↔ message.threadId ↔ actorId) stay consistent and a
//     broken reference still surfaces as a golden mismatch.
//   - every timestamp-shaped string becomes "<ts>" — time is shape-only in this tier.
// A committed golden is thus a canonical shape; CI canonicalizes the live response and byte-diffs it.

var (
	contractUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// RFC3339 with optional fractional seconds, plus the zero-time the store emits for "never".
	contractTimestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
)

func canonicalizeContractJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numeric shape (int vs float) intact instead of coercing to float64
	var tree interface{}
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	aliases := map[string]string{}
	canon := canonicalizeContractValue(tree, aliases)
	// The encoder sorts map keys (deterministic output order regardless of insertion); the sorted-key
	// traversal above is what makes ALIAS ASSIGNMENT order deterministic. Disable HTML escaping so the
	// <id-N> / <ts> placeholders stay literal and the committed goldens are readable in review diffs.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(canon); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func canonicalizeContractValue(v interface{}, aliases map[string]string) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]interface{}, len(t))
		for _, k := range keys {
			out[k] = canonicalizeContractValue(t[k], aliases)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = canonicalizeContractValue(e, aliases)
		}
		return out
	case string:
		return canonicalizeContractString(t, aliases)
	default:
		return v
	}
}

func canonicalizeContractString(s string, aliases map[string]string) string {
	if contractUUIDPattern.MatchString(s) {
		alias, ok := aliases[s]
		if !ok {
			alias = fmt.Sprintf("<id-%d>", len(aliases)+1)
			aliases[s] = alias
		}
		return alias
	}
	if contractTimestampPattern.MatchString(s) {
		return "<ts>"
	}
	return s
}

func mustCanonicalize(t *testing.T, raw string) string {
	t.Helper()
	out, err := canonicalizeContractJSON([]byte(raw))
	if err != nil {
		t.Fatalf("canonicalize %q: %v", raw, err)
	}
	return string(out)
}

func TestCanonicalizeContractJSONStableAliasesAndRefConsistency(t *testing.T) {
	// The same uuid appears as thread.id AND message.threadId AND participant.participantId; each must
	// canonicalize to the SAME alias so the referential graph survives. A distinct uuid gets a distinct
	// alias, numbered in first-seen (sorted-key) order.
	const threadID = "11111111-1111-1111-1111-111111111111"
	const actorID = "22222222-2222-2222-2222-222222222222"
	raw := fmt.Sprintf(`{
		"thread": {"id": %[1]q, "createdBy": %[2]q},
		"messages": [{"threadId": %[1]q, "authorId": %[2]q}]
	}`, threadID, actorID)
	got := mustCanonicalize(t, raw)

	if strings.Contains(got, threadID) || strings.Contains(got, actorID) {
		t.Fatalf("raw uuids leaked into canonical form:\n%s", got)
	}
	// sorted keys: top-level "messages" < "thread"; within messages, "authorId" < "threadId"; within
	// thread, "createdBy" < "id". First-seen order therefore assigns actorID -> <id-1>, threadID -> <id-2>.
	if c := strings.Count(got, "<id-1>"); c != 2 {
		t.Fatalf("actor uuid should map to one stable alias used twice, got %d occurrences of <id-1>:\n%s", c, got)
	}
	if c := strings.Count(got, "<id-2>"); c != 2 {
		t.Fatalf("thread uuid should map to one stable alias used twice, got %d occurrences of <id-2>:\n%s", c, got)
	}
}

func TestCanonicalizeContractJSONReplacesTimestampsShapeOnly(t *testing.T) {
	raw := `{"createdAt": "2026-07-06T12:34:56Z", "lastSeenAt": "0001-01-01T00:00:00Z", "updatedAt": "2026-07-06T12:34:56.789+02:00"}`
	got := mustCanonicalize(t, raw)
	if strings.Contains(got, "2026") || strings.Contains(got, "0001-01-01") {
		t.Fatalf("timestamps not canonicalized:\n%s", got)
	}
	if c := strings.Count(got, `"<ts>"`); c != 3 {
		t.Fatalf("expected 3 <ts> placeholders, got %d:\n%s", c, got)
	}
}

func TestCanonicalizeContractJSONIsDeterministicAndIdempotent(t *testing.T) {
	raw := `{"b": "33333333-3333-3333-3333-333333333333", "a": {"ref": "33333333-3333-3333-3333-333333333333", "when": "2026-07-06T00:00:00Z"}, "list": [1, 2, 3]}`
	first := mustCanonicalize(t, raw)
	second := mustCanonicalize(t, raw)
	if first != second {
		t.Fatalf("canonicalization is not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if idem := mustCanonicalize(t, first); idem != first {
		t.Fatalf("canonicalization is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", first, idem)
	}
	// the shared uuid collapses to a single alias
	if !strings.Contains(first, "<id-1>") || strings.Contains(first, "<id-2>") {
		t.Fatalf("shared uuid should collapse to exactly one alias:\n%s", first)
	}
}

func TestCanonicalizeContractJSONPreservesShapeOfNonSpecialValues(t *testing.T) {
	// Non-uuid/non-timestamp strings, empty collections, numbers, bools, and null all pass through so a
	// real shape change (a [] becoming null, an int becoming a string, a field vanishing) still diffs.
	raw := `{"name": "Local daemon", "connectionStatus": "online", "count": 42, "ratio": 3.5, "active": true, "deletedAt": null, "agents": [], "labels": {}}`
	got := mustCanonicalize(t, raw)
	for _, want := range []string{`"name": "Local daemon"`, `"connectionStatus": "online"`, `"count": 42`, `"ratio": 3.5`, `"active": true`, `"deletedAt": null`, `"agents": []`, `"labels": {}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q preserved in:\n%s", want, got)
		}
	}
}
