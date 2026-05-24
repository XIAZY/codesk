# Move daemon document sync and reconciliation into per-workspace replicas

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document follows `.agent/PLANS.md` from the repository root. It is self-contained: a future contributor should be able to read only this file plus the working tree and continue the implementation.

## Purpose / Big Picture

The daemon currently has one process-wide document websocket manager and one process-wide reconciliation loop. That makes the daemon reason about all local workspace roots together. The target behavior is simpler: every local workspace root behaves like an independent CRDT client. The primary daemon workspace and each agent workspace should each receive remote document updates, observe local file changes, reconcile those changes, and persist their own durable replica state. The backend remains the merge point for concurrent edits.

After this change, a user can create, edit, and delete files in multiple local workspace roots without a shared daemon-wide document cache or a shared daemon-wide dirty queue. A regression test will prove a realistic create-edit-delete sequence across multiple files, and the daemon tests will prove that document syncs and reconciliation are owned by each workspace runtime.

## Progress

- [x] (2026-05-24T09:05:35Z) Read `.agent/PLANS.md` and created this ExecPlan.
- [x] (2026-05-24T09:15:04Z) Inventoried the daemon sync/reconcile call graph. `Service.Run` owned the process-wide ticker, `document_sync.go` owned a process-wide document sync manager, and `Service.collectTrackedByDocument` aggregated primary plus agent replicas for one document.
- [x] (2026-05-24T09:15:04Z) Introduced `workspaceRuntime` in `daemon/internal/syncer/workspace_runtime.go` with per-root document syncs, reconcile queue, local create queue, isolated cache namespace, and one filesystem replica.
- [x] (2026-05-24T09:15:04Z) Moved document websocket sync management from `Service` to `workspaceRuntime` by changing `reconcileDocumentSyncs` and `closeDocumentSyncs` receivers.
- [x] (2026-05-24T09:15:04Z) Moved dirty/local-create reconciliation loops out of `Service.Run` and into `workspaceRuntime.reconcileLoop`, with channel wakeups and a two-second minimum interval.
- [x] (2026-05-24T09:15:04Z) Kept `Service` as process supervisor for the tool gateway, sessions, agent workers, workspace event stream, and runtime lifecycle. A compatibility adapter remains for older unit tests that instantiate `Service` directly.
- [x] (2026-05-24T09:15:04Z) Added tests for queue wake coalescing, local-create wakeups, per-runtime cache isolation, and a create-edit-delete multiple-file regression.
- [x] (2026-05-24T09:15:04Z) Ran `go test ./daemon/internal/syncer`; it passed.
- [x] (2026-05-24T09:16:57Z) Ran the full Go test suite with `go test ./...`; it passed.
- [x] (2026-05-24T18:57:06Z) Added an event-driven workspace runtime regression after Docker exposed that files written before the watcher was fully active waited for the old periodic scan.
- [x] (2026-05-24T18:59:08Z) Ran `go test ./daemon/internal/syncer`; it passed.
- [x] (2026-05-24T18:59:22Z) Ran `go test ./...`; it passed.
- [x] (2026-05-24T19:00:52Z) Ran `sudo env PATH="$PATH" HOME="$HOME" go test -tags=regression ./test/regression -run TestLocalCreateEditDeleteMultipleFiles -count=1 -v`; it launched the Docker Compose stack and passed.
- [x] (2026-05-24T19:41:00Z) Corrected per-workspace state placement so `state.bin`, `metadata.json`, pending remote logs, outbox records, `base.txt`, and `base.state.bin` all live in one per-document directory under the runtime root: `<root>/.notty/documents/<doc-id>/`.
- [x] (2026-05-24T22:19:31Z) Re-ran `go test ./...` and the targeted Docker regression after removing the compatibility migration path; both passed.
- [x] (2026-05-24T22:24:15Z) Rebased onto remote `Fix daemon move path root tracking`, preserved the root-tracking fix, and reran `go test ./...` plus the targeted Docker regression; both passed.

## Surprises & Discoveries

- Observation: Local create cannot safely store a locally invented CRDT state for uploaded plain-text bytes, because the backend creates its own CRDT state for the same text. The next daemon edit may be generated against different CRDT item IDs than the server has.
  Evidence: The first create-edit-delete regression left backend content at `"alpha\n"` after editing to `"alpha edited\n"` when local create uploaded text and then generated an edit from local state.

- Observation: Removing the fixed two-second reconciliation ticker means local-create candidates need their own wake path. Filesystem discovery can enqueue a new path without a document ID.
  Evidence: `localCreateQueue.Mark` does not touch `reconcileQueue`, so a purely new file would otherwise wait forever unless some other document event woke reconciliation.

