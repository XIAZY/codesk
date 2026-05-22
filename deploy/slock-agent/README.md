# Slock Agent Image

This image is intended to run Slock agents with Codex, GitHub CLI, Node.js, Go, Rust, and Docker CLI tooling available in the container.

Build it from this directory:

```sh
docker build -t slock-agent:local .
```

The default build uses Node.js 24, Go 1.26, stable Rust via rustup, the latest Codex CLI, GitHub CLI from GitHub's Debian package repo, and Docker CLI tooling from Docker's Debian repository. Override the language/tool versions with build args when needed:

```sh
docker build \
  --build-arg NODE_MAJOR=24 \
  --build-arg GO_VERSION=1.26 \
  --build-arg RUSTUP_TOOLCHAIN=stable \
  --build-arg CODEX_CLI_VERSION=latest \
  -t slock-agent:local .
```

Run it with persistent volumes, mounted Codex auth, and access to the host Docker daemon:

```sh
docker run -d --name slock-agent \
  -e SLOCK_API_KEY="$SLOCK_API_KEY" \
  -e SLOCK_SERVER_URL="https://api.slock.ai" \
  -v slock_workspace:/workspace \
  -v slock_codex:/root/.codex \
  -v slock_state:/root/.slock \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$HOME/.codex/auth.json:/run/host-codex/auth.json:ro" \
  slock-agent:local
```

The entrypoint copies `/run/host-codex/auth.json` into `/root/.codex/auth.json` on startup. Codex identity is retained inside the persistent `slock_codex` volume, and Slock state plus the npm cache used by `npx` are retained inside the persistent `slock_state` volume.

Because `.slock` is persisted, the entrypoint also prunes stale daemon locks left by a previous container instance. Set `SLOCK_PRUNE_STALE_LOCKS=0` if you intentionally run multiple daemons against the same persisted Slock state.

Docker access is provided by the host socket mount. Commands such as `docker ps`, `docker build`, and `docker compose` run inside the container but operate against the host Docker daemon.

The Compose file reads runtime credentials from the ignored local `.env` file in this directory:

```sh
SLOCK_API_KEY=sk_machine_redacted
SLOCK_SERVER_URL=https://api.slock.ai
GH_TOKEN=github_pat_redacted
```

Keep the real key in `.env` or your container platform secret store rather than baking it into the image.

For GitHub CLI, paste a GitHub personal access token into `GH_TOKEN`. When `GH_TOKEN` is set, the container uses that token directly and does not need host `gh` keychain auth.

You can also use the included Compose file:

```sh
cd deploy/slock-agent
docker compose up -d --build
```
