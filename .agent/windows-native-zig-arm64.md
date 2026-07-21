# Replace the Windows daemon toolchain with Zig and ship native ARM64

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. Maintain this document in accordance with `.agent/PLANS.md`.

## Purpose / Big Picture

Codesk users on Windows should receive a daemon compiled for their machine rather than relying on x64 emulation. After this change, the canonical daemon release contains native AMD64 and ARM64 ZIP archives, and `deploy/daemons/install.ps1` selects the matching archive from the host architecture. Both Windows artifacts are built with one Zig-based C toolchain, so the repository no longer maintains separate MSYS2 or MinGW-GCC construction paths.

The behavior is visible in three ways. A focused release build produces `notty-daemon_<version>_windows_amd64.zip` and `notty-daemon_<version>_windows_arm64.zip`. The structured verifier reports PE machine `0x8664` for AMD64 and `0xaa64` for ARM64 for both executables in each archive. On a real Windows ARM64 machine, the installer downloads the ARM64 filename and the installed daemon starts and completes a sync smoke test without x64 emulation.

## Progress

- [x] (2026-07-14 17:31Z) Reproduced the missing Windows ARM64 mapping and inventoried the hard-coded MinGW, AMD64-only release, and Windows 11 emulation paths.
- [x] (2026-07-14 17:47Z) Added Zig targets for Windows AMD64 and ARM64, Rust Yrs targets for both architectures, and native ARM64 release packaging.
- [x] (2026-07-14 17:54Z) Resolved Rust static-library unwind linkage for both Windows targets and constrained targeted Yrs builds to the static library consumed by Go.
- [x] (2026-07-14 17:59Z) Built both canonical archives and verified manifests, SHA256 entries, ZIP contents, and both executable PE machine types.
- [x] (2026-07-14 18:05Z) Added installer architecture selection and deterministic AMD64/ARM64 routing assertions.
- [x] (2026-07-14 18:10Z) Passed Windows cross vet and syncer test-binary linkage for both architectures, full Go test/vet/build, focused race tests, actionlint, shell syntax, and a Linux ARM64 release regression.
- [x] (2026-07-14 18:17Z) Opened draft PR #159, pushed the standalone review branch, and prepared one exact-head evidence handoff for the platform reviewers.
- [ ] Run the PowerShell lifecycle and execution smoke on Windows AMD64 and native Windows ARM64; retain raw native ARM64 output as the final platform gate.

## Surprises & Discoveries

- Observation: Zig can compile the C parts of both Windows targets, but Rust's prebuilt Windows standard libraries still introduce the unwind ABI into the static Yrs library.
  Evidence: the first links failed with unresolved `_Unwind_*` and `_GCC_specific_handler` symbols on AMD64 and ARM64. Adding the Windows-only CGO dependency `-lunwind` made both exact target pairs link.

- Observation: `cargo build` tried to emit an unused `cdylib` as well as the required static library for `aarch64-pc-windows-gnullvm`.
  Evidence: Cargo attempted to invoke absent `aarch64-w64-mingw32-clang` for the dynamic library even though the static library had already compiled. `cargo rustc -p yffi --release --locked --target <target> --lib --crate-type staticlib` emits only the artifact consumed by `internal/ycrdt`.

- Observation: changing targeted Yrs construction affects Linux cross releases because all non-host targets share `scripts/build-yffi.sh`.
  Evidence: a complete `PLATFORMS=linux/arm64` release build passed after the static-library-only change, including the Zig static link with go-sqlite3 and Yrs.

- Observation: cross compilation proves archive shape and link compatibility, but it cannot prove Win32 execution, NTFS behavior, PowerShell lifecycle behavior, or native ARM64 execution.
  Evidence: this Linux ARM64 development host has no PowerShell or Windows execution environment. The pull request must remain unmerged until separate real-machine evidence is attached to the exact review SHA.

## Decision Log

