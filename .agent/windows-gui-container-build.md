# Containerize the Windows GUI MSI build

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document is maintained in accordance with `.agent/PLANS.md` from the repository root.

## Purpose / Big Picture

After this change, a developer on a supported Windows ARM64 or AMD64 machine can run `make build-windows-builder-image` once to create `alphatoad/notty:windows-builder`, then run `make windows-gui-build`, `make windows-gui-release`, or the equivalent `make.ps1` commands without installing Go, Rust, Zig, Git Bash, .NET, WiX, or a C cross-compiler on the host. Image construction is independent from product construction: the product targets consume the existing image and never rebuild it. Build products still appear under `dist/windows-gui` in the checkout.

The implementation is proven on the available Windows 11 ARM64 build 26100 host without Hyper-V. The Dockerfile uses `TARGETARCH` to select the base and native tool archives, so the dependency implementation does not live in the wrapper or hard-code this development machine.

## Progress

- [x] (2026-07-20 01:03Z) Read `AGENTS.md`, `.agent/PLANS.md`, the Windows build scripts, Make routes, documentation, WiX project, and contract tests.
- [x] (2026-07-20 01:03Z) Confirmed the Docker engine is Windows ARM64 on host build 26100 and ran Nano Server LTSC 2025 ARM64 with process isolation.
- [x] (2026-07-20 01:03Z) Proved Nano Server cannot run the complete build because it lacks Windows PowerShell and `msi.dll`, both required by the existing WiX validation flow.
- [x] (2026-07-20 01:47Z) Added `deploy/windows-desktop/Dockerfile` with the complete reusable toolchain.
- [x] (2026-07-20 01:55Z) Added `scripts/run-windows-gui-container.ps1`, routed `make.ps1` through it, and retained the existing inner orchestrator for CI and advanced native use.
- [x] (2026-07-20 02:04Z) Added native LLVM-MinGW compiler overrides after Zig failed to complete cgo compilation in the process-isolated ARM64 container.
- [x] (2026-07-20 02:11Z) Added static compiler linking after runtime inspection found an external `libunwind.dll` dependency in LLVM-MinGW's default output.
- [x] (2026-07-20 02:18Z) Updated source-contract tests and the Windows build documentation.
- [x] (2026-07-20 02:22Z) Ran `make.ps1 windows-gui-build` successfully in the process-isolated ARM64 container.
- [x] (2026-07-20 02:24Z) Ran `make.ps1 windows-gui-release` successfully with the root `DAEMON_VERSION`, producing and validating both AMD64 and ARM64 MSIs.
- [x] (2026-07-20 02:27Z) Rechecked artifact checksums, provenance, PE runtime imports, PowerShell syntax, focused source tests, Docker image identity, Go contract tests, and whitespace.
- [x] (2026-07-20 02:31Z) Moved the WiX SDK restore into the reusable image-level NuGet cache so release runs do not fetch the packaging toolchain again.
- [x] (2026-07-20 04:02Z) Split image construction into `scripts/build-windows-gui-builder-image.ps1`, named the default image `alphatoad/notty:windows-builder`, and added `make build-windows-builder-image`.
- [x] (2026-07-20 04:05Z) Changed the container runner to inspect and consume the prebuilt image without invoking `docker build`, including an actionable missing-image failure.
- [x] (2026-07-20 04:09Z) Built the named image entirely from cached layers, then successfully ran `windows-gui-build` from that tag with no image-build step.
- [x] (2026-07-20 04:55Z) Moved all dependency versions, HTTPS URLs, architecture selection, and installation logic into the Dockerfile; left only the legacy `TARGETARCH` fallback in the image-builder wrapper and removed archive checksum plumbing.
- [x] (2026-07-20 04:55Z) Rebuilt `alphatoad/notty:windows-builder` from the refactored Dockerfile, completed a native ARM64 product build from it, confirmed a 2.8-second cache-only rebuild, and reran Go contracts, PowerShell parsing, dependency-ownership checks, and whitespace validation.

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
  Evidence: the final Dockerfile-owned dependency build completed successfully and produced image ID `d91d53b3681d`, including its WiX cache. It is `windows/arm64`, Windows version `10.0.26100.33158`, and 10,747,993,583 bytes. A separate product build entered that image directly and completed in about 161 seconds without reinstalling the toolchain.

