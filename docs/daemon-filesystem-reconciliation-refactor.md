# Daemon Filesystem Reconciliation Refactor Proposal

## Summary

The daemon should be refactored around one first-principle model:

- `documentID` is the canonical identity.
- Paths are mutable metadata.
- Small loops observe changes and enqueue document IDs.
- The main reconciler owns all document-level decisions.
- Filesystem operations are performed last through a strict shared abstraction.

This design addresses the recurring daemon filesystem correctness issues we found: empty files on startup, dirty local edits overwritten by remote path changes, unsafe deletion, stale log files caused by atomic rename plus long-lived file handles, scattered cache/projected-base handling, and path-based logic leaking into sync decisions.

The refactor should not treat specific entities specially. Logs, markdown files, generated files, and regular files should all go through the same filesystem semantics if they live in tracked workspaces.

## Current State

The daemon currently has three relevant loops:

1. Workspace replica loops watch local files using `fsnotify`.
2. Document websocket sync loops receive remote CRDT updates and append them to the cache pending log.
3. The service reconciliation loop drains dirty document IDs every 2 seconds and reconciles documents.

This is directionally good, but filesystem policy is still distributed across several places:

- `workspaceReplica.reconcileLocalWorkspace` scans paths and directly calls backend create, move, and delete APIs.
- `workspaceReplica.removeMissingTracked` directly removes local files when backend documents disappear.
- `workspaceReplica.ensureTracked` handles backend path changes and can call `moveLocalFile`.
- `moveLocalFile` performs path movement and then writes synthesized content into the target path.
- `applyProjectedContent` writes projections directly.
- `archiveUnknownWorkingCopy` is a one-off archive helper instead of part of a shared filesystem policy.
- `agentLog` still keeps a long-lived append file descriptor, which is unsafe when projection uses atomic rename.

The result is that path/content/delete/archive behavior is decided in too many places. That is the root cause of many filesystem bugs.

## Problems This Refactor Targets

### 1. Path Used As Identity

Paths are mutable. Documents can be renamed by the backend, by the user, or by another workspace. If side loops pass paths to reconciliation, the daemon can act on stale paths.

The canonical key must be `documentID`. Path should only be resolved at the moment the main reconciler applies metadata.

### 2. Remote Rename Can Overwrite Dirty Local Content

Current remote path handling can move a local file and then write projected/base content into the target path. If the local file has unsynced edits, those edits can be lost.

Path reconciliation must not perform content reconciliation. A move should preserve bytes or fail.

### 3. Remote Delete Can Remove Dirty Local Content

If a backend document disappears, current code can remove the local file directly. If the local copy is dirty or unknown, that is data loss.

Delete must be guarded by a clean projected-base precondition. Dirty or unknown files should be archived, not deleted.

### 4. Missing Or Corrupt Projected Base Is Ambiguous

The daemon generates outgoing CRDT updates by diffing the current working file against the projected base. If the projected base is missing, corrupt, or inconsistent with its CRDT state, the daemon cannot safely compute an update.

The rule should be simple: missing or corrupt projected base means unknown local state. Unknown local state cannot be diffed. If a working file exists, archive it and reconstruct from canonical cache/backend state.

### 5. Atomic Rename And Long-Lived Writers Are Incompatible

Projection writes use atomic rename. If a Notty-owned writer keeps a long-lived file descriptor open, that descriptor can continue writing to the old unlinked inode after the path is replaced.

This caused stale-looking files, especially logs. The fix is generic: tracked workspace writes owned by Notty should use short-lived path-based open/write/close operations through the shared filesystem layer.

### 6. Incoming And Outgoing Ordering Is Currently Not First-Principle

For local workspace content, outgoing local intent is more important to capture before applying remote updates. If incoming updates advance the local cache before local dirty bytes are converted into a CRDT update, the projected-base model becomes harder to reason about.

The main reconciler should handle outgoing first, incoming second, filesystem projection last.

## Target Architecture

### Components

The daemon should have these responsibilities:

- `workspaceReplica`: local watcher and tracker only.
- `documentSync`: remote update receiver only.
- `reconcileQueue`: deduped document ID queue.
- `documentCache`: durable CRDT state, pending remote inbox, and outgoing outbox.
- `documentReconciler`: one-document-at-a-time policy owner.
- `WorkspaceFS`: shared filesystem safety abstraction, instantiated per workspace root.

