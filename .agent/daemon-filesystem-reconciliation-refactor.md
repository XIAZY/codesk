# Refactor daemon filesystem reconciliation around document identity

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. It is self-contained and incorporates the detailed design from `docs/daemon-filesystem-reconciliation-refactor.md`.

## Purpose / Big Picture

After this change, the daemon should preserve local file edits across backend updates, path changes, daemon restarts, and multiple agent workspaces. The visible behavior is that edits made in the primary workspace and agent workspaces are converted to CRDT updates before remote projections overwrite disk files; remote projections only write when the current file still matches its projected base; unsafe files are archived instead of deleted. This matters because Notty agents and users share filesystem-backed workspaces, and the daemon must not lose edits when files are renamed, deleted, projected, or updated concurrently.

## Progress

- [x] (2026-05-20T02:48:40Z) Read `.agent/PLANS.md`, current daemon code, and the filesystem reconciliation proposal.
- [x] (2026-05-20T02:48:40Z) Created this ExecPlan to guide the refactor.
- [x] (2026-05-20T02:56:20Z) Add a per-workspace `WorkspaceFS` abstraction with path locks, atomic projection, rename-first archive, and tests.
- [x] (2026-05-20T02:58:10Z) Convert agent logs to short-lived path appends so atomic rename cannot detach log writes from visible files.
- [x] (2026-05-20T03:01:20Z) Refactor document outbox storage from a single update to a deterministic list of updates, preserving actor attribution per workspace.
- [x] (2026-05-20T03:16:19Z) Refactor `workspaceReplica` so it tracks documents and enqueues document IDs/local-create candidates instead of directly mutating backend documents or deleting files.
- [x] (2026-05-20T03:16:19Z) Refactor document reconciliation into phases: retry outbox, capture all local outgoing updates, apply incoming, then reconcile filesystem.
- [x] (2026-05-20T03:16:19Z) Add correctness tests for dirty projection races, remote rename/delete, missing projected base, multi-workspace outgoing edits, and local create.
- [x] (2026-05-20T03:23:10Z) Compile, run Go tests for daemon/backend critical paths, restart the local stack, and validate behavior in a daemon container.
- [x] (2026-05-20T03:36:10Z) Review the final diff for first-principle simplicity and remove obsolete helpers.

## Surprises & Discoveries

- Observation: The current `workspaceReplica` still directly mutates backend document metadata and local files from filesystem scans.
  Evidence: `daemon/internal/syncer/replica.go` has `reconcileLocalWorkspace`, `createRemoteDocument`, `moveRemoteDocument`, `deleteRemoteDocument`, and `removeMissingTracked`.

- Observation: Current archive implementation is copy/delete, which can lose concurrent writes between read and remove.
  Evidence: `daemon/internal/syncer/service.go` `archiveUnknownWorkingCopy` reads bytes, writes recovered file, then removes source.

- Observation: Agent logs still keep a long-lived writable descriptor.
  Evidence: `daemon/internal/syncer/agent_logs.go` `agentLog` stores `file *os.File` and `Printf` writes through that descriptor.

- Observation: Focused `WorkspaceFS` tests pass after adding the abstraction.
  Evidence: `go test ./daemon/internal/syncer -run 'TestWorkspaceFS'` returned `ok notty/daemon/internal/syncer`.

- Observation: Agent log code now avoids long-lived file descriptors.
  Evidence: `go test ./daemon/internal/syncer -run 'TestWorkspaceFS|TestAgent'` returned `ok notty/daemon/internal/syncer`.

- Observation: The outbox file now supports JSON arrays while still decoding legacy single-record files.
  Evidence: `go test ./daemon/internal/syncer -run 'TestDocumentCache|TestOutgoingOutbox'` returned `ok notty/daemon/internal/syncer`.

- Observation: Projection through `WorkspaceFS.WriteIfUnchanged` must create missing files even when the target content is empty.
  Evidence: `TestReconcileTrackedDocumentArchivesLocalUpdateWithoutProjectedBase` failed until `WriteIfUnchanged` distinguished missing files from existing empty files.

- Observation: Resetting a locked `documentCacheEntry` by assigning a zero struct corrupts the embedded mutex.
  Evidence: `TestCentralReconcileRemoteDeleteArchivesDirtyWorkingCopy` triggered `fatal error: sync: unlock of unlocked mutex`; the fix resets cache metadata fields without replacing the mutex.

