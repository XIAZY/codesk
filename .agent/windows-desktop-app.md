# Build and package the native Windows Codesk desktop app

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This repository contains `.agent/PLANS.md`. Maintain this document in accordance with that file.

## Purpose / Big Picture

After this change, a Windows user can run one signed Codesk setup program for AMD64 or ARM64, install the app for the current user without elevation, connect it to a Codesk workspace in a browser, and keep the workspace daemon running from a native tray. The app has no console window, protects its credential with Windows Data Protection API (DPAPI), launches at login when enabled, rejects a second instance across Terminal Services sessions, and terminates its entire child process tree when the desktop process dies unexpectedly. The application coordinator is an untagged, host-testable package shared with the macOS composition; Windows files contain only Windows adapter construction and bootstrap policy.

Setup replaces or removes the installed program only while the desktop is stopped. It preserves legacy and desktop user data, removes exact legacy launchers, and uses durable records so an interrupted install, upgrade, or uninstall converges to either the complete old state or the complete committed new state. The release builder emits deterministic dual-architecture artifacts from pinned Go, Rust, Zig, and resource-generator inputs. Portable tests and Linux ARM64 cross-construction do not substitute for the final native Windows AMD64 and ARM64 lifecycle and crash/restart matrix.

## Progress

- [x] (2026-07-15 09:34Z) Created `feat/windows-desktop-app` from desktop-controller merge `aafe1ad8d3b83e846bc7e749572e44962c9a3f69` and mapped the shared desktop, syncer, legacy installer, and release surfaces.
- [x] (2026-07-15 10:34Z) Implemented token-free configuration, DPAPI-backed secrets, the Windows tray composition, deterministic Codesk icon resources, strict self-extracting payload parsing, and real AMD64/ARM64 syncer links.
- [x] (2026-07-15 11:57Z) Implemented the first transactional setup/release version and published draft PR #172 at historical head `7fe9e0baa541f4936aaab02b4f7b83215dd9ebf3` for exact-head review.
- [x] (2026-07-15 13:35Z) Folded review blockers locally: nonzero setup failures, shared per-user file locks, synchronous external versioned uninstallers, fail-if-desktop-active setup, durable install/uninstall commit proofs, complete registration snapshots, a kill-on-close Job Object, one shared URL validator with causal tests, early version validation, exact toolchain pins, and forbidden Go VCS metadata checks.
- [x] (2026-07-15 14:06Z) Closed the transaction cleanup crash window: synced JSON is published by write-through rename, orphan commit proofs replay their desired registration state before deletion, and install/uninstall tests cover pre-proof rollback, every committed cleanup crash point, recovery retry, and recovery-twice idempotency.
- [x] (2026-07-15 14:06Z) Cross-compiled the real desktop and setup test binaries for Windows AMD64 and ARM64 after the blocker fold, then restored the host Yrs library. This is construction evidence only.
- [x] (2026-07-15 14:06Z) Rewrote this plan to match the amended design and removed stale claims about local mutexes, detached uninstall workers, forced desktop termination, old artifact hashes, and cryptographic Authenticode verification.
- [x] (2026-07-15 14:52Z) Ran the final race/full-repository gates and full unsigned dual-architecture release construction after the exact-handle cleanup change. Go test/race/vet/build, frontend typecheck/354 tests/build, shell/diff checks, both setup target compiles, PE verification, host Yrs restoration, and generated-resource cleanup are green. The inherited manual-reset-handle test is construction evidence for helper ordering only, not the native process-lifecycle seal.
- [x] (2026-07-15 15:20Z) Ran the first dirty-outer-repository, two-linked-worktree proof with spaces in one checkout path. It correctly failed before push: both desktop binaries differed only in the Go build ID plus the derived deterministic PE timestamp/hash, while the agent tools, icons, and all remaining desktop bytes were identical. The builder now clears published Go build IDs and remaps the complete Rust temporary tree, including Cargo target and `OUT_DIR`, to a canonical prefix.
- [x] (2026-07-15 15:25Z) Re-ran both complete unsigned builds after the canonicalization fix. The two setup executables, manifest, checksums, and both extracted payload directories are byte-identical across the dirty outer repositories and spaces-in-path checkout. Printable-string sweeps contain only relative Rust source paths and canonical `/build/cargo-home` registry paths, with no real checkout, Go temporary, agent-home, or Zig cache path.
- [x] (2026-07-15 16:59Z) Published the reviewed process-exit amendment and rebased its eight commits without content drift onto current main `8789ef92981f87bd9d5ff40fd9a2c76b508a733b`, producing historical pre-architecture-amendment head `cf1a42d207b3151c2e3d5d10b43cd4a1592f923b`.
- [x] (2026-07-15 17:44Z) Accepted the cross-platform architecture correction: the 337-line application coordinator must move out of the Windows-tagged command and gain Linux-hosted behavior tests; genuinely native adapters, setup, signing, and packaging remain separate.
- [x] (2026-07-15 20:20Z) Landed the untagged coordinator, Linux-hosted behavior tests, thin Windows composition root, and invariant-first `docs/desktop-architecture.md` through shared PR #173 at exact head `c7b376dd9a50192f7504f79eb984aee1b59063f2`.
- [x] (2026-07-16 02:38Z) Verified that merged macOS PR #174 consumes the same coordinator, then released the Windows rework hold on current main `7ae7ef3451ede28bf2ee4651dabcee084b072fd1`.
- [x] (2026-07-16 03:40Z) Reconstructed, committed, and published the canonical 52-path Windows-only remainder directly on current main with no shared PR #173 paths, after passing focused race, full Go test/vet/build, frontend typecheck/354 tests/build, diff/actionlint, AMD64/ARM64 construction, complete unsigned release build, PE verification, host Yrs restoration, and generated-resource cleanup.
- [x] (2026-07-16 04:02Z) Completed a whole-document currency sweep after publication: reconciled the merged macOS wording, publication state, milestone statuses, exact focused-race and cross-construction commands, historical-head wording, and revision note without embedding the source commit's own SHA.
- [x] (2026-07-17 06:20Z) Withdrew the merge recommendation and returned PR #172 to draft after the first native Windows build repeatedly flashed PowerShell/console windows. Traced every production child constructor: transactional cleanup covers the historical `Codesk daemon <workspace>` five-second launcher, installer PowerShell already uses hidden/no-window policy, and the recurring uncovered class is the five Codex/Claude detection/runtime constructors in `daemon/internal/syncer`.
- [x] (2026-07-17 07:05Z) Transplanted the canonical 52-path Windows remainder onto current main `1a2a467ff4e1507ae55262a928dec1ba9bc69aff`, preserving the merged app-server lifecycle fixes, and routed all five syncer-managed background children through one platform policy. Windows sets `CREATE_NO_WINDOW` and `HideWindow`; other platforms are unchanged; context, arguments, streams, and exit status are preserved. Added an aliased-import-aware production AST inventory, portable Windows-policy source guard, native Windows attribute test, exact constructor semantics test, and a separate detached-installer attribute seam/test. Removing either Windows flag or bypassing the factory makes the committed portable rows fail.
- [x] (2026-07-17 07:45Z) Added the token-free runtime acceptance stream and source-bound release metadata requested by the independent QA runner. Runtime records expose only service/observation/runtime/turn sequences, runtime kind, PID, and bounded lifecycle state; duplicate provider starts are suppressed and a portable crash/replacement row proves generation disambiguation under PID reuse. The Windows builder now requires a clean committed checkout, records its full lowercase `source_revision` beside both setup hashes, and includes the manifest hash in `SHA256SUMS`; generator and verifier mutations reject missing, zero, uppercase, or changed bindings.
- [x] (2026-07-17 07:54Z) Re-ran focused syncer plus desktop-app race, complete repository Go test/vet/build, frontend typecheck plus 357 tests/build, shell syntax, actionlint, and release metadata/verifier gates on the amended source.
- [x] (2026-07-17 09:28Z) Replaced duplicated Windows/macOS release policy and metadata mechanics with one strict `desktoprelease` package and clean-checkout shell helper, then moved durable token-free configuration and DPAPI file state into a directly consumed `desktopstate` package. Concrete file stores expose only exact-byte hash/size/presence checkpoints; the shared secret interface and macOS Keychain remain unchanged. Full host gates, focused race, canonical-metadata mutations, the CGO-free state boundary, and Windows AMD64/ARM64 desktop vet/test linking are green; host Yrs was restored.
- [x] (2026-07-17 09:52Z) Closed the pre-publication root-trust finding: Windows now resolves the current account's Local AppData through `FOLDERID_LocalAppData` and never falls back to mutable `LOCALAPPDATA`, `APPDATA`, `USERPROFILE`, or `HOME`. One resolved `dirs.Data` feeds the singleton lock, configuration store, and DPAPI store. A portable composition/source guard and Windows-tagged forged-environment row require the canonical root and no redirected-root creation. Full Go test/vet/build, focused race, and real AMD64/ARM64 Windows desktop test linking plus vet are green; host Yrs was restored.
- [ ] Freeze and publish one exact clean source revision, then construct and externally verify its deterministic AMD64/ARM64 unsigned bundle and attach the source/manifest/Setup hashes to the review and native handoff without editing the bound source afterward.
- [ ] Have real Windows AMD64 and ARM64 owners run the native lifecycle, cross-session lock, abnormal-death process-tree, exact registration recovery, and injected durability-failure matrix. Do not close the task from cross-compilation alone.

