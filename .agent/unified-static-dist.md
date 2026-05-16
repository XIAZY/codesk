# Unified Static Distribution Root

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`.

## Purpose / Big Picture

Notty has three kinds of static assets: the Vite application, the homepage, and daemon install artifacts. Before this change, local development served daemon scripts directly from `deploy/daemons`, while production published generated daemon releases from a different directory. That made local daemon installation depend on accidental files in the source tree. After this change, both local and production use one generated static root, `dist/static`, with `app`, `homepage`, and `daemons` subdirectories. A developer can start local dev and expect `http://localhost:5174/daemons/install.sh` and `http://localhost:5174/daemons/latest/manifest.json` to exist without manually copying artifacts.

## Progress

- [x] (2026-05-16T01:27:36Z) Created this self-contained plan after reading `.agent/PLANS.md` and the current static/deployment scripts.
- [x] (2026-05-16T01:32:00Z) Updated the static build scripts so daemon artifacts are generated under `dist/static/daemons`.
- [x] (2026-05-16T01:34:00Z) Updated local Docker Compose and Makefile targets so local static serving uses `dist/static`.
- [x] (2026-05-16T01:35:00Z) Updated production publishing defaults so daemon publishing reads from `dist/static/daemons`.
- [x] (2026-05-16T01:36:00Z) Updated README documentation to describe the unified static root.
- [x] (2026-05-16T01:39:00Z) Ran installer/static validation commands and recorded outcomes.
- [x] (2026-05-16T01:44:00Z) Updated the direct daemon release defaults to `dist/static/daemons` so all static-serving outputs use the unified root.

## Surprises & Discoveries

- Observation: The local static service currently mounts `./deploy/daemons`, which contains source scripts only unless someone manually generated release directories there.
  Evidence: `find deploy/daemons -maxdepth 3 -type f` showed only `install.sh` and `uninstall.sh` before local release artifacts were generated.

- Observation: Restarting the static service after changing the mount was required before `http://127.0.0.1:5174/daemons/latest/manifest.json` reflected `dist/static`.
  Evidence: `docker compose --env-file deploy/env/dev.server.env up -d static` recreated `notty-static-1`, then `curl -fsS http://127.0.0.1:5174/daemons/latest/manifest.json` returned a `dev` manifest for `darwin/arm64`.

## Decision Log

- Decision: Use `dist/static` as the only generated static root, with daemon artifacts under `dist/static/daemons`.
  Rationale: This keeps source files under `deploy/daemons` separate from generated tarballs and manifests while preserving one URL shape for local and production static assets.
  Date/Author: 2026-05-16 / Codex

- Decision: Keep targeted build modes in `scripts/build-static.sh`.
  Rationale: Production and local workflows need the same output layout, but deploying only frontend or only daemon artifacts should not rebuild unrelated assets every time.
  Date/Author: 2026-05-16 / Codex

- Decision: Make `make dev` depend on `static-build-local` rather than requiring direct `docker compose up` to build artifacts.
  Rationale: Docker Compose should run services, while Make can prepare generated host artifacts first. This keeps the static container simple and avoids adding Go/Node build concerns to the static service.
  Date/Author: 2026-05-16 / Codex

- Decision: Change `scripts/build-daemon-release.sh` and `make daemon-release` defaults from `dist/daemons` to `dist/static/daemons`.
  Rationale: The old default kept a second generated daemon artifact tree. A unified static root is easier to reason about and matches the local static server mount.
  Date/Author: 2026-05-16 / Codex

## Outcomes & Retrospective

Completed. Local development now uses `dist/static` as the static service root. `make static-build-local` builds `dist/static/daemons` with `VERSION=dev` for the current host platform. `docker-compose.yml` serves `dist/static`, so the local daemon installer URL and production daemon installer URL have the same path shape. Production publishing and direct daemon release builds read and write daemon artifacts from `dist/static/daemons` by default.

The temporary generated directories `deploy/daemons/dev` and `deploy/daemons/latest` were removed after the static service moved to `dist/static`, leaving `deploy/daemons` source-only again.

## Context and Orientation