- Observation: A purely event-driven local file watcher still needs an initial full scan once the root watcher is registered.
  Evidence: The first Docker regression run created backend documents with empty content only after the old 60-second local scan, and an event-driven unit regression reproduced missed pre-watcher file creates. `workspaceReplica.Run` now calls `reconcileLocalWorkspace` immediately after registering the root watcher.

- Observation: The first per-workspace cache extraction still routed `state.bin` through `Config.CacheDir`, while projection state was rooted from the actual local workspace root.
  Evidence: `trackedFile.projectionDir` used `WorkspaceRoot`, but `newWorkspaceRuntime` accepted a separate `cacheDir` derived from `Config.CacheDir`. The corrected implementation derives both document cache and projection files from the runtime root.

## Decision Log

- Decision: Treat each local workspace root as an independent replica and let the backend be the merge point.
  Rationale: This matches the user’s explicit preference for first-principle simplicity over connection optimization. It avoids a shared daemon-wide document reconciler that must inspect every local copy of a document.
  Date/Author: 2026-05-24 / Codex

- Decision: Keep channels as wakeup signals only; durable state remains on disk behind per-document locks.
  Rationale: A channel message is lost if the process exits. Inbox, outbox, state, and projection recovery files are the source of truth.
  Date/Author: 2026-05-24 / Codex

- Decision: Implement a minimal `workspaceRuntime` extraction first, then improve crash-safe projection records if needed.
  Rationale: The current daemon already has durable pending remote logs and outboxes, but code ownership is daemon-wide. Moving ownership without changing the whole reconciliation algorithm gives a verifiable, incremental migration.
  Date/Author: 2026-05-24 / Codex

- Decision: For daemon-discovered local creates, create an empty backend document first and then let the workspace reconciler send the file bytes as the first normal CRDT update.
  Rationale: The existing create API accepts plain text and returns metadata, not canonical CRDT state. Treating the local file as the first outgoing update keeps the local workspace replica and backend merge semantics aligned without adding a new backend API.
  Date/Author: 2026-05-24 / Codex

- Decision: Keep `Service` compatibility fields and wrappers temporarily, while actual `New`, `Run`, and agent runtime lifecycle use `workspaceRuntime`.
  Rationale: Many existing unit tests instantiate `Service` with unexported fields and call document reconciliation helpers directly. The wrappers preserve test coverage while the production ownership path moves to per-workspace runtimes.
  Date/Author: 2026-05-24 / Codex

- Decision: Thread runtime actor identity through document websocket syncs.
  Rationale: Under the per-workspace model, an agent workspace should identify as that agent on its document websocket rather than always using the daemon actor identity. This keeps each local workspace behaving like an independent client.
  Date/Author: 2026-05-24 / Codex

- Decision: Keep the workspace filesystem watcher event-driven, but perform one local scan on runtime startup.
  Rationale: Channels and fsnotify events are wakeups, not durable state. A startup scan makes the local projection correct if files are created before the watcher goroutine reaches its select loop, while the per-document reconciliation work still runs through the channel-throttled workspace queue.
  Date/Author: 2026-05-24 / Codex

- Decision: Store all per-document workspace replica state together under `<runtime-root>/.notty/documents/<doc-id>/`.
  Rationale: `workspaceRuntime` owns one local root. Its durable document cache and its projection base are both workspace-specific state for the same local replica, so they should share one per-document directory rooted at that runtime root. This removes the daemon-wide cache path from document sync and avoids a split between `documents` and `projections`.
  Date/Author: 2026-05-24 / Codex

- Decision: Do not carry a compatibility migration path for the old projection directory.
  Rationale: The clean storage invariant is more valuable for this refactor. Existing users can reinstall/reset local daemon workspace state instead of preserving old `.notty` metadata across this layout change.
  Date/Author: 2026-05-24 / Codex

## Outcomes & Retrospective

Milestone outcome 2026-05-24T09:15:04Z: The daemon now has a `workspaceRuntime` owner for document syncs, local-create queues, dirty queues, and per-root cache namespaces. `Service.Run` no longer runs the process-wide document reconciliation ticker. Targeted daemon tests pass, including the new create-edit-delete regression.

Completion outcome 2026-05-24T09:16:57Z: Full Go tests pass. The implementation moves production document sync and reconciliation loop ownership into per-workspace runtimes, while preserving `Service` compatibility wrappers for existing tests and thread-anchor/tool helpers.

Docker validation outcome 2026-05-24T19:00:52Z: The targeted regression `TestLocalCreateEditDeleteMultipleFiles` passes against the real Docker Compose stack. This covers the requested create, edit, then delete sequence for multiple local files through daemon, backend, and Postgres.