- Observation: Two old projection-race tests no longer exercised the production path after `WorkspaceFS` replaced the monkey-patched projection hook.
  Evidence: `writeProjectedFile` was no longer called by `applyProjectedContent`; the obsolete tests and hook were removed in favor of tests around `WriteIfUnchanged`, central delete/archive, local create, and multi-workspace outbox records.

- Observation: The rebuilt daemon container can preserve local edits across restart with the refactored reconciliation path.
  Evidence: after rebuilding `notty-daemon`, `docs/hello.md` retained the first validation append, a second `>>` append advanced backend `updateId` from `12837` to `12839`, and `.notty/projections/.../base.txt` matched the working file.

## Decision Log

- Decision: Use one `WorkspaceFS` per workspace root rather than one daemon-wide filesystem helper.
  Rationale: The daemon has a primary workspace plus one workspace per agent. Filesystem safety must be rooted in the specific workspace to reject paths outside that root and to store locks/recovered files in the right `.notty` directory.
  Date/Author: 2026-05-20 / Codex.

- Decision: Keep filesystem helpers thin and policy-free.
  Rationale: Filesystem helpers should enforce path preconditions and atomic mechanics only. The reconciler knows document identity, projected-base validity, dirty state, and whether to archive/retry/reconstruct.
  Date/Author: 2026-05-20 / Codex.

- Decision: Capture outgoing local changes before applying incoming remote updates.
  Rationale: Raw filesystem bytes are not CRDT-protected until converted to a CRDT update. Capturing local intent first prevents remote projection from advancing the base underneath a local edit.
  Date/Author: 2026-05-20 / Codex.

- Decision: Requeue whole documents after partial filesystem projection failures, but keep successful workspace projections committed.
  Rationale: Reconciliation phases are idempotent. Requeueing the document is simple and safe, while rolling back successful projections would introduce more risk.
  Date/Author: 2026-05-20 / Codex.

## Outcomes & Retrospective

Tests pass for `go test ./daemon/...` and `go test ./backend/internal/notty` with `GOCACHE=/private/tmp/notty-go-cache`. Local Docker validation created a workspace-scoped daemon token, started a daemon container, projected `docs/hello.md`, appended to the local daemon workspace file, observed the backend `updateId` advance, rebuilt and restarted the daemon image, appended again, and verified the workspace projected base matched the appended file content.

## Context and Orientation

The daemon code lives under `daemon/internal/syncer`. The `Service` in `service.go` owns daemon-wide state, starts workspace replicas, starts document websocket sync loops, and drains a `reconcileQueue` every 2 seconds. A `workspaceReplica` in `replica.go` watches a single local workspace root with `fsnotify`; there is one primary daemon workspace and one workspace per agent. A `documentSync` in `document_sync.go` receives remote Yjs updates over a websocket and appends them to the daemon disk cache. The `documentCache` in `document_cache.go` stores one directory per `documentID`, including `state.bin`, `pending_remote.log`, and `outbox_update.json`.

Important terms: a document ID is the stable backend identifier for a document. A path is the mutable file path where that document is projected into a workspace. A projected base is the last content and CRDT state the daemon believes it wrote to a workspace file; it is stored under `<workspace>/.notty/projections/<documentID>/`. A dirty workspace file is a file whose current bytes differ from the projected base. An inbox is `pending_remote.log`, which holds remote CRDT updates received from the backend. An outbox is durable local CRDT updates that must be sent to the backend before they can be cleared.

## Plan of Work

First, add `daemon/internal/syncer/workspace_fs.go`. It should define `WorkspaceFS`, typed filesystem errors, path validation, logical path lock files under `<workspace>/.notty/locks`, `Read`, `WriteIfUnchanged`, `DeleteIfUnchanged`, `MoveIfNoTarget`, `Archive`, and `EnsureParent`. Archive should use `os.Rename` first and only fall back to copy/delete on `EXDEV`. Lock files should not be deleted during normal operations; stale locks can be removed at startup before watchers begin.

Second, update `agent_logs.go` to stop storing a long-lived `*os.File`. `agentLog` should store only the path and append by opening, locking, appending, and closing the current path. This is a generic tracked-workspace write rule, not a log-specific exclusion.

