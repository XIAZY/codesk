# Render document threads in an annotation rail

This ExecPlan is a living document. It follows `.agent/PLANS.md` and must keep `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` current.

## Purpose / Big Picture

Users need thread anchors to feel attached to text without becoming part of the editable document. Today the frontend inserts thread chips directly into CodeMirror lines. This shifts document content and makes comments look like text. After this change, CRDT-relative thread anchors still move with the document, but they render as a right-side annotation rail outside the editable text flow. A user can open a document with anchored threads, see subtle text highlights, and click rail badges aligned to the current line to open thread details.

## Progress

- [x] (2026-05-16 23:15Z) Read the current CodeMirror thread rendering code in `frontend/src/DocumentSurface.tsx` and confirmed it uses inline `WidgetType` chips.
- [x] (2026-05-16 23:15Z) Created this plan before implementation because the change is a non-trivial frontend refactor.
- [x] (2026-05-16 23:14Z) Replaced inline thread widgets with rail markers derived from CRDT-relative positions.
- [x] (2026-05-16 23:14Z) Updated CSS so the rail is visually separate from editable text and remains usable with multiple threads on the same line.
- [x] (2026-05-16 23:16Z) Updated frontend tests to assert markers render outside the CodeMirror content flow and move after CRDT-relative anchors move.
- [x] (2026-05-16 23:16Z) Ran frontend tests and build successfully.
- [x] (2026-05-16 23:16Z) Verified in a browser by opening a document with existing threads and clicking a rail marker.

## Surprises & Discoveries

- Observation: The current implementation stores the correct CRDT-native data, but renders it in the wrong layer.
  Evidence: `frontend/src/DocumentSurface.tsx` resolves `relativeStart` and `relativeEnd` against the live `Y.Doc`, then inserts a `ThreadChipWidget` at `line.from`.

- Observation: jsdom cannot always return DOM coordinates from CodeMirror `coordsAtPos`, even when the CRDT anchor resolves correctly.
  Evidence: The first test run rendered the text highlight but no rail marker. The implementation now falls back to CodeMirror `lineBlockAt` for tests and other cases where browser coordinates are temporarily unavailable.

## Decision Log

- Decision: Keep `relativeStart` and `relativeEnd` as the only source of truth for anchored thread position.
  Rationale: Yjs relative positions move with CRDT edits. Persisting line numbers or pixel positions would become stale and violate the current CRDT model.
  Date/Author: 2026-05-16 / Codex

- Decision: Render text-range highlights inside CodeMirror, but render clickable thread badges in a React overlay rail.
  Rationale: Highlighting is a text decoration, while opening/comment navigation is metadata UI. Separating them prevents the UI from changing document layout.
  Date/Author: 2026-05-16 / Codex

- Decision: Render rail markers only for the current CodeMirror viewport.
  Rationale: Large files such as logs must not create one marker or expensive coordinate lookup for every possible line. The rail is a visible projection, so offscreen markers can be recomputed when scrolling brings them into view.
  Date/Author: 2026-05-16 / Codex

## Outcomes & Retrospective

Implemented. Thread badges now sit in a right-side rail next to the editor, while anchored text remains subtly highlighted in CodeMirror. The UI still derives marker line and pixel positions from CRDT-relative anchors at render time, so markers move when text is inserted before the anchor. Tests and browser verification passed.

## Context and Orientation

The frontend document editor lives in `frontend/src/DocumentSurface.tsx`. It receives a live `Y.Doc` and `Y.Text` from `frontend/src/useDocument.ts`. A `Y.Doc` is the in-browser CRDT document. A CRDT-relative position is a Yjs anchor that can be resolved against the current document state to produce an absolute text offset. The backend stores thread anchors as `relativeStart` and `relativeEnd`; those fields should remain unchanged.

The current rendering flow in `DocumentSurface.tsx` creates a CodeMirror editor from `ytext.toString()`, observes Y.Text deltas, and applies thread decorations through a CodeMirror `StateField`. The old UI also defines `ThreadChipWidget`, a CodeMirror widget inserted at each anchored line. That widget is the part to remove.

## Plan of Work

First, update `DocumentSurface.tsx` so `applyThreadDecorations` only creates `Decoration.mark` ranges for resolved text anchors. Add a `ThreadRailMarker` type and a React state array to hold markers. Each marker will contain a derived line number, a y-position relative to the document surface shell, and the live threads grouped on that line.

Second, compute rail markers from the same `resolveThreadAnchorForEditor` function that powers highlights. For each resolved anchor, use the current CodeMirror document to derive the current line. Use `view.coordsAtPos(line.from)` and the shell element rectangle to derive the visible y-position. If a line is offscreen and CodeMirror cannot return coordinates, skip rendering that marker until scrolling brings it into view. This keeps the rail cheap for large files.

Third, schedule marker/decorator refreshes after document changes, CRDT updates, thread list changes, viewport changes, scroll, and window resize. This makes marker positions a live projection of the current CRDT state rather than cached line numbers.

Fourth, update CSS in `frontend/src/styles.css` so `.document-surface-shell` has space for a right rail, `.thread-anchor-rail` overlays the side, and `.thread-rail-marker` is a compact clickable badge.

Finally, update `frontend/src/DocumentSurface.test.tsx` to assert that a CRDT-relative marker renders as a rail button and that CodeMirror content remains virtualized. Run tests, build, and verify with the browser against local `http://localhost:5173`.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run:

    npm test

from `frontend/` and expect all frontend tests to pass.

Observed:

    Test Files  4 passed (4)
    Tests  20 passed (20)

Run:

    npm run build

from `frontend/` and expect Vite to complete successfully. The existing large chunk warning is acceptable for this change.

Observed:

    ✓ 96 modules transformed.
    ✓ built in 1.26s

Verified in browser by opening `http://localhost:5173`, selecting `notes/untitled.md`, and checking that anchored threads appear as right-rail badges instead of inline chips in the editor text. A browser DOM check showed:

    [{"label":"1 thread on line 3","inEditor":false,"rail":true},{"label":"2 threads on line 12","inEditor":false,"rail":true}]

## Validation and Acceptance

Acceptance is behavioral. A thread anchored to text should highlight the selected text but should not insert a visible chip into the line content. A badge should appear in the rail aligned to the current resolved line. Clicking the badge should open the existing thread popover. Switching documents and editing text should not break syncing.

Tests should cover the fragile part: CRDT-relative anchors still resolve and render after the UI moves from inline widgets to rail markers. Browser verification should cover the visual interaction because jsdom cannot fully simulate CodeMirror layout.

## Idempotence and Recovery

The changes are frontend-only. If a refresh or test fails, rerun `npm test` and `npm run build` after fixing the compile or assertion error. The backend data model is unchanged, so no migration or data reset is needed. If the rail implementation causes visual regressions, reverting `frontend/src/DocumentSurface.tsx` and `frontend/src/styles.css` returns to the prior inline widget behavior.

## Artifacts and Notes

This plan will be updated with test output and browser observations after implementation.

## Interfaces and Dependencies

`DocumentSurface` keeps the same external props. Internally it should expose no new backend API and should not change `ThreadItem` or `ThreadAnchor` types.

The key internal helper signatures at completion should be:

    function applyThreadDecorations(view: EditorView, ydoc: Y.Doc, threads: ThreadItem[]): DecorationSet projection through CodeMirror dispatch

    function computeThreadRailMarkers(view: EditorView, shell: HTMLElement, ydoc: Y.Doc, threads: ThreadItem[]): ThreadRailMarker[]

These helpers derive display state from the live CRDT document and do not persist any derived line or pixel data.
