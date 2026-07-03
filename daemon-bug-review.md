# Daemon code review — potential bugs

Scope: `daemon/` (cmd/daemon, cmd/agenttool, internal/syncer), reviewed 2026-07-02.
Ordered by estimated severity. Line numbers refer to current `codex/email-verification` HEAD.

---

## High

### 1. Panic risk: send on closed `events` channel in codex app-server
`daemon/internal/syncer/appserver.go:100-105` closes `c.events` from the `cmd.Wait()`
goroutine, while `readLoop` (`appserver.go:305-308`) may still be draining buffered
stdout lines and does `case c.events <- …`. A send on a closed channel panics even
inside a `select` with `default` — one badly timed codex exit can crash the whole
daemon. Related: calling `cmd.Wait()` concurrently with reads from `StdoutPipe` is
documented as incorrect (`os/exec`: Wait closes the pipes), so trailing output can
also be lost/truncated. Fix: let `readLoop` own closing `events` (close after the
scanner loop exits), and only `cmd.Wait()` after both pipe loops finish.

### 2. Pending JSON-RPC requests hang forever when the codex process dies
`appserver.go:220-244` (`request`) parks the caller on a per-id channel; nothing
fails entries in `c.pending` when the process exits (`readLoop` just returns).
Callers pass long-lived contexts — e.g. `startSession` → `WriteStdin` via
`agent_sessions.go:398-441` uses the refresh/daemon context — so a codex crash
mid-request blocks `ensureSession` indefinitely. Because the agent stays in
`s.starting`, every later `ensureSession` for that agent waits on `start.done`
(`agent_sessions.go:343-355`), which wedges `Reconcile` and therefore the entire
`Service.refresh` path. Fix: on process exit, drain `c.pending` with an error.

### 3. Dropped runtime events can leave a session stuck in "working"
`appserver.go:305-308` drops notifications when the 128-slot buffer is full
(`select … default`). If a `turn/completed` or idle status event is dropped, the
supervisor never runs `markIdle` (`agent_sessions.go:704`), so `state` stays
`"working"`, queued follow-ups are never delivered, and new notification turns are
suppressed until the process restarts. Turn lifecycle events should not be dropped
(block, or grow the buffer and treat overflow as a fatal stream error).

### 4. HTTP response body leak in `sendPresence`
`daemon/internal/syncer/workspace_runtime.go:213-214`:

```go
_, err = r.client.Do(req)
return err
```

The response body is never read or closed → leaked connection/FD every 60s per
runtime (primary + one per agent). Same pattern is handled correctly everywhere
else. Also `dialWorkspaceWebsocket` callers ignore the `*http.Response` on
handshake failure (`backend_api.go:118`), leaking its body.

### 5. Two independent locking schemes on workspace files don't exclude each other
`WorkspaceFS` operations lock via the sqlite lease store (`workspace_fs.go:292`,
`path_locks.go`), but `readFileLocked`/`writeFileLocked`/`scanWorkspaceFiles`
(`file_lock.go:10-76`, used by `reconcileLocalWorkspace` and
`materializeTrackedFile` via `writeIfChanged`) use `flock`. A projection write
(`WriteIfUnchanged` → rename) can interleave with a `readFileLocked` scan or a
`writeFileLocked` with no mutual exclusion at all. Additionally `writeFileLocked`
itself is a lost-update TOCTOU: the flock is held on the inode that
`replaceFileAtomically` renames away, so a second writer blocked on the old inode
proceeds against stale content and clobbers the first write.

### 6. Data race on `workspaceRuntime.rootDocumentID` (and concurrent `refresh`)
`applyWorkspace` writes `r.rootDocumentID` (`workspace_runtime.go:222`) from:
- the `Service.Run` ticker goroutine (`service.go:269`),
- the websocket event goroutine (`service.go:374`), and
- the reconcile goroutine via `processLocalCreates` → `loadWorkspace`
  (`service.go:482-490`).

It is read without synchronization from the reconcile loop, document socket,
thread delivery and tool handlers. `Service.refresh` itself has no mutual
exclusion, so the ticker and websocket-event goroutines can run it concurrently.
This is a genuine data race (`go test -race` should confirm). Guard it with the
runtime's existing `mu`, or serialize `refresh`.

