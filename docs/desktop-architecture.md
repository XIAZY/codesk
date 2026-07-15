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
commit, restart, menu publication, login-item toggling, and joined shutdown.

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
| Credentials | The daemon token is never stored in configuration, URLs, logs, or plaintext at rest; only the current user can recover it. | The file secret store encrypts values with current-user DPAPI and forbids UI prompts. | The macOS composition stores the token in Keychain under the Codesk service identity. Keychain, rather than a DPAPI-shaped file format, is the native user-bound credential store. |
| Launch at login | The setting is per-user, idempotent, and reported as enabled only when the exact Codesk registration is active. | The adapter owns the exact quoted executable value in `HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run`. | The macOS composition uses `SMAppService` for the signed app bundle. macOS login-item approval and status belong to Service Management, not a registry-command emulation. |
| Instance and process ownership | One desktop generation owns the user state, and quitting or crashing must not leave an embedded daemon/provider generation behind. | A shared per-user file lock excludes other desktop generations across Terminal Services sessions. An unnamed, non-inheritable, kill-on-close Job Object contains descendants before the syncer can spawn them. | The macOS composition must enforce one app instance and make the `.app` process the lifecycle owner of the embedded daemon, using macOS-native app and process rules and proving joined normal shutdown plus no orphan after abnormal exit. macOS has no Job Object, so the selected native ownership mechanism and its acceptance evidence stay in the macOS adapter/composition. |
| Opening URLs and logs | Only validated application HTTP(S) URLs and the designated log directory are opened; no shell command is assembled. | `ShellExecute` delegates the target to the registered Windows handler. | The macOS composition delegates validated targets to `NSWorkspace`, which is the native Launch Services API. |
| Directories | Configuration, logs, caches, workspace state, and helper discovery use deterministic, per-user native locations and never touch legacy CLI state. | Runtime data is rooted under `%LOCALAPPDATA%\\Codesk`; setup additionally resolves Windows Known Folders for per-user programs and Start Menu registration. | Runtime data uses `~/Library/Application Support/Codesk`, logs use `~/Library/Logs/Codesk`, and caches use `~/Library/Caches/Codesk`; bundle helpers live under `Contents/Helpers`. |
| Installation, recovery, and trust | Installation is per-user, upgrades do not lose the last committed generation, and every published artifact has platform-native integrity and trust evidence. | `CodeskSetup.exe` owns registry and shortcut snapshots, crash-durable install/uninstall recovery, exact payload verification, and Authenticode signing. | The macOS release owns a signed and notarized universal `.app`, nested helper signing, hardened runtime, stapling, and a notarized DMG. App-bundle replacement and Gatekeeper rules differ fundamentally from registry/shortcut recovery. |

## Deliberate non-abstractions

- `daemon/internal/desktopsetup` and the Windows release/signing scripts remain
  Windows-specific. There is no generic installer transaction interface.
- Native APIs do not enter `desktopapp`, and platform composition roots do not
  reimplement coordinator behavior.
- Cross-compilation and structural inspection are construction evidence only.
  Real Windows and macOS acceptance must separately prove native credential,
  login-item, instance, process, tray, signing, install, upgrade, and uninstall
  behavior.
- Configuration remains credential-free, and desktop paths remain separate
  from legacy CLI configuration and data.

This split lets portable tests seal product behavior once while keeping the
security, lifecycle, recovery, and trust evidence attached to the operating
system that actually provides it.
