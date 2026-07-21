# Make replies and creation locations visible

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. Maintain this document in accordance with `.agent/PLANS.md`.

## Purpose / Big Picture

A person currently has no workspace-level indication that another person or agent replied inside a thread, so a reply can remain invisible until the person happens to reopen the right document and thread. Creating documents also silently inherits the active document's folder, and the workspace switcher has no discoverable path to the already-built workspace creation page. After this work, the top-right toolbar shows truthful unread thread replies for the current workspace, each notification opens its exact document and thread, document creation has an explicit root or folder location, and a visible workspace-switcher action opens the existing new-workspace flow.

The result is observable in the real app: create or receive a reply from a different actor and see one unread notification; click it and land in that thread detail with only that thread cleared; use the Documents header or a folder-row action and see the new file appear at the stated location; use the workspace-switcher plus action and reach `/new` without losing the existing creation behavior.

## Progress

- [x] (2026-07-21 12:00Z) Claimed task #64 and audited the current task board, prior Inbox removal, current `main`, and repository instructions.
- [x] (2026-07-21 12:35Z) Traced real thread WebSocket events, document path-derived folders, document creation, move-folder behavior, workspace routing, and the existing creation form.
- [x] (2026-07-21 12:55Z) Published the proposed behavioral contract for product and design review without waiting on token-capped agents.
- [x] (2026-07-21 15:09Z) Added focused pure tests for unread derivation, first-load baseline, self-reply exclusion, exact-thread clearing, timestamp ties, persistence isolation, and corrupt/unavailable storage.
- [x] (2026-07-21 15:42Z) Implemented the constant notification bell, retained unread snapshots, exact document/thread navigation, visible-and-focused acknowledgement, and explicit unavailable-row acknowledgement.
- [x] (2026-07-21 15:20Z) Added red-first tests for explicit root/folder document creation and the visible new-workspace route action.
- [x] (2026-07-21 15:25Z) Implemented root and folder document affordances plus the workspace creation entry by reusing the existing document/root namespace and `/new` form.
- [x] (2026-07-21 16:02Z) Passed typecheck, 31 files / 379 frontend tests, production build, and `git diff --check`; the repository has no `make diff-check` target, and CI defines the gate as `git diff --check` directly.
- [x] (2026-07-21 15:58Z) Passed 11/11 production-build Playwright tests against compose/Postgres, including a real invited-member reply journey; inspected desktop and 390x844 screenshots and corrected the initial mobile stacking/height defect.
- [ ] Obtain an independent QA/design review, address findings on the exact head, and request explicit @AlphaToad approval.

## Surprises & Discoveries

- Observation: The former workspace Inbox was intentionally removed because it inferred a fictional `Needs review` state from agent inbox rows.
  Evidence: task #42 and commit `caeaa57` remove the Inbox button, badge, flyout, and `workspaceInboxSummary`. This feature must use actual thread messages only.

- Observation: The browser already receives `thread.message.created` in the workspace WebSocket snapshot/reducer, so reply attention does not require polling.
  Evidence: `frontend/src/useWorkspace.ts` dispatches workspace frames and `frontend/src/logic.ts` appends the message and updates the thread timestamp.

- Observation: Folders are not durable entities. They are derived from path prefixes and cannot exist empty.
  Evidence: `buildDocumentTree` and `documentFolders` in `frontend/src/App.tsx` derive folders from `DocumentItem.path`; `MoveDocumentModal` materializes a new prefix only when a document moves into it.

- Observation: `createDocument` currently chooses `parentPath(activeDocument?.path)` without telling the user.
  Evidence: `untitledDocumentPath(rootDocuments, activeDocument?.path)` runs behind the single Documents-header plus action.

- Observation: The complete workspace creation form and `/new` route already support accounts with existing workspaces; only the switcher entry is missing.
  Evidence: `WorkspaceOnboarding`, `CreateWorkspaceForm`, and `AppRoute.newWorkspace` are present on current `main`.

- Observation: Deriving notification rows only from live threads silently loses an already-observed unread reply if the document/thread later disappears.
  Evidence: the initial hook returned `unreadThreadReplies(workspace.threads, ...)`; the retained-state regression removes all live threads after observing a reply and now proves the row remains until explicit acknowledgement.

