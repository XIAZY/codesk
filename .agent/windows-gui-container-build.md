# Containerize the Windows GUI MSI build

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document is maintained in accordance with `.agent/PLANS.md` from the repository root.

## Purpose / Big Picture

After this change, a developer on a supported Windows ARM64 or AMD64 machine can run `make windows-gui-build`, `make windows-gui-release GUI_VERSION=1.2.3`, or the equivalent `make.ps1` command without installing Go, Rust, Zig, Git Bash, .NET, WiX, or a C cross-compiler on the host. Docker builds and caches a pinned Windows toolchain image, then runs the existing payload and MSI logic inside a native, process-isolated Windows container. Build products still appear under `dist/windows-gui` in the checkout.

The implementation is proven on the available Windows 11 ARM64 build 26100 host without Hyper-V. The same wrapper selects AMD64 base images and AMD64-native tool archives when the Docker server and host are AMD64, so the implementation does not hard-code ARM64.

## Progress

- [x] (2026-07-20 01:03Z) Read `AGENTS.md`, `.agent/PLANS.md`, the Windows build scripts, Make routes, documentation, WiX project, and contract tests.
- [x] (2026-07-20 01:03Z) Confirmed the Docker engine is Windows ARM64 on host build 26100 and ran Nano Server LTSC 2025 ARM64 with process isolation.
- [x] (2026-07-20 01:03Z) Proved Nano Server cannot run the complete build because it lacks Windows PowerShell and `msi.dll`, both required by the existing WiX validation flow.
- [x] (2026-07-20 01:47Z) Added `deploy/windows-desktop/Dockerfile` with checksum-pinned ARM64/AMD64 inputs and the complete reusable toolchain.
- [x] (2026-07-20 01:55Z) Added `scripts/run-windows-gui-container.ps1`, routed `make.ps1` through it, and retained the existing inner orchestrator for CI and advanced native use.
- [x] (2026-07-20 02:04Z) Added native LLVM-MinGW compiler overrides after Zig failed to complete cgo compilation in the process-isolated ARM64 container.
- [x] (2026-07-20 02:11Z) Added static compiler linking after runtime inspection found an external `libunwind.dll` dependency in LLVM-MinGW's default output.
- [x] (2026-07-20 02:18Z) Updated source-contract tests and the Windows build documentation.
- [x] (2026-07-20 02:22Z) Ran `make.ps1 windows-gui-build` successfully in the process-isolated ARM64 container.
- [x] (2026-07-20 02:24Z) Ran `make.ps1 windows-gui-release GUI_VERSION=1.2.3` successfully, producing and validating both AMD64 and ARM64 MSIs.
- [x] (2026-07-20 02:27Z) Rechecked artifact checksums, provenance, PE runtime imports, PowerShell syntax, focused source tests, Docker image identity, Go contract tests, and whitespace.
- [x] (2026-07-20 02:31Z) Moved the WiX SDK restore into the reusable image-level NuGet cache so release runs do not fetch the packaging toolchain again.

## Surprises & Discoveries

- Observation: Nano Server is runnable but unsuitable for this build.
  Evidence: `docker run --rm --isolation=process mcr.microsoft.com/windows/nanoserver:ltsc2025-arm64 cmd /c ver` succeeded, while checks for `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe` and `C:\Windows\System32\msi.dll` both returned `File Not Found`. The existing reproducibility verifier loads WiX DTF, which calls the native Windows Installer API supplied by `msi.dll`.

- Observation: A native Windows ARM64 Rust host does not provide the self-contained GNU environment this build needs.
  Evidence: the available ARM64 Rust host is `aarch64-pc-windows-msvc`, which requires Visual Studio Build Tools and a Windows SDK. The x64 `x86_64-pc-windows-gnu` Rust host works under Windows ARM64 emulation and installs both the x64 GNU and ARM64 GNU/LLVM targets without adding Visual Studio to the image.

- Observation: Zig 0.16.0 does not complete the cgo compiler probe in this process-isolated Server Core ARM64 environment.
  Evidence: both the native ARM64 Zig executable and the x64 Zig executable stalled during the first Go cgo compile. Native ARM64 LLVM-MinGW completed the same AMD64 and ARM64 payload builds. Zig remains installed because the shared inner script and non-container CI default to it.

