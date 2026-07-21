# Split static hosting from backend deployment

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. This document follows `.agent/PLANS.md`.

## Purpose / Big Picture

Notty should be easy to run locally and easy to deploy to production without requiring source files on the server. After this change, local development uses Docker Compose for `postgres`, `backend`, and the Vite frontend dev server. Production uses Docker Compose only for the Go backend, connects to an external Postgres database through environment variables, and serves all static content from object/CDN hosting such as Cloudflare R2. Static content means files that do not need a server process after build: the homepage, compiled React frontend, daemon installer, and daemon release tarballs.

The observable local outcome is that `docker compose up --build` starts the local backend, frontend, and database without nginx. The observable production-prep outcome is that scripts can build frontend/static artifacts, daemon release artifacts, and a backend Docker image suitable for pushing to Docker Hub, while leaving actual deployment to an explicit deploy script.

## Progress

- [x] (2026-05-15 00:00Z) Read the existing compose file, frontend build config, daemon release script, and deployment assumptions.
- [x] (2026-05-15 00:05Z) Decide to keep `docker-compose.yml` as the default local-dev compose file so `docker compose up` remains the simple local command.
- [x] (2026-05-15 00:20Z) Add production backend-only compose configuration with all secrets supplied by `/opt/notty/.env` or caller-provided environment.
- [x] (2026-05-15 00:25Z) Add scripts for static builds, R2-compatible publishing, and remote backend deployment to SSH host `notty`.
- [x] (2026-05-15 00:30Z) Remove production nginx/static serving assumptions from the default compose path.
- [x] (2026-05-15 00:35Z) Update README deployment documentation.
- [x] (2026-05-15 00:45Z) Test the local dev compose path.
- [x] (2026-05-15 00:50Z) Align local frontend networking with production by using explicit `VITE_API_BASE` instead of Vite proxy defaults.
- [x] (2026-05-15 01:20Z) Add production nginx API proxy config with browser-facing CORS and websocket handling.
- [x] (2026-05-15 05:30Z) Test R2 publishing against the configured R2 bucket; upload was blocked before object creation because Wrangler requires an explicit `CLOUDFLARE_ACCOUNT_ID` for the provided token.
- [x] (2026-05-15 05:35Z) Confirm actual Cloudflare bucket name is `notty-staic-prod`, then align repo defaults with that spelling.
- [x] (2026-05-15 05:45Z) User deleted typo bucket; restore repo defaults to `notty-static-prod`.
- [x] (2026-05-15 05:50Z) Deploy static artifacts to `notty-static-prod` and verify key objects can be fetched back from R2.
- [x] (2026-05-15 06:05Z) Add tracked non-secret production defaults and make deploy scripts load them automatically.
- [x] (2026-05-15 06:20Z) Split env files into `deploy/env/prod.deploy.env`, `deploy/env/prod.server.env`, and `deploy/env/dev.server.env`.
- [x] (2026-05-15 06:30Z) Connect `nottyai.co` to the R2 static bucket and add a root-path rewrite to `homepage/index.html`.
- [x] (2026-05-15 06:45Z) Replace shared static bucket routing with dedicated R2 buckets for homepage, app, and daemon artifacts; remove the transform rewrite.
- [x] (2026-05-15 07:00Z) Split deployment entrypoints into frontend, daemon static, backend, and full-release scripts.
- [x] (2026-05-15 07:20Z) Move production nginx into Docker Compose so backend remains private on the Compose network.

## Surprises & Discoveries

- Observation: The frontend is a Vite React single-page application, not a runtime Node server. Node is needed for local development and building, but production can serve the compiled `frontend/dist` directory from static hosting.
  Evidence: `frontend/package.json` has `vite build`; `frontend/src/main.tsx` mounts React into `#root`; no server-side rendering code exists.

- Observation: The current `frontend/Dockerfile` builds static files and then runs `vite preview`, which is unnecessary for production once static hosting is used.
  Evidence: `frontend/Dockerfile` ends with `CMD ["npm", "exec", "vite", "preview", ...]`.

