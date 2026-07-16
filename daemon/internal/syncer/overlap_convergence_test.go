package syncer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sergi/go-diff/diffmatchpatch"
	crdt "notty/internal/ycrdt"
)

// Task #27 (P0): an overlapping local-file rewrite and a concurrent remote CRDT
// rewrite of the same line must CONVERGE without losing characters from the DOMINATED
// replaced/moved region.
//
// Root cause: the local reconcile differ (computeLocalTextEdits) previously left every
// in-order common character from DiffMain as an equality, so a semantically-replaced
// or short-moved span REUSED the base document's CRDT identities; a concurrent remote
// delete of the base region then stripped those characters from the new content,
// truncating it into mixed/lost text (the byte-stable `title\nlocl writeremote
// rewrite\n` signature on CI runs 29411756267 / 29469669480). The fix folds equalities
// DOMINATED by their flanking edits into fresh identities (collapseCoincidentalReuse).
//
// Scope, stated honestly: the guarantee is for DOMINATED equalities. A long
// non-dominated overlap that is actually a move (e.g. "LONGSPAN" in
// abcLONGSPAN->LONGSPANdef) is indistinguishable from a genuine anchor given only
// (base, local); it is retained as minimal-ops / legitimate concurrent-conflict
// semantics and is asserted as such (a positive control), NOT as corruption-free.

// --- harness helpers -------------------------------------------------------------

// docFromState builds a fresh document seeded with the given base CRDT state.
func docFromState(t *testing.T, baseState []byte) *crdt.Doc {
	t.Helper()
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, baseState, "base"); err != nil {
		t.Fatalf("apply base state: %v", err)
	}
	return d
}

// remoteLineReplace produces an update that deletes line 2 (the "title\n" prefix is at
// offset 6) of `base` and inserts `newLine` in its place, from a peer that shares the
// same base state — i.e. a concurrent whole-line rewrite.
func remoteLineReplace(t *testing.T, baseState []byte, base, newLine string) []byte {
	t.Helper()
	start := utf16Length("title\n")
	length := utf16Length(base) - start
	d := docFromState(t, baseState)
	text := d.GetText("content") // handle acquired OUTSIDE Update (Update holds the doc mutex)
	update, err := d.Update(func(txn *crdt.Transaction) error {
		if err := text.DeleteRange(txn, start, length); err != nil {
			return err
		}
		return text.InsertValue(txn, start, newLine)
	}, "remote")
	if err != nil {
		t.Fatalf("remote line replace: %v", err)
	}
	return update
}

// mergeContentAndState applies updates onto the base state in the given order and
// returns the converged content and the DECODED state vector (a client->clock map;
// the raw V1 encoding is non-canonical in map order, so compare the decoded map).
func mergeContentAndState(t *testing.T, baseState []byte, updates ...[]byte) (string, crdt.StateVector) {
	t.Helper()
	d := docFromState(t, baseState)
	for i, u := range updates {
		if err := crdt.ApplyUpdateV1(d, u, "u"); err != nil {
			t.Fatalf("apply update %d: %v", i, err)
		}
	}
	raw, err := d.StateVectorV1()
	if err != nil {
		t.Fatalf("state vector: %v", err)
	}
	sv, err := crdt.DecodeStateVectorV1(raw)
	if err != nil {
		t.Fatalf("decode state vector: %v", err)
	}
	return d.GetText("content").ToString(), sv
}

// convergedIdentically asserts both apply orders reach identical content AND state.
func convergedIdentically(t *testing.T, c1 string, sv1 crdt.StateVector, c2 string, sv2 crdt.StateVector) {
	t.Helper()
	if c1 != c2 {
		t.Fatalf("apply orders diverged in content:\n  order1=%q\n  order2=%q", c1, c2)
	}
	if !reflect.DeepEqual(sv1, sv2) {
		t.Fatalf("apply orders diverged in state vector: %v vs %v", sv1, sv2)
	}
}

// --- Row A: dominated replacement + concurrent old-region delete = lossless ------