- Observation: A mobile fixed popover inside the sticky toolbar cannot out-rank the root onboarding checklist merely by increasing the child's z-index, because the toolbar is its stacking context.
  Evidence: the first 390x844 production screenshot showed the checklist painting over a full-height notification sheet. The sealed UI makes the sheet content-sized with a viewport cap and elevates the toolbar context only while notifications are open; the repeat screenshot has no overlap.

## Decision Log

- Decision: Count only messages after the thread's opening message, and exclude messages authored by `workspace.currentUserId`.
  Rationale: A new thread is not a reply, and a person's own message cannot be new information to that person. This keeps the count semantically truthful.
  Date/Author: 2026-07-21 / Deniz

- Decision: Baseline all existing replies as read when no stored watermark exists, then persist per-account, per-workspace, per-thread watermarks in versioned browser storage.
  Rationale: Shipping the feature must not flood users with old history. Browser storage survives reloads on the same browser while keeping this iteration frontend-only. The UI and release notes must not claim cross-device read synchronization; a future server read-receipt model would be separate work.
  Date/Author: 2026-07-21 / Deniz

- Decision: Mark one thread read only when its detail is actually visible. A visible detail consumes newly arriving replies automatically; opening the notification menu alone changes nothing.
  Rationale: Clearing on menu-open or document-open would lose attention without proving the person saw the reply. Product review tightened "visible": `document.visibilityState` must be `visible` and `document.hasFocus()` must be true. A mounted detail in a background tab is not viewed.
  Date/Author: 2026-07-21 / Deniz

- Decision: If a notification's document or thread cannot be opened, retain its unread state and show an unavailable row with an explicit `Mark read` escape hatch.
  Rationale: Deleted documents and permission changes must not silently consume attention. Only successful detail mounting or a deliberate user command may clear the row.
  Date/Author: 2026-07-21 / Deniz and Anton product review

- Decision: Reuse the existing current-workspace WebSocket and thread snapshot, not the deleted agent Inbox or agent notification APIs.
  Rationale: Those systems have different actors and semantics. Reusing them would reintroduce the false review model that was deliberately removed.
  Date/Author: 2026-07-21 / Deniz

- Decision: The Documents header plus creates at root; each folder row gets a separate, accessible plus action that creates inside that folder.
  Rationale: The destination becomes explicit without forcing a modal into the common fast path. It also preserves the true path-prefix model and does not promise empty folders.
  Date/Author: 2026-07-21 / Deniz

- Decision: Add a visible plus action beside the workspace switcher and navigate to the existing `/new` route.
  Rationale: This solves discoverability while keeping one creation form and one backend path. For an account with existing workspaces, the reused page must say `Create a workspace` and offer `Back to {current workspace}`; the first-workspace copy remains only for an empty account.
  Date/Author: 2026-07-21 / Deniz

## Outcomes & Retrospective

The three requested workflows are implemented without a database migration or new backend endpoint. Notifications use actual workspace thread events, persist browser-local retained snapshots per account/workspace, and do not revive the removed fictional agent Inbox. Document and workspace creation reuse existing path, root-namespace, routing, and form primitives. The exact local object is functionally sealed: 31/31 frontend files and 379/379 tests pass, production build passes, and the real compose/Postgres browser suite passes 11/11 with an invited second human. Final completion is intentionally held for independent exact-head review and explicit @AlphaToad approval.

## Context and Orientation

The frontend is a React and TypeScript application under `frontend/src`. `frontend/src/App.tsx` contains the workspace shell, document tree, toolbar popovers, thread panels, and the create/move forms. `frontend/src/useWorkspace.ts` loads the initial workspace snapshot and applies WebSocket events. `frontend/src/logic.ts` is the pure workspace reducer. `frontend/src/types.ts` defines `ThreadItem`, `ThreadMessage`, and `WorkspaceState`. `frontend/src/styles.css` owns the visual system. Tests live beside these files and use Vitest with Testing Library.

A thread contains an opening message at `thread.messages[0]`; later entries are replies. An unread watermark is the newest reply timestamp/id pair that this account has actually viewed for one thread. The notification surface covers only the workspace currently open because this client subscribes only to that workspace's snapshot and WebSocket. It is not a server mailbox and must not imply cross-workspace or cross-device coverage.