Third, refactor `document_cache.go` outbox functions. The outbox should support multiple records because a single document can have dirty local edits in several workspaces. Use a deterministic JSON array file or a framed log; JSON array is acceptable for this pre-MVP because outbox sizes are expected to be small and it is easier to inspect. Provide functions to load all outbox records, store all records, append generated records, and drop only the accepted first record. Preserve compatibility in tests by updating helper call sites.

Fourth, change `workspaceReplica`. It should maintain tracking maps and enqueue document IDs, but no longer directly create, move, delete backend documents, or delete/move/write local files as part of path scans. For known tracked paths, local writes enqueue the document ID. For create/rename/remove scans, the replica records local-create candidates and marks affected document IDs dirty. Local-create candidates are paths without known document identity.

Fifth, refactor `Service.reconcileTrackedDocument` into smaller phase helpers. `retryOutbox` sends existing outbox records one at a time and clears only accepted records. `captureAndSendLocalOutgoingForAllWorkspaces` scans all tracked copies with valid projected base and dirty content, generates CRDT updates, stores them to outbox, then sends them. `applyIncoming` applies pending remote updates into the daemon cache after outgoing work succeeds or no outgoing work exists. `reconcileFilesystem` writes projections, moves paths, deletes clean removed files, archives unsafe files, and stores projected bases through `WorkspaceFS`.

Sixth, update tests. Remove or rewrite tests that assume incoming remote updates are applied before local outgoing updates. Add focused tests for filesystem primitives and high-risk reconciliation behavior.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run focused tests frequently:

    go test ./daemon/internal/syncer

Then run backend tests touched by API behavior:

    go test ./backend/internal/notty

For local container validation, use the existing docker compose workflow after compilation succeeds:

    docker compose up -d --build backend daemon
    docker compose ps

If containers are already running, restart only what is needed.

## Validation and Acceptance

The change is accepted when:

1. `go test ./daemon/internal/syncer` passes.
2. `go test ./backend/internal/notty` passes or unchanged backend tests remain passing.
3. A local daemon container can start and stay running.
4. Tests demonstrate that a projection race keeps successful work committed, marks only the diverged workspace dirty, and later captures the diverged local bytes as an outgoing CRDT update.
5. Tests demonstrate that remote delete archives dirty/unknown local files rather than deleting them.
6. Tests demonstrate that multi-workspace dirty edits generate multiple outbox records and preserve actor attribution.
7. Tests demonstrate that local create stores projected base from uploaded bytes without rewriting the working file.

## Idempotence and Recovery

The refactor should be safe to rerun. Generated `.notty` test directories live in temporary test directories. Runtime containers may be restarted with `docker compose up -d --build`. If a filesystem operation fails, the reconciler should requeue the document rather than deleting data. If projected base is invalid, the working copy should be archived and reconstructed rather than diffed.

## Artifacts and Notes

The detailed design document is `docs/daemon-filesystem-reconciliation-refactor.md`. This ExecPlan is the implementation guide and should be kept updated as code changes proceed.

## Interfaces and Dependencies

In `daemon/internal/syncer/workspace_fs.go`, define:

    type WorkspaceFS struct {
        Root string
    }

    type FileSnapshot struct {
        Path string
        Exists bool
        Bytes []byte
        Hash projectedContentHash
    }

    var ErrDivergedWorkingCopy error
    var ErrPathCollision error
    var ErrUnsafeDelete error
    var ErrOutsideWorkspace error

    func NewWorkspaceFS(root string) *WorkspaceFS
    func (fs *WorkspaceFS) CleanupStaleLocks() error
    func (fs *WorkspaceFS) Read(path string) (FileSnapshot, error)
    func (fs *WorkspaceFS) WriteIfUnchanged(path string, expected projectedContentHash, content []byte) error
    func (fs *WorkspaceFS) DeleteIfUnchanged(path string, expected projectedContentHash) error
    func (fs *WorkspaceFS) MoveIfNoTarget(from string, to string) error
    func (fs *WorkspaceFS) Archive(path string, reason string) (string, error)
    func (fs *WorkspaceFS) EnsureParent(path string) error

Document cache outbox functions should support multiple `outboxUpdateRecord` values. The final names can change during implementation, but the API must support loading all records, appending records, and removing only accepted records.

Revision note, 2026-05-20 / Codex: Initial ExecPlan created before code changes to guide the daemon filesystem reconciliation refactor.