- Observation: Docker build context had no `.dockerignore`, so generated directories like root `node_modules`, `dist`, and archive files could be sent to Docker builds.
  Evidence: `ls -la .dockerignore` returned no file before this change.

- Observation: The frontend lockfile was not clean-installable in the Node 22 Alpine container.
  Evidence: `docker compose up --build -d postgres backend frontend` failed during `RUN npm ci` with missing lockfile entries for `@emnapi/core`, `@emnapi/runtime`, and `esbuild`. Regenerating `frontend/package-lock.json` inside `node:22-alpine` fixed the clean install.

- Observation: After separating static frontend and backend in production, keeping Vite proxy as the local default made local browser networking less representative than production.
  Evidence: `frontend/vite.config.ts` proxied `/api`, `/healthz`, `/ws`, and `/daemons`; production static builds instead use `VITE_API_BASE=https://api.nottyai.co`.

- Observation: Backend CORS remains permissive for direct local development, so production nginx must hide upstream `Access-Control-*` headers if nginx also owns CORS.
  Evidence: `backend/internal/notty/server_http.go` sets `Access-Control-Allow-Origin: *`; `deploy/nginx/notty-api.conf` uses `proxy_hide_header` for upstream CORS headers.

- Observation: The available Cloudflare token is present but cannot list account IDs, so non-interactive Wrangler R2 publishing needs `CLOUDFLARE_ACCOUNT_ID`.
  Evidence: `npx wrangler r2 bucket list` and `scripts/publish-static-r2.sh` both failed with "Failed to automatically retrieve account IDs" before uploading `homepage/index.html`.

- Observation: The created R2 bucket is named `notty-staic-prod`, not `notty-static-prod`.
  Evidence: `npx wrangler r2 bucket list` with `CLOUDFLARE_ACCOUNT_ID=d8cad58199d138868f519a3a55c7e3c5` returned `name: notty-staic-prod`.

- Observation: The typo bucket was intentionally deleted and the desired production bucket name is `notty-static-prod`.
  Evidence: User said "sorry, typo. i've deleted the bucket. please deploy to notty-static-prod".

- Observation: R2 custom domains serve exact object keys and do not automatically map `/` to `/index.html`.
  Evidence: After connecting `nottyai.co` to `notty-homepage-prod` and deleting the transform rewrite, `https://nottyai.co/` returned 404 while `https://nottyai.co/index.html` returned the homepage.

- Observation: R2 accepts an empty object key, and custom-domain root requests map to that key.
  Evidence: Uploading `dist/static/homepage/index.html` to object key `""` in `notty-homepage-prod` made `https://nottyai.co/` return HTTP 200 with the same SHA-256 hash as the local homepage artifact.

## Decision Log

- Decision: Keep `docker-compose.yml` as the local-dev compose file instead of renaming it to `compose.dev.yml`.
  Rationale: The user asked for an easy local flow. Preserving `docker compose up --build` minimizes local friction while still allowing production to use a separate `compose.prod.yml`.
  Date/Author: 2026-05-15 / Codex

- Decision: Production compose will run only the backend container.
  Rationale: Static content belongs in R2/CDN, and production Postgres is remote. Running frontend, nginx, or Postgres containers in production would duplicate responsibilities and require source-mounted files or extra runtime processes.
  Date/Author: 2026-05-15 / Codex

- Decision: R2 publishing scripts will use the AWS CLI against an S3-compatible endpoint.
  Rationale: Cloudflare R2 exposes an S3-compatible API, and `aws s3 cp/sync --endpoint-url` is widely available. Credentials and bucket names must remain environment variables, not git-tracked values.
  Date/Author: 2026-05-15 / Codex

- Decision: Add `.dockerignore` as part of deployment cleanup.
  Rationale: The new deployment relies on Docker builds. Sending local `node_modules`, `dist`, or archived artifacts into every build is slow and can produce confusing results even though those paths are not source.
  Date/Author: 2026-05-15 / Codex

