# Native Editor Pane

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`.

## Purpose / Big Picture

The document editor currently feels like a small embedded CodeMirror card inside the middle pane because both the page container and CodeMirror maintain scrollable regions. After this change, the editor should feel like the middle pane itself: a single primary scroll surface, centered document content, and no hover-card frame around the editor. Markdown documents should feel document-native, while CodeMirror remains the text engine so CRDT sync, selection, and thread anchors keep working.

## Progress

- [x] (2026-05-17) Confirmed the current layout has nested scroll owners: `.doc-canvas` scrolls and `.cm-scroller` also scrolls with `max-height`.
- [x] (2026-05-17) Wrote this ExecPlan to define the minimal layout change and validation approach.
- [x] (2026-05-17) Updated `frontend/src/DocumentSurface.tsx` so CodeMirror fills the pane and markdown documents do not show code-editor gutters.
- [x] (2026-05-17) Updated `frontend/src/styles.css` so the document canvas no longer creates a second editor scroll area or hover-card frame.
- [x] (2026-05-17) Added focused frontend regression coverage for markdown document mode.
- [x] (2026-05-17) Ran focused frontend tests, full frontend tests, and production build successfully.
- [x] (2026-05-17) Verified the local app in the browser on `docs/untitled.md`.
- [x] (2026-05-17) Investigated a document correctness discrepancy and found the backend WebSocket bootstrap still served stale in-memory `document.Doc` state before persisted Postgres CRDT history.
- [x] (2026-05-17) Fixed Postgres-backed document sync to ignore stale materialized `document.Doc` state and added a regression test.
- [x] (2026-05-17) Unified markdown and non-markdown editor chrome/width by using the same pane layout and removing code-editor gutters from both.
- [x] (2026-05-17) Fixed browser WebSocket bootstrap to stream raw persisted updates instead of Go-generated checkpoints, because browser Yjs could render deleted/stale content from those checkpoints.
- [x] (2026-05-17) Rebuilt the local backend and verified in a fresh browser tab that `docs/untitled.md` no longer renders the stale poem content.

## Surprises & Discoveries

- Observation: The scrollbar problem is not caused by markdown preview decorations. It is caused by CSS ownership: `.doc-canvas` is scrollable and `.cm-scroller` is also scrollable.
  Evidence: `frontend/src/styles.css` sets `.doc-canvas { overflow: auto; }`, while `frontend/src/DocumentSurface.tsx` sets `.cm-scroller { maxHeight: calc(100vh - 220px); overflow: auto; }`.

- Observation: `docs/untitled.md` rendered content that did not match the persisted Postgres CRDT history for the same document ID and update ID.
  Evidence: Browser showed poem text for `docs/untitled.md` at version `11495`, while replaying `document_updates` from Postgres with the backend CRDT library produced text beginning `# okay`.

- Observation: The mismatch was possible because `Store.encodeDocumentCheckpointSyncUpdates` used `document.Doc` if it was non-nil, even when Postgres was configured.
  Evidence: The old branch `if document.Doc != nil { EncodeStateAsUpdateV1(document.Doc, ...) }` could bypass persisted checkpoint/update history.

- Observation: Ignoring stale `document.Doc` was necessary but not sufficient for browser correctness. Browser Yjs still rendered stale/deleted text when bootstrapped from a Go-generated checkpoint.
  Evidence: After rebuilding with stale memory ignored, a fresh browser tab still showed poem text for `docs/untitled.md`; after switching human clients to raw update bootstrap, the same document rendered the persisted content beginning `# okay`.

## Decision Log

- Decision: Keep CodeMirror as the only scroll owner for the editor region instead of moving scroll ownership to an outer DOM wrapper.
  Rationale: CodeMirror uses its own scroller for virtualization, cursor geometry, selections, and IME behavior. Making the outer page scroll the editor content would fight CodeMirror and risk correctness bugs for large documents.
  Date/Author: 2026-05-17 / Codex

- Decision: Remove line-number and fold gutters for markdown live-preview documents, while keeping them for non-markdown files.
  Rationale: Markdown is the primary document-writing experience and should not look like a code panel. Non-markdown files still benefit from editor affordances.
  Date/Author: 2026-05-17 / Codex

- Decision: Remove gutters and active-line code chrome for all document surfaces, not only markdown.
  Rationale: The user explicitly reported inconsistent widths and ugly non-markdown rendering. One document pane layout is simpler and avoids bifurcated editor geometry.
  Date/Author: 2026-05-17 / Codex

- Decision: In Postgres-backed mode, never use `document.Doc` to answer WebSocket sync bootstrap.
  Rationale: Postgres is the datastore. Any in-memory materialized CRDT doc is a disposable cache and must not override persisted CRDT history.
  Date/Author: 2026-05-17 / Codex

- Decision: Browser/human document WebSocket bootstrap must stream the raw persisted update log instead of checkpoint-plus-tail.
  Rationale: Browser Yjs must receive original Yjs-compatible update frames. Go-generated checkpoints are acceptable for Go daemon/agent clients but are not safe as the browser bootstrap source.
  Date/Author: 2026-05-17 / Codex

