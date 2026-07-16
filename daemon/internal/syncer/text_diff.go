package syncer

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
)

var errUnsupportedTextContent = errors.New("unsupported text content")

type unsupportedTextContentError struct {
	reason string
}

func (e *unsupportedTextContentError) Error() string {
	if e == nil || e.reason == "" {
		return errUnsupportedTextContent.Error()
	}
	return fmt.Sprintf("%s: %s", errUnsupportedTextContent, e.reason)
}

func (e *unsupportedTextContentError) Unwrap() error {
	return errUnsupportedTextContent
}

func unsupportedTextContent(reason string) error {
	return &unsupportedTextContentError{reason: reason}
}

func unsupportedTextContentReason(err error) string {
	var unsupported *unsupportedTextContentError
	if errors.As(err, &unsupported) && unsupported.reason != "" {
		return unsupported.reason
	}
	return err.Error()
}

type localTextEditKind int

const (
	localTextEditInsert localTextEditKind = iota + 1
	localTextEditDelete
)

type localTextEdit struct {
	Kind   localTextEditKind
	Text   string
	Start  int
	Length int
}

func computeLocalTextEdits(baseContent, localContent string) ([]localTextEdit, error) {
	if !utf8.ValidString(baseContent) {
		return nil, unsupportedTextContent("projected base is not valid UTF-8")
	}
	if !utf8.ValidString(localContent) {
		return nil, unsupportedTextContent("local file is not valid UTF-8")
	}
	if strings.IndexByte(baseContent, 0) >= 0 {
		return nil, unsupportedTextContent("projected base contains NUL byte")
	}
	if strings.IndexByte(localContent, 0) >= 0 {
		return nil, unsupportedTextContent("local file contains NUL byte")
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(baseContent, localContent, false)
	// Character-level DiffMain keeps every in-order common character as an equality,
	// which on a local-file reconcile leaves those characters carrying the BASE
	// document's CRDT identities. When a semantically-replaced or MOVED span reuses a
	// base identity (the 'a'/'r'/'e' shared by "shared"/"local rewrite", or the "xxx"
	// shared by "abcxxx"/"xxxdef"), a concurrent remote delete of that base region
	// strips the reused characters from the new content, truncating it into mixed/lost
	// text (task #27). collapseCoincidentalReuse folds such coincidental/moved spans
	// back into the surrounding delete+insert so they get FRESH identities, while
	// preserving genuine anchors (a common prefix/suffix, and interior runs longer
	// than their flanking edits) so ordinary incremental edits keep minimal ops.
	diffs = collapseCoincidentalReuse(dmp, diffs)
	edits := make([]localTextEdit, 0, len(diffs))
	cursor := 0
	for _, diff := range diffs {
		length := utf16Length(diff.Text)
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			cursor += length
		case diffmatchpatch.DiffDelete:
			if length > 0 {
				edits = append(edits, localTextEdit{
					Kind:   localTextEditDelete,
					Text:   diff.Text,
					Start:  cursor,
					Length: length,
				})
			}
		case diffmatchpatch.DiffInsert:
			if diff.Text != "" {
				edits = append(edits, localTextEdit{
					Kind:   localTextEditInsert,
					Text:   diff.Text,
					Start:  cursor,
					Length: length,
				})
				cursor += length
			}
		}
	}
	return edits, nil
}