Document folders are path prefixes such as `Specs/Drafts`, not records. A document at `Specs/Drafts/Plan.md` makes both `Specs` and `Specs/Drafts` visible. Therefore a folder-row plus creates a real document under that prefix; it must never create a hollow row that disappears after reload.

The top-level router already recognizes `/new`. The new workspace action only invokes that route. `CreateWorkspaceForm` remains the single creation implementation.

## Plan of Work

First, add a small pure module in `frontend/src/threadUnread.ts` with versioned storage parsing and derivation functions. Keep storage access behind explicit load/save functions so malformed JSON, unavailable storage, account switches, and workspace switches are testable. Represent a watermark as the newest viewed reply's `createdAt` and `id`. Derive unread replies by a stable `(createdAt, id)` comparison, excluding the opening message and `authorId === currentUserId`. On a missing store, seed every current thread to its newest existing message and write once; do not treat the seed as user activity.

Then integrate one hook or state owner in `WorkspaceApp`. It loads watermarks for `account.id` and `workspaceId`, observes `workspace.threads`, and exposes unread rows, total count, and `markThreadRead(threadId)`. Add a top-right icon trigger that remains available even when no document is open, plus a single mutually exclusive popover in `frontend/src/App.tsx`. A row shows the document name/path, newest unread actor, concise body preview, time, and per-thread count. Selecting a resolvable row closes competing toolbar popovers, navigates to the row's document, opens `ThreadsPanel` on the exact thread after the document switch, and marks that thread read only after the detail is rendered in a visible, focused browser tab. Thread selections in both `ThreadsPanel` and `ThreadPopover` use the same visibility-aware transition. A missing document or thread stays unread, renders as unavailable, and offers a separate explicit `Mark read` control. The trigger has a stable size, an accessible count label, a capped numeric badge, keyboard focus, outside-click close, Escape close, and viewport-safe dimensions.

Next, change `createDocument` to accept an explicit folder path and calculate `uniqueDocumentPath(rootDocuments, joinDocumentPath(folderPath, "Untitled.md"))`. The Documents-header action passes the empty root path. Extend `DocumentTree` with `onCreateDocument(folderPath)`. Refactor each folder row into a non-overlapping row container with a folder-toggle button and sibling icon button so there are no nested interactive elements. The folder plus states its destination in `aria-label` and tooltip. Creating expands the relevant ancestors, navigates to the new file, and enters the existing rename flow.

Finally, add `onCreateWorkspace` to `WorkspaceApp` and pass it from the authenticated routing owner as a navigation to `{ kind: "newWorkspace" }`. Place a plus icon beside the current workspace switch control with an accessible label and tooltip. Preserve the native switch select and all existing workspace switch behavior. Make `WorkspaceOnboarding` distinguish an empty account from an account that already has workspaces: the latter uses `Create a workspace` and a `Back to {current workspace}` command, while successful creation still selects the new workspace.

## Concrete Steps

Work from the repository root `/home/ubuntu/.slock/agents/b9afbe61-602c-4f47-ad94-718e69765f3b/work/notty-task64-ux-audit` on branch `qa/task64-ux-audit`, initially based at `378b0f03d76093c600900e0d56bb99aea832098e`.

Create the pure unread tests first and run:

    cd frontend
    npm test -- --run src/threadUnread.test.ts

Add the workspace integration tests for notification navigation/read behavior and run only their files while iterating. Add document-tree and routing tests to `frontend/src/App.test.tsx` or a narrowly named new file. Run:

    npm test -- --run src/ThreadNotifications.test.tsx src/App.test.tsx

After focused tests pass, run the complete frontend gates:

    npm test
    npm run build

From the repository root, run the diff policy used by this project:

    git diff --check

Start the real development stack using the repository's documented development command, seed two actors and nested documents, then use Playwright to capture desktop and mobile states. Keep the server running until screenshots and browser console/network checks are complete, then stop it cleanly.

## Validation and Acceptance

With no stored watermark, loading a workspace containing old replies shows zero unread and writes a baseline. After a different user or agent sends one new reply over the existing WebSocket, the top-right indicator shows one without reload. A reply sent by the current user does not increase it. A new thread opening message does not increase it.