## Surprises & Discoveries

- Observation: a `Local\\` named mutex does not exclude the same user in another Terminal Services session.
  Evidence: Windows local kernel-object namespaces are session-scoped. Both setup and desktop now open the same `%LOCALAPPDATA%\\Codesk\\Locks\\desktop.lock` with share mode zero, and setup additionally holds `setup.lock` to serialize setup processes.

- Observation: a commit proof can outlive its transaction record if the process dies between deleting the record and deleting the commit during cleanup.
  Evidence: discarding that orphan proof would lose the only durable desired registration state. Recovery now reads and reapplies the orphan commit, retains it when restoration fails, deletes it only after success, and becomes a no-op on a second recovery.

- Observation: a failed directory-move call does not prove that no filesystem mutation occurred, and a failed proof-publication call does not prove that no final proof exists.
  Evidence: install/uninstall rollback now derives its action from the observed install, backup, and tombstone topology even when the in-memory phase flag was never advanced. Commit failure reopens and compares the final proof: an absent proof permits rollback, while an exact or uninspectable final proof fails closed, suppresses rollback/cleanup, and leaves forward recovery authoritative.

- Observation: writing and syncing a JSON file in place does not make publication ordering as explicit as a synced temporary file followed by a write-through rename.
  Evidence: transaction record and commit publication now uses a fixed `.publishing` sibling, `Sync`, close, and `MoveFileExW` with `MOVEFILE_WRITE_THROUGH`. Recovery removes an incomplete publication before interpreting records.