- Observation: the full compiled `codesk-desktop` test suite contains pre-existing line-ending-sensitive source assertions.
  Evidence: the ARM64 executable runs, but three assertions that search LF-delimited YAML/XML blocks fail against this Windows checkout's CRLF files. A focused runtime invocation passes. The new repository-level container contract tests pass independently with `go test ./scripts`.

- Observation: this Windows Docker installation cannot provide BuildKit's automatic platform arguments itself.
  Evidence: `docker buildx version` returned `docker: unknown command: docker buildx`, and the engine uses the legacy Windows builder. The Dockerfile still uses the standard `TARGETARCH` interface; the lightweight wrapper passes the Docker server architecture as the compatibility value.

## Decision Log

- Decision: Use `mcr.microsoft.com/windows/servercore:ltsc2025-<architecture>` rather than Nano Server.
  Rationale: Server Core is the smallest Microsoft base in this image family that supplies both Windows PowerShell and the Windows Installer native API required by the existing MSI validation. Copying a few files into Nano Server would not recreate unsupported operating-system API surface.
  Date/Author: 2026-07-20 / Codex

- Decision: Use the Dockerfile's `TARGETARCH` argument to select Server Core and every downloaded tool archive, while the legacy Windows builder wrapper supplies only `TARGETARCH=<Docker server architecture>`.
  Rationale: BuildKit defines `TARGETARCH` automatically, but this host uses Docker's legacy Windows builder and has no `docker buildx` command. Passing the same standard argument is the smallest compatibility fallback. Dependency versions and URLs remain entirely in the Dockerfile, downloads use HTTPS, and archive checksum arguments are intentionally omitted per user direction.
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

- Decision: Separate reusable image construction from product builds, with `alphatoad/notty:windows-builder` as the shared default tag.
  Rationale: a builder-image job can now create or publish the environment once, while actual build jobs only consume it. `WINDOWS_GUI_BUILDER_IMAGE` provides one override used consistently by both jobs, so CI can supply a registry tag or digest without changing the scripts.
  Date/Author: 2026-07-20 / Codex

## Outcomes & Retrospective

The requested workflow is complete. `make build-windows-builder-image` owns the reusable environment, and `make.ps1` product targets enter that prebuilt process-isolated Windows toolchain container without rebuilding it. The host-native build produced ARM64 GUI, agent, and compiled-test PE files. The release build cross-compiled both payload architectures and produced `Codesk_1.2.3_windows_amd64.msi` and `Codesk_1.2.3_windows_arm64.msi`, with checksum and provenance files for each.

The existing MSI builder linked each architecture twice, compared normalized installer contents, rejected a deliberate causal mismatch, ran ICE validation, and verified embedded payload identity. The two final `SHA256SUMS` files validate. Source contract tests and PowerShell parsing pass. The only non-product gap is the pre-existing CRLF sensitivity of three source-inspection tests in the compiled desktop test suite; it does not prevent compilation, launch, payload verification, MSI validation, or release creation.

The central lesson is that “native toolchain whenever possible” needs to be evaluated per executable on Windows ARM64. Native Go, .NET, Git, and LLVM-MinGW work; Rust's self-contained route is x64 GNU under emulation; and Zig can remain present for compatibility without being the cgo driver in this container.

## Context and Orientation

`make.ps1` is the Make-free Windows entry point. It parses settings and dispatches `build-windows-builder-image` to `scripts/build-windows-gui-builder-image.ps1`; product targets dispatch to `scripts/run-windows-gui-container.ps1`. The Windows branches in `Makefile` delegate all three public targets to `make.ps1`.

`scripts/build-windows-gui-builder-image.ps1` checks the host and Docker server, passes the server architecture as the legacy builder's `TARGETARCH` compatibility value, and builds `deploy/windows-desktop/Dockerfile` as `alphatoad/notty:windows-builder`. `scripts/run-windows-gui-container.ps1` is only the outer product-build wrapper: it checks that the named image exists and matches the Docker architecture, bind mounts the repository at `C:\workspace`, and calls `scripts/run-windows-gui-target.ps1` inside it. “Process isolation” means the container shares the host's Windows kernel instead of starting a Hyper-V virtual machine.

