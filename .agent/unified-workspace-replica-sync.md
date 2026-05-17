# Unify Daemon Filesystem Replicas

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. It is self-contained and describes the current daemon sync refactor from the perspective of a reader who only has this working tree.

## Purpose / Big Picture

Notty has a daemon that mirrors backend CRDT documents into local files. Today the primary human workspace and agent workspaces use two different wrapper implementations even though they represent the same concept: a local filesystem replica of backend documents. This split makes correctness fixes fragile because watcher, dirty-file, projection, delete, move, websocket, and reconciliation behavior can differ between humans and agents.

After this change, every local filesystem root is represented by the same `workspaceReplica` type. The daemon `Service` coordinates backend metadata, agent sessions, a shared document cache, and one reconciliation scheduler. The user-visible behavior should stay the same: humans edit files under `~/Notty/workspaces/<workspace_id>`, agents edit files under `~/Notty/agents/<workspace_id>/<agent_id>`, and both sync through the backend. The observable improvement is that the daemon reconciles only dirty document IDs on the 2-second cadence, while preserving a slower full sweep for safety.

## Progress

- [x] (2026-05-17T22:32:53Z) Read `.agent/PLANS.md`, `daemon/internal/syncer/service.go`, `daemon/internal/syncer/replica.go`, installer path layout, and related tests to identify duplicated primary-vs-agent sync paths.
- [x] (2026-05-17T22:32:53Z) Chose a minimal refactor: reuse `workspaceReplica` for primary sync, keep daemon-global `documentCache`, and add a tiny document-ID dirty queue drained by the existing 2-second tick.
- [x] (2026-05-17T22:43:41Z) Implemented `reconcileQueue`, primary replica ownership, replica dirty marking, targeted reconciliation, and removal of primary-specific `Service` tracking logic.
- [x] (2026-05-17T22:43:41Z) Updated focused daemon tests so they exercise the unified replica model and high-churn dirty coalescing.
- [x] (2026-05-17T22:43:41Z) Ran focused daemon tests and the broader Go test suite.
- [x] (2026-05-17T22:43:41Z) Reviewed the resulting code for unnecessary special cases and removed the replica startup self-refresh fallback so metadata ownership stays in `Service`.

## Surprises & Discoveries

- Observation: The existing `workspaceReplica` already implements almost all primary workspace behavior: tracking, watching, document websocket connection, local create/move/delete, and presence. The primary `Service` path duplicates this logic.
  Evidence: `daemon/internal/syncer/replica.go` has `ensureTracked`, `connectDocument`, `handleLocalChange`, `reconcileLocalWorkspace`, and HTTP document lifecycle calls that mirror methods in `daemon/internal/syncer/service.go`.
- Observation: Backend auth already distinguishes daemon and agent actors. A daemon token without `X-Notty-Acting-Agent-ID` is attributed as `ActorType: "daemon"`; with the header it becomes an agent actor.
  Evidence: `backend/internal/notty/auth.go` sets daemon actor metadata from `auth.DaemonID`, and agent metadata from `auth.ActingAgentID`.
- Observation: Tests sometimes construct `workspaceReplica` directly without a watcher.
  Evidence: Converting primary tests to the unified replica path hit a nil watcher panic in `workspaceReplica.ensureTracked`. Production replicas still use `newWorkspaceReplica`, but the method now tolerates a nil watcher so focused tests can exercise the state machine without fsnotify.
- Observation: A naive dirty-queue drain can lose later IDs if reconciliation returns on the first document error.
  Evidence: Code review of `reconcileDocumentIDsWithTracked` after the first pass showed it drained the whole queue and returned immediately on one document error. The implementation now keeps processing independent IDs, requeues failed IDs, and has a regression test for that case.
- Observation: `applyWorkspace` can be reached from more than one service-owned path for the same replica.
  Evidence: Newly created replicas apply `initialWorkspace` in `Run`, and existing replicas receive metadata through `Service.refresh`. A per-replica `applyMu` now serializes metadata application so duplicate refresh/startup events cannot race tracking setup.

## Decision Log

- Decision: Do not change the current one-websocket-per-document-per-replica topology in this refactor.
  Rationale: Websocket topology is a separate performance/architecture change. Combining it with filesystem-replica unification would make correctness regressions harder to isolate.
  Date/Author: 2026-05-17 / Codex
- Decision: Keep the 2-second reconciliation tick, but drain a document-ID dirty set rather than scanning every document on every tick.
  Rationale: The 2-second tick has proven useful as a simple durability cadence. A dirty set keeps that cadence while making work proportional to changed documents and coalescing high-churn files.
  Date/Author: 2026-05-17 / Codex