- Observation: Windows registry value names compare without case, while Go byte sorting is case-sensitive.
  Evidence: registration-state validation rejects case-folded duplicate uninstall values as well as unsorted and byte-identical duplicates, so replay cannot silently collapse two serialized entries into one Windows value.

- Observation: Go build information records the enclosing Git repository unless every release-producing Go command disables VCS stamping.
  Evidence: the builder now uses `-buildvcs=false` for the release tool, icon generator, resource generator installation, desktop, agent tool, and setup. The release verifier reads every Go PE's build information and rejects any `vcs.*` setting.

- Observation: exact compiler versions are not enough when ambient build flags or repository paths can change output.
  Evidence: the builder disables Go workspace/user configuration, pins `GOAMD64=v1` and `GOARM64=v8.0`, isolates Cargo configuration, supplies exact Rust flags rather than inheriting `RUSTFLAGS`, remaps the repository and complete Rust temporary tree, and fixes locale, timezone, and source-date inputs.

- Observation: Go `-trimpath` removes source paths but does not guarantee a location-independent external-link build ID.
  Evidence: the first two-worktree proof confined every difference in each `Codesk.exe` to two 20-byte Go build-ID segments and two copies of the four-byte deterministic PE content hash; 46 individual byte positions differed because two build-ID characters happened to coincide. Every other desktop byte matched, and the embedded agent tools and icons already matched. Published Go executables now use an empty build ID, while the full Rust temporary root is remapped to `/build` so Cargo target, build-script `OUT_DIR`, and sibling temporary paths cannot enter output by location.

- Observation: Go's `-H=windowsgui` did not override the subsystem when Zig performed the external link, and Zig's default CodeView record made otherwise identical builds differ.
  Evidence: `-Wl,--subsystem,windows` selects the GUI subsystem and `-Wl,-s` removes the varying PDB/CodeView record. The verifier rejects the wrong subsystem.

- Observation: a PE certificate table can include up to seven zero padding bytes inside `WIN_CERTIFICATE.dwLength`, and the certificate table is appended after the setup payload.
  Evidence: payload discovery accepts its footer only at unsigned EOF or immediately before bounded certificate alignment, while the certificate parser accepts only bounded zero DER padding and requires the SignedData/AuthentiCode structural OIDs.

- Observation: structural Authenticode parsing is not publisher authentication.
  Evidence: this repository can prove PE certificate-table and PKCS#7/AuthentiCode shape, but not signature mathematics, publisher identity, chain/EKU policy, revocation, or timestamp trust. A Microsoft-signed fixture exercises parser compatibility only. The external signer and Windows trust APIs own authenticity.

- Observation: a PID is not a durable identity for an unbounded post-exit wait because Windows may reuse it before a helper opens the process.
  Evidence: setup opens an inheritable `SYNCHRONIZE` handle to its current process before creating PowerShell, lists only that handle in `AdditionalInheritedHandles`, and the helper waits indefinitely on that inherited handle without resolving a PID. The manual-reset-handle integration row proves helper-side wait/delete ordering; only the separate native production-creator row can seal real parent-termination signaling.