State placement outcome 2026-05-24T19:41:00Z: Per-document local replica files now live together under `<runtime-root>/.notty/documents/<doc-id>/`. `documentCache` writes `state.bin`, `metadata.json`, `pending_remote.log`, and `outbox_update.json` there; `trackedFile.storeProjectedBase` writes `base.txt` and `base.state.bin` in the same directory.

## Future TODOs

- [ ] Harden workspace runtime startup for agent roots. If an agent `workspaceRuntime` exits while applying its initial workspace snapshot, the daemon should remove that runtime from `agentRuntimes` and retry on the next refresh instead of treating it as live. Add agent-workspace coverage for create, edit, and delete once this path is hardened.

## Context and Orientation

This repository is a Go and TypeScript application. The backend is under `backend/internal/notty`. The daemon, which mirrors backend documents into local files and drives agent workspaces, is under `daemon/internal/syncer`.

A daemon is a long-running local process. A workspace root is a local directory containing projected document files. There is one primary workspace root from `Config.WorkspaceDir`, and there can be one agent workspace root per backend agent under `Config.AgentWorkspaceRoot`. A CRDT update is a binary document change that can be merged safely with other updates. A replica is one independent local copy of CRDT state plus filesystem projection state.

The current daemon process-wide owner is `Service` in `daemon/internal/syncer/service.go`. It currently contains `primaryReplica`, `agentReplicas`, `documentSyncs`, `docCache`, `reconcileQueue`, and `localCreates`. `documentSync` in `daemon/internal/syncer/document_sync.go` opens one websocket for a document, appends received CRDT updates to `pending_remote.log`, and marks the document dirty. `workspaceReplica` in `daemon/internal/syncer/replica.go` watches a local filesystem root, tracks files by `documentID`, and marks documents dirty when local paths change.

The first-principle target model is that `Service` supervises process-level lifecycle, while `workspaceRuntime` owns one local workspace root and all document-level state for that root. Each `workspaceRuntime` has its own `documentCache` directory, its own `reconcileQueue`, its own `localCreateQueue`, its own `documentSyncs`, and its own `workspaceReplica`.

## Plan of Work

First, add a new file `daemon/internal/syncer/workspace_runtime.go`. Define `workspaceRuntime` with fields for `cfg`, `client`, `replica`, `docCache`, `reconcileQueue`, `localCreates`, `documentSyncs`, and optional callbacks back to `Service` for workspace refresh and agent wakeups. Give it `Run`, `reconcileDocumentSyncs`, `closeDocumentSyncs`, `markDocumentDirty`, `processLocalCreates`, `reconcileDirtyDocuments`, and `reconcileTrackedDocuments` methods. In the first implementation, these methods can reuse existing reconciliation helpers moved from `Service`, but they must operate only on the runtime’s single `workspaceReplica`.

Second, update constructors. `Service.New` should create the primary runtime instead of a primary replica plus process-wide document syncs and queues. Agent replica reconciliation should create one runtime per agent root instead of only a `workspaceReplica`. Because the current agent automation and session code still lives on `Service`, this migration keeps agent worker lifecycle on `Service`; only filesystem/document sync/reconcile behavior moves to runtimes. Existing tests that call `Service` helpers directly are supported by `daemon/internal/syncer/service_compat.go`.

Third, colocate durable document state by local workspace root. Each runtime’s document state directory is `<runtime-root>/.notty/documents`. For a document ID, the directory `<runtime-root>/.notty/documents/<doc-id>/` contains both the runtime CRDT cache files (`state.bin`, `metadata.json`, `pending_remote.log`, `outbox_update.json`) and projection base files (`base.txt`, `base.state.bin`). This prevents one local workspace from advancing another workspace’s replica state and keeps all per-document local state under the root that owns it.

Fourth, change the reconciliation loop from a process-wide ticker in `Service.Run` to a throttled per-runtime wake loop. `reconcileQueue.Mark` should wake a buffered channel. The runtime reconcile loop should run when the channel is signaled, but not more often than once every two seconds. Local-create discovery uses the sentinel document ID `__local_create__` only to wake this queue; it is not treated as a real document. A rare safety sweep can remain as a long-period timer if existing tests or recovery require it, but normal reconciliation must be channel-woken and coalesced.

Fifth, add tests. Unit tests should cover `reconcileQueue` wake coalescing and cache isolation. The requested regression should create several local files, let local create reconciliation create backend documents, edit those files, reconcile outgoing updates, delete files, and verify backend delete calls or state. If a fully networked regression is too broad for one test, add a focused daemon syncer test around `workspaceReplica.reconcileLocalWorkspace`, `processLocalCreates`, and `reconcileTrackedDocument` using `httptest`.

## Concrete Steps

Work from `/home/ubuntu/notty`.

