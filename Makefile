DIST_DIR ?= dist/static/daemons
GO_BUILD_FLAGS ?= -trimpath
FILE_VERSION := $(shell cat VERSION 2>/dev/null || printf dev)
GO_LDFLAGS ?= -s -w
ifeq ($(OS),Windows_NT)
VERSION ?= $(FILE_VERSION)
WINDOWS_PROCESSOR_ARCH := $(if $(PROCESSOR_ARCHITEW6432),$(PROCESSOR_ARCHITEW6432),$(PROCESSOR_ARCHITECTURE))
HOST_OS := windows
HOST_ARCH := $(if $(filter AMD64 amd64 x86_64,$(WINDOWS_PROCESSOR_ARCH)),amd64,$(if $(filter ARM64 arm64 aarch64,$(WINDOWS_PROCESSOR_ARCH)),arm64,$(WINDOWS_PROCESSOR_ARCH)))
override MACOS_GUI_HOST_OS :=
else
VERSION ?= $(FILE_VERSION)
UNAME_S := $(shell uname -s | tr '[:upper:]' '[:lower:]')
UNAME_M := $(shell uname -m)
HOST_OS := $(if $(filter darwin,$(UNAME_S)),darwin,$(if $(filter linux,$(UNAME_S)),linux,$(UNAME_S)))
HOST_ARCH := $(if $(filter x86_64 amd64,$(UNAME_M)),amd64,$(if $(filter arm64 aarch64,$(UNAME_M)),arm64,$(UNAME_M)))
override MACOS_GUI_HOST_OS := $(if $(filter darwin,$(UNAME_S)),darwin,)
endif
HOST_PLATFORM := $(HOST_OS)/$(HOST_ARCH)
PLATFORMS ?= $(HOST_PLATFORM)
DAEMON_ALL_PLATFORMS ?= all
GUI_VERSION ?= $(FILE_VERSION)
MACOS_GUI_DIST_DIR ?= dist/macos-desktop
MACOS_GUI_UNSIGNED ?=
WINDOWS_GUI_ARCHES ?= amd64 arm64
WINDOWS_GUI_ROOT ?= dist/windows-gui
WINDOWS_GUI_PAYLOAD_ROOT ?= $(WINDOWS_GUI_ROOT)/payload
WINDOWS_GUI_TEST_DIR ?= $(WINDOWS_GUI_ROOT)/tests
WINDOWS_GUI_MSI_ROOT ?= $(WINDOWS_GUI_ROOT)/msi
WINDOWS_GUI_CI_DIR ?= $(WINDOWS_GUI_ROOT)/ci
WINDOWS_GUI_REPOSITORY ?= XIAZY/notty
WINDOWS_GUI_BUILDER_IMAGE ?= alphatoad/notty:windows-builder
WINDOWS_GUI_RUN_ID ?=
WINDOWS_GUI_POWERSHELL ?= powershell.exe
WINDOWS_GUI_ZIG_VERSION ?= 0.16.0

.PHONY: dev dev-down dev-config-check prod-config-check \
	test tests test-unit test-go test-frontend test-postgres test-regression test-live \
	build build-yffi build-go build-frontend build-daemon build-static build-static-local build-backend-image \
	publish publish-backend publish-frontend publish-static \
	deploy deploy-backend deploy-frontend deploy-static \
	prod-config-check static-build static-build-local static-publish backend-image daemon-build daemon-release daemon-release-all release-daemons daemon-checksums daemon-clean daemon-installer-check daemon-installer-windows-check daemon-uninstall-test \
	macos-gui-build macos-gui-release build-windows-builder-image windows-gui-payloads windows-gui-build windows-gui-release windows-verify

dev: static-build-local
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
	CGO_ENABLED=1 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS) -X notty/daemon/internal/buildinfo.Version=$(FILE_VERSION)" -o bin/notty-daemon ./daemon/cmd/daemon
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS) -X notty/daemon/internal/buildinfo.Version=$(FILE_VERSION)" -o bin/notty-agent-tool ./daemon/cmd/agenttool

daemon-build: build-daemon

daemon-release:
	VERSION="$(VERSION)" DIST_DIR="$(DIST_DIR)" PLATFORMS="$(PLATFORMS)" scripts/build-daemon-release.sh "$(VERSION)" "$(DIST_DIR)"

daemon-release-all:
	VERSION="$(VERSION)" DIST_DIR="$(DIST_DIR)" PLATFORMS="$(DAEMON_ALL_PLATFORMS)" scripts/build-daemon-release.sh "$(VERSION)" "$(DIST_DIR)"

release-daemons: daemon-release-all