- Observation: linking the desktop itself as `windowsgui` does not suppress consoles allocated by background console/npm-wrapper children created with default process attributes.
  Evidence: the first native desktop build repeatedly flashed PowerShell/console windows. Install/shortcut PowerShell already sets `CREATE_NO_WINDOW` plus `HideWindow`, and detached self-delete cannot explain recurring steady-state flashes. The uncovered recurring surface was exactly Codex `--version`, Codex `app-server --help`, Codex `app-server`, Claude `--version`, and the Claude runtime, all constructed with plain `os/exec`. Controller detection/retry and agent starts exercise those constructors repeatedly.

## Decision Log

- Decision: use a native self-extracting setup executable rather than introduce MSI authoring.
  Rationale: the setup remains per-user, dual-architecture, self-contained, and able to enforce the repository's exact legacy cleanup and recovery rules without an administrator-only toolchain.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: serialize setup with a shared file lock and refuse to mutate while the desktop file lock is held.
  Rationale: setup must not kill the active desktop and race its tray/controller state. The same per-user path works across Windows sessions, unlike a `Local\\` named mutex. The user receives a nonzero "quit Codesk and run setup again" result.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: assign the desktop process to an unnamed, non-inheritable Job Object configured with only `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` before constructing the app or syncer.
  Rationale: Windows automatically places descendants in the job. Keeping the sole job handle for process lifetime makes abnormal desktop death terminate child and grandchild processes without broad process-name matching.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: install a signed, versioned uninstaller at `%LOCALAPPDATA%\\Codesk\\Setup\\<version>\\Uninstall Codesk.exe` and run uninstall synchronously.
  Rationale: the uninstaller is outside `%LOCALAPPDATA%\\Programs\\Codesk`, so it can tombstone and remove the program tree while returning the real result to Apps & Features. Only final removal of the external setup tree is asynchronous, after the uninstaller process exits.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: bind final setup-tree deletion to an explicitly inherited process handle rather than a parent PID or timeout.
  Rationale: the normal success dialog may remain open indefinitely, and a PID can be reused before a detached helper resolves it. An inherited `SYNCHRONIZE` handle is the exact waitable identity; the helper cannot delete early or attach to an unrelated reused PID.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: treat the commit proof as the complete intended post-state and make recovery converge forward to it.
  Rationale: before proof, recovery restores the old program and exact prior registration. After proof, recovery re-applies the exact committed Run value/type, shortcut bytes, and all raw uninstall-key values, completes file cleanup, and retains proof on any failure. This avoids partial-step replay and makes repeated recovery idempotent.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: production construction fails without an external signer; explicit unsigned mode is construction-only.
  Rationale: CI and this Linux seat do not own the release certificate. The builder signs desktop and agent payload files, creates setup, then signs setup last so the signature covers the appended payload. The external signer must perform cryptographic verification and timestamp policy.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: pin Go `go1.26.5`, Rust `1.97.0`, Cargo `1.97.0`, Zig `0.16.0`, and `go-winres` `v0.3.1`, and record them in the release manifest.
  Rationale: reproducibility must be independent of enclosing repositories, dirty state, user Go/Cargo configuration, and compiler upgrades. The verifier checks the manifest and Go build information rather than trusting builder narration.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: use an empty Go build ID in every published Go executable and remap the entire Rust build temporary root to `/build`.
  Rationale: the external CGO/Zig link action ID includes build-location inputs even under `-trimpath`, and isolating Cargo only relocates paths. Clearing that non-product identifier plus canonical Rust source, Cargo home, target, and `OUT_DIR` prefixes makes the released bytes location-independent while the explicit release version and manifest retain provenance.
  Date/Author: 2026-07-15, Vitaliy.

- Decision: move the application owner/action loop into one untagged `daemon/internal/desktopapp` package, while keeping native adapter construction and installer models platform-specific.
  Rationale: controller ownership, menu publication, connection commit/restart, login-item toggling, action routing, and joined shutdown have no Win32 dependency and must not be copied into the Darwin app. DPAPI versus Keychain, HKCU Run versus `SMAppService`, Windows Job Objects versus macOS process ownership, ShellExecute versus `NSWorkspace`, and the two installation/release models satisfy common invariants through incompatible operating-system APIs. A generic installer abstraction would erase important recovery and trust differences instead of sharing behavior.
  Date/Author: 2026-07-15, Vitaliy, following @AlphaToad's architecture direction.