func TestOverlappingRewriteConvergesWithoutLostCharacters(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		localLine  string
		remoteLine string
	}{
		// Canonical CI signature: LCS of "shared"/"local rewrite" = a,r,e,\n (all short,
		// dominated islands).
		{"shared_signature", "title\nshared\n", "local rewrite", "remote rewrite"},
		// No shared characters: control — no coincidental identities to reuse at all.
		{"disjoint_base", "title\nXXXXXX\n", "local rewrite", "remote update"},
		// Dominated MOVED span: "xxx" (3) is dominated by "abc"(3)/"def"(3); the case
		// DiffCleanupSemantic's overlap extraction reintroduced.
		{"moved_span_dominated", "title\nabcxxx\n", "xxxdef", "remote value"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			local := "title\n" + tc.localLine + "\n"
			baseState := crdtStateFromContent(base)

			localUpdate, _, err := buildLocalUpdateFromBase(baseState, base, local)
			if err != nil {
				t.Fatalf("build local update: %v", err)
			}
			remoteUpdate := remoteLineReplace(t, baseState, base, tc.remoteLine+"\n")

			// Direct boundary inspection: the reconcile must NOT retain any base
			// character inside the replaced line (no equality island) — that is the
			// identity-level authority, independent of the merge outcome.
			assertNoRetainedIdentityInReplacedLine(t, base, local)

			// Convergence: both apply orders must reach IDENTICAL content AND state
			// vector, and neither edit's payload may lose characters.
			c1, sv1 := mergeContentAndState(t, baseState, localUpdate, remoteUpdate)
			c2, sv2 := mergeContentAndState(t, baseState, remoteUpdate, localUpdate)
			convergedIdentically(t, c1, sv1, c2, sv2)
			for _, got := range []string{c1, c2} {
				if !strings.Contains(got, tc.localLine) {
					t.Fatalf("local rewrite %q lost characters (dominated-span identity reuse); merged=%q", tc.localLine, got)
				}
				if !strings.Contains(got, tc.remoteLine) {
					t.Fatalf("remote rewrite %q missing after merge; merged=%q", tc.remoteLine, got)
				}
			}
		})
	}
}

// assertNoRetainedIdentityInReplacedLine checks the reconcile edits for the changed
// line contain no interior equality gap — i.e. the whole replaced run is delete+insert
// with fresh identities, so no base character survives inside it.
func assertNoRetainedIdentityInReplacedLine(t *testing.T, base, local string) {
	t.Helper()
	edits, err := computeLocalTextEdits(base, local)
	if err != nil {
		t.Fatalf("computeLocalTextEdits: %v", err)
	}
	// The changed region begins after the common "title\n" prefix (offset 6); no edit
	// may touch the prefix. For a fully-dominated line replacement the reconcile must
	// emit exactly ONE delete of the whole old line content and ONE insert of the whole
	// new content — a retained interior base span (identity reuse) would SPLIT the delete
	// (and insert) into two around the kept equality, so >1 delete signals reuse.
	prefix := utf16Length("title\n")
	dels, ins := 0, 0
	for _, e := range edits {
		if e.Start < prefix {
			t.Fatalf("edit touched the unchanged prefix (retained-boundary violation): %+v", e)
		}
		switch e.Kind {
		case localTextEditDelete:
			dels++
		case localTextEditInsert:
			ins++
		}
	}
	if dels != 1 || ins != 1 {
		t.Fatalf("fully-replaced line must be one contiguous delete+insert (no interior retained identity); got %d deletes, %d inserts: %+v", dels, ins, edits)
	}
}

// --- Row B: long non-dominated overlap = retained (positive control) -------------

func TestLongOverlapMoveRetainsIdentityAsLegitimateConflict(t *testing.T) {
	// "LONGSPAN"(8) is NOT dominated by "abc"(3)/"def"(3); indistinguishable from a
	// genuine anchor given only (base, local). It is retained by design — documented as
	// minimal-ops / legitimate concurrent-conflict semantics, explicitly NOT claimed
	// corruption-free. This pins the scope boundary so a future over-broad change that
	// starts collapsing long overlaps turns this control RED.
	base := "title\nabcLONGSPAN\n"
	local := "title\nLONGSPANdef\n"
	edits, err := computeLocalTextEdits(base, local)
	if err != nil {
		t.Fatalf("edits: %v", err)
	}
	// The retained "LONGSPAN" equality means the edits do NOT delete+insert the whole
	// line: there is a gap the delete/insert don't cover (the kept span).
	total := 0
	for _, e := range edits {
		if e.Kind == localTextEditDelete {
			total += e.Length
		}
	}
	oldLine := utf16Length("abcLONGSPAN\n")
	if total >= oldLine {
		t.Fatalf("long overlap should be RETAINED (partial delete), but the whole old line was deleted (over-correction): edits=%+v", edits)
	}
}