- Decision: Keep the daemon-global `documentCache` outside replicas, and keep `.notty/projections` inside each replica root.
  Rationale: The shared cache represents backend document state, pending remote updates, and outgoing outbox. Projection bases represent what one local filesystem copy last saw, and must remain per replica.
  Date/Author: 2026-05-17 / Codex
- Decision: Make `workspaceReplica` actor-aware with `actorType`, and use acting-agent headers only for agent replicas.
  Rationale: Primary workspace edits should be authored by the daemon principal under token auth. Agent workspace edits should be authored by the specific agent ID.
  Date/Author: 2026-05-17 / Codex
- Decision: Keep workspace metadata refresh in `Service`, not in each replica's periodic loop.
  Rationale: A replica should synchronize a filesystem root and mark dirty documents. Periodic workspace metadata polling belongs to the daemon coordinator so primary and agent roots do not drift through independent metadata fetches.
  Date/Author: 2026-05-17 / Codex

## Outcomes & Retrospective

Implemented. The primary human workspace and agent workspaces now both use `workspaceReplica`; `Service` no longer has primary-only watcher or projected-file maps. Local fsnotify writes and incoming document websocket updates mark a document ID in a small coalescing `reconcileQueue`. The existing 2-second tick drains that queue, while the 60-second tick still performs a full tracked-document safety sweep and workspace metadata refresh.

Validation:

    go test ./daemon/internal/syncer -count=1
    go test ./...
    go test -race ./daemon/internal/syncer -count=1

All passed. The Go linker still prints the known y-crdt static library macOS-version warnings during tests; the tests completed successfully.

## Context and Orientation

The daemon code lives under `daemon/internal/syncer`. A daemon is a long-running local process that connects a user’s machine to the Notty backend. A CRDT document is a collaborative document represented by binary Yjs-compatible updates. A filesystem replica is a local directory containing plaintext files projected from backend CRDT documents.

The primary human workspace is configured by `NOTTY_WORKSPACE_DIR`, installed as `~/Notty/workspaces/<workspace_id>`. Agent workspaces are configured by `NOTTY_AGENT_WORKSPACE_ROOT`, installed as `~/Notty/agents/<workspace_id>`, with one subdirectory per agent. The daemon-global CRDT cache is configured by `NOTTY_CACHE_DIR`; if unset it is `NOTTY_RUNTIME_DIR/.notty/documents`, installed under `~/.notty/runtime/<workspace_id>/.notty/documents`.

`daemon/internal/syncer/service.go` currently owns the primary workspace directly through `Service.projectedByID`, `Service.projectedByPath`, a primary `fsnotify.Watcher`, and methods such as `Service.ensureTracked`, `Service.handleLocalChange`, `Service.reconcileLocalWorkspace`, `Service.connectDocument`, and `Service.readLoop`.

`daemon/internal/syncer/replica.go` owns agent workspaces through `workspaceReplica`, with equivalent tracking, watching, HTTP document lifecycle, and websocket behavior. This plan makes `workspaceReplica` the only filesystem-replica implementation and makes `Service` coordinate replicas.

`daemon/internal/syncer/document_cache.go` stores shared CRDT cache files under the daemon runtime directory. Each document ID has `state.bin`, `metadata.json`, `pending_remote.log`, and `outbox_update.json`. `pending_remote.log` holds backend updates received over websocket but not yet projected. `outbox_update.json` holds a local update until the server state vector proves it was accepted.

Projection baselines are stored under each replica root in `.notty/projections/<document_id>/base.txt` and `base.state.bin`. They tell the daemon what plaintext and CRDT state a specific local folder last received, so local edits can be turned into CRDT updates safely.

## Plan of Work

First, add a small `reconcileQueue` type in `daemon/internal/syncer` with `Mark`, `Drain`, and `Len` helpers. It stores unique dirty document IDs under a mutex. It is intentionally not a timer system. The existing 2-second tick remains the scheduler.

Second, extend `workspaceReplica` so it has `actorType` and a `markDirty` callback. When a local file change marks a tracked file dirty, the replica calls `markDirty(documentID)`. When a document websocket receives and appends a remote CRDT update to pending cache, it also calls `markDirty(documentID)`. The replica should use `actorType` in websocket query and presence payload. Only agent replicas should pass their actor ID as `X-Notty-Acting-Agent-ID`; primary daemon replica should not.

Third, modify `Service` to own `primaryReplica *workspaceReplica` and `reconcileQueue *reconcileQueue`. Remove the primary watcher and primary tracking maps from `Service`. `New` should create the shared document cache, create the dirty queue, and create the primary replica for `cfg.WorkspaceDir`.

