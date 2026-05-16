VERSION ?= dev
DIST_DIR ?= dist/static/daemons
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
GO_BUILD_FLAGS ?= -trimpath
GO_LDFLAGS ?= -s -w
UNAME_S := $(shell uname -s | tr '[:upper:]' '[:lower:]')
UNAME_M := $(shell uname -m)
HOST_OS := $(if $(filter darwin,$(UNAME_S)),darwin,$(if $(filter linux,$(UNAME_S)),linux,$(UNAME_S)))
HOST_ARCH := $(if $(filter x86_64 amd64,$(UNAME_M)),amd64,$(if $(filter arm64 aarch64,$(UNAME_M)),arm64,$(UNAME_M)))
HOST_PLATFORM := $(HOST_OS)/$(HOST_ARCH)

.PHONY: dev dev-down prod-config-check static-build static-build-local static-publish deploy deploy-backend deploy-frontend deploy-static backend-image daemon-build daemon-release daemon-checksums daemon-clean daemon-installer-check daemon-uninstall-test

dev: static-build-local
	docker compose --env-file deploy/env/dev.server.env up --build

dev-down:
	docker compose --env-file deploy/env/dev.server.env down

daemon-build:
	mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/notty-daemon ./daemon/cmd/daemon
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/notty-agent-tool ./daemon/cmd/agenttool

daemon-release:
	VERSION="$(VERSION)" DIST_DIR="$(DIST_DIR)" PLATFORMS="$(PLATFORMS)" scripts/build-daemon-release.sh "$(VERSION)" "$(DIST_DIR)"

static-build:
	VERSION="$(VERSION)" scripts/build-static.sh

static-build-local:
	VERSION="dev" STATIC_BUILD_TARGET=daemons STATIC_DIST_DIR=dist/static PLATFORMS="$(HOST_PLATFORM)" scripts/build-static.sh

static-publish:
	VERSION="$(VERSION)" scripts/publish-static-r2.sh "$(VERSION)"

backend-image:
	docker buildx build --load -f backend/Dockerfile -t alphatoad/notty:backend-$(VERSION) .

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
