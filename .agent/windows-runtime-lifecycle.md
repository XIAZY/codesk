# Make daemon resource lifetimes explicit and keep native tests portable

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` and is intentionally self-contained.

## Purpose / Big Picture

The native Windows syncer suite currently cannot finish reliably even though the Windows-specific file replacement, identity, and SQLite DSN rows work. A sync test can deadlock before Go's ten-minute package timeout, several otherwise-successful tests fail while deleting their temporary directories, and the fake Codex and Claude processes cannot be launched. After this change, the same daemon package should run on Windows without Windows-only workarounds: filesystem watchers exist only while their event loop is running, SQLite pools are closed by the component that owns them, path-lock connections exist only for the lock lease, and subprocess fixtures are Go helpers that run on every Go-supported host.

The observable proof is a passing native `go test -race -count=1 -v ./daemon/internal/syncer`, including every required Windows row from `.github/workflows/ci.yml`, followed by the repository's full Go, frontend, installer, regression, build, vet, and formatting gates wherever their documented local dependencies are available.

## Progress

- [x] (2026-07-13 13:55 EDT) Reproduced the canonical windows/amd64 release build and artifact verification at `4639f09`.
- [x] (2026-07-13 14:08 EDT) Reproduced the native syncer timeout in `TestWorkspaceRuntimeCreateEditDeleteMultipleFilesRegression` and captured the blocked `fsnotify.Watcher.Add` stack.
- [x] (2026-07-13 14:18 EDT) Ran all 28 required Windows rows independently: 17 passed and 11 failed only during temporary-directory cleanup with an open `path_locks.db` or `sync.db`.
- [x] (2026-07-13 14:31 EDT) Traced watcher, path-lock SQLite, document-cache SQLite, and fake-process ownership from constructors through production shutdown.
- [x] (2026-07-13 14:42 EDT) Verified `origin/feat/windows-cli-daemon` still points to `4639f09` and created `fix/windows-runtime-lifecycle` from that exact tip.
- [x] (2026-07-13 15:42 EDT) Implemented idempotent document-cache close semantics, runtime-owned shutdown, service-level cleanup for direct refresh users, and primary-runtime shutdown waiting.
- [x] (2026-07-13 15:36 EDT) Scoped path-lock SQLite connections to acquisition/release and removed the process-global store map.
- [x] (2026-07-13 15:36 EDT) Moved filesystem watcher acquisition into `workspaceReplica.Run`, beside its sole event consumer.
- [x] (2026-07-13 15:38 EDT) Replaced POSIX-shell fake Codex and Claude executables with the Go test binary in helper-process mode.
- [x] (2026-07-13 15:44 EDT) Added cleanup helpers for directly-owned test caches/runtimes and a watcher-construction lifecycle assertion; retained the multi-file hang regression.
- [x] (2026-07-13 16:02 EDT) Ran focused tests, the native Windows race command and 28-row audit, full Go test/vet/build, Yjs canary, frontend typecheck/Vitest/build, uninstall harness, installer syntax, canonical Windows release build/verification, and all locally representable gates. Recorded Docker/Postgres and installer-host limitations explicitly.
- [x] (2026-07-13 16:08 EDT) Committed and pushed `fix/windows-runtime-lifecycle`; opened draft PR #149 targeting `feat/windows-cli-daemon`.

## Surprises & Discoveries

- Observation: The Windows-specific atomic-replace, concurrent-observation, file-identity, cross-device, and SQLite drive-path rows all pass at the base tip.
  Evidence: The isolated required-row run reported 17 functional passes; all 11 failures were `TempDir RemoveAll cleanup` errors for open SQLite files.

- Observation: The watcher deadlock is caused by lifecycle order, not an insufficient Windows event buffer.
  Evidence: `fsnotify` uses one Windows backend goroutine both to deliver events on an unbuffered channel and to acknowledge later `Add` requests. Tests construct a watcher and add directories without running `workspaceReplica.Run`, so an undrained event blocks the backend and a later `Add` waits forever.

- Observation: `WorkspaceFS` behaves as a stateless, freely-created value throughout production and tests.
  Evidence: short-lived instances are created in local-create validation, materialization, tracked-file fallbacks, and 25 tests. Giving every instance a long-lived SQLite pool would expand rather than clarify ownership.

- Observation: Unix currently hides both SQLite leaks because it permits unlinking an open file.
  Evidence: the same tests pass on Unix while Windows reports sharing violations deleting `.notty/path_locks.db` and `.notty/sync.db`.

- Observation: Adding `.exe` or `.cmd` to the fake process names would not make the fixture portable.
  Evidence: their contents are POSIX shell programs, while the native Go process APIs require a host-executable program. The test binary itself is already a native executable on every test host.

- Observation: Direct `Service.refresh` tests are resource owners even though they never call `Service.Run`.
  Evidence: after the first fixes, the complete package found exactly four remaining `sync.db` sharing violations. Each test caused `refresh` to lazily create `primaryRuntime`; registering `closePrimaryRuntime` removed all four and the package passed.

- Observation: Operation-scoped path-lock connections are fast enough for the current native suite.
  Evidence: the full race-enabled package, including concurrent scan and filesystem rows, completed in 21.243 seconds; the formerly failing append row completed in 0.09 seconds.

- Observation: The backend daemon-state golden failure was not wire drift.
  Evidence: generated and committed JSON had identical tokens and values; the only difference was LF from `json.MarshalIndent` versus CRLF from the Windows checkout. Compacting both JSON documents before the exact comparison keeps field/value/number spelling checks while ignoring insignificant whitespace.

- Observation: Three documented gates share one unavailable local dependency.
  Evidence: the live-Postgres script, tagged regression suite, and browser smoke each stopped before application code because `docker` is not installed. The PostgreSQL canary separately reported `NOTTY_DATABASE_TEST_URL is not set` and skipped, so it was not counted as passing.

- Observation: The installer behavior harness intentionally rejects MSYS as an unsupported test OS.
  Evidence: its first branch accepts only `Darwin` or `Linux`. Shell syntax and the uninstall behavior harness pass; faking only the initial `uname` is not faithful because the harness later replaces `PATH` and re-detects MSYS.

## Decision Log

- Decision: Do not catch or retry Windows sharing violations.
  Rationale: The sharing violation is evidence of an open resource. Hiding it would preserve the leak and make shutdown nondeterministic.
  Date/Author: 2026-07-13 / Codex

- Decision: Do not add an arbitrary buffer to `fsnotify`.
  Rationale: Any finite buffer can fill when no consumer exists. The watcher belongs inside the lifetime of the event loop that drains it.
  Date/Author: 2026-07-13 / Codex

- Decision: Keep `WorkspaceFS` stateless and make each path-lock store live from acquisition through release.
  Rationale: This mirrors the actual lease lifetime and avoids a hidden `Close` requirement on numerous ephemeral `WorkspaceFS` values. The path-lock database remains the platform-neutral cross-process lock mechanism.
  Date/Author: 2026-07-13 / Codex

- Decision: Make `workspaceRuntime` own and close its document cache, and make close idempotent.
  Rationale: The cache is intentionally long-lived and shared by the runtime's loops. The runtime is the smallest component that knows when all users have stopped. Idempotence lets production and test cleanup compose safely.
  Date/Author: 2026-07-13 / Codex

- Decision: Use the current Go test executable as the fake Codex/Claude subprocess, selected by an inherited test-only environment variable.
  Rationale: It is native on the host, requires no shell, compiler invocation, or OS-specific wrapper, and can implement the exact protocol behavior under test.
  Date/Author: 2026-07-13 / Codex

## Outcomes & Retrospective

The implementation is complete and locally validated. The final native rerun, `go test -race -count=1 -v -timeout=10m ./daemon/internal/syncer`, passed in 26.662 seconds and an automated extraction of the workflow's `$required` array reported `required=28 passed=28 missing=0`. The final `go test ./...`, `go vet ./...`, and `go build ./...` all passed. The Yjs canary ran and passed rather than skipping. Frontend typecheck plus 250 Vitest tests passed, and the production build passed.

The canonical windows/amd64 release rebuilt and verified two PE binaries, one exact manifest artifact, and matching checksums. The uninstall harness and both installer shell syntax checks passed. Docker-dependent live PostgreSQL, tagged compose regression, and browser smoke could not start because this host has no Docker executable; the installer behavior harness cannot model a Linux/macOS install from MSYS. These are concrete environment limitations rather than green checks.

Published as draft PR [#149](https://github.com/XIAZY/notty/pull/149), with `fix/windows-runtime-lifecycle` targeting `feat/windows-cli-daemon`.

## Context and Orientation

The affected package is `daemon/internal/syncer`.

`workspaceRuntime` in `workspace_runtime.go` coordinates reconciliation, presence, websocket delivery, a `workspaceReplica`, and a `documentCache`. `newDocumentCache` in `document_cache.go` opens the workspace-local `.notty/sync.db` through `database/sql`. Before this change the store has no `Close` method, so both production runtimes and direct unit-test caches retain a SQLite connection indefinitely.

`workspaceReplica` in `replica.go` materializes files and watches the workspace with `fsnotify`. Before this change `newWorkspaceReplica` creates the watcher, while only `workspaceReplica.Run` consumes `watcher.Events` and `watcher.Errors`. Synchronous tests call reconciliation methods without `Run`, which violates that implicit ordering.

`WorkspaceFS` in `workspace_fs.go` wraps reads, appends, create-or-read, moves, deletes, and projections. These operations coordinate across daemon processes through `pathLockStore` in `path_locks.go`. Before this change `pathLockStoreForRoot` caches every opened `*sql.DB` forever in a package-global `sync.Map`. A lock lease is the interval beginning when `lockPaths` returns successfully and ending when its returned unlock function runs.

`codex_driver_test.go`, `appserver_test.go`, and `claude_driver_test.go` currently write extensionless `#!/bin/sh` scripts and pass those paths to `os/exec`. That works on Unix but not native Windows. A Go helper process is the package's own test executable launched again with a test-only environment marker; `TestMain` detects that marker before invoking `m.Run`, handles the requested fake protocol, and exits.