- Decision: Local development should set `VITE_API_BASE=http://localhost:${NOTTY_BACKEND_PORT:-8080}` and remove Vite proxy defaults.
  Rationale: This matches production behavior more closely: the browser makes direct API and websocket requests to an explicit backend origin. It also removes a proxy layer that could hide CORS or URL bugs.
  Date/Author: 2026-05-15 / Codex

- Decision: Production nginx owns browser-facing API CORS for `api.nottyai.co`.
  Rationale: Static frontend and backend are on different origins in production. Nginx is the public API edge, so it should consistently answer preflight requests, proxy websocket upgrades, and hide upstream backend CORS headers to prevent duplicate/conflicting response headers.
  Date/Author: 2026-05-15 / Codex

- Decision: Wrangler-based static publishing requires explicit `CLOUDFLARE_ACCOUNT_ID`.
  Rationale: Account discovery depends on extra token permissions. A deterministic deploy script should fail before uploading if the target account is ambiguous or undiscoverable.
  Date/Author: 2026-05-15 / Codex

- Decision: Split non-secret env files by runtime boundary.
  Rationale: Deploy-machine config, production backend runtime config, and local development config have different consumers. Keeping them separate avoids pushing deploy-only values onto the server and keeps `.env.example` secrets-only.
  Date/Author: 2026-05-15 / Codex

- Decision: Use separate R2 buckets for homepage, app, and daemon artifacts.
  Rationale: Separate buckets avoid prefix collisions and let each public domain point directly to its bucket. Root requests are handled by publishing the root `index.html` to both `index.html` and the empty object key, avoiding a transform rule.
  Date/Author: 2026-05-15 / Codex

- Decision: Keep separate deploy entrypoints for frontend, daemon static artifacts, and backend.
  Rationale: These deploy units have different dependencies and failure modes. Frontend and daemon artifacts only need build tooling plus R2, while backend deploy requires Docker push and SSH. Separate scripts make retries safer.
  Date/Author: 2026-05-15 / Codex

- Decision: Manage production nginx inside Docker Compose.
  Rationale: Nginx is part of the backend service boundary. Keeping it in Compose makes host matching, CORS, websocket proxying, and upstream isolation versioned with the backend deploy instead of relying on host-level mutable config.
  Date/Author: 2026-05-15 / Codex

## Outcomes & Retrospective

The local development path now starts with `docker compose up --build -d postgres backend frontend` and does not require nginx. The backend health endpoint returned HTTP 200 with `{"status":"ok"}`, and the frontend Vite dev server returned HTTP 200 from `http://127.0.0.1:5173`. `docker compose ps` showed only `postgres`, `backend`, and `frontend` running after stopping the optional daemon profile container left over from earlier runs.

Production deployment is now prepared but not executed. `compose.prod.yml` runs only the backend image and requires external Postgres through `NOTTY_DATABASE_URL`. Static homepage, frontend, and daemon artifacts are built into `dist/static` and `dist/daemons` and can be published through `scripts/publish-static-r2.sh`. `scripts/deploy-notty.sh` wires the intended remote flow for SSH host `notty`, Docker Hub repository `alphatoad/notty`, R2 static publishing, and optional nginx installation, but it was intentionally not run.

## Context and Orientation

The repository currently has `docker-compose.yml`, which starts Postgres, the Go backend, the Vite frontend container, a daemon profile, and nginx. Nginx serves homepage files, frontend traffic, backend API/websocket paths, and daemon static artifacts from local source-mounted directories. That is convenient for a single-machine proof of concept but is not the desired production shape because production static assets should live on CDN/object storage and production database credentials should not be committed.

The frontend lives in `frontend/` and is a browser application built by Vite. During local development, Vite should run as a dev server. In production, Vite should produce static files in `frontend/dist`.

The backend lives in `backend/` and builds into a Go binary with `backend/Dockerfile`. Production should run this backend image and read `NOTTY_DATABASE_URL` and `NOTTY_JWT_SECRET` from an environment file on the remote server.

