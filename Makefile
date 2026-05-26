VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf dev)
DIST_DIR ?= dist/static/daemons
GO_BUILD_FLAGS ?= -trimpath
GO_LDFLAGS ?= -s -w
UNAME_S := $(shell uname -s | tr '[:upper:]' '[:lower:]')
UNAME_M := $(shell uname -m)
HOST_OS := $(if $(filter darwin,$(UNAME_S)),darwin,$(if $(filter linux,$(UNAME_S)),linux,$(UNAME_S)))
HOST_ARCH := $(if $(filter x86_64 amd64,$(UNAME_M)),amd64,$(if $(filter arm64 aarch64,$(UNAME_M)),arm64,$(UNAME_M)))
HOST_PLATFORM := $(HOST_OS)/$(HOST_ARCH)
PLATFORMS ?= $(HOST_PLATFORM)

.PHONY: dev dev-down \
	test tests test-unit test-go test-frontend test-postgres test-regression test-live \
	build build-yffi build-go build-frontend build-daemon build-static build-static-local build-backend-image \
	publish publish-backend publish-frontend publish-static \
	deploy deploy-backend deploy-frontend deploy-static \
	prod-config-check static-build static-build-local static-publish backend-image daemon-build daemon-release daemon-checksums daemon-clean daemon-installer-check daemon-uninstall-test

dev: static-build-local
	docker compose --env-file deploy/env/dev.server.env up --build

dev-down:
	docker compose --env-file deploy/env/dev.server.env down

test: tests

tests: test-unit test-postgres test-regression test-live

test-unit: test-go test-frontend daemon-installer-check daemon-uninstall-test

test-go: build-yffi
	go test ./...

test-frontend:
	cd frontend && npm test

test-postgres:
	scripts/test-postgres.sh

test-regression:
	go test -tags=regression ./test/regression

test-live:
	scripts/test-live.sh

build: build-go build-frontend build-daemon build-static-local

build-yffi:
	scripts/build-yffi.sh

build-go: build-yffi
	go build ./...

build-frontend:
	cd frontend && if [ ! -d node_modules ]; then npm ci; fi && npm run build

build-daemon: build-yffi
	mkdir -p bin
	CGO_ENABLED=1 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/notty-daemon ./daemon/cmd/daemon
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/notty-agent-tool ./daemon/cmd/agenttool

daemon-build: build-daemon

daemon-release:
	VERSION="$(VERSION)" DIST_DIR="$(DIST_DIR)" PLATFORMS="$(PLATFORMS)" scripts/build-daemon-release.sh "$(VERSION)" "$(DIST_DIR)"

build-static:
	VERSION="$(VERSION)" scripts/build-static.sh

static-build: build-static

build-static-local:
	VERSION="dev" STATIC_BUILD_TARGET=daemons STATIC_DIST_DIR=dist/static PLATFORMS="$(HOST_PLATFORM)" scripts/build-static.sh

static-build-local: build-static-local

build-backend-image:
	VERSION="$(VERSION)" scripts/build-backend-image.sh

backend-image: build-backend-image

publish: publish-backend publish-frontend publish-static

publish-backend:
	VERSION="$(VERSION)" scripts/publish-backend.sh

publish-frontend:
	VERSION="$(VERSION)" scripts/publish-frontend.sh

publish-static:
	VERSION="$(VERSION)" scripts/publish-static.sh

static-publish:
	VERSION="$(VERSION)" scripts/publish-static-r2.sh "$(VERSION)"

deploy:
	VERSION="$(VERSION)" scripts/deploy-notty.sh

deploy-backend:
	VERSION="$(VERSION)" scripts/deploy-backend.sh

deploy-frontend:
	VERSION="$(VERSION)" scripts/deploy-frontend.sh

deploy-static:
	VERSION="$(VERSION)" scripts/deploy-static.sh

prod-config-check:
	docker compose -f compose.prod.yml --env-file deploy/env/prod.server.env --env-file .env.example config >/dev/null

daemon-checksums:
	cd "$(DIST_DIR)/$(VERSION)" && shasum -a 256 *.tar.gz > SHA256SUMS

daemon-installer-check:
	sh -n deploy/daemons/install.sh
	sh -n deploy/daemons/uninstall.sh
	sh scripts/test-daemon-installer.sh

daemon-uninstall-test:
	sh scripts/test-daemon-uninstall.sh

daemon-clean:
	rm -rf bin "$(DIST_DIR)"