### 7. Data race on `trackedFile` identity fields
`replica.go:136-151` (`ensureTracked`) mutates `tracked.ActorID`, `ActorType`,
`FS`, `Owner`, `WorkspaceRoot`, `DocumentPath` on an already-tracked file while
the reconcile goroutine concurrently reads them (`desiredPath()`,
`actorForTracked`, `reconcileTrackedDocument`). `stateMu` only protects the dirty
flags/hash — these fields have no lock. `setTrackedPath` (`replica.go:503`) writes
`tracked.Path` under `r.mu`, but readers of `tracked.Path` don't take `r.mu`.

### 8. Stale agent workspace deleted while its runtime is still shutting down
`agent_replicas.go:71-76`: `cancel()` is asynchronous, but `os.RemoveAll` runs
immediately. The replica watcher, reconcile loop and the open sqlite `sync.db`
(WAL + journal) in that directory are still live; in-flight reconciles can
recreate `.notty/` mid-delete or write into a half-removed tree. Also
`workspaceStore` has no `Close()` — every removed agent runtime leaks its sqlite
handle permanently.

### 9. `pathLockStores` cache goes stale after workspace deletion
`path_locks.go:27-55` caches one store per root forever. After an agent workspace
is `RemoveAll`'d (see #8) and the agent later returns with the same root, the
cached store still writes leases to the *unlinked* `path_locks.db` inode, while
any other opener creates a fresh file → cross-process path locking silently stops
excluding anything. Entries also leak FDs/memory on agent churn. Invalidate the
cache when a root is removed (or verify the inode on use).

### 10. Codex workdir may not be the synced replica directory
The session supervisor computes the agent's working directory from
`filepath.Base(agent.WorkspaceRoot)` (`agent_sessions.go:747-759`), while the
syncing runtime for the same agent uses `safeAgentWorkspaceName(agentID)`
(`agent_replicas.go:38`). Whenever `WorkspaceRoot`'s basename ≠ agent ID, the
codex process reads/writes files in a directory that nothing syncs, and the
replica syncs a directory codex never touches.

---

## Medium

### 11. Turn-state race in `ScheduleNotificationTurn`
`agent_sessions.go:560-591`: between `WriteStdin` returning and the re-lock, a
fast `turn/completed` event may already have run `markIdle`. The code then
unconditionally sets `state="working"` / `activeTurn=turnID`, leaving the session
marked busy (and publishing "working" after "idle") until some later runtime
event arrives. Compare a generation/sequence counter before overwriting state.

### 12. Snapshot integrity hashes are stored but never verified
`document_cache.go`: `documentSnapshotRow.hashesValid()` (line 1228) is dead code
— loads only go through `metadataValid()` (line 1239), which just checks the hash
fields are non-empty. A corrupted `document_snapshots.state_update` or
`content_text` row is decoded and trusted, and the resulting wrong base content
will be diffed against local files (potential silent data corruption). Same for
`projectedBaseRow` (only `content_len` is checked, not `content_sha256`).

### 13. Dead check in `validateLocalNamespaceIntent`
`service.go:677-683`: the `ObservedContentHash` mismatch branch and the
fall-through both `return true, nil`. Either the hash comparison is pointless and
should be deleted, or a mismatch was supposed to be handled differently — as
written the condition has no effect.

### 14. Full-content scan of the whole workspace every 5 seconds
`replica.go:285-356` (`reconcileLocalWorkspace`) calls `scanWorkspaceFiles`
(`service.go:1762`), which reads the *entire byte content of every file* under the
root (with an flock per file) every `workspaceLocalScanInterval` (5s), per
runtime. On any non-trivial workspace this is heavy sustained I/O; move detection
(`findMovedPath`) also compares full contents. Consider size/mtime pre-filtering.

### 15. `replyThreadAsRun` builds URL without escaping the thread ID
`agent_tools.go:482`: `"/api/threads/"+threadID+"/messages"` — every sibling
endpoint uses `url.PathEscape`. An agent-supplied thread ID containing `/` or
`?` can redirect the request to a different backend path.

