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

`build-macos-desktop-release.sh [dist-dir]` must run on macOS with
Go 1.26.5, Rust/Cargo 1.97.0, the x86_64 and arm64 Apple Rust targets, Xcode
command-line tools, a Developer ID Application identity, and a notarytool
Keychain profile. Signed releases require:

```sh
CODESK_MACOS_SIGN_IDENTITY='Developer ID Application: Example Corp (TEAMID)' \
CODESK_MACOS_NOTARY_PROFILE='codesk-notary' \
scripts/build-macos-desktop-release.sh
```

The native build entry point builds the same universal application in explicit
unsigned, construction-only mode:

```sh
make macos-gui-build
```

The deployment entry point is `make macos-gui-deploy`. By default it requires
both signing variables documented above, then builds, signs, notarizes, and
uploads the versioned release plus `latest` metadata through
`scripts/upload-r2.sh`. To explicitly publish an unsigned build instead, run:

```sh
ALLOW_UNSIGNED_MACOS_DESKTOP=1 make macos-gui-deploy
```

The version is read from the root `DAEMON_VERSION` file (fail-closed). Both
human-facing targets fail before construction on a non-macOS kernel, and the
release script still owns every toolchain check. Signed mode additionally owns
the signing and notarization checks. Tracked, staged, and untracked working-tree
changes do not block either build or deploy mode.

The script signs the universal nested helper before `Codesk.app`, enables the
hardened runtime, notarizes and staples the app, produces and notarizes the
drag-to-Applications DMG, and writes a source-bound `manifest.json` plus
`SHA256SUMS`. An explicitly deployed unsigned release records
`signed_and_notarized=false` and has no signing, notarization, stapling, or
Gatekeeper trust evidence. It is publishable to R2 but remains a visibly labeled
construction-only artifact.

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

## Windows desktop build and deploy

The public Windows GUI targets consume the reusable
`alphatoad/notty:windows-builder` image and run the complete payload and MSI
toolchain in a process-isolated Windows container. The image pins Go 1.23.12,
Rust 1.97.0, Zig 0.16.0, LLVM-MinGW 20260616, .NET SDK 8.0.423, Git for Windows
2.55.0.3, and WiX SDK 4.0.5. Build products are written through the repository
bind mount and retain the existing layout under `dist/windows-gui`.

### Prerequisites

The host needs Windows 11 24H2 or newer (or Windows Server 2025) and a Docker
engine configured for Windows containers. The engine must allow process
isolation; Hyper-V isolation is not used. The checkout must include the
`third_party/y-crdt` submodule. Initialize it once if the checkout tool did not:

```powershell
git submodule update --init --recursive
```

GNU Make is optional. `make.ps1` is the Make-free entry point. Docker must be
available from the same PowerShell session:

```powershell
docker info --format '{{.OSType}} {{.Architecture}} {{.OSVersion}}'
# prints windows arm64 ... on Windows ARM64, or windows amd64 ... on AMD64
```

The first `windows-gui-build` or `windows-gui-deploy` invocation constructs the
reusable builder image automatically when it is absent. That image build
downloads Windows Server Core plus versioned tool archives over
HTTPS and restores WiX into the image's NuGet cache. Payload builds may
also download the source tree's Go modules and Cargo crates. Later image builds
reuse Docker's toolchain layers. Server Core is intentional:
Nano Server is smaller, but it omits both Windows PowerShell and `msi.dll`; the
WiX reproducibility and ICE validation path requires those operating-system
components.

Later product builds reuse the image and reject one whose Windows architecture
does not match the Docker engine. To use another local or pre-pulled tag, set
`WINDOWS_GUI_BUILDER_IMAGE`; the image-build and product-build jobs must use the
same value.

```sh
make windows-gui-build
```

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-build
```

The Dockerfile uses BuildKit's `TARGETARCH` argument to select the LTSC 2025
Server Core variant and native Go, Zig, LLVM-MinGW, .NET, MinGit, and rustup
archives. Docker's legacy Windows builder does not populate automatic platform
arguments, so the lightweight image-builder supplies only
`TARGETARCH=<Docker server architecture>` as a compatibility fallback. Image
construction and product execution both explicitly use `--isolation=process`.
The product build target does not accept `WINDOWS_GUI_ARCHES`; it builds exactly
the native container architecture. On this machine that means an ARM64
container and ARM64 payload.

Rust is the narrow exception to native host tools on ARM64. Rust's native
Windows ARM64 host uses the MSVC ABI and would require the much larger Visual
Studio Build Tools plus Windows SDK. The image instead uses Rust's self-contained
`x86_64-pc-windows-gnu` host under Windows ARM64 emulation and installs both
Windows target libraries. Go cgo uses the native LLVM-MinGW clang driver for
the selected output architecture. Zig remains available for the shared inner
script and non-container CI path, but the container supplies
`WINDOWS_GUI_CC_AMD64` and `WINDOWS_GUI_CC_ARM64` overrides because Zig 0.16.0's
native ARM64 compiler does not complete a cgo compile in process-isolated
Server Core.

CI and explicit native-toolchain users invoke the shared inner script directly
from a host that already has Go, Rust/Cargo, the required Rust targets, a POSIX
shell, and Zig 0.16.0:

```sh
WINDOWS_GUI_ARCHES="amd64 arm64" \
WINDOWS_GUI_SAFE_PARENT_DIRECTORY="$RUNNER_TEMP" \
scripts/build-windows-desktop-payloads.sh \
  "$RUNNER_TEMP/windows-desktop-payload" \
  "$RUNNER_TEMP/windows-desktop-tests"
