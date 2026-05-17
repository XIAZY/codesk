# Source-native CodeMirror Markdown live preview

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. It is self-contained for a contributor who has only this checkout and no prior conversation context.

## Purpose / Big Picture

Notty stores document text as raw Markdown in a Yjs `Y.Text` CRDT, and document threads are anchored with Yjs relative positions into that raw text. A rich editor that converts Markdown into ProseMirror, Quill Delta, or block JSON makes those anchors lossy because selected rendered text has to be translated back into Markdown source offsets. This work keeps raw Markdown as the only document model and improves the editor visually by adding a CodeMirror 6 live-preview layer. After this change, users should see headings, emphasis, links, quotes, lists, and code styled inside the editor while selections and thread anchors still refer to exact Markdown source offsets.

The user-visible result is a Markdown editor that feels more like a document editor without sacrificing external editability. Agents and humans can still edit the same Markdown files as plaintext, and comments still anchor to exact ranges.

## Progress

- [x] (2026-05-16 19:12Z) Removed the abandoned Milkdown experiment while preserving unrelated `homepage/index.html` work.
- [x] (2026-05-16 19:18Z) Researched CodeMirror 6, HyperMD, Markdown syntax nodes, and the existing `DocumentSurface` implementation.
- [x] (2026-05-16 19:22Z) Implemented a Notty-specific CodeMirror live-preview extension that styles raw Markdown source without changing the document model.
- [x] (2026-05-16 19:46Z) Added focused tests for fragile source-offset behavior, command behavior, GFM task/list widgets, URLs/images, and large Markdown virtualization.
- [x] (2026-05-16 19:23Z) Verified frontend tests and production build.
- [x] (2026-05-16 19:31Z) Verified visually in a local browser against a Markdown document with headings, emphasis, strikethrough, inline code, links, blockquotes, lists, fenced code, and tables.
- [x] (2026-05-16 19:33Z) Limited the preview extension to `.md` and `.markdown` files after browser verification showed there is no product value in parsing logs as Markdown.
- [x] (2026-05-16 19:49Z) Verified GFM task checkboxes, bullet lists, ordered lists, bare URLs, angle autolinks, and image alt placeholders in a local browser.
- [x] (2026-05-17 00:14Z) Restored consistent active-line plaintext behavior for GFM task list markers: active task lines show raw `- [x]`, inactive task lines show checkbox widgets.
- [x] (2026-05-17 00:21Z) Simplified the integration surface: centralized Markdown path detection, renamed the preview prop to `enableMarkdownLivePreview`, hid Markdown formatting controls for non-Markdown files, removed unused command exports, and normalized editor CSS variables.
- [x] (2026-05-17 00:35Z) Switched the editor body and source markers from monospace to the app's sans-serif font; monospace remains only for code spans and fenced code.

## Surprises & Discoveries

- Observation: CodeMirror's Markdown parser exposes useful source nodes for common Markdown constructs. For example, `# Title` yields `ATXHeading1` and `HeaderMark`, `**bold**` yields `StrongEmphasis` and `EmphasisMark`, and `[link](url)` yields `Link`, `LinkMark`, and `URL`.
  Evidence: A local parser inspection showed nodes with exact offsets such as `StrongEmphasis 17-25 "**bold**"` and `URL 47-61 "https://x.test"`.

- Observation: CodeMirror decorations can hide or replace ranges while the underlying document remains unchanged, but dynamically computed viewport decorations must not introduce block widgets or replacing decorations that cover line breaks.
  Evidence: CodeMirror's reference docs state that decorations computed from functions after viewport computation must not introduce layout-changing block widgets or replacements covering line breaks.

- Observation: The current `DocumentSurface` already virtualizes large files and already stores thread highlights as source-offset decorations resolved from Yjs relative anchors.
  Evidence: `frontend/src/DocumentSurface.test.tsx` includes a 50,000-line regression asserting the editor does not render one DOM node per document line.

- Observation: Applying live-preview Markdown parsing to every file is not first-principle-compliant because `.log` and non-Markdown files do not benefit from Markdown styling but can be large and high-churn.
  Evidence: Browser verification initially opened `codex-agent.log`; the UI did not need Markdown preview there, and the product requirement is specifically Markdown WYSIWYG behavior. The implementation now enables the preview extension only for `.md` and `.markdown` paths.

- Observation: GFM task items are parsed as a `ListMark` plus a `TaskMarker`, not as a single checkbox node. The list marker should be hidden for inactive task lines and the task marker should become the visible checkbox.
  Evidence: Parser inspection showed `ListMark 42-43 "-"` followed by `TaskMarker 44-47 "[ ]"` for `- [ ] nested todo`.