The disabled native Windows job in `.github/workflows/ci.yml` remains the source of truth for the exact release and syncer commands and its 28 required test names. The enabled Linux backend, frontend, regression, e2e, Windows cross-build, and diff-check jobs define the broader repository gates.

## Plan of Work

First add idempotent `Close` to `workspaceStore`. Ensure `newWorkspaceRuntime` closes a cache if later construction fails, make `workspaceRuntime.Run` close its owned resources after all child loops stop, and make `Service.Run` wait for the primary runtime before returning. Direct test caches and runtimes will use small `t.Cleanup` helpers so a passing assertion cannot leave a database open until process exit.

Next replace `pathLockStoreForRoot` and its global map with `newPathLockStore`. `CleanupStaleLocks` will close its temporary store after cleanup. `lockPaths` will close on acquisition failure and return an idempotent unlock closure that first releases its leases and then closes the database. This keeps a connection alive for exactly as long as a filesystem operation can hold the corresponding lease.

Then move `fsnotify.NewWatcher` from `newWorkspaceReplica` to the start of `workspaceReplica.Run`. Assign it before initial watch registration, consume the same local watcher's channels in the run loop, and clear/close it on exit. Synchronous reconciliation before `Run` will deliberately skip watch registration; periodic scanning still performs the requested work. Add a focused assertion that construction itself does not acquire a watcher, while retaining the existing multi-file regression that previously hung.

