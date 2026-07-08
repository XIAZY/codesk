# PINS — the feature pin register

A pin is a test that goes red the day a business feature stops working. This file is the human-readable
index of what is pinned and, just as importantly, what is **not yet** pinned. It is reviewed like code:
any PR that adds, moves, or removes a pin updates its line here in the same PR. A pin that lives only in
a chat thread rots; one whose line fails review when stale does not.

Format: **Feature — breaks → the test that notices.** The gap list is simply the features whose line we
cannot honestly write yet; it is the program's backlog, ordered by business weight.

Seeded 2026-07-07 from the Tests-improvements scorecard (Tom/Bill/Deniz), alongside the contract tier
(PR #80) and its frontend consumption (PR2).

## Pinned

### Data integrity
- **Documents never lose data** — breaks → per-entity restart-survival tests + the compose sync-convergence E2Es (move / delete / concurrent-writer reconstruction) + checkpoint reconstruction (`test/regression`, `backend/internal/notty` store tests). _Documented exception: the WS-write ack gap is accepted-risk C3 — pinned as a decision in `test/regression`, not a test._

### API contracts
- **The workspace answer never changes shape by accident** — breaks → contract-tier golden rows `TestContractWorkspaceEmptyState` / `TestContractWorkspacePopulatedState` (`backend/internal/notty/contract_workspace_test.go`); a drift goes red naming the exact field.
- **The read surface never changes shape by accident (endpoint×state matrix)** — breaks → `TestContractEndpointsEmptyState` / `TestContractEndpointsPopulatedState` (`backend/internal/notty/contract_endpoints_test.go`): GET `/auth/me`, `/workspaces`, `/members`, `/daemons`, `/documents/{id}/threads`, `/threads/{id}` each pinned to a committed golden across empty + populated states, with the A1 never-null invariant on every collection row. A response-shape drift on any of them is a red, reviewable golden diff naming the exact field. _Corpus suite 2.1; grows one builder + one table row per endpoint added._
- **Every error is the same envelope `{"error": "<non-empty>"}`** — breaks → `TestContractErrorEnvelopePerClass` (`backend/internal/notty/contract_errors_test.go`): 400/401/403/404 each return exactly one `error` key with a non-null non-empty string at the right status, so the frontend parses any failure uniformly. A handler that invents a different error shape or a status/body mismatch goes red. _Corpus suite 2.3; 409/410/413 rows next._
- **Collections are never JSON null** (the workspace-switch white-screen) — breaks → `TestWorkspaceResponseNeverNulls…` (backend, in-package) + the contract rows' A1 assertion + the frontend consumption test (`frontend/src/contractFixtures.test.ts`).
- **Snapshot presences are keyed and visible to the online ring** — breaks → `frontend/src/contractFixtures.test.ts` (populated golden → presence retrievable by actorId). _Pinned by PR2; the bug it caught is fixed in the same PR._

### Browser flows (end-to-end, Playwright vs the compose prod bundle)
- **The core browser flow works: login → workspace switch → open/edit a document** — breaks → `e2e/tests/smoke.spec.ts` core-flow test (production `vite preview` bundle + real compose backend, retries=0). The A→B switch into a fully idle workspace pins the **white-screen incident**: any uncaught exception on the switch fails via the `page.on('pageerror')` net. _Scope: the crash needs both sides (a `null` collection from the backend AND a frontend deref), so this row reds only when both #75's reducer-seam coercion and #76's never-null backend regress — it is the last net over the layered unit pins (#75 reducer test, #76 in-package never-null, the contract goldens), not the only one; red-proof captured `pageerror: f.threads is not iterable` on the both-sides revert._
- **A healthy thread anchor survives a document reopen (no false "anchor lost")** — breaks → `e2e/tests/smoke.spec.ts` false-orphan pin: a thread on a real Yjs relative position, after the doc is reopened and re-synced over the WS, must show no orphan warning. _Scope: defended in depth by #100's `ready` guard AND #40's post-sync `ytext.observe` re-classification; reds only when both regress (reproducing the original stuck false-orphan, `.thread-orphan-warning` count 1) while the core flow stays green._

### Workspace & thread lifecycle
- **Workspace creation is atomic; deletion cascades and is honest** — breaks → creation tests + #83's deletion suite (cascade proof across 16 tables, exact-name confirm, broadcast-count honesty).
- **Threads: create is idempotent under contention; resolve is idempotent + reversible** — breaks → thread create tests (real-DB contention) + #84's resolve suite (broker-level).

### Auth & isolation
- **A workspace's data is scoped to its members; destructive ops are human-only** — breaks → the authZ matrix (owner/admin/member/non-member) + daemon-bypass traps in the #83/#84 suites.

### Daemon
- **Daemon lifecycle & install** — breaks → 212 daemon boundary tests + installer lifecycle (shell).

### Agent lifecycle (daemon-side, in-process)
- **Session state machine** — idle/working/disconnected transitions, resume-falls-back-to-fresh, steer-vs-queue on a busy session, stale-process events ignored, idle-notification-once — breaks → the fake-driver suite in `daemon/internal/syncer/agent_sessions_test.go` (`TestAgentSessionResumeFailureStartsNewSession`, the `BusyForMe`/`BusyGeneral` steer/queue tests, the missing/unregistered-runtime disconnect tests, `…IgnoresEventsFromStaleRuntimeProcess`, `…IdleNotificationStartsOncePerInboxSignature`).
- **A failed turn-start leaves no stale active turn** — breaks → `TestClaudeStartTurnWriteErrorLeavesNoStaleActiveTurn` (`daemon/internal/syncer/claude_driver_test.go`): a StartTurn whose write fails must clear the turn it optimistically armed, or a later steer targets a phantom turn.

## Gap — not yet pinned (business-weight order)
1. **The agent collaboration loop — wire half** (claim/reclaim contention under concurrent claimers, tool-gateway per-session token auth, one full notification-turn round-trip) — the session state machine is now pinned in-process (above); this remaining half needs the real daemon↔backend protocol → compose + fake-runtime, nightly lane.
2. **A dropped turn-end must not wedge a session** — the daemon's own event queue can drop a lifecycle event on overflow, stranding a session in `working`. → task #12 (guaranteed lifecycle-event delivery + a full-queue-can't-lose-a-turn-end regression test), in progress. _A wedge from the runtime process itself going silent (external code) has no detection today — revisit with a production incident as the evidence, per the ruling that dropped the observe-only watchdog._
3. **WS event envelopes** — 13 types; only the two workspace ones (`workspace.updated` / `workspace.deleted`) scheduled so far. _Suite 3 of the corpus generalizes this._

_User-facing browser flows (login / switch / edit) are now pinned above (Browser flows) — the corpus suite 1 smoke walks them; thread-reply via the UI (1.8) remains a planned extension._

_What a pin can never carry is the **why** — that lives in the regression catalog and the decision
records. The register says what breaks and what notices; the catalog says what we chose and why._
