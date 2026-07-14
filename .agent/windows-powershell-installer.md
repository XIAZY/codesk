# Add a first-class Windows daemon installer

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must stay current while implementation proceeds. Maintain this file in accordance with `.agent/PLANS.md` from the repository root.

## Purpose / Big Picture

Codesk already produces Windows AMD64 command-line executables, but a Windows user must download a ZIP, place two programs manually, create environment variables, and arrange for the daemon to start. After this work, the local-environment creation and maintenance screens will let a user select Windows and copy one PowerShell command. That command will download a repository-owned installer, verify the release checksum, install both executables, write workspace-scoped configuration, and start a current-user background task without requiring administrator privileges. The same screens will produce Windows reinstall and uninstall commands.

The observable result is a Windows option beside the existing macOS/Linux option in the frontend. Selecting Windows changes the displayed command from `curl ... | sh` to a PowerShell command that downloads `install.ps1` or `uninstall.ps1`. A generated daemon release contains and publishes those scripts, and `install.ps1 -NoService` can be exercised in an isolated Windows test directory without changing the real user's task scheduler.

## Progress

- [x] (2026-07-14 04:48Z) Read `.agent/PLANS.md`, the OXO design reference, the Unix installer and uninstaller, release/publish scripts, Windows release plans, frontend command builders, modal UI, styles, and tests.
- [x] (2026-07-14 04:48Z) Chose the Windows installation, service, fallback, and frontend platform-selection contracts and recorded them below.
- [x] (2026-07-14 05:16Z) Added PowerShell install, run, and uninstall scripts with an isolated Windows lifecycle harness; all scripts pass the PowerShell parser in the official PowerShell Docker image.
- [x] (2026-07-14 05:16Z) Packaged and published PowerShell entrypoints, included Windows AMD64 in the public release set, added an active Windows installer CI job, and updated build targets and documentation.
- [x] (2026-07-14 05:16Z) Added platform-aware frontend command builders, selectors in create/detail/reinstall flows, responsive styles, and unit/component coverage.
- [x] (2026-07-14 05:16Z) Ran all locally available script, frontend, release-build, YAML, and diff checks and started the frontend at `http://localhost:5173/`. Browser screenshot inspection could not run because this session did not expose the in-app browser control tool; that limitation is recorded below.
- [x] (2026-07-14 05:16Z) Recorded final evidence and remaining environment-specific validation in this plan.

## Surprises & Discoveries

- Observation: Windows executable construction is already automatic on Linux, and a prior Windows host run recorded native runtime acceptance, but Windows was deliberately excluded from the public `all` release set until installer packaging existed.
  Evidence: `scripts/build-daemon-release.sh` maps `windows/amd64`; `.github/workflows/ci.yml` has an active GNU cross-build; `.agent/windows-runtime-lifecycle.md` records a passing native suite; `.agent/windows-cli-daemon.md` explicitly names packaging as the remaining publication condition.

- Observation: The existing hosted installer contract includes checksum validation, two installed programs, workspace credentials, bounded runtime `PATH` construction, runtime availability warnings, per-workspace directories, automatic startup, and global uninstall. A Windows script that only unzips the `.exe` would not be equivalent.
  Evidence: `deploy/daemons/install.sh` performs those operations and `scripts/test-daemon-installer.sh` pins them with behavior tests.

- Observation: The current frontend always emits POSIX shell commands, including reinstall and global uninstall, so adding only a Windows script would leave users unable to discover or invoke it.
  Evidence: `frontend/src/logic.ts` hardcodes `install.sh`, `uninstall.sh`, and `sh -s --` in all three command builders.

- Observation: The current macOS development host has no `pwsh` or `powershell` executable. Syntax validation can still use the official PowerShell Linux container, while Task Scheduler, Windows ACL, shortcut, and process-lifecycle behavior requires Windows.
  Evidence: `command -v pwsh` and `command -v powershell` return no path; `mcr.microsoft.com/powershell:7.4-ubuntu-22.04` parsed `install.ps1`, `run-windows.ps1`, `uninstall.ps1`, and the behavior harness without errors.