// --- Row C: genuine anchors keep identity; concurrent edit ELSEWHERE both survive --

func TestGenuineAnchorsRetainedAndConcurrentEditElsewhereSurvives(t *testing.T) {
	// Two lines. Local incrementally edits line 2 ("cat"->"dog"); a concurrent remote
	// edits line 3 ("sat"->"lay") — a DIFFERENT region. Genuine anchors ("the ", " ")
	// keep identity, ops stay scoped, and both edits survive.
	base := "the cat\nsat here\n"
	local := "the dog\nsat here\n" // line 2: cat->dog (mid-line, anchors "the "/"\n")
	baseState := crdtStateFromContent(base)

	localUpdate, _, err := buildLocalUpdateFromBase(baseState, base, local)
	if err != nil {
		t.Fatalf("local update: %v", err)
	}
	// concurrent remote edits line 3 region: "here" -> "there".
	d := docFromState(t, baseState)
	text := d.GetText("content")
	remoteStart := utf16Length("the cat\nsat ")
	remoteUpdate, err := d.Update(func(txn *crdt.Transaction) error {
		return text.InsertValue(txn, remoteStart, "over ")
	}, "remote")
	if err != nil {
		t.Fatalf("remote update: %v", err)
	}
	c1, sv1 := mergeContentAndState(t, baseState, localUpdate, remoteUpdate)
	c2, sv2 := mergeContentAndState(t, baseState, remoteUpdate, localUpdate)
	convergedIdentically(t, c1, sv1, c2, sv2)
	if !strings.Contains(c1, "the dog") || !strings.Contains(c1, "over here") {
		t.Fatalf("both edits must survive when they touch different regions; got %q", c1)
	}
	// Minimal ops: the reconcile must NOT have replaced the whole first line (anchor
	// "the " retained), or the concurrent line-3 edit could have been clobbered.
	edits, _ := computeLocalTextEdits(base, local)
	for _, e := range edits {
		if e.Kind == localTextEditDelete && e.Start < utf16Length("the ") {
			t.Fatalf("genuine prefix anchor 'the ' was not retained (over-correction): %+v", edits)
		}
	}
}

// --- Row D: leftward cascade reaches the dominance fixpoint -----------------------

func TestDominanceCollapseReachesFixpointOnCascade(t *testing.T) {
	// Genuine multi-round cascade: the "xx" island is NOT dominated in pass 1 (its left
	// neighbour is a 1-char edit), but folding the leftward "y" island grows that
	// neighbour so "xx" IS dominated in pass 2. A single pass RETAINS "xx" (base
	// identity reused → lost on a concurrent old-region delete); only the fixpoint folds
	// it. Verified: fixpoint => [-"AyxxCCCC"][+"yBxxDDDD"]; single-pass => keeps [="xx"].
	base := "title\nAyxxCCCC\n"
	local := "title\nyBxxDDDD\n"
	// After the fixpoint, no interior equality remains inside the replaced run: the
	// whole changed span is delete+insert (fresh identity), contiguous.
	assertNoRetainedIdentityInReplacedLine(t, base, local)
	// End-to-end: concurrent remote delete of the old line loses nothing — including the
	// "xx" that only the second iteration reclaimed.
	baseState := crdtStateFromContent(base)
	localUpdate, _, _ := buildLocalUpdateFromBase(baseState, base, local)
	remoteUpdate := remoteLineReplace(t, baseState, base, "remote value\n")
	c1, sv1 := mergeContentAndState(t, baseState, localUpdate, remoteUpdate)
	c2, sv2 := mergeContentAndState(t, baseState, remoteUpdate, localUpdate)
	convergedIdentically(t, c1, sv1, c2, sv2)
	if !strings.Contains(c1, "yBxxDDDD") {
		t.Fatalf("cascade: local payload lost characters (fixpoint not reached); got %q", c1)
	}
}

