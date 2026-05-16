# Replace the document renderer with a virtualized CodeMirror surface

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. A future contributor should be able to resume from this file alone.

## Purpose / Big Picture

Notty currently renders the whole document as React markdown preview nodes. A large log document such as `codex-agent.log` turned about 75 MB of text into about 1.45 million DOM elements and about 2.25 GB of browser JavaScript heap. After this change, the frontend will render the document through CodeMirror 6, an editor toolkit that only keeps visible lines in the DOM. Users should be able to open normal markdown files and very large log/text files without the tab running out of memory, while keeping collaborative Yjs sync and anchored threads.

The observable outcome is that opening the existing large log file no longer creates hundreds of thousands of DOM nodes. The document remains editable, threads can be created from selections, thread markers appear in the editor gutter, and clicking a thread scrolls to its current CRDT-relative anchor.

## Progress

- [x] 2026-05-16T02:29:16Z Read the current renderer, Yjs sync hook, logic tests, and frontend package configuration.
- [x] 2026-05-16T02:29:16Z Confirmed the failure mode: `renderMarkdownPreview()` splits the entire document into React nodes and the browser showed about 1.45 million DOM elements for a large log.
- [x] 2026-05-16T02:42:32Z Added CodeMirror dependencies and a minimal Y.Text-to-CodeMirror binding in `frontend/src/DocumentSurface.tsx`.
- [x] 2026-05-16T02:42:32Z Replaced `DocumentEditor`'s preview/textarea dual renderer with a single CodeMirror document surface.
- [x] 2026-05-16T02:42:32Z Preserved thread creation, thread line grouping, virtualized thread markers, and sidebar jump behavior through CodeMirror decorations.
- [x] 2026-05-16T02:42:32Z Added focused regression tests for bounded DOM rendering and CRDT-relative thread markers.
- [x] 2026-05-16T02:42:32Z Ran frontend tests, typecheck, container rebuild, and browser verification on the large local document.

## Surprises & Discoveries

- Observation: The frontend container was not the memory culprit. The browser tab was.
  Evidence: Chrome reported about 2.25 GB used JS heap and about 1.45 million DOM elements, while `docker stats` showed `notty-frontend-1` around 153 MiB.

- Observation: The largest DOM subtree was `.markdown-preview`.
  Evidence: Browser inspection reported `.markdown-preview` had about 730,541 direct children and about 75 MB of body text.

- Observation: The post-change large log document no longer renders proportional DOM.
  Evidence: Browser inspection on `codex-agent.log` after the refactor reported about 217 total DOM elements, 17 visible CodeMirror lines, about 2,237 characters of rendered body text, and about 313 MB used JS heap.

- Observation: The CodeMirror editor path sends local edits through the existing Yjs websocket path.
  Evidence: Browser verification typed `cm-check` into `notes/untitled.md`, saw the document version advance, then used undo and saw the text revert with another version advance.

- Observation: The host lockfile initially became incompatible with the Docker frontend build.
  Evidence: Docker `npm ci` failed with missing lockfile entries after host npm 11 generated the lock. Regenerating `package-lock.json` with the Node 22/npm 10 Docker image fixed `npm ci`.

## Decision Log

- Decision: Use CodeMirror 6 directly instead of keeping the markdown preview and adding a size threshold.
  Rationale: The first-principle bug is unbounded rendering. A threshold fallback would keep two document models and leave the fragile full-render path in place for medium-large files. A single virtualized editor path keeps behavior consistent for markdown, logs, and plain text.
  Date/Author: 2026-05-16 / Codex

- Decision: Implement a small explicit Y.Text binding rather than introducing `y-codemirror` initially.
  Rationale: The application already owns the Yjs websocket protocol. A local binding is small: CodeMirror changes become Yjs transactions, and Y.Text observe deltas become CodeMirror changes. This keeps the integration understandable and testable.
  Date/Author: 2026-05-16 / Codex

- Decision: Keep the editor as syntax-highlighted text instead of rich markdown preview.
  Rationale: Rich markdown preview caused the unbounded rendering model. Syntax-highlighted text preserves editing and scales. A rich preview can be added later as an explicit bounded/virtualized mode if product needs it.
  Date/Author: 2026-05-16 / Codex

## Outcomes & Retrospective

Complete for this implementation pass. The document surface now uses CodeMirror instead of full React markdown rendering. The large local `codex-agent.log` no longer creates a million-node DOM, and the existing thread marker workflow is still visible on a small anchored document. Remaining future work is product-level polish: if Notty needs rich rendered markdown, it should be implemented as a separate bounded preview mode rather than reintroducing full-document React rendering.

## Context and Orientation

The frontend source lives in `frontend/src`. The main application component is `frontend/src/App.tsx`. The existing `DocumentEditor` component renders a document either as a full markdown preview or as a full `<textarea>`. The preview path calls `renderMarkdownPreview(content, ...)`, which runs `content.split("\n")` and creates React nodes for every document line. This is the immediate source of the large DOM explosion.

The Yjs sync hook is `frontend/src/useDocument.ts`. Yjs is a conflict-free replicated data type library; in this app, each document has a `Y.Doc`, and the text content is stored in `ydoc.getText("content")`. The hook currently converts the Y.Text into a full JavaScript string and stores it in React state after sync updates. It also exposes `replaceContent()` for the textarea path, which computes a text diff and mutates Y.Text.