- Observation: The existing Unix installer contracts remain green after adding the PowerShell peers and public release wiring.
  Evidence: `make daemon-installer-check`, `make daemon-uninstall-test`, `sh -n scripts/build-daemon-release.sh scripts/publish-static-r2.sh`, and `git diff --check` all completed successfully on 2026-07-14.

- Observation: Node 25 enables an experimental web-storage implementation that shadows jsdom's `localStorage` without a valid backing path, causing unrelated frontend cleanup failures. The repository pins Node 22 in `.nvmrc`; disabling that Node 25 experiment reproduces the intended test environment.
  Evidence: plain `npm test` failed with `localStorage.clear is not a function`; `NODE_OPTIONS=--no-experimental-webstorage npm test` passed all 255 tests across 21 files.

- Observation: The in-app browser plugin was listed but its required control tool was not exposed in this session. The Vite server was started and returned HTTP 200, but desktop/mobile screenshot claims would be unsupported.
  Evidence: the browser skill's required `mcp__node_repl__js` tool was unavailable; a localhost fetch confirmed `http://localhost:5173/` serves the Vite entrypoint.

## Decision Log

- Decision: Add `deploy/daemons/install.ps1`, `deploy/daemons/run-windows.ps1`, and `deploy/daemons/uninstall.ps1` rather than trying to make the POSIX scripts polyglot.
  Rationale: PowerShell provides native ZIP, SHA-256, ACL, process, and Task Scheduler APIs. Separate scripts keep quoting and lifecycle behavior understandable and testable on each operating-system family.
  Date/Author: 2026-07-14, Codex.

- Decision: Install each Windows release under `<install-dir>/<version>/` and put that directory first on the launched daemon's `PATH`.
  Rationale: Windows does not permit replacing an executable while another process is using it. Versioned directories avoid cross-workspace upgrade collisions while still exposing `notty-daemon.exe` and `notty-agent-tool.exe` under their expected names.
  Date/Author: 2026-07-14, Codex.

- Decision: Store workspace configuration as a user-private JSON file beside the per-workspace launcher and never place the daemon token in a task action or generated launcher source.
  Rationale: JSON gives PowerShell structured parsing and escaping. Restricting inheritance and granting the current user access mirrors the Unix mode-0600 secret boundary. The Scheduled Task command line then contains only the path to `run.ps1`.
  Date/Author: 2026-07-14, Codex.

- Decision: Use a current-user Scheduled Task with an `Interactive` logon principal, `Limited` run level, an at-logon trigger, and restart settings. If task registration is unavailable, create a shortcut in the current user's Startup folder and start the same launcher immediately. `-NoService` writes files but starts nothing.
  Rationale: This needs no administrator-level run mode, survives login, and uses Windows-native user startup. The Startup shortcut preserves an install path on machines where Task Scheduler policy blocks registration. The launcher owns a named mutex, writes its PID, restarts the daemon after failures, and lets uninstall terminate its process tree.
  Date/Author: 2026-07-14, Codex.

- Decision: Add `windows/amd64` to the release builder's `all` set as part of this feature.
  Rationale: Production uses `DAEMON_PLATFORMS=all`. Advertising a Windows install command while omitting the corresponding ZIP from production would create a guaranteed user-facing failure. Native runtime evidence and automatic cross-link coverage already exist; user-facing packaging was the recorded remaining hold.
  Date/Author: 2026-07-14, Codex.

- Decision: Keep the frontend API source-compatible by adding an optional platform field to the three command builders, defaulting to macOS/Linux. Add a two-option segmented control labeled `macOS / Linux` and `Windows`; initially select Windows when either the connected daemon reports `os: windows` or the browser user agent is Windows.
  Rationale: Existing call sites and tests continue to work, while users get an explicit, familiar mode control. Connected-daemon metadata is more reliable than browser detection for reinstall and uninstall; the selector remains manually overridable.
  Date/Author: 2026-07-14, Codex.

- Decision: Run the PowerShell lifecycle harness in a small, active `windows-latest` CI job instead of relying on the deliberately paused Windows native compiler job.
  Rationale: Installer correctness does not require rebuilding the Rust/Go binaries. The harness uses a harmless Windows executable fixture and can continuously verify checksum rejection, protected config, Scheduled Task or Startup registration, runner startup, cleanup, and keep-binaries behavior.
  Date/Author: 2026-07-14, Codex.