Opening the notification popover leaves counts unchanged. Clicking a row for a document other than the active document navigates there, opens the exact thread detail, moves focus into that detail, and clears only that thread. Other unread threads remain. A new reply arriving while the exact detail remains visible in a focused browser tab is consumed as read; a background tab retains it until visibility and focus return. A deleted/unavailable document cannot be navigated, remains unread, and clears only through its explicit `Mark read` command. Closing the popover without selecting changes nothing. Reloading the same browser retains read state. Switching account or workspace cannot leak watermarks. Corrupt or unavailable storage fails safely without crashing.

Clicking the Documents-header plus creates `Untitled.md` (or its unique numbered variant) at root regardless of the active document's folder. Clicking a folder-row plus creates the document directly below that folder and expands the path. Buttons have distinct accessible names, do not nest, and do not resize the row on hover. Existing rename, move, New Folder, collapse, active selection, and keyboard shortcut behavior remains green.

Clicking the visible workspace plus routes to `/new`. With existing workspaces the page says `Create a workspace` and offers `Back to {current workspace}`; with none it retains `Create your first workspace`. Successful creation enters the new workspace. Switching existing workspaces still works.

At 1280x800 and 390x844, the toolbar, notification badge, popover, workspace switcher, folder rows, and text remain contained with no overlaps or horizontal overflow. Keyboard Tab, Enter/Space, and Escape work; focus returns to triggers when popovers close. Browser console has no errors. The exact implementation head must receive an independent QA/design report and explicit @AlphaToad approval before task #64 is done.

## Idempotence and Recovery

All edits are frontend-only and additive until the existing implicit document destination is replaced. Tests may be rerun safely. Version the storage key so a future schema can baseline cleanly instead of misreading old data. If browser storage throws, keep an in-memory session state and never clear unread merely because persistence failed. If exact-thread navigation cannot resolve a deleted document or thread, keep the item unread and show a truthful unavailable state rather than silently clearing it.

The worktree is isolated from other agents. Do not reset or overwrite unrelated branches. Remove any temporary fixtures, screenshots not intended as review artifacts, and development-only data before sealing the branch.

## Artifacts and Notes

The pre-change identity is:

    branch: qa/task64-ux-audit
    base/head: 378b0f03d76093c600900e0d56bb99aea832098e
    status: pristine

The removed false-Inbox reference is commit `caeaa57`. The existing folder/New Folder implementation is in `buildDocumentTree`, `documentFolders`, and `MoveDocumentModal` in `frontend/src/App.tsx`; commits `950d19c` and `1b77e7f` document the path-prefix and honesty constraints.

## Interfaces and Dependencies

Do not add a package. Use React, the existing `ApiClient`, router helpers, `localStorage`, and the existing icon/CSS family.

In `frontend/src/threadUnread.ts`, expose pure, named functions and serializable types equivalent to:

    export type ThreadReadWatermark = { createdAt: string; messageId: string };
    export type ThreadReadState = Record<string, ThreadReadWatermark>;
    export type ThreadNotificationState = { reads: ThreadReadState; pending: Record<string, UnreadThreadReply> };
    export function unreadThreadReplies(threads, currentUserId, state): UnreadThreadReply[];
    export function reconcileThreadNotificationState(state, threads, currentUserId): ThreadNotificationState;
    export function markThreadNotificationRead(state, threadId, thread?): ThreadNotificationState;
    export function baselineThreadNotificationState(threads): ThreadNotificationState;
    export function loadThreadNotificationState(storage, accountId, workspaceId): ThreadNotificationState | null;
    export function saveThreadNotificationState(storage, accountId, workspaceId, state): void;

The exact `threads` parameter types should use `ThreadItem[]` from `frontend/src/types.ts`, and `UnreadThreadReply` should identify the thread, document, newest unread message, and count without copying mutable thread state. Storage keys must include a schema version plus encoded account and workspace identity.

At the end, revise every living section above with actual commands, counts, screenshots, review messages, and any changed decision. Add a final change note explaining the completed revision and why.

Change note (2026-07-21 15:10Z, Deniz): Folded Anton's product-green corrections into visibility-aware reads, unavailable-row retention, predictable shortcut behavior, and the non-first-workspace `/new` escape path; recorded the first passing pure-test milestone.

Change note (2026-07-21 16:03Z, Deniz): Recorded the completed implementation and exact gates, including retained deleted-thread notifications, the two-human production browser journey, and the screenshot-driven mobile stacking correction. The only remaining acceptance item is independent exact-head review plus @AlphaToad approval.
