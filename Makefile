DIST_DIR ?= dist/static/daemons
GO_BUILD_FLAGS ?= -trimpath
WINDOWS_GUI_POWERSHELL ?= powershell.exe
GO_LDFLAGS ?= -s -w
ifeq ($(OS),Windows_NT)
override REPOSITORY_DAEMON_VERSION = $(shell "$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/read-daemon-version.ps1)
WINDOWS_PROCESSOR_ARCH := $(if $(PROCESSOR_ARCHITEW6432),$(PROCESSOR_ARCHITEW6432),$(PROCESSOR_ARCHITECTURE))
HOST_OS := windows
HOST_ARCH := $(if $(filter AMD64 amd64 x86_64,$(WINDOWS_PROCESSOR_ARCH)),amd64,$(if $(filter ARM64 arm64 aarch64,$(WINDOWS_PROCESSOR_ARCH)),arm64,$(WINDOWS_PROCESSOR_ARCH)))
override MACOS_GUI_HOST_OS :=
else
override REPOSITORY_DAEMON_VERSION = $(shell scripts/read-daemon-version.sh)
UNAME_S := $(shell uname -s | tr '[:upper:]' '[:lower:]')
UNAME_M := $(shell uname -m)
HOST_OS := $(if $(filter darwin,$(UNAME_S)),darwin,$(if $(filter linux,$(UNAME_S)),linux,$(UNAME_S)))
HOST_ARCH := $(if $(filter x86_64 amd64,$(UNAME_M)),amd64,$(if $(filter arm64 aarch64,$(UNAME_M)),arm64,$(UNAME_M)))
override MACOS_GUI_HOST_OS := $(if $(filter darwin,$(UNAME_S)),darwin,)
endif
HOST_PLATFORM := $(HOST_OS)/$(HOST_ARCH)
HOST_DAEMON_PLATFORM := $(if $(filter darwin,$(HOST_OS)),macos,$(HOST_OS))
DAEMON_DIST_ROOT ?= $(DIST_DIR)
MACOS_GUI_DIST_DIR ?= dist/macos-desktop
WINDOWS_GUI_ROOT ?= dist/windows-gui
WINDOWS_GUI_PAYLOAD_ROOT ?= $(WINDOWS_GUI_ROOT)/payload
WINDOWS_GUI_TEST_DIR ?= $(WINDOWS_GUI_ROOT)/tests
WINDOWS_GUI_MSI_ROOT ?= $(WINDOWS_GUI_ROOT)/msi
WINDOWS_GUI_REPOSITORY ?= XIAZY/notty
WINDOWS_GUI_BUILDER_IMAGE ?= ghcr.io/xiazy/notty-windows-builder:latest
WINDOWS_GUI_ZIG_VERSION ?= 0.16.0

.PHONY: dev dev-down dev-config-check prod-config-check \
	test tests test-unit test-go test-frontend test-postgres test-regression test-live \
	build build-yffi build-go _build-daemon-host _static-build-local \
	linux-daemon-build macos-daemon-build windows-daemon-build daemon-deploy \
	macos-gui-build macos-gui-deploy windows-gui-build windows-gui-deploy \
	frontend-build frontend-deploy homepage-build homepage-deploy \
	backend-build backend-deploy deploy \
	daemon-clean daemon-installer-check daemon-installer-windows-check daemon-uninstall-test daemon-version-contract-check build-deploy-contract-check

dev: _static-build-local
	docker compose --env-file deploy/env/dev.server.env up --build

dev-down:
	docker compose --env-file deploy/env/dev.server.env down

dev-config-check:
	tmp_secrets="$$(mktemp)" && \
	trap 'rm -f "$$tmp_secrets"' EXIT && \
	{ \
		printf '%s\n' 'NOTTY_DATABASE_URL=postgres://notty:notty@postgres:5432/notty?sslmode=disable'; \
		printf '%s\n' 'NOTTY_JWT_SECRET=notty-test-secret'; \
		printf '%s\n' 'NOTTY_MAILGUN_API_KEY=notty-test-mailgun-key'; \
	} > "$$tmp_secrets" && \
	NOTTY_SECRETS_ENV_FILE="$$tmp_secrets" docker compose --env-file deploy/env/dev.server.env config >/dev/null

test: tests

tests: test-unit test-postgres test-regression test-live

test-unit: test-go test-frontend daemon-installer-check daemon-uninstall-test daemon-version-contract-check build-deploy-contract-check

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

build: build-go frontend-build _build-daemon-host _static-build-local

build-yffi:
	scripts/build-yffi.sh

build-go: build-yffi
	go build ./...

frontend-build:
	scripts/build-frontend.sh

homepage-build:
	scripts/build-homepage.sh

_build-daemon-host: build-yffi
	repository_daemon_version="$(REPOSITORY_DAEMON_VERSION)" && \
	[ -n "$$repository_daemon_version" ] && \
	mkdir -p bin && \
	CGO_ENABLED=1 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS) -X notty/daemon/internal/buildinfo.Version=$$repository_daemon_version" -o bin/notty-daemon ./daemon/cmd/daemon && \
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS) -X notty/daemon/internal/buildinfo.Version=$$repository_daemon_version" -o bin/notty-agent-tool ./daemon/cmd/agenttool

_static-build-local:
	DAEMON_DIST_ROOT="$(DAEMON_DIST_ROOT)" DAEMON_ARCHES="$(HOST_ARCH)" scripts/build-daemon-platform.sh "$(HOST_DAEMON_PLATFORM)"

linux-daemon-build:
	DAEMON_DIST_ROOT="$(DAEMON_DIST_ROOT)" DAEMON_ARCHES="amd64 arm64" scripts/build-daemon-platform.sh linux