## Outcomes & Retrospective

The repository now has a complete Windows AMD64 installation path. `install.ps1` downloads and verifies the ZIP, installs versioned executables and private workspace configuration, and registers a limited current-user Scheduled Task with a Startup shortcut fallback. `run-windows.ps1` owns the long-running process lifecycle, and `uninstall.ps1 -All` removes registrations, managed data, and binaries. Release builds and R2 publication expose stable PowerShell entrypoints, and `PLATFORMS=all` includes Windows AMD64.

The create, uninstall, and reinstall frontend flows now share an explicit macOS/Linux versus Windows segmented control and emit correctly quoted Shell or PowerShell commands. The full frontend suite passed 255 tests, the production bundle built, the Unix installer regression harnesses passed, YAML and shell syntax checks passed, all PowerShell files parsed in Docker, and a host-platform release smoke build produced the expected four public scripts. Windows-only lifecycle execution is intentionally delegated to the new active `windows-daemon-installer` CI job; it could not run on this macOS host. Visual screenshot verification was also unavailable for the tooling reason recorded above, although the affected component tests and production build are green and the dev server is running.

## Context and Orientation

The daemon is the local Go process that syncs a Codesk workspace to disk and runs agent-provider command-line tools. `daemon/cmd/daemon/main.go` reads configuration from environment variables. `daemon/cmd/agenttool/main.go` builds the helper program agents use. `scripts/build-daemon-release.sh` builds both programs and creates platform archives plus `manifest.json` and `SHA256SUMS`. Windows AMD64 archives are ZIP files containing `notty-daemon.exe` and `notty-agent-tool.exe`.

The existing Unix user path starts at `deploy/daemons/install.sh`; it downloads an archive from the static daemon origin, checks its SHA-256 digest, writes per-workspace state below `~/.notty/daemons`, and registers launchd or a systemd user service. `deploy/daemons/uninstall.sh` removes every managed local daemon. `scripts/publish-static-r2.sh` uploads the release directory and stable installer URLs to object storage. The new PowerShell scripts must be peers of these files and must be copied to both the generated daemon static root and each immutable version directory.

The React frontend creates install commands in `frontend/src/logic.ts`. `CreateDaemonModal` in `frontend/src/App.tsx` reveals a one-time token and install command. `DaemonDetailModal` displays global uninstall and tokenized reinstall commands. `ShellScriptBlock` renders and copies command text. `frontend/src/styles.css` supplies the restrained paper/ink OXO interface. The new platform control belongs next to the script command and should be a compact segmented control, not a card or a marketing panel.

A Scheduled Task is a Windows-owned registration that starts a program at a trigger such as user logon. Here it runs only as the current interactive user with limited privileges. A Startup shortcut is a per-user `.lnk` file Windows opens after login; it is the fallback when task registration is blocked. Neither mechanism is a machine-wide Windows Service and neither should require elevation.

## Plan of Work

First, add the PowerShell implementation. `deploy/daemons/install.ps1` will expose `-BackendUrl`, `-WorkspaceId`, `-DaemonToken`, `-StaticBase`, `-Version`, `-InstallDir`, `-DataDir`, and `-NoService`. It will accept only AMD64 Windows, support HTTPS and `file://` sources, bypass stale CDN metadata with cache-busting query parameters, validate the release checksum, expand the ZIP, install into a version directory, and copy `run-windows.ps1` into the workspace daemon directory as `run.ps1`. It will preserve configured Codex and Claude commands, resolve their tool directories for a bounded runtime path, warn rather than abort when either provider is unavailable, write a protected `daemon.env.json`, and install/start the current-user background registration.

`deploy/daemons/run-windows.ps1` will load the adjacent JSON, rebuild a bounded `PATH` from installed and known user/system tool directories, set the daemon environment variables in the process, acquire a workspace mutex, write a launcher PID, append startup and exit information to the daemon log, run the daemon, and restart it after a short delay if it exits. `deploy/daemons/uninstall.ps1` will require `-All`, stop and unregister each task or Startup shortcut, terminate recorded launcher process trees, remove managed workspace and agent directories, and remove only recognized Codesk binary version directories unless `-KeepBinaries` is supplied.