- Decision: construct every syncer-managed background child through one syncer-owned platform process policy, while retaining desktopsetup's detached process policy as a separate lifecycle boundary.
  Rationale: probes and long-lived Codex/Claude runtimes share the invariant that a native GUI owner must never expose a console, but they retain distinct context and stream semantics. On Windows the factory adds `CREATE_NO_WINDOW` and `HideWindow` without disabling standard-handle inheritance; other platforms are a no-op. Installer self-delete needs `DETACHED_PROCESS`, an exact inherited wait handle, and process release instead; Windows explicitly ignores redundant `CREATE_NO_WINDOW` when `DETACHED_PROCESS` is present. Production AST inventory plus portable and Windows-specific policy tests make bypasses and flag removal causal failures.
  Date/Author: 2026-07-17, Vitaliy, following native defect triage with @AlphaToad, @Bill, @Thomas, and @Deniz.

## Outcomes & Retrospective

The cross-platform architecture correction is accepted on main through PR #173, and merged macOS PR #174 consumes the same shared coordinator. This amended branch is the canonical Windows-only remainder: shared locks prevent concurrent state owners, setup reports real failures, external uninstall is synchronous, committed transactions carry complete registration state, rollback follows observed topology, uncertain proof publication fails closed into forward recovery, desktop descendants are job-contained, final setup deletion waits on an exact inherited process handle, URL validation is shared and causally tested, and release inputs are explicit. The full portable/race/frontend gates and complete unsigned AMD64/ARM64 construction are green on current main.

The work is not accepted yet. Native Windows use exposed a release-blocking recurring console-flash defect after the prior source/CI seal, so PR #172 is draft and every earlier merge recommendation is withdrawn. The class-level syncer launch-policy correction and causal tests are implemented locally on current main; fresh complete construction, exact-head reviews/CI, and task #42 native Windows AMD64/ARM64 evidence remain. Native evidence must correlate executable/PID creation with zero visible consoles and prove the app owns exactly one daemon after idempotent legacy-launcher removal.

## Context and Orientation

The Go daemon lives under `daemon/`. `daemon/internal/syncer` is the long-running workspace synchronization service; its managed-process factory owns platform process attributes for every Codex/Claude probe and runtime child. `daemon/internal/desktopstate` owns token-free durable configuration, the cross-platform secret-store contract, private file mechanics, and the Windows DPAPI-backed concrete store. `daemon/internal/desktop` owns the controller, connection handoff, tray model, platform interfaces, the Windows file lock adapter, and Job Object containment. `daemon/internal/desktopapp` owns the platform-neutral application coordinator: durable-state loading, controller/service construction, menu/action publication, connection commit/restart, login-item toggling, and joined shutdown. `daemon/cmd/codesk-desktop` is the Windows GUI composition root. It acquires the desktop lock and joins the Job Object before constructing `desktopapp.Application` from directly imported state and native adapters.

The app's mutable user data remains under the current account's native `FOLDERID_LocalAppData\\Codesk` root; installed programs use `FOLDERID_UserProgramFiles\\Codesk`; the external versioned uninstaller lives under the data root's `Setup` directory. The product resolves these roots through Windows Known Folder APIs and never through mutable process environment. Ordinary uninstall preserves the account-local `Codesk` user data except for the setup subtree's own final self-removal, and preserves the native-profile `.notty` legacy data.

`daemon/internal/desktopsetup` owns payload parsing plus install, upgrade, recovery, and uninstall. A same-volume staged directory and backup/tombstone are moved with Windows write-through renames. A v2 install record stores the old registration state before mutation. A separate commit proof stores the desired new state before post-commit cleanup. Registration means the HKCU Run value, Start Menu `.lnk`, and the complete HKCU uninstall key. Shortcut bytes and uninstall registry bytes/types are preserved exactly; registry writes and deletions are flushed.

`scripts/build-windows-desktop-release.sh` requires a clean committed checkout, builds Yrs for `x86_64-pc-windows-gnu` and `aarch64-pc-windows-gnullvm`, links the desktop with Zig targets `x86_64-windows-gnu` and `aarch64-windows-gnu`, builds the agent tool and setup, embeds deterministic resources, and emits setup executables plus a source-bound `manifest.json` and `SHA256SUMS`. `daemon/cmd/codesk-desktop-release` creates and verifies those artifacts; `SHA256SUMS` covers both setup executables and the manifest that binds them to the exact source revision.

## Plan of Work

Milestone one is complete. Configuration remains free of credentials, only DPAPI ciphertext enters the Windows secret file, and `daemon/internal/desktopurl` validates the workspace URL. Untagged `daemon/internal/desktopapp` and its host tests landed through PR #173; they prove action routing, connection commit/restart, menu publication, login-item toggling, and joined shutdown. This branch keeps `daemon/cmd/codesk-desktop` to Windows adapter composition: acquire `%LOCALAPPDATA%\\Codesk\\Locks\\desktop.lock`, assign the process to its kill-on-close Job Object, apply executable/PATH bootstrap, construct `desktopapp.Application`, and run the native tray. Focused race tests and both Windows target links are green.