### 16. Path-lock leases can expire mid-operation and are acquired by spin-polling
`path_locks.go`: 30s leases are never renewed; an `Archive` that falls back to
`copyThenRemove` on a large file (or any slow FS op) can outlive its lease, after
which a competing process acquires "the same" lock. Acquisition busy-polls every
10ms for up to 5s. Also `tryAcquire` is not re-entrant for the same owner (the
`where expires_at <= ?` upsert refuses even your own live lease — fine today
because locks are scoped to one call, but a foot-gun).

### 17. `copyThenRemove` drops file permissions
`workspace_fs.go:342`: cross-device archive fallback creates the target with
hardcoded `0o644`, losing the source mode (`Archive`'s rename path preserves it).

### 18. `paceDocumentConnect` sleeps while holding a global mutex
`service.go:872-881`: all runtimes serialize through one mutex and the sleep
ignores context cancellation; with N agent runtimes reconnecting, shutdown or
connect latency degrades linearly (100ms × queue length).

### 19. CRDT `Doc` lifecycle relies on GC finalizers in several paths
`crdt.New()` results are carefully `Close()`d in most of `document_cache.go`, but
not in `buildLocalUpdateFromBase` (`service.go:1559`), `crdtStateFromContent`
(`service.go:2076`), or the placeholder doc in `materializeTrackedFile`
(`service.go:1670`, replaced at 1680 without closing). `ycrdt.Doc` is CGO-backed
(finalizer exists, `internal/ycrdt/doc.go:73`), so these won't leak forever, but
C-heap allocations exert no Go GC pressure — under heavy edit churn RSS can grow
until an eventual GC cycle. Close them explicitly like the rest of the code does.

### 20. `agentSessionStartupError` makes any one agent's startup failure fatal
`service.go:325-335`: during initial refresh, `Reconcile`'s joined errors include
`agentSessionStartupError`, which `isFatalInitializationError` treats as fatal →
`main.go:33` `log.Fatalf`. One misconfigured agent (e.g. codex present but a
single `thread/start` error) prevents the entire daemon — including plain file
sync — from starting. Verify this is intended.

---

## Low / cleanups

### 21. Inconsistent error output: `fmt.Printf` instead of `log.Printf`
`service.go:270,306,309`, `workspace_runtime.go:287-301`,
`agent_sessions.go:207,284,672` — errors go to stdout without timestamps while
everything else uses `log`.

### 22. Dead code
Unused outside tests: `driveAgentAutomation` (automation.go:95),
`projectRootProjectionEntries` (root_namespace.go:451),
`createDocumentFromLocalCandidate` (service.go:686), `loadOutboxUpdateLocked`
(document_cache.go:563), `pendingThreadIntentCountLocked` (thread_outbox.go:58),
`Pending` (agent_sessions.go:596), `agentLogPath` (agent_logs.go:99),
`hashesValid` (document_cache.go:1228, see #12).

### 23. Confusing always-true condition
`document_cache.go:1347`: `if snapshotKnown && startSeq < seq || !snapshotKnown`
is always true at that point (the `startSeq == seq` case returned earlier).

### 24. `storeOutboxUpdatesLocked` deletes all existing outbox rows first
`document_cache.go:628`. Safe under the current call pattern (callers only store
when the outbox is empty; `mutateRootDoc` re-checks), but any future caller that
skips that check silently discards unflushed local updates.

### 25. Redundant declarations / nits
- `service.go:1421`: `record.Update == nil || len(record.Update) == 0` (first
  clause redundant).
- `workspace_runtime.go:284-285`: `requeuePathChanges := false` immediately
  shadow-assigned by `requeuePathChanges, pathChangeErr := …`.
- `nullableInt` (thread_outbox.go:259) maps a legitimate HTTP status 0 vs NULL
  ambiguously (harmless today).

### 26. Tool gateway hardening
`tool_gateway.go:31-44`: `http.Server` has no read/write timeouts and
`Serve` errors are discarded; if the listener dies the daemon keeps running with
tools silently broken. Default bind is loopback, so exposure is config-dependent.

### 27. agenttool error reporting
`cmd/agenttool/main.go:289-292,316-319`: the gateway returns plain-text errors
(`http.Error`), but the CLI only surfaces a JSON `error` field, so the actual
message is dropped and users see just `400 Bad Request`.

### 28. `workspaceRelativePath` falls back to the absolute path
`service.go:1839-1845`: on `filepath.Rel` failure the *absolute* path is returned
and can end up persisted as a document path; safer to propagate the error.
