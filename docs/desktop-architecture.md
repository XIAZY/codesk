# Desktop Application Architecture

Codesk Desktop has one platform-neutral application coordinator and narrow
native composition roots. The shared code owns product behavior; platform code
owns operating-system integration and packaging.

## Shared coordinator

`daemon/internal/desktopapp` is the only desktop application coordinator. It:

- loads token-free configuration and protected credentials through interfaces;
- owns the embedded daemon controller and its restart policy;
- publishes typed menu models and routes typed menu actions;
- runs browser connection handoffs, commits accepted metadata, and restarts the
  daemon with the new connection;
- toggles and verifies the login item through the `desktop.LoginItem` contract;
- cancels and joins the controller, connection handoffs, and action loop during
  shutdown.

The coordinator imports no native API and has no operating-system build tag.
Its behavior is tested on the host, including action routing, connect and
commit, monotonic online service generations across restart, menu publication,
login-item toggling, and joined shutdown.

The Windows composition must acquire the Windows instance lock,
establish process-tree containment, select native directories, construct the
configuration, secret, login-item, and opener adapters, and pass them to
`desktopapp.New`. The macOS composition must construct that same
coordinator. Neither composition root may copy the action loop or controller
ownership.

The typed tray renderer may remain shared where `fyne.io/systray` implements
the same rendering contract on both platforms. It renders models and forwards
actions; it does not own configuration, credentials, controller state, or
shutdown.

## Native boundaries

Each native boundary preserves a common invariant but uses the operating
system's security or lifecycle model. These are adapter boundaries, not reasons
to duplicate the coordinator.

| Boundary | Common invariant | Windows | macOS |
| --- | --- | --- | --- |
| Credentials | The daemon token is never stored in configuration, URLs, logs, or plaintext at rest; only the current user can recover it. | The file secret store encrypts values with current-user DPAPI and forbids UI prompts. | The Keychain adapter fixes the generic-password service to `com.getcodesk.desktop`, uses the desktop secret key as the account, and stores it in the standard per-user login Keychain without authentication UI. Save exposes the caller-owned C buffer to Security.framework without an extra `NSData` copy; caller-owned Go and C byte buffers are zeroed after use. Security.framework-owned ARC buffers and immutable Go strings are not claimed to be wipeable. |
| Launch at login | The setting is per-user, idempotent, and reported as enabled only when the exact Codesk registration is active. | The adapter owns the exact quoted executable value in `HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run`. | `SMAppService.mainAppService` registers the signed app on macOS 13+. The adapter verifies the resulting status and reports `RequiresApproval` with the exact System Settings path instead of claiming success. |
| Instance and process ownership | One desktop generation owns the user state, and quitting or crashing must not leave an embedded daemon/provider generation behind. | A shared per-user file lock excludes other desktop generations across Terminal Services sessions. An unnamed, non-inheritable, kill-on-close Job Object contains descendants before the syncer can spawn them. | `LSMultipleInstancesProhibited` supplies the LaunchServices hint and a private nonblocking `flock` remains authoritative for direct executable launches. Before the syncer can spawn, the app becomes a process-group leader and starts a separately grouped copy of its signed executable in watchdog mode. The watchdog binds to its exact parent with `kqueue(EVFILT_PROC, NOTE_EXIT)` and kills the complete app process group after normal or abnormal parent exit; the parent reciprocally kills its group if the watchdog exits first. |
| Opening URLs and logs | Only validated application HTTP(S) URLs and the designated log directory are opened; no shell command is assembled. | `ShellExecute` delegates the target to the registered Windows handler. | The macOS composition delegates validated targets to `NSWorkspace`, which is the native Launch Services API. |
| Directories | Configuration, logs, caches, workspace state, and helper discovery use deterministic, per-user native locations and never touch legacy CLI state. | Runtime data is rooted from the current user's Local App Data Known Folder under `Codesk`; the WiX definition installs application files below `LocalAppDataFolder\Programs\Codesk` and uses `ProgramMenuFolder` for the Start Menu shortcut. | The current uid's operating-system account record, never caller-controlled `HOME`, roots `Library/Application Support/Codesk`, `Library/Logs/Codesk`, and `Library/Caches/Codesk`; bundle helpers live under `Contents/Helpers`. |
| Installation and trust | Installation is per-user, installer lifecycle operations preserve the user's exact launch-at-login preference, and every published artifact has platform-native integrity and trust evidence. | The declarative WiX package delegates install, repair, major upgrade, and uninstall to Windows Installer. Product signing and trust verification remain separate release gates; the desktop owns no installer transaction or update engine. | The macOS release owns a signed and notarized universal `.app`, nested helper signing, hardened runtime, stapling, and a notarized DMG. App-bundle replacement and Gatekeeper rules differ fundamentally from Windows Installer ownership. |

