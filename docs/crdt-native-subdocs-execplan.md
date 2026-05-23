# Implement CRDT-Native Root Manifest and Generic Streams

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. It is self-contained so a contributor can resume from this file plus the current working tree.

## Purpose / Big Picture

Notty currently stores document path metadata and document CRDT updates through document-specific SQL tables and document-specific HTTP/websocket routes. The proposal in `docs/crdt-native-subdocs-proposal.md` changes that authority model: workspace namespace metadata becomes a root manifest CRDT stream, every file's bytes live in a separate generic content CRDT stream, and the daemon projects those streams to and from a normal filesystem using local SQLite state.

After this work, a user can create, rename, edit, delete, and concurrently create same-path files through filesystem or API workflows while Notty preserves stable document identity, never treats path as identity, and does not overwrite dirty local bytes. The working behavior is demonstrated by backend unit tests, daemon projector and scan tests, and Docker regression tests that run the backend, daemon, and Postgres together.

## Progress

- [x] (2026-05-23 00:00Z) Read `.agent/PLANS.md` and the CRDT-native proposal through Part N.
- [x] (2026-05-23 00:00Z) Confirmed no prior ExecPlan file exists for this migration; created this living plan.
- [x] (2026-05-23 00:20Z) Inventory current backend document-specific storage and routes against proposal milestones D and L.
- [x] (2026-05-23 01:25Z) Inventory current daemon filesystem sync against proposal milestones E through K.
- [x] (2026-05-23 00:45Z) Milestone 1: add generic backend stream storage tables and root stream ID to workspaces.
- [x] (2026-05-23 00:50Z) Milestone 2: add generic stream HTTP and websocket API.
- [x] (2026-05-23 04:25Z) Milestone 3 partial: extend `internal/ycrdt` with document GUID support and native root `YMap` JSON/string operations; root manifest now writes `entriesById` as a native YMap and treats the old text payload as migration fallback.
- [x] (2026-05-23 01:05Z) Milestone 4: add root manifest helpers, validator, and deterministic projection resolver.
- [x] (2026-05-23 12:20Z) Milestone 5: `/api/workspace`, workspace websocket snapshots, and document by-path lookup derive document metadata from the root stream with `desiredPath` exposed separately from materialized `path`.
- [x] (2026-05-23 12:20Z) Milestone 6: document create/move/delete/update aliases mutate root/content streams directly, allow duplicate desired paths, and publish stream update events for CRDT content changes.
- [x] (2026-05-23 01:50Z) Milestone 7: create full daemon `state.sqlite` schema from day one.
- [x] (2026-05-23 01:55Z) Milestone 8: create daemon `fslock.sqlite` mutex and route all mutating `WorkspaceFS` operations through it.
- [x] (2026-05-23 13:05Z) Milestone 9: `RootManifestProjector` handles stat-first local creates, file deletes, rename/move evidence, remote creates/renames, dirty remote-delete preservation, conservative directory materialization, content stream queueing on stat changes, live-only materialized path reservations, untracked-local/remote same-path preservation, and projection path swaps; the daemon runtime has no old document replica.
- [x] (2026-05-23 12:20Z) Milestone 10: generic content stream runtime supports stream websocket sync, HTTP stream POST sends, stream-discovered content websocket startup, primary and agent stream projections, local edit capture, dirty overlap preservation, delete-set updates, and stream-only regression harnesses. Document websocket routes are stream aliases.
- [x] (2026-05-23 13:05Z) Milestone 11: backend SQL document authority is removed; schema initialization drops old document tables and backend/regression tests use generic stream tables. The daemon document cache, workspace replica, document reconcile queue, document sync implementation, and their old tests have been deleted. Thread-anchor resolution now reads stream state.
- [x] (2026-05-23 13:05Z) Milestone 12: scan acceleration, capability probes, bounded pending creates, a 5s content-create stability window, periodic full scan hints, startup local scans, idempotent write-content completion, pending-create reaper, and directory handling are wired.
- [x] (2026-05-23 02:15Z) Milestone 12 partial: add scan stat primitives, FileKey/mtime capability probes, `SameStatTuple`, and `ReadBytesStable`.
- [x] (2026-05-23 02:55Z) Milestone 12 partial: add stat-only `WorkspaceFS.Scan`, hint coalescing, budget/cursor reporting, `.notty` skipping, directory-cache use gated by reliable directory mtime, and FileKey clearing when unreliable.
- [x] (2026-05-23 03:05Z) Milestone 12 partial: add first-boot scan capability initialization that persists FileKey, directory-mtime, and ctime probe results and inserts an initial full scan hint exactly once.
- [x] (2026-05-23 03:50Z) Milestone 9/10 prerequisite: add daemon stream inbox/outbox primitives with idempotent mutation keys, duplicate inbox suppression, cross-stream dependency resolution, local-apply gating, and sendable-row gating.
- [x] (2026-05-23 04:05Z) Milestone 12 partial: add bounded pending content-create processor that claims `needs_bytes` rows, reads targeted bytes with `ReadBytesStable`, creates dependent `content:init` outbox rows, requeues by row/byte budget, and cancels stale paths with scan hints.
- [x] (2026-05-23 04:35Z) Milestone 10 prerequisite: add daemon `StreamSender` that sends only locally-applied, dependency-acked outbox rows and durably records sent/acked state.
- [x] (2026-05-23 08:00Z) Milestone 10 prerequisite: add daemon `HTTPStreamTransport` for generic stream update POSTs with workspace-scoped paths, daemon auth, acting-agent headers, and legacy actor query fallback.
- [x] (2026-05-23 08:05Z) Milestone 3/4 cleanup: move root manifest model, native YMap reader/writer, validator, and resolver into shared `internal/rootmanifest`; backend keeps aliases over the shared package.
- [x] (2026-05-23 08:15Z) Milestone 10 partial: add daemon content projection helpers, content projector local-edit capture, safe write-content fs jobs, dirty divergence preservation, and tests.
- [x] (2026-05-23 08:30Z) Milestone 9 partial: add daemon root manifest projector baseline for stat-only local create/delete/rename intent capture, pending content-create insertion, remote create tracking, and remote rename move-job planning.
- [x] (2026-05-23 08:45Z) Milestone 9/10 partial: add `WorkspaceSyncLoop` ordering path, generic stream websocket inbox receiver, and service wiring for workspace-scoped stream bootstrap/sync/sender.
- [x] (2026-05-23 09:00Z) Milestone 9/12 partial: add clean-hash move detection for unreliable FileKey mode and remote-delete handling that deletes only clean files while preserving dirty bytes for detach/recreate.
- [x] (2026-05-23 09:10Z) Milestone 9 partial: when workspace stream sync is active, daemon local-create candidates become scan hints and old document reconcile/websocket paths are skipped.
- [x] (2026-05-23 10:35Z) Add and pass representative backend tests for generic streams, root/content mirroring, nested/root paths, stream route fallback, and delete-set stream updates. Full proposal M1 audit remains open.
- [x] (2026-05-23 10:35Z) Add and pass representative root resolver/projector tests for duplicate desired paths, conflict materialization, remote creates, remote renames, clean delete, unprojected remote creates, and clean-hash move detection. Full property-style M2 audit remains open.
- [x] (2026-05-23 10:35Z) Add and pass representative SQLite/projector tests for state schema, outbox/inbox dependency gating, pending creates, content projection jobs, tombstoned path reuse, and fs job recovery. Full proposal M3 audit remains open.
- [x] (2026-05-23 10:35Z) Add and pass representative scan acceleration tests for stat equality, stable reads, capability probes, scan hints, budget cursors, cache use, and ignored paths. Full proposal M4 audit remains open.
- [x] (2026-05-23 13:05Z) Add and pass Docker integration coverage in stream-only mode, including backend-seeded duplicate desired paths and true offline-local same-path creates from two daemon workspaces converging to the same conflict paths with both byte payloads preserved.
- [x] (2026-05-23 08:50Z) Run full local Go test suite.
- [x] (2026-05-23 09:25Z) Run Docker end-to-end regression suite at reduced stress size for current iteration.
- [x] (2026-05-23 09:35Z) Run Docker end-to-end regression suite at default stress size for current iteration.
- [x] (2026-05-23 10:40Z) Run full local Go suite after stream runtime fixes: `PATH="$HOME/.cargo/bin:$PATH" go test ./...`.
- [x] (2026-05-23 10:40Z) Run full backend Postgres harness after stream storage fixes: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh`.
- [x] (2026-05-23 12:20Z) Run reduced Docker regression suite after stream projection manager and primary-replica runtime removal: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" NOTTY_REGRESSION_STRESS_LINES=100 go test -tags=regression ./test/regression -count=1`.
- [x] (2026-05-23 12:20Z) Run full default Docker regression suite after pending-create stability hardening: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -count=1`.
- [x] (2026-05-23 12:20Z) Run full local Go suite: `PATH="$HOME/.cargo/bin:$PATH" go test ./...`.
- [x] (2026-05-23 12:20Z) Run backend Postgres harness: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh`.
- [x] (2026-05-23 13:05Z) Remove obsolete daemon document-cache/runtime code and rewrite anchored-thread lookup to use stream state.
- [x] (2026-05-23 13:05Z) Run focused daemon suite after cleanup and offline-create fixes: `PATH="$HOME/.cargo/bin:$PATH" go test ./daemon/internal/syncer`.
- [x] (2026-05-23 13:05Z) Run full local Go suite after cleanup and offline-create fixes: `PATH="$HOME/.cargo/bin:$PATH" go test ./...`.
- [x] (2026-05-23 13:05Z) Run frontend typecheck/Vitest suite: `npm test` from `frontend`.
- [x] (2026-05-23 13:05Z) Run backend Postgres harness: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh`.
- [x] (2026-05-23 13:05Z) Run focused Docker offline-local same-path create regression: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -run TestOfflineLocalSamePathCreatesConvergeToConflictPaths -count=1`.
- [x] (2026-05-23 13:05Z) Run focused Docker append-only regression at default stress after a transient full-suite failure: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -run TestAppendOnlyFileSyncReconstructsBackend -count=1`.
- [x] (2026-05-23 13:05Z) Run full default Docker regression suite with all regression tests: `sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -count=1`.
- [x] (2026-05-23 13:05Z) Complete final requirement audit against the proposal milestones and acceptance criteria.

## Surprises & Discoveries

- Observation: The repository already requires Postgres; `NewStoreForWorkspace` rejects non-Postgres DSNs and calls `initPostgresSchema`.
  Evidence: `backend/internal/notty/store.go` opens the `pgx` driver and errors when `isPostgresDSN(databaseURL)` is false.
- Observation: Current CRDT wrapper exposes Y.Text operations but not Y.Map or document GUID APIs.
  Evidence: `internal/ycrdt/doc.go` exposes `New`, `GetText`, `ApplyUpdateV1`, `StateVectorV1`, and `EncodeStateAsUpdateV1`; no map or GUID methods are present in the initial inspection.
- Observation: Host Go tests initially could not link because the Yrs static library was missing, and host shell has no `cargo`.
  Evidence: `go test ./backend/internal/notty` failed with `cannot find ... third_party/y-crdt/target/release/libyrs.a`; `scripts/build-yffi.sh` failed with `cargo: not found`.
- Observation: Docker is available only through passwordless sudo for this user.
  Evidence: plain `docker run` failed with permission denied on `/var/run/docker.sock`; `sudo -n docker ...` started a Go/Cargo container successfully.
- Observation: `internal/ycrdt` tests need frontend Node dependencies for the Yjs compatibility check.
  Evidence: `go test ./internal/ycrdt` failed with `Cannot find module 'yjs'` before `npm ci` in `frontend`, then passed after dependencies were installed.
- Observation: The old daemon filesystem protection used per-path lock files under `.notty/locks`.
  Evidence: `daemon/internal/syncer/workspace_fs.go` had `lockPaths` creating hashed `.lock` files under `fs.lockRoot()`. The new `WorkspaceFS` initializes `.notty/fslock.sqlite` and mutating operations call `FSLockDB.WithFilesystemLock`.
- Observation: FileKey stat equality is now enforced in one primitive rather than being duplicated in projectors.
  Evidence: `daemon/internal/syncer/scan_stat.go` defines `SameStatTuple`, and tests in `daemon/internal/syncer/scan_stat_test.go` prove FileKey changes, missing FileKeys, and unreliable FileKey capabilities all disable the fast path.
- Observation: `WorkspaceFS.Scan` now returns relative stat-only snapshots and treats scan acceleration as optional.
  Evidence: `daemon/internal/syncer/scan_workspace.go` disables directory cache when `DirectoryMTimeReliable` is false, clears FileKey when `FileKeyReliable` is false, skips only `.notty`, and reports `Incomplete` plus `CursorPath` when the configured budget is exhausted.
- Observation: `scan_state` capability flags are no longer inert schema fields.
  Evidence: `WorkspaceStateDB.InitializeScanCapabilities` runs the prober once when `capabilities_initialized = 0`, stores the resulting flags, and inserts a `full` `capability-probe` scan hint; `TestInitializeScanCapabilitiesRunsOnceAndInsertsFullHint` proves reruns reuse stored flags without duplicate hints.
- Observation: The old document path uniqueness rule was still encoded in Postgres and blocked the proposal's duplicate desired path invariant.
  Evidence: `scripts/test-postgres.sh` initially failed `TestDocumentMutationsMirrorToRootAndContentStreams` with `idx_documents_workspace_path`; schema init now drops that unique index and creates non-unique `idx_documents_workspace_path_lookup`.
- Observation: Cross-stream ordering can now be represented in `state.sqlite`, but no projector is yet producing these rows.
  Evidence: `daemon/internal/syncer/state_stream_queue.go` adds `UpsertOutbox`, `ReadyLocalOutbox`, `NextSendableOutboxRow`, and inbox helpers; `TestStreamOutboxDependenciesGateLocalApplyAndSend` proves content-init rows are blocked until the root-create dependency is acked.
- Observation: The two-phase local-create byte finalizer now exists independently of the root projector.
  Evidence: `daemon/internal/syncer/pending_content_create.go` defines `PendingContentCreateProcessor`; tests prove dependent content-init creation, row-budget requeueing, and stale-path cancellation with a path scan hint.
- Observation: The sender contract from Part F3 is implemented at the state/transport boundary.
  Evidence: `daemon/internal/syncer/stream_sender.go` loops over `NextSendableOutboxRow`, marks rows sent before transport I/O, records acks afterward, and `stream_sender_test.go` proves root rows are sent before dependent content rows and failed sends remain unacked for retry.
- Observation: The daemon now starts stream-native projection managers for the primary workspace and agent workspaces.
  Evidence: `daemon/internal/syncer/stream_projection.go` owns `WorkspaceStateDB`, `WorkspaceFS`, `WorkspaceSyncLoop`, `StreamSender`, and per-stream websocket syncs for one local root; `Service.Run` no longer starts `workspaceReplica.Run` as primary sync authority.
- Observation: New local files need a stronger stability gate than a single stable open/read when writers keep file descriptors open.
  Evidence: the full Docker append regression could otherwise project an early snapshot over an actively appended file; `PendingContentCreateProcessor` now requires a 5s stat-stability window before creating a content-init outbox row.
- Observation: Root projection must not treat unprojected remote entries as local deletes.
  Evidence: stream remote-create regressions orphaned backend-created files under `Recovered/orphans` until file entries without `LastCleanHash` and directories not yet materialized were protected from tombstone capture.
- Observation: Stream content routes need their own sync clients once root projection discovers content streams.
  Evidence: remote-created files initially materialized empty because the content projector ran before any content websocket had fetched the remote stream update; `markStreamDirty` now starts a managed content stream sync for discovered content streams.
- Observation: State vectors alone cannot identify applied Yjs delete-set updates.
  Evidence: stream websocket delete regression posted a 7-byte delete update that changed content but not the state vector, so `ApplyStreamUpdate` now compares encoded document state as well before treating an update as no-op.
- Observation: Destructive local rewrites need different conflict handling from local appends.
  Evidence: Docker overlap rewrite coverage keeps the local rewrite as dirty divergence when a remote rewrite lands concurrently, while append coverage still merges local and remote appends into one content stream.
- Observation: Offline-local same-path creates can arrive after the peer's root entry is visible locally.
  Evidence: the new Docker regression initially produced only one document until `RootManifestProjector` stopped letting an unprojected remote entry claim an existing untracked local file at the same path.
- Observation: Logical root mutations may be re-encoded as different Yjs binary updates during retry/reconcile.
  Evidence: the offline-local Docker regression hit repeated `outbox mutation ... already exists with different update` errors until unapplied rows were replaceable and already locally-applied rows were treated as idempotent reuse.
- Observation: Projection path swaps need SQLite upserts to tolerate temporary path ownership changes.
  Evidence: the offline-local Docker regression hit `UNIQUE constraint failed: manifest_projection.materialized_path` until manifest/content projection path indexes allowed temporary empty paths and upserts cleared the prior owner before assigning the final path.

## Decision Log

- Decision: Implement the migration in proposal milestone order, with backend generic streams and manifest helpers before daemon projector replacement.
  Rationale: The daemon must send root/content mutations to stable backend APIs; building the daemon first would preserve old document authority and create throwaway compatibility code.
  Date/Author: 2026-05-23 / Codex
- Decision: Keep compatibility shims only where the proposal explicitly asks for temporary old endpoints, then remove obsolete authority code once stream-backed behavior covers callers.
  Rationale: The user explicitly requested no compatibility preservation beyond fulfilling the proposal and a clean codebase without obsolete legacy paths.
  Date/Author: 2026-05-23 / Codex
- Decision: Start with deterministic root manifest helpers independent of Y.Map internals if the existing C wrapper cannot expose maps quickly.
  Rationale: The proposal allows canonical JSON for nested values until `internal/ycrdt` has ergonomic nested support. A typed helper over Y.Text or a JSON payload can de-risk projection, validation, and route behavior while preserving stream semantics.
  Date/Author: 2026-05-23 / Codex
- Decision: Store the first root manifest implementation as canonical JSON in `Y.Text("rootManifestJSON")` while retaining generic CRDT stream persistence.
  Rationale: The current `internal/ycrdt` wrapper does not expose Y.Map or GUID operations. This keeps root metadata inside a CRDT stream now and lets resolver, validator, and compatibility shims proceed without path-authority shortcuts. Milestone 3 remains open until native map/GUID support is added or the proposal is revised to accept this representation permanently.
  Date/Author: 2026-05-23 / Codex
- Decision: Move root manifest authority from `Y.Text("rootManifestJSON")` to native `Y.Map("entriesById")`, with the text field retained only as a read fallback for already-written updates.
  Rationale: The yffi layer already contained map/GUID primitives; adding minimal wrappers removes the largest representation workaround without blocking on subdoc convenience references.
  Date/Author: 2026-05-23 / Codex

## Outcomes & Retrospective

Backend generic stream storage, generic stream routes, native-map root manifest resolver helpers, root-derived workspace document metadata, and stream-backed document route aliases are implemented and verified locally, against Postgres, and through Docker regression coverage. Stream update no-op detection handles delete sets, and document websocket routes now delegate to generic stream websockets.

Update, 2026-05-23 13:05Z: backend document-specific SQL tables are no longer created or persisted, and schema initialization drops `documents`, `document_heads`, `document_updates`, and `document_checkpoints`. Document HTTP/websocket routes that remain for clients are stream-backed aliases. The daemon runtime uses stream projection managers for primary and agent workspaces; obsolete document cache, document sync, workspace replica, and reconcile queue code paths have been removed.

Daemon local SQLite initialization and SQLite filesystem locking are implemented. `OpenWorkspaceStateDB` creates the full v1 schema from proposal Part E, initializes the `scan_state` singleton, resets stale `pending_content_creates.reading` rows, and resets stale running `fs_jobs`. `OpenFSLockDB` creates `.notty/fslock.sqlite`; `WorkspaceFS` now serializes mutating operations through it.

Scan safety primitives are implemented for the current runtime. `FileStat`, `ScanCapabilities`, `SameStatTuple`, FileKey/directory-mtime/ctime probes, first-boot capability initialization, `ReadBytesStable`, stat-first `WorkspaceFS.Scan`, periodic full-scan hints, startup scans, and stream projection scanning exist with focused tests. Daemon stream inbox/outbox dependency primitives, the bounded pending-create byte finalizer and reaper, HTTP stream transport, generic stream receiver, sender boundary, root projector, content projector, `WorkspaceSyncLoop`, and stream projection manager also exist. The final cleanup removed obsolete daemon document-cache tests/helpers and added true offline two-daemon allocation coverage.

Change note, 2026-05-23: `WorkspaceFS.Scan` is implemented for stat-first full scans, path hints, directory hints, optional directory-cache reads/writes, and conservative fallback when capabilities are unreliable. Root/content projectors now cover local creates/edits, remote creates/renames, clean deletes, dirty divergence preservation, conservative directory materialization, and clean-hash move detection.

Verification completed:

    npm ci
    go test ./internal/ycrdt ./internal/yproto ./backend/internal/notty
    go test ./daemon/internal/syncer
    go test ./...
    go test ./daemon/internal/syncer
    sudo -n env PATH="$PATH" scripts/test-postgres.sh
    sudo -n env PATH="$PATH" scripts/test-postgres.sh
    sudo -n env PATH="$PATH" NOTTY_REGRESSION_STRESS_LINES=100 go test -tags=regression ./test/regression -count=1
    PATH="$HOME/.cargo/bin:$PATH" go test ./...
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" NOTTY_REGRESSION_STRESS_LINES=100 go test -tags=regression ./test/regression -count=1
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -count=1
    PATH="$HOME/.cargo/bin:$PATH" go test ./daemon/internal/syncer
    PATH="$HOME/.cargo/bin:$PATH" go test ./...
    (cd frontend && npm test)
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -run TestOfflineLocalSamePathCreatesConvergeToConflictPaths -count=1
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -run TestAppendOnlyFileSyncReconstructsBackend -count=1
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -count=1
    PATH="$HOME/.cargo/bin:$PATH" go test ./...
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" NOTTY_REGRESSION_STRESS_LINES=100 go test -tags=regression ./test/regression -count=1
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -count=1
    PATH="$HOME/.cargo/bin:$PATH" go test ./...
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" NOTTY_REGRESSION_STRESS_LINES=100 go test -tags=regression ./test/regression -count=1
    sudo -n env PATH="$HOME/.cargo/bin:$PATH" go test -tags=regression ./test/regression -count=1

The local Go commands passed after `third_party/y-crdt/target/release/libyrs.a` was built. The backend Postgres harness passed under sudo Docker, so the generic stream storage tests have been executed against Postgres rather than only compiled/skipped. The frontend typecheck/Vitest suite passes. The default Docker regression suite passes at full stress, and the focused offline-local same-path create regression passes.

## Context and Orientation

The backend lives in `backend/internal/notty`. `Store` in `backend/internal/notty/store.go` owns persistent workspace state. The existing schema is created by `initPostgresSchema` in `backend/internal/notty/store_postgres.go`. Document-specific HTTP handlers live in `backend/internal/notty/server_documents.go`, and routes are registered in `backend/internal/notty/server_routes.go`. Document websocket rooms are implemented in `backend/internal/notty/rooms.go`. CRDT operations use the local package `internal/ycrdt`, a Go wrapper around the vendored Yrs C library in `third_party/y-crdt`.

The daemon lives in `daemon/internal/syncer`. Runtime sync now flows through `streamProjection`, `WorkspaceSyncLoop`, `RootManifestProjector`, `ContentProjector`, `state.sqlite`, and `fslock.sqlite`. Obsolete document-cache, document-sync, workspace-replica, and reconcile-queue helpers have been removed.

Important terms used in this plan:

Root manifest means the CRDT document that stores filesystem namespace metadata: entry IDs, file or directory kind, parent/name location, tombstones, and content stream IDs for files. It does not store file bytes.

Content stream means a CRDT document that stores one file's bytes in `Y.Text("content")`.

Materialized path means the deterministic local path chosen for an entry after resolving duplicate desired paths, orphaned entries, and cycles. Desired paths are stored in the root manifest; materialized paths are projection output.

FileKey means a strong local filesystem identity such as device plus inode on Unix. It is only trusted after a startup reliability probe.

Projection means the deterministic process of comparing CRDT stream state, local SQLite tracking tables, and filesystem snapshots to plan safe local filesystem operations or outgoing CRDT updates.

## Plan of Work

First, add generic stream storage to the backend schema and store layer without yet deleting document endpoints. This includes `workspaces.root_stream_id`, `crdt_stream_heads`, `crdt_stream_updates`, and `crdt_stream_checkpoints`. The store must be able to apply a Yjs update to any stream ID, dedupe by SHA-256, advance the stream head, checkpoint periodically, restore a stream from checkpoint plus tail, and bootstrap a workspace root stream.

Second, add backend route handlers for `GET /api/workspaces/{workspaceID}/bootstrap`, `POST /api/workspaces/{workspaceID}/streams/{streamID}/updates`, and `GET /ws/workspaces/{workspaceID}/streams/{streamID}`. The websocket must use the same y-protocol framing as document websockets. Document-specific websocket routes should become aliases to content stream routes once content streams are in place.

Third, implement root manifest helpers in backend/internal code. The helper must represent entries by ID, validate root manifest schema and immutable fields, allow duplicate desired paths, and resolve deterministic materialized paths including conflict suffixes, tombstoned entries, orphan recovery, and cycles. If `internal/ycrdt` lacks map support, store a canonical JSON manifest payload in a CRDT text field for the first implementation while preserving stream persistence and update semantics.

Fourth, convert the workspace metadata API and old document create/move/delete/update endpoints into shims over root and content streams. `GET /api/workspace` must derive document metadata from the root stream, with `path` as the materialized path and `desiredPath` exposed separately. Old document create must emit a root create and content init. Old move must update the root `loc` atomically. Old delete must set a tombstone. Old by-path lookup must resolve materialized paths from root.

Fifth, build daemon local state. Add full `state.sqlite` schema from proposal Part E, including stat cache columns, scan tables, pending content creates, and fs jobs. Add `fslock.sqlite` with a single-row lock table and make every `WorkspaceFS` mutation acquire it. Reset stale running jobs and reading pending creates on startup.

Sixth, replace daemon document-specific sync with `WorkspaceSyncLoop`, `RootManifestProjector`, and `ContentProjector`. The root projector must use stat-first byte-lazy scans and two-phase local create. The content projector must use `SameStatTuple` with FileKey included, read bytes only when necessary, and write remote content using `WriteIfUnchanged` so dirty bytes are preserved.

Seventh, wire scan acceleration. Initialize scan capabilities on first boot, insert full scan hints, implement bounded hint draining and overflow-to-full behavior, directory cache caps, periodic full scan scheduling, bounded FileKey/hash move detection, stable byte reads without holding the filesystem lock through large reads, directory entries, and pending-create cleanup.

Finally, remove obsolete SQL document authority and document-specific daemon code paths that no longer define source of truth. Keep aliases only where tests and clients still need old route names, and ensure those aliases call stream-backed code.

## Concrete Steps

Run commands from repository root `/home/ubuntu/notty`.

To inspect current state:

    git status --short --branch
    rg -n "documents|document_heads|document_updates|DocumentRoom|workspace_fs|state.sqlite|fslock" backend daemon internal test

After each backend milestone:

    go test ./internal/ycrdt ./internal/yproto ./backend/internal/notty

After each daemon milestone:

    go test ./daemon/internal/syncer

Before claiming broad progress:

    go test ./...

For Docker end-to-end validation:

    docker compose -f test/regression/docker-compose.yml up --build --abort-on-container-exit

The Docker command is expected to exit with status 0 when regression tests pass. If it fails, inspect service logs, fix the failing requirement, and rerun.

## Validation and Acceptance

Backend generic streams are accepted when tests prove that posting a stream update stores one row, advances `crdt_stream_heads`, dedupes repeated update bytes without advancing twice, restores checkpoint plus tail to the same CRDT state as full replay, and syncs root/content streams over y-protocol websocket.

Root manifest behavior is accepted when tests prove duplicate sibling desired paths resolve deterministically by lexical entry ID, conflict suffixes are projection-only, tombstoned entries disappear from materialized output, orphaned and cyclic entries materialize under `Recovered/orphans/<entryID>/`, and directory descendant paths update when a directory location changes.

Compatibility shims are accepted when old create/move/delete/by-path document endpoints mutate root/content streams and the workspace response is derived from root. No test should need `documents.path` as authoritative source of truth after this milestone.

Daemon local state is accepted when startup creates the full v1 `state.sqlite` and `fslock.sqlite` schemas, resets stale rows, initializes scan capabilities, and tests can recover from crashes after local create allocation, fs job creation, and file write completion.

Projection is accepted when local create is two-phase and byte-lazy, content init waits for root create acknowledgement, content local edit reads bytes only when stat fast path is invalid, remote writes never overwrite dirty bytes, local rename preserves content stream identity when evidence is strong, and local delete tombstones root entries.

Scan acceleration is accepted when tests prove FileKey is included in stat equality, unreliable FileKey disables content stat short-circuiting and FileKey move detection, scan hint overflow collapses to one full hint, directory cache caps are enforced, full scans fall back to budgeted readdir when capabilities are unreliable, and `ReadBytesStable` does not hold the filesystem lock while reading bytes.

Docker integration is accepted when the Part M5 scenarios pass: offline same-path creates converge to the same conflict paths on both daemons, edit plus rename preserves path and content, delete plus edit tombstones the old document and creates a new document for dirty bytes, primary and agent workspaces merge edits to one content stream, and remote rename colliding with local untracked bytes preserves all bytes.

## Idempotence and Recovery

Schema migrations must use `CREATE TABLE IF NOT EXISTS`, `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, and idempotent indexes where possible. Applying the schema repeatedly must not destroy data. Stream updates are deduped by `(workspace_id, stream_id, update_sha256)` so network retries are safe. Outbox rows are keyed by `(stream_id, mutation_key)` so daemon retries reuse the same logical mutation.