```

The inner script also accepts `WINDOWS_GUI_CC_AMD64` and
`WINDOWS_GUI_CC_ARM64` as full compiler commands. When unset, each defaults to
the existing `zig cc -target ...` command.

The two positional arguments are the payload and compiled-test output roots.
The inner script requires both to resolve as distinct, non-overlapping,
nonsymlink children of `WINDOWS_GUI_SAFE_PARENT_DIRECTORY`, then cleans both
before compiling. Cross-target Yffi staging is transactional: the script
restores the exact pre-build host `libyrs.a` bytes on success, failure, or
interruption, and removes the staged archive when no host archive existed.

The public deploy entry point uses the same native container, requires both
payload architectures in canonical `amd64 arm64` order, passes each to the
reproducible WiX builder, then uploads both validated bundles through the shared
R2 uploader on the host:

Every R2 deploy path requires `rclone` 1.59 or newer and authenticates its
ephemeral Cloudflare S3 remote from `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, and the configured `R2_ENDPOINT_URL`. The shared
uploader does not read or write an rclone configuration file.

Run public deploy targets as single-writer operations, one publisher at a time.
Automating or parallelizing publication requires atomic R2 conditional admission
before any immutable write.

```sh
make windows-gui-deploy
```

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-deploy
```

That target fails closed unless both the host and Docker engine are real
Windows, the Docker engine architecture matches the host, and all requested
output paths remain under the repository bind mount. Linux containers, Wine,
WSL, cross-architecture images, and Hyper-V isolation do not produce a release
claim. The deploy target does not accept an architecture override: either both
canonical bundles pass preflight and publish together, or no R2 write starts.
The version is read from the root `DAEMON_VERSION` file (fail-closed) and must
be a canonical numeric value in the MSI range
(major and minor at most 255, build at most 65535) before compiling. It produces
exactly one MSI for each architecture:

```text
dist/windows-gui/msi/amd64/Codesk_1.2.3_windows_amd64.msi
dist/windows-gui/msi/arm64/Codesk_1.2.3_windows_arm64.msi
```

Each architecture directory also contains `provenance.json` and
`SHA256SUMS`. Before its first remote write, the uploader requires exactly those
three real files in each of exactly two architecture directories, verifies both
checksum inventories and actual hashes, and binds structured release provenance
to the root version, architecture, repository, and checked-out source commits.
The builder derives the MSI ProductCode with UUIDv5 from its
pinned product namespace and the canonical `"<version>+<arch>"` name while
preserving the package's stable UpgradeCode. Production and uploaded CI
artifacts always use the root `DAEMON_VERSION`. Upgrade/reproducibility CI uses the
explicit `-TestOnlyUpgradeQa` mode and versions from
`scripts/testdata/windows-msi-upgrade-versions.ps1`; those artifacts are
written under an `upgrade-qa` path, marked `publishable=false`, and never
uploaded. Deployed bundles live under `desktop/windows/<DAEMON_VERSION>/<arch>`; a
short-cache `desktop/windows/latest/manifest.json` pointer is written only
after both architecture bundles are present.

The Windows MSI CI surface was removed on 2026-07-31: its job had been disabled for want of a
Windows ARM64 runner while its artifact uploads still ran on every build, filling the repository's
artifact quota and failing unrelated CI rows. `scripts/verify-windows-gui-ci.sh` went with it — it
downloaded `windows-desktop-msi-<arch>`, which no workflow produces any more, so it could only fail
confusingly. Rebuilding MSI CI (task #21) means rebuilding its artifact handoff and its verifier
together. The builder scripts and their contracts are unchanged and still cover local and native
construction.

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
SHA must equal the checkout's `HEAD`, but tracked, staged, and untracked
working-tree changes are permitted. The evidence directory must be outside that
checkout so evidence artifacts remain isolated from source files. Prepare copies
the verified candidate DMG into a private read-only evidence snapshot. Resume
revalidates that snapshot's exact hash and size at installation, and the installed
app tree must equal the persisted manifest tree.

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