`scripts/run-windows-gui-target.ps1` remains the inner orchestrator. The build target detects the native operating-system architecture and builds only that payload. The release target builds AMD64 and ARM64 payloads by default, then invokes `scripts/build-windows-desktop-msi-artifact.ps1` for each. `scripts/build-windows-desktop-payloads.sh` is the shared payload implementation. It uses Go and Rust and accepts `WINDOWS_GUI_CC_AMD64` and `WINDOWS_GUI_CC_ARM64` compiler-command overrides; without overrides, existing CI behavior still defaults to Zig.

`scripts/windows_msi_release_contract_test.go` guards the public dispatch, process-isolation flags, Dockerfile-owned `TARGETARCH` dependency selection, the lightweight builder wrapper, static LLVM compiler overrides, safe bind mount, and inner-script call. `scripts/README.md` documents the public Docker workflow and the retained native/CI inner path.

## Milestones

The first milestone established base-image feasibility. It succeeded when Nano Server ran with process isolation on ARM64, then demonstrated that the complete WiX validation required Server Core because Nano lacked PowerShell and `msi.dll`.

The second milestone created the reusable build environment and public boundary. It succeeded when the image reported the pinned versions and native Go architecture, `make.ps1 windows-gui-build` entered the ARM64 process container, and the expected native payload and compiled tests appeared under `dist/windows-gui`.

The third milestone made cross-architecture release reliable. It replaced the stalled Zig cgo driver with native LLVM-MinGW, statically linked its runtime, and succeeded when both MSIs passed the repository's two-link reproducibility, ICE, decompilation, and payload validation flow.

The final milestone added regression protection and documentation. It succeeded when `go test ./scripts`, PowerShell parser checks, artifact checksum/provenance checks, PE import inspection, and `git diff --check` all passed.

The follow-up milestone separated image production from image consumption. It succeeded when the builder target tagged `alphatoad/notty:windows-builder`, the product build log began directly with `Running windows-gui-build with reusable builder image`, and a missing custom tag failed with an instruction to run `make build-windows-builder-image`.

The dependency-ownership milestone moved every tool version, HTTPS URL, architecture mapping, download, installation, and validation into the Dockerfile. It succeeded when the lightweight wrapper built image ID `d91d53b3681d` using only `TARGETARCH` as a legacy-builder fallback and the resulting image completed a native ARM64 product build.

## Plan of Work

The completed implementation keeps Go 1.23.12, rustup 1.29.0, Rust 1.97.0, Zig 0.16.0, .NET SDK 8.0.423, MinGit 2.55.0.3, LLVM-MinGW 20260616, and their HTTPS URLs in `deploy/windows-desktop/Dockerfile`. `TARGETARCH` selects the Server Core tag and native archive names. The Dockerfile installs both Rust targets, restores WiX SDK 4.0.5 into the image-level NuGet cache, checks tool identities, and leaves the reusable tools in the final image.

The completed `scripts/build-windows-gui-builder-image.ps1` owns only host preflight, the legacy `TARGETARCH` fallback, and `docker build --isolation=process`. The completed `scripts/run-windows-gui-container.ps1` rejects non-Windows hosts, non-Windows Docker servers, unsupported or mismatched architectures, missing or mismatched builder images, missing submodule source, unsupported Zig versions, and output paths outside the bind-mounted checkout. It contains no Docker build operation and uses the selected image only with `docker run --isolation=process`.

The completed changes route `make.ps1` through the wrapper, let the inner shell script accept compiler overrides, teach the inner PowerShell script to find MinGit's POSIX shell, update the source contracts, and document both public and advanced workflows.

## Concrete Steps

All commands below run from `C:\Users\zhong\notty` in Windows PowerShell. Initialize the existing source submodule once:

    git submodule update --init --recursive

Build or refresh the reusable image:

    powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 build-windows-builder-image

Build the native GUI payload from that image:

    powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-build

Build both installers:

    powershell.exe -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-release

Run the focused source contracts and whitespace check:

    go test ./scripts
    git diff --check

The first image build can take many minutes because Server Core and all tools are stored in the image. Later image builds reuse those layers. Product builds do not invoke Docker build and fail clearly if the image has not been built or pulled.

## Validation and Acceptance

Acceptance is demonstrated when the image target reports `Built reusable Windows GUI builder image alphatoad/notty:windows-builder`, then a separate product target reports that it is running that image with process isolation without a Docker build transcript. On this host, image inspection reports:

    windows/arm64 10.0.26100.33158 10747993583

