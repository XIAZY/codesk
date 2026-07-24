# Rename the canonical daemon version contract

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. This document is maintained in accordance with `.agent/PLANS.md`.

## Purpose / Big Picture

The repository currently stores the release number for every daemon-family artifact in a generically named root file, `VERSION`. That name suggests the frontend or backend may share the same release identity even though the frontend is unversioned and backend Docker images use Git commit tags. After this change, the canonical file will be `DAEMON_VERSION`, its POSIX and PowerShell readers will have daemon-specific names, and every daemon, agent-tool, macOS desktop daemon, and Windows desktop daemon build will continue to receive the same numeric `0.0.1` value. A user can verify the distinction by running frontend and backend contract fixtures without `DAEMON_VERSION`, while daemon-family builds continue to reject a missing or malformed daemon version.

## Progress

- [x] (2026-07-24 07:44Z) Read `.agent/PLANS.md`, inspected the dirty working tree, and mapped direct root-version consumers across shell, PowerShell, Docker, Make, CI, documentation, and tests.
- [x] (2026-07-24 07:47Z) Renamed the canonical file and reader entry points, then updated every production consumer across shell, PowerShell, Make, Docker, and CI.
- [x] (2026-07-24 07:48Z) Updated fixtures, source-contract assertions, documentation, and historical checked-in plans so no active instruction points to the old names.
- [x] (2026-07-24 07:50Z) Passed shell syntax, daemon-version, build/deploy, and Go contract validation, then confirmed the old paths and active references are absent.

## Surprises & Discoveries

- Observation: Frontend and backend paths are already independent of the canonical numeric file.
  Evidence: `scripts/test-version-contract.sh` has no-file fixtures proving backend deploy uses `backend-deadbee` and frontend build/deploy uploads successfully without `VERSION`.

- Observation: The macOS and Windows desktop applications belong to the daemon family in this repository, even though their package formats impose numeric version rules.
  Evidence: Their builders compile code under `daemon/`, embed `notty/daemon/internal/buildinfo.Version`, and use the root numeric value for app bundle and MSI metadata.

- Observation: PowerShell is not installed on the current macOS host, so runtime PowerShell parser assertions cannot execute locally.
  Evidence: `make daemon-version-contract-check` printed `SKIP: PowerShell runtime assertions (pwsh unavailable)`, while `go test ./scripts` passed the PowerShell source-contract and mutation tests.

## Decision Log

- Decision: Rename `VERSION` to `DAEMON_VERSION`, `scripts/read-version.sh` to `scripts/read-daemon-version.sh`, and `scripts/read-version.ps1` to `scripts/read-daemon-version.ps1`.
  Rationale: Renaming only the data file would leave generic public entry points that continue to imply a repository-wide release version.
  Date/Author: 2026-07-24 / Codex

- Decision: Rename the Make variable to `REPOSITORY_DAEMON_VERSION` and the contract runner to `daemon-version-contract-check`, while keeping ordinary local variables such as `version` inside already daemon-specific scripts.
  Rationale: Shared orchestration names need explicit ownership, while local variables are unambiguous within daemon release code and changing all of them would add noise without improving the interface.
  Date/Author: 2026-07-24 / Codex

- Decision: Do not rename unrelated toolchain or package-format identifiers such as `GO_VERSION`, `RUST_VERSION`, PowerShell `$ProductVersion`, or the Windows upgrade-fixture file.
  Rationale: Those names describe their own external version domains and are not aliases for the canonical daemon release file.
  Date/Author: 2026-07-24 / Codex

- Decision: Preserve the current uncommitted deployment work and do not create an intermediate commit.
  Rationale: The working tree contains the directly related R2 replacement, backend Git-SHA, and versionless frontend changes from this user request chain. Committing only part of that intertwined state would create an artificial boundary, and the user did not ask for commits.
  Date/Author: 2026-07-24 / Codex

## Outcomes & Retrospective

The canonical daemon-family release file is now `DAEMON_VERSION`, retaining the exact bytes `0.0.1\n`. The old root file, generic readers, generic shell contract filename, generic Go contract filename, and generic Make contract target are gone. Production callers now use `scripts/read-daemon-version.sh` or `scripts/read-daemon-version.ps1`, and Make names its lazy private value `REPOSITORY_DAEMON_VERSION`. Backend images remain Git-SHA-addressed and the frontend remains versionless, proven by isolated no-file deploy fixtures.

All required local checks passed: shell syntax, `make daemon-version-contract-check`, `make build-deploy-contract-check`, `go test ./scripts`, the stale-name audit, and `git diff --check`. PowerShell runtime assertions remain unavailable on this host, but their Go source-contract tests passed. No image, object, or service was published.

## Context and Orientation

The root numeric file currently contains `0.0.1` and is validated as canonical `X.Y.Z`, with major and minor at most 255 and build at most 65535 so it can also populate Windows MSI metadata. `scripts/read-version.sh` and `scripts/read-version.ps1` are the only intended parsers. Daemon-family build scripts call those readers, `daemon/Dockerfile` copies the file into its build context, the Makefile embeds the value into host daemon binaries, and `.github/workflows/ci.yml` uses the readers to verify release output. The shared `scripts/upload-r2.sh` reads the numeric value only for daemon-family targets; its frontend branch is intentionally versionless. Backend image scripts use `scripts/read-git-sha.sh` instead.

The contract tests duplicate these paths in isolated temporary repositories. `scripts/test-version-contract.sh` checks strict parsing and proves frontend/backend independence. `scripts/version_buildinfo_contract_test.go` checks Dockerfile build-info injection. `scripts/windows_msi_release_contract_test.go` checks PowerShell and Make source contracts. `scripts/test-build-deploy-contract.sh` stages daemon release fixtures. These tests must be renamed or updated with the production files so they continue to detect an accidental return to a generic repository version.