windows-gui-payloads:
	WINDOWS_GUI_ARCHES="$(WINDOWS_GUI_ARCHES)" \
	WINDOWS_GUI_ZIG_VERSION="$(WINDOWS_GUI_ZIG_VERSION)" \
	WINDOWS_GUI_SAFE_PARENT_DIRECTORY="$(WINDOWS_GUI_ROOT)" \
		scripts/build-windows-desktop-payloads.sh \
		"$(WINDOWS_GUI_PAYLOAD_ROOT)" "$(WINDOWS_GUI_TEST_DIR)"

ifeq ($(OS),Windows_NT)
macos-gui-build:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 macos-gui-build

macos-gui-release:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 macos-gui-release

build-windows-builder-image:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 build-windows-builder-image "WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"

windows-gui-build:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 windows-gui-build "GUI_VERSION=$(GUI_VERSION)" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)" "WINDOWS_GUI_PAYLOAD_ROOT=$(WINDOWS_GUI_PAYLOAD_ROOT)" "WINDOWS_GUI_TEST_DIR=$(WINDOWS_GUI_TEST_DIR)" "WINDOWS_GUI_MSI_ROOT=$(WINDOWS_GUI_MSI_ROOT)" "WINDOWS_GUI_REPOSITORY=$(WINDOWS_GUI_REPOSITORY)" "WINDOWS_GUI_ZIG_VERSION=$(WINDOWS_GUI_ZIG_VERSION)" "WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"

windows-gui-release:
	@"$(WINDOWS_GUI_POWERSHELL)" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File make.ps1 windows-gui-release "GUI_VERSION=$(GUI_VERSION)" "WINDOWS_GUI_ARCHES=$(WINDOWS_GUI_ARCHES)" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)" "WINDOWS_GUI_PAYLOAD_ROOT=$(WINDOWS_GUI_PAYLOAD_ROOT)" "WINDOWS_GUI_TEST_DIR=$(WINDOWS_GUI_TEST_DIR)" "WINDOWS_GUI_MSI_ROOT=$(WINDOWS_GUI_MSI_ROOT)" "WINDOWS_GUI_REPOSITORY=$(WINDOWS_GUI_REPOSITORY)" "WINDOWS_GUI_ZIG_VERSION=$(WINDOWS_GUI_ZIG_VERSION)" "WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"
else
macos-gui-build:
	@if [ "$(MACOS_GUI_HOST_OS)" != darwin ]; then \
		printf '%s\n' 'macos-gui-build requires a real macOS host; no GUI was built' >&2; \
		exit 1; \
	fi
	ALLOW_UNSIGNED_MACOS_DESKTOP=1 \
		scripts/build-macos-desktop-release.sh "$(GUI_VERSION)" "$(MACOS_GUI_DIST_DIR)"

macos-gui-release:
	@if [ "$(MACOS_GUI_HOST_OS)" != darwin ]; then \
		printf '%s\n' 'macos-gui-release requires a real macOS host; no release was built' >&2; \
		exit 1; \
	fi
	ALLOW_UNSIGNED_MACOS_DESKTOP="$(MACOS_GUI_UNSIGNED)" \
		scripts/build-macos-desktop-release.sh "$(GUI_VERSION)" "$(MACOS_GUI_DIST_DIR)"

build-windows-builder-image:
	@printf '%s\n' 'build-windows-builder-image requires a real Windows host running Windows containers' >&2
	@exit 1

windows-gui-build:
	@printf '%s\n' 'windows-gui-build requires a real Windows host; no GUI was built' >&2
	@printf '%s\n' 'Use the Windows build CI, or invoke make windows-gui-payloads for an explicit cross-compile' >&2
	@exit 1

windows-gui-release:
	@printf '%s\n' 'windows-gui-release requires real Windows for WiX link and ICE validation; no MSI was built' >&2
	@printf '%s\n' 'Use the Windows release CI, then run make windows-verify [WINDOWS_GUI_RUN_ID=<run-id>]' >&2
	@exit 1
endif