### Event Flow

Local filesystem event:

1. `workspaceReplica` receives `fsnotify` event.
2. If event path maps to a tracked file, it enqueues `documentID`.
3. If event is a create/rename/delete and cannot be mapped to a known document, it goes through a separate local-create or local-discovery path.
4. It does not write backend metadata directly.
5. It does not write, move, delete, or archive files directly.

Remote websocket update:

1. `documentSync` receives a Yjs update.
2. It appends the update bytes to `pending_remote.log` for that `documentID`.
3. It enqueues `documentID`.
4. It does not apply projections.
5. It does not generate outgoing updates.

Main loop:

1. Every 2 seconds, drain document IDs from `reconcileQueue`.
2. Process one document at a time.
3. Requeue documents that still have outbox, inbox, dirty local files, or unresolved filesystem failures.

## Main Reconciliation Algorithm

The reconciler should process each document in this order:

1. Resolve current backend document metadata by `documentID`.
2. Collect all tracked workspace copies for that `documentID`.
3. Retry existing outgoing outbox first.
4. If no outbox exists, inspect local workspace copies and generate outgoing CRDT updates for every workspace copy that has valid projected base and dirty local content.
5. Persist outgoing updates to outbox before network I/O.
6. POST outbox updates to backend one at a time in deterministic order.
7. After backend accepts each update, fold that accepted update into daemon cache and clear only that accepted outbox item.
8. Apply pending remote updates from inbox to daemon cache.
9. Clear inbox only after daemon cache update succeeds.
10. Compute desired filesystem state from current backend metadata plus reconciled cache.
11. Perform filesystem reconciliation through the `WorkspaceFS` for each tracked file's workspace.
12. Requeue if any outbox, inbox, dirty local state, or retryable filesystem state remains.

If an outgoing send fails, skip incoming and filesystem projection for that document in this cycle. Keep remaining outbox and pending remote updates intact. This preserves local intent and makes retries deterministic.

Partial filesystem projection failure should not roll back successful work. If projection succeeds for one workspace and fails with `ErrDivergedWorkingCopy` for another, keep the successful workspace clean, keep already-applied inbox updates in daemon cache, mark only the diverged workspace copy dirty, and requeue the document. The next cycle is cheap and safe because outgoing, incoming, and projection phases are idempotent.

### Multi-Workspace Outgoing Updates

A document can be represented in multiple local workspaces:

- the primary daemon workspace;
- one workspace per agent.

The reconciler must preserve edits from all of them. For a dirty document, it should inspect every tracked workspace copy. For each copy with a valid projected base, an existing local file, and local content different from the projected base, the reconciler should generate a CRDT update from that workspace's projected base to its current bytes.

Those updates should all be saved to the document outbox before network I/O. Outbox records must keep actor attribution per workspace copy: agent workspace edits are attributed to that agent, and primary workspace edits are attributed to the daemon.

The reconciler should send outbox records one at a time in deterministic order. After each accepted update, it folds that update into daemon cache and clears only that accepted outbox record. This ensures one workspace's dirty edit does not hide or overwrite another workspace's dirty edit. The CRDT layer remains responsible for merging concurrent text edits.

### Partial Filesystem Failure Example

Assume three workspace copies start clean at:

```text
hello
```

The daemon receives incoming CRDT update `U1`, which changes daemon cache to:

```text
hello world
```

Filesystem reconciliation then projects this cache result to each workspace with:

```text
WriteIfUnchanged(path, expectedHash("hello\n"), "hello world\n")
```

Possible result:

- primary workspace succeeds;
- agent A workspace succeeds;
- agent B workspace fails with `ErrDivergedWorkingCopy` because the file changed locally to `hello bob\n` between inbox apply and projection.

Correct reconciler behavior:

1. Keep daemon cache at `hello world\n`.
2. Keep the incoming update cleared from inbox because it was already durably applied to daemon cache.
3. Keep primary and agent A clean with projected base advanced to `hello world\n`.
4. Do not overwrite agent B.
5. Leave agent B's projected base at the previous value.
6. Mark agent B's tracked copy dirty.
7. Requeue the document.

On the next cycle, outgoing capture sees agent B dirty against its old projected base and generates a CRDT update for Bob's local edit. This converts the raw filesystem edit into a CRDT update instead of losing it. Requeueing the whole document is acceptable because already-finished work no-ops on the next cycle.

