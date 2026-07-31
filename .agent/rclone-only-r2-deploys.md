# Use rclone for every R2 deploy

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. Maintain this document in accordance with `.agent/PLANS.md`.

## Purpose / Big Picture

After this change, every deploy target that publishes to Cloudflare R2 uses the same rclone-based path. Operators install one uploader, export the standard R2 S3 access key and secret, and run the existing Make targets without installing the AWS CLI or Wrangler. The result is visible by running the build/deploy contract suite: homepage, frontend, daemon, macOS GUI, and Windows GUI fixtures must all invoke rclone and no production uploader code may mention Wrangler or the AWS CLI.

## Progress

- [x] (2026-07-28 19:11Z) Audited the shared uploader, deploy documentation, and uploader-specific contract fixtures.
- [x] (2026-07-28 19:20Z) Replaced the three production uploader branches with one rclone implementation for object reads, object writes, and directory synchronization.
- [x] (2026-07-28 19:20Z) Removed Wrangler- and AWS-specific workarounds, configuration checks, and dead test fixtures.
- [x] (2026-07-28 19:20Z) Updated contract assertions and operator documentation for the single rclone dependency.
- [x] (2026-07-28 19:27Z) Passed shell syntax, the focused Windows source contract, and the complete build/deploy contract suite.
- [x] (2026-07-28 19:32Z) Committed the rclone-only source as `d5dd7da` so the real build could bind provenance to a concrete commit.
- [x] (2026-07-28 19:40Z) Confirmed a rebuilt 0.0.2 cannot replace its different immutable R2 manifest; no objects were written.
- [x] (2026-07-28 19:49Z) Advanced the canonical release version to 0.0.3 and completed the user-requested real Windows GUI deploy with exit 0.
- [x] (2026-07-28 19:50Z) Independently verified the seven remote objects, manifest equality, both MSI hashes, and provenance source commit for 0.0.3.
- [x] (2026-07-28 19:57Z) Passed the complete build/deploy contract suite again with canonical version 0.0.3.
- [x] (2026-07-28 20:00Z) Pushed the existing branch and updated draft PR #225 to describe the rclone-only uploader and verified 0.0.3 release.

## Surprises & Discoveries

- Observation: `rclone copyto` and `rclone cat` return success when a requested single S3 object is absent because rclone can interpret that path as an empty directory.
  Evidence: the real Windows GUI deploy initially reported a conflicting empty manifest; `rclone lsjson <remote> --stat --files-only` returns `null` for an absent object and a JSON file record with `"IsDir": false` for a present object.
- Observation: the old shared uploader gives directory deletion semantics only to the AWS CLI path. Its Wrangler path uploads enumerated files without deleting stale remote objects.
  Evidence: `scripts/upload-r2.sh` calls `aws s3 sync --delete` for AWS but loops over `wrangler r2 object put` for Wrangler.
- Observation: rebuilding Windows GUI 0.0.2 from the rclone-only commit produces a different MSI manifest than the already-published 0.0.2 release, so the immutable release guard rejects it.
  Evidence: both architecture builds completed, rclone read the remote ledger, and the deploy exited with `windows-gui release 0.0.2 is already published with a different manifest` before any write.

## Decision Log