- Observation: LLVM-MinGW's default clang link added a non-system runtime DLL dependency.
  Evidence: `llvm-objdump -p` showed `libunwind.dll`, and an ARM64 test executable exited with Windows loader status `0xc0000135`. Supplying `-static` in each compiler override removed that import; all four final payload executables import only Windows system/API-set DLLs, and the ARM64 compiled test executable launches and passes a focused test on the host.

- Observation: Building the reusable Windows toolchain image is expensive once but cheap to reuse.
  Evidence: the final image, including its WiX cache, is `windows/arm64`, Windows version `10.0.26100.33158`, and 10,603,888,723 bytes. Repeated public commands reuse Docker layers and do not reinstall the toolchain.

- Observation: the full compiled `codesk-desktop` test suite contains pre-existing line-ending-sensitive source assertions.
  Evidence: the ARM64 executable runs, but three assertions that search LF-delimited YAML/XML blocks fail against this Windows checkout's CRLF files. A focused runtime invocation passes. The new repository-level container contract tests pass independently with `go test ./scripts`.

## Decision Log

- Decision: Use `mcr.microsoft.com/windows/servercore:ltsc2025-<architecture>` rather than Nano Server.
  Rationale: Server Core is the smallest Microsoft base in this image family that supplies both Windows PowerShell and the Windows Installer native API required by the existing MSI validation. Copying a few files into Nano Server would not recreate unsupported operating-system API surface.
  Date/Author: 2026-07-20 / Codex

- Decision: Select the digest-pinned base and downloaded tool archives from the Docker server architecture, require the server architecture to equal the host architecture, and force `--isolation=process` for build and run.
  Rationale: process-isolated Windows containers share the host kernel and processor architecture. The mapping makes ARM64 native here and AMD64 native on an AMD64 Windows host without Hyper-V or a hard-coded platform override.
  Date/Author: 2026-07-20 / Codex

- Decision: Keep all compilers and packaging tools in the image and bind mount only source and outputs.
  Rationale: this follows the user's requirement that the reusable image own its toolchain. The Dockerfile restores WiX SDK 4.0.5 into its persistent NuGet cache in addition to installing the language tools. This avoids repeating installations on the host or in each ephemeral release container and keeps source changes from invalidating the large toolchain layers.
  Date/Author: 2026-07-20 / Codex

- Decision: Keep `scripts/run-windows-gui-target.ps1`, `scripts/build-windows-desktop-payloads.sh`, and `scripts/build-windows-desktop-msi-artifact.ps1` as the inner build implementation, adding a separate outer Docker wrapper.
  Rationale: CI already exercises these scripts directly. Reuse avoids creating a second payload or WiX implementation.
  Date/Author: 2026-07-20 / Codex

- Decision: Install the x64 Windows GNU Rust host on both image architectures, while using host-native Go, .NET, MinGit, Zig, and LLVM-MinGW archives.
  Rationale: this is the smallest working Rust configuration on ARM64 Server Core. Windows ARM64 can execute the x64 Rust host, and Rust still emits native x64 or ARM64 libraries through explicit targets.
  Date/Author: 2026-07-20 / Codex

- Decision: Override the inner script's Zig C compiler with architecture-specific LLVM-MinGW clang commands ending in `-static`.
  Rationale: LLVM-MinGW is reliable in the ARM64 process container, and static compiler runtime linking ensures the installed GUI does not require a toolchain DLL that is not included in the MSI.
  Date/Author: 2026-07-20 / Codex

- Decision: Bind mount the checkout at `C:\workspace` and reject output paths outside the checkout.
  Rationale: artifacts remain visible on the host while source and outputs stay out of the image. Rejecting external paths prevents a misleading successful build whose products cannot be written through the sole mount.
  Date/Author: 2026-07-20 / Codex

## Outcomes & Retrospective

The requested workflow is complete. `make.ps1` now enters a process-isolated Windows toolchain container, and one cached image contains every build dependency. The host-native build produced ARM64 GUI, agent, and compiled-test PE files. The release build cross-compiled both payload architectures and produced `Codesk_1.2.3_windows_amd64.msi` and `Codesk_1.2.3_windows_arm64.msi`, with checksum and provenance files for each.

The existing MSI builder linked each architecture twice, compared normalized installer contents, rejected a deliberate causal mismatch, ran ICE validation, and verified embedded payload identity. The two final `SHA256SUMS` files validate. Source contract tests and PowerShell parsing pass. The only non-product gap is the pre-existing CRLF sensitivity of three source-inspection tests in the compiled desktop test suite; it does not prevent compilation, launch, payload verification, MSI validation, or release creation.