The macOS login Keychain keeps the token under the current user's
operating-system-protected credential store, but it does not provide Keychain
access-group app isolation.
This profile-less policy is intentional while unsigned functional builds are
supported. Moving Codesk to the data-protection Keychain requires an Apple
Developer App ID, provisioning profile, matching Keychain access-group
entitlements, and signed native validation; that stronger isolation is a future
release upgrade rather than a claim of the current app.

## Windows composition and installation

`daemon/cmd/codesk-desktop/main_windows.go` is a composition root for the same
shared coordinator. It acquires the current user's cross-session instance lock,
assigns the process to its kill-on-close Job Object, resolves the native data
directories, and constructs the DPAPI, registry login-item, and ShellExecute
adapters before calling `desktopapp.New`. It contains no installer state
machine.

The production desktop executable is linked separately for AMD64 and ARM64
with the Windows GUI subsystem. Enabled CI constructs both exact production
commands and inspects each PE machine and subsystem. The cross-build job uploads
the real architecture-bound desktop and agent-tool bytes; each hosted Windows
installer row downloads only its matching artifact, re-inspects both PE machine
and subsystem identities, and links those exact files. This establishes linked
binary structure, not native execution. `deploy/windows-desktop/Codesk.wixproj`
then compiles and links the declarative `Package.wxs` for both installer
platforms. A successful link is followed by a compiler-only invalid-version
mutation that must be rejected by WiX even though the source contract remains
unchanged. Versioned release construction must pass distinct AMD64 and ARM64
ProductCodes while retaining the package UpgradeCode.

The MSI owns one stable `NeverOverwrite="yes"` registry component for the exact
`HKCU\Software\Microsoft\Windows\CurrentVersion\Run\Codesk` value. Its authored
empty string is an explicit disabled sentinel, and the runtime reports enabled
only when the stored value is the exact current quoted executable command.
Repair and major upgrade must therefore preserve either the exact enabled
command or the disabled sentinel, while uninstall removes only the owned value.
The upgrade is scheduled after install execution so the stable component can
transfer between versioned products without an uninstall gap.

Native Windows acceptance is required for each exact candidate MSI and is not
substituted by source inspection or cross-construction. The transcript must
prove all of the following:

- fresh install creates exactly one current product and payload, does not
  launch the app, and reports the empty sentinel as disabled;
- repair and same-architecture major upgrade install the candidate bytes,
  remove the prior product, ARP entry, and payload, leave exactly one current
  product, and preserve both enabled and disabled preferences in separate rows;
- uninstall removes the current product, payload, shortcut, and exact owned Run
  value while preserving sibling registry values and keys;
- x64-to-ARM64 handoff installs the ARM64 bytes, removes the x64 product, ARP
  entry, and payload, leaves exactly one current product and correctly preserves
  the Run preference. If Windows Installer cannot establish that row, the
  package must explicitly block cross-architecture handoff instead;
- every installed executable has the expected PE machine and GUI/console
  subsystem, and every observed Run command names the installed desktop bytes.

Failure of any registry preservation or cleanup row invalidates the cleanup
component design. The fallback is an explicit persisted launch preference, not
a custom installer action or a weakened acceptance claim.

## macOS composition and release

`daemon/cmd/codesk-desktop/main_darwin.go` is deliberately a composition root,
not a second application implementation. It resolves the running executable as
exactly `Codesk.app/Contents/MacOS/Codesk`, validates the bundled
`Contents/Helpers/notty-agent-tool`, pins that helper directory to the front of
the bounded desktop `PATH`, then adds only the same common user, Homebrew, and
system tool directories used by the CLI installer. It never inherits arbitrary
ambient entries or the legacy `~/.notty` tree. The composition creates the
three native private roots, acquires the instance lock and process watchdog,
constructs the native adapters, and passes them once to
`desktopapp.New`. The Cocoa status item only renders the shared typed menu and
forwards actions. Its deterministic 32-pixel alpha-mask template PNG is embedded
in the executable and rendered by AppKit at 16 points, preserving a native 2x
representation on Retina displays. The app has `LSUIElement=1`, so it owns no
Dock tile or application window.