macos-daemon-build:
	DAEMON_DIST_ROOT="$(DAEMON_DIST_ROOT)" DAEMON_ARCHES="amd64 arm64" scripts/build-daemon-platform.sh macos

windows-daemon-build:
	DAEMON_DIST_ROOT="$(DAEMON_DIST_ROOT)" DAEMON_ARCHES="amd64 arm64" scripts/build-daemon-platform.sh windows

daemon-deploy:
	DAEMON_DIST_ROOT="$(DAEMON_DIST_ROOT)" scripts/deploy-daemon.sh

ifeq ($(OS),Windows_NT)
macos-gui-build:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 macos-gui-build

macos-gui-deploy:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 macos-gui-deploy

windows-gui-build:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 windows-gui-build "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)" "WINDOWS_GUI_PAYLOAD_ROOT=$(WINDOWS_GUI_PAYLOAD_ROOT)" "WINDOWS_GUI_TEST_DIR=$(WINDOWS_GUI_TEST_DIR)" "WINDOWS_GUI_MSI_ROOT=$(WINDOWS_GUI_MSI_ROOT)" "WINDOWS_GUI_REPOSITORY=$(WINDOWS_GUI_REPOSITORY)" "WINDOWS_GUI_ZIG_VERSION=$(WINDOWS_GUI_ZIG_VERSION)" "WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"

windows-gui-deploy:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 windows-gui-deploy "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)" "WINDOWS_GUI_PAYLOAD_ROOT=$(WINDOWS_GUI_PAYLOAD_ROOT)" "WINDOWS_GUI_TEST_DIR=$(WINDOWS_GUI_TEST_DIR)" "WINDOWS_GUI_MSI_ROOT=$(WINDOWS_GUI_MSI_ROOT)" "WINDOWS_GUI_REPOSITORY=$(WINDOWS_GUI_REPOSITORY)" "WINDOWS_GUI_ZIG_VERSION=$(WINDOWS_GUI_ZIG_VERSION)" "WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"
else
macos-gui-build:
	@if [ "$(MACOS_GUI_HOST_OS)" != darwin ]; then \
		printf '%s\n' 'macos-gui-build requires a real macOS host; no GUI was built' >&2; \
		exit 1; \
	fi
	ALLOW_UNSIGNED_MACOS_DESKTOP=1 \
		scripts/build-macos-desktop-release.sh "$(MACOS_GUI_DIST_DIR)"

macos-gui-deploy:
	@if [ "$(MACOS_GUI_HOST_OS)" != darwin ]; then \
		printf '%s\n' 'macos-gui-deploy requires a real macOS host; no GUI was deployed' >&2; \
		exit 1; \
	fi
	MACOS_GUI_DIST_DIR="$(MACOS_GUI_DIST_DIR)" scripts/deploy-macos-gui.sh

windows-gui-build:
	@printf '%s\n' 'windows-gui-build requires a real Windows host; no GUI was built' >&2
	@printf '%s\n' 'Use the Windows build CI or scripts/build-windows-desktop-payloads.sh for an explicit cross-compile' >&2
	@exit 1

windows-gui-deploy:
	@printf '%s\n' 'windows-gui-deploy requires real Windows for WiX link and ICE validation; no GUI was deployed' >&2
	@printf '%s\n' 'Use the Windows native CI workflow for published-builder MSI evidence' >&2
	@exit 1
endif

backend-build:
	scripts/build-backend-image.sh

backend-deploy:
	scripts/deploy-backend.sh

frontend-deploy:
	scripts/deploy-frontend.sh

homepage-deploy:
	scripts/deploy-homepage.sh

deploy:
	$(MAKE) frontend-deploy
	$(MAKE) daemon-deploy
	$(MAKE) backend-deploy

prod-config-check:
	tmp_secrets="$$(mktemp)" && \
	trap 'rm -f "$$tmp_secrets"' EXIT && \
	{ \
		printf '%s\n' 'NOTTY_DATABASE_URL=postgres://notty:notty@postgres:5432/notty?sslmode=disable'; \
		printf '%s\n' 'NOTTY_JWT_SECRET=notty-test-secret'; \
		printf '%s\n' 'NOTTY_MAILGUN_API_KEY=notty-test-mailgun-key'; \
	} > "$$tmp_secrets" && \
	NOTTY_BACKEND_IMAGE=alphatoad/notty:backend-test NOTTY_SECRETS_ENV_FILE="$$tmp_secrets" docker compose -f compose.prod.yml --env-file deploy/env/prod.server.env config >/dev/null

daemon-installer-check:
	sh -n deploy/daemons/install.sh
	sh -n deploy/daemons/uninstall.sh
	sh scripts/test-daemon-installer.sh

daemon-installer-windows-check:
	@if command -v pwsh >/dev/null 2>&1; then \
		pwsh -NoLogo -NoProfile -File scripts/test-daemon-installer-windows.ps1; \
	elif command -v powershell.exe >/dev/null 2>&1; then \
		powershell.exe -NoLogo -NoProfile -File scripts/test-daemon-installer-windows.ps1; \
	else \
		printf '%s\n' 'PowerShell is required for daemon-installer-windows-check' >&2; \
		exit 1; \
	fi

daemon-uninstall-test:
	sh scripts/test-daemon-uninstall.sh

daemon-version-contract-check:
	sh scripts/test-daemon-version-contract.sh

build-deploy-contract-check:
	sh scripts/test-build-deploy-contract.sh
	sh scripts/test-frontend-desktop-binding.sh

daemon-clean:
	rm -rf bin "$(DIST_DIR)"