The central lesson is that “native toolchain whenever possible” needs to be evaluated per executable on Windows ARM64. Native Go, .NET, Git, and LLVM-MinGW work; Rust's self-contained route is x64 GNU under emulation; and Zig can remain present for compatibility without being the cgo driver in this container.

## Context and Orientation

`make.ps1` is the Make-free Windows entry point. It parses settings such as `GUI_VERSION=1.2.3` and now calls `scripts/run-windows-gui-container.ps1`. The Windows branches in `Makefile` already delegate to `make.ps1`, so both Make and direct PowerShell use the container.

`scripts/run-windows-gui-container.ps1` is the outer wrapper. It checks the host and Docker server, selects the matching architecture's pinned inputs, builds `deploy/windows-desktop/Dockerfile`, bind mounts the repository at `C:\workspace`, and calls `scripts/run-windows-gui-target.ps1` in the container. “Process isolation” means the container shares the host's Windows kernel instead of starting a Hyper-V virtual machine.

`scripts/run-windows-gui-target.ps1` remains the inner orchestrator. The build target detects the native operating-system architecture and builds only that payload. The release target builds AMD64 and ARM64 payloads by default, then invokes `scripts/build-windows-desktop-msi-artifact.ps1` for each. `scripts/build-windows-desktop-payloads.sh` is the shared payload implementation. It uses Go and Rust and accepts `WINDOWS_GUI_CC_AMD64` and `WINDOWS_GUI_CC_ARM64` compiler-command overrides; without overrides, existing CI behavior still defaults to Zig.

`scripts/windows_msi_release_contract_test.go` guards the public dispatch, process-isolation flags, dual-architecture URLs, pinned base digests, archive checksum flow, static LLVM compiler overrides, safe bind mount, and inner-script call. `scripts/README.md` documents the public Docker workflow and the retained native/CI inner path.

## Milestones

The first milestone established base-image feasibility. It succeeded when Nano Server ran with process isolation on ARM64, then demonstrated that the complete WiX validation required Server Core because Nano lacked PowerShell and `msi.dll`.

The second milestone created the reusable build environment and public boundary. It succeeded when the image reported the pinned versions and native Go architecture, `make.ps1 windows-gui-build` entered the ARM64 process container, and the expected native payload and compiled tests appeared under `dist/windows-gui`.

The third milestone made cross-architecture release reliable. It replaced the stalled Zig cgo driver with native LLVM-MinGW, statically linked its runtime, and succeeded when both MSIs passed the repository's two-link reproducibility, ICE, decompilation, and payload validation flow.

The final milestone added regression protection and documentation. It succeeded when `go test ./scripts`, PowerShell parser checks, artifact checksum/provenance checks, PE import inspection, and `git diff --check` all passed.

## Plan of Work

The completed implementation added `deploy/windows-desktop/Dockerfile` with an `ARG BASE_IMAGE` before `FROM`. Its wrapper supplies architecture-specific URLs and hashes for Go 1.23.12, rustup 1.29.0, Zig 0.16.0, .NET SDK 8.0.423, MinGit 2.55.0.3, and LLVM-MinGW 20260616. The Dockerfile validates every archive before extracting or running it, installs Rust 1.97.0 with both Windows targets, restores WiX SDK 4.0.5 into the image-level NuGet cache, checks tool identities, and leaves the reusable tools in the final image.

The completed `scripts/run-windows-gui-container.ps1` rejects non-Windows hosts, non-Windows Docker servers, unsupported or mismatched architectures, missing submodule source, unsupported Zig versions, and output paths outside the bind-mounted checkout. It uses PowerShell argument arrays for Docker calls and explicitly requests process isolation for both operations.

The completed changes route `make.ps1` through the wrapper, let the inner shell script accept compiler overrides, teach the inner PowerShell script to find MinGit's POSIX shell, update the source contracts, and document both public and advanced workflows.

## Concrete Steps

All commands below run from `C:\Users\zhong\notty` in Windows PowerShell. Initialize the existing source submodule once:

    git submodule update --init --recursive

Build the native GUI payload:

    powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-build

Build both installers:

    powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-release GUI_VERSION=1.2.3