- Decision: Replace the Windows toolchain as one unit rather than retaining a MinGW-GCC fallback.
  Rationale: one Zig command model supports both required architectures and prevents two Windows compiler paths from drifting. `CC_WINDOWS_AMD64` and `CC_WINDOWS_ARM64` remain escape hatches for equivalent target compiler commands, not built-in compatibility paths.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Pair Zig `x86_64-windows-gnu` with Rust `x86_64-pc-windows-gnu`, and Zig `aarch64-windows-gnu` with Rust `aarch64-pc-windows-gnullvm`.
  Rationale: these are the target pairs that produce link-compatible static Yrs archives and PE executables for the two Windows machine types.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Declare `-lunwind` only for Windows CGO packages in `internal/ycrdt/doc.go`.
  Rationale: the dependency comes from Rust's Windows static library. Encoding it at the CGO consumer keeps Linux and Darwin link contracts unchanged while covering every Windows daemon build and test binary.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Verify releases with a Go program using `encoding/json`, `archive/zip`, `crypto/sha256`, and `debug/pe`.
  Rationale: structured parsers make duplicate architectures, malformed checksums, wrong filenames, missing archive members, and incorrect PE machines explicit failures instead of relying on shell text matching.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Select the release from `PROCESSOR_ARCHITEW6432` before `PROCESSOR_ARCHITECTURE` in PowerShell.
  Rationale: an emulated PowerShell process can report its process architecture in `PROCESSOR_ARCHITECTURE`; `PROCESSOR_ARCHITEW6432` exposes the native operating-system architecture and therefore selects the native package.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Treat native Windows ARM64 output as a required external gate rather than inferring execution from PE inspection.
  Rationale: a correct `0xaa64` header is construction evidence, not proof that the installer, runner, daemon, and sync path work on Windows ARM64.
  Date/Author: 2026-07-14 / Vitaliy

## Outcomes & Retrospective

The implementation now constructs and structurally verifies both native Windows archives from Linux ARM64 with a single Zig toolchain. It removes active MSYS2/MinGW-GCC setup, removes the installer Windows 11/x64-emulation branch, and makes native ARM64 part of the public `all` release set. Cross vet and linked syncer test binaries prove that Yrs and go-sqlite3 coexist for both target pairs.

Draft PR #159 contains the standalone replacement. The remaining outcome is deliberately external: its exact pushed head still needs PowerShell lifecycle output and executable smoke evidence on real Windows, including a native ARM64 machine. Those results must be tied to the unchanged pull-request head; a later head invalidates the seals and requires rerunning them.

## Context and Orientation

`scripts/build-daemon-release.sh` is the canonical release constructor. For each requested operating-system and architecture pair it builds the Rust Yrs static library, stages that library at the path consumed by CGO, builds `daemon/cmd/daemon` and `daemon/cmd/agenttool`, creates the platform archive, and writes `manifest.json` and `SHA256SUMS`.

`scripts/build-yffi.sh` builds the Rust Foreign Function Interface library from `third_party/y-crdt`. The Go package `internal/ycrdt` links that archive through the CGO directives in `internal/ycrdt/doc.go`. The daemon also compiles `github.com/mattn/go-sqlite3`, so the selected C compiler must link both libraries into one executable.

`deploy/daemons/install.ps1` is the public Windows installer. It resolves the native machine architecture, downloads the architecture-specific ZIP and checksum list, validates the archive, installs both executables, writes configuration, and installs or starts the user-level runner. `scripts/test-daemon-installer-windows.ps1` parses all PowerShell files and exercises this lifecycle on Windows.

`.github/workflows/ci.yml` has an active Linux ARM64 construction job and preserved Windows execution jobs. The Linux job is authoritative for both cross builds, PE structure, manifests, checksums, Windows-tagged vet, and linked test-binary compilation. A Windows runner is still authoritative for PowerShell and executable behavior.

The older `.agent/windows-cli-daemon.md` and `.agent/windows-powershell-installer.md` plans record how the prior AMD64-only and temporary x64-emulation paths were introduced. Their historical transcripts remain useful, but this plan supersedes those paths as the current release architecture.

