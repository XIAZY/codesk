# Host-native daemon installation

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`.

## Purpose / Big Picture

Users should be able to deploy a Notty daemon without cloning the repository or running the product's development Docker Compose stack. After this change, the frontend daemon creation flow will show a one-line command that downloads a hosted installer from a static daemon artifact path, passes the workspace ID and one-time daemon token, and installs a host-native daemon service. A release builder can create versioned daemon tarballs containing both `notty-daemon` and `notty-agent-tool`, plus a manifest and checksums that Nginx can serve from a static subdomain.

## Progress

- [x] (2026-05-14) Added initial daemon build/release Make targets and tarball packaging before this ExecPlan was created.
- [x] (2026-05-14) Added a repository-owned hosted installer script that downloads, verifies, installs, configures, and starts the daemon.
- [x] (2026-05-14) Updated the release packaging script to publish the installer beside versioned artifacts.
- [x] (2026-05-14) Added an example Nginx static hosting config for `/daemons`.
- [x] (2026-05-14) Updated frontend command generation so daemon creation displays the hosted installer command instead of Docker Compose.
- [x] (2026-05-14) Added focused tests for install command construction because token, URL, and shell quoting bugs would break user deployment.
- [x] (2026-05-14) Ran build/test validation and recorded results.
- [x] (2026-05-14) Rebuilt the running frontend container after discovering it still served a 47-hour-old bundle with the old Docker Compose daemon command.
- [x] (2026-05-14) Wired frontend public origin/static configuration as Docker build args so production frontend containers bake the intended deploy command values.
- [x] (2026-05-14) Removed the placeholder static-domain default from code and docs.
- [x] (2026-05-14) Added an Nginx gateway service to Docker Compose to serve the frontend, proxy backend HTTP/websocket traffic, and serve daemon static files.

## Surprises & Discoveries

- Observation: The frontend TypeScript target does not include `String.prototype.replaceAll`.
  Evidence: `npm test --prefix frontend` initially failed with `TS2550: Property 'replaceAll' does not exist on type 'string'`. The shell quoting helper now uses regex replacement instead.

- Observation: The user's running frontend container was stale even though the source had already changed.
  Evidence: `docker compose exec -T frontend grep ... /app/src /app/dist` showed `/app/src/App.tsx` still contained `docker compose --profile daemon up -d --build daemon`, and `docker compose ps` showed `notty-frontend-1` was created 47 hours earlier. Rebuilding recreated the container and the bundle no longer contains that string.

- Observation: Runtime `environment: VITE_*` values do not affect a Vite production bundle served by `vite preview`.
  Evidence: The frontend Dockerfile builds static assets with `npm run build`, so Vite reads `VITE_*` during the image build. Docker Compose now passes these values as build args.

- Observation: The placeholder static domain had leaked into real defaults.
  Evidence: A repository search found the placeholder in frontend source, installer defaults, Compose build args, README examples, and Nginx sample config. Code now derives daemon static base from the configured public origin unless explicitly overridden.

## Decision Log

- Decision: The end-user path is host-native installation, not Docker Compose.
  Rationale: Docker Compose remains the source-checkout development path for the full local stack. External users who only need to run a daemon should not need the source tree, so they get a daemon binary plus helper binary through the hosted installer.
  Date/Author: 2026-05-14 / Codex

- Decision: Keep Docker Compose as the easy full-stack development command.
  Rationale: The deployment problem is specifically external daemon installation. It should not remove or weaken the existing local development workflow.
  Date/Author: 2026-05-14 / Codex

- Decision: Pass frontend public configuration as Docker build args, not only runtime environment variables.
  Rationale: Vite replaces `import.meta.env` at build time. Build args are the minimal fix for static preview containers.
  Date/Author: 2026-05-14 / Codex

- Decision: Add Nginx as the public gateway while keeping backend and frontend direct dev ports available.
  Rationale: Public deployments need one externally reachable origin for frontend assets, backend APIs, websockets, and daemon artifacts. Keeping direct ports avoids disrupting existing local debugging.
  Date/Author: 2026-05-14 / Codex

- Decision: The installer is served as a static shell script and receives `--backend-url`, `--workspace-id`, `--daemon-token`, and `--static-base`.
  Rationale: A piped shell script cannot reliably know the URL it came from, so the frontend command passes the static base explicitly. This also supports custom domains.
  Date/Author: 2026-05-14 / Codex

- Decision: Command generation lives in frontend logic with unit tests rather than being hardcoded inside the modal.
  Rationale: The command contains user token material and shell quoting; keeping it as a pure function makes it cheap to test and avoids UI-only regressions.
  Date/Author: 2026-05-14 / Codex

## Outcomes & Retrospective

Implemented the host-native daemon deployment path. Release builders can set the canonical version in the root `DAEMON_VERSION` file, run `make daemon-release`, and serve `dist/daemons` from Nginx. Users creating a daemon in the frontend now receive a hosted installer command instead of a Docker Compose command. The installer supports Linux/macOS on amd64/arm64, checksum verification, workspace-scoped config, LaunchAgent/systemd user services, and a background-process fallback.

## Context and Orientation

The daemon is the local process that syncs workspace documents to disk and supervises Codex agents. Its entry point is `daemon/cmd/daemon/main.go`. Agents use a helper CLI at `daemon/cmd/agenttool/main.go`; both binaries must be installed together. The current frontend daemon creation modal lives in `frontend/src/App.tsx` and currently shows a Docker Compose command. Shared frontend logic and tests live in `frontend/src/logic.ts` and `frontend/src/logic.test.ts`.

Release packaging is rooted at `Makefile` and `scripts/build-daemon-release.sh`. Generated artifacts go under `dist/daemons`, which is ignored by Git. Nginx static hosting will serve those generated files from a static subdomain or a `/daemons` path.

## Plan of Work

Add `deploy/daemons/install.sh` as a POSIX shell installer. It detects Linux or macOS and amd64 or arm64, downloads `manifest.json` and `SHA256SUMS`, fetches the matching tarball, verifies SHA256, installs `notty-daemon` and `notty-agent-tool` under `~/.notty/bin` by default, writes a workspace-scoped env file under `~/.notty/daemons/<workspace-id>/daemon.env`, writes a runner script that prepends the install directory to `PATH`, and starts a LaunchAgent on macOS, a systemd user service on Linux, or a background process as a fallback.

Update `scripts/build-daemon-release.sh` so every release output contains `install.sh` at `dist/daemons/install.sh` and a copy in the version directory. Add `deploy/nginx/notty-static.conf` as a sample static server config with immutable caching for versioned tarballs and short caching for `latest` and `install.sh`.

Update frontend logic to build a shell-safe install command from API base URL, static artifact base URL, workspace ID, and one-time daemon token. The modal should display "Install daemon" and the hosted installer command instead of "Deploy with Docker Compose". Tests should assert that command generation trims trailing slashes and shell-quotes unsafe values.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run `make daemon-build` to verify native binaries.

Observed:

    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/notty-daemon ./daemon/cmd/daemon
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/notty-agent-tool ./daemon/cmd/agenttool

Run `make daemon-release PLATFORMS=darwin/arm64` to verify packaging and static installer placement with the canonical root `DAEMON_VERSION`.

Observed:

    Built daemon release test in /Users/zhongyangxia/Downloads/notty/dist/daemons/test

Run `npm test --prefix frontend` to verify frontend command generation and type safety.

Observed:

    Test Files  2 passed (2)
    Tests  11 passed (11)

Run `sh -n deploy/daemons/install.sh` to catch installer syntax errors.

Observed:

    sh -n deploy/daemons/install.sh

Run `npm run build --prefix frontend` to verify the production frontend bundle.

Observed:

    vite v5.4.21 building for production...
    ✓ built in 600ms

Browser smoke check:

    Opened http://localhost:5173 and observed the auth screen rendering with title `notty`.

After rebuilding the stale Compose frontend container:

    docker compose exec -T frontend sh -lc "grep -R \"docker compose --profile daemon\" -n /app/src /app/dist 2>/dev/null || true"

Observed no output. The current bundle includes `Install daemon` and `curl -fsSL`.

    node -e "fetch('http://localhost:5173').then(async r=>{const text=await r.text(); console.log('frontend', r.status, text.match(/index-[^\"']+\\.js/)?.[0] || text.slice(0,120));})"

Observed:

    frontend 200 index-BTfZry7n.js

After adding the Nginx gateway:

    docker compose exec -T nginx nginx -t

Observed:

    nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
    nginx: configuration file /etc/nginx/nginx.conf test is successful

    node -e "fetch('http://localhost:8088/healthz').then(async r=>console.log('healthz',r.status,await r.text()))"

Observed:

    healthz 200 {"status":"ok"}

    node -e "fetch('http://localhost:8088').then(async r=>{const text=await r.text(); console.log('frontend',r.status,text.includes('root'))})"

Observed:

    frontend 200 true

    node -e "fetch('http://localhost:8088/daemons/install.sh').then(async r=>{const text=await r.text(); console.log('install',r.status,text.slice(0,60).replace(/\n/g,' '))})"

Observed:

    install 200 #!/usr/bin/env sh set -eu

Browser smoke check through the gateway:

    Opened http://localhost:8088 and observed the auth screen rendering with title `notty`.

## Validation and Acceptance

The release directory should contain `dist/daemons/install.sh`, `dist/daemons/latest/manifest.json`, `dist/daemons/latest/SHA256SUMS`, and a versioned tarball containing both binaries. The frontend daemon modal should show a command shaped like:

    curl -fsSL <public-origin>/daemons/install.sh | sh -s -- --backend-url <public-origin> --workspace-id ws_xxx --daemon-token nottyd_xxx --static-base <public-origin>/daemons

The command should not mention Docker Compose.

## Idempotence and Recovery

The build and release commands are safe to rerun because they replace generated `dist/daemons/<version>` output. The installer overwrites the daemon env file and runner script for the same workspace ID and restarts the service if the platform service manager is available. Generated `bin/` and `dist/` files are ignored by Git.

## Artifacts and Notes

Validation output will be added after implementation.

## Interfaces and Dependencies

The frontend should expose:

    buildDaemonInstallCommand(input: {
      backendUrl: string;
      workspaceId: string;
      daemonToken: string;
      staticBaseUrl?: string;
    }): string

The installer should accept:

    --backend-url <url>
    --workspace-id <id>
    --daemon-token <token>
    --static-base <url>
    --version <version>
    --install-dir <path>
    --data-dir <path>
    --no-service
