# Add daemon filesystem lifecycle sync tests

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document follows `.agent/PLANS.md`.

## Purpose / Big Picture

The daemon watches a local workspace directory and turns filesystem operations into CRDT updates that are sent to the backend. This work adds tests proving the full local lifecycle: a local file create allocates a backend document and root entry, local edits produce content CRDT updates, local moves produce root CRDT path changes, and local deletes produce root tombstones. The behavior is observable through daemon unit tests that inspect `.notty/sync.db` and through Docker regression tests that query the backend Postgres database after each operation.

## Progress

- [x] (2026-06-03 00:00Z) Read the daemon runtime, root namespace, document cache, and existing regression test structure.
- [x] (2026-06-03 00:00Z) Added `TestWorkspaceRuntimeFilesystemLifecycleRecordsSQLiteAndDaemonCalls` to drive create, edit, move, edit-after-move, and delete through a temp workspace and validate send callbacks plus SQLite document/root/projection state.
- [x] (2026-06-03 00:00Z) Added `TestLocalFilesystemLifecycleSyncsServerDatabase` to drive the daemon-managed filesystem in Docker and reconstruct root/content state from Postgres after every operation.
- [x] (2026-06-03 00:00Z) Ran focused daemon and Docker regression tests; fixed the production gaps they exposed and reran successfully.

## Surprises & Discoveries

- Observation: Local move reconciliation updates the root CRDT and tracked in-memory path, but existing tests only checked the root CRDT entry, not durable SQLite projection/path state.
  Evidence: `TestCentralReconcilePublishesQueuedCleanMove` checks the decoded root entry and tracked path but not `documents.path` or `root_projection_entries`.

- Observation: The new unit test needs root projection reconciliation to happen after a local move, not only after create and delete.
  Evidence: The move branch now marks the root document dirty after sending the root path update, allowing the normal queue to persist `root_projection_entries`.

- Observation: The Docker regression initially failed on local move because the daemon finalized the root outbox as soon as a websocket message was queued, before the socket writer had actually written it.
  Evidence: `TestLocalFilesystemLifecycleSyncsServerDatabase` timed out waiting for Postgres to show the moved root path; the root entry stayed at the old path while the local unit test with a synchronous fake send passed.

- Observation: The Docker regression still failed on local move after synchronous websocket writes, which means the real runtime can miss the move scan before central dirty reconciliation projects the old path.
  Evidence: The root entry again stayed at the old path in Postgres, while the unit test that manually called `reconcileLocalWorkspace` passed.

- Observation: Failure diagnostics showed the daemon filesystem contained only the moved file, while Postgres contained only the old root path.
  Evidence: The failure printed `paths=[regression/fs-lifecycle-...md]` and `localFiles="./regression/fs-lifecycle-moved-.../renamed.md\n"`.

- Observation: Move detection was working, but the central reconciler repeatedly reported the document still needed work because sent websocket outbox rows were never deleted in the real production finalization path.
  Evidence: Temporary diagnostics showed `local move detected` followed by repeated `document ... still needs reconcile` lines until the test timed out.

## Decision Log

- Decision: Use the existing daemon test runtime and fake backend websocket hook instead of introducing a new filesystem abstraction.
  Rationale: The production behavior is driven by real `workspaceReplica` methods and SQLite writes. Directly invoking those methods with a temp directory is simpler and closer to runtime behavior than a new mock layer.
  Date/Author: 2026-06-03 / Codex

- Decision: Keep the regression test to one document lifecycle but include repeated edits and a move.
  Rationale: The requested guarantee is per-operation correctness in the real backend database. One document exercises create, edits, move, and delete without making the Docker test unnecessarily slow or brittle.
  Date/Author: 2026-06-03 / Codex

- Decision: Update local move reconciliation to refresh the SQLite `documents.path` row and mark the root document dirty after the root CRDT path update.
  Rationale: The move is not complete from a durable daemon perspective until SQLite reflects the new content path and root projection rows can be recomputed by the existing root reconciliation path.
  Date/Author: 2026-06-03 / Codex