Add `scripts/test-daemon-installer-windows.ps1`. It will parse all PowerShell files through the PowerShell parser, construct an isolated fake Windows ZIP and checksums under a temporary directory, run the installer with `-NoService` and custom paths, verify installed files and protected structured configuration, prove checksum rejection, exercise a Scheduled Task or Startup fallback and runner PID, verify that uninstall requires `-All`, and cover ordinary and keep-binaries global uninstall. Add a Make target that runs this harness when Windows PowerShell exists and a lightweight active Windows CI job that runs it unconditionally. The ordinary Unix test target must remain green on hosts without PowerShell.

Second, extend release and publication. `scripts/build-daemon-release.sh` will require and include `run-windows.ps1` in Windows ZIPs, copy all four public install/uninstall scripts to the generated root and version directory, and include `windows/amd64` in `all_platforms`. `scripts/publish-static-r2.sh` will upload both `.ps1` stable URLs with the same short cache lifetime as the shell scripts. The active Linux Windows cross-build will assert that the generated ZIP and static root include the expected PowerShell artifacts.

Third, extend frontend logic and UI. `frontend/src/logic.ts` will define the install-platform type, a user-agent default helper, PowerShell single-quote escaping, and platform-aware install/reinstall/uninstall command builders. Windows commands will download script text with `Invoke-WebRequest`, create a script block, and invoke it with PowerShell-native parameter names and backtick line continuations. `frontend/src/App.tsx` will add a reusable platform segmented control and use it in create, uninstall, and reinstall blocks. Existing Unix prose remains concise; badges identify `Shell` or `PowerShell`. `frontend/src/styles.css` will give the control stable dimensions, small radii, clear selected/focus states, and mobile wrapping without text overlap.

Finally, update `README.md` with Windows prerequisites, commands, output paths, and the production cross-toolchain requirement. Run all available checks, build the frontend, start its development server, and inspect the affected modals at desktop and mobile widths in the in-app browser. Update this document after each milestone and record exact validation outcomes.

## Concrete Steps

All commands run from `/Users/zhongyangxia/Documents/dev/notty` unless a command says otherwise.

After adding PowerShell files, run behavior checks on Windows with PowerShell:

    pwsh -NoLogo -NoProfile -File scripts/test-daemon-installer-windows.ps1

Expected final line:

    Windows daemon installer tests passed

On the current macOS host, first run the checks that do not require PowerShell:

    sh -n deploy/daemons/install.sh deploy/daemons/uninstall.sh scripts/build-daemon-release.sh scripts/publish-static-r2.sh
    make daemon-installer-check
    make daemon-uninstall-test

Run frontend coverage and production compilation:

    cd frontend
    npm test
    npm run build

Run a focused Windows release build on a host with `x86_64-pc-windows-gnu`, MinGW-w64, and ZIP installed:

    PLATFORMS=windows/amd64 scripts/build-daemon-release.sh windows-installer-test /tmp/codesk-windows-release

The result must include `/tmp/codesk-windows-release/install.ps1`, `/tmp/codesk-windows-release/uninstall.ps1`, and a ZIP under the version directory. Expanding that ZIP must show both `.exe` files plus `run-windows.ps1`.

Start the frontend after tests:

    cd frontend
    npm run dev

Open the reported local URL. Create a local environment, select Windows, and confirm the command contains `install.ps1`, `Invoke-WebRequest`, and PowerShell parameter names. In a daemon detail modal, confirm uninstall and reinstall both switch between shell and PowerShell without changing modal dimensions incoherently.

## Validation and Acceptance

Acceptance requires all of the following observable behavior.

On Windows AMD64, running the displayed install command downloads a ZIP whose checksum matches `SHA256SUMS`, creates versioned `notty-daemon.exe` and `notty-agent-tool.exe`, writes a user-private workspace configuration without putting the daemon token on a task command line, and starts either a limited current-user Scheduled Task or the documented Startup fallback. Running the command again for the same version and workspace is safe. `-NoService` installs everything but starts nothing and prints the foreground PowerShell command.

