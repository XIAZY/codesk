# Make Windows daemon core lifetimes atomic and runtime-owned

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`. It is a follow-up to the checked-in `.agent/windows-runtime-lifecycle.md`, but it restates all context needed for this correction. The earlier plan removed process-global SQLite stores and moved watcher construction into `Run`; local diagnostics subsequently proved that those changes close Windows handles but leave three deeper ownership defects.

## Purpose / Big Picture

After this change the daemon becomes ready only after its primary workspace watcher is actively draining events and its initial watch/reconcile setup succeeds. A fatal primary watcher or refresh failure stops the daemon and is returned to its caller, while an agent process remains supervised independently and cannot make core infrastructure fail. On shutdown, network and filesystem ingress stop first, every cache user is joined, and only then are the document database and shared path-lock database closed. Each workspace runtime opens the path-lock database and initializes its schema exactly once rather than once per filesystem operation.

The change is demonstrated with tests written before the behavioral implementation. Each defect must fail on commit `3389d1b` for the expected reason. A defect that cannot be made to fail deterministically will not be “fixed” speculatively. Final tests use injected watcher and store implementations where practical so Linux CI can exercise the Windows ordering contract without relying on timing, while native Windows integration tests retain coverage of the real backend.

## Progress

- [x] (2026-07-13 13:28 EDT) Re-read `.agent/PLANS.md` and the prior Windows lifecycle ExecPlan; verified the branch is `fix/windows-runtime-lifecycle` at `3389d1b` with only pre-existing untracked `agents/` and `workspaces/` directories.
- [x] (2026-07-13 13:28 EDT) Reconfirmed the source ownership graph for `workspaceReplica`, `workspaceRuntime`, `Service`, the tool gateway, agent runtimes, agent workers, and the agent session supervisor.
- [x] (2026-07-13 13:39 EDT) Added behavior-preserving watcher/store test seams, current-boundary readiness observation, and seven permanent lifecycle regressions without changing acquisition or teardown order.
- [x] (2026-07-13 13:39 EDT) Captured the native Windows ARM64 RED run on `3389d1b`: all seven tests failed for their named lifecycle defects, including the blocked tool request returning HTTP 400 with `sql: database is closed`.
- [x] (2026-07-13 13:48 EDT) Implemented an always-live watcher pump, logically unbounded ordered work queue, independent serialized handler, startup barrier, and replica-to-runtime fatal propagation; all three focused watcher/readiness tests pass natively on Windows ARM64.
- [x] (2026-07-13 13:52 EDT) Moved one path-lock store into each workspace runtime, routed runtime filesystem operations and materialization through the shared `WorkspaceFS`, made close idempotent with document-cache-before-path-store ordering, and covered both normal and constructor-error close counts.
- [x] (2026-07-13 14:02 EDT) Replaced ad hoc service shutdown branches with one idempotent teardown owner, split gateway ingress close from request drain, joined the primary runtime and workspace event loop, and delayed primary cache closure until all core and agent users quiesce.
- [x] (2026-07-13 14:02 EDT) Made inbox workers, runtime-event consumers, delayed restarts, and status workers cancelable and joinable; proved a terminal agent exit remains nonfatal while a primary watcher failure terminates the ready service.
- [x] (2026-07-13 14:14 EDT) Ran the focused lifecycle set, full native ARM64 syncer package, Windows/amd64 race suite with all 28 required rows, full Go test/vet/build, Yjs canary, frontend typecheck/Vitest/build, uninstall harness, installer syntax, and direct static Windows/amd64 release-style links.
- [x] (2026-07-13 14:24 EDT) Confirmed `origin/feat/windows-cli-daemon` remains at `4639f09` with no base-only commits, pushed the complete fix series through `ec62e6a`, and updated draft PR #149 without staging the pre-existing `agents/` or `workspaces/` directories.

## Surprises & Discoveries

- Observation: Moving `fsnotify.NewWatcher` into `workspaceReplica.Run` did not establish consumer-before-producer ordering.
  Evidence: `Run` still calls root `Add`, recursively adds directories, and performs initial reconciliation before its first receive from `watcher.Events` or `watcher.Errors`. A local Windows diagnostic blocked the recursive `Add` in three of three attempts until event draining began.

- Observation: A dedicated event pump must remain independent from the event handler, not merely start the existing select earlier.
  Evidence: Windows fsnotify uses one backend goroutine to send every event and acknowledge `Add`. If a handler receives one event and synchronously calls `Add` while the backend is trying to send a second event from the same OS batch, the same cycle can recur. The pump therefore needs a non-blocking, logically unbounded in-process queue feeding a separate serialized handler.

- Observation: Context cancellation alone does not stop the established workspace event WebSocket.
  Evidence: `runWorkspaceEventStream` blocks in Gorilla WebSocket `ReadMessage`; a local test remained blocked after cancellation and returned only after the peer socket closed. `Service.Run` does not currently await that goroutine.

- Observation: The operation-scoped path-lock change solved Windows handle cleanup by paying connection and schema initialization cost on every filesystem call.
  Evidence: Five identical one-second `WorkspaceFS.Read` samples on this host measured base `4639f09` at 0.189–0.208 ms and 38 allocations, versus `3389d1b` at 4.38–4.88 ms and 136 allocations. The median slowdown was 24.2 times on the same Windows/amd64 process.

- Observation: Shutdown ordering is an observable semantic race even when Go's race detector does not report a memory race.
  Evidence: Holding the document store's sole connection, starting the same cache query used by the list-documents tool, and closing the runtime caused the query to return `sql: database is closed`.

- Observation: `Service` currently has two agent-owned trees in addition to core infrastructure.
  Evidence: resident external agent processes and their event consumers live in `agentSessionSupervisor`, while inbox automation polling goroutines live in `Service.agentWorkers`. Both must be cancelled and joined, but their normal, terminal, or restartable exits must not be sent to the core fatal-error path.

- Observation: Native ARM64 test execution is available, but the repository staging script cannot currently refresh that archive under the inherited PowerShell/MSYS path.
  Evidence: the cached `aarch64-pc-windows-gnullvm/release/libyrs.a` links and the focused Go test passes natively in 1.252 seconds. Running `scripts/build-yffi.sh` tried to use MSYS `/usr/bin/link.exe` for Rust host build scripts and failed. Atomically staging the already-built target archive works; amd64 will be restaged before race/release validation.

- Observation: An ordered barrier has to cross both watcher stages, not just prove that the backend channel has a receiver.
  Evidence: `Add` may unblock as soon as the pump receives an event, before the independent handler has processed it. The pump therefore enqueues a barrier into the same FIFO after setup; readiness waits for the handler to close that barrier after all preceding startup events.

- Observation: Keeping an operation-scoped fallback for standalone `WorkspaceFS` values avoids introducing unowned database handles outside a runtime.
  Evidence: Production runtime paths now receive the retained store explicitly, including materialization and local-create checks. Existing focused filesystem tests construct standalone values without a lifetime owner; their fallback still opens and closes around one operation, while the runtime ownership regression proves the retained path opens once and closes once.

- Observation: Correct replica-level fatal propagation was insufficient because `Service.Run` still logged and discarded the primary runtime result.
  Evidence: The added service-boundary regression remained online for 500 ms after an injected primary watcher error and returned nil only after test cancellation. The service now awaits primary readiness and selects its completion as a core-fatal result.

- Observation: Stopping agent processes without owning delayed restart work lets the supervisor resurrect a process after service shutdown.
  Evidence: The new ownership-split regression observed a second fake process 2 seconds after shutdown. Delayed restart now remains in the tracked event-consumer task and selects the supervisor context; the same test observes one process while the service itself remains ready after the original agent exit.

- Observation: Gorilla WebSocket reads become promptly joinable when cancellation owns the connection, not merely the dialing context.
  Evidence: Registering a `context.AfterFunc` that closes the established workspace-event connection makes the cancellation regression return immediately without peer cleanup.

- Observation: The repository-wide formatting probe is not meaningful on this checkout without normalizing line endings.
  Evidence: `core.autocrlf` presents most untouched tracked Go files as CRLF, so native `gofmt -l` lists 145 baseline files while none of this change's Go files are listed. The changed-file formatting check and `git diff --check` pass; no unrelated line-ending rewrite was made.

- Observation: Two documented gates are unavailable for infrastructure reasons, not source failures.
  Evidence: all 13 tagged regression tests stop at setup with `exec: "docker": executable file not found in %PATH%`, which also prevents live-Postgres and browser-compose E2E. The installer functional harness rejects `MSYS_NT-10.0-26200-ARM64` because it intentionally accepts only Darwin/Linux; both installer scripts pass `sh -n`, and the platform-independent uninstall harness passes.

- Observation: The canonical release wrapper cannot rebuild Rust host build scripts on this ARM64 Windows installation even though the actual release-style link is valid.
  Evidence: ARM64-MSVC Rust resolves MSYS `/usr/bin/link.exe` and reports its Unix `link` operand error; Visual Studio `link.exe` is absent. Using the retained, previously built x86_64 GNU Yrs archive, the wrapper's exact static Go linker flags produced valid AMD64 PE daemon and agent-tool binaries.

- Observation: Agent workspace runtimes also have tool-handler cache consumers, so invoking their self-closing public `Run` under the service context was still too early.
  Evidence: A final owner-boundary regression cancelled a managed agent runtime and then probed its path-lock store before registry teardown; it failed RED with `sql: database is closed`. Managed runtimes now call the non-owning run helper, while registry removal or global teardown performs cancel, join, and close after gateway drain. The regression and affected race set pass.

## Decision Log

- Decision: Enforce a real red/green gate before changing ownership behavior.
  Rationale: The proposed fixes are broad enough to hide unrelated cleanup behind an architectural rewrite. Tests must first prove the watcher cycle, partial readiness, gateway leak, cache-close ordering, and per-operation store creation on the unmodified behavior. Small interfaces or factories may be added solely to observe existing behavior, but they must not change acquisition, readiness, or teardown order before the RED transcript is recorded.
  Date/Author: 2026-07-13 / Codex

- Decision: Supersede the prior “stateless WorkspaceFS with operation-scoped store” decision.
  Rationale: Measurements show that ownership choice is materially too expensive, and the runtime already provides the exact lifetime boundary needed to close Windows handles safely. A workspace runtime will own one path-lock store and pass a reference to its `WorkspaceFS`; standalone tests will explicitly own and close their stores.
  Date/Author: 2026-07-13 / Codex

- Decision: Use a dedicated watcher pump and a serialized handler joined by the replica owner.
  Rationale: The pump must always be able to receive from fsnotify while setup or event handling performs synchronous `Add` calls. An unbounded mutex-protected queue with a wake channel avoids replacing the deadlock with a finite-buffer limit.
  Date/Author: 2026-07-13 / Codex

- Decision: Core failures are fatal; agent exits remain supervised and nonfatal.
  Rationale: Losing the primary watcher, gateway, or workspace refresh means the daemon cannot safely claim to be online. An individual agent process is expected to stop, fail terminally, or restart transiently without taking the workspace daemon down.
  Date/Author: 2026-07-13 / Codex

- Decision: Teardown follows reverse dependency order and uses the same path for every return.
  Rationale: Gateway handlers and refresh code use runtime caches. Shutdown must cancel, close watcher/WebSocket/gateway ingress, join core handlers and both agent trees, then close the document cache and finally the shared path-lock store. Installing the teardown immediately after the first acquisition also covers early setup errors.
  Date/Author: 2026-07-13 / Codex

## Outcomes & Retrospective

The implementation, locally representable validation, and publication are complete. Tests first proved every claimed defect on the old behavior. The final implementation owns watcher delivery before `Add`, propagates primary core failures without making agent exits fatal, shares one path-lock store per runtime, and tears down ingress/users/stores in dependency order. All native Windows, full Go, frontend, Yjs, and direct static-link gates pass. Docker/Postgres/browser E2E, the Unix-only installer functional harness, and a fresh Rust archive rebuild remain external host limitations recorded above. Draft PR #149 targets the requested `feat/windows-cli-daemon` branch; its native Windows job remains intentionally disabled while the replacement cross-platform CI run proceeds.

## Context and Orientation

All production changes are in `daemon/internal/syncer`.

`workspaceReplica` in `replica.go` watches one workspace directory. An fsnotify watcher exposes filesystem events and errors and supports synchronous directory `Add` calls. On Windows, the dependency's backend has one goroutine that both sends unbuffered events and acknowledges `Add`, so the daemon must continuously drain the event channels whenever any goroutine can call `Add`.

`workspaceRuntime` in `workspace_runtime.go` owns one `workspaceReplica`, a `documentCache` backed by `.notty/sync.db`, reconciliation and presence loops, and a document WebSocket. Its current `Run` starts the replica asynchronously, logs replica startup errors, waits only for context cancellation, and closes the document cache when it exits. There is no startup readiness result for `Service` to await.

`Service` in `service.go` owns the primary workspace runtime, dynamically managed agent workspace runtimes, the backend workspace-event WebSocket, inbox automation workers, the agent session supervisor, and the local HTTP tool gateway from `tool_gateway.go`. The gateway binds before workspace directories are created. Current early `MkdirAll` returns leak that listener. Current normal shutdown closes runtimes before draining the gateway and never joins `workspaceEventLoop`.

`WorkspaceFS` in `workspace_fs.go` wraps file reads, writes, appends, moves, deletes, and archives. It obtains cross-process leases from `pathLockStore` in `path_locks.go`, backed by `.notty/path_locks.db`. At `3389d1b`, `CleanupStaleLocks` and every `lockPaths` call invoke `newPathLockStore`, which opens SQLite, initializes WAL/busy-timeout/schema/index state, performs one operation, and closes.

The “core lifetime” in this plan means only infrastructure whose loss makes the daemon unsafe to advertise as online: the primary watcher pump and handler, primary runtime loops, workspace refresh/event stream, and tool gateway serve/drain path. “Agent trees” means external agent processes and inbox automation loops. Agent trees are cancelled and awaited on shutdown but do not return errors through the core fatal channel.

## Plan of Work

The first milestone adds tests without repairing behavior. Introduce the smallest behavior-preserving interfaces needed for deterministic observation: a watcher interface/factory around fsnotify and a path-lock store interface/opener used exactly where the current concrete calls occur. Add a test readiness observation point without moving the current readiness boundary. Then add regressions for an `Add` implementation whose unbuffered event send must be drained, an injected `Add` error, a post-readiness watcher error, a leaked gateway after injected directory creation failure, a blocked tool cache query during shutdown, a blocked workspace-event connection after cancellation, and repeated filesystem operations through a counting store opener. Run only these tests and retain output showing each fails for its named defect. If any test unexpectedly passes or fails for an unrelated harness problem, correct the test before implementation; do not alter production ordering to force RED.

The second milestone replaces the replica's startup sequence. Wrap the real fsnotify watcher in the injected interface. Start a pump that does nothing except receive events/errors and append events to an unbounded internal queue. Start one handler that serializes startup (`MkdirAll`, stale-lock cleanup, root and recursive Adds, initial reconcile), queued filesystem event handling, and periodic reconciliation. Await a startup result before signaling readiness. Any startup, pump, handler, or post-readiness watcher error closes watcher ingress, cancels and joins both goroutines, and is returned. Expected context cancellation returns nil after the same join path.

The third milestone makes a workspace runtime own one path-lock store. Open it once in `newWorkspaceRuntime`, close it if any later constructor step fails, and pass it into the runtime's single `WorkspaceFS`. Replace every runtime-local `NewWorkspaceFS` call with that shared instance, including tracked-file materialization and local-create validation. `WorkspaceFS.lockPaths` acquires and releases leases without opening or closing the store. `CleanupStaleLocks` uses the same store. Runtime close remains idempotent and closes `documentCache` before the path-lock store, after all internal users have joined. Standalone filesystem tests use a helper that opens one store and registers cleanup.

The fourth milestone gives `Service.Run` one explicit core owner. The gateway object separates listener acquisition, serving, graceful ingress shutdown, and serve completion. The workspace event stream arranges to close its established WebSocket when the core context is cancelled. Primary runtime readiness is awaited before online status is published. Unexpected gateway serve exit, terminal workspace-event failure, periodic fatal refresh failure, or primary runtime failure records the first core error and cancels the group. One deferred teardown is installed immediately after gateway acquisition and performs cancellation, watcher/WebSocket/gateway ingress closure, gateway request drain, core join, inbox-worker and session-supervisor cancellation/join, agent-runtime join, primary-runtime join, document-cache close, then path-lock close. The teardown runs for normal cancellation and all setup failures.

The fifth milestone makes both agent trees explicitly joinable without adding their errors to the core group. Add completion channels to inbox workers. Give `agentSessionSupervisor` an owned cancellation context and wait group for event consumers and delayed restarts; replace `context.Background` restart work with the supervisor context. Shutdown stops accepting restarts, stops processes, joins event consumers/restarts and status workers, and remains idempotent. Tests prove an agent process event stream closing changes only that agent's status/restart state while the service core remains ready.

Finally run focused tests, the complete syncer suite, the native Windows required-row audit, and repository-wide Go validation. Keep the benchmark as evidence but never use latency as a pass/fail threshold. Inspect all diffs and stage only intended files.

## Concrete Steps

All commands run from `C:\Users\zhong\notty` in PowerShell.

1. Confirm the native ARM64 test toolchain. Prefer the host-native target for non-race tests if the installed Go command can build it with MSYS2 clangarm64:

       $env:GOOS = 'windows'
       $env:GOARCH = 'arm64'
       $env:CGO_ENABLED = '1'
       $env:CC = 'C:\msys64\clangarm64\bin\clang.exe'
       go test -run '^TestWorkspaceFSRejectsOutsideWorkspace$' ./daemon/internal/syncer

   If that target cannot link, use the known Windows/amd64 toolchain under emulation:

       $env:GOARCH = 'amd64'
       $env:CC = 'C:\msys64\mingw64\bin\gcc.exe'

2. After adding only test seams and tests, run the named regression set with `-count=1 -v`. Expect failures that explicitly report undrained watcher startup, swallowed startup errors, a live gateway after setup failure, cache closure before blocked users finish, an unjoined event loop, and a store open count greater than one. Save this transcript in `Artifacts and Notes` before touching behavioral implementation.

3. Implement one milestone at a time. After each change, run the smallest named tests, format changed Go files with `gofmt -w`, and update `Progress`, `Surprises & Discoveries`, and `Decision Log`.

4. Run race validation with the supported Windows/amd64 race toolchain:

       $env:GOARCH = 'amd64'
       $env:CGO_ENABLED = '1'
       $env:CC = 'C:\msys64\mingw64\bin\gcc.exe'
       go test -race -count=1 -v -timeout=10m ./daemon/internal/syncer

5. Run repository validation:

       gofmt -l <all tracked Go files outside third_party>
       go vet ./...
       go test ./...
       go build ./...
       git diff --check

   Re-run the workflow's 28 required Windows rows and other local gates documented in `.agent/windows-runtime-lifecycle.md` when the focused work is green.

## Validation and Acceptance

Acceptance requires permanent tests with these observable behaviors:

The injected watcher test reaches readiness even though every `Add` performs an unbuffered event send before returning. An injected startup `Add` error makes readiness fail, returns the same error, closes every acquired resource exactly once, and never reports online. After readiness, an injected watcher error makes the primary runtime and `Service.Run` return that error. The equivalent agent-supervised exit changes the agent state without ending the ready core service.

During shutdown, a blocked authenticated tool request and a blocked event-triggered refresh retain usable document and path-lock stores. `Service.Run` remains blocked until those users are released. After release, requests and core handlers finish, agent trees join, the document cache closes once, the path-lock store closes once and last, and `Run` returns. A lifecycle recorder asserts the exact dependency sequence. Cancelling an established workspace event stream closes its socket and joins its loop.

When setup fails after the gateway binds, the gateway no longer accepts connections and its serve goroutine is joined. When startup fails after path-lock acquisition, prior users unwind and the store closes exactly once.

Many sequential and concurrent operations through one runtime report one store open, one successful schema initialization, zero closes while operations are active, and one close after runtime quiescence. The read benchmark remains near the retained-store baseline but has no timing assertion.

The complete syncer package passes under `-race` on Windows/amd64, all 28 workflow-required native rows pass, and full Go test/vet/build and formatting checks pass. No Windows temporary-directory cleanup reports an open SQLite handle.

## Idempotence and Recovery

All tests and formatting commands are safe to rerun. Every close/stop path introduced here must be idempotent because setup errors, fatal core errors, and caller cancellation can converge. Tests must use bounded waits and explicitly release fakes in cleanup so an intentionally RED run does not leave a test process hanging.

If native ARM64 linking fails, clear `GOARCH`, `CC`, and related environment variables or restore the documented amd64 values before continuing. Do not install or modify system toolchains merely to satisfy the optional resource-saving path.

Never remove, edit, or stage the pre-existing untracked `agents/` and `workspaces/` directories. Do not reset unrelated user changes. Before each commit, inspect `git status --short`, `git diff --check`, and the staged diff.

## Artifacts and Notes

Starting branch and commit:

    fix/windows-runtime-lifecycle
    3389d1bbba3439a61cfcb252ce61184435e97e57

Pre-plan local diagnostic evidence, produced with temporary tests that were removed afterward:

    recursive Add remained blocked while fsnotify.Events was undrained
    runtime stayed online after its replica startup failed
    gateway remained bound after Run returned: bind: Only one usage of each socket address ...
    in-flight tool cache query failed after runtime Close: sql: database is closed
    workspace event loop remained blocked in ReadMessage after context cancellation

Path-lock benchmark evidence:

    base 4639f09: 189127–208038 ns/op, 2672 B/op, 38 allocs/op
    head 3389d1b:  4379161–4880283 ns/op, 8168 B/op, 136 allocs/op

The permanent RED transcript and final GREEN transcript will be appended here verbatim in abbreviated form.

Permanent RED transcript on Windows/ARM64, `3389d1b` plus observation-only seams:

    TestWorkspaceReplicaStartupDrainsEventsBeforeFirstAdd
      replica called Add before any watcher event consumer was active
    TestWorkspaceRuntimeStartupPropagatesReplicaAddFailure
      runtime published ready instead of returning replica startup failure: <nil>
    TestWorkspaceReplicaWatcherErrorAfterReadyIsFatal
      ready replica logged and ignored a fatal watcher error
    TestWorkspaceFSReusesOnePathLockStoreAcrossOperations
      path-lock store open/schema-init count = 13, want 1 per runtime
    TestServiceEarlyWorkspaceSetupFailureClosesGateway
      tool gateway still accepted connections after setup failed
    TestWorkspaceEventLoopStopsWhenContextIsCanceled
      workspace event loop remained blocked in ReadMessage after context cancellation
    TestServiceShutdownDrainsToolRequestBeforeClosingRuntimeCache
      status=400 body="sql: database is closed\n"

    FAIL notty/daemon/internal/syncer 1.998s

Focused watcher GREEN transcript on Windows/ARM64:

    go test ./daemon/internal/syncer -run \
      'TestWorkspaceReplicaStartupDrainsEventsBeforeFirstAdd|TestWorkspaceRuntimeStartupPropagatesReplicaAddFailure|TestWorkspaceReplicaWatcherErrorAfterReadyIsFatal' -count=1
    ok notty/daemon/internal/syncer 1.270s

Focused retained-store GREEN transcript on Windows/ARM64:

    go test ./daemon/internal/syncer -run \
      'TestWorkspaceFSReusesOnePathLockStoreAcrossOperations|TestWorkspaceRuntimeConstructorFailureClosesAcquiredPathLocks' -count=1
    ok notty/daemon/internal/syncer 1.186s

    go test ./daemon/internal/syncer -run \
      'TestWorkspaceFS|TestMaterialize|TestWorkspaceRuntime' -count=1
    ok notty/daemon/internal/syncer 1.227s

Combined lifecycle GREEN transcript on Windows/ARM64:

    12 lifecycle tests PASS
    ok notty/daemon/internal/syncer 3.217s

Complete syncer validation:

    Windows/ARM64: go test ./daemon/internal/syncer -count=1 -timeout=10m
    ok notty/daemon/internal/syncer 17.073s

    Windows/amd64: go test -race ./daemon/internal/syncer -count=1 -timeout=10m
    ok notty/daemon/internal/syncer 27.662s

Exact Windows required-row audit:

    test_exit=0 required=28 passed=28 missing=0
    ok notty/daemon/internal/syncer 26.980s

Repository gates:

    go test ./... -count=1 -timeout=10m          PASS
    go vet ./...                                 PASS
    go build ./...                               PASS
    Yjs cross-compat canary                      PASS (not skipped)
    frontend typecheck + Vitest                  PASS (21 files, 250 tests)
    frontend production build                    PASS
    deploy installer/uninstaller sh -n           PASS
    daemon uninstall harness                     PASS
    tagged regression suite                      BLOCKED (Docker absent)
    installer functional harness                 BLOCKED (Darwin/Linux only)

Direct Windows/amd64 release-style static links:

    notty-daemon.exe     PE machine=0x8664 bytes=12886016
      sha256=43bbea21d02c1725456b831ab5d3466b3fcb9568d3fd6b61e2ad242585bf9eab
    notty-agent-tool.exe PE machine=0x8664 bytes=6376448
      sha256=868d48812cde1a76143107777cbd4a3a4c3b959ae94bd3baa7a5c79e60047353

Final managed-agent owner-boundary proof:

    RED: agent runtime closed its stores before the registry owner drained tool users:
         sql: database is closed
    GREEN: TestManagedAgentRuntimeRetainsStoresUntilRegistryOwnerCloses PASS
    affected Windows/amd64 race set: PASS (9.117s)

## Interfaces and Dependencies

Define a watcher abstraction in `replica.go` with `Add(string) error`, `Close() error`, and receive-only event/error channels exposed by methods. A real adapter wraps `*fsnotify.Watcher`; tests inject a factory on `workspaceReplica`.

Define a path-lock lease-store abstraction implemented by `*pathLockStore` with cleanup, acquire, release, and close operations. A `WorkspaceFS` holds a reference to one lease store and never opens or closes it per operation. `workspaceRuntime` stores the concrete closeable owner so it can close exactly once after its document cache.

Provide internal run helpers that accept readiness reporting while retaining simple `Run(context.Context) error` entry points for existing callers. Readiness is a one-shot result: nil means all startup work succeeded with the pump active; an error means readiness was never published.

Represent the tool gateway as an owned object with listener acquisition, `Serve() error`, graceful `Shutdown(context.Context) error`, forced `Close() error`, and completion. Normalize `http.ErrServerClosed` only when shutdown was requested.

Use a small core lifetime owner (an `errgroup` or equivalent first-error-plus-wait-group type) whose context is cancelled on the first fatal core error. Do not add agent processes or inbox workers to that fatal group. The agent supervisor and inbox-worker owner expose idempotent shutdown methods that cancel and wait independently.

Revision note (2026-07-13): Initial follow-up plan written after the first PR's handle-cleanup changes were benchmarked and subjected to deterministic local lifecycle probes. It replaces the operation-scoped path-lock decision and requires permanent RED tests before implementation.

Revision note (2026-07-13 13:39 EDT): Recorded the behavior-preserving test seams, successful native ARM64 execution using the cached Yrs archive, and the seven-test RED transcript. Behavioral implementation remains untouched.

Revision note (2026-07-13 13:48 EDT): Recorded the green watcher pipeline and runtime readiness/fatal propagation milestone.

Revision note (2026-07-13 13:52 EDT): Recorded retained per-runtime path-lock ownership, constructor unwind coverage, and focused filesystem validation.

Revision note (2026-07-13 14:02 EDT): Recorded the owned service teardown, joined agent supervisor, ownership-split regressions, complete native ARM64 package pass, and Windows/amd64 race pass.

Revision note (2026-07-13 14:14 EDT): Recorded the final locally available repository gates, exact 28-row audit, release-style static link, and concrete host limitations.

Revision note (2026-07-13 14:20 EDT): Added the managed-agent-runtime store boundary found during final dependency review and its RED/GREEN proof.

Revision note (2026-07-13 14:24 EDT): Recorded base freshness, final publication to PR #149, and the intentionally skipped native-Windows CI job.