URLs and the log directory pass a portable allowlist before the Objective-C
adapter invokes `NSWorkspace.openURL`. No shell command is involved. The
credential adapter invokes Security.framework directly, and launch-at-login
invokes ServiceManagement.framework directly. These Objective-C calls are kept
behind the existing Go interfaces, so native error handling cannot leak into
the coordinator.

The release boundary is fail closed. `build-macos-desktop-release.sh` requires
the pinned Go and Rust toolchains, builds the Rust bridge and both Go executables
for x86_64 and arm64 with a macOS 13 deployment target, creates exact two-slice
Mach-O files, and checks the generated icons and canonical `Info.plist`. It
signs the nested helper before the app, uses Developer ID timestamps and the
hardened runtime, notarizes and staples the app, then builds, signs, notarizes,
and staples the DMG. The release manifest binds the source revision, app-tree
hash, DMG hash, size, version, bundle identity, and signed/notarized state.
Both build and verification require the mounted image root to contain exactly a
real `Codesk.app` plus an `Applications -> /Applications` symlink, then compare
the mounted and source app-tree hashes. `verify-macos-desktop-release.sh`
repeats structural, signature, stapler, Gatekeeper, disk-image, and mounted-tree
checks independently. The manifest's `signed_and_notarized` claim selects those
obligations: a true claim always executes trust checks, while a false claim is
accepted only under explicit unsigned relaxation and reports construction-only
evidence.

The checked-in CI matrix can run this unsigned construction on native Intel and
Apple Silicon macOS runners. Each row compiles and tests the Darwin+cgo Cocoa,
Security, ServiceManagement, and composition-root code on its host architecture,
then constructs and independently verifies the same universal x86_64+arm64
release. The matrix is explicitly skipped while this repository has no macOS
runner capacity and becomes active when the `CODESK_MACOS_RUNNERS_AVAILABLE`
repository variable is set to `true`. Until then, native compilation,
construction, and functional validation are manual and must be bound to the
exact proposed merge commit and its artifact hashes. This owner-accepted manual
boundary is not a green CI result and does not establish signing, notarization,
stapling, Gatekeeper acceptance, or publishability.

The watchdog and release verifiers make construction and lifecycle failures
fail closed, but they do not turn a non-macOS build into native evidence. When
Apple credentials are unavailable, the native harness can exercise the exact
unsigned artifact in an explicit functional-only mode. Its output is permanently
marked non-trusted and non-publishable: it establishes runtime behavior, never
signing, notarization, stapling, Gatekeeper acceptance, or release trust. Final
trusted acceptance runs the exact notarized artifact separately on Intel and
Apple Silicon, across a real logout/login boundary, with a real provider
descendant for both the abnormal-app-exit and watchdog-exit rows. The restart
row must observe the candidate's logged online service generation advance while
the desktop pid remains stable; a successful sync alone is not restart proof.
Candidate installation uses a private read-only DMG snapshot whose hash and size
are revalidated at mount time, and the installed tree must equal the persisted
manifest tree. Each sync row plans a unique local-absent path before triggering
the remote mutation. Provider rows bind a newly started PID to its inspected
executable path/hash and a live ancestry chain rooted in the Codesk controller,
then separately prove process-group containment and disappearance.

## Deliberate non-abstractions

- `deploy/windows-desktop/Package.wxs` remains a thin Windows-specific
  declarative boundary. Windows Installer owns install, repair, major upgrade,
  and uninstall; the product contains no generic or Windows-specific installer
  transaction, recovery, or update abstraction. Signing and trust verification
  remain separate release gates.
- Native APIs do not enter `desktopapp`, and platform composition roots do not
  reimplement coordinator behavior.
- Cross-compilation and structural inspection are construction evidence only.
  Real Windows and macOS execution must separately prove native credential,
  login-item, instance, process, tray, install, upgrade, and uninstall behavior;
  signed publication must separately prove platform trust.
- Configuration remains credential-free, and desktop paths remain separate
  from legacy CLI configuration and data.

This split lets portable tests seal product behavior once while keeping the
security, lifecycle, installer, and trust evidence attached to the operating
system that actually provides it.