// collapseCoincidentalReuse rewrites a diff so its FINAL form reuses no base identity
// for semantically-new content, moved spans included (task #27). An equality run that
// is a genuine anchor keeps identity; an equality that is merely coincidental or a
// MOVED common span is folded into the surrounding delete+insert so the replaced /
// moved content gets fresh identities.
//
// The policy is deterministic dominance + movement, NOT a claim of semantic truth: an
// interior equality is "dominated" (collapsed) when its length is <= the larger of the
// delete/insert flanking it on BOTH sides — a short common run wedged between changes,
// or a common span that only aligned because character-LCS shifted it (e.g. "xxx" in
// abcxxx -> xxxdef). Genuine anchors are preserved: a common prefix (no edit before
// it) or suffix (no edit after it) is a stationary boundary anchor, and an interior
// run longer than its flanking edits is a genuine unchanged span between disjoint
// changed blocks. Unlike diffmatchpatch's DiffCleanupSemantic this performs NO
// delete/insert overlap extraction, so a span folded here is never re-carved into a
// moved equality. All lengths are counted in UTF-16 code units to match the edit
// offsets computed by computeLocalTextEdits.
func collapseCoincidentalReuse(dmp *diffmatchpatch.DiffMatchPatch, diffs []diffmatchpatch.Diff) []diffmatchpatch.Diff {
	// Structural merge only (adjacent same-type ops, delete-before-insert ordering);
	// this does not do the semantic overlap extraction that reintroduces moved spans.
	diffs = dmp.DiffCleanupMerge(diffs)
	// Iterate to a FIXPOINT: folding one equality into delete+insert grows the
	// neighbouring edit groups, which can newly dominate an equality that a single
	// pass already walked past. Repeat until a pass folds nothing.
	//
	// Termination is guaranteed by a strict decreasing measure, not merely observed on a
	// corpus: every round that folds anything removes at least one INTERIOR equality
	// (turning it into delete+insert), and DiffCleanupMerge can only factor common text
	// onto the OUTER boundaries of a merged change group — it never manufactures a NEW
	// interior equality. So the count of interior equalities strictly decreases each
	// folding round and is bounded below by zero. (Boundary prefix/suffix equalities are
	// exempt precisely because folding them lacks this monotone reduction: their affix is
	// re-extracted, so the count would not fall — that is the boundary-exemption's reason,
	// verified by TestCollapseInteriorEqualityCountStrictlyDecreases.)
	for {
		out := make([]diffmatchpatch.Diff, 0, len(diffs)+2)
		folded := false
		for i, d := range diffs {
			if d.Type != diffmatchpatch.DiffEqual {
				out = append(out, d)
				continue
			}
			leftMax := maxAdjacentEditLen(diffs, i, -1)
			rightMax := maxAdjacentEditLen(diffs, i, +1)
			eqLen := utf16Length(d.Text)
			// Keep genuine anchors: a boundary prefix/suffix (no flanking edit on a
			// side), or an interior run LONGER than the edits flanking it. Otherwise the
			// run is DOMINATED on both sides — a coincidental or short moved island — and
			// gets fresh identity. NOTE: a long non-dominated overlap that happens to be
			// a move (e.g. "LONGSPAN" in abcLONGSPAN->LONGSPANdef) is indistinguishable
			// from a genuine anchor given only (base, local); it is deliberately retained
			// (minimal-ops, legitimate concurrent-conflict semantics), so the guarantee
			// here is scoped to dominated equalities, not "all moved spans".
			//
			// The boundary exemption (leftMax==0 / rightMax==0, i.e. a common prefix or
			// suffix) is load-bearing, not cosmetic: folding a boundary anchor emits a
			// delete+insert whose common prefix/suffix DiffCleanupMerge immediately
			// re-extracts into a fresh equality, so folding boundaries could never reach a
			// fixpoint — the loop would spin forever. Interior runs have no such shared
			// affix, so folding them terminates.
			if leftMax == 0 || rightMax == 0 || eqLen > leftMax || eqLen > rightMax {
				out = append(out, d)
				continue
			}
			out = append(out,
				diffmatchpatch.Diff{Type: diffmatchpatch.DiffDelete, Text: d.Text},
				diffmatchpatch.Diff{Type: diffmatchpatch.DiffInsert, Text: d.Text},
			)
			folded = true
		}
		// Re-merge folded deletes/inserts into their neighbours (delete-before-insert
		// normalised per change group; still no overlap extraction).
		diffs = dmp.DiffCleanupMerge(out)
		if !folded {
			return diffs
		}
	}
}

// maxAdjacentEditLen returns the larger of the delete/insert edit lengths (UTF-16 code
// units) immediately adjacent to diffs[i] in the given direction (-1 = before, +1 =
// after), scanning the contiguous run of edit ops until the next equality or a
// boundary. It returns 0 when the neighbour is an equality or diffs[i] is at that
// boundary (i.e. diffs[i] is a stationary prefix/suffix anchor on that side).
func maxAdjacentEditLen(diffs []diffmatchpatch.Diff, i, dir int) int {
	del, ins := 0, 0
	for j := i + dir; j >= 0 && j < len(diffs); j += dir {
		switch diffs[j].Type {
		case diffmatchpatch.DiffDelete:
			del += utf16Length(diffs[j].Text)
		case diffmatchpatch.DiffInsert:
			ins += utf16Length(diffs[j].Text)
		default: // DiffEqual: end of the adjacent edit run.
			return maxInt(del, ins)
		}
	}
	return maxInt(del, ins)
}