## Plan of Work

Extend the release mappings in `scripts/build-daemon-release.sh` so `all` includes `windows/arm64`, Windows Rust targets map to the GNU and gnullvm triples above, and the default C compiler commands are the corresponding Zig targets. Remove automatic GCC and MinGW selection. Permit either `zip` or `7z` so the same constructor works on Linux and Windows.

In `scripts/build-yffi.sh`, make targeted builds request only `staticlib`. In `internal/ycrdt/doc.go`, add the Windows unwind library alongside the existing Win32 system libraries. Do not change host Yrs construction or non-Windows CGO flags.

In `deploy/daemons/install.ps1`, replace the Windows build-number and emulation guard with two deterministic helpers: one maps native machine names to `amd64` or `arm64`, and one creates the exact archive filename. Use the selected architecture in the download, status text, extraction directory, and installed package path. Extend `scripts/test-daemon-installer-windows.ps1` to assert all accepted aliases, reject unsupported x86, assert both filenames, and require a native PE fixture for whichever architecture runs the lifecycle.

Add `scripts/verify-windows-daemon-release.go`. It must reject unknown manifest fields, duplicate or missing architectures, unexpected filenames, malformed or duplicate checksum rows, hash mismatches, missing archive members, and daemon or agent-tool PE headers that do not match the artifact architecture.

Update `.github/workflows/ci.yml` to install Zig and both Rust targets without MSYS2 or MinGW-GCC. Build both canonical archives once, run the structured verifier, then rebuild and stage each Yrs target before Windows-tagged vet and syncer test-binary compilation. Preserve the native Windows execution suite with Zig so it can be enabled when runners are available.

Update `README.md` so supported platforms, focused build commands, toolchain prerequisites, and installer behavior all describe native AMD64 and ARM64. Do not retain Windows 11 or x64-emulation instructions.

## Concrete Steps

Run all commands from the repository root.

Install the cross prerequisites when they are not already present:

    rustup target add x86_64-pc-windows-gnu aarch64-pc-windows-gnullvm
    zig version
    zip -v

Build and verify the two Windows releases:

    release_version="$(scripts/read-version.sh)"
    PLATFORMS="windows/amd64 windows/arm64" \
      scripts/build-daemon-release.sh /tmp/notty-task31-release
    go run ./scripts/verify-windows-daemon-release.go \
      "/tmp/notty-task31-release/$release_version" "$release_version"

The verifier must report both rows:

    verified windows/amd64 archive notty-daemon_task31_windows_amd64.zip (PE machine 0x8664)
    verified windows/arm64 archive notty-daemon_task31_windows_arm64.zip (PE machine 0xaa64)

For each target, stage the matching Rust library and compile the Windows-tagged packages:

    RUST_TARGET=x86_64-pc-windows-gnu RUSTFLAGS="-C panic=abort" scripts/build-yffi.sh
    CC="zig cc -target x86_64-windows-gnu" CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
      go vet ./daemon/internal/syncer ./internal/ycrdt
    CC="zig cc -target x86_64-windows-gnu" CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
      go test -c -o /tmp/notty-syncer-amd64.test.exe ./daemon/internal/syncer

    RUST_TARGET=aarch64-pc-windows-gnullvm RUSTFLAGS="-C panic=abort" scripts/build-yffi.sh
    CC="zig cc -target aarch64-windows-gnu" CGO_ENABLED=1 GOOS=windows GOARCH=arm64 \
      go vet ./daemon/internal/syncer ./internal/ycrdt
    CC="zig cc -target aarch64-windows-gnu" CGO_ENABLED=1 GOOS=windows GOARCH=arm64 \
      go test -c -o /tmp/notty-syncer-arm64.test.exe ./daemon/internal/syncer