## Plan of Work

First, rename the root data file and both strict readers. Update reader error messages and paths to say `DAEMON_VERSION`, preserving the exact numeric grammar and MSI bounds. Change all shell, PowerShell, Make, Docker, and CI callers to the daemon-specific reader names. Rename `REPOSITORY_VERSION` to `REPOSITORY_DAEMON_VERSION` so Make evaluates the file lazily only for host daemon binaries.

Second, update test fixtures and source assertions. Rename the shell contract runner to `scripts/test-daemon-version-contract.sh`, rename its Make target to `daemon-version-contract-check`, and rename the Go build-info contract file to make daemon ownership clear. All temporary fixture files must be called `DAEMON_VERSION`. The no-file backend and frontend scenarios must still pass, while daemon-family scenarios must fail before build work when `DAEMON_VERSION` is absent or malformed.

Third, update current documentation and checked-in historical plans that still instruct a reader to edit `VERSION` or invoke a generic reader. Do not rewrite unrelated prose that discusses version numbers conceptually, and do not alter third-party or toolchain version constants.

Finally, audit the repository for the obsolete root filename, reader names, Make variable, and contract target. Run the relevant tests from a writable Go cache and record concise success output here.

## Concrete Steps

Work from `/Users/zhongyangxia/Documents/dev/notty`.

Apply the renames and update all callers. Then run:

    sh -n scripts/read-daemon-version.sh scripts/build-daemon-release.sh scripts/build-daemon-platform.sh scripts/deploy-daemon.sh scripts/build-frontend.sh scripts/deploy-frontend.sh scripts/upload-r2.sh scripts/test-daemon-version-contract.sh
    make daemon-version-contract-check
    GOCACHE=/tmp/notty-daemon-version-build-contract make build-deploy-contract-check
    GOCACHE=/tmp/notty-daemon-version-go-contract go test ./scripts
    git diff --check

Audit stale names with a repository search that excludes `.git`, generated `dist`, frontend dependencies, this ExecPlan, and the two contract scripts that deliberately name obsolete paths and targets to assert their absence. The search should find no production-code or documentation references to `read-version.sh`, `read-version.ps1`, `REPOSITORY_VERSION`, the root path `VERSION`, or `version-contract-check`.

Observed validation excerpts:

    $ scripts/read-daemon-version.sh
    0.0.1

    $ make daemon-version-contract-check
    PASS: root daemon-version file and reader names are explicit
    PASS: backend build and deploy use Git SHA without reading DAEMON_VERSION
    PASS: frontend build and deploy work without a DAEMON_VERSION file
    All daemon version contract tests passed.

    $ make build-deploy-contract-check
    All build/deploy contract tests passed.

    $ go test ./scripts
    ok      notty/scripts

## Validation and Acceptance

The change is accepted when `DAEMON_VERSION` contains the original `0.0.1` bytes and `VERSION` no longer exists; both daemon-specific readers return `0.0.1`; daemon and desktop source contracts still pass; the isolated frontend and backend fixtures succeed with no `DAEMON_VERSION`; malformed and missing daemon-version fixtures fail; and the build/deploy and Go contract suites pass. No Docker image, R2 object, or production service should be published during validation.

## Idempotence and Recovery

The edits and tests are local and safe to repeat. Test fixtures use temporary directories and fake Docker, Git, AWS, SSH, SCP, and npm executables, so they do not mutate external systems. If a stale reference causes a failure, update that caller to the daemon-specific name and rerun the focused contract before the full suite. Preserve all pre-existing uncommitted deployment changes; do not reset or overwrite them.

## Artifacts and Notes

The initial canonical file bytes are:

    30 2e 30 2e 31 0a    # "0.0.1\n"

The initial worktree already includes related uncommitted changes for replaceable daemon R2 uploads, backend Git-SHA tags, and versionless frontend deployment. This plan treats those changes as the baseline and will not revert them.

The final stale-path check confirmed that `VERSION`, `scripts/read-version.sh`, `scripts/read-version.ps1`, `scripts/test-version-contract.sh`, and `scripts/version_buildinfo_contract_test.go` do not exist. The production-code and documentation audit found only daemon-specific canonical names, apart from unrelated external version domains intentionally excluded by this plan. The new contract scripts intentionally retain the obsolete strings as negative assertions so a future reintroduction fails tests.

## Interfaces and Dependencies

At completion, `scripts/read-daemon-version.sh` and `scripts/read-daemon-version.ps1` are the only canonical parsers for the root `DAEMON_VERSION` file. They take no arguments, ignore caller attempts to override the file through environment variables, print the validated numeric value on success, and fail with a daemon-specific diagnostic otherwise. The Makefile exposes `daemon-version-contract-check` and uses the private `REPOSITORY_DAEMON_VERSION` variable only when embedding build information into host daemon-family binaries. No new external dependency is introduced.

Plan revision note (2026-07-24 07:44Z): Created the initial self-contained plan after mapping the canonical version contract; the plan chooses a complete daemon-specific interface rename while preserving unrelated version domains and current uncommitted work.

Plan revision note (2026-07-24 07:50Z): Marked implementation complete, recorded the PowerShell runtime limitation, added observed validation output, and documented the successful stale-name and no-external-write audits.

Plan revision note (2026-07-24 07:51Z): Clarified that stale-name audits exclude deliberate negative assertions in contract tests; production code and documentation remain free of the obsolete names.