Milestone two is complete locally. `daemon/internal/desktopsetup` verifies version and payload before creating version-derived paths, acquires `setup.lock` and the shared desktop lock, and refuses active desktop state. It preserves a complete registration snapshot, publishes the record durably, stages and swaps the program, writes and flushes registration, publishes a complete commit proof, then removes backup/record/proof in that order. Uninstall follows the same shape using a tombstone and an empty committed registration state. Rollback derives state from the observed directory topology rather than return codes. A reported proof-publication failure is reconciled against the final proof and fails closed when its outcome is not absent. Recovery rolls back only without proof and converges forward with proof, including the orphan-proof cleanup window.

Milestone three is complete for unsigned construction. The builder pins and validates all tool versions, disables ambient Go/Cargo configuration and VCS metadata, remaps Rust source paths, builds both required architectures, restores the host Yrs library, creates exact metadata, and runs the strict verifier. Signed mode still requires the external signer; unsigned mode is explicitly construction-only. The verifier's Authenticode claim remains structural.

Milestone four is at exact-head review and CI. The coherent current-main source head is published after the portable/race/full gates and both-target construction; the builder's location independence remains backed by the completed isolated two-worktree proof recorded above. Fresh reviews and enabled CI must close on the published SHA. Real Windows owners then execute the native lifecycle and crash matrix on that exact SHA under task #42.

## Concrete Steps

Run commands from the repository root in `work/notty-windows-desktop-current`.

Format and run portable logic:

    gofmt -w <all changed Go files>
    go test -race ./daemon/internal/desktopapp ./daemon/internal/desktop ./daemon/internal/desktop/handoff ./daemon/internal/desktopurl ./daemon/internal/desktopsetup ./daemon/cmd/codesk-desktop-release
    go test ./...
    go vet ./...
    go build ./...
    git diff --check

The transaction tests must name separate rows for uncommitted rollback, committed recovery before cleanup, mid-cleanup, orphan-commit recovery, recovery failure/retry, and recovery twice. The URL configuration and handoff tests must each become red independently if the shared validator stops requiring HTTP/HTTPS.

Cross-compile the platform tests with the target Yrs archive staged, then restore the host archive:

    RUST_TARGET=x86_64-pc-windows-gnu RUSTFLAGS='-C panic=abort' scripts/build-yffi.sh
    CC='zig cc -target x86_64-windows-gnu' CGO_ENABLED=1 GOOS=windows GOARCH=amd64 go test -c -o /tmp/codesk-desktop-amd64.test.exe ./daemon/internal/desktop
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o /tmp/codesk-desktopsetup-amd64.test.exe ./daemon/internal/desktopsetup
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o /tmp/codesk-desktop-setup-command-amd64.test.exe ./daemon/cmd/codesk-desktop-setup
    CC='zig cc -target x86_64-windows-gnu' CGO_ENABLED=1 GOOS=windows GOARCH=amd64 go build -o /tmp/Codesk-amd64.exe ./daemon/cmd/codesk-desktop
    RUST_TARGET=aarch64-pc-windows-gnullvm RUSTFLAGS='-C panic=abort' scripts/build-yffi.sh
    CC='zig cc -target aarch64-windows-gnu' CGO_ENABLED=1 GOOS=windows GOARCH=arm64 go test -c -o /tmp/codesk-desktop-arm64.test.exe ./daemon/internal/desktop
    CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go test -c -o /tmp/codesk-desktopsetup-arm64.test.exe ./daemon/internal/desktopsetup
    CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go test -c -o /tmp/codesk-desktop-setup-command-arm64.test.exe ./daemon/cmd/codesk-desktop-setup
    CC='zig cc -target aarch64-windows-gnu' CGO_ENABLED=1 GOOS=windows GOARCH=arm64 go build -o /tmp/Codesk-arm64.exe ./daemon/cmd/codesk-desktop
    scripts/build-yffi.sh

For unsigned construction, place Zig 0.16.0 on `PATH` and run:

    ALLOW_UNSIGNED_WINDOWS_DESKTOP=1 scripts/build-windows-desktop-release.sh dev "$PWD/dist/windows-desktop"
    scripts/verify-windows-desktop-release.sh --allow-unsigned "$PWD/dist/windows-desktop/dev" dev

To prove path and VCS independence, commit the complete source first. Create two linked worktrees below two different initialized outer repositories, use a path containing spaces for one, dirty an unrelated tracked file in each outer repository, run the full builder in both, and compare `CodeskSetup_dev_windows_amd64.exe`, `CodeskSetup_dev_windows_arm64.exe`, `manifest.json`, and `SHA256SUMS` with `cmp` and `sha256sum`. Also compare the extracted desktop and agent payloads and sweep their printable strings for absolute source, Cargo, CGO, Zig, and temporary paths. Do not reuse output from the working tree or compare partial binaries.

