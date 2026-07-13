# Make the headless daemon safe and usable on Windows

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must stay current while implementation proceeds. Maintain this file in accordance with `.agent/PLANS.md` from the repository root.

## Purpose / Big Picture

After this work, a Windows user can run the existing command-line daemon directly on Windows, point it at a shared Codesk workspace, and trust that document creation, editing, renaming, deletion, restart recovery, and Codex or Claude agent execution preserve the same data-integrity guarantees as Linux and macOS. The daemon must not silently skip a path that NTFS cannot represent, treat two differently cased spellings as separate files, leave provider subprocesses behind, or claim health while materialization is failing.

This plan deliberately excludes the system-tray application, PowerShell installer, upgrade/uninstall UX, signing, and macOS tray. Those are later packaging tasks. “Windows-ready” here means the existing headless daemon builds, starts from a config file or environment overrides, syncs safely on NTFS, controls both provider CLIs, reports actionable failures, and passes a Windows-native acceptance suite.

## Progress

- [x] (2026-07-12 21:47Z) Claimed implementation task #4 in `#daemon-gui`; Deniz claimed independent QA task #3.
- [x] (2026-07-12 21:50Z) Reproduced the baseline: `notty-agent-tool` cross-builds for Windows with CGO disabled, while `notty-daemon` fails at the CGO-only Yrs boundary because `crdt.Doc` disappears.
- [x] (2026-07-12 21:51Z) Froze the CLI-only boundary with Bill and the Phase 2 path contract with Thomas.
- [x] (2026-07-12 22:08Z) Added red-first semantic lock, stable identity, atomic replacement, and cross-device rows; implemented platform twins; full Linux syncer suite is green and the selected Windows files/tests cross-compile.
- [x] (2026-07-12 22:08Z) Added GNU Yrs target staging, Windows release mappings, and a native `windows-latest` CI job with required-test pass guards.
- [x] (2026-07-12 22:37Z) Addressed P1 review holds: metadata and identity now come from one filesystem object observation, zero Windows file indexes are invalid, shared-lock semantics are pinned, Windows rename/recreate rows are required, and native CI executes and verifies the canonical release artifact path.
- [x] (2026-07-12 23:52Z) Native Windows execution invalidated the target byte-lock model: append-lock and replace-under-lock returned `Access is denied`, and the POSIX-disposition identity row modeled a lifecycle the daemon does not use.
- [x] (2026-07-13 00:00Z) Froze and implemented the amended P1 protocol: `WorkspaceFS.lockPaths` is the sole daemon mutation coordinator, fallback scan is an old-or-new delete-shared observation, initial creation is create-empty-or-read/preserve under the path lease, and identity tests use the production close/remove/recreate lifecycle.
- [x] (2026-07-13 00:29Z) Narrowed the initial-creation API after QA review: `CreateEmptyOrRead` cannot stage arbitrary bytes, never removes a pathname after late failure, re-observes the final pathname after create, and is the cache-nil branch's single observation/creation operation.
- [x] (2026-07-13 01:08Z) Replaced `MoveFileEx` with handle-based `FileRenameInfoEx` using replace/POSIX semantics after native Windows proved that delete sharing alone does not make `MoveFileEx` compatible with an open observation. Added direct ABI and behavior rows, plus repeated scan/write stress coverage.
- [ ] Milestone 1 checkpoint: finish full repository gates, publish an exact head, and obtain native Windows CI evidence plus lead stamp before Phase 2 or Phase 3.
- [ ] Milestone 2: implement deterministic portable-path materialization, host path keys, containment, and visible health failures red-first.
- [ ] Milestone 3: implement Windows provider command/process-tree ownership and prove Codex/Claude lifecycle behavior.
- [ ] Milestone 4: run Windows-native NTFS/fsnotify, restart, convergence, config, and live-provider acceptance; close remaining defects and document the supported CLI launch.

## Surprises & Discoveries