Run current targeted tests before changes if possible:

    go test ./daemon/internal/syncer

Add and update Go files using `apply_patch`, then format:

    gofmt -w daemon/internal/syncer/*.go

Run targeted tests after each milestone:

    go test ./daemon/internal/syncer

Observed targeted result:

    ok  	notty/daemon/internal/syncer	0.978s

Run the full Go suite before final response:

    go test ./...

Observed full Go result:

    ?   	notty/backend/cmd/server	[no test files]
    ok  	notty/backend/internal/notty	(cached)
    ?   	notty/daemon/cmd/agenttool	[no test files]
    ?   	notty/daemon/cmd/daemon	[no test files]
    ok  	notty/daemon/internal/syncer	(cached)
    ok  	notty/internal/ycrdt	(cached)
    ok  	notty/internal/yproto	(cached)

Run the targeted Docker regression for the requested multi-file create-edit-delete path:

    sudo env PATH="$PATH" HOME="$HOME" go test -tags=regression ./test/regression -run TestLocalCreateEditDeleteMultipleFiles -count=1 -v

Observed Docker regression result:

    --- PASS: TestLocalCreateEditDeleteMultipleFiles (52.27s)
    PASS
    ok  	notty/test/regression	52.290s

Observed Docker regression result after colocating document cache and projection base files, with no migration fallback:

    --- PASS: TestLocalCreateEditDeleteMultipleFiles (51.72s)
    PASS
    ok  	notty/test/regression	51.735s

Expected success is `ok` lines for all packages. If a package has no test files, Go prints `[no test files]`.

## Validation and Acceptance

Acceptance requires:

1. `go test ./daemon/internal/syncer` passes.
2. `go test ./...` passes, or any failure is documented as unrelated with evidence.
3. A new regression test proves local create, edit, then delete across multiple files under a workspace root.
4. The code no longer has process-wide document sync ownership in `Service`; document sync maps live under per-workspace runtimes.
5. The code no longer has process-wide dirty reconciliation ownership in `Service`; dirty queues and reconcile wake loops live under per-workspace runtimes.
6. Each runtime uses an isolated workspace-local document state directory so one workspace root cannot mutate another root’s replica state.

## Idempotence and Recovery

The implementation should be safe to rerun. Tests use temporary directories and `httptest` servers. Cache namespace creation with `os.MkdirAll` is idempotent. Reconciliation retries should keep outbox and pending remote updates durable when network operations fail.

Do not use destructive git commands. If tests leave temporary files, Go’s `t.TempDir` cleanup should remove them. If a partial edit fails to compile, rerun `gofmt` and targeted tests after completing the next small patch.

## Artifacts and Notes

Initial evidence from source inspection:

    Service currently owns documentSyncs, docCache, reconcileQueue, and localCreates in daemon/internal/syncer/service.go.
    documentSync appends remote updates to documentCache.appendPendingRemoteUpdate and calls markDirty.
    workspaceReplica already owns per-root fsnotify watching and tracking maps.

## Interfaces and Dependencies

New or final interfaces should include:

    type workspaceRuntime struct {
        cfg Config
        client *http.Client
        mu sync.Mutex
        replica *workspaceReplica
        docCache *documentCache
        reconcileQueue *reconcileQueue
        localCreates *localCreateQueue
        documentSyncs map[string]*managedDocumentSync
    }

    func newWorkspaceRuntime(cfg Config, client *http.Client, rootDir string, actorID string, actorType string) (*workspaceRuntime, error)
    func (r *workspaceRuntime) Run(ctx context.Context) error
    func (r *workspaceRuntime) applyWorkspace(ctx context.Context, workspace *workspaceResponse) error
    func (r *workspaceRuntime) reconcileLoop(ctx context.Context)
    func (r *workspaceRuntime) markDocumentDirty(documentID string)

`reconcileQueue` should expose a wake channel:

    func (q *reconcileQueue) Wake() <-chan struct{}

`Service` should keep process-level methods such as `startToolGateway`, `fetchWorkspace`, agent automation, and session reconciliation. It may retain maps of managed workspace runtimes, but it should not own document sync maps or the dirty queue.

Revision note 2026-05-24T09:05:35Z: Created the initial self-contained ExecPlan before implementation, per `.agent/PLANS.md`, because the requested refactor changes daemon architecture and tests.

Revision note 2026-05-24T09:15:04Z: Updated progress, discoveries, decisions, and observed test evidence after implementing the per-workspace runtime extraction and regression tests. Added the local-create design correction because tests showed plain-text create plus locally invented CRDT state is not correct.

Revision note 2026-05-24T09:16:57Z: Added final validation evidence and the actor-identity decision after threading runtime actor identity through document websocket syncs and running the full Go suite.