- Observation: The task-list preview path must follow the same active-line rule as headings, links, and emphasis. If a task line is active, both the list marker and the `[ ]` or `[x]` marker need to remain visible as plaintext.
  Evidence: The focused regression `reveals GFM task source markers when the task line is active` now asserts the active line contains `- [x] active task`, while the inactive task line still renders as a clickable checkbox.

- Observation: The app integration must not leak Markdown-specific behavior into non-Markdown documents.
  Evidence: Before cleanup, the selection toolbar offered `H1`, `Bold`, `Link`, `Quote`, and `List` for every document type even though the live-preview extension only ran for Markdown files. The toolbar now keeps `Open thread` globally and shows formatting controls only when `isMarkdownDocumentPath(document.path)` is true.

## Decision Log

- Decision: Do not fork HyperMD or keep Milkdown. Build a small Notty-specific CodeMirror 6 extension.
  Rationale: HyperMD is CodeMirror 5-era code and porting it would be a rewrite. Milkdown/ProseMirror creates a rendered-tree-to-Markdown-source mapping problem that is too lossy for exact CRDT anchors.
  Date/Author: 2026-05-16 / Codex

- Decision: Keep raw Markdown source as the only canonical model and use CodeMirror decorations only for visual treatment.
  Rationale: This preserves Yjs relative position correctness, external file edits, and agent filesystem edits.
  Date/Author: 2026-05-16 / Codex

- Decision: Start with conservative inline marker hiding and line/block styling instead of full WYSIWYG replacement widgets.
  Rationale: Simple inline replacements are less likely to break cursor movement, browser input, and source offsets. Full block widgets can be added later only after correctness is proven.
  Date/Author: 2026-05-16 / Codex

- Decision: Enable the live-preview extension only for Markdown document paths.
  Rationale: Parsing and decorating logs or arbitrary plaintext as Markdown is unnecessary overhead and can make high-churn files feel worse. Source-native comments and editing still work for all document types without this visual layer.
  Date/Author: 2026-05-16 / Codex

- Decision: Render inactive GFM task markers as real checkbox widgets that update the raw Markdown marker, but reveal raw `[ ]` / `[x]` source when the line is active.
  Rationale: This gives users the expected GitHub-style checkbox interaction while keeping source offsets exact and editable.
  Date/Author: 2026-05-16 / Codex

- Decision: Name the editor prop `enableMarkdownLivePreview` rather than `markdownPreview`.
  Rationale: The prop is not a mode switch or alternate document representation. It only enables Markdown-specific CodeMirror parsing and decoration extensions while the raw source remains canonical.
  Date/Author: 2026-05-17 / Codex

- Decision: Keep Markdown detection as a small shared helper, `isMarkdownDocumentPath(path)`.
  Rationale: A single helper avoids scattered regex checks and keeps document-type gating explicit without adding a registry or plugin abstraction before it is needed.
  Date/Author: 2026-05-17 / Codex

- Decision: Use the app's sans-serif font for normal editor text and active Markdown source markers.
  Rationale: The editor is a writing surface, not a terminal. Keeping monospace only for inline code and fenced code preserves code readability without making regular prose look technical and heavy.
  Date/Author: 2026-05-17 / Codex

- Decision: Render image syntax as styled alt text rather than loading remote images inline.
  Rationale: Loading remote images would introduce layout shifts, network behavior, and security/privacy concerns. Alt-text placeholders cover the common readability case without changing the source model.
  Date/Author: 2026-05-16 / Codex

## Outcomes & Retrospective

Implemented the first Notty-native CodeMirror Markdown live-preview layer. It preserves raw Markdown as canonical `Y.Text`, styles common Markdown constructs with CodeMirror decorations, reveals raw syntax on the active line or selected range, and keeps existing CRDT thread anchor resolution source-native. The implementation intentionally avoids ProseMirror, Quill, Editor.js, hidden metadata, and Markdown serialization.

Tests and build passed. Browser verification showed the feature working on real local Markdown documents. The browser also exposed that applying the extension to logs would be wasteful, so the integration now only enables it for `.md` and `.markdown` files. A regression now covers that large Markdown documents still stay virtualized with live preview enabled. GFM task lists now render as checkboxes and can toggle the underlying `[ ]` / `[x]` source marker when inactive, while active task lines reveal the raw Markdown source consistently with other previewed syntax. The integration is now explicit about Markdown-only behavior: non-Markdown documents keep the same source-native editor and thread selection flow without Markdown formatting controls.

## Context and Orientation

The frontend lives in `frontend/src`. The current editor component is `frontend/src/DocumentSurface.tsx`. It creates a CodeMirror 6 `EditorView`, initializes it with `ytext.toString()`, listens to CodeMirror document changes, and applies those changes into the Yjs `Y.Text`. It also observes Yjs events and applies remote edits back into CodeMirror. This two-way bridge must remain source-native.

