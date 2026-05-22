ARG GO_VERSION=1.26
ARG NODE_MAJOR=24

FROM golang:${GO_VERSION}-bookworm AS go-toolchain

FROM node:${NODE_MAJOR}-bookworm-slim

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG CODEX_CLI_VERSION=latest
ARG RUSTUP_TOOLCHAIN=stable

ENV CARGO_HOME=/root/.cargo \
    CODEX_HOME=/root/.codex \
    DEBIAN_FRONTEND=noninteractive \
    DOCKER_HOST=unix:///var/run/docker.sock \
    GOPATH=/go \
    NPM_CONFIG_CACHE=/root/.slock/npm-cache \
    RUSTUP_HOME=/root/.rustup \
    SLOCK_HOME=/root/.slock \
    SLOCK_SERVER_URL=https://api.slock.ai

ENV PATH="/usr/local/go/bin:/go/bin:/root/.cargo/bin:${PATH}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        build-essential \
        ca-certificates \
        curl \
        git \
        jq \
        libssl-dev \
        openssh-client \
        pkg-config \
        python3 \
        ripgrep \
        sudo \
        xz-utils \
        zsh \
    && rm -rf /var/lib/apt/lists/*

RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "${VERSION_CODENAME}") stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        docker-buildx-plugin \
        docker-ce-cli \
        docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod a+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && gh --version \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /workspace "${CODEX_HOME}" "${SLOCK_HOME}" "${NPM_CONFIG_CACHE}" \
    && chmod 700 "${CODEX_HOME}" "${SLOCK_HOME}"

COPY --from=go-toolchain /usr/local/go /usr/local/go

RUN curl -fsSL https://sh.rustup.rs \
    | sh -s -- -y --profile minimal --default-toolchain "${RUSTUP_TOOLCHAIN}" \
    && rustc --version \
    && cargo --version

RUN npm install -g "@openai/codex@${CODEX_CLI_VERSION}" \
    && codex --version \
    && npm cache clean --force

COPY entrypoint.sh /usr/local/bin/slock-agent-entrypoint
RUN chmod +x /usr/local/bin/slock-agent-entrypoint

WORKDIR /workspace

VOLUME ["/workspace", "/root/.codex", "/root/.slock"]

ENTRYPOINT ["/usr/local/bin/slock-agent-entrypoint"]
CMD ["slock-daemon"]
