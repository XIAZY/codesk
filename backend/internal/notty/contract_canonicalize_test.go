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
	// Unanchored variants: UUIDs and timestamps also appear EMBEDDED in human-readable strings —
	// activity summaries like "<uuid> started a thread on document <uuid>". Left raw, those non-
	// deterministic ids leak into the golden, so we replace each occurrence in place with the same
	// alias it gets as a standalone id field (referential consistency across the sentence and the graph).
	contractEmbeddedUUIDPattern      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	contractEmbeddedTimestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
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
	// Whole-string id / timestamp fields — the common case.
	if contractUUIDPattern.MatchString(s) {
		return contractAliasFor(s, aliases)
	}
	if contractTimestampPattern.MatchString(s) {
		return "<ts>"
	}
	// Otherwise the string may still embed ids / timestamps (activity summaries, error messages).
	// Collapse embedded timestamps first (they never overlap a UUID), then alias embedded UUIDs so
	// the same id reads as the same alias whether it appears as a field or inside a sentence.
	s = contractEmbeddedTimestampPattern.ReplaceAllString(s, "<ts>")
	s = contractEmbeddedUUIDPattern.ReplaceAllStringFunc(s, func(m string) string {
		return contractAliasFor(m, aliases)
	})
	return s
}

func contractAliasFor(uuid string, aliases map[string]string) string {
	alias, ok := aliases[uuid]
	if !ok {
		alias = fmt.Sprintf("<id-%d>", len(aliases)+1)
		aliases[uuid] = alias
	}
	return alias
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

func TestCanonicalizeContractJSONAliasesEmbeddedIdsAndTimestamps(t *testing.T) {
	// Activity summaries embed ids and timestamps inside a human-readable sentence. Each must
	// canonicalize to the SAME alias the id carries as a standalone field (and timestamps to <ts>),
	// or raw non-deterministic ids leak into the golden and it drifts every run — the exact bug the
	// populated-workspace row surfaced.
	const actorID = "44444444-4444-4444-4444-444444444444"
	const docID = "55555555-5555-5555-5555-555555555555"
	raw := fmt.Sprintf(`{
		"actorId": %[1]q,
		"documentId": %[2]q,
		"summary": "%[1]s started a thread on document %[2]s at 2026-07-06T12:00:00Z"
	}`, actorID, docID)
	got := mustCanonicalize(t, raw)

	if strings.Contains(got, actorID) || strings.Contains(got, docID) || strings.Contains(got, "2026") {
		t.Fatalf("embedded id/timestamp leaked into canonical form:\n%s", got)
	}
	// sorted keys: actorId < documentId < summary → actorID=<id-1>, docID=<id-2>; the summary reuses both.
	if c := strings.Count(got, "<id-1>"); c != 2 {
		t.Fatalf("actor id should read as <id-1> both as a field and inside the summary, got %d:\n%s", c, got)
	}
	if c := strings.Count(got, "<id-2>"); c != 2 {
		t.Fatalf("document id should read as <id-2> both as a field and inside the summary, got %d:\n%s", c, got)
	}
	if !strings.Contains(got, "started a thread on document <id-2> at <ts>") {
		t.Fatalf("expected embedded ids and timestamp replaced in place:\n%s", got)
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