The Windows uninstall command refuses to run without `-All`. With `-All`, it stops launchers and daemon descendants, removes all managed daemon/workspace/agent data, removes registrations, and removes recognized binary directories. `-KeepBinaries` leaves the executable directories while removing configuration and workspaces.

A release built with `PLATFORMS=all` includes the Windows AMD64 ZIP. Generated and published daemon roots expose `install.sh`, `uninstall.sh`, `install.ps1`, and `uninstall.ps1`; version directories also retain copies for immutable release inspection.

The frontend defaults existing calls to macOS/Linux, defaults a Windows browser or known Windows daemon to Windows, and lets the user switch explicitly. Copying each displayed command copies exactly the visible platform-specific install, reinstall, or uninstall text. Unit tests cover apostrophes and spaces in every user-controlled PowerShell value so tokens and URLs cannot break out of single-quoted literals.

The full frontend typecheck, Vitest suite, and production build pass. Existing Unix installer and uninstall harnesses remain green. PowerShell parser checks pass in Docker and behavior tests pass on Windows. Browser inspection should show no clipped buttons, overlapping command text, nested cards, or unstable modal sizing at desktop and mobile widths; that manual/screenshot check remains outside the evidence available in this session.

## Idempotence and Recovery

Release builds recreate only their generated version and latest directories. Installer downloads use a unique temporary directory removed in `finally`, so interrupted downloads do not become installed binaries. Versioned Windows binary directories avoid replacing programs used by another workspace. A repeated install stops only the matching workspace launcher before replacing its config and registration.

The uninstaller removes only recognized managed paths and binary directories containing the two expected executable names. It never recursively deletes an arbitrary custom install root. If task registration fails, installer cleanup removes any partial task before writing a Startup shortcut. If the frontend work needs to be reverted, the optional platform field means the existing Unix command behavior can remain while Windows UI exposure is removed independently.

## Artifacts and Notes

The stable public PowerShell invocation will have this shape:

    $ErrorActionPreference = 'Stop'
    & ([ScriptBlock]::Create((Invoke-WebRequest -UseBasicParsing 'https://static.example/daemons/install.ps1').Content)) `
      -BackendUrl 'https://api.example' `
      -WorkspaceId 'ws_123' `
      -DaemonToken 'nottyd_secret' `
      -StaticBase 'https://static.example/daemons'

The task action contains only a command shaped like:

    powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "C:\Users\name\.notty\daemons\ws_123\run.ps1"

The token stays in `daemon.env.json`, whose inherited ACL entries are removed and whose access rule grants the current user control.

## Interfaces and Dependencies

No new third-party runtime dependency is needed. Windows PowerShell 5.1 supplies `Invoke-WebRequest`, `Expand-Archive`, `Get-FileHash`, JSON conversion, ACL cmdlets, COM shortcut creation, and the `ScheduledTasks` module on the supported Windows baseline. The scripts must remain valid in both Windows PowerShell 5.1 and PowerShell 7.

In `frontend/src/logic.ts`, define:

    export type DaemonInstallPlatform = "unix" | "windows";

    export function defaultDaemonInstallPlatform(osHint?: string, userAgent?: string): DaemonInstallPlatform;

Add `platform?: DaemonInstallPlatform` to the existing input objects for `buildDaemonInstallCommand`, `buildDaemonReinstallCommand`, and `buildDaemonUninstallCommand`. Omitting it must preserve current POSIX output.

`deploy/daemons/install.ps1` must expose PowerShell parameters named `BackendUrl`, `WorkspaceId`, `DaemonToken`, `StaticBase`, `Version`, `InstallDir`, `DataDir`, and `NoService`. `deploy/daemons/uninstall.ps1` must expose `All`, `InstallDir`, `DataDir`, and `KeepBinaries`.

Plan revision note: created on 2026-07-14 to turn the existing Windows executable construction path into a complete, discoverable end-user installation workflow. Updated at 05:16Z after completing implementation, adding active Windows installer CI, and recording final local validation and environment-specific limitations.