Fourth, change `Service.Run` so it starts the primary replica goroutine, keeps the workspace metadata event loop, drains dirty document IDs every 2 seconds, and runs a full tracked-document sweep plus `refresh` every 60 seconds. The primary replica handles primary workspace fsnotify and local structural reconciliation just like agent replicas.

Fifth, change `Service.refresh` and `reconcileAgentReplicas` so workspace metadata is applied to all replicas. Existing agent replicas should receive `applyWorkspace(ctx, workspace)` instead of only new replicas getting initial metadata. New replicas can still receive `initialWorkspace` before their goroutine starts.

Sixth, change `collectTrackedByDocument` to collect from the primary replica plus every agent replica. Add targeted reconciliation methods: one method drains dirty IDs and reconciles only those documents, while the existing full sweep remains for the 60-second safety path and tests. After each targeted document reconcile, if the document still has local dirty state, pending remote updates, or an outbox, mark it dirty again so the next 2-second tick retries.

Finally, update tests. Tests that reached into `Service.projectedByPath` should now build a primary `workspaceReplica` or call replica methods directly. Add focused tests for dirty queue coalescing and for service collection from primary plus agent replicas. Keep tests limited to fragile sync paths.

## Concrete Steps

From `/Users/zhongyangxia/Downloads/notty`, edit the daemon syncer files using `apply_patch`.

Run focused tests during implementation:

    go test ./daemon/internal/syncer -run 'TestReconcileQueue|TestServiceCollectTrackedByDocumentIncludesPrimaryReplica|TestWorkspaceReplicaLocalChangeMarksDirtyDocument|TestHandleRemoteSyncMessage' -count=1 -v

Run the broader daemon syncer test package after the refactor:

    go test ./daemon/internal/syncer -count=1

If daemon tests pass, run the backend websocket protocol tests only if changes cross backend behavior. This refactor should not require backend code changes.

## Validation and Acceptance

Acceptance is behavior-based:

The primary workspace and agent workspaces both use `workspaceReplica`. This can be verified by reading `Service` and observing that it no longer has `projectedByID`, `projectedByPath`, or direct primary watcher handling.

A local edit in either primary or agent replica marks exactly one document ID dirty. This is validated by unit tests against `workspaceReplica.handleLocalChange` and `reconcileQueue`.

Incoming remote updates append to pending cache and mark the document dirty. This is validated by a focused test that calls the websocket sync handler path and checks the queue.

The 2-second tick reconciles only dirty document IDs, while the 60-second path still runs a full safety sweep. This is validated by tests around queue draining and service collection.

Existing outbox retry behavior remains correct. If a document still has an outbox after a reconcile attempt, it is re-marked dirty for the next tick.

## Idempotence and Recovery

The refactor is code-only and does not migrate or delete user data. It is safe to rerun tests repeatedly. The daemon cache and workspace projection directories keep the same on-disk format. If a test fails after partial edits, inspect `git diff`, fix forward, and rerun the focused daemon tests. No production deployment is part of this plan unless separately requested.

## Artifacts and Notes

Important current duplicated paths before the refactor:

    Service primary path:
      daemon/internal/syncer/service.go: projectedByID/projectedByPath, ensureTracked, handleLocalChange, reconcileLocalWorkspace, connectDocument, readLoop

    Agent replica path:
      daemon/internal/syncer/replica.go: workspaceReplica with equivalent methods

The target is not to change visible files or cache layout. It is to make one implementation own filesystem-replica behavior.

## Interfaces and Dependencies

In `daemon/internal/syncer/reconcile_queue.go`, define:

    type reconcileQueue struct { ... }
    func newReconcileQueue() *reconcileQueue
    func (q *reconcileQueue) Mark(documentID string)
    func (q *reconcileQueue) Drain() []string
    func (q *reconcileQueue) Len() int

In `daemon/internal/syncer/replica.go`, `workspaceReplica` must include:

    actorType string
    markDirty func(documentID string)

The constructor should become:

    func newWorkspaceReplica(cfg Config, rootDir, actorID, actorType string, markDirty func(string)) (*workspaceReplica, error)

In `daemon/internal/syncer/service.go`, `Service` should include:

    primaryReplica *workspaceReplica
    reconcileQueue *reconcileQueue

and should no longer include a primary `fsnotify.Watcher` or primary projected maps.

Revision note 2026-05-17T22:32:53Z: Initial plan created after reading the current daemon sync implementation. The plan chooses targeted dirty-set reconciliation over debounce timers because the user explicitly wants to keep the proven 2-second tick.
