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
	// A dominated-interior fixpoint deliberately exempts boundary equalities for
	// termination, so a fully-rewritten line's trailing delimiter survives as a
	// reused-base equality. A concurrent remote whole-line delete removes that base
	// delimiter, stripping the local rewrite's terminator and concatenating it with the
	// next line (task #27 newline-loss class). freshenReplacedLineDelimiters runs ONCE
	// here, after the fixpoint and with NO later DiffCleanupMerge (which would re-extract
	// the delimiter as a common suffix), giving each rewritten line's delimiter fresh
	// identity.
	diffs = freshenReplacedLineDelimiters(diffs)
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

// collapseCoincidentalReuse folds DOMINATED interior equalities into the surrounding
// delete+insert so they get fresh CRDT identities (task #27). The operational guarantee
// is scoped precisely to dominated equalities — it does NOT claim to freshen "all
// semantically-new content" or "all moved spans," and it makes no semantic
// classification of the runs it retains.
//
// The policy is a deterministic dominance measure, not a claim of semantic truth: an
// interior equality is "dominated" (collapsed) when its length is <= the larger of the
// delete/insert flanking it on BOTH sides — a short common run wedged between changes,
// or a common span that only aligned because character-LCS shifted it (e.g. "xxx" in
// abcxxx -> xxxdef). Everything else is RETAINED, without asserting what it is: a
// boundary prefix/suffix (no edit on one side) keeps identity, and an interior run
// longer than its flanking edits keeps identity too — that longer run may be a genuine
// unchanged span between disjoint changes OR a long moved overlap; (base, local) alone
// cannot distinguish them, so it is retained (minimal ops, legitimate concurrent-
// conflict semantics), NOT claimed corruption-free. Unlike diffmatchpatch's
// DiffCleanupSemantic this performs NO delete/insert overlap extraction, so a span
// folded here is never re-carved as an interior/moved equality — the per-round
// DiffCleanupMerge DOES intentionally re-factor a common affix onto an OUTER boundary,
// where the boundary exemption recovers it as a genuine anchor (see
// TestPerRoundNormalizationRetainsFactoredBoundaryAnchor). All lengths are counted in
// UTF-16 code units to match the edit offsets computed by computeLocalTextEdits.
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
		// normalised per change group; still no overlap extraction). This per-round
		// normalization is LOAD-BEARING, not cosmetic: after a dominated fold it factors a
		// common affix onto the OUTER boundary of the merged change group, where the
		// boundary exemption recovers it as a genuine anchor. Run-summing in
		// maxAdjacentEditLen does NOT subsume it — without this merge a factored suffix/
		// prefix anchor is lost and freshened unnecessarily. Killed by
		// TestPerRoundNormalizationRetainsFactoredBoundaryAnchor (skip this -> RED); the
		// Ayxx... cascade row only kills single-pass. (The idempotence row is
		// representation-sensitive and is NOT the killer for this mechanism.)
		diffs = dmp.DiffCleanupMerge(out)
		if !folded {
			return diffs
		}
	}
}

// freshenReplacedLineDelimiters gives a line delimiter a fresh CRDT identity when it
// terminates a fully-rewritten line, so a concurrent remote whole-line delete — which
// removes the base line INCLUDING its trailing delimiter — cannot strip the local
// rewrite's terminator and concatenate it with the next line (task #27 newline-loss
// class).
//
// The dominated-interior fixpoint deliberately exempts boundary equalities for
// termination, so a rewritten line's trailing delimiter survives as a reused-base
// equality; that reused delimiter is exactly what a whole-line delete removes. This
// pass peels the leading delimiter out of an equality that terminates a fully
// rewritten line and re-emits it as an explicit delete+insert so the terminator is
// local's own identity. It runs ONCE (from computeLocalTextEdits, after the fixpoint)
// and must NOT be followed by DiffCleanupMerge: a re-merge would recombine the
// appended delete/insert with the preceding group and re-extract the common delimiter
// suffix into a reused equality, undoing the fix and reintroducing the non-termination
// the boundary exemption avoids.
//
// Scope, independent of edit kind: the delimiter is freshened iff the group before it
// (a) is a change — any delete and/or insert — and (b) starts at document start or
// immediately after a line delimiter, i.e. the WHOLE line's content was rewritten. A
// mid-line edit that leaves surviving content on the line (a partial prefix/suffix
// anchor) shares that line — and its delimiter — with retained base text, so it is NOT
// freshened. The missing delete/insert side of a one-sided group is synthesized: the
// delimiter always becomes delete-base + insert-fresh. LF and CRLF are both handled.
func freshenReplacedLineDelimiters(diffs []diffmatchpatch.Diff) []diffmatchpatch.Diff {
	out := make([]diffmatchpatch.Diff, 0, len(diffs)+2)
	for i := 0; i < len(diffs); i++ {
		d := diffs[i]
		if delim := leadingLineDelimiter(d.Text); d.Type == diffmatchpatch.DiffEqual && delim != "" && terminatesFullyRewrittenLine(diffs, i) {
			// Merge the delimiter INTO the preceding change group's delete and insert so
			// the whole rewritten line — content plus terminator — is one contiguous delete
			// and one contiguous insert. Emitting the fresh delimiter as a separate insert
			// is not enough: a concurrent remote insert at the same line position can
			// interpose between the local content and a separate delimiter insert, so the
			// terminator must be atomic with its content. The missing side of a one-sided
			// group is synthesized (delete base delimiter / insert fresh delimiter).
			var delText, insText string
			for len(out) > 0 && out[len(out)-1].Type != diffmatchpatch.DiffEqual {
				op := out[len(out)-1]
				out = out[:len(out)-1]
				if op.Type == diffmatchpatch.DiffDelete {
					delText = op.Text + delText
				} else {
					insText = op.Text + insText
				}
			}
			out = append(out,
				diffmatchpatch.Diff{Type: diffmatchpatch.DiffDelete, Text: delText + delim},
				diffmatchpatch.Diff{Type: diffmatchpatch.DiffInsert, Text: insText + delim},
			)
			if rest := d.Text[len(delim):]; rest != "" {
				out = append(out, diffmatchpatch.Diff{Type: diffmatchpatch.DiffEqual, Text: rest})
			}
			continue
		}
		out = append(out, d)
	}
	return out
}

// leadingLineDelimiter returns the line terminator ("\r\n" or "\n") at the start of s,
// or "" if s does not begin with one.
func leadingLineDelimiter(s string) string {
	switch {
	case strings.HasPrefix(s, "\r\n"):
		return "\r\n"
	case strings.HasPrefix(s, "\n"):
		return "\n"
	default:
		return ""
	}
}

// terminatesFullyRewrittenLine reports whether the change group immediately before
// diffs[i] rewrote a WHOLE line whose delimiter is diffs[i]'s leading terminator: the
// group must be a change (delete and/or insert) and must start at document start or
// right after a line delimiter. When surviving base content precedes the group on the
// same line (the preceding equality does not end in a delimiter), the edit is mid-line
// and the delimiter is shared with retained text, so it is not freshened.
func terminatesFullyRewrittenLine(diffs []diffmatchpatch.Diff, i int) bool {
	if i == 0 || diffs[i-1].Type == diffmatchpatch.DiffEqual {
		return false
	}
	j := i - 1
	for j >= 0 && diffs[j].Type != diffmatchpatch.DiffEqual {
		j--
	}
	if j < 0 {
		return true // group starts at document start
	}
	return strings.HasSuffix(diffs[j].Text, "\n")
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