Daemon filesystem writes must be expressed as durable `fs_jobs` before the write runs. On restart, `running` jobs reset to `pending`; if a prior write already produced the target hash, the job is marked done and projection state advances instead of rewriting blindly.

The local create path must persist generated entry IDs before any network send or byte read. On restart, pending rows reuse the same entry ID and do not create duplicate documents.

## Artifacts and Notes

Initial inspection found the current backend has document-specific storage and routes. Relevant files include:

    backend/internal/notty/store_postgres.go
    backend/internal/notty/server_documents.go
    backend/internal/notty/server_routes.go
    backend/internal/notty/rooms.go
    internal/ycrdt/doc.go

The worktree initially showed:

    ## main...origin/main
     M docs/crdt-native-subdocs-proposal.md

That proposal modification is treated as existing work and must not be reverted unless explicitly requested.

## Interfaces and Dependencies

Backend stream storage must expose store-level methods equivalent to:

    type StreamKind string

    type ApplyStreamUpdateResult struct {
        Accepted bool
        Applied bool
        UpdateID int64
        StateVector []byte
    }

    func (s *Store) BootstrapWorkspaceStreams() (rootStreamID string, err error)
    func (s *Store) ApplyStreamUpdate(workspaceID string, streamID string, update []byte, meta OperationMeta) (ApplyStreamUpdateResult, error)
    func (s *Store) RestoreStreamDoc(workspaceID string, streamID string) (*crdt.Doc, int64, []byte, error)
    func (s *Store) GetStreamHead(workspaceID string, streamID string) (updateID int64, stateVector []byte, err error)