Daemon binaries are built by `scripts/build-daemon-release.sh` into `dist/daemons`. Those artifacts should be uploaded to static hosting under a `/daemons` path. The install script `deploy/daemons/install.sh` downloads those artifacts.

## Plan of Work

First, rewrite `docker-compose.yml` into a local-development compose file. It should include `postgres`, `backend`, `frontend`, and the optional `daemon` profile. It should not include nginx. The frontend service should run Vite dev mode and receive `VITE_API_BASE=http://localhost:${NOTTY_BACKEND_PORT:-8080}` so browser API and websocket traffic goes directly to the backend, just like production uses `https://api.nottyai.co`.

Second, add `compose.prod.yml`. It should contain a backend service using `alphatoad/notty:backend-latest` by default, with an override variable for a specific image tag. It should not contain Postgres, frontend, nginx, or daemon services. It should require `NOTTY_DATABASE_URL` and `NOTTY_JWT_SECRET` through environment.

Third, add scripts. `scripts/build-static.sh` builds the frontend with production origins and copies the homepage and frontend bundle into `dist/static`. `scripts/publish-static-r2.sh` uploads homepage, app, and daemon artifacts to R2-compatible buckets using the AWS CLI and environment variables. `scripts/deploy-notty.sh` builds daemon artifacts, builds static files, optionally publishes static files, builds and pushes the backend Docker image to `alphatoad/notty`, uploads `compose.prod.yml` to SSH host `notty`, validates remote `.env`, and restarts the backend. The script must not deploy during implementation; it only needs to exist and be shell-checked.

Fourth, update `Makefile` and README so humans have clear commands for local dev, static build, and production deployment.

Finally, validate local dev. Run relevant unit tests and at minimum start the local Docker Compose stack enough to verify backend and frontend health locally. Do not run the production deploy script.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run formatting and tests as appropriate:

    npm --prefix frontend run test
    go test ./backend/internal/notty ./daemon/internal/syncer
    sh -n scripts/build-static.sh scripts/publish-static-r2.sh scripts/deploy-notty.sh

Start local dev:

    docker compose up --build -d postgres backend frontend

Then verify:

    curl -f http://localhost:8080/healthz
    curl -f http://localhost:5173

Observed validation output:

    backend 200 {"status":"ok"}
    frontend 200 <!doctype html>
    npm frontend tests: 2 test files passed, 12 tests passed
    go tests: backend/internal/notty and daemon/internal/syncer passed

Stop local dev when finished:

    docker compose down

## Validation and Acceptance

The local development stack is accepted when `docker compose up --build -d postgres backend frontend` starts without nginx and both `http://localhost:8080/healthz` and `http://localhost:5173` return successful HTTP responses.

The production-prep scripts are accepted when `sh -n` passes for all new shell scripts, `make static-build` creates `dist/static/app/index.html` and `dist/static/homepage/index.html`, and the deploy script clearly refuses to run without required remote or R2 environment rather than embedding secrets in git.

## Idempotence and Recovery

The local compose stack can be started and stopped repeatedly. `scripts/build-static.sh` removes and recreates only generated directories under `dist/static`, which is ignored by git. `scripts/build-daemon-release.sh` already recreates version-specific daemon release output. `scripts/publish-static-r2.sh` should upload versioned daemon artifacts before updating `latest` metadata to avoid publishing a partial latest release.

## Artifacts and Notes

Important generated paths are ignored by git:

    dist/static
    dist/daemons
    bin

The production server must keep `/opt/notty/.env` outside git. It should contain at least:

    NOTTY_DATABASE_URL=postgres://...
    NOTTY_JWT_SECRET=...

## Interfaces and Dependencies

The new scripts require standard shell utilities. `scripts/publish-static-r2.sh` can use either `aws` CLI configured through `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `R2_ENDPOINT_URL`, or Wrangler via `CLOUDFLARE_ACCOUNT_ID` plus `CLOUDFLARE_API_TOKEN` or `NOTTY_CLOUDFLARE_TOKEN`. Docker image building requires Docker Buildx. Remote deployment requires `ssh` and `scp` access to host `notty`.
