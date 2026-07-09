FROM golang:1.23-alpine AS build
WORKDIR /app
RUN apk add --no-cache build-base cargo
COPY go.mod go.sum ./
COPY third_party ./third_party
COPY scripts/build-yffi.sh ./scripts/build-yffi.sh
RUN go mod download
RUN scripts/build-yffi.sh
COPY backend ./backend
COPY internal ./internal
# Build identity, injected at link time so /healthz can report the served commit.
# Declared right before the build so changing it only busts this layer.
ARG NOTTY_COMMIT=dev
ARG NOTTY_BUILT_AT=unknown
RUN CGO_ENABLED=1 GOOS=linux go build \
	-ldflags "-X notty/backend/internal/buildinfo.Commit=${NOTTY_COMMIT} -X notty/backend/internal/buildinfo.Time=${NOTTY_BUILT_AT}" \
	-o /bin/notty-backend ./backend/cmd/server

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache libgcc && mkdir -p /data
COPY --from=build /bin/notty-backend /usr/local/bin/notty-backend
EXPOSE 8080
CMD ["notty-backend"]