Root manifest helpers must expose:

    func ReadRootManifest(doc *crdt.Doc) (RootManifest, error)
    func ApplyRootIntents(doc *crdt.Doc, intents []RootIntent) ([]byte, error)
    func ValidateRootManifest(previous RootManifest, next RootManifest) error
    func ResolveMaterializedPaths(manifest RootManifest) Projection

Daemon scan and filesystem interfaces must expose the proposal shapes:

    func SameStatTuple(cached FileStat, current FileStat, caps ScanCapabilities) bool
    func (fs *WorkspaceFS) Scan(ctx context.Context, opts ScanOptions) (WorkspaceScan, error)
    func (fs *WorkspaceFS) ReadBytesStable(ctx context.Context, path string, opts StableReadOptions) (ReadBytesResult, bool, error)
    func (fs *WorkspaceFS) WriteIfUnchanged(ctx context.Context, path string, expected Hash, content []byte) error
    func (fs *WorkspaceFS) DeleteIfUnchanged(ctx context.Context, path string, expected Hash) error
    func (fs *WorkspaceFS) MoveIfNoTarget(ctx context.Context, from string, to string) error

Change note, 2026-05-23: Created the ExecPlan because the requested migration is a complex feature/refactor and `AGENTS.md` requires use of `.agent/PLANS.md` for such work.

