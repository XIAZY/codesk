# Harden CRDT Stream Sync After Review

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` from the repository root. It is self-contained: a contributor should be able to start from this file plus the current working tree and complete the review hardening work without reading prior chat context.

## Purpose / Big Picture

PR #2 implements the large CRDT-native stream sync migration: workspace namespace metadata is stored in a root manifest CRDT stream, each file's bytes are stored in a separate content CRDT stream, and the daemon projects those streams to and from a local filesystem. The review comments identified several risks that can corrupt local state after crashes, accept invalid stream writes, drop legitimate repeated root mutations, or leave filesystem jobs stuck.

After this plan is complete, a user can rely on the stream sync system under realistic crash, retry, rename, conflict, and agent-copy workflows. The observable proof is that targeted regression tests demonstrate previously risky cases, the full local and Docker suites pass, and the PR description can truthfully say the state machine is crash-safe enough for merge.

## Progress

- [x] (2026-05-23 21:10Z) Read `.agent/PLANS.md` and confirmed that this review follow-up is large enough to require an ExecPlan.
- [x] (2026-05-23 21:10Z) Read `docs/crdt-review-comments.md` and classified the comments into merge-blocking correctness issues, second-pass correctness issues, and cleanup/documentation issues.
- [x] (2026-05-23 21:10Z) Cross-checked the most important review claims against code in `daemon/internal/syncer`, `backend/internal/notty`, and `internal/rootmanifest`.
- [x] (2026-05-23 23:22Z) Added focused tests for the quick correctness fixes before editing implementation code.
- [x] (2026-05-23 23:22Z) Implemented quick backend/root/stream safety fixes: hard-delete rejection, payload-sensitive root mutation keys, stream GUID use, stream authorization, root-first compatibility create, and stream event kind filtering.
- [x] (2026-05-23 23:22Z) Implemented production filesystem lock fail-closed behavior and removed active no-op lock-file remnants.
- [x] (2026-05-23 23:22Z) Replaced actor-type projection behavior with an explicit projection mode in root projection.
- [x] (2026-05-23 23:27Z) Implemented crash-atomic local stream reconciliation with transaction-scoped state methods.
- [x] (2026-05-23 23:29Z) Delayed projection path advancement until filesystem move jobs actually complete.
- [x] (2026-05-23 23:32Z) Made retryable filesystem job failures retryable and added deterministic handling for move collisions and swaps.
- [x] (2026-05-23 23:34Z) Clarified and implemented first-class directory delete semantics.
- [x] (2026-05-23 23:35Z) Documented namespace normalization and remaining compatibility policies.
- [x] (2026-05-24 00:18Z) Fixed stale content outbox retry semantics by adding terminal dropped outbox rows that are excluded from pending/sendable queues while still preserving evidence of unsent local edits.
- [x] (2026-05-24 00:39Z) Fixed tombstoned-content liveness so a content stream no longer referenced by the root manifest cannot apply local outbox or project dirty bytes back to the filesystem.
- [x] (2026-05-24 00:48Z) Ran full local, frontend, Postgres, live, and Docker regression validation.
- [ ] Update PR #2 with a summary of the review fixes and verification evidence.

## Surprises & Discoveries

- Observation: Local stream reconciliation currently marks inbox rows and outbox rows applied before the new stream state is persisted.
  Evidence: `daemon/internal/syncer/workspace_sync_loop.go` calls `ApplyReadyLocalOutbox` and `ApplyUnappliedInbox`, and `daemon/internal/syncer/state_stream_state.go` marks rows applied inside those helpers before `PersistLatestStreamDoc` is called.
- Observation: Root mutation keys currently identify only intent type and entry ID, not the desired payload.
  Evidence: `daemon/internal/syncer/root_projector.go` has `rootMutationKey` building strings like `loc:<entryID>`.
- Observation: Backend stream update APIs can restore a new unknown stream doc for arbitrary stream IDs.
  Evidence: `backend/internal/notty/store_streams.go` returns `crdt.New()` with `StreamKindUnknown` when `restoreStreamDocForUpdateTx` cannot find a stream head.
- Observation: Backend stream docs are restored with `crdt.New()` rather than `crdt.New(crdt.WithGUID(streamID))`.
  Evidence: `backend/internal/notty/store_streams.go` uses `crdt.New()` in `restoreStreamDocForUpdateTx`, `restoreStreamDocForHeadTx`, and the at-update empty-state path.
- Observation: Root projection writes `manifest_projection.materialized_path` and `content_projection.materialized_path` before queued move jobs run.
  Evidence: `RootManifestProjector.PlanApplyMerged` upserts projection rows before inserting `move-entry` filesystem jobs.
- Observation: Filesystem job insertions cannot revive failed jobs.
  Evidence: `WorkspaceStateDB.InsertFSJob` uses `ON CONFLICT(job_key) DO NOTHING`, and `nextPendingFSJob` only selects `status = 'pending'`.
- Observation: Filesystem locking currently fails open.
  Evidence: `NewWorkspaceFS` sets `lock = nil` if `OpenFSLockDB` fails, and `withFilesystemLockContext` runs the operation directly when `fs.Locks == nil`.
- Observation: Some review comments are reasonable cleanup but not immediate correctness blockers.
  Evidence: `DocumentRoom` naming, `ClientIDSeed: 1001`, and hard-coded log path suppression are real design smells, but they do not explain a concrete data-loss or crash-recovery failure in the current code paths.
- Observation: The quick safety fixes are covered by targeted package tests before moving on to transaction and projection recovery work.
  Evidence: `PATH="$HOME/.cargo/bin:$PATH" go test ./internal/rootmanifest ./daemon/internal/syncer ./backend/internal/notty` exited 0 at 2026-05-23 23:22Z.
- Observation: Inbox/outbox applied markers now roll back with stream state persistence.
  Evidence: `TestApplyStreamQueueAtomicallyRollsBackOutboxMarkersWithState` and `TestApplyStreamQueueAtomicallyRollsBackInboxMarkersWithState` inject a pre-commit error, then verify no applied markers and no `stream_states` rows were committed before retrying successfully.
- Observation: Root rename planning now records desired paths separately from completed materialization.
  Evidence: `TestRootManifestProjectorPlanRemoteRenameSchedulesMove` verifies content and manifest projections remain at `old.md` while the move job is pending, then advance to `new.md` only after `RunPendingFSJobs` completes.
- Observation: Move conflicts now have a retryable status and swaps use a deterministic temp move through `.notty/tmp/moves`.
  Evidence: `TestRetryableMoveJobCanBeRevivedAfterCollisionClears` verifies collision recovery, and `TestRootManifestProjectorPlansSwapThroughTempMove` verifies a two-file swap completes through a temp move and advances both projection rows.
- Observation: Missing directories are no longer treated as one case.
  Evidence: `TestRootManifestProjectorDoesNotTombstoneUnprojectedRemoteCreate` still schedules mkdir for an unmaterialized remote directory, while `TestRootManifestProjectorTombstonesLocallyDeletedDirectoryTree` tombstones a previously materialized deleted directory and child.
- Observation: The implementation summary now states the explicit normalization and compatibility policies instead of leaving them implicit in code.
  Evidence: `docs/crdt-native-subdocs-implementation-summary.md` documents `NormalizeName`, conflict paths, stream-backed compatibility aliases, `ClientIDSeed: 1001`, `DocumentRoom` naming, and log-path notification suppression.
- Observation: Dropping backend-rejected stale content rows cannot erase the fact that local dirty bytes existed.
  Evidence: the dirty-delete Docker regression initially flaked because a stale old-stream content edit was locally applied, backend-rejected, then ignored by root delete planning; the old stream's projected clean hash advanced to the dirty bytes and a `delete-clean-entry` job removed the file. `TestWorkspaceSyncLoopDropsLocalOutboxForTombstonedContentStream` now proves tombstoned content outbox is dropped before local apply, and `TestDroppedOutboxRowsAreNotPendingOrSendable` proves dropped rows stop queue progress without disappearing as local mutation evidence.

## Decision Log

- Decision: Treat crash-atomic reconciliation, stream authorization, payload-sensitive mutation keys, root-first create, delayed projection path advancement, retryable filesystem jobs, and fail-closed filesystem locking as merge-blocking correctness work.
  Rationale: These issues can lose updates, accept unauthorized state, drop user renames, leave state inconsistent after crashes, or silently run without the locking the design depends on.
  Date/Author: 2026-05-23 / Codex
- Decision: Defer a full field-level root manifest storage rewrite unless tests show current whole-entry JSON values are causing unavoidable convergence loss.
  Rationale: The review comment is architecturally right that separate CRDT fields are stronger, but backend validation already rejects tombstone removal in sequential application. The immediate high-value fix is to reject hard deletes and add payload-sensitive mutation keys; a field-level rewrite is larger and should not block smaller correctness fixes.
  Date/Author: 2026-05-23 / Codex
- Decision: Keep stream-backed compatibility routes for now, but make their internals obey root/content stream authority.
  Rationale: The migration intentionally preserves old HTTP and websocket surfaces for clients. The problem is not the route names; it is whether they mutate streams in the same safe order and with the same validation as daemon-originated updates.
  Date/Author: 2026-05-23 / Codex
- Decision: Replace actor-type conditional projection behavior with an explicit projection mode instead of removing the behavior outright.
  Rationale: Agent-copy workspaces are intentionally more conservative about tombstoning missing files. That is a projection role, not actor provenance, so the code should say so directly.
  Date/Author: 2026-05-23 / Codex
- Decision: Keep dropped outbox rows visible to root dirty-byte decisions, but invisible to send/local-apply queues.
  Rationale: A backend-rejected content edit is no longer sendable, but it still proves local bytes existed and were not accepted by the authoritative root/content stream namespace. Treating it as nonexistent can turn unsent local edits into "clean" bytes and delete them during remote tombstone handling.
  Date/Author: 2026-05-24 / Codex

## Outcomes & Retrospective

Implemented. The review-hardening pass fixed the merge-blocking correctness issues identified in review while leaving lower-value mechanical cleanup deferred. The final state machine now rejects hard root deletes, authorizes stream writes against root/content namespace authority, applies local queue state atomically, advances projection paths only after filesystem work succeeds, retries recoverable filesystem jobs, treats directory deletes explicitly, fails closed on filesystem lock initialization, and keeps projection mode separate from actor type.

The main late discovery was that "dropped" is not the same thing as "never happened." A stale content edit rejected by the backend must stop blocking later sends, but root delete planning must still preserve the corresponding dirty local bytes. The final implementation models that distinction with dropped outbox metadata plus a root-liveness gate before content local apply.

## Context and Orientation

The backend code lives under `backend/internal/notty`. A backend `Store` owns persistent workspace state. Generic CRDT stream persistence is in `backend/internal/notty/store_streams.go`. Stream HTTP and websocket handlers are in `backend/internal/notty/server_streams.go`. Stream-backed document compatibility helpers are in `backend/internal/notty/store_root_documents.go` and `backend/internal/notty/server_documents.go`.

The daemon code lives under `daemon/internal/syncer`. In this repository, "daemon" means the local sync process that watches a workspace directory, talks to the backend, and projects CRDT streams onto files. The daemon's local SQLite database is represented by `WorkspaceStateDB`. Its stream sync loop is `WorkspaceSyncLoop` in `daemon/internal/syncer/workspace_sync_loop.go`. Root namespace projection is handled by `RootManifestProjector` in `daemon/internal/syncer/root_projector.go`. File content projection is handled by `ContentProjector` in `daemon/internal/syncer/content_projector.go`.

A "root stream" is a CRDT document that stores filesystem namespace metadata. It says which entries exist, their IDs, whether they are files or directories, each entry's parent/name location, tombstone state, and the content stream ID for each file.

A "content stream" is a CRDT document that stores one file's text bytes in `Y.Text("content")`.

A "projection row" is local SQLite state that records what CRDT state has been safely materialized on the local filesystem. A projection row should describe completed local materialization, not merely a desired future filesystem operation.

An "outbox row" is a durable local mutation waiting to be applied locally and sent to the backend. An "inbox row" is a durable remote mutation received from the backend and waiting to be applied locally.

A "filesystem job" is a durable local operation such as moving, deleting, or writing a file. Filesystem jobs must be crash-safe: if the daemon crashes mid-work, restart should either retry the job or detect that the job already completed.

## Plan of Work

Milestone 1 fixes quick correctness issues that do not require reshaping the local transaction model. Add tests first. In `internal/rootmanifest/root_manifest.go`, change `Validate` so an entry present in the previous manifest cannot disappear from the next manifest; non-root entries must be tombstoned instead. Add a test in `internal/rootmanifest/root_manifest_test.go` proving hard deletion is rejected while tombstoning is allowed.

In `daemon/internal/syncer/root_projector.go`, change `rootMutationKey` to hash canonical intent payloads, not just intent type and entry ID. A `loc` intent key must include the canonical JSON location; a tombstone intent key must include tombstone data; create intents must include the canonical entry. Add a test proving two renames of the same entry to different locations produce different mutation keys and both can enter the outbox.

In `backend/internal/notty/store_streams.go`, instantiate stream docs with `crdt.WithGUID(streamID)` whenever the backend restores or creates a CRDT doc for a stream. Add tests that inspect a restored doc's GUID if the wrapper exposes it; if not, add a focused test around the lower-level wrapper if needed.

In `backend/internal/notty/store_streams.go` and `backend/internal/notty/server_streams.go`, reject external writes to unknown non-root streams. A stream update should be allowed if the stream ID is the workspace root stream, or if the current root manifest references the stream ID as a live file's `contentStreamId`. Internal compatibility-create code may use a private helper that creates root first and then initializes content. Add tests for `POST /streams/random/updates` returning a client error and for valid content stream updates still passing.

In `backend/internal/notty/store_root_documents.go`, reorder `MirrorDocumentCreateToStreams` so root create is accepted before content init, or implement one backend SQL transaction that applies root and content together. The preferred first implementation is one transaction, because it avoids root-without-content if content init fails. If one transaction is too invasive, root-first is acceptable only with an explicit follow-up note. Add a test where root validation failure does not leave an orphan content stream.

In `backend/internal/notty/server_streams.go`, publish stream kind in `stream.updated` events. Root updates should carry `kind: "root"` and content updates should carry `kind: "content"`. In `daemon/internal/syncer/service.go`, suppress content-stream events from broad workspace refresh and agent wakeups. Add tests for event payload kind and daemon event filtering.

Milestone 2 makes filesystem locking fail closed and removes zombie lock-file APIs. Change `NewWorkspaceFS` to return `(*WorkspaceFS, error)` or add a new `OpenWorkspaceFS` constructor that returns an error. Production runtime code such as `newStreamProjection` must use the failing constructor and return startup errors if `.notty/fslock.sqlite` cannot be opened. Tests can use an explicit unsafe constructor or helper, but the unsafe name must say it does not enforce lock creation. Remove or stop using `CleanupStaleLocks`, `lockRoot`, and no-op `lockPaths`. Update callers and tests. Add one test that makes lock DB creation fail and proves production construction returns an error.

Milestone 3 replaces actor-type projection policy with explicit projection mode. Add a `ProjectionMode` type in `daemon/internal/syncer/root_projector.go` or a small nearby file. Use values such as `ProjectionPrimary` and `ProjectionAgentCopy`. Add a `Mode ProjectionMode` field to `RootManifestProjector` and configure it from `streamProjection` construction. Replace `isAgentReplica()` with `isAgentCopyProjection()`. Existing agent-copy behavior should remain, but tests should assert mode behavior without setting `ActorType: "agent"`.

Milestone 4 makes local stream reconciliation crash-atomic. Introduce transaction-scoped methods in `WorkspaceStateDB`, likely by adding a small `WorkspaceStateTx` wrapper around `*sql.Tx`. The atomic operation must load the latest stream state, apply ready local outbox rows, apply unapplied inbox rows, insert the new `stream_states` row, update `streams.latest_state_id`, and mark the exact outbox/inbox rows applied inside one SQLite transaction. Filesystem I/O and network sends must remain outside this transaction. Add crash-simulation tests that fail before this change: mark an inbox row applied, simulate a crash before persisting state, and prove the new code cannot reach that partially-applied state. A practical test can use a hook or helper that intentionally returns an error before commit and then verifies no inbox/outbox applied markers were written.

Milestone 5 separates desired projection state from completed filesystem projection. `RootManifestProjector.PlanApplyMerged` currently updates projection paths before move jobs run. Change this so move jobs carry the desired source and target, but `manifest_projection.materialized_path` and `content_projection.materialized_path` stay at the last successfully materialized path until job completion. On successful `move-entry`, update the corresponding projection rows in the same local SQLite step that marks the job done. If schema additions are needed, add `desired_materialized_path` or job metadata columns, but prefer using existing job fields first. Add restart tests where a remote rename is planned, the daemon crashes before the move, and restart still knows the file is at the old path and retries the move rather than treating the old path as untracked.

Milestone 6 makes filesystem jobs retryable and handles move collisions. Classify filesystem errors as retryable, conflict-needs-planning, or fatal. Retryable errors should not become permanent `failed` rows that cannot be revived. Change `InsertFSJob` or add `UpsertFSJob` so an existing failed retryable job can be moved back to pending with attempts/backoff metadata. For `ErrPathCollision`, prefer deterministic conflict or detach planning over permanent failure. For path swaps and cycles, implement a generic move planner that uses a temporary path under `.notty/tmp/moves/` or materializes one side to a deterministic conflict path. Add tests for `A -> B` and `B -> A`, plus a failed collision that later becomes retryable.

Milestone 7 clarifies directory deletion semantics. Directories are first-class root entries, so a missing tracked directory cannot always mean "remote directory not yet projected." Track whether the local root projection is already caught up to the root state. If a tracked directory is missing and root projection is behind, schedule or retry `mkdir`. If root projection is current and a full or sufficiently targeted scan confirms the directory is missing, emit tombstones for the directory and live descendants in deterministic order. Add tests for local empty directory delete, local `rm -rf` of a tracked directory tree, and remote tombstoned parent with a concurrent live child becoming recovered/orphaned rather than corrupting the tree.

Milestone 8 addresses cleanup and documentation that should not obscure the state-machine fixes. Document namespace normalization in `docs/crdt-native-subdocs-implementation-summary.md` or a new short doc: for v1, names are trimmed and case-insensitive through `rootmanifest.NormalizeName`. Decide whether hard-coded log path notification suppression remains; if it remains, document it as an existing product policy outside the CRDT projection model. Rename `DocumentRooms`, `DocumentRoom`, and `DocumentConn` to stream names only if the PR has stabilized and the diff risk is acceptable. Treat `ClientIDSeed: 1001` as a legacy response field and either document it or remove it from new stream-native paths if tests show no client depends on it.

Milestone 9 validates the complete hardening pass. Run focused tests after every milestone, then run the broad suite. The full pass must include local Go tests, frontend tests/build, installer script tests, Postgres-backed tests, live smoke tests, and Docker regression tests. Update PR #2 with the fixes and the validation evidence.

## Concrete Steps

Start from the repository root:

    cd /home/ubuntu/notty
    git status --short --branch

Expected state before implementation is a branch based on `codex/crdt-native-subdocs`. There may be an untracked `docs/crdt-review-comments.md` file; do not rely on it for implementation because this ExecPlan contains the actionable review context.

Run focused searches before editing:

    rg -n "func Validate|rootMutationKey|ReconcileOne|ApplyReadyLocalOutbox|ApplyUnappliedInbox|PersistLatestStreamDoc" internal daemon
    rg -n "MirrorDocumentCreateToStreams|handlePostStreamUpdateForID|restoreStreamDocForUpdateTx|ApplyStreamUpdate" backend/internal/notty
    rg -n "NewWorkspaceFS|CleanupStaleLocks|lockPaths|stream.updated|isAgentReplica" daemon backend

For Milestone 1, add tests first:

    PATH="$HOME/.cargo/bin:$PATH" go test ./internal/rootmanifest ./daemon/internal/syncer ./backend/internal/notty

Expect one or more new tests to fail before implementation. After each Milestone 1 fix, rerun the same command until it passes.

For Milestone 2 and daemon constructor changes, many tests call `NewWorkspaceFS`. Update tests deliberately, then run:

    PATH="$HOME/.cargo/bin:$PATH" go test ./daemon/internal/syncer

For Milestones 4 through 7, prefer small commits and targeted tests. Add tests named around the failure mode, for example:

    TestWorkspaceSyncLoopDoesNotMarkInboxAppliedWithoutPersistingState
    TestRootMutationKeyIncludesLocationPayload
    TestPostStreamUpdateRejectsUnreferencedContentStream
    TestRootProjectionKeepsOldPathUntilMoveJobSucceeds
    TestFailedRetryableFSJobCanBeRevived
    TestRootManifestProjectorTombstonesLocallyDeletedDirectoryTree

Do not proceed to the next milestone while the focused package tests fail.

## Validation and Acceptance

The plan is accepted only when the following behavior is demonstrated:

Root validation rejects hard deletion. A test should start from a manifest containing `doc_a`, apply a next manifest missing `doc_a`, and observe an error that says removal is not allowed. A separate tombstone test should still pass.

Root outbox mutation keys are payload-sensitive. A test should simulate two local renames of the same entry to different names before ack and observe two distinct durable outbox rows or a later row whose key cannot collide with the earlier desired location.

Unknown stream writes are rejected. A backend HTTP or store test should attempt to apply an update to `random_stream_not_in_root` and receive a client error, while applying to the root stream and a root-referenced content stream still succeeds.

Compatibility create is root-authorized. A test should force root create failure and prove no content stream head/update is left behind for the attempted document ID. A normal create should still return a document that appears in workspace metadata with initial content readable through the content stream.

Local reconciliation is crash-atomic. A test should exercise an injected failure during reconciliation and prove that inbox/outbox applied markers and `streams.latest_state_id` either all changed together or none changed.

Projection paths describe completed filesystem state. A remote rename test should show that before the move job succeeds, projection rows still point to the old path. After job success, they point to the new path.

Retryable filesystem failures do not get stuck. A test should make a move fail with a retryable collision, clear the collision, reconcile again, and observe the job complete.

Filesystem locking fails closed in production construction. A test should make the lock DB unavailable and prove production workspace FS creation returns an error rather than silently disabling locks.

The full final command set is:

    PATH="$HOME/.cargo/bin:$PATH" go test ./...
    cd frontend && npm test && npm run build
    cd /home/ubuntu/notty
    sh -n deploy/daemons/install.sh && sh -n deploy/daemons/uninstall.sh && sh scripts/test-daemon-installer.sh && sh scripts/test-daemon-uninstall.sh
    sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" scripts/test-postgres.sh
    sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" scripts/test-live.sh
    sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" go test -tags=regression ./test/regression -count=1

All commands must exit 0. Vite chunk-size warnings are acceptable if tests and build succeed. Deprecated Vite esbuild/oxc warnings are acceptable unless this plan is expanded to include frontend build cleanup.

Final validation run on 2026-05-24:

    PATH="$HOME/.cargo/bin:$PATH" go test ./...
    cd frontend && npm test && npm run build
    cd /home/ubuntu/notty
    sh -n deploy/daemons/install.sh && sh -n deploy/daemons/uninstall.sh && sh scripts/test-daemon-installer.sh && sh scripts/test-daemon-uninstall.sh
    sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" scripts/test-postgres.sh
    sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" scripts/test-live.sh
    sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" go test -tags=regression ./test/regression -count=1 -timeout 30m -v

All exited 0. Frontend build retained the known Vite esbuild/oxc deprecation warnings and chunk-size warning.

## Idempotence and Recovery

All schema changes must be idempotent. Use `CREATE TABLE IF NOT EXISTS`, `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` where supported by the target database, and safe index creation. Do not write migrations that destroy user data as part of this review hardening work.

Local SQLite changes must tolerate restart. If a new transaction-scoped helper is introduced, keep the old public helper only if tests or other code still need it; otherwise remove it once callers are migrated. Do not mark inbox, outbox, filesystem job, or projection state as complete unless the corresponding durable state has been written in the same transaction or the filesystem operation has actually succeeded.

Docker regression runs may leave containers if interrupted. Clean them with:

    sudo -n docker ps --format '{{.Names}}' | rg 'notty' || true
    sudo -n docker compose -f test/regression/docker-compose.yml -p <project-name> down -v --remove-orphans

Do not use destructive git commands such as `git reset --hard` or `git checkout --` unless the user explicitly asks. This branch may contain user-created review notes such as `docs/crdt-review-comments.md`; leave unrelated untracked files alone.

## Artifacts and Notes

The review comments considered actionable are summarized here so this plan does not depend on an untracked notes file:

    - Root manifest hard deletes must be rejected.
    - Local stream reconciliation must be crash-atomic.
    - Root mutation keys must include canonical desired payloads.
    - Compatibility create must root-authorize content creation.
    - Generic stream writes must reject unreferenced content streams.
    - Backend stream docs should use stream GUIDs.
    - Projection paths should advance only after filesystem jobs complete.
    - Retryable filesystem jobs must not get permanently stuck.
    - Move cycles/swaps need deterministic handling.
    - Directory deletion semantics must distinguish local delete from unprojected remote mkdir.
    - Content stream updates should not broadly wake/refresh the workspace.
    - Filesystem locking must fail closed.
    - Actor type should not encode projection mode.
    - Old no-op lock-file remnants should be removed.
    - Namespace normalization should be documented.

The review comments considered lower-priority cleanup are:

    - Rename `DocumentRoom` types to stream names.
    - Remove or document `ClientIDSeed: 1001`.
    - Replace log-path notification suppression with explicit notification policy.
    - Consider a future field-level root manifest CRDT layout instead of whole-entry JSON values.

## Interfaces and Dependencies

Use the existing Go toolchain and Rust-backed `internal/ycrdt` wrapper. Host test commands should include:

    PATH="$HOME/.cargo/bin:$PATH"

because the Go CRDT package links against the local Yrs static library built by Rust tooling.

The expected new or changed interfaces include:

In `daemon/internal/syncer/root_projector.go`, define an explicit projection mode:

    type ProjectionMode string

    const (
        ProjectionPrimary ProjectionMode = "primary"
        ProjectionAgentCopy ProjectionMode = "agent-copy"
    )

    type RootManifestProjector struct {
        ...
        Mode ProjectionMode
    }

In `daemon/internal/syncer/state_stream_state.go` or a nearby state file, add transaction-scoped stream reconciliation helpers. Exact names may vary, but the behavior must exist:

    func (s *WorkspaceStateDB) ApplyStreamQueueAtomically(ctx context.Context, streamID string, kind string, doc *crdt.Doc) (stateID int64, local []StreamOutboxRow, inbox []StreamInboxRow, err error)

If the implementation uses a `WorkspaceStateTx`, keep it small and private unless tests need it:

    type WorkspaceStateTx struct {
        tx *sql.Tx
    }

In `backend/internal/notty/store_streams.go`, add stream authorization helpers:

    func streamUpdateAllowedTx(tx *sql.Tx, workspaceID string, streamID string) (StreamKind, error)

The helper should return root/content kind for allowed streams and an error for unknown unreferenced streams.

In `daemon/internal/syncer/workspace_fs.go`, production construction must return errors:

    func OpenWorkspaceFS(root string) (*WorkspaceFS, error)

Tests may use a helper such as:

    func NewWorkspaceFSTest(root string) *WorkspaceFS

or a clearly named option:

    type WorkspaceFSOptions struct {
        AllowUnlockedForTests bool
    }

Update this section if final names differ. The important interface property is not the exact spelling; it is that production daemon startup cannot silently proceed without `fslock.sqlite`.

Change note, 2026-05-23: Created this ExecPlan to convert the critical review comments into a staged, test-driven implementation plan. The plan intentionally separates merge-blocking correctness work from cleanup so future implementers can preserve momentum without losing sight of larger architectural improvements.