- Decision: Make daemon websocket sends wait for the socket writer result before finalizing durable outbox rows.
  Rationale: Queueing is not delivery. The existing durable outbox should clear only after the websocket writer successfully writes the message, otherwise root/content updates can be lost when the socket is reconnecting.
  Date/Author: 2026-06-03 / Codex

- Decision: Run a local workspace scan at the start of dirty document reconciliation.
  Rationale: Local filesystem state must be observed before the daemon projects cached/root state back onto disk. This keeps local moves from being undone when fsnotify event ordering is incomplete or delayed.
  Date/Author: 2026-06-03 / Codex

- Decision: Track and refresh recursive directory watches in `workspaceReplica`.
  Rationale: Local lifecycle behavior should not depend on whether fsnotify observed the creation of an intermediate directory quickly enough. Refreshing watches during scans keeps future operations in existing subdirectories observable.
  Date/Author: 2026-06-03 / Codex

- Decision: Clear each sent outbox row in `finalizeSentOutbox` after the websocket write succeeds and the update is applied locally.
  Rationale: The durable outbox represents unsent work. Keeping a successfully sent row caused the daemon to retry the same content update forever and prevented queued local move metadata from being published.
  Date/Author: 2026-06-03 / Codex

- Decision: Stop clearing outbox rows in common test websocket hooks.
  Rationale: Test hooks should simulate backend acceptance only. Production finalization must own local durable-state cleanup so tests catch regressions in that cleanup.
  Date/Author: 2026-06-03 / Codex

## Outcomes & Retrospective

The daemon unit test now proves local create, edit, move, edit-after-move, and delete produce expected websocket send calls and durable SQLite state. The Docker regression now proves the same lifecycle reaches the real backend Postgres database after each filesystem operation. The implementation also fixed real bugs in local move metadata handling, websocket send durability, recursive directory watch coverage, and production outbox clearing.

## Context and Orientation

The daemon code lives in `daemon/internal/syncer`. A daemon is the background process that watches a workspace directory. A CRDT stream is the Yjs-compatible sequence of updates for one document. The root document is also a CRDT stream; its map named `root.entriesById` tells the daemon which content document ID should appear at which filesystem path. The local SQLite database is `.notty/sync.db`, implemented by `daemon/internal/syncer/document_cache.go`; it stores document rows, CRDT update rows, pending outgoing update rows, and root projection rows.

`daemon/internal/syncer/replica.go` observes filesystem changes. `handleLocalChange` marks tracked files dirty for content edits. `reconcileLocalWorkspace` scans the directory and detects creates, moves, and deletes. `daemon/internal/syncer/service.go` reconciles dirty documents, sends CRDT updates through `sendDocumentUpdate`, and finalizes accepted local updates into SQLite. `daemon/internal/syncer/root_namespace.go` mutates and projects root entries.

The Docker regression tests live in `test/regression/sync_regression_test.go`. They start Postgres, backend, and daemon with Docker Compose, then verify backend state by reconstructing CRDT documents from Postgres tables.

## Plan of Work

Add a daemon unit test in `daemon/internal/syncer/workspace_runtime_test.go`. The test will start a `workspaceRuntime` against a fake HTTP backend, replace the runtime's `sendDocumentUpdate` callback with the existing fake backend hook, and perform real temp-directory operations. After each create, edit, move, and delete, it will reconcile the runtime until idle and assert both callback counts and SQLite state.

Add small test helper functions near the existing workspace runtime helpers to read SQLite rows and decoded root entries. These helpers will stay in test files.

Add a regression test in `test/regression/sync_regression_test.go`. The test will operate on the daemon-managed filesystem using Docker exec, then query Postgres through existing helpers after each step. It will prove local filesystem changes are reflected in the server database, not only in the frontend or daemon filesystem.

