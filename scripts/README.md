# scripts/

Build, deploy, and test helper scripts. This note documents the one cross-cutting convention every
script (and every human/agent working on the box) is expected to follow.

## Test-artifact tmp discipline

Test harnesses and throwaway worktrees write scratch state to disk. Left unmanaged it accumulates —
on 2026-07-06 the box's root filesystem hit 100% and broke tooling for multiple agents. The rule that
prevents a repeat has two halves:

1. **Everything scratch lives under one root.** `scripts/lib/testtmp.sh` defines it:
   - `notty_test_tmp_root` → `${NOTTY_TEST_TMP_ROOT:-${TMPDIR:-/tmp}/notty-test}`
   - `notty_test_mktemp <label>` → creates and prints a fresh per-run dir under that root.

   Harnesses source the lib and use the helper instead of `mktemp -d /tmp/...` directly (see
   `test-daemon-installer.sh`, `test-daemon-uninstall.sh`, `build-daemon-release.sh`). **If you spin
   up a worktree or scratch dir by hand, put it under the root too** — e.g.
   `git worktree add "$(sh -c '. scripts/lib/testtmp.sh; notty_test_mktemp worktree')" <branch>` —
   so the sweep can reclaim it when you abandon it.

2. **A sweep reclaims what outlived its run.** `scripts/sweep-test-tmp.sh` removes entries under the
   root older than `NOTTY_TEST_TMP_MAX_AGE_HOURS` (default 24):

   ```sh
   scripts/sweep-test-tmp.sh                      # reclaim >24h scratch
   NOTTY_SWEEP_DRY_RUN=1 scripts/sweep-test-tmp.sh   # show what it would remove
   NOTTY_TEST_TMP_MAX_AGE_HOURS=6 scripts/sweep-test-tmp.sh
   ```

   It only ever touches entries **under the dedicated root** — never generic `/tmp/notty-*` — so it
   cannot eat an active worktree, a deploy config, or anything outside the convention. A run's dir
   mtime advances while it writes, so an in-progress run is not eligible until it has been idle for
   the full window.