- Decision: Configure one ephemeral named rclone S3 remote from `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `R2_ENDPOINT_URL` for every target.
  Rationale: This matches Cloudflare R2's S3-compatible credentials, avoids a persistent credential file, and eliminates uploader selection branches.
  Date/Author: 2026-07-28 / Codex
- Decision: Preserve optional-object reads as a stat followed by `copyto` and verify that the destination file was created.
  Rationale: A bare `copyto` cannot distinguish an absent object from an empty transfer, while the release ledger must fail closed.
  Date/Author: 2026-07-28 / Codex
- Decision: Use rclone directory synchronization for static directory deploys and explicit single-object uploads for release commit ordering and metadata overrides.
  Rationale: Every static directory caller requires stale-object deletion, so one `rclone sync` path is sufficient. Release targets require payload-before-manifest ordering that is already expressed by `upload_file` and `upload_committed_release_dir`.
  Date/Author: 2026-07-28 / Codex
- Decision: Advance `DAEMON_VERSION` from 0.0.2 to 0.0.3 for the requested real deployment.
  Rationale: The user requires a successful real deploy, 0.0.2 is already immutable with different payload bytes, and a read-only rclone stat confirmed `desktop/windows/0.0.3/manifest.json` is absent.
  Date/Author: 2026-07-28 / Codex

## Outcomes & Retrospective

The implementation now has one production uploader path for all five R2 targets and removes 270 lines while adding the rclone fixtures and contracts needed to preserve failure ordering. The complete contract suite passes with canonical version 0.0.3. A real Windows GUI 0.0.3 deployment completed successfully, and independent R2 reads verified its exact inventory, manifests, MSI hashes, and provenance. The branch is pushed and draft PR #225 now describes the complete rclone-only result.

## Context and Orientation

`scripts/upload-r2.sh` is the only shared R2 publisher. `UPLOAD_TARGET` selects homepage, frontend, daemon, macOS GUI, or Windows GUI behavior. A release ledger is the versioned `manifest.json` object written after all payload objects; the `latest/manifest.json` object is a short-cache pointer written after the ledger. `scripts/test-build-deploy-contract.sh` supplies fake upload commands and exercises failure ordering, retries, immutable desktop releases, and replaceable daemon releases. `scripts/windows_msi_release_contract_test.go` checks source-level invariants for the Windows path. `README.md` and `scripts/README.md` document operator prerequisites.

The branch `agent/use-rclone-r2-upload` already contains a narrower Windows-only migration and draft PR #225. This plan broadens that same branch and PR rather than creating another branch. Existing Windows GUI release 0.0.2 has already been published and independently verified. The broader migration changed the built payload identity, so the requested real deployment uses the next canonical release, 0.0.3, rather than weakening the 0.0.2 immutability guard.

## Plan of Work

First simplify `scripts/upload-r2.sh`. Delete `aws_s3`, `wrangler_cmd`, and `wrangler_put`, remove the `uploader` selector, and make `download_optional_file`, `upload_file`, and `upload_dir` call rclone directly. Require rclone, the endpoint, and both S3 credentials once after target configuration. Export the ephemeral `RCLONE_CONFIG_NOTTYR2_*` variables once. For directory targets, use `rclone sync` when deletion is requested and `rclone copy` otherwise, with upload cache metadata; keep explicit `copyto --no-check-dest` for ordered release writes.

Next revise `scripts/test-build-deploy-contract.sh`. The fake rclone must model `lsjson`, `copyto`, and the chosen directory commands, retain the existing object-store and injected-failure behavior, and log enough detail for ordering and metadata assertions. Delete the fake Wrangler command and Wrangler-only variables, helper names, messages, and duplicate test cases. Keep coverage of discovery failures, mid-payload failures, no-op reads, current-version daemon replacement, immutable desktop conflicts, and static directory deletion through rclone.

Then update `scripts/windows_msi_release_contract_test.go`, `README.md`, and `scripts/README.md` so they describe a shared rclone requirement for every R2 deploy rather than a Windows-only exception. Search the tracked tree for obsolete uploader references and retain Cloudflare account identifiers only where they serve a purpose unrelated to Wrangler authentication.

Finally run the exact checks below, inspect the complete diff and working tree, and amend or add a terse commit on the existing branch. Run the real Windows GUI deploy from that commit and require it to finish successfully as an immutable 0.0.2 no-op. Then push and update PR #225's title and body to describe the rclone-only result.

## Concrete Steps

Work from `C:\Users\zhong\notty` on branch `agent/use-rclone-r2-upload`.

Inspect obsolete paths with:

    rg -n -i "wrangler|aws_s3|command -v aws|CLOUDFLARE_API_TOKEN|NOTTY_CLOUDFLARE_TOKEN" scripts/upload-r2.sh scripts/test-build-deploy-contract.sh README.md scripts/README.md

After editing, validate with:

    C:\Program Files\Git\bin\sh.exe -n scripts/upload-r2.sh scripts/test-build-deploy-contract.sh
    go test ./scripts -run '^TestWindowsGUIUploadPreflightSourceContract$' -count=1
    C:\Program Files\Git\bin\bash.exe scripts/test-build-deploy-contract.sh

The focused Go test must print `ok`. The shell suite must finish with `All build/deploy contract tests passed.` A final search must show no Wrangler or AWS CLI uploader implementation in `scripts/upload-r2.sh`.

After committing the 0.0.3 version, run `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File .\make.ps1 windows-gui-deploy` with the permanent R2 credentials and rclone on `PATH`. Expect both architectures to build, seven version objects to publish, and `latest/manifest.json` to advance. Verify the remote inventory, hashes, and provenance before publishing the branch and updating the existing draft PR.

## Validation and Acceptance

Acceptance requires all five `UPLOAD_TARGET` values to fail early when rclone or any of the three credential inputs are missing and to configure the Cloudflare S3 provider without writing a config file. Static homepage and app fixtures must prove the correct destination and synchronization semantics. Daemon and both GUI fixture sets must prove that rclone reads version ledgers, uploads payloads before manifests, stops before commit points on injected failures, and preserves their existing conflict policies. The complete contract suite must pass with no fake Wrangler executable. The final committed source must also complete a real Windows GUI deploy without changing immutable 0.0.2 objects.

The production script must contain no uploader selector and no invocation of `aws`, `wrangler`, or `npx wrangler`. Documentation must name `rclone`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `R2_ENDPOINT_URL` as the common R2 deployment prerequisites. Windows GUI 0.0.3 must exist remotely only after all six architecture payload files are present, and its provenance must name the release commit.

## Idempotence and Recovery

Local fixture validation is repeatable and writes only under test temporary directories. Do not run a new real release deploy because 0.0.2 is immutable and already published. If a contract fails, inspect its isolated log, repair the fake rclone or production primitive, and rerun the focused failing section followed by the full suite. Preserve unrelated worktree changes; stage only files listed in this plan.

## Artifacts and Notes

The real deploys verify this rclone configuration against Cloudflare R2:

    RCLONE_CONFIG_NOTTYR2_TYPE=s3
    RCLONE_CONFIG_NOTTYR2_PROVIDER=Cloudflare
    RCLONE_CONFIG_NOTTYR2_ENV_AUTH=true
    RCLONE_CONFIG_NOTTYR2_ENDPOINT=<R2_ENDPOINT_URL>
    RCLONE_CONFIG_NOTTYR2_REGION=auto

Earlier R2 verification found the exact seven Windows GUI 0.0.2 objects, matching version and latest manifests, matching local MSI hashes, and provenance bound to commit `dcd488632b49954eb8d30965211959f7934c3399`. Final verification found the same exact seven-object, manifest, hash, and provenance guarantees for 0.0.3, bound to release commit `cb9b95d025faea94f2da0dea31d4e469f8e95a8f`.

## Interfaces and Dependencies

The external dependency is rclone 1.59 or newer. The script exposes no new command-line interface: callers continue to set `UPLOAD_TARGET` and run the existing Make targets. The required secret environment interface is `AWS_ACCESS_KEY_ID` plus `AWS_SECRET_ACCESS_KEY`; `R2_ENDPOINT_URL` and the existing `R2_*_BUCKET` and `R2_*_PREFIX` values remain non-secret destination configuration. The named remote `nottyr2:` exists only through exported environment variables in the uploader process and is never persisted.

Plan revision note (2026-07-28): Created this plan after the user broadened PR #225 from a Windows-only rclone migration to a single rclone uploader for every R2 deploy target. Updated it after implementation, after the user explicitly required another real Windows GUI deployment, after the immutable 0.0.2 conflict required version 0.0.3, and after the verified release and PR update completed.

## Windows MSI CI path removed (2026-07-31)

The `windows-desktop-msi` job and the two `upload-artifact` steps that fed it were deleted. The job
had been `if: false` since no Windows ARM64 runner exists, so its two payload uploads ran on every
build for a consumer that could never execute — 1.5 GB of the repository's 2.63 GB artifact quota,
which then failed every `Windows daemon (Zig …)` row at `Upload AMD64 desktop installer payload`
while the cross-build itself passed.

Note the uploads already carried `retention-days: 1` and 138 artifacts aged 1–7 days were still
present with `expired: false` — **the retention policy was set and not honored**, so bounding the
quota with retention would have been a no-op.

The payload build and PE/manifest/checksum verification are unchanged; only the uploads and their
dead consumer are gone. **Rebuilding MSI means rebuilding its artifact handoff too** — the
producer/consumer pair went together deliberately, so nothing is left dangling. Windows runner
revival is tracked in task #21.