Finally add a package-level `TestMain` helper mode for fake external programs. Replace script-writing helpers with functions that return `os.Executable()` and select a Codex or Claude behavior through `t.Setenv`. Reimplement only the protocol surface used by the tests: detection arguments, Codex JSON-RPC initialization and event flooding, and Claude stream-JSON request/result handling plus its existing environment-controlled failure cases.

After focused validation, run the exact native Windows commands and inspect the raw required-row results. Then run every documented repository gate that can run on this machine. Any unavailable external service or browser/container dependency must be reported distinctly from a code failure; it must not be silently treated as a pass.

## Concrete Steps

All commands run from `C:\Users\zhong\notty` in PowerShell unless stated otherwise.

1. Format and run focused syncer tests after each logical edit:

       gofmt -w daemon/internal/syncer/<changed-go-files>
       go test -count=1 ./daemon/internal/syncer

2. Reproduce the native Windows CI gate using the installed x86_64 Go/Rust/GNU toolchain:

       $env:CC = 'C:\msys64\mingw64\bin\gcc.exe'
       $env:CGO_ENABLED = '1'
       go test -race -count=1 -v ./daemon/internal/syncer

   Save the output and programmatically verify that each name in the `$required` array in `.github/workflows/ci.yml` has a `--- PASS:` row.

3. Run the full repository gates:

       gofmt -l <all tracked Go files outside third_party>
       go vet ./...
       go test ./...
       go build ./...
       make daemon-installer-check
       make daemon-uninstall-test
       npm test --prefix frontend
       npm run build --prefix frontend
       go test -tags=regression -count=1 ./test/regression

   Also run the live-Postgres canary and browser smoke if their required local services are available. Set `NODE_PATH` to `frontend/node_modules` so the Yjs cross-compat row runs instead of courtesy-skipping.

