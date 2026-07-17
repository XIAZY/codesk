# Windows desktop native acceptance

This suite destructively installs, launches, crashes, upgrades, and removes
Codesk. Run it only from a dedicated Windows account on native AMD64 or ARM64
hardware. Cross-compilation and PE inspection do not satisfy a native row.

The Go scenario engine is platform-neutral. It owns sequencing, timeouts,
assertions, checkpoint integrity, redaction, and the JSON report. The Windows
adapter owns only native release inspection and operating-system facts/actions:
Windows Installer metadata, Authenticode trust, Known Folder and Registry
queries, Job Objects, `msiexec`, process and window observation, and guarded
native paths. It consumes the same
CGO-free `desktopstate` configuration and Windows DPAPI contract as the product;
no credential parser is copied into the suite. Window evidence is retained only when its
process ancestry is rooted in the acceptance runner, the exact release inputs,
or the installed desktop; unrelated user-session windows are neither classified
nor copied into the report. The observer treats the native `SHOW` event itself
as evidence, so a console that closes before a later visibility query is not
discarded. A console-class SHOW that disappears before exact process binding is
fail-closed rather than silently omitted, and the suite never collects window-title text.
Release/evidence paths are checked both lexically and after filesystem-link
resolution, and reset refuses any symlink or reparse point in its deletion tree.

## Build the runners

From a clean checkout containing the suite commit:

```sh
scripts/build-windows-desktop-acceptance.sh \
  "$(git rev-parse HEAD)" dev dist/windows-desktop-acceptance
```

The embedded runner revision identifies the suite implementation. It is
intentionally independent from `-SourceRevision`, which identifies the product
release under test. Reports bind both full revisions and the exact runner bytes.

## Required inputs

- Previous and candidate release directories, each containing only its canonical
  `manifest.json`, canonical `SHA256SUMS`, and the two exact MSI artifacts
  `CodeskMSI_<version>_windows_amd64.msi` and
  `CodeskMSI_<version>_windows_arm64.msi`.
- The exact full candidate product source revision.
- The exact full runner-suite revision embedded by the deterministic builder.
- A new evidence directory outside `%LOCALAPPDATA%\Codesk`.
- The runner matching the host's native architecture.
- A runner process that is not already constrained by another Job Object, so the
  product's own containment assignment is observable rather than inherited. An
  inherited foreign Job produces `BLOCKED`, not a product failure.
- A dedicated account that begins with no Codesk installation, process, user
  data, credential, or legacy launcher.

Signed artifacts are required for publishable evidence. Development artifacts
may be exercised with `-AllowUnsignedFunctional`; that produces an explicit
trust waiver and `publishable=false`.

The manifest schema is `1`. It binds the exact numeric MSI version, full source
revision, canonical Codesk UpgradeCode, `converge` or `block`
cross-architecture policy, construction toolchain, package signatures, distinct
per-architecture ProductCodes, MSI hashes, and the installed `Codesk.exe` and
`notty-agent-tool.exe` hashes. Previous and candidate releases must use four
distinct ProductCodes. The native verifier opens each MSI through Windows
Installer and rejects any mismatch in platform template, product identity,
version, ProductCode, UpgradeCode, per-user scope, or Authenticode state.

## Prepare and resume

Run the prepare phase from PowerShell:

```powershell
.\scripts\test-windows-desktop-native.ps1 `
  -Phase Prepare `
  -SourceRevision <candidate-product-sha> `
  -RunnerSourceRevision <runner-suite-sha> `
  -PreviousReleaseDir <previous-release-dir> `
  -PreviousVersion <previous-version> `
  -CandidateReleaseDir <candidate-release-dir> `
  -CandidateVersion <candidate-version> `
  -EvidenceDir <new-evidence-dir> `
  -RunnerPath <native-runner.exe> `
  -Destructive
```

Prepare installs and connects the previous version, records token-free
configuration and protected-credential fingerprints, quits normally, and writes
a hash-bound checkpoint. Its expected verdict is `PENDING`.

Sign out of Windows and sign back in without manually launching Codesk. Rerun
the identical command with `-Phase Resume`. The resume phase rejects the same
Windows logon authentication identity, proves connected autostart, asks the
operator to confirm that no forbidden surface flashed during login, revalidates
all release and checkpoint identities, disables autostart through the product
menu, and proves that choice survives candidate replacement before completing
the remaining lifecycle.

The final expected verdict is `PASS`. Preserve `report.json`, `transcript.log`,
`checkpoint.json`, `msi-lifecycle.json`, and every `msiexec-*.log` together.
Repeat the full prepare/resume flow independently on native AMD64 and native
ARM64, using the exact artifacts named in each report. Before installing the
previous product for Prepare, the adapter runs an isolated MSI-only preflight
and restores its own product/registry fixtures. Preflight cleanup is a separate
row and cannot downgrade residue to a prerequisite block. Product installation
or user data left by a later interactive lifecycle failure is intentionally not
force-deleted and must be inspected before the dedicated account is reused.

## Covered lifecycle

- Canonical release metadata, checksums, source identity, architecture, and
  native MSI identity/signature trust.
- Fresh MSI installation with exactly one current ProductCode, the exact two
  payload hashes and native PE machines, nested `Programs\Codesk\Codesk.lnk`, no launched process, and
  the empty HKCU Run sentinel. Disabled and exact-enabled Run choices each
  survive repair and same-architecture major upgrade. The prior product, ARP
  registration, and payload must disappear.
- MSI uninstall removes only the owned `Codesk` Run value, product, payload,
  shortcut and its `Programs\Codesk` directory, and ARP registration while preserving a seeded sibling value and
  the shared Run key. On native ARM64, x64-to-ARM64 replacement must converge to
  one ARM64 ProductCode/payload/exact Run value, or a manifest-declared `block`
  policy must fail before any x64 state mutates. AMD64 reports that ARM64-only
  row as waived rather than claiming execution.
- Clean-account previous install, first browser connection, normal quit, and
  real logout/login autostart with preserved connection state.
- Candidate replacement upgrade, single-instance and native process
  containment, including preservation of a disabled autostart choice across
  replacement. The thin WiX MSI does not claim removal of historical bespoke
  Setup/Scheduled-Task/Startup-link artifacts; the dedicated-account baseline
  requires those launchers absent. The second launch uses forged `LOCALAPPDATA`,
  `APPDATA`, `USERPROFILE`, and `HOME` values and must hand off to the original
  authority without creating a redirected state root or changing the resident
  process set.
- Opaque byte-for-byte preservation of the account's native legacy CLI root
  across previous installation, real login, replacement, both uninstalls, and
  suite cleanup. The report contains only a deterministic aggregate digest,
  entry count, and byte count; legacy paths, filenames, contents, and per-file
  hashes never leave the native adapter.
- Surviving real Codex turn, runtime crash/replacement (including PID reuse),
  abnormal desktop termination with application and descendant handles bound to
  exact executable/creation-time identities before termination and every bound
  handle observed exited, relaunch plus a second real turn, daemon restart
  without desktop replacement, and a post-restart turn.
- Plaintext credential scanning, native-surface observation, normal quit,
  uninstall-with-data-preservation, explicit data reset, and a second clean
  candidate install/connect/turn/uninstall/reset cycle.
