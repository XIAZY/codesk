// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import * as Y from "yjs";
import { encodeRelativeAnchor, resolveThreadAnchorLive, uint8ArrayToBase64 } from "./logic";

// Signed anchor-continuity regression matrix (AlphaToad's 9 ruled rows). Each row
// drives a real Y.Doc + real range anchor through a realistic edit and asserts
// resolveThreadAnchorLive's `resolved` verdict on BOTH criterion paths:
//   • NEW    = assoc-fixed edges (start right, end LEFT) + stateAtAnchor → the
//     span-empty continuity criterion (adoption; a range that empties is
//     permanently dead — "a moment of nothing" is structural).
//   • LEGACY = old symmetric assoc (both right), no field → token-overlap fallback,
//     kept for pre-fix anchors during natural migration.
// resolved === true => alive/anchored; false => dead ("Anchor lost").

function anchorOn(doc: Y.Doc, text: Y.Text, needle: string, encoding: "new" | "legacy") {
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
  return {
    kind: "text-range",
    relativeStart: encodeRelativeAnchor(text, from, "start"),
    relativeEnd: encodeRelativeAnchor(text, to, "start"), // old: end also right-associated
    excerpt: needle,
  };
}

function resolvedAfter(seed: string, needle: string, edit: (d: Y.Doc, t: Y.Text) => void, encoding: "new" | "legacy") {
  const doc = new Y.Doc();
  const text = doc.getText("content");
  doc.transact(() => text.insert(0, seed));
  const anchor = anchorOn(doc, text, needle, encoding);
  edit(doc, text);
  return resolveThreadAnchorLive(anchor, doc, text.toString()).resolved;
}

const S = "big fox jumps over the lazy dog";
const N = "big fox";

// newAlive / legacyAlive = signed expected `resolved` on each path.
const ROWS: Array<{
  name: string;
  edit: (d: Y.Doc, t: Y.Text) => void;
  newAlive: boolean;
  legacyAlive: boolean;
}> = [
  { name: "1 type at END (no growth)", newAlive: true, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf(N) + N.length; d.transact(() => t.insert(i, "123")); } },
  { name: "2 type at START (no growth)", newAlive: true, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => t.insert(i, "X")); } },
  { name: "3 insert MIDDLE (adopt/grow)", newAlive: true, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf("big ") + 4; d.transact(() => t.insert(i, "brown ")); } },
  // Row 4 is Alpha's overrule + the red-first flip: NEW alive (survivor "brown"
  // keeps the region), LEGACY dead (token-overlap can't adopt).
  { name: "4 grow then delete big+fox (ALIVE new)", newAlive: true, legacyAlive: false,
    edit: (d, t) => { d.transact(() => { const i = t.toString().indexOf("big ") + 4; t.insert(i, "brown "); }); d.transact(() => { let s = t.toString(); t.delete(s.indexOf("big "), 4); s = t.toString(); t.delete(s.indexOf("fox"), 3); }); } },
  { name: "5 delete exact range (dead)", newAlive: false, legacyAlive: false,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => t.delete(i, N.length)); } },
  { name: "6 delete whole doc (dead)", newAlive: false, legacyAlive: false,
    edit: (d, t) => { d.transact(() => t.delete(0, t.length)); } },
  // 7a/7b are Eva's honesty floor: a range that emptied is permanently dead, so BOTH
  // unrelated retype and IDENTICAL retype are dead under the new criterion.
  { name: "7a delete then type unrelated (dead)", newAlive: false, legacyAlive: false,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => { t.delete(i, N.length); t.insert(i, "red cat"); }); } },
  { name: "7b delete then retype IDENTICAL (dead new)", newAlive: false, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf(N); d.transact(() => { t.delete(i, N.length); t.insert(i, "big fox"); }); } },
  { name: "8 typo fix inside (alive)", newAlive: true, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf("fox"); d.transact(() => { t.delete(i, 1); t.insert(i, "F"); }); } },
  { name: "9 delete half leave fox (alive)", newAlive: true, legacyAlive: true,
    edit: (d, t) => { const i = t.toString().indexOf("big "); d.transact(() => t.delete(i, 4)); } },
];

describe("anchor-continuity matrix (NEW=span-empty, LEGACY=token)", () => {
  for (const row of ROWS) {
    it(`${row.name} — new encoding`, () => {
      expect(resolvedAfter(S, N, row.edit, "new")).toBe(row.newAlive);
    });
    it(`${row.name} — legacy encoding`, () => {
      expect(resolvedAfter(S, N, row.edit, "legacy")).toBe(row.legacyAlive);
    });
  }
});
