# Windows desktop native acceptance

This suite destructively installs, launches, crashes, upgrades, and removes
Codesk. Run it only from a dedicated Windows account on native AMD64 or ARM64
hardware. Cross-compilation and PE inspection do not satisfy a native row.

The Go scenario engine is platform-neutral. It owns sequencing, timeouts,
assertions, checkpoint integrity, redaction, and the JSON report. The Windows
adapter owns only native release inspection and operating-system facts/actions:
AuthentiCode/PE identity, Known Folder and Registry queries, Job Objects, Setup,
process and window observation, and guarded native paths. It consumes the same
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
  manifest, canonical `SHA256SUMS`, and architecture artifacts.
- The exact full candidate product source revision.
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

## Prepare and resume

Run the prepare phase from PowerShell:

```powershell
.\scripts\test-windows-desktop-native.ps1 `
  -Phase Prepare `
  -SourceRevision <candidate-product-sha> `
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
and `checkpoint.json` together. Repeat the full prepare/resume flow independently
on native AMD64 and native ARM64, using the exact artifacts named in each report.
On every ordinary runner exit, the suite removes only its exact synthetic
Scheduled Task and Startup-link fixtures; a product failure and the cleanup
result remain separate report rows. Product installation or user data left by a
failed lifecycle row is intentionally not force-deleted and must be inspected
before the dedicated account is reused.

## Covered lifecycle

- Canonical release metadata, checksums, source identity, architecture, and
  native signature trust.
- Clean-account previous install, first browser connection, normal quit, and
  real logout/login autostart with preserved connection state.
- Legacy launcher removal, candidate replacement upgrade, single-instance and
  native process containment, including preservation of a disabled autostart
  choice across replacement. The second launch uses forged `LOCALAPPDATA`,
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