For production, unset the unsigned override and set `CODESK_WINDOWS_SIGNER` to an executable that accepts one file, applies the approved Authenticode certificate and timestamp, verifies the result cryptographically, and returns nonzero on any failure. The repository verifier then checks layout and self-consistency; it does not replace the signer or Windows trust validation.

## Validation and Acceptance

Portable acceptance requires exact whole-state recovery. Before a commit proof, any crash restores the prior program and the exact prior Run value/type, shortcut bytes, and raw uninstall-key values. After a commit proof, every crash point converges to the committed program and committed registration. A failed registry or file durability operation returns nonzero and leaves the record/proof for retry. Running recovery twice produces no drift and no second registration replay after proof cleanup.

Construction acceptance requires real desktop/syncer links for AMD64 and ARM64, PE machine `0x8664` and `0xaa64`, GUI subsystem for desktop/setup, console subsystem for the agent tool, required icon/version/manifest resources, exact payload hashes, the pinned toolchain manifest, a canonical full lowercase nonzero `source_revision`, a `SHA256SUMS` entry for that manifest, Go build version `go1.26.5`, and no `vcs.*` settings in setup or either embedded Go binary. All four published files must compare byte-for-byte across two clean isolated linked-worktree builds at that exact source revision.

Windows and macOS release adapters share `daemon/internal/desktoprelease` for version/source/trust policy, canonical JSON, ordered `SHA256SUMS`, exact release-root inventory, regular-file hashing, and atomic metadata writes. Platform adapters retain only their native PE/AuthentiCode/setup and Mach-O/ICNS/DMG checks. `scripts/lib/desktop-release.sh` is the sole build-script owner for clean-checkout source revision binding.

Native acceptance requires real Windows AMD64 and ARM64 transcripts bound to the frozen SHA and artifact hashes. Each architecture must install without elevation, connect and sync, persist only DPAPI ciphertext, restart, launch at login, reject a second instance, upgrade with data preserved, and uninstall with program/shortcut/Run/uninstall key gone while user data remains. A cross-session same-user process must fail to acquire the desktop lock. Setup while desktop is active must return nonzero and leave files and registration byte-for-byte unchanged.

Across startup, connect, sync, restart, and update, native executable/PID tracing must attribute every Codex/Claude probe and runtime child to the desktop-owned daemon and show zero visible PowerShell, cmd, or console windows. Existing-install upgrade must remove the exact historical `Codesk daemon <workspace>` Scheduled Task or Startup link idempotently, leave no lifecycle gap, and prove exactly one app-owned daemon with no legacy duplicate or orphan. Logs and traces must never contain tokens.

The native crash matrix must run setup as separate processes and terminate it at four points: before proof, after proof before cleanup, during cleanup, and after cleanup. Restarting setup must recover to a consistent whole, and running recovery again must be a no-op. Injected write-through and registry-flush failures must remain nonzero and recoverable. Killing the desktop owner without calling Shutdown must terminate a spawned child and grandchild before any destructive phase begins.

The native self-delete row must exercise the production creator, not the manual-handle construction fixture. A parent subprocess launches the real uninstall helper and reports ready only after child creation, remains alive with the success dialog held past 60 seconds, then exits; the harness proves the versioned Setup tree and uninstaller are eventually removed. Repeat with immediate parent exit and PID churn to prove the inherited handle cannot bind to an unrelated process. These are native OS guarantees and cannot be claimed from this Linux ARM64 seat.

## Idempotence and Recovery

The build uses a private temporary directory, remaps its complete Rust-visible path to `/build`, clears location-dependent Go build IDs in published executables, deletes generated `.syso` files, isolates Cargo configuration, and restores the host Yrs archive on success, failure, or signal. Repeating the build replaces only the requested version output after a complete verified publish directory exists.

Setup holds both locks for every recovery and mutation. Record and commit JSON are bounded, strict, unknown-field rejecting, and published from a synced `.publishing` file by write-through rename. Recovery removes incomplete publication files first. A reported publication failure is reconciled against the final proof and rollback remains disabled when the proof is exact, mismatched, or cannot be inspected. Unexpected sibling directories, symlinks, duplicate/case-folded registry names, mismatched transaction IDs, or ambiguous install states fail closed for manual recovery rather than deleting uncertain data.