`scripts/build-static.sh` builds into `dist/static`. With `STATIC_BUILD_TARGET=frontend`, it builds the Vite frontend into `dist/static/app` and copies the homepage into `dist/static/homepage`. With `STATIC_BUILD_TARGET=daemons`, it calls `scripts/build-daemon-release.sh` to create daemon tarballs, checksums, manifests, and copies of `deploy/daemons/install.sh` and `deploy/daemons/uninstall.sh` under `dist/static/daemons`. With no target, it builds all three static asset groups. `docker-compose.yml` serves `./dist/static` at `/srv/static`, so `/daemons/install.sh` comes from the same generated root as app and homepage assets. `scripts/publish-static-r2.sh` publishes frontend assets from `dist/static/app` and `dist/static/homepage`, and daemon assets from `dist/static/daemons` by default.

The term "daemon artifacts" means the hosted files consumed by `deploy/daemons/install.sh`: `install.sh`, `uninstall.sh`, `latest/manifest.json`, `latest/SHA256SUMS`, and versioned tarballs such as `notty-daemon_dev_darwin_arm64.tar.gz`.

## Plan of Work

First, update `scripts/build-static.sh` so it can build `frontend`, `daemons`, or `all`, defaulting to `all`. The daemon target will call `scripts/build-daemon-release.sh` with its output directory set to `dist/static/daemons`.

Second, update `scripts/publish-static-r2.sh` so its default daemon distribution directory is `dist/static/daemons`. Update `scripts/deploy-static.sh` to build only the daemon target through `scripts/build-static.sh`, and update `scripts/deploy-frontend.sh` to build only the frontend/homepage target.

Third, update `docker-compose.yml` so the local static service mounts `./dist/static` instead of `./deploy/daemons`. Update the Makefile with a host-platform-aware `static-build-local` target and make `make dev` depend on it.

Fourth, update README sections that describe local and production static serving. Then validate by building the local daemon static tree, restarting the static service, fetching the local manifest, and running the installer check suite.

## Concrete Steps

Run commands from `/Users/zhongyangxia/Downloads/notty`.

After editing scripts, run:

    make daemon-installer-check
    make static-build-local
    docker compose --env-file deploy/env/dev.server.env up -d static

Then verify:

    curl -fsS http://127.0.0.1:5174/daemons/latest/manifest.json

Expected result: the response is JSON with `"version": "dev"` and an artifact matching the host operating system and architecture.

Observed validation:

    make static-build-local
    # Built daemon release dev in /Users/zhongyangxia/Downloads/notty/dist/static/daemons/dev
    # Built static assets:
    #   daemons: dist/static/daemons

    make daemon-installer-check
    # daemon installer tests passed

    curl -fsS http://127.0.0.1:5174/daemons/latest/manifest.json
    # {
    #   "version": "dev",
    #   "artifacts": [
    #     {"os": "darwin", "arch": "arm64", "file": "notty-daemon_dev_darwin_arm64.tar.gz", "...": "..."}
    #   ]
    # }

    curl -fsS http://127.0.0.1:5174/daemons/install.sh | NOTTY_INSTALL_DIR=/tmp/notty-install-unified/bin NOTTY_DATA_DIR=/tmp/notty-install-unified/data sh -s -- --backend-url http://localhost:8080 --workspace-id ws_test --daemon-token nottyd_test --static-base http://localhost:5174/daemons --no-service
    # Installing Notty daemon dev for darwin/arm64
    # Installed daemon binaries and config. Start manually with: /tmp/notty-install-unified/data/daemons/ws_test/run.sh
    # Notty daemon install complete.

## Validation and Acceptance

Acceptance is met when local development serves daemon installer artifacts from `dist/static/daemons`, the installer regression tests pass, and the local static endpoint returns a valid `latest/manifest.json`. Production acceptance is met by preserving the existing R2 publishing commands while changing only their default source path to the unified static root.

## Idempotence and Recovery

`scripts/build-static.sh` and `scripts/build-daemon-release.sh` remove and recreate only generated output under `dist/static`, which is ignored by git. Re-running the build is safe. If local static files become stale, rerun `make static-build-local` and restart the static service.

## Artifacts and Notes

Important files changed by this plan are expected to include `scripts/build-static.sh`, `scripts/publish-static-r2.sh`, `scripts/deploy-static.sh`, `scripts/deploy-frontend.sh`, `docker-compose.yml`, `Makefile`, and `README.md`.

## Interfaces and Dependencies

`scripts/build-static.sh` must support `STATIC_BUILD_TARGET=all`, `STATIC_BUILD_TARGET=frontend`, and `STATIC_BUILD_TARGET=daemons`. `STATIC_DIST_DIR` remains the root output directory and defaults to `dist/static`. For daemon builds, `VERSION` controls the daemon release version and `PLATFORMS` controls the platform list passed to `scripts/build-daemon-release.sh`.
