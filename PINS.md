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
- **The workspace answer never changes shape by accident** — breaks → contract-tier golden rows `TestContractWorkspaceEmptyState` / `TestContractWorkspacePopulatedState` (`backend/internal/notty/contract_workspace_test.go`); a drift goes red naming the exact field. Today 2 of ~40 endpoints; the endpoint×state matrix generalizes it.
- **Collections are never JSON null** (the workspace-switch white-screen) — breaks → `TestWorkspaceResponseNeverNulls…` (backend, in-package) + the contract rows' A1 assertion + the frontend consumption test (`frontend/src/contractFixtures.test.ts`).
- **Snapshot presences are keyed and visible to the online ring** — breaks → `frontend/src/contractFixtures.test.ts` (populated golden → presence retrievable by actorId). _Pinned by PR2; the bug it caught is fixed in the same PR._

### Workspace & thread lifecycle
- **Workspace creation is atomic; deletion cascades and is honest** — breaks → creation tests + #83's deletion suite (cascade proof across 16 tables, exact-name confirm, broadcast-count honesty).
- **Threads: create is idempotent under contention; resolve is idempotent + reversible** — breaks → thread create tests (real-DB contention) + #84's resolve suite (broker-level).

### Auth & isolation
- **A workspace's data is scoped to its members; destructive ops are human-only** — breaks → the authZ matrix (owner/admin/member/non-member) + daemon-bypass traps in the #83/#84 suites.

### Daemon
- **Daemon lifecycle & install** — breaks → 212 daemon boundary tests + installer lifecycle (shell).

### Agent lifecycle (tier E, in-process half)
- **Session state machine** — idle/working/disconnected transitions, resume-falls-back-to-fresh, steer-vs-queue on a busy session, stale-process events ignored, idle-notification-once — breaks → the fake-driver suite in `daemon/internal/syncer/agent_sessions_test.go` (`TestAgentSessionResumeFailureStartsNewSession`, the `BusyForMe`/`BusyGeneral` steer/queue tests, the missing/unregistered-runtime disconnect tests, `…IgnoresEventsFromStaleRuntimeProcess`, `…IdleNotificationStartsOncePerInboxSignature`).
- **A wedged turn is detected** (the P0 dropped-turn-end class behind hotfix #70: a turn whose runtime stream goes silent while the process stays alive) — breaks → `daemon/internal/syncer/runtime_wedge_watchdog_test.go` (observe logs the trip; enforce `Stop()`s into the existing recovery) + the codex/claude driver-wiring pins. _Observe mode DETECTS but does not yet recover — a wedged session still never self-heals until the enforce default is armed (phase 2); detection is pinned now, auto-recovery lands with enforce._

## Gap — not yet pinned (business-weight order)
1. **The agent collaboration loop — wire half** (claim/reclaim contention under concurrent claimers, tool-gateway per-session token auth, one full notification-turn round-trip) — the session state machine is now pinned in-process (above); this remaining half needs the real daemon↔backend protocol → compose + fake-runtime, nightly lane.
2. **Wedge auto-recovery in production** — the watchdog ships observe-only; arming enforce (so a real wedge recovers, not just logs) is phase 2, gated on the observe-log evidence justifying the window.
3. **User-facing browser flows** (login / switch / edit / thread-reply) — task #5, spec final, unbuilt. Yesterday's white-screen shipped because nothing walks these.
4. **WS event envelopes** — 13 types; only the two workspace ones (`workspace.updated` / `workspace.deleted`) scheduled so far.

_What a pin can never carry is the **why** — that lives in the regression catalog and the decision
records. The register says what breaks and what notices; the catalog says what we chose and why._
