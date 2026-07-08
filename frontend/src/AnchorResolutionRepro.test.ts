// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import * as Y from "yjs";
import { encodeRelativeAnchor, resolveThreadAnchorLive, uint8ArrayToBase64 } from "./logic";

// Signed anchor-continuity regression matrix (AlphaToad's 9 ruled rows). Each row
// drives a real Y.Doc + real range anchor through a realistic edit and asserts
// resolveThreadAnchorLive on BOTH criterion paths:
//   • NEW    = assoc-fixed edges (start right, end LEFT) + stateAtAnchor → the
//     span-empty continuity criterion (adoption; a range that empties is
//     permanently dead — "a moment of nothing" is structural).
//   • LEGACY = old symmetric assoc (both right), no field → token-overlap fallback.
// The criterion keys on the encoding's own geometry (relativeEnd.assoc), not on the
// field, so the #102 morning cohort (vector + old geometry) routes correctly.
// resolved === true => alive/anchored; false => dead ("Anchor lost").

function anchorOn(doc: Y.Doc, text: Y.Text, needle: string, encoding: "new" | "legacy" | "cohort") {
  const from = text.toString().indexOf(needle);
  const to = from + needle.length;
  if (encoding === "new") {
    return {
      kind: "text-range",
      relativeStart: encodeRelativeAnchor(text, from, "start"),
      relativeEnd: encodeRelativeAnchor(text, to, "end"),
      excerpt: needle,
      stateAtAnchor: uint8ArrayToBase64(Y.encodeStateVector(doc)),
    };
  }
  // "cohort" = the #102 morning cohort: a state vector IS present, but the geometry
  // is OLD (end also right-associated). The criterion must key on the geometry, not
  // the field, and route these to the legacy path.
  return {
    kind: "text-range",
    relativeStart: encodeRelativeAnchor(text, from, "start"),
    relativeEnd: encodeRelativeAnchor(text, to, "start"), // old: end also right-associated
    excerpt: needle,
    ...(encoding === "cohort" ? { stateAtAnchor: uint8ArrayToBase64(Y.encodeStateVector(doc)) } : {}),
  };
}

function resolveAnchorAfter(seed: string, needle: string, edit: (d: Y.Doc, t: Y.Text) => void, encoding: "new" | "legacy" | "cohort") {
  const doc = new Y.Doc();
  const text = doc.getText("content");
  doc.transact(() => text.insert(0, seed));
  const anchor = anchorOn(doc, text, needle, encoding);
  edit(doc, text);
  return resolveThreadAnchorLive(anchor, doc, text.toString());
}

const S = "big fox jumps over the lazy dog";
const N = "big fox";

// newAlive / legacyAlive = signed expected `resolved` on each path.
// newSpan = the signed resolved EXTENT (excerpt) on the new path — asserted where
// Alpha's ruling is about the highlighted range, not just aliveness (rows 1/2 pin
// the "test123" growth bug; 3 pins adopted extent; 9 pins shrunk extent).
const ROWS: Array<{
  name: string;
  edit: (d: Y.Doc, t: Y.Text) => void;
  newAlive: boolean;
  legacyAlive: boolean;
  newSpan?: string;
}> = [
  { name: "1 type at END (no growth)", newAlive: true, legacyAlive: true, newSpan: "big fox",
    edit: (d, t) => { const i = t.toString().indexOf(N) + N.length; d.transact(() => t.insert(i, "123")); } },
  { name: "2 type at START (no growth)", newAlive: true, legacyAlive: true, newSpan: "big fox",
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => t.insert(i, "X")); } },
  { name: "3 insert MIDDLE (adopt/grow)", newAlive: true, legacyAlive: true, newSpan: "big brown fox",
    edit: (d, t) => { const i = t.toString().indexOf("big ") + 4; d.transact(() => t.insert(i, "brown ")); } },
  { name: "4 grow then delete big+fox (ALIVE new)", newAlive: true, legacyAlive: false,
    edit: (d, t) => { d.transact(() => { const i = t.toString().indexOf("big ") + 4; t.insert(i, "brown "); }); d.transact(() => { let s = t.toString(); t.delete(s.indexOf("big "), 4); s = t.toString(); t.delete(s.indexOf("fox"), 3); }); } },
  { name: "5 delete exact range (dead)", newAlive: false, legacyAlive: false,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => t.delete(i, N.length)); } },
  { name: "6 delete whole doc (dead)", newAlive: false, legacyAlive: false,
    edit: (d, t) => { d.transact(() => t.delete(0, t.length)); } },
  { name: "7a delete then type unrelated (dead)", newAlive: false, legacyAlive: false,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => { t.delete(i, N.length); t.insert(i, "red cat"); }); } },
  { name: "7b delete then retype IDENTICAL (dead new)", newAlive: false, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => { t.delete(i, N.length); t.insert(i, "big fox"); }); } },
  { name: "8 typo fix inside (alive)", newAlive: true, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf("fox"); d.transact(() => { t.delete(i, 1); t.insert(i, "F"); }); } },
  { name: "9 delete half leave fox (alive)", newAlive: true, legacyAlive: true, newSpan: "fox",
    edit: (d, t) => { const i = t.toString().indexOf("big "); d.transact(() => t.delete(i, 4)); } },
];

describe("anchor-continuity matrix (NEW=span-empty, LEGACY=token)", () => {
  for (const row of ROWS) {
    it(`${row.name} — new encoding`, () => {
      const r = resolveAnchorAfter(S, N, row.edit, "new");
      expect(r.resolved).toBe(row.newAlive);
      if (row.newSpan !== undefined) {
        // Extent, not just aliveness: pins the growth bug — text at the boundary
        // must stay OUTSIDE the anchor, adoption/shrink must move the range.
        expect(r.excerpt).toBe(row.newSpan);
        expect(r.end - r.start).toBe(row.newSpan.length);
      }
    });
    it(`${row.name} — legacy encoding`, () => {
      expect(resolveAnchorAfter(S, N, row.edit, "legacy").resolved).toBe(row.legacyAlive);
    });
  }
});

// Tom's cohort finding: anchors created between the #102 deploy and this fix carry
// a state vector but OLD geometry. Keying on the field (not the assoc) would route
// them to span-empty and bring the irrelevant-text bug back for exactly the threads
// AlphaToad made while reporting it. The assoc discriminator routes them to legacy.
describe("morning cohort (#102 anchors: state vector + OLD geometry)", () => {
  it("delete then type unrelated → DEAD via legacy token-overlap, not span-empty", () => {
    const edit = (d: Y.Doc, t: Y.Text) => {
      const i = t.toString().indexOf(N);
      d.transact(() => { t.delete(i, N.length); t.insert(i, "red cat"); });
    };
    expect(resolveAnchorAfter(S, N, edit, "cohort").resolved).toBe(false);
  });
});