- Observation: the first Windows failure is not a Windows syscall compile error. Disabling CGO removes the entire Yrs-backed `internal/ycrdt` API, so daemon compilation stops at `crdt.Doc` references first.
  Evidence: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./daemon/cmd/daemon` fails with `undefined: crdt.Doc`; the same command for `./daemon/cmd/agenttool` succeeds.

- Observation: native Windows proved the target byte-lock layer was not only non-portable but also a false coordination boundary. An `O_APPEND` handle lacks the access `LockFileEx` requires, and a byte-range lock on the destination prevents `MoveFileEx` from replacing it.
  Evidence: AlphaToad's exact-head run failed both append lock rows and `TestWriteFileLockedCanReplaceItsDeleteSharedDestinationOnWindows` with `Access is denied`.

- Observation: the target byte locks did not coordinate with the real `WorkspaceFS` write/delete/move protocol, while external editors never reliably honored Unix `flock`. Preserving them through a sidecar would add a third lock system without fixing ownership.
  Evidence: canonical mutations already serialize through `.notty/path_locks.db`; the lock helpers were used only by fallback scan, an initial empty-materialization outlier, and tests.

- Observation: Go's ordinary Windows read handle does not request delete sharing, so an unlocked fallback scan can still block `MoveFileEx` during overlap.
  Evidence: the amended Windows observation opens with read/write/delete sharing and the concurrency row requires every scan to return complete old-or-new bytes while atomic replacements commit without `Access denied` or temp residue.

- Observation: `MoveFileEx(REPLACE_EXISTING)` still rejects an open destination even when that observation opted into delete sharing. The Windows rename contract that explicitly preserves existing handles while rebinding the path is `FileRenameInfoEx` with `FILE_RENAME_REPLACE_IF_EXISTS | FILE_RENAME_POSIX_SEMANTICS`.
  Evidence: the direct open-observation replacement row failed with `Access is denied` under both native ARM64 and emulated AMD64, then passed with the handle-based rename. Microsoft documents that POSIX rename semantics keep existing handles valid while subsequent opens resolve to the replacement object.

- Observation: the Win32 `FILE_RENAME_INFO` buffer requires a trailing UTF-16 NUL even though `FileNameLength` excludes it.
  Evidence: omitting the terminator passed the single-row probe but intermittently returned `ERROR_INVALID_NAME` under repeated concurrent scan/write stress; an ABI row now pins both the buffer size and excluded length.

- Observation: Windows stable file identity cannot be implemented honestly from the current `fileIdentityForInfo(os.FileInfo)` signature. Windows needs an opened file handle to call `GetFileInformationByHandle` and retrieve volume serial plus 64-bit file index.
  Evidence: `daemon/internal/syncer/replica.go` currently type-asserts `info.Sys()` to Unix `*syscall.Stat_t`.

- Observation: portable filename validation exists only in the frontend. The daemon currently accepts root-namespace paths that Windows cannot materialize.
  Evidence: reserved-name and illegal-character rules are in `frontend/src/App.tsx`; no daemon equivalent exists.

- Observation: replacing SQLite with a pure-Go driver would not remove the Windows C toolchain because Yrs remains a Rust static library linked through CGO. Keep `go-sqlite3` in this effort.
  Evidence: `internal/ycrdt/doc.go` is a CGO translation unit and `go.mod` uses `github.com/mattn/go-sqlite3`.

- Observation: Cargo places a targeted GNU Windows static library under `target/x86_64-pc-windows-gnu/release`, while the CGO directive intentionally consumes one stable `target/release/libyrs.a` path.
  Evidence: the Windows build must stage the target-specific `libyrs.a` into the stable link location before Go compilation; a CGO-disabled daemon build cannot validate this boundary.

- Observation: Claude instructions currently use `--append-system-prompt <full text>`, exposing arbitrary prompt contents in host process inspection on every platform and making `.cmd` invocation unsafe on Windows.
  Evidence: `claude_driver.go` currently builds the full instruction text into argv; the installed Claude CLI supports `--append-system-prompt-file`.

- Observation: a path-based identity helper is not sufficient if callers separately stat metadata first. A rename or delete/recreate between those observations can combine object A's kind/existence with object B's identity.
  Evidence: the first P1 checkpoint called `os.Stat` or `DirEntry.Info` and then reopened the path through `fileIdentityForPath` in all three local-create paths. The amended `statFileWithIdentity` returns both values from one Unix stat or one Windows handle.

## Decision Log

- Decision: keep the task CLI-only and exclude tray, installer, signing, update/uninstall, and macOS packaging.
  Rationale: a GUI must not hide unresolved daemon data-integrity defects; the headless daemon is the prerequisite.
  Date/Author: 2026-07-12, AlphaToad/Bill/Thomas/Vitaliy.

- Decision: keep `go-sqlite3` and build it with the same MinGW toolchain used by Yrs.
  Rationale: migrating the persistence driver adds behavioral and performance risk without removing CGO.
  Date/Author: 2026-07-12, Thomas/Bill.

- Decision: Phase 1 uses platform-specific files and semantic common APIs, with no `runtime.GOOS` branches inside sync decisions.
  Rationale: platform mechanics should be isolated and directly testable while the shared sync model remains one implementation.
  Date/Author: 2026-07-12, Thomas/Vitaliy.

- Decision: remove the redundant target byte-lock API and keep `WorkspaceFS.lockPaths` as the single cross-process mutation coordinator.
  Rationale: target byte locks conflict with Windows atomic replacement, do not coordinate with canonical workspace mutations, and a sidecar would create another lock lifecycle plus Phase-2 key ambiguity. Fallback scans are old-or-new observations; initial creation uses create-empty-or-read under the existing path lease.
  Date/Author: 2026-07-12, Thomas/Deniz/Bill/Vitaliy, after native Windows red evidence.

- Decision: stable file identity becomes path/handle-based rather than `os.FileInfo`-based.
  Rationale: Windows volume serial and file index require a handle; pretending `FileInfo.Sys()` provides the same contract would return invalid identities and silently disable move pairing.
  Date/Author: 2026-07-12, Vitaliy, pending Phase 1 lead diff review.

- Decision: callers that need both metadata and stable identity use `statFileWithIdentity(path)` and consume one observation. Windows identities are valid only when the combined file index is nonzero.
  Rationale: independent path observations create a TOCTOU false-pairing window, while an unconditional zero file index can make unrelated files on one volume compare equal.
  Date/Author: 2026-07-12, Thomas/Bill/Vitaliy.

- Decision: Windows atomic replacement uses a sibling staging file plus handle-based `FileRenameInfoEx` with replace/POSIX semantics. Daemon scan observations opt into delete sharing; externally held destinations that deny it fail visibly and retain complete old content.
  Rationale: `MoveFileEx` cannot perform the required namespace swap while a scan observation is open. POSIX rename semantics are the Windows 10 v1607+ primitive whose contract explicitly keeps old handles valid while future opens resolve to the replacement object.
  Date/Author: 2026-07-13, AlphaToad/Vitaliy, amended after native Windows red and stress evidence.

- Decision: initial absent-file materialization uses the narrow `WorkspaceFS.CreateEmptyOrRead` operation under the path lease and never replaces a local file that appeared before exclusive create. It does not remove the pathname after a late create/close failure and re-observes the final path after successful create.
  Rationale: production creates only an empty placeholder. Encoding that limit in the API makes partial arbitrary content impossible, avoids deleting a non-cooperating editor's replacement object by pathname during cleanup, and returns the actual bytes/hash if an external rename-over wins while the created handle is open.
  Date/Author: 2026-07-12, Thomas/Deniz/Vitaliy.

- Decision: portable workspace naming is global behavior, not a Windows-only filter.
  Rationale: Linux/macOS writers must not publish shared root state that Windows peers cannot materialize.
  Date/Author: 2026-07-12, Thomas.

- Decision: preserve `DesiredPath` as user intent and compute a separate deterministic `MaterializedPath` for non-portable remote entries.
  Rationale: invalid remote state must remain represented and converge across Windows peers; it may not disappear or overwrite another document.
  Date/Author: 2026-07-12, Thomas.

- Decision: local non-portable creates remain untouched and unpublished while the daemon reports an actionable health error.
  Rationale: silently renaming a user file or publishing an unmaterializable path both violate user intent.
  Date/Author: 2026-07-12, Thomas.

- Decision: arbitrary provider payload never enters a Windows command line. Claude instructions move through a unique user-private prompt file for the provider lifetime; turns remain JSON over stdin; unsupported old Claude versions are unavailable on Windows.
  Rationale: eliminating shell-interpreted payload is safer than attempting to hand-escape an unstable `cmd.exe` boundary and also removes prompt text from process inspection on every platform.
  Date/Author: 2026-07-12, Thomas/Bill/Deniz.

- Decision: provider drivers receive an owned process abstraction and never handle `cmd.exe` or Job handles. Windows containment is race-free `CREATE_SUSPENDED -> AssignProcessToJobObject -> ResumeThread`, with one kill-on-close Job per provider.
  Rationale: assigning after ordinary `exec.Cmd.Start` permits a fork-at-first-instruction child to escape; PID-only kill leaks wrapper descendants.
  Date/Author: 2026-07-12, Bill/Thomas/Deniz.

## Outcomes & Retrospective

The task is in progress. Native Windows execution found the target-lock defects, a POSIX-assumptive identity row, and the mismatch between `MoveFileEx` and delete-shared observations before merge. The amended P1 code now has one mutation owner, no target byte-lock layer, deterministic local-create preservation, handle-based POSIX replacement, delete-shared scan observations, and production-lifecycle identity rows. Milestone 1 still requires a green exact-head native rerun before Phase 2 or Phase 3 opens.

## Context and Orientation

The repository is a Go monorepo. `daemon/cmd/daemon/main.go` starts the headless daemon. Most behavior lives in `daemon/internal/syncer`. A workspace replica watches a local directory with `fsnotify`, projects CRDT documents to files, and records local changes back to the shared root namespace. The CRDT engine is `internal/ycrdt`, a CGO wrapper around the Rust `yffi` static library in the `third_party/y-crdt` submodule. Local metadata and path leases use `go-sqlite3`, which is also CGO.

The immediate Unix-only filesystem sites at baseline were:

* the former `daemon/internal/syncer/file_lock.go`, where common callers used Unix `flock` constants and `replaceFileAtomically` used rename-over-existing semantics; the byte-lock layer was retired after native Windows invalidated its ownership model;
* `daemon/internal/syncer/replica.go`, where `fileIdentityForInfo` asserts Unix `syscall.Stat_t` and feeds rename pairing.
* `daemon/internal/syncer/workspace_fs.go`, where archive fallback recognizes only Unix `EXDEV`.
* `scripts/build-daemon-release.sh`, where release mappings include only Linux and Darwin and assume Unix package formats.

“Move pairing” means recognizing that a disappeared tracked file and a newly created path are the same physical file. `workspaceChangeIndex` stores a stable identity for each path and pairs missing/created events when identities match. Linux obtains device and inode. Windows must obtain volume serial and the combined high/low file index from `GetFileInformationByHandle`. Losing this identity does not merely reduce performance; it turns a rename into an unrelated delete plus create and can corrupt document identity.

There are two different path-key requirements. A portable shared key compares root-namespace document paths identically on every OS using slash normalization, Unicode normalization, and case folding. A host-local key compares absolute filesystem paths according to host semantics so maps, watches, and SQLite leases cannot assign two owners to one NTFS path. Do not combine these keys or reuse a display path as a key.

Provider runtimes are implemented by `codex_driver.go`, `appserver.go`, and `claude_driver.go`. They currently call `exec.LookPath`, `exec.Command`, or `exec.CommandContext` directly and kill one PID. On Windows, npm-installed CLIs commonly resolve to `.cmd` shims requiring command-interpreter quoting, and killing the wrapper PID does not guarantee its Node descendants exit. A Windows Job Object with `KILL_ON_JOB_CLOSE` must own each provider process tree.

Configuration currently comes only from environment variables in `daemon/internal/syncer/config.go`, with container-oriented `/workspace/...` defaults. A clicked Windows executable has no shell environment. CLI-only Windows support therefore needs an OS-appropriate config/data location and config-file loading, while environment variables remain the highest-priority override for headless deployments and tests.

## Plan of Work

### Milestone 1: platform primitives and a Windows build gate

First make the platform boundary honest without changing path allocation or sync policy. Keep `WorkspaceFS.lockPaths` as the only daemon mutation coordinator and remove the redundant target byte-lock API. `WorkspaceFS.Append`, `WriteIfUnchanged`, and `CreateEmptyOrRead` execute under that path lease. Fallback scan remains a cheap eventual observation; on Windows its read handle opts into delete sharing so it cannot block an atomic replacement. Native tests prove complete append records, complete old-or-new scan observations during replacement, no stranded temp file, preservation of a local file that appears before exclusive creation, and no pathname deletion after a late create/close failure.

Move atomic replacement to `file_replace_unix.go` and `file_replace_windows.go`. Unix retains temp-write then rename. Windows closes the temp file, reopens it with delete access, and commits it through `FileRenameInfoEx` with replace/POSIX semantics so delete-shared observations retain the old object while new opens resolve to the replacement. Sharing violations must return a visible error; never truncate the destination first. Windows tests cover destination absent, destination present, destination open with and without delete sharing, the exact UTF-16 rename-info ABI, and unchanged/complete content after failure.

Replace `fileIdentityForInfo` with `fileIdentityForPath`. Unix stats the path and maps device/inode. Windows opens the path with read-attributes and read/write/delete sharing, includes backup-semantics for directories, calls `GetFileInformationByHandle`, and maps volume serial and 64-bit file index into the existing `fileIdentity`. Update watcher and scan call sites to pass the path. Windows tests prove identity remains stable across a rename and differs for delete/recreate.

Replace the inline `syscall.EXDEV` branch with `isCrossDeviceError`. Unix checks `EXDEV`; Windows unwraps path/link errors and checks `ERROR_NOT_SAME_DEVICE`.

Extend the build tooling without changing SQLite drivers. `scripts/build-daemon-release.sh` gains `windows/amd64` target mappings and `.exe` output/package handling. `scripts/build-yffi.sh` and the CGO link path must copy or select the `x86_64-pc-windows-gnu` `libyrs.a` deterministically. Add a dedicated Windows CI job that installs Go, Rust GNU target, MSYS2/MinGW, builds Yrs, runs the Windows-native syncer tests, and builds both daemon and agent-tool. The job must fail if Windows tests are skipped.

The Windows job must enter through `scripts/build-daemon-release.sh`, not a parallel ad hoc `go build`. It verifies the zip members are the two `.exe` binaries with valid PE headers/signatures, the manifest contains exactly the Windows/amd64 artifact, and `SHA256SUMS` plus the manifest hash match the archive. The native primitive suite structurally guards removal of the legacy byte-lock files/symbols and requires canonical append/create-empty-or-read/scan-replacement rows, file and directory rename stability, production close/remove/recreate inequality, and deterministic replacement-object identity.

Milestone 1 acceptance is a Windows-native job that builds both binaries and passes the primitive tests, while the existing Linux suite remains green. It is not yet “Windows-ready” because path convergence, configuration, and provider trees remain unresolved.

### Milestone 2: portable paths, collision ownership, containment, and health

Add three explicit modules: syntax normalization, portable path validation/materialization, and comparison keys. The shared portable key must slash-normalize, Unicode-normalize, and case-fold. Use it for root projection allocation (`used`, `claimedByPath`, and previous materialization). Keep original display casing and desired user path separately.

Reject traversal, drive-relative or absolute paths, UNC/device paths, empty/dot segments, Windows device names even with extensions, control/illegal characters, and trailing dots/spaces. Define a deterministic total path limit that does not depend on the machine registry. For an invalid remote CRDT path, retain `DesiredPath`, derive a safe `MaterializedPath` from the desired name plus document-ID suffix, and report a visible warning. For an invalid local create, leave the file untouched, do not publish it, and report an actionable warning.

Add one host-local path-key seam and route `projectedByPath`, watched directories, change-index identities/creates, and SQLite path lock keys through it. On Windows, normalize volumes, separators, and case; on Unix preserve case. Implement case-only rename through a unique intermediate path when direct rename would collide.

Containment must reject junction or reparse-point escape, not merely lexical `filepath.Rel` traversal. Resolve and validate every materialization parent against the workspace root before writing. The check must not follow a newly introduced junction outside the root.

Extend daemon status reporting so unresolved materialization or local portable-name failures are visible to the backend/UI. Logs alone are not acceptance. Define a small structured health item with stable code, desired/local path, document ID when available, and actionable summary; do not leak file contents or credentials.

Milestone 2 acceptance is the frozen red matrix: case collisions, Unicode normalization collision, case-only rename, all invalid-name/path classes, deterministic remote fallback across restart, local invalid create remaining unpublished, one owner/lease for casing variants, reparse escape rejection, and deterministic long-path behavior.

### Milestone 3: provider payload transport and process-tree ownership

Create one resolver used by Codex detection, Codex app-server spawn, and Claude detection/spawn. It returns a resolved path plus `native` or `batchShim`, honors `PATHEXT`, and prefers a native `.exe`. Keep detection, payload transport, invocation, and process ownership separate. Batch-shim argv is restricted to fixed flags, validated UUID session IDs, and controlled paths; arbitrary instructions never enter it.

Move Claude instructions to a uniquely created prompt file in user-private state via `--append-system-prompt-file`; turns remain JSON over stdin. Keep each file for its provider process lifetime because Claude has no consumption acknowledgment. Delete it on failed start and after Wait/Stop. On daemon startup, perform a bounded stale-file sweep before new providers start. Enforce owner-only mode/ACL, exclusive create, reparse/symlink protection, per-provider isolation, and content/path redaction in logs and status. Mark an old Claude lacking the file option visibly unavailable on Windows rather than falling back to prompt-in-argv.

Create an owned provider process abstraction. Unix retains current process behavior unless a tested process-group improvement is needed. Windows uses custom suspended creation, assigns the process to a per-provider Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, then resumes it before protocol streams run. Closing one Job kills exactly that provider tree; daemon crash closes all Jobs; sibling providers remain alive. Drivers must not know about `cmd.exe` or Job handles.

Milestone 3 acceptance includes native Windows rows for exact prompt-file bytes with shell metacharacters/CRLF/Unicode, no payload in process argv/logs, no injection side effect, unsupported-version cleanup, fork-at-first-instruction containment, sibling isolation, daemon-crash reaping, and current Codex/Claude CLI protocol smoke.

### Milestone 4: Windows-native end-to-end acceptance

Replace container-only defaults in `config.go` with OS-aware helper functions. Windows defaults use the user config/data directory and a clear user workspace directory. Add a config-file representation with non-secret paths and endpoints; environment variables override file values. Do not pass the daemon token as a command-line flag. Credential Manager/DPAPI storage may remain a later hardening subtask only if the CLI config contract records a user-only ACL requirement and QA can verify it.

Run the existing move/rename regression suite against Windows `ReadDirectoryChangesW`, then add file rename, directory rename, case-only rename, delete/recreate, burst/coalesced events, lock contention, atomic replace, restart recovery, SQLite outbox recovery, offline backend, invalid token, and CRDT projection convergence. The real Windows runner must launch the built daemon against an isolated backend/Postgres fixture and exercise both runtime CLIs.

Record the supported CLI launch and config paths in repository documentation. At completion, a clean Windows machine must build or download the unsigned internal binary, load configuration without a shell, sync safely, surface failures, run both providers, recover after restart/network loss, and stop without orphans.

## Concrete Steps

Run commands from the repository root unless stated otherwise.

Baseline red and control:

    env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/notty-agent-tool.exe ./daemon/cmd/agenttool
    env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/notty-daemon.exe ./daemon/cmd/daemon

The first command succeeds. The second currently fails with `undefined: crdt.Doc` because CGO/Yrs is unavailable.

Initialize and build the Yrs submodule for the host before Linux regression work:

    git submodule update --init --recursive
    scripts/build-yffi.sh
    go test ./daemon/internal/syncer ./daemon/cmd/daemon ./daemon/cmd/agenttool

For every milestone, run formatting and repository gates:

    gofmt -w <changed Go files>
    go test ./daemon/internal/syncer ./daemon/cmd/daemon ./daemon/cmd/agenttool
    go test ./...
    go vet ./...
    git diff --check

The Windows CI job must execute the equivalent native commands and print actual test passes; a compile-only or skipped test job is a failure.

## Validation and Acceptance

Milestone 1 passes only when Windows builds the Yrs static library, links `go-sqlite3`, compiles both binaries, and executes native primitive/config tests. Existing Linux unit tests must remain green.

Milestone 2 passes only when each required red row fails against the prior implementation and passes after the change. A remote invalid path must remain represented, materialize deterministically, and appear in daemon health. A local invalid create must stay untouched and unpublished with an actionable error. Case variants must share one owner and one lease.

Milestone 3 passes only when fake-shim tests and current real Codex/Claude smoke demonstrate protocol behavior and zero orphan descendants after stop/crash.

Milestone 4 passes only on a real Windows runner/box using NTFS and Windows fsnotify semantics. Linux cross-compilation is supporting evidence, never the final acceptance.

## Idempotence and Recovery

All build outputs belong under temporary directories or the existing ignored `third_party/y-crdt/target` and `dist` trees. Re-running Yrs/Go builds is safe. Windows tests use `t.TempDir` and isolated workspace IDs. If a rebase conflicts with another daemon program branch, abort, fetch current main, and re-apply only the milestone delta; do not preserve stale runtime or sync assumptions.

Portable materialization must be deterministic across retries and restarts. No recovery step may rename a user's local invalid file automatically. Atomic replacement always stages a sibling temp file and either replaces the destination completely or leaves the prior complete destination intact.

## Artifacts and Notes

Baseline evidence on `main@e6fa504`:

    $ env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./daemon/cmd/agenttool
    # success

    $ env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./daemon/cmd/daemon
    daemon/internal/syncer/service.go:72:30: undefined: crdt.Doc
    daemon/internal/syncer/document_cache.go:76:21: undefined: crdt.Doc
    ...

The canonical Raft implementation task is `#daemon-gui:918283c0`; independent QA is task #3 in `#daemon-gui`. Keep code/checkpoint evidence in the implementation thread and QA evidence in Deniz's task thread.