windows-verify:
	@command -v gh >/dev/null 2>&1 || { \
		printf '%s\n' 'windows-verify requires the GitHub CLI (gh)' >&2; \
		exit 1; \
	}
	@set -eu; \
	head="$$(git rev-parse --verify HEAD)"; \
	run_id="$(WINDOWS_GUI_RUN_ID)"; \
	if [ -z "$$run_id" ]; then \
		run_id="$$(gh run list -R "$(WINDOWS_GUI_REPOSITORY)" --workflow ci.yml --commit "$$head" --status success --limit 1 --json databaseId --jq '.[0].databaseId')"; \
	fi; \
	case "$$run_id" in ''|*[!0-9]*) \
		printf '%s\n' 'No successful exact-HEAD CI run was found; set WINDOWS_GUI_RUN_ID to an exact successful release run' >&2; \
		exit 1 ;; \
	esac; \
	metadata="$$(gh run view "$$run_id" -R "$(WINDOWS_GUI_REPOSITORY)" --json headSha,conclusion --jq '.headSha + " " + .conclusion')"; \
	if [ "$$metadata" != "$$head success" ]; then \
		printf 'CI run %s is not a successful exact-HEAD run: %s (want %s success)\n' "$$run_id" "$$metadata" "$$head" >&2; \
		exit 1; \
	fi; \
	case "$(WINDOWS_GUI_CI_DIR)" in ''|/|.) \
		printf 'unsafe WINDOWS_GUI_CI_DIR: %s\n' "$(WINDOWS_GUI_CI_DIR)" >&2; \
		exit 1 ;; \
	esac; \
	out="$(WINDOWS_GUI_CI_DIR)/$$run_id"; \
	rm -rf "$$out"; \
	mkdir -p "$$out/amd64" "$$out/arm64"; \
	for arch in amd64 arm64; do \
		gh run download "$$run_id" -R "$(WINDOWS_GUI_REPOSITORY)" -n "windows-desktop-msi-$$arch" -D "$$out/$$arch" || exit $$?; \
		dir="$$out/$$arch"; \
		expected="$$(printf '%s\n' "Codesk_0.0.1_windows_$$arch.msi" "Codesk_0.0.2_windows_$$arch.msi" SHA256SUMS provenance.json | LC_ALL=C sort)"; \
		actual="$$(LC_ALL=C ls -1A "$$dir")"; \
		if [ "$$actual" != "$$expected" ]; then \
			printf 'unexpected %s artifact inventory:\n%s\n' "$$arch" "$$actual" >&2; \
			exit 1; \
		fi; \
		for path in "$$dir"/* "$$dir"/.[!.]* "$$dir"/..?*; do \
			[ -e "$$path" ] || continue; \
			[ -f "$$path" ] && [ ! -L "$$path" ] || { \
				printf 'artifact entry is not a real file: %s\n' "$$path" >&2; \
				exit 1; \
			}; \
		done; \
		normalized="$$out/.SHA256SUMS-$$arch"; \
		awk '{ sub(/\r$$/, ""); print }' "$$dir/SHA256SUMS" >"$$normalized"; \
		expected_checksums="$$(printf '%s\n' "Codesk_0.0.1_windows_$$arch.msi" "Codesk_0.0.2_windows_$$arch.msi" provenance.json)"; \
		actual_checksums="$$(awk 'length($$1) == 64 && $$1 ~ /^[0-9a-f]+$$/ && NF == 2 { print $$2; next } { exit 1 }' "$$normalized")" || { \
			printf 'invalid %s SHA256SUMS format\n' "$$arch" >&2; \
			exit 1; \
		}; \
		if [ "$$actual_checksums" != "$$expected_checksums" ]; then \
			printf 'unexpected %s SHA256SUMS inventory:\n%s\n' "$$arch" "$$actual_checksums" >&2; \
			exit 1; \
		fi; \
		if command -v sha256sum >/dev/null 2>&1; then \
			(cd "$$dir" && sha256sum -c -) <"$$normalized" || exit $$?; \
		elif command -v shasum >/dev/null 2>&1; then \
			(cd "$$dir" && shasum -a 256 -c -) <"$$normalized" || exit $$?; \
		else \
			printf '%s\n' 'windows-verify requires sha256sum or shasum' >&2; \
			exit 1; \
		fi; \
		rm -f "$$normalized"; \
	done; \
	printf 'Verified Windows GUI CI artifacts for %s from run %s in %s\n' "$$head" "$$run_id" "$$out"

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
	tmp_secrets="$$(mktemp)" && \
	trap 'rm -f "$$tmp_secrets"' EXIT && \
	{ \
		printf '%s\n' 'NOTTY_DATABASE_URL=postgres://notty:notty@postgres:5432/notty?sslmode=disable'; \
		printf '%s\n' 'NOTTY_JWT_SECRET=notty-test-secret'; \
		printf '%s\n' 'NOTTY_MAILGUN_API_KEY=notty-test-mailgun-key'; \
	} > "$$tmp_secrets" && \
	NOTTY_BACKEND_IMAGE=alphatoad/notty:backend-test NOTTY_SECRETS_ENV_FILE="$$tmp_secrets" docker compose -f compose.prod.yml --env-file deploy/env/prod.server.env config >/dev/null

daemon-checksums:
	cd "$(DIST_DIR)/$(VERSION)" && shasum -a 256 *.tar.gz > SHA256SUMS

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

daemon-clean:
	rm -rf bin "$(DIST_DIR)"