An uncommitted install removes the new program and restores the backup; an uncommitted uninstall restores its tombstone. A committed operation re-applies the desired registration, removes staging/backup/tombstone, then removes record and proof. If only the proof remains, it is still replayed and retained until successful. The external uninstaller removes the full program tree synchronously; only a hidden PowerShell process waits for the uninstaller to exit and recursively removes `%LOCALAPPDATA%\\Codesk\\Setup`.

PowerShell receives only an inherited exact process `SYNCHRONIZE` handle and waits without a timeout or PID lookup before deleting the setup tree. The source and manual-reset-handle test establish construction and helper ordering; real parent termination, dialog-past-60-seconds behavior, and immediate-exit/PID-churn resistance remain native acceptance rows.

## Artifacts and Notes

The coherent Windows remainder plus class-level syncer process-policy amendment is based directly on current main after the shared coordinator, macOS composition, and daemon lifecycle merges:

    Base: 1a2a467ff4e1507ae55262a928dec1ba9bc69aff

The frozen pre-reconstruction PR head is historical and must not be used for amended acceptance:

    Historical PR #172 head: cf1a42d207b3151c2e3d5d10b43cd4a1592f923b

The earlier unsigned artifact hashes are intentionally retired because the shared extraction and current-main reconstruction change the linked application bytes. Every cross-built result remains construction evidence until native Windows owners bind their transcript to the same frozen SHA and artifacts.

The following complete current-main construction hashes are historical after the console-policy amendment and must not be used for amended acceptance:

    CodeskSetup_dev_windows_amd64.exe: e3e111a6263cbdf46d5c79cd7f40470080a416a0114ed758a077260c71643faf
    CodeskSetup_dev_windows_arm64.exe: 46fe715a413346dbafb4014c5b63c8ea762570367694cad34b3cd563e1947f0a
    manifest.json:                         4fa4832edbdbe917c7deff2c04066e73e009c72dfc02d1795d7ca1f128788852
    SHA256SUMS:                            a3df3093c22c9f4ede2938d782bb04d2975b76706f7b0a6a94bffb53109ad920

## Interfaces and Dependencies

`daemon/internal/desktop/config.go` keeps `Configuration` token-free. Both it and `daemon/internal/desktop/handoff/session.go` call `desktopurl.Valid` for the workspace URL.

Windows desktop construction uses these interfaces:

    func NewWindowsSecretStore(dataDir string) (*desktopstate.WindowsSecretStore, error)
    func (*desktopstate.FileConfigurationStore) Fingerprint() (desktopstate.Fingerprint, error)
    func (*desktopstate.WindowsSecretStore) ProtectedFingerprint(key string) (desktopstate.Fingerprint, error)
    func NewWindowsInstanceLock(path string) InstanceLock
    func NewWindowsLoginItem(valueName, executablePath string) (LoginItem, error)
    func NewWindowsShellOpener() OpenURL
    func ContainWindowsProcessTree() error

`NewWindowsInstanceLock` receives the absolute shared file path, not a mutex name. `ContainWindowsProcessTree` is called once after lock acquisition and before application construction.

The two fingerprint methods hash exact bounded persisted bytes and expose only presence, lowercase SHA-256, and size. Configuration fingerprinting first performs the normal strict semantic load and validation. DPAPI fingerprinting describes ciphertext continuity only; a legitimate DPAPI rewrite may change it without changing the token. `desktopstate.SecretStore` remains only Save/Load/Delete so the macOS Keychain adapter is not burdened with a file checkpoint.

The setup entrypoint uses:

    type Options struct {
        Version   string
        Arch      string
        Quiet     bool
        NoLaunch  bool
        Uninstall bool
    }

    func Run(context.Context, Options) error

There is no detached worker or parent PID interface. `daemon/cmd/codesk-desktop-setup/main_windows.go` converts `Run` failure to a nonzero process exit. The payload API remains `OpenPayload`, `Verify`, `Extract`, and `AppendPayload`, with exactly `Codesk.exe`, `notty-agent-tool.exe`, `codesk.ico`, and `payload.json` accepted.

Shipped programs use the Go standard library plus existing `golang.org/x/sys` and `fyne.io/systray`. Build-time resources use pinned `github.com/tc-hib/go-winres@v0.3.1`. No credential, release certificate, or signer secret enters the repository.

Plan revision note (2026-07-17 09:52Z): recorded the release-blocking native console-flash report, withdrawn merge recommendation/draft state, exhaustive legacy-plus-syncer child-process trace, current-main transplant, class-level managed-process policy, mutation-adequate inventory/flag/semantics tests, separate detached-installer policy rationale, token-free runtime acceptance stream, source-bound manifest/checksum contract, shared release/state ownership, exact persisted-byte fingerprints, native Known Folder root authority with adversarial-environment guards, retired pre-fix hashes, and the expanded native acceptance bundle for zero consoles plus exactly one app-owned daemon after legacy removal.
