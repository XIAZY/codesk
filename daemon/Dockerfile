FROM golang:1.23-alpine AS build
WORKDIR /app
RUN apk add --no-cache build-base cargo
COPY go.mod go.sum ./
COPY DAEMON_VERSION ./DAEMON_VERSION
COPY third_party ./third_party
COPY scripts/build-yffi.sh ./scripts/build-yffi.sh
COPY scripts/read-daemon-version.sh ./scripts/read-daemon-version.sh
RUN go mod download
RUN scripts/build-yffi.sh
COPY backend ./backend
COPY daemon ./daemon
COPY internal ./internal
RUN version="$(scripts/read-daemon-version.sh)" && CGO_ENABLED=1 GOOS=linux go build -ldflags "-X notty/daemon/internal/buildinfo.Version=$version" -o /bin/notty-daemon ./daemon/cmd/daemon
RUN version="$(scripts/read-daemon-version.sh)" && CGO_ENABLED=0 GOOS=linux go build -ldflags "-X notty/daemon/internal/buildinfo.Version=$version" -o /bin/notty-agent-tool ./daemon/cmd/agenttool

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache nodejs npm libgcc bash
ARG CODEX_CLI_VERSION=latest
RUN npm install -g "@openai/codex@${CODEX_CLI_VERSION}" && codex --version
ARG CLAUDE_CLI_VERSION=latest
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CLI_VERSION}" && claude --version
COPY --from=build /bin/notty-daemon /usr/local/bin/notty-daemon
COPY --from=build /bin/notty-agent-tool /usr/local/bin/notty-agent-tool
# The daemon runs as root in this container — the only supported root context —
# so declare the sandbox that lets the claude CLI accept
# --dangerously-skip-permissions. Claude's Bash tool also requires a supported
# POSIX shell in SHELL; Alpine's BusyBox /bin/sh is not sufficient.
ENV IS_SANDBOX=1 SHELL=/bin/bash
CMD ["notty-daemon"]