If the new tests expose a durable-state bug, change only the production line needed to make the asserted behavior true.

## Concrete Steps

From `/Users/zhongyangxia/Documents/dev/notty`, run:

    go test ./daemon/internal/syncer -run TestWorkspaceRuntimeFilesystemLifecycleRecordsSQLiteAndDaemonCalls -count=1

Then run:

    go test -tags=regression ./test/regression -run TestLocalFilesystemLifecycleSyncsServerDatabase -count=1

Finally run a broader check:

    go test ./daemon/internal/syncer ./test/regression -run 'TestWorkspaceRuntimeFilesystemLifecycleRecordsSQLiteAndDaemonCalls|TestLocalFilesystemLifecycleSyncsServerDatabase' -count=1

The regression package command without `-tags=regression` is expected not to include the Docker regression test.

## Validation and Acceptance

Acceptance requires the daemon unit test to pass and show that create/edit/move/delete produce the expected daemon send calls and durable SQLite rows. Acceptance also requires the Docker regression to pass by reconstructing the expected root entries and document content from Postgres after every local filesystem operation.

## Idempotence and Recovery

The unit test uses `t.TempDir` and is repeatable. The Docker regression uses a unique Compose project name and registers cleanup with `docker compose down -v --remove-orphans`. If a Docker run fails, rerun the same `go test -tags=regression ...` command; the next run gets a new project name.

## Artifacts and Notes

Validation completed with these commands from `/Users/zhongyangxia/Documents/dev/notty`:

    go test ./daemon/internal/syncer -run 'TestWorkspaceRuntimeFilesystemLifecycleRecordsSQLiteAndDaemonCalls|TestOutgoingOutboxClearsOnHTTPAcceptance|TestOutgoingOutboxSurvivesCacheReopenAndResendsIdempotently|TestWorkspaceRuntimeOutboxPostDoesNotBlockPendingRemoteAppend' -count=1
    ok  	notty/daemon/internal/syncer	0.310s

    go test ./daemon/internal/syncer
    ok  	notty/daemon/internal/syncer	1.687s

    go test -tags=regression ./test/regression -run TestLocalFilesystemLifecycleSyncsServerDatabase -count=1
    ok  	notty/test/regression	20.162s

    go test -tags=regression ./test/regression -run TestBackendAPIDocumentLifecycleSyncsDatabaseAndDaemonFilesystem -count=1
    ok  	notty/test/regression	71.885s

    go test ./...
    ok  	notty/backend/internal/notty	(cached)
    ok  	notty/daemon/internal/syncer	(cached)
    ok  	notty/internal/ycrdt	0.329s
    ok  	notty/internal/yproto	(cached)

The Go test output includes existing macOS linker warnings for the bundled Yrs static library being built for a newer macOS version than the local linker target.

## Interfaces and Dependencies

No new production API is introduced. The tests use existing functions: `newWorkspaceRuntime`, `workspaceReplica.reconcileLocalWorkspace`, `workspaceReplica.handleLocalChange`, `workspaceRuntime.processLocalCreates`, `workspaceRuntime.reconcileDirtyDocuments`, `decodeRootEntries`, `documentCache.loadRootProjectionEntries`, and the existing Docker regression stack helpers.

Revision note 2026-06-03: Created this plan to guide the requested daemon SQLite unit tests and server database regression tests.

Revision note 2026-06-03: Added concrete unit and regression tests, plus documented the small local-move durable-state fix needed for root projection correctness.

Revision note 2026-06-03: Documented the websocket send durability fix discovered by the Docker local-move regression.

Revision note 2026-06-03: Documented the dirty-reconcile filesystem scan added after the Docker regression showed local move detection still depended on event timing.

Revision note 2026-06-03: Added the recursive directory-watch decision after diagnostics showed the moved file existed locally but no root update was published.

Revision note 2026-06-03: Recorded the final outbox-clearing fix and successful focused validation.