Change note, 2026-05-23: Updated progress after adding generic stream tables and APIs in `backend/internal/notty/store_streams.go` and `backend/internal/notty/server_streams.go`, plus root manifest helpers in `backend/internal/notty/root_manifest.go`. Recorded the Docker/Cargo and frontend dependency verification findings so the next contributor understands how tests were made runnable.

Change note, 2026-05-23: Updated progress after adding daemon `state.sqlite` and `fslock.sqlite` implementations in `daemon/internal/syncer/state_sqlite.go` and `daemon/internal/syncer/fslock_sqlite.go`, wiring `WorkspaceFS` mutations through the SQLite lock, and passing the full Go suite.

Change note, 2026-05-23: Updated progress after adding scan stat/capability primitives and stable byte reads in `daemon/internal/syncer/scan_stat.go` and `daemon/internal/syncer/stable_read.go`, with tests for FileKey-sensitive stat equality and stable-read replacement rejection.

Change note, 2026-05-23: Recorded Docker validation evidence. The isolated Postgres backend harness passed, and the regression package passed with `NOTTY_REGRESSION_STRESS_LINES=100`. The first single-test Docker regression attempt timed out waiting for `codex-agent.log` while images were still cold, then the same test passed on rerun with cached images; final completion still requires default-size regression.