// --- Row E: UTF-16 / astral surrogate-pair threshold boundary --------------------

func TestDominanceThresholdAstralCharacters(t *testing.T) {
	// 😀 / 🎉 are surrogate pairs: utf16Length 2, one rune each. The offset math and the
	// dominance threshold count UTF-16 code units, so a dominated island containing an
	// astral char must fold and reconcile with correct offsets (no split surrogate, no
	// byte corruption) when a concurrent remote delete hits the base region.
	base := "title\nZZ😀ZZ\n"      // astral island flanked by 2-char runs
	local := "title\nAAAA😀BBBB\n" // 😀(utf16 2) dominated by 4-char edits both sides
	baseState := crdtStateFromContent(base)
	localUpdate, _, err := buildLocalUpdateFromBase(baseState, base, local)
	if err != nil {
		t.Fatalf("astral local update: %v", err)
	}
	remoteUpdate := remoteLineReplace(t, baseState, base, "remote 🎉\n")
	c1, sv1 := mergeContentAndState(t, baseState, localUpdate, remoteUpdate)
	c2, sv2 := mergeContentAndState(t, baseState, remoteUpdate, localUpdate)
	convergedIdentically(t, c1, sv1, c2, sv2)
	// The local astral payload survives intact (dominated fold gave it fresh identity),
	// the remote astral payload is present, and no surrogate half leaked.
	if !strings.Contains(c1, "AAAA😀BBBB") || !strings.Contains(c1, "remote 🎉") {
		t.Fatalf("astral payloads lost/corrupted: %q", c1)
	}
	if !utf8ValidNoLoneSurrogate(c1) {
		t.Fatalf("astral merge produced invalid UTF-8 / lone surrogate: %q", c1)
	}
}

func utf8ValidNoLoneSurrogate(s string) bool {
	return strings.ToValidUTF8(s, "�") == s && !strings.ContainsRune(s, '�')
}

// --- Row F: genuine INTERIOR anchor between disjoint changed blocks is retained -----

func TestGenuineInteriorAnchorRetained(t *testing.T) {
	// "AAAA keep BBBB" -> "XXXX keep YYYY": " keep " is a genuine unchanged run between
	// two disjoint changed blocks — longer than its flanking edits, so it is NOT
	// dominated and must retain identity (minimal ops; a concurrent edit to " keep "
	// itself would merge). The reconcile must therefore NOT collapse the whole line into
	// one delete+insert — the retained anchor SPLITS the delete. An over-broad fix that
	// folds interior anchors would emit a single whole-line delete, reddening this row.
	base := "title\nAAAA keep BBBB\n"
	local := "title\nXXXX keep YYYY\n"
	edits, err := computeLocalTextEdits(base, local)
	if err != nil {
		t.Fatalf("edits: %v", err)
	}
	dels := 0
	for _, e := range edits {
		if e.Kind == localTextEditDelete {
			dels++
		}
	}
	if dels < 2 {
		t.Fatalf("genuine interior anchor \" keep \" must be retained (splitting the delete into 2); got %d delete(s): %+v — over-correction folded the anchor", dels, edits)
	}
}

// --- Idempotence / termination of the collapse pass ------------------------------

func TestCollapseCoincidentalReuseIsIdempotentAndTerminates(t *testing.T) {
	dmp := diffmatchpatch.New()
	corpus := [][2]string{
		{"shared\n", "local rewrite\n"},
		{"abcxxx\n", "xxxdef\n"},
		{"abcLONGSPAN\n", "LONGSPANdef\n"},
		{"the cat sat\n", "the dog sat\n"},
		{"AAAA keep BBBB\n", "XXXX keep YYYY\n"},
		{"aXbYcZ\n", "P123Q456\n"},
		{"ZZ😀ZZ\n", "AAAA😀BBBB\n"},
		{"", "brand new\n"},
		{"delete me\n", ""},
	}
	for _, c := range corpus {
		once := collapseCoincidentalReuse(dmp, dmp.DiffMain(c[0], c[1], false))
		twice := collapseCoincidentalReuse(dmp, once)
		if !reflect.DeepEqual(once, twice) {
			t.Fatalf("collapse not idempotent for %q->%q:\n once=%v\n twice=%v", c[0], c[1], once, twice)
		}
		// Reconstruct: applying the final diff's deletes/inserts to base yields local.
		if got := diffText(once); got != c[1] {
			t.Fatalf("collapse changed the diff's resulting text for %q->%q: got %q", c[0], c[1], got)
		}
	}
}

