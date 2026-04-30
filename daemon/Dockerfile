FROM golang:1.23-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY backend ./backend
COPY daemon ./daemon
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/notty-daemon ./daemon/cmd/daemon
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/notty-agent-tool ./daemon/cmd/agenttool

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache nodejs npm
ARG CODEX_CLI_VERSION=latest
RUN npm install -g "@openai/codex@${CODEX_CLI_VERSION}" && codex --version
COPY --from=build /bin/notty-daemon /usr/local/bin/notty-daemon
COPY --from=build /bin/notty-agent-tool /usr/local/bin/notty-agent-tool
CMD ["notty-daemon"]