Restore the host library before host Go tests:

    scripts/build-yffi.sh
    go test ./...
    go vet ./...
    go build ./...
    go test -race ./daemon/internal/syncer ./internal/ycrdt
    go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 .github/workflows/ci.yml
    sh -n scripts/build-daemon-release.sh scripts/build-yffi.sh
    git diff --check

On Windows AMD64 and ARM64, use the exact archives from the review commit. Run `scripts/test-daemon-installer-windows.ps1` with a native fixture when needed, install from an isolated `file://` release tree, start the installed daemon, complete one sync smoke test, and record the raw architecture, PE-machine, installer, process, and sync output.

## Validation and Acceptance

The release gate passes only when exactly two Windows artifacts appear in both `manifest.json` and `SHA256SUMS`, every recorded hash matches the archive bytes, each ZIP contains the runner and both executables, and both executables carry the machine type declared by the artifact architecture.

The link gate passes only when Windows-tagged vet and a linked syncer test binary complete for each Zig/Rust pair. The syncer binary imports both `internal/ycrdt` and go-sqlite3, so a successful external link proves their static and system-library contracts are compatible.

The installer gate passes only when AMD64 and x86_64 map to the AMD64 filename, ARM64 and aarch64 map to the ARM64 filename, unsupported x86 fails before installation, the selected native archive installs, and checksum corruption is rejected.

The execution gate passes only with real Windows output from both architectures. The ARM64 evidence must show the host is ARM64, both installed PE files are `0xaa64`, the daemon starts, and an end-to-end sync smoke completes. Emulated AMD64 execution on ARM64 does not satisfy this gate.

## Idempotence and Recovery

The release constructor removes and recreates only the requested output version and `latest` directories. Use a dedicated `/tmp` output path while testing. If a build is interrupted, delete that dedicated output path and rerun the complete two-platform command; do not treat a partial manifest as evidence.

Targeted Yrs builds intentionally replace `third_party/y-crdt/target/release/libyrs.a` with the selected target archive. Always run `scripts/build-yffi.sh` without `RUST_TARGET` before host Go commands. The script restores a host archive without changing source files.

Do not amend the pushed review commit after reviewers begin. If any code or workflow changes, push a new commit, publish its full SHA, and rerun all construction and native evidence against that new head.

## Artifacts and Notes

The local two-architecture construction produced these focused evidence hashes before the review commit was created:

    cabf9214298bdd8f51dcc7875c20be533fa2e69ac44e2e52fe3c0e20eb48284f  notty-daemon_task31_windows_amd64.zip
    ae87389356db027f5016778da70dffaeca0b2eb660dade072d1bedaca837807e  notty-daemon_task31_windows_arm64.zip

These hashes prove the local build described above, not a future CI run. Reviewers must bind their approval to the exact pushed commit and its own artifacts.

## Interfaces and Dependencies

`scripts/build-daemon-release.sh` must retain the environment overrides `CC_WINDOWS_AMD64` and `CC_WINDOWS_ARM64`; without overrides it must use `zig cc -target x86_64-windows-gnu` and `zig cc -target aarch64-windows-gnu` respectively. It requires Rust targets `x86_64-pc-windows-gnu` and `aarch64-pc-windows-gnullvm` and either `zip` or `7z`.

`deploy/daemons/install.ps1` defines:

    Get-WindowsReleaseArchitecture -MachineArchitecture <string>
    Get-WindowsArtifactName -ReleaseVersion <string> -ReleaseArchitecture <string>

The first returns only `amd64` or `arm64` and throws for unsupported values. The second returns `notty-daemon_<version>_windows_<architecture>.zip`.

`scripts/verify-windows-daemon-release.go` accepts exactly two arguments: the version directory and the expected version. A zero exit status means both archives, structured metadata, checksums, archive members, and PE machines passed.

Plan revision note (2026-07-14): created during implementation after the prior Windows AMD64 and installer plans were found to describe superseded MinGW and x64-emulation paths. This plan records the complete replacement, the exact local evidence, and the native ARM64 gate that remains before merge. Updated after draft PR #159 was opened so the living progress and outcome match the published review state.