Thread anchors are CRDT-relative positions. A relative position is an encoded Yjs location that moves as text is inserted or deleted before it. Helpers for encoding and resolving anchors live in `frontend/src/logic.ts`, especially `encodeRelativeAnchor()` and `resolveThreadAnchorLive()`.

CodeMirror 6 is an editor toolkit. It keeps only visible lines plus a small buffer in the DOM, and it supports decorations, gutters, selections, and custom update listeners. In this implementation, React should render the app shell and thread panels, while CodeMirror renders the document.

## Plan of Work

First, add the CodeMirror packages needed for a basic editor: state, view, commands, language support, markdown language, and default setup if available through individual packages. Then create a new `frontend/src/DocumentSurface.tsx` component. This component will own the `EditorView`, build an `EditorState`, and bind it to the `Y.Text` from `useDocumentSync`.

The binding must be bidirectional. When CodeMirror changes due to local typing, its update listener will apply the changed ranges to Y.Text inside a Yjs transaction with a local origin. When Y.Text changes due to remote websocket updates, a Y.Text observer will translate Yjs delta operations into CodeMirror changes and dispatch them to the editor with a remote origin. The binding must ignore its own local and remote origins to avoid loops.

Thread rendering will move into CodeMirror. The app will resolve each thread's relative anchor against the current `Y.Doc`, then create line gutter markers grouped by CodeMirror line number and inline highlight decorations for anchored ranges. Clicking a gutter marker will open the existing line-thread popover. Clicking a thread in the sidebar will scroll the CodeMirror editor to the resolved anchor and briefly highlight it.

`DocumentEditor` in `frontend/src/App.tsx` will keep the document metadata row and thread drafter UI, but remove `renderMarkdownPreview()`, `renderInlineText()`, the textarea edit mode, and full-document React rendering. It will pass callbacks and thread data into `DocumentSurface`. Thread creation will use the current CodeMirror selection and encode relative anchors immediately.

Tests will focus on fragile behavior, not getters and setters. Pure logic tests will continue covering relative anchors. A component regression test will mount the CodeMirror surface with a large generated document and assert the DOM node count stays bounded. Browser verification will log in locally, open the large `codex-agent.log`, and confirm the DOM is no longer proportional to the file size.

## Concrete Steps

Run commands from `/Users/zhongyangxia/Downloads/notty` unless otherwise noted.

Install CodeMirror dependencies in `frontend`:

    cd frontend
    npm install @codemirror/state @codemirror/view @codemirror/commands @codemirror/language @codemirror/lang-markdown @codemirror/search

After editing, run:

    cd frontend
    npm test

For local browser verification, keep the stack running with:

    docker compose up -d --build

Then open `http://localhost:5173`, log in with the local test user, open the large log document, and measure:

    performance.memory.usedJSHeapSize
    document.querySelectorAll("*").length
    document.body.innerText.length

Success is a DOM count bounded by the editor viewport rather than the number of document lines.

## Validation and Acceptance

Acceptance requires all of the following:

The frontend test command `cd frontend && npm test` passes. A new large-document component test demonstrates that mounting a generated large document does not create one DOM node per line.

In the browser, after logging in as the local test account and opening the large log file, the total DOM element count stays in the low thousands rather than hundreds of thousands or millions. The editor remains scrollable and editable.

Thread functionality remains intact. Selecting text opens the thread composer, posting creates an anchored thread, gutter markers show grouped thread counts by current editor line, and clicking a sidebar thread scrolls to the anchor.

## Idempotence and Recovery

The dependency installation is idempotent through `package-lock.json`. The frontend refactor is additive at first by creating `DocumentSurface.tsx`, then subtractive by removing the old preview/textarea path. If browser verification exposes a regression, keep `DocumentSurface.tsx` and restore the last known `DocumentEditor` call site from git while preserving tests that document the failing case.

No backend database migration is part of this plan. No local user data should be deleted.

## Artifacts and Notes

The key failure evidence from browser inspection was:

    Before:
    usedJSHeapSize: 2258122867
    total DOM elements: 1453416
    bodyTextLength: 74762246
    largest subtree: .markdown-preview with 730541 direct children

    After:
    usedJSHeapSize: 312686051
    total DOM elements: 217
    bodyTextLength: 2237
    visible CodeMirror lines: 17

Validation commands completed:

    cd frontend && npm test
    cd frontend && npm run build
    docker compose up -d --build frontend

The post-change numbers above were collected from the local browser after the implementation.

## Interfaces and Dependencies

`frontend/src/useDocument.ts` should return at least:

    {
      ydoc: Y.Doc;
      ytext: Y.Text;
      ready: boolean;
      connected: boolean;
    }

`frontend/src/DocumentSurface.tsx` should export a React component that accepts:

    documentId: string
    ydoc: Y.Doc
    ytext: Y.Text
    threads: ThreadItem[]
    focusThreadId: string
    onFocusThreadHandled: () => void
    onLineThreadsOpen: (group, point) => void
    onSelectionChange: (selection) => void

Exact type names can change during implementation, but the component must provide these behaviors: bind CodeMirror to Y.Text, show thread markers, report selections for thread creation, and scroll to focused threads.