// diffText reconstructs the "new" side of a diff (equalities + insertions).
func diffText(diffs []diffmatchpatch.Diff) string {
	var b strings.Builder
	for _, d := range diffs {
		if d.Type != diffmatchpatch.DiffDelete {
			b.WriteString(d.Text)
		}
	}
	return b.String()
}

// countInteriorEqualities counts equality ops that are neither the first nor last op
// (i.e. genuine interior runs, the ones the collapse can fold). This is the strict
// decreasing measure that proves the fixpoint terminates.
func countInteriorEqualities(diffs []diffmatchpatch.Diff) int {
	n := 0
	for i, d := range diffs {
		if d.Type == diffmatchpatch.DiffEqual && i > 0 && i < len(diffs)-1 {
			n++
		}
	}
	return n
}

// oneCollapseRound performs exactly one folding round + normalization (the loop body of
// collapseCoincidentalReuse), returning the result and whether it folded anything.
func oneCollapseRound(dmp *diffmatchpatch.DiffMatchPatch, diffs []diffmatchpatch.Diff) ([]diffmatchpatch.Diff, bool) {
	out := make([]diffmatchpatch.Diff, 0, len(diffs)+2)
	folded := false
	for i, d := range diffs {
		if d.Type != diffmatchpatch.DiffEqual {
			out = append(out, d)
			continue
		}
		l := maxAdjacentEditLen(diffs, i, -1)
		r := maxAdjacentEditLen(diffs, i, +1)
		e := utf16Length(d.Text)
		if l == 0 || r == 0 || e > l || e > r {
			out = append(out, d)
			continue
		}
		out = append(out,
			diffmatchpatch.Diff{Type: diffmatchpatch.DiffDelete, Text: d.Text},
			diffmatchpatch.Diff{Type: diffmatchpatch.DiffInsert, Text: d.Text},
		)
		folded = true
	}
	return dmp.DiffCleanupMerge(out), folded
}

// TestCollapseInteriorEqualityCountStrictlyDecreases proves the structural termination
// measure: every round that folds anything strictly reduces the interior-equality
// count (DiffCleanupMerge only factors onto OUTER boundaries, never manufacturing a new
// interior equality), so the pass reaches its fixpoint in a bounded number of rounds.
func TestCollapseInteriorEqualityCountStrictlyDecreases(t *testing.T) {
	dmp := diffmatchpatch.New()
	corpus := []string{"shared\n|local rewrite\n", "abcxxx\n|xxxdef\n",
		"AyxxCCCC\n|yBxxDDDD\n", "the cat sat\n|the dog sat\n",
		"AAAA keep BBBB\n|XXXX keep YYYY\n", "ZZ😀ZZ\n|AAAA😀BBBB\n"}
	for _, pair := range corpus {
		parts := strings.SplitN(pair, "|", 2)
		diffs := dmp.DiffCleanupMerge(dmp.DiffMain(parts[0], parts[1], false))
		prev := countInteriorEqualities(diffs)
		for round := 0; round < 64; round++ {
			next, folded := oneCollapseRound(dmp, diffs)
			if !folded {
				break // fixpoint reached
			}
			cur := countInteriorEqualities(next)
			if cur >= prev {
				t.Fatalf("%q: interior-equality count did not strictly decrease on a folding round: %d -> %d", pair, prev, cur)
			}
			prev = cur
			diffs = next
			if round == 63 {
				t.Fatalf("%q: did not reach fixpoint within the interior-count bound", pair)
			}
		}
	}
}