The native build must produce nonempty `Codesk.exe`, `notty-agent-tool.exe`, `notty-syncer-arm64.test.exe`, and `codesk-desktop-arm64.test.exe` files under `dist/windows-gui`. The release must additionally produce these files and a `SHA256SUMS` plus `provenance.json` beside each:

    dist/windows-gui/msi/amd64/Codesk_1.2.3_windows_amd64.msi
    dist/windows-gui/msi/arm64/Codesk_1.2.3_windows_arm64.msi

`go test ./scripts` must pass. Each checksum line must match its named file. Each provenance record must contain two MSI builds, nonempty normalized database and extracted-tree hashes, and a rejected causal mismatch. Inspecting all four payload executables with LLVM `llvm-objdump -p` must show no `libunwind.dll` import. Running the ARM64 desktop test executable from `daemon/cmd/codesk-desktop` with `-test.run TestWindowsCompositionUsesOneAccountRootForStateAndSingleton` must print `PASS`.

## Idempotence and Recovery

The image target gives the image the deterministic default tag `alphatoad/notty:windows-builder`, so repeated image builds reuse validated layers. Product container runs use `--rm`; a failed run does not leave a stopped container. Existing payload and MSI scripts clean only checked output children below their safe parent and transactionally restore the host Yffi archive.

If a download or image layer fails, rerun the image target. Docker reuses earlier layers. Inspect the cached image without changing state by running `docker image inspect alphatoad/notty:windows-builder`. Do not delete Docker data or repository output as part of ordinary recovery.

## Artifacts and Notes

The decisive final transcript is:

    Successfully tagged alphatoad/notty:windows-builder
    Built reusable Windows GUI builder image alphatoad/notty:windows-builder (windows/arm64, Windows 10.0.26100.33158)
    Running windows-gui-build with reusable builder image alphatoad/notty:windows-builder (windows/arm64) and process isolation
    amd64 checksums and reproducibility provenance verified
    arm64 checksums and reproducibility provenance verified
    PowerShell syntax verified
    windows/arm64 10.0.26100.33158 10747993583
    Codesk.exe [amd64] runtime imports verified
    notty-agent-tool.exe [amd64] runtime imports verified
    Codesk.exe [arm64] runtime imports verified
    notty-agent-tool.exe [arm64] runtime imports verified
    PASS

The final release emits WiX ICE91 warnings about per-user directory placement. Those warnings predate this container boundary and are not errors; the existing validation deliberately runs without suppressing ICE checks and completes successfully.

## Interfaces and Dependencies

`scripts/build-windows-gui-builder-image.ps1` accepts `RepositoryRoot` and `BuilderImage`, defaulting to `alphatoad/notty:windows-builder`. `scripts/run-windows-gui-container.ps1` accepts `Target`, `Version`, `Architectures`, `RepositoryRoot`, `WindowsRoot`, `PayloadRoot`, `TestRoot`, `MsiRoot`, `Repository`, `ZigVersion`, and the same `BuilderImage`. `Makefile` exposes the shared override as `WINDOWS_GUI_BUILDER_IMAGE`.

The Dockerfile accepts the standard `TARGETARCH` build argument. It uses that value for the Server Core tag and every native archive choice, and supplies `powershell.exe`, `git.exe`, `sh.exe`, `go.exe`, `cargo.exe`, `rustc.exe`, `zig.exe`, `dotnet.exe`, and both LLVM-MinGW clang target drivers. WiX SDK 4.0.5 remains restored by the existing project through NuGet.

`scripts/build-windows-desktop-payloads.sh` accepts optional `WINDOWS_GUI_CC_AMD64` and `WINDOWS_GUI_CC_ARM64` environment variables containing complete compiler commands. The container passes absolute compiler paths plus `-static`; when the variables are unset, the script preserves its previous Zig commands.

Plan revision note (2026-07-20): Marked the plan complete and revised every section to reflect live implementation evidence. The final design differs from the initial native-tool assumption because ARM64 Rust requires the x64 GNU host under emulation, Zig stalled during cgo compilation, and LLVM-MinGW needed static runtime linking. A final review moved WiX restore into the image so the packaging toolchain, not merely `dotnet`, is reusable. Later revisions separated image construction into `build-windows-builder-image`, made all product builds consume `alphatoad/notty:windows-builder` without rebuilding it, and moved all dependency knowledge into the Dockerfile. The wrapper now supplies only the legacy builder's standard `TARGETARCH` fallback.