4. Inspect `git diff --check`, the focused diff against `feat/windows-cli-daemon`, and the final status. Stage only intentional tracked changes, leaving `agents/` and `workspaces/` untouched.

5. Commit and push `fix/windows-runtime-lifecycle`, then open a PR whose base is `feat/windows-cli-daemon`.

## Validation and Acceptance

Acceptance requires all of the following:

The multi-file workspace regression returns promptly and passes under `-race`. The complete syncer package passes natively on Windows. All 28 names in the native job's `$required` array appear as passing rows. No test reports a `TempDir RemoveAll cleanup` sharing violation. Codex and Claude subprocess tests launch and pass without relying on `sh`, `.cmd`, or OS build tags.

The full Go suite, Go build, vet, formatting, frontend tests, and frontend production build pass. Installer, regression, live-Postgres, and browser gates either pass or have a concrete, reproduced local infrastructure limitation recorded in this plan and the PR; a skip is never described as a pass. `git diff --check` is clean.

The production resource contract is visible in code: path-lock connections close in the unlock path, the document cache exposes `Close`, the runtime closes it after its goroutines stop, and the watcher is created and closed inside `Run`. Tests explicitly register cleanup for resources they construct directly.

## Idempotence and Recovery

All test and formatting commands are safe to rerun. `Close` methods must be idempotent so a test can close early to exercise reopen behavior and still retain its registered cleanup. The path-lock unlock closure must also be idempotent.

If a full-suite command is interrupted, rerun it from the repository root; it does not mutate tracked source. Release outputs must go under a temporary directory, not the worktree. Do not remove or stage the pre-existing untracked `agents/` and `workspaces/` directories.

If the base branch moves before publication, fetch it and inspect the new commits before rebasing. Do not reset or overwrite unrelated work.

## Artifacts and Notes

Base branch and commit:

    feat/windows-cli-daemon
    4639f095ae8976d992361a3ea57aebd1aa80e6c6

Baseline canonical archive:

    C:\Users\zhong\AppData\Local\Temp\notty-local-windows-ci-4639f09\notty-release\p1-ci\notty-daemon_p1-ci_windows_amd64.zip
    SHA256 ba97cb3e68bcd3e95a3252016234c00e9fe795d9a247e7a2afb04490f7a5417f

Final canonical archive:

    C:\Users\zhong\AppData\Local\Temp\notty-release-fix-windows-runtime-lifecycle\p1-ci\notty-daemon_p1-ci_windows_amd64.zip
    SHA256 bea361779af88b6162e86c9091518799286615dba21087aadf29a47442e04c44

Baseline syncer timeout stack, abbreviated:

    TestWorkspaceRuntimeCreateEditDeleteMultipleFilesRegression
      workspaceReplica.reconcileLocalWorkspace
      workspaceReplica.ensureDirectoryWatches
      workspaceReplica.addWatchDir
      fsnotify.(*Watcher).Add
      fsnotify Windows backend waiting for input reply

The backend's only `readEvents` goroutine was simultaneously blocked sending an event to the unbuffered `Events` channel.

## Interfaces and Dependencies

`workspaceStore` gains:

    func (c *workspaceStore) Close() error

`pathLockStore` gains:

    func newPathLockStore(root string) (*pathLockStore, error)
    func (s *pathLockStore) Close() error

`pathLockStoreForRoot` and the package-global `pathLockStores` map are removed.

`workspaceRuntime` gains:

    func (r *workspaceRuntime) Close() error

Test-only helpers return the existing concrete types while registering `t.Cleanup`; production public configuration remains unchanged.

Revision note (2026-07-13): Initial plan written after reproducing all three native failure classes and tracing resource ownership. The operation-scoped path-lock decision supersedes the earlier idea of adding a persistent store to every `WorkspaceFS` instance because production treats `WorkspaceFS` as an ephemeral value.

Revision note (2026-07-13): Updated after implementation and native race validation. The first complete package run revealed four direct-refresh service fixtures as an additional ownership boundary; explicit primary-runtime cleanup fixed those without platform-specific behavior.

Revision note (2026-07-13): Updated after full-repository validation. Added the JSON-whitespace golden fix discovered by the complete suite, final pass counts, canonical artifact hash, and exact local infrastructure limitations.

Revision note (2026-07-13): Recorded publication as draft PR #149 against the requested feature branch.