## Interfaces and Dependencies

Milestone 1 should end with repository-owned interfaces equivalent to:

    func (fs *WorkspaceFS) CreateEmptyOrRead(path string) (FileSnapshot, error)
    func readFileObservation(path string) ([]byte, error)
    func replaceFileAtomically(path string, content string, mode os.FileMode) error
    func statFileWithIdentity(path string) (os.FileInfo, fileIdentity, error)
    func fileIdentityForPath(path string) fileIdentity
    func isCrossDeviceError(err error) bool

Windows implementations use `golang.org/x/sys/windows`, already an indirect dependency in `go.mod`; promote or update it only as required by the APIs. Unix implementations use `syscall` or `golang.org/x/sys/unix`, preferring the smallest change that preserves current behavior.

Milestone 2 must define separate shared and host key functions rather than one ambiguous normalizer. Exact names may change during lead review, but their contracts must remain distinct:

    func portableWorkspacePathKey(path string) (string, error)
    func materializePortableWorkspacePath(desiredPath, documentID string) (materialized string, warning *DaemonHealthItem)
    func hostPathKey(path string) string

Milestone 3 must centralize command and process ownership so drivers do not contain platform branches:

    type providerExecutable struct {
        Path string
        Kind providerExecutableKind // native or batchShim
    }

    type providerProcessFactory interface {
        Start(providerExecutable, []string, providerProcessOptions) (providerProcess, error)
    }

    type providerProcess interface {
        PID() int
        Stop() error
        Wait() error
    }

Revision note (2026-07-12): created the initial self-contained plan after reproducing the Windows build red, auditing the current platform/process/config sites, and incorporating Bill's CLI-only boundary plus Thomas's portable-path contract. Updated it after the red-first Phase 1 implementation, the final prompt-file/suspended-Job provider contract freeze, and the native Windows run invalidated the target byte-lock protocol.

Revision note (2026-07-13): replaced the amended `MoveFileEx` design after exact-head native Windows testing showed that it still conflicts with delete-shared observations. Recorded the `FileRenameInfoEx` replace/POSIX contract, its Windows 10 v1607+ boundary, the required trailing UTF-16 terminator, and the direct plus stress evidence that promoted the prototype into the Milestone 1 implementation.
