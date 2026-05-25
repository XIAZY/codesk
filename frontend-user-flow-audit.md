# Frontend User Flow Code Audit

Date: 2026-05-11  
Scope: analysis only. No product code changes were made for this audit.

## Validation Evidence

Commands run from `/Users/zhongyangxia/Downloads/notty`:

- `cd frontend && npm test` passed: 2 test files, 9 tests.
- `cd frontend && npm run build` passed: Vite production build completed.
- `go test ./backend/internal/notty` passed.
- `go test ./daemon/internal/syncer` passed.
- `go test -tags=regression ./test/regression` passed in 122.043s.

Important limitation: `test/regression/README.md` says the backend-restart append test is opt-in because it currently reproduces a lost-write/reconnect problem. The default regression run does not prove restart safety for that case.

## Review Standard

I used the following first-principle criteria:

- One clear source of truth per flow.
- No frontend loading of full CRDT history except the active document.
- Backend should mostly pass through metadata and CRDT updates, not materialize documents unless intentionally doing bounded diff/checkpoint work.
- Daemon filesystem reconciliation should preserve correctness under local edits, remote edits, reconnects, high-churn writes, and multiple agent workspaces.
- Tests should focus on fragile correctness boundaries, not trivial getters/setters.
- Compatibility paths should be removed when they create duplicate API surfaces or hidden behavior.

## Highest-Risk Findings

### 1. Postgres CRDT update apply path still treats every inbound update as applied

Evidence:

- `backend/internal/notty/store.go` has `ApplyCRDTUpdateWithResult`.
- In the Postgres path, the code appends the update, clears document in-memory state fields, persists, and returns `Applied: true`.
- In the non-Postgres path, the code captures before/after CRDT snapshots and returns `Applied: false` when the update is a duplicate/no-op.
- `backend/internal/notty/server_test.go` tests duplicate suppression through the in-memory/server handler path.
- I did not find an equivalent Postgres duplicate/no-op test in `backend/internal/notty/store_postgres_test.go`.

Why this matters:

The product relies on CRDT idempotency. A repeated binary Yjs update should not advance document head metadata, produce duplicate workspace events, or inflate checkpoint/diff work. This is directly related to earlier failures around no-op update amplification and backend memory pressure.

Recommendation:

Unify the in-memory and Postgres apply semantics. The Postgres path should have an explicit idempotency check or a persisted update identity/dedup mechanism, and that behavior should be tested against real Postgres.

### 2. Workspace snapshot reduction is shallow and can retain stale cross-workspace state

Evidence:

- `frontend/src/useWorkspace.ts` keeps one reducer instance across `workspaceId` changes.
- `frontend/src/logic.ts` handles `workspace.snapshot` with `{ ...state, ...(event.data as Partial<WorkspaceState>) }`.
- `backend/internal/notty/server_workspace.go` HTTP `handleWorkspace` includes `workspaceId`, `currentUserId`, `currentDaemonId`, `name`, and `updatedAt`.
- `backend/internal/notty/server_workspace.go` websocket `workspace.snapshot` omits those identity fields and only sends arrays/maps.

Why this matters:

Shallow merging is fragile for tenant switching and reconnect races. If a websocket snapshot arrives after switching workspaces, or if a future snapshot omits a field, old state can remain in memory. A multi-tenant product should make workspace identity replacement explicit.

Recommendation:

Make snapshots replace the workspace state instead of shallow-merging, or include a mandatory `workspaceId` on every snapshot and discard mismatched snapshots. Add reducer tests for workspace switching and empty snapshots.

### 3. Browser document sync is critical but lightly tested at the frontend level

Evidence:

- `frontend/src/useDocument.ts` owns the active `Y.Doc`, websocket, local text state, local update sender, reconnect loop, and textarea replacement logic.
- `frontend/src/yProtocol.test.ts` has one test: it suppresses canonical empty sync-step-2 replies.
- `frontend/src/logic.test.ts` tests small Y.Text insert/backspace helpers, but not `useDocumentSync` reconnect behavior, delayed websocket opens, simultaneous remote/local updates, or document switching while a socket is closing.
- Backend and daemon websocket paths are well covered by Go tests and regression tests, but those do not exercise the React hook lifecycle.

Why this matters:

Earlier user-visible failures were disappearing/duplicated typed letters and slow live sync. The browser hook is the boundary that turns DOM textarea changes into Yjs updates, so it deserves targeted tests even if backend/daemon tests pass.

Recommendation:

Add a small frontend integration test with a fake websocket/y-protocol peer for these cases: local edits before open, local edit during reconnect, remote update during local typing, document ID switch closes stale socket, and delete/backspace propagation.

### 4. Backend restart correctness is not proven by the default regression suite

Evidence:

- `test/regression/README.md` documents that backend-restart append coverage is opt-in and currently reproduces a lost-write/reconnect problem.
- `test/regression/sync_regression_test.go` skips `TestAppendOnlyFileSyncSurvivesBackendRestart` unless `NOTTY_REGRESSION_BACKEND_RESTART=1`.

Why this matters:

The product has a daemon outbox/pending-update model. Correctness across backend restart is a first-principle requirement because websocket write success is not the same thing as durable persistence.

Recommendation:

Before treating sync as complete, make the restart test pass and move it into the default regression tier, or clearly label backend restart as unsupported.

## Flow-by-Flow Audit

### 1. Register and login

Implementation:

- Frontend: `AuthScreen` in `frontend/src/App.tsx`.
- API: `ApiClient.register`, `ApiClient.login`, `ApiClient.me` in `frontend/src/api.ts`.
- Backend: `handleRegister`, `handleLogin`, `handleMe` in `backend/internal/notty/server_auth.go`.
- Tests: `backend/internal/notty/server_auth_test.go` covers tenant isolation, slug allocation, and daemon token scope; frontend only has type/build coverage.

Assessment:

The flow is minimal and aligned with the current JWT design. The frontend stores the token in `localStorage`, restores with `/api/auth/me`, and clears auth on restore failure.

Edge cases:

- `handleRegister`, `handleLogin`, and `handleMe` ignore errors from `listWorkspacesForAccount` and can return auth success with missing workspace data.
- Modal-level frontend error handling exists for auth, but not for most authenticated modals.

Dead-code/minimality notes:

- `ApiClient.listWorkspaces` exists but is not used; session restore uses `/api/auth/me`.

### 2. Create/select workspace

Implementation:

- Frontend: `WorkspacePicker` in `frontend/src/App.tsx`.
- Backend: `POST /api/workspaces`, `GET /api/auth/me`, `GET /api/workspaces`.
- Tests: backend slug uniqueness and workspace isolation are covered.

Assessment:

The flow is minimal for first workspace creation and selection. Workspace ownership is created through the normal workspace creation flow, which matches the multi-tenant design.

Edge cases:

- There is no frontend test for token restore plus workspace selection.
- Workspace reducer state is not reset on workspace changes; see high-risk finding 2.

Dead-code/minimality notes:

- `GET /api/workspaces` is implemented and documented, but the current frontend does not call it after initial login/restore.

### 3. Member management

Implementation:

- Backend routes exist: `GET /api/workspaces/{workspaceID}/members` and `POST /api/workspaces/{workspaceID}/members`.
- README documents the flow.
- Frontend has no member management UI and no `ApiClient` methods for members.

Assessment:

This is backend-only in the current frontend. If member management is in scope, the frontend flow is missing. If it is not in scope for the MVP, the README/API surface is ahead of the UI.

Tests:

- I did not find frontend tests for this flow.
- Backend auth tests cover workspace isolation generally, but not a full frontend-visible member add/list flow.

### 4. Workspace shell metadata and live events

Implementation:

- Frontend: `WorkspaceApp`, `useWorkspace`, `reduceWorkspaceEvent`.
- Backend: `handleWorkspace` and workspace websocket `handleWebsocket`.
- Tests: backend tests assert workspace endpoints omit document content/CRDT payloads; frontend tests cover only a few reducer events.

Assessment:

The current shape follows the earlier performance principle: workspace metadata is lightweight, and active document CRDT state is not loaded through the workspace endpoint.

Edge cases:

- `reduceWorkspaceEvent` ignores `user.created`, `user.updated`, and `user.deleted`, even though the backend publishes those events.
- `workspace.activities` is included in backend snapshots and types, but the frontend Activity panel renders `agentEvents`, not `activities`.
- Workspace websocket snapshot shape differs from HTTP workspace shape.

Dead-code/minimality notes:

- `activities` appears partially integrated: stored and returned by backend, typed in frontend, but not rendered by the current Activity panel.

### 5. Document create, rename, delete

Implementation:

- Frontend create: `CreateDocumentModal`.
- Frontend rename: `RenameDocumentModal`.
- API wrapper includes `createDocument`, `renameDocument`, and `deleteDocument`.
- Backend document CRUD routes exist.
- Tests: backend document lifecycle tests exist; frontend has no component tests for modals.

Assessment:

Create and rename are implemented. Delete is not exposed in the current frontend even though the API wrapper exists and README documents delete.

Edge cases:

- Modal submissions do not catch API errors, so validation failures can become uncaught async errors rather than user-visible form errors.
- Document deletion updates reducer metadata, but active document selection behavior after deletion is not tested.

Dead-code/minimality notes:

- `ApiClient.deleteDocument` is currently unused by frontend code.

### 6. Active document editing and CRDT sync

Implementation:

- Frontend: `DocumentEditor`, `useDocumentSync`, `yProtocol.ts`, `computeReplace`, `applyReplaceToYText`.
- Backend: `handleDocumentWebsocket`, `handleDocumentProtocolMessageWithStore`.
- Daemon: per-document websocket plus document cache/reconcile path in `daemon/internal/syncer/service.go`.
- Tests: frontend helper tests, backend y-protocol tests, daemon reconciliation tests, and regression websocket tests all exist.

Assessment:

The architecture is aligned with current product principles: only the active frontend document holds a `Y.Doc`, workspace metadata avoids full content, and the backend persists CRDT updates/checkpoints.

Fragile areas:

- Frontend hook reconnect/local-edit behavior is not covered.
- The Postgres apply path still has the no-op/idempotency issue described in high-risk finding 1.
- Backend document websocket logs every inbound update and apply result. That is useful while debugging, but it is a hot path under keystroke load.

Tests:

- Good: regression tests cover multiple websocket recipients, concurrent websocket writers, insert/delete websocket broadcast, local/remote append merge, and overlap divergence behavior.
- Gap: no browser-hook lifecycle test for reconnect and stale socket races.

### 7. Thread creation, anchored ranges, and line thread viewing

Implementation:

- Frontend range thread creation is in `DocumentEditor.createThread`.
- Frontend line badges show a floating card with all threads for a selected line.
- Backend thread creation/reply routes are in `server_threads.go`.
- Store requires relative anchor pairs for text-range threads.
- Daemon tool gateway resolves simple agent-facing thread anchors.

Assessment:

The anchored range model is first-principle compliant: the frontend computes Yjs relative anchors and the backend stores caller-supplied anchors without document-wide materialization.

Edge cases:

- Backend supports document-level threads, but the frontend does not expose a working document-level thread composer. The plus buttons in `ThreadsPanel` have no behavior.
- The thread composer appears only after a range selection or existing `threadBody`; there is no obvious way to create a cursor/document-level thread from the UI.
- UI tests cover grouping helper behavior, not the click-through thread panel.

Tests:

- Good: backend and regression tests cover relative anchors, rejection of raw offsets, and thread mention notification enqueueing.
- Gap: frontend component flow for “select text, create thread, click line badge, open all threads” is not tested.

### 8. Thread replies and mentions

Implementation:

- Frontend: `ThreadsPanel` reply form.
- Backend: `ReplyThread`, mention extraction, `enqueueThreadMentionEventsLocked`, `enqueueThreadReplyEventsLocked`.
- Tests: backend store tests cover direct mention events, reply events, self-reply suppression, and document-text mentions being ignored.

Assessment:

The backend logic matches the current product decision: only thread mentions notify agents; plain `@handle` text in Markdown does not. This is well tested on the backend.

Edge cases:

- Frontend has no test for reply form behavior or failure states.
- Mention autocomplete does not exist; users must type handles exactly.

### 9. Daemon creation, token display, liveness, deletion

Implementation:

- Frontend: `DaemonsManagement`, `CreateDaemonModal`, `DaemonDetailModal`.
- Backend: daemon create/list/delete and token authentication in `server_auth.go`/`auth.go`.
- Daemon: token is sent as bearer token; `X-Notty-Acting-Agent-ID` is used only for agent actions.
- Tests: backend daemon token scoping is covered; frontend daemon status helper has basic tests.

Assessment:

The one-time daemon token flow is correctly shaped: backend returns plaintext token once and stores only a hash. The UI shows a deploy command.

Edge cases:

- `DaemonDetailModal` delete action does not catch errors.
- Liveness display has basic helper coverage, but the user-visible `visibleAgentStatus` function lives in `App.tsx` and is not directly tested.

Dead-code/minimality notes:

- Non-auth legacy daemon-unaware routes still exist when auth is disabled. If this product is now strictly multi-tenant, those routes increase the API surface.

### 10. Agent creation, status, detail, run start, deletion

Implementation:

- Frontend: `CreateAgentModal`, `AgentsManagement`, `AgentDetailModal`.
- Backend: daemon-scoped agent creation, generic agent creation, update/delete/start-run routes.
- Daemon: app-server supervisor and notification-driven turns in `agent_sessions.go`.
- Tests: backend agent lifecycle tests and daemon session tests cover many fragile paths.

Assessment:

The current frontend matches the desired UX of one `New agent` flow where the user selects a daemon. The UI does not expose system prompt editing.

Edge cases:

- `ApiClient.updateAgent` exists but no frontend edit-agent UI uses it.
- `AgentDetailModal` can start a run even when the owning daemon is disconnected. Backend creates a run request; the UI label says “Start run” rather than “Queue run”.
- Agent status display depends on both daemon liveness and run state, but the combined helper is not exported or unit-tested.

Dead-code/minimality notes:

- Generic `POST /api/workspaces/{workspaceID}/agents` remains alongside daemon-scoped `POST /api/workspaces/{workspaceID}/daemons/{daemonID}/agents`.
- Request structs still include `systemPrompt`, while backend tests assert custom prompts are ignored.

### 11. Activity, people, and presence panels

Implementation:

- Frontend `ActivityPanel` renders `workspace.agentEvents`.
- Frontend `PeoplePanel` renders daemons and agents.
- Backend presence route exists and workspace websocket broadcasts `presence.updated`.
- Document websocket awareness is sent by `useDocumentSync`.

Assessment:

This flow is partial. It is visually useful for agents/daemons, but it does not render workspace users or REST presence in a complete way.

Edge cases:

- `workspace.users` is loaded but not displayed in `PeoplePanel`.
- `workspace.presences` is reduced but not used to show active human collaborators.
- `workspace.activities` is returned but not rendered.
- The README documents a REST presence publish flow, but the current frontend does not call `POST /presence`.

Dead-code/minimality notes:

- Either wire `activities`, `users`, and `presences` into the UI, or remove them from frontend state until a concrete design uses them.

### 12. Agent notification center and diff tools

Implementation:

- Backend routes exist for inbox, inbox item update, diff, and mark viewed.
- Daemon `notty-agent-tool` has list/get/complete/dismiss/diff commands.
- Frontend does not expose agent inbox tooling directly, but shows aggregate `agentEvents`.
- Tests: backend and daemon tests cover inbox routing, thread mentions, document updates, dedupe, diff limits, and tool gateway behavior.

Assessment:

This is mostly daemon/agent-facing and is well covered compared with the frontend flows. The frontend aggregate display is intentionally shallow.

Edge cases:

- Backend diff is intentionally a materializing path with limits. Tests cover large diff rejection and identical large-content short-circuit.
- The Postgres no-op update issue can still indirectly make diff/checkpoint work heavier by inflating update history.

### 13. Daemon filesystem projection and multi-agent workspaces

Implementation:

- Main daemon workspace and agent workspace replicas each track projected files.
- Per-document state, pending/outbox data, and projected bases live in each workspace root’s `.notty/sync.db`.
- Dot paths are ignored by daemon workspace scanning.
- Agent logs remain regular synced workspace files.

Assessment:

This subsystem is complex, but most complexity maps to concrete bugs previously encountered: projection/local append clashes, remote pending updates, divergent agent workspaces, file locks, cache corruption, and outgoing update retry.

Tests:

- Strong: daemon unit tests cover file locks, atomic projected writes, corrupt cache, pending update dedupe, local+remote merge, high-pressure single-writer append, outbox retry, shared remote updates across agent workspaces, projected write false positives, missing tracked files during projection, and document update wake behavior.
- Strong: regression tests cover append pressure, dot path ignoring, websocket recipients, concurrent writers, delete/backspace broadcast, local/remote append merge, and thread anchors.
- Gap: backend restart append correctness remains opt-in/known problematic.

## Dead Code and Cleanup Candidates

These are candidates, not implementation instructions:

- Frontend `ApiClient.listWorkspaces`, `ApiClient.deleteDocument`, and `ApiClient.updateAgent` are unused by the current UI.
- `ThreadsPanel` plus buttons are inert.
- Frontend `WorkspaceState.activities`, `users`, and `presences` are loaded/reduced but not meaningfully rendered.
- Backend non-auth legacy routes under `/api/workspace`, `/api/documents`, `/api/agents`, `/ws`, and `/ws/documents/{id}` remain active when auth is disabled.
- Backend legacy notification aliases remain alongside inbox endpoints.
- Backend generic agent creation/run endpoints remain alongside daemon-scoped or agent-specific preferred endpoints.
- `systemPrompt` remains in frontend types and request models even though the product says prompts are backend-derived and not end-user customizable.

## Test Coverage Assessment

Well covered:

- Backend auth isolation and daemon token scope.
- Workspace metadata omitting document content/CRDT payloads.
- Backend/daemon CRDT insert/delete/concurrent websocket paths.
- Thread relative anchors and thread mention enqueueing.
- Agent notification inbox routing/deduping/diffing.
- Daemon file locks, projection races, cache corruption, outbox retry, and high-pressure append.

Under-tested:

- React auth/workspace/document/thread/daemon/agent component flows.
- `useDocumentSync` websocket lifecycle and browser-side Yjs race conditions.
- `useWorkspace` snapshot replacement and workspace switching.
- Frontend-visible agent status composition.
- Postgres duplicate/no-op CRDT update idempotency.
- Backend restart/reconnect durability in the default regression suite.

## Recommended Next Steps

1. Fix and test Postgres CRDT update idempotency before optimizing more performance paths.
2. Make workspace snapshots replace state safely across tenant switches.
3. Add targeted browser-side sync lifecycle tests rather than broad snapshot UI tests.
4. Decide whether member management, document deletion, document-level thread creation, activity, and presence are MVP flows; either implement them or remove them from the frontend/API documentation surface.
5. Promote backend restart append safety into the default regression suite once it passes.