Threads use anchors stored as `relativeStart` and `relativeEnd` in `ThreadItem.anchor`. `DocumentSurface` resolves these with `Y.createAbsolutePositionFromRelativePosition` and draws CodeMirror decorations over the resolved source offsets. A "source offset" means a zero-based character position in the raw Markdown string.

CodeMirror decorations are visual overlays. A mark decoration adds CSS classes to a text range. A replace decoration can hide or replace a text range in the rendered editor while the underlying document still contains the original characters. A line decoration can add a CSS class to an entire visual line.

## Plan of Work

Add a new module `frontend/src/markdownLivePreview.ts`. This module will export `nottyMarkdownLivePreview()` for CodeMirror and `markdownPreviewCommand(name)` for toolbar and tests. The extension will inspect the Markdown syntax tree for visible ranges and create decorations:

Headings get line classes such as `cm-md-heading cm-md-heading-1`, and the leading hash marker is dimmed or hidden when inactive. Emphasis and strong emphasis get text styling and marker hiding when inactive. Strikethrough is supported when the Markdown parser exposes strikethrough nodes. Inline code gets code styling but keeps backticks visible when active. Links show label text and hide URL syntax when inactive. Lists and blockquotes get line styling while preserving source markers. Fenced code blocks get line styling and fence marker styling but will not be replaced with nested editors.

The extension will define "active" as any Markdown construct that intersects the current selection or cursor line. Active constructs reveal their raw syntax so editing is predictable. Inactive constructs can hide safe inline markers.

Integrate the extension into `DocumentSurface` after `markdown()` and before thread decorations, but only when `enableMarkdownLivePreview` is true. `App.tsx` computes that boolean with `isMarkdownDocumentPath(document.path)`, so Markdown behavior stays centralized and non-Markdown files keep plain source editing plus thread selection. Thread decorations should continue to be driven by source offsets. If needed, expose thread highlights via `EditorView.outerDecorations` in a later iteration, but the first implementation keeps the existing state field because it renders correctly with preview decorations.

Add tests in `frontend/src/markdownLivePreview.test.ts` for scanner and command behavior. Existing `DocumentSurface` tests should continue to pass. Add one test that renders a Markdown document with a CRDT-relative thread anchor and confirms the thread marker still works when live preview is enabled.

## Concrete Steps

From repository root, run:

    cd frontend
    npm run test
    npm run build

For browser verification, start the stack or frontend as appropriate, open the local app, log in if necessary, create or open a Markdown file, type Markdown examples, select text, and open a thread. The selected quote and resulting highlight must match the selected raw source range.

## Validation and Acceptance

Acceptance is behavior-based:

The editor still syncs raw Markdown through Yjs. Typing `# Heading`, `**bold**`, `[link](https://example.com)`, `> quote`, `- item`, and a fenced code block keeps those exact source characters in the document while styling them visually. Selecting visible text and opening a thread creates a thread over exactly the selected Markdown source range. Thread highlights move correctly after edits before the anchor. Large documents remain virtualized and do not render one DOM node per source line.

Run `npm run test` in `frontend` and expect all tests to pass. Run `npm run build` in `frontend` and expect a production build to complete. Use the browser to verify at least one Markdown document interactively.

## Idempotence and Recovery

The implementation is isolated to the frontend editor and does not change stored document data. If a preview decoration causes cursor or selection issues, remove that construct's hiding behavior first and leave only styling. Because the source document is untouched by the preview layer, disabling `enableMarkdownLivePreview` should never corrupt document data.

## Artifacts and Notes

Current known unrelated working-tree change:

    M homepage/index.html

This file is unrelated to the Markdown editor work and must not be reverted or claimed as part of this task. The Markdown editor work itself touches `frontend/src/App.tsx`, `frontend/src/DocumentSurface.tsx`, `frontend/src/DocumentSurface.test.tsx`, `frontend/src/logic.ts`, `frontend/src/styles.css`, `frontend/src/markdownLivePreview.ts`, and `frontend/src/markdownLivePreview.test.ts`.

## Interfaces and Dependencies

The new module should export:

    export function nottyMarkdownLivePreview(): Extension

    export function markdownPreviewCommand(name: MarkdownPreviewCommandName): Command

`frontend/src/logic.ts` should export:

    export function isMarkdownDocumentPath(path: string): boolean

The implementation should use only existing dependencies: `@codemirror/state`, `@codemirror/view`, `@codemirror/language`, `@codemirror/lang-markdown`, React, and Yjs. Do not add a rich editor dependency.