For hands-off hygiene, wire the sweep to a timer the same way the weekly `docker system prune` timer
is set up (tech-debts task #9); a daily `sweep-test-tmp.sh` is enough at current volume.

## macOS desktop release

`build-macos-desktop-release.sh <version> [dist-dir]` must run on macOS with
Go 1.26.5, Rust/Cargo 1.97.0, the x86_64 and arm64 Apple Rust targets, Xcode
command-line tools, a Developer ID Application identity, and a notarytool
Keychain profile. Signed releases require:

```sh
CODESK_MACOS_SIGN_IDENTITY='Developer ID Application: Example Corp (TEAMID)' \
CODESK_MACOS_NOTARY_PROFILE='codesk-notary' \
scripts/build-macos-desktop-release.sh 1.2.3
```

The native build entry point builds the same universal application in explicit
unsigned, construction-only mode:

```sh
make macos-gui-build
make macos-gui-build GUI_VERSION=1.2.3
```

The signed release entry point is `make macos-gui-release GUI_VERSION=1.2.3`;
provide both signing variables documented above. `MACOS_GUI_UNSIGNED=1` remains
an explicit construction-only escape hatch. Both human-facing targets fail
before construction on a non-macOS kernel; the release script still owns every
toolchain, signing, notarization, and source-cleanliness check.

The script signs the universal nested helper before `Codesk.app`, enables the
hardened runtime, notarizes and staples the app, produces and notarizes the
drag-to-Applications DMG, and writes a source-bound `manifest.json` plus
`SHA256SUMS`. `ALLOW_UNSIGNED_MACOS_DESKTOP=1` exists only for construction
debugging and cannot produce publishable or trust evidence.

The desktop credential uses the standard per-user login Keychain so the same
runtime path works without an Apple provisioning profile. The token remains in
the operating-system-protected credential store and is never written to a
plaintext file, but this mode does not provide Keychain access-group app
isolation. A future move to the data-protection Keychain requires an Apple
Developer App ID, provisioning profile, matching entitlements, and signed
native validation.

Independently recheck a release on macOS with:

```sh
scripts/verify-macos-desktop-release.sh dist/macos-desktop/1.2.3 1.2.3
```

That verifier repeats universal Mach-O, bundle inventory, metadata, icon,
signature, notarization ticket, Gatekeeper, DMG, checksum, and mounted app-tree
checks. The manifest selects the obligations: a `signed_and_notarized=true`
claim always executes every trust check even when unsigned relaxation is set;
an unsigned claim requires explicit relaxation and produces unmistakably
construction-only output. It does not exercise runtime behavior.

## Windows desktop build and release

The Windows GUI payloads reuse the same locked Rust bridge, Zig cross-compiler,
PE subsystem flags, and PE verifier as CI. The human-facing build target is
native-only, builds exactly the architecture reported by the Windows runtime,
and fails before tool execution on every non-Windows kernel. GNU Make users on
Windows can use the Make target without `uname` or a POSIX recipe shell;
ordinary PowerShell users use the root shim without installing Make:

```sh
make windows-gui-build
```

```powershell
.\make.ps1 windows-gui-build
```

The build architecture is not supplied by a Make variable: the shared
PowerShell orchestrator resolves and enforces the real Windows host
architecture. `WINDOWS_GUI_ARCHES` defaults to `amd64 arm64` for releases. CI
and explicit cross-compile users invoke the honestly named
`make windows-gui-payloads` target or the shared script directly from any host
with Go, Rust/Cargo, the required Rust targets, and Zig 0.16.0. Native Windows
entry points also require Git for Windows because that shared payload script is
the single cross-toolchain implementation:

```sh
WINDOWS_GUI_ARCHES="amd64 arm64" \
WINDOWS_GUI_SAFE_PARENT_DIRECTORY="$RUNNER_TEMP" \
scripts/build-windows-desktop-payloads.sh \
  "$RUNNER_TEMP/windows-desktop-payload" \
  "$RUNNER_TEMP/windows-desktop-tests"
```

The two positional arguments are the payload and compiled-test output roots.
The script requires both to resolve as distinct, non-overlapping, nonsymlink
children of `WINDOWS_GUI_SAFE_PARENT_DIRECTORY`, then cleans both before
compiling. Cross-target Yffi staging is transactional: the script restores the
exact pre-build host `libyrs.a` bytes on success, failure, or interruption, and
removes the staged archive when no host archive existed. The release entry
point builds both payload architectures and passes each to the reproducible WiX
builder, which links the requested release twice and runs ICE validation:

```sh
make windows-gui-release GUI_VERSION=1.2.3
```

```powershell
.\make.ps1 windows-gui-release GUI_VERSION=1.2.3
```

That target fails closed unless it is running on real Windows; Linux, macOS,
Wine, and WSL do not produce a release claim. It also requires a clean source
checkout and a canonical numeric `GUI_VERSION` in the MSI range (major and
minor at most 255, build at most 65535) before compiling. It produces exactly
one MSI per requested architecture:

```text
dist/windows-gui/msi/amd64/Codesk_1.2.3_windows_amd64.msi
dist/windows-gui/msi/arm64/Codesk_1.2.3_windows_arm64.msi
```

Each architecture directory also contains `provenance.json` and
`SHA256SUMS`. The builder derives the MSI ProductCode with UUIDv5 from its
pinned product namespace and the canonical `"<version>+<arch>"` name while
preserving the package's stable UpgradeCode. Its existing
`-PreviousProductCode`/`-CandidateProductCode` parameter set remains the
two-version QA mode used by the upgrade/reproducibility CI. Signing and
publication remain separate release-policy work.

From any host with `gh` plus `sha256sum` or `shasum`, download both
architecture bundles from a successful CI run bound to the checked-out `HEAD`
and verify their exact inventories and checksums with:

```sh
make windows-verify
make windows-verify WINDOWS_GUI_RUN_ID=123456789
```

Without an explicit run ID, the target selects the newest successful CI run
for the exact checked-out commit. It never falls back to a stale commit or a
non-successful run.

### Native acceptance

`test-macos-desktop-native.sh` is the destructive runtime harness. Run it in a
dedicated GUI test account twice on an Intel Mac and twice on an Apple Silicon
Mac: `prepare`, then a real logout/login, then `resume`. It requires a previous
release and the exact candidate release so replacement-upgrade behavior is real
rather than a same-build reinstall. By default both releases must be signed and
notarized. When Apple credentials are unavailable,
`CODESK_MACOS_ACCEPT_UNSIGNED_FUNCTIONAL=1` permits the same functional rows
with manifests explicitly marked unsigned. That mode records
`evidence_scope=native-functional-only`, `artifact_trust=NOT_ESTABLISHED`, and
`publishable=false`; it cannot establish signing, notarization, stapling,
Gatekeeper acceptance, trust, or publishability. The candidate manifest source
SHA must equal the clean checkout's `HEAD`. The evidence directory must be
outside that checkout so untracked source can never affect the verifier used
for the run. Prepare copies the verified candidate DMG into a private read-only
evidence snapshot. Resume revalidates that snapshot's exact hash and size at
installation, and the installed app tree must equal the persisted manifest tree.

The harness requires three external environment drivers because browser
authentication, remote workspace mutation, and starting a real provider belong
to the acceptance environment, not production test hooks:

- `CODESK_MACOS_CONNECT_DRIVER` completes the browser handoff after the harness
  clicks **Connect...**.
- `CODESK_MACOS_SYNC_DRIVER` plans, then creates, a pairwise-unique remote file
  for the requested `initial`, `upgrade`, or `restart` stage.
- `CODESK_MACOS_PROVIDER_DRIVER` starts a real provider descendant for the
  abnormal-exit containment row.

Every driver receives `CODESK_ACCEPT_STAGE`, `CODESK_ACCEPT_ACTION`,
`CODESK_ACCEPT_RUN_ID`,
`CODESK_ACCEPT_APP_PATH`, the three native directory variables, and
`CODESK_ACCEPT_RESULT_DIR`. Connect and provider drivers receive action `run`
and must write a non-secret `receipt.txt`. A sync driver first receives action
`plan`; without mutating the remote workspace it writes `relative-path`, the
lowercase 64-character `sha256`, and `receipt-plan.txt`, then exits. The harness
rejects reused or locally present paths. Only then does action `trigger` receive
the immutable plan again in `CODESK_ACCEPT_RELATIVE_PATH` and
`CODESK_ACCEPT_SHA256`; it performs the remote mutation and writes
`receipt-trigger.txt`. The plan files must remain unchanged while the harness
observes the file appear locally.

The provider driver must additionally write `provider-pid`, the absolute real
`provider-executable`, and its lowercase `provider-executable-sha256`. The
harness proves the PID did not exist before the trigger, its OS start time is
not earlier than the trigger, the inspected executable path/hash match the
claim and are not a Codesk binary, and its live parent chain reaches the Codesk
controller before applying the PGID and post-crash disappearance assertions.
Driver stdout/stderr is captured and scanned for the exact Keychain secret;
drivers must never print credentials.

Example invocation shape:

```sh
export CODESK_MACOS_ACCEPT_DESTRUCTIVE=1
export CODESK_MACOS_CONNECT_DRIVER=/opt/codesk-test/connect-driver
export CODESK_MACOS_SYNC_DRIVER=/opt/codesk-test/sync-driver
export CODESK_MACOS_PROVIDER_DRIVER=/opt/codesk-test/provider-driver
# For explicitly non-trusted unsigned functional evidence only:
# export CODESK_MACOS_ACCEPT_UNSIGNED_FUNCTIONAL=1

scripts/test-macos-desktop-native.sh prepare \
  /releases/1.2.2 1.2.2 /releases/1.2.3 1.2.3 /evidence/codesk-1.2.3-intel
# Log out and back in. Do not launch Codesk manually.
scripts/test-macos-desktop-native.sh resume \
  /releases/1.2.2 1.2.2 /releases/1.2.3 1.2.3 /evidence/codesk-1.2.3-intel
```

The terminal running the harness needs Accessibility access so System Events
can click and inspect the native status menu. The prepare phase waits for login
item approval in **System Settings > General > Login Items**, then explicitly
quits Codesk and joins its process group before the operator logs out. The
resume phase therefore proves auto-launch from an absent-at-logout process
before it launches anything itself. Driver and second-instance commands are
bounded by `CODESK_MACOS_ACCEPT_TIMEOUT`; LaunchServices failures count only
when they are explicit multiple-instance outcomes. The altered-`HOME` direct
launch must still resolve account-record paths, and candidate restart must
advance the logged online service generation while preserving the app pid. The
remaining rows prove second-instance exclusion, Keychain-only token storage,
causally planned remote sync on pairwise-unique absent paths before/after restart
and upgrade, no Dock/window, normal joined Quit, fail-closed watchdog death,
identity-bound real-provider cleanup, exact-snapshot replacement upgrade,
app-only uninstall, preserved desktop data, and unchanged legacy `~/.notty`
state. The resulting evidence directory contains no secret;
retain its transcript, driver receipts, hashes, and `result.txt` for the exact
PR head. User data and Keychain state remain intentionally preserved after the
app is removed and should be cleaned only after evidence capture.
