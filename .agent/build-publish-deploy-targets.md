# Simplify Build, Publish, and Deploy Targets

This ExecPlan is a living document. It follows `.agent/PLANS.md` and keeps the user-facing command surface small while separating local builds, artifact publishing, and production deployment.

## Purpose / Big Picture

After this change, a developer can use one simple set of Makefile commands: `make build` to compile local artifacts, `make publish` to upload immutable artifacts, `make deploy` to publish and activate production services, and `make tests` to run the test suite. The important behavior is observable from the command names themselves: build commands have no network side effects, publish commands upload/push artifacts, and deploy commands make production use the published artifacts.

## Progress

- [x] (2026-05-16 00:25 America/Toronto) Inspected the current Makefile and deploy scripts.
- [x] (2026-05-16 00:28 America/Toronto) Decided that deploy scripts should remain safe when run directly by calling their corresponding publish scripts internally.
- [x] (2026-05-16 00:31 America/Toronto) Added publish scripts for backend image, frontend/homepage static assets, and daemon static artifacts.
- [x] (2026-05-16 00:42 America/Toronto) Added a reusable backend image build script used by both local image builds and backend publishing.
- [x] (2026-05-16 00:32 America/Toronto) Refactored deploy scripts to call publish scripts before production activation.
- [x] (2026-05-16 00:34 America/Toronto) Updated Makefile targets around `build`, `publish`, `deploy`, and compatibility aliases.
- [x] (2026-05-16 00:36 America/Toronto) Updated README command documentation.
- [x] (2026-05-16 00:39 America/Toronto) Validated shell syntax and ran non-network local build checks.

## Surprises & Discoveries

- Observation: `scripts/deploy-frontend.sh` and `scripts/deploy-static.sh` already only build and publish R2 assets; they do not SSH into a server.
  Evidence: both scripts call `scripts/build-static.sh` and `scripts/publish-static-r2.sh` directly.
- Observation: `scripts/deploy-backend.sh` mixes backend image publish and remote server restart.
  Evidence: it runs `docker buildx build --push` before SSHing to the server.

## Decision Log

- Decision: Keep `deploy-*` scripts directly runnable and have them call `publish-*` scripts internally.
  Rationale: Relying only on Makefile prerequisites would make `scripts/deploy-backend.sh` unsafe or incomplete when run directly. Simple scripts should preserve their full intent.
  Date/Author: 2026-05-16 / Codex
- Decision: Add `make build`, `make publish`, and focused subtargets without removing older target names.
  Rationale: The new commands provide a cleaner mental model while compatibility aliases avoid unnecessary breakage.
  Date/Author: 2026-05-16 / Codex

## Outcomes & Retrospective

The Makefile now exposes `make build`, `make publish`, and `make deploy` as intent-level commands. Backend image building is extracted into `scripts/build-backend-image.sh`; backend image publishing is extracted into `scripts/publish-backend.sh`; frontend/homepage publishing into `scripts/publish-frontend.sh`; and daemon static artifact publishing into `scripts/publish-static.sh`. Deploy scripts remain safe to invoke directly because they call the publish scripts themselves. Local validation passed for shell syntax, Go build, frontend build, daemon build, local daemon static build, aggregate `make build`, and `make test-go`. Production publish and deploy were intentionally not executed because they require DockerHub, Cloudflare R2, and SSH production credentials.

## Context and Orientation

The repository currently has a `Makefile` with development, testing, static build, daemon build, and deploy targets. Static assets are built by `scripts/build-static.sh`; this script can build the frontend/homepage assets, daemon release assets, or both depending on `STATIC_BUILD_TARGET`. R2 uploads are handled by `scripts/publish-static-r2.sh`. Production backend deployment is handled by `scripts/deploy-backend.sh`, which currently builds and pushes the backend Docker image and then restarts the remote Compose stack. The goal is not to add many aliases; it is to make the few top-level commands match the lifecycle of artifacts.

In this plan, "build" means creating local files or images without publishing them, "publish" means pushing/uploading immutable artifacts to DockerHub or R2, and "deploy" means making production use a published artifact.

## Plan of Work

Add `scripts/publish-backend.sh` by extracting the backend Docker build-and-push portion from `scripts/deploy-backend.sh`. Add `scripts/publish-frontend.sh` and `scripts/publish-static.sh` by moving the R2 build-and-publish behavior currently in `scripts/deploy-frontend.sh` and `scripts/deploy-static.sh` into scripts whose names match their behavior. Then reduce the deploy scripts to wrappers: frontend/static deploys call their publish scripts, and backend deploy calls backend publish before SSHing to the production server.

Update `Makefile` so `make build` runs local compilation checks, `make publish` runs the three publish jobs, and `make deploy` continues to run the three deploy jobs. Keep existing target names like `daemon-build`, `static-build`, `static-build-local`, `static-publish`, and `backend-image` as aliases or focused lower-level targets so current workflows do not break.

Update the README to document the simpler command surface.

## Concrete Steps

From `/Users/zhongyangxia/Downloads/notty`, edit the Makefile and deploy scripts with `apply_patch`. Then run:

    sh -n scripts/publish-backend.sh scripts/publish-frontend.sh scripts/publish-static.sh scripts/deploy-backend.sh scripts/deploy-frontend.sh scripts/deploy-static.sh scripts/deploy-notty.sh
    make build-go
    make build-frontend
    make build-daemon
    make build-static-local

The publish and deploy scripts require DockerHub, Cloudflare R2, and SSH credentials, so full production publishing is not part of local validation unless explicitly requested.

## Validation and Acceptance

Acceptance is met when `make build` creates local build artifacts without publishing, `make publish-*` commands exist for each publishable artifact group, and `make deploy-*` still remains a single-command deployment path. Shell syntax checks must pass for every touched script. The local build targets must pass without using production credentials.

## Idempotence and Recovery

The edits are idempotent. Running `make build` repeatedly overwrites local build artifacts in `bin`, `frontend/dist`, and `dist/static`. Publish and deploy commands remain explicit and keep their existing environment-variable configuration. If a publish fails halfway, rerun the same publish target after fixing credentials or network access.

## Artifacts and Notes

This plan intentionally avoids adding a large command matrix. The top-level mental model is `dev`, `tests`, `build`, `publish`, and `deploy`.

Validation evidence:

    sh -n scripts/publish-backend.sh scripts/publish-frontend.sh scripts/publish-static.sh scripts/deploy-backend.sh scripts/deploy-frontend.sh scripts/deploy-static.sh scripts/deploy-notty.sh scripts/build-static.sh scripts/publish-static-r2.sh
    make build-go
    make build-daemon
    make build-frontend
    make build-static-local
    make build
    make test-go

All listed commands completed successfully. `make -n publish deploy` showed the expected publish and deploy entrypoints without executing network operations.

## Interfaces and Dependencies

The public Makefile targets at the end of this work are:

    make build
    make publish
    make deploy
    make tests

Focused targets include:

    make build-go
    make build-frontend
    make build-daemon
    make build-static-local
    make publish-backend
    make publish-frontend
    make publish-static
    make deploy-backend
    make deploy-frontend
    make deploy-static

The new scripts are:

    scripts/publish-backend.sh
    scripts/publish-frontend.sh
    scripts/publish-static.sh

Backend image builds use:

    scripts/build-backend-image.sh

Set `BACKEND_IMAGE_MODE=push` to build and push the multi-platform production image. Leave it unset for a local `docker buildx build --load` image.