### Pseudocode

```go
func (r *DocumentReconciler) ReconcileOne(ctx context.Context, documentID string) error {
    meta, exists, err := r.documents.GetMetadata(documentID)
    if err != nil {
        r.queue.Mark(documentID)
        return err
    }

    tracked := r.tracker.TrackedByDocumentID(documentID)

    if err := r.retryOutbox(ctx, documentID, tracked); err != nil {
        r.queue.Mark(documentID)
        return err
    }

    if exists {
        sent, err := r.captureAndSendLocalOutgoingForAllWorkspaces(ctx, meta, tracked)
        if err != nil {
            r.queue.Mark(documentID)
            return err
        }
        if sent {
            // Outgoing was folded into local cache after backend acceptance.
            // Continue to incoming and filesystem reconciliation.
        }
    }

    if err := r.applyIncoming(ctx, documentID); err != nil {
        r.queue.Mark(documentID)
        return err
    }

    if err := r.reconcileFilesystem(ctx, meta, exists, tracked); err != nil {
        r.queue.Mark(documentID)
        return err
    }

    if r.needsAnotherCycle(documentID, tracked) {
        r.queue.Mark(documentID)
    }
    return nil
}
```

## Outgoing Before Incoming

Outgoing local changes should be captured before applying pending incoming updates.

Reason:

- The working file represents local user or agent intent.
- The projected base is the exact base used to compute that intent.
- If incoming updates are applied first, the cache advances before the local intent is captured.
- That forces the daemon to reason about rebasing local filesystem bytes at the application level.

CRDT updates already solve concurrent edit merging after updates are generated. The daemon should not create extra app-level conflict states by changing the base before capturing local intent.

## Filesystem Abstraction

Add a dedicated filesystem module, for example:

```text
daemon/internal/syncer/workspace_fs.go
```

This module should own tracked workspace path mechanics. It should stay thin: it enforces immediate path preconditions, performs atomic path operations, and returns precise errors. It should not decide document policy.

The reconciler owns policy such as:

- whether a missing projected base means archive and reconstruct;
- whether a dirty file blocks remote deletion;
- whether a new local path creates a backend document;
- whether a failed filesystem operation should retry, archive, or become fatal.

`WorkspaceFS` should be instantiated per workspace root. The daemon has one primary workspace and N agent workspaces, so a single daemon-wide `WorkspaceFS{Root string}` is not sufficient unless every method also takes a root. The cleaner shape is one `WorkspaceFS` per `workspaceReplica`, stored on the replica or on tracked workspace state.

### Types

```go
type WorkspaceFS struct {
    Root string
}

type FileSnapshot struct {
    Path   string
    Exists bool
    Bytes  []byte
    Hash   projectedContentHash
}

type FSError struct {
    Op         string
    Path       string
    TargetPath string
    Err        error
}

func (e *FSError) Error() string
func (e *FSError) Unwrap() error
```

### Semantic Errors

```go
var (
    ErrDivergedWorkingCopy = errors.New("working copy diverged from expected hash")
    ErrPathCollision       = errors.New("target path already exists")
    ErrUnsafeDelete        = errors.New("refusing to delete non-matching working copy")
    ErrOutsideWorkspace    = errors.New("path is outside workspace")
)
```

These errors express path-level precondition failures. The filesystem layer should not know whether a projected base is missing, whether local state is unknown, or whether a document should be reconstructed. Those are reconciler decisions.

### Operations

```go
func (fs *WorkspaceFS) Read(path string) (FileSnapshot, error)
func (fs *WorkspaceFS) WriteIfUnchanged(path string, expected projectedContentHash, content []byte) error
func (fs *WorkspaceFS) DeleteIfUnchanged(path string, expected projectedContentHash) error
func (fs *WorkspaceFS) MoveIfNoTarget(from string, to string) error
func (fs *WorkspaceFS) Archive(path string, reason string) (string, error)
func (fs *WorkspaceFS) EnsureParent(path string) error
```

`MovePreservingBytes` and `MoveCleanProjection` should collapse into `MoveIfNoTarget`. Most move sites want the same path-level primitive: preserve the source bytes exactly and refuse target collision. Whether the source is clean, dirty, or unknown is reconciler policy, not filesystem policy.

### Locking