## Outcomes & Retrospective

The editor now behaves as the middle pane rather than a nested card. CodeMirror is still the single editor scroll owner, preserving virtualization and cursor/selection correctness, but the surrounding document canvas no longer scrolls independently or draws hover-card chrome. Markdown and non-markdown documents now share the same document-pane width and omit line-number/fold gutters. Postgres-backed browser sync now uses raw persisted update history instead of stale in-memory docs or Go-generated checkpoints.

## Context and Orientation

`frontend/src/App.tsx` renders the middle pane. The document area contains `.doc-canvas`, `.doc-inner`, `.editor-frame`, and `DocumentSurface`. `frontend/src/DocumentSurface.tsx` creates a CodeMirror `EditorView` bound to a Yjs `Y.Text`, where Yjs is the shared text data structure used for collaborative CRDT sync. `frontend/src/styles.css` defines the outer pane layout. Thread anchor markers are rendered by `DocumentSurface` in `.thread-anchor-rail`, outside CodeMirror lines but positioned from CodeMirror coordinates.

The current nested-scroll behavior comes from two layers. The outer `.doc-canvas` has `overflow: auto`, and CodeMirror’s `.cm-scroller` has a max height and its own `overflow: auto`. The desired behavior is still one scroll surface, but it must be CodeMirror’s scroll surface because CodeMirror needs it for correctness and virtualization.

## Plan of Work

First, update `frontend/src/DocumentSurface.tsx` so the base CodeMirror theme uses full-height sizing instead of a fixed minimum height and viewport-based max height. Add a markdown-specific theme extension that centers document text inside the full pane. Include line numbers and folding only for non-markdown documents.

Second, update `frontend/src/styles.css` so `.doc-canvas`, `.doc-inner`, `.editor-frame`, `.document-surface-shell`, and `.document-surface` form a full-height flex chain. Remove the hover border/background that makes the editor feel like a floating card. Keep metadata chips centered above the editor content, but let CodeMirror own the rest of the pane.

Third, add a focused frontend test in `frontend/src/DocumentSurface.test.tsx` that markdown live-preview documents do not render CodeMirror gutters. This prevents regressing back into a code-editor chrome for markdown documents without over-testing basic CSS.

Finally, run the frontend tests and build, then open the local frontend in the browser. Verify a markdown document visually and inspect computed layout values to confirm there is no outer document scroll, no editor card frame, and no markdown gutters.

## Concrete Steps

From `/Users/zhongyangxia/Downloads/notty`, edit:

    frontend/src/DocumentSurface.tsx
    frontend/src/styles.css
    frontend/src/DocumentSurface.test.tsx

Run:

    cd frontend && npm run test -- DocumentSurface.test.tsx
    cd frontend && npm run build

Then open the local frontend at `http://localhost:5173` and inspect the document editor.

## Validation and Acceptance

The change is accepted when a markdown document in the browser shows one pane-filling editor surface, the document text is centered, the editor no longer shows a hover card frame, and markdown documents do not show line-number/fold gutters. Programmatic browser inspection should show `.doc-canvas` has `overflow-y: hidden` and `.cm-scroller` is the scrollable element that fills the editor region.

## Idempotence and Recovery

The layout changes are confined to frontend files and can be reverted by git if needed. The tests do not mutate external services. Browser verification can be repeated after Vite hot reload or a frontend container restart.

## Artifacts and Notes

Focused tests and build passed:

    cd frontend && npm run test -- DocumentSurface.test.tsx
    Test Files  1 passed (1)
    Tests  5 passed (5)

    cd frontend && npm run test
    Test Files  5 passed (5)
    Tests  30 passed (30)

    cd frontend && npm run build
    ✓ built in 1.40s

    go test ./backend/internal/notty
    ok   notty/backend/internal/notty

    scripts/test-postgres.sh -run TestPostgresDocumentSyncIgnoresStaleMaterializedDoc
    ok   notty/backend/internal/notty

    scripts/test-postgres.sh -run 'TestPostgres(DocumentProtocolColdBootstrapStreamsCheckpointAndTail|HumanDocumentProtocolBootstrapStreamsRawUpdates|DocumentSyncIgnoresStaleMaterializedDoc)'
    ok   notty/backend/internal/notty

Browser verification on `http://localhost:5173`, document `docs/untitled.md`, confirmed these computed values:

    .doc-canvas overflow-y: hidden
    .cm-scroller overflow-y: auto
    .document-surface-shell border-top-width: 0px
    .cm-gutters present: false
    .cm-activeLine count: 0

After the sync fix, browser verification in a fresh tab showed:

    docs/untitled.md visible content starts with: # okay
    docs/untitled.md no longer shows the stale poem text
    markdown and non-markdown .cm-content width: 712px
    markdown and non-markdown .cm-content padding-left/right: 32px / 32px
    markdown and non-markdown .cm-gutters present: false

## Interfaces and Dependencies

No backend API changes are required. The relevant frontend dependency is CodeMirror through `@codemirror/view`; `EditorView.theme` remains the styling interface. The markdown live-preview flag is already passed through `DocumentSurface` as `enableMarkdownLivePreview`.
