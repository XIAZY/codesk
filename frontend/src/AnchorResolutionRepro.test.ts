// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import * as Y from "yjs";
import { encodeRelativeAnchor, resolveThreadAnchorLive, uint8ArrayToBase64 } from "./logic";

// Signed anchor-orphan hybrid regression matrix (this incident's reproduction
// artifact). Each row drives a real Y.Doc + real range anchor through a realistic
// edit and asserts resolveThreadAnchorLive's `resolved` verdict on BOTH criterion
// paths — pinning the hybrid seam:
//   • NEW anchors carry stateAtAnchor → item-identity walk (correct by construction).
//   • LEGACY anchors (no field) → token-overlap fallback.
// Red-first: against the pre-fix code (which returned resolved:true unconditionally)
// every orphaned expectation here fails.
//
// resolved === true  => anchored;  resolved === false => orphaned ("Anchor lost").

function anchorOn(doc: Y.Doc, text: Y.Text, needle: string, withStateVector: boolean) {
  const from = text.toString().indexOf(needle);
  const to = from + needle.length;
  return {
    kind: "text-range",
    relativeStart: encodeRelativeAnchor(text, from),
    relativeEnd: encodeRelativeAnchor(text, to),
    excerpt: needle,
    stateAtAnchor: withStateVector ? uint8ArrayToBase64(Y.encodeStateVector(doc)) : undefined,
  };
}

function resolvedAfter(seed: string, needle: string, edit: (doc: Y.Doc, text: Y.Text) => void, withStateVector: boolean) {
  const doc = new Y.Doc();
  const text = doc.getText("content");
  doc.transact(() => text.insert(0, seed));
  const anchor = anchorOn(doc, text, needle, withStateVector);
  edit(doc, text);
  return resolveThreadAnchorLive(anchor, doc, text.toString()).resolved;
}

const SEED = "The quick brown fox jumps over the lazy dog";
const NEEDLE = "brown fox";

// newAnchored / legacyAnchored = the signed expected `resolved` on each path.
const ROWS: Array<{
  name: string;
  edit: (doc: Y.Doc, text: Y.Text) => void;
  newAnchored: boolean;
  legacyAnchored: boolean;
}> = [
  { name: "1 delete exact range", newAnchored: false, legacyAnchored: false,
    edit: (d, t) => { const i = t.toString().indexOf(NEEDLE); d.transact(() => t.delete(i, NEEDLE.length)); } },
  { name: "2 delete whole doc", newAnchored: false, legacyAnchored: false,
    edit: (d, t) => { d.transact(() => t.delete(0, t.length)); } },
  { name: "3a delete front-overlap (' fox' survives)", newAnchored: true, legacyAnchored: true,
    edit: (d, t) => { const i = t.toString().indexOf("quick brown"); d.transact(() => t.delete(i, "quick brown".length)); } },
  { name: "3b delete back-overlap ('brown ' survives)", newAnchored: true, legacyAnchored: true,
    edit: (d, t) => { const i = t.toString().indexOf("fox jumps"); d.transact(() => t.delete(i, "fox jumps".length)); } },
  { name: "4 select-all retype", newAnchored: false, legacyAnchored: false,
    edit: (d, t) => { d.transact(() => { t.delete(0, t.length); t.insert(0, "Completely different words"); }); } },
  { name: "5 delete then type unrelated (Alpha's bug)", newAnchored: false, legacyAnchored: false,
    edit: (d, t) => { const i = t.toString().indexOf(NEEDLE); d.transact(() => { t.delete(i, NEEDLE.length); t.insert(i, "red cat"); }); } },
  // 5b is the documented transition divergence: identity orphans (original chars
  // are strangers), token-overlap anchors (identical text, err-toward-life).
  { name: "5b delete then retype SAME text", newAnchored: false, legacyAnchored: true,
    edit: (d, t) => { const i = t.toString().indexOf(NEEDLE); d.transact(() => { t.delete(i, NEEDLE.length); t.insert(i, "brown fox"); }); } },
  { name: "6 edit inside range ('brown very fox')", newAnchored: true, legacyAnchored: true,
    edit: (d, t) => { const i = t.toString().indexOf("brown ") + "brown ".length; d.transact(() => t.insert(i, "very ")); } },
  { name: "6b typo fix inside range", newAnchored: true, legacyAnchored: true,
    edit: (d, t) => { const i = t.toString().indexOf("brown") + 1; d.transact(() => { t.delete(i, 1); t.insert(i, "R"); }); } },
  // 7 requires the clock filter on the identity path; token-overlap catches it too
  // because the inserted text ("NEW") shares no token with the excerpt.
  { name: "7 insert inside then delete all originals", newAnchored: false, legacyAnchored: false,
    edit: (d, t) => {
      d.transact(() => { const i = t.toString().indexOf("brown") + 1; t.insert(i, "NEW"); });
      d.transact(() => {
        const bi = t.toString().indexOf("bNEW"); t.delete(bi, 1);
        const ri = t.toString().indexOf("rown fox"); t.delete(ri, "rown fox".length);
      });
    } },
];

describe("anchor-orphan hybrid regression matrix (NEW=identity, LEGACY=token)", () => {
  for (const row of ROWS) {
    it(`${row.name} — new anchor (identity walk)`, () => {
      expect(resolvedAfter(SEED, NEEDLE, row.edit, true)).toBe(row.newAnchored);
    });
    it(`${row.name} — legacy anchor (token overlap)`, () => {
      expect(resolvedAfter(SEED, NEEDLE, row.edit, false)).toBe(row.legacyAnchored);
    });
  }
});
