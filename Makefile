VERSION ?= dev
DIST_DIR ?= dist/daemons
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
GO_BUILD_FLAGS ?= -trimpath
GO_LDFLAGS ?= -s -w

.PHONY: daemon-build daemon-release daemon-checksums daemon-clean daemon-installer-check

daemon-build:
	mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/notty-daemon ./daemon/cmd/daemon
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/notty-agent-tool ./daemon/cmd/agenttool

daemon-release:
	VERSION="$(VERSION)" DIST_DIR="$(DIST_DIR)" PLATFORMS="$(PLATFORMS)" scripts/build-daemon-release.sh "$(VERSION)" "$(DIST_DIR)"

daemon-checksums:
	cd "$(DIST_DIR)/$(VERSION)" && shasum -a 256 *.tar.gz > SHA256SUMS

daemon-installer-check:
	sh -n deploy/daemons/install.sh

daemon-clean:
	rm -rf bin "$(DIST_DIR)"