The current file locking uses `flock` on the data file. That is insufficient when atomic rename replaces the data inode.

The filesystem abstraction should use logical path locks:

```text
<workspace>/.notty/locks/<hash-of-relative-path>.lock
```

This protects a path across atomic rename. It also gives all Notty-owned readers and writers one coordination mechanism.

Data file locks can still be used, but they should not be the primary safety mechanism for path replacement.

### Path Lock Lifecycle

Path lock files under `.notty/locks` should be treated as operational metadata, not document state.

Recommended behavior:

- Keep lock files small and reusable.
- Do not delete a lock file while the daemon process is running, because another goroutine may be about to use the same logical path.
- On daemon startup, remove stale lock files before watchers and reconciliation begin.
- Optionally run periodic best-effort cleanup for lock files whose target path no longer exists and whose lock can be acquired non-blockingly.

This intentionally allows bounded temporary accumulation during one daemon process lifetime. It avoids unsafe deletion races while keeping disk usage negligible. Lock files are not CRDT state, not synced content, and not part of correctness recovery.

### Archive Mechanics

Archive must avoid the current copy/delete race.

Current `archiveUnknownWorkingCopy` does:

1. read bytes from source;
2. write bytes to recovered path;
3. remove source.

Concurrent writes between read and remove can be lost.

The new `WorkspaceFS.Archive` should use `os.Rename` as the primary operation:

1. choose a unique archive path under `<workspace>/.notty/recovered/...`;
2. lock the source logical path and archive logical path;
3. call `os.Rename(source, archivePath)`;
4. return archive path.

Only fall back to copy/delete on `EXDEV`, because cross-device rename is not atomic. If copy/delete fallback is used, the method should treat it as best-effort recovery and return enough context in `FSError` for logging. Normal in-workspace archive should be rename-only and atomic.

## Reconciler Error Handling

Filesystem handlers should return typed errors. The reconciler should own policy.

To keep the code clean, do not scatter `errors.Is` checks throughout the main loop. Add a small classifier and named policy helpers.

### Error Classifier

```go
type fsOutcome int

const (
    fsOK fsOutcome = iota
    fsRetry
    fsArchiveAndRetry
    fsMarkDirtyAndRetry
    fsFatal
)

func classifyFSError(err error) fsOutcome {
    switch {
    case err == nil:
        return fsOK
    case errors.Is(err, ErrDivergedWorkingCopy):
        return fsMarkDirtyAndRetry
    case errors.Is(err, ErrUnsafeDelete):
        return fsArchiveAndRetry
    case errors.Is(err, ErrPathCollision):
        return fsArchiveAndRetry
    case errors.Is(err, ErrOutsideWorkspace):
        return fsFatal
    default:
        return fsRetry
    }
}
```

### Policy Helpers

```go
func (r *DocumentReconciler) projectTrackedFile(tracked *trackedFile, content []byte, state []byte) error
func (r *DocumentReconciler) moveTrackedFile(tracked *trackedFile, nextPath string) error
func (r *DocumentReconciler) deleteTrackedFile(tracked *trackedFile) error
func (r *DocumentReconciler) archiveAndReconstruct(tracked *trackedFile, reason string) error
```

The main algorithm should call these named helpers rather than calling `WorkspaceFS` directly everywhere.

Example:

```go
func (r *DocumentReconciler) projectTrackedFile(tracked *trackedFile, content []byte, state []byte) error {
    err := r.workspaceFS(tracked).WriteIfUnchanged(tracked.Path, tracked.projectedHash(), content)
    switch classifyFSError(err) {
    case fsOK:
        if err := tracked.storeProjectedBase(string(content), state); err != nil {
            return err
        }
        tracked.clearLocalDirty()
        return nil
    case fsMarkDirtyAndRetry:
        tracked.markLocalDirty()
        r.queue.Mark(tracked.DocumentID)
        return nil
    case fsArchiveAndRetry:
        return r.archiveAndReconstruct(tracked, "projection-precondition-failed")
    case fsRetry:
        r.queue.Mark(tracked.DocumentID)
        return err
    default:
        return err
    }
}
```

This keeps policy in the reconciler while keeping the main flow readable.

## Projected Base Model

Each workspace copy should have a projected base under:

```text
<workspace>/.notty/projections/<documentID>/
```

Required files:

```text
base.txt
base.state.bin
metadata.json
```

Recommended metadata:

```json
{
  "documentId": "doc_...",
  "documentPath": "docs/spec.md",
  "contentSha256": "...",
  "stateSha256": "...",
  "updatedAt": "..."
}
```

Rules:

- `base.txt` and `base.state.bin` must agree.
- If either is missing, projected base is unknown.
- If hash validation fails, projected base is unknown.
- If CRDT state materializes to different text than `base.txt`, projected base is unknown.
- Unknown projected base means no outgoing diff can be generated.
- Existing unknown working files should be archived and reconstructed.

This avoids multiple special-case “cache corrupted” paths.

## Workspace Replica Refactor

`workspaceReplica` should be reduced to tracking and event production. Each replica should own or provide access to its own `WorkspaceFS`, because each replica has its own workspace root.

### Keep

- `projectedByID`
- `projectedByPath`
- fsnotify watcher setup
- mapping known paths to document IDs
- enqueueing dirty document IDs
- periodic discovery scan
- presence updates

### Remove Or Move

Move out of `workspaceReplica`:

- direct backend document create
- direct backend document move
- direct backend document delete
- direct local file delete
- content rewrite on path move

These decisions belong in the main reconciler.

### Local Create Exception

New untracked files do not have document IDs yet. They need a separate create queue:

```go
type localCreateQueue struct {
    paths map[string]struct{}
}
```

Flow:

1. Workspace scan sees untracked non-dot file.
2. Enqueue path into local create queue.
3. Main service reads the file bytes once through that workspace's `WorkspaceFS`.
4. Main service creates backend document with path and content.
5. Backend returns `documentID`.
6. Store projected base as exactly the current bytes that were uploaded, with `documentPath` equal to the new path.
7. Do not rewrite the working file after create.
8. Enqueue the new `documentID` for normal reconciliation.

This keeps document reconciliation document-ID based while still supporting brand-new local files.

### Projection-First Startup Rule

Startup has an unavoidable ambiguity:

```text
foo.md exists locally
foo.md has no .notty/projections/<documentID>/metadata.json
```

This could be:

- a genuinely new local file that should create a backend document;
- a stale projection whose `.notty` metadata was deleted;
- a file copied in from another workspace;
- a file from a previous corrupt daemon run.

There is no safe way to infer document identity from path alone. The first-principle rule is:

- if a local file can be matched to a known `documentID` through tracked metadata, reconcile it as that document;
- if it cannot be matched to a known `documentID`, treat it as a local-create candidate;
- after backend creation succeeds, store projected base from the exact bytes uploaded and do not rewrite the file.

This avoids hardcoded special handling. The identity source is explicit metadata; without identity, the file is new local input.

## Metadata Operations

Document metadata operations are not CRDT content updates. They still need first-principle handling.

### Local Rename

If a clean projected file moves from `oldPath` to `newPath`:

1. Detect by matching projected content/hash.
2. PATCH backend document path.
3. Update tracked metadata.
4. Let filesystem reconciliation finalize path state.

If the moved file is dirty or unknown, do not infer rename. Treat it as local dirty/unknown state and let normal reconciliation decide.

Dirty rename semantics should be explicit:

- Clean rename is a metadata move.
- Dirty rename is not inferred as a metadata move by content hash.
- Dirty rename is treated as local content intent plus untracked path discovery.

For example, if `old.md` is tracked, then a user renames it to `new.md` and edits it before reconciliation, the daemon should not guess that dirty `new.md` is the same document unless document identity metadata moved with it. Without identity metadata, the safe behavior is:

- preserve any dirty edit that can still be associated with the old `documentID`;
- treat the unmatched new path as a local-create candidate;
- never delete or overwrite bytes while trying to infer the rename.

This may behave like delete plus create for dirty renames. That is acceptable for the first refactor because it follows the identity rule. A future enhancement can preserve dirty renames by moving explicit per-file identity metadata, but this proposal should not infer identity from dirty content.

### Remote Rename

If backend metadata says document path changed:

1. Resolve tracked file by `documentID`.
2. If old path exists, use `MoveIfNoTarget` to preserve bytes exactly.
3. If the move collides with an existing target, the reconciler decides whether to archive one side and reconstruct.
4. Never synthesize content during path move.
5. Projection/reconstruction happens after the path operation, through `WriteIfUnchanged`.

### Local Delete

If a tracked file disappears locally:

1. If previous projected base is known and clean deletion is intended, DELETE backend document.
2. If state is unknown, do not delete backend.
3. If the file reappears or was moved, let scan detect it.

### Remote Delete

If backend document disappears:

1. If local file is clean, remove it.
2. If local file is dirty or unknown, archive it.
3. Remove tracking after safe local handling.

## Agent Logs

Agent logs should stop using long-lived writable file descriptors.

Current risky pattern:

```go
type agentLog struct {
    file *os.File
}
```

New pattern:

- Store only `path`.
- Each log append opens current path, locks logical path, appends, closes.

This is slightly less efficient, but correct. Logs are high-churn, but correctness matters more, and the 2-second reconciliation cadence already coalesces sync work.

## Document Cache Changes

The daemon cache should remain document-ID keyed:

```text
<cache>/<documentID>/state.bin
<cache>/<documentID>/metadata.json
<cache>/<documentID>/pending_remote.log
<cache>/<documentID>/outbox_update.json
```

Recommended cleanups:

- Normalize invalid state cleanup: if `state.bin` is missing/corrupt/hash-mismatched, clear state metadata in the same operation.
- Keep pending remote updates as append-only framed bytes.
- Keep outgoing outbox as one durable update record.
- Do not infer empty document from missing cache.
- Do not use path as cache identity.

## Tests

### Unit Tests For WorkspaceFS

Add tests for:

- `WriteIfUnchanged` writes when current file matches expected hash.
- `WriteIfUnchanged` refuses dirty file with `ErrDivergedWorkingCopy`.
- `DeleteIfUnchanged` deletes matching file.
- `DeleteIfUnchanged` refuses non-matching file with `ErrUnsafeDelete`.
- `MoveIfNoTarget` preserves exact bytes.
- `MoveIfNoTarget` refuses target collision.
- `Archive` uses `os.Rename` for same-device archive.
- `Archive` only falls back to copy/delete on `EXDEV`.
- Logical path lock works across atomic rename.

### Reconciliation Tests

Add or rewrite tests for:

- Startup with existing local file and valid projected base generates outgoing update.
- Startup with existing local file and missing projected base archives and reconstructs.
- Remote rename while local file is dirty preserves or archives dirty bytes, never overwrites them.
- Remote delete while local file is dirty archives instead of deleting.
- Local clean rename patches backend path.
- Local dirty edit plus pending incoming update sends outgoing first.
- If outgoing POST fails, pending incoming updates remain pending.
- Outbox survives daemon restart and resends same binary update.
- Multiple agent workspaces dirty the same document converge deterministically.
- High-churn append test with incrementing numbers validates final backend and workspace content.

### Tests To Remove Or Replace

Replace tests that encode incoming-before-outgoing behavior.

The old behavior is no longer the desired model. The new invariant is:

```text
local outgoing intent is captured before incoming remote updates advance the local base
```

## Migration Strategy

This is pre-MVP and compatibility is not required, so the implementation can be bold:

1. Add `WorkspaceFS` and tests first.
2. Convert agent logs to short-lived appends.
3. Replace projection/archive helpers with `WorkspaceFS` calls.
4. Refactor `workspaceReplica` to enqueue document IDs instead of directly mutating backend metadata.
5. Move metadata operations into reconciler.
6. Refactor `reconcileTrackedDocument` into phase helpers.
7. Delete obsolete helpers after call sites are gone.
8. Rewrite tests around the new invariants.

## Recommended Implementation Order

1. Filesystem abstraction and tests.
2. Agent log writer fix.
3. Projected-base integrity wrapper.
4. Reconciler policy helpers.
5. Outgoing-before-incoming reconciliation order.
6. Workspace replica simplification.
7. Metadata operation centralization.
8. Full daemon integration tests.

This order keeps the riskiest low-level behavior testable before changing the higher-level reconciliation flow.

## Expected Outcome

After the refactor:

- Filesystem correctness policy exists in one place.
- Main reconciliation is document-ID driven and deterministic.
- Local outgoing changes are captured before incoming updates.
- Incoming updates are durable and applied only by the main loop.
- Filesystem operations are safe, preconditioned, and archive on unsafe state.
- Workspace replica loops become simple observers.
- The daemon no longer has scattered file move/delete/write/archive behavior.

This should address the majority of filesystem-related issues we found and make future correctness bugs easier to isolate.