Run the focused source contracts and whitespace check:

    go test ./scripts
    git diff --check

The first Docker build can take many minutes because Server Core and all tools are stored in the image. Later invocations reuse those layers.

## Validation and Acceptance

Acceptance is demonstrated when the build log says it is building and running `codesk-windows-gui-build:ltsc2025-arm64` or `ltsc2025-amd64` with process isolation, then reports verified PE machine and subsystem values. On this host, image inspection reports:

    windows/arm64 10.0.26100.33158 10603888723

The native build must produce nonempty `Codesk.exe`, `notty-agent-tool.exe`, `notty-syncer-arm64.test.exe`, and `codesk-desktop-arm64.test.exe` files under `dist/windows-gui`. The release must additionally produce these files and a `SHA256SUMS` plus `provenance.json` beside each:

    dist/windows-gui/msi/amd64/Codesk_1.2.3_windows_amd64.msi
    dist/windows-gui/msi/arm64/Codesk_1.2.3_windows_arm64.msi

`go test ./scripts` must pass. Each checksum line must match its named file. Each provenance record must contain two MSI builds, nonempty normalized database and extracted-tree hashes, and a rejected causal mismatch. Inspecting all four payload executables with LLVM `llvm-objdump -p` must show no `libunwind.dll` import. Running the ARM64 desktop test executable from `daemon/cmd/codesk-desktop` with `-test.run TestWindowsCompositionUsesOneAccountRootForStateAndSingleton` must print `PASS`.

## Idempotence and Recovery

The wrapper gives the image a deterministic architecture/version tag, so repeated builds reuse validated layers. Container runs use `--rm`; a failed run does not leave a stopped container. Existing payload and MSI scripts clean only checked output children below their safe parent and transactionally restore the host Yffi archive.

If a download or image layer fails, rerun the same command. Docker reuses earlier layers. Inspect the cached image without changing state by running `docker image inspect codesk-windows-gui-build:ltsc2025-<architecture>`. Do not delete Docker data or repository output as part of ordinary recovery.

## Artifacts and Notes

The decisive final transcript is:

    amd64 checksums and reproducibility provenance verified
    arm64 checksums and reproducibility provenance verified
    PowerShell syntax verified
    windows/arm64 10.0.26100.33158 10603888723
    Codesk.exe [amd64] runtime imports verified
    notty-agent-tool.exe [amd64] runtime imports verified
    Codesk.exe [arm64] runtime imports verified
    notty-agent-tool.exe [arm64] runtime imports verified
    PASS

The final release emits WiX ICE91 warnings about per-user directory placement. Those warnings predate this container boundary and are not errors; the existing validation deliberately runs without suppressing ICE checks and completes successfully.

## Interfaces and Dependencies

`scripts/run-windows-gui-container.ps1` accepts `Target`, `Version`, `Architectures`, `RepositoryRoot`, `WindowsRoot`, `PayloadRoot`, `TestRoot`, `MsiRoot`, `Repository`, and `ZigVersion`, matching the values forwarded by `make.ps1`. Its image tag includes LTSC 2025 and the Docker server architecture.

The Dockerfile accepts `BASE_IMAGE`, `TOOLCHAIN_ARCH`, and URL/hash pairs for Go, Zig, rustup, .NET, MinGit, and LLVM-MinGW. The image supplies `powershell.exe`, `git.exe`, `sh.exe`, `go.exe`, `cargo.exe`, `rustc.exe`, `zig.exe`, `dotnet.exe`, and both LLVM-MinGW clang target drivers. WiX SDK 4.0.5 remains restored by the existing project through NuGet.

`scripts/build-windows-desktop-payloads.sh` accepts optional `WINDOWS_GUI_CC_AMD64` and `WINDOWS_GUI_CC_ARM64` environment variables containing complete compiler commands. The container passes absolute compiler paths plus `-static`; when the variables are unset, the script preserves its previous Zig commands.

Plan revision note (2026-07-20): Marked the plan complete and revised every section to reflect live implementation evidence. The final design differs from the initial native-tool assumption because ARM64 Rust requires the x64 GNU host under emulation, Zig stalled during cgo compilation, and LLVM-MinGW needed static runtime linking. A final review also moved WiX restore into the image so the packaging toolchain, not merely `dotnet`, is reusable. The plan records these changes so a future contributor can reproduce both the reasoning and validation.
