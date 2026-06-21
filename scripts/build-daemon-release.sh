#!/usr/bin/env sh
set -eu

version="${1:-${VERSION:-dev}}"
dist_dir="${2:-${DIST_DIR:-dist/static/daemons}}"
platforms="${PLATFORMS:-}"
all_platforms="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
case "$dist_dir" in
	/*) dist_abs="$dist_dir" ;;
	*) dist_abs="$root_dir/$dist_dir" ;;
esac

out_dir="$dist_abs/$version"
latest_dir="$dist_abs/latest"
tmp_dir="${TMPDIR:-/tmp}/notty-daemon-release-$$"
manifest="$out_dir/manifest.json"
installer="$root_dir/deploy/daemons/install.sh"
uninstaller="$root_dir/deploy/daemons/uninstall.sh"

host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$host_os" in
	darwin|linux) ;;
	*) host_os="$(uname -s)" ;;
esac
host_arch="$(uname -m)"
case "$host_arch" in
	x86_64|amd64) host_arch="amd64" ;;
	arm64|aarch64) host_arch="arm64" ;;
esac
host_platform="$host_os/$host_arch"
if [ -z "$platforms" ]; then
	platforms="$host_platform"
fi
platforms="$(printf '%s' "$platforms" | tr ',' ' ')"
if [ "$platforms" = "all" ]; then
	platforms="$all_platforms"
fi

checksum() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

preflight_status=0

preflight_error() {
	printf 'build-daemon-release: %s\n' "$*" >&2
	preflight_status=1
}

rust_target_for() {
	case "$1/$2" in
		linux/amd64) printf 'x86_64-unknown-linux-musl' ;;
		linux/arm64) printf 'aarch64-unknown-linux-musl' ;;
		darwin/amd64) printf 'x86_64-apple-darwin' ;;
		darwin/arm64) printf 'aarch64-apple-darwin' ;;
		*) printf '' ;;
	esac
}

zig_target_for() {
	case "$1/$2" in
		linux/amd64) printf 'x86_64-linux-musl' ;;
		linux/arm64) printf 'aarch64-linux-musl' ;;
		*) printf '' ;;
	esac
}

darwin_arch_for() {
	case "$1" in
		amd64) printf 'x86_64' ;;
		arm64) printf 'arm64' ;;
		*) printf '' ;;
	esac
}

rust_linker_env_for() {
	printf 'CARGO_TARGET_%s_LINKER' "$(printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_')"
}

make_zig_cc_wrapper() {
	os="$1"
	arch="$2"
	wrapper="$3"
	zig_target="$(zig_target_for "$os" "$arch")"
	[ -n "$zig_target" ] || {
		printf 'build-daemon-release: no zig target mapping for %s/%s\n' "$os" "$arch" >&2
		exit 1
	}
	command -v zig >/dev/null 2>&1 || {
		env_name="$(cc_env_name_for "$os" "$arch")"
		printf 'build-daemon-release: zig is required for native cross-compiling %s/%s; install zig or set %s to a target C compiler\n' "$os" "$arch" "$env_name" >&2
		exit 1
	}
	cat > "$wrapper" <<EOF
#!/usr/bin/env sh
exec zig cc -target $zig_target "\$@"
EOF
	chmod +x "$wrapper"
}

make_darwin_cc_wrapper() {
	arch="$2"
	wrapper="$3"
	darwin_arch="$(darwin_arch_for "$arch")"
	[ -n "$darwin_arch" ] || {
		printf 'build-daemon-release: no Darwin arch mapping for %s\n' "$arch" >&2
		exit 1
	}
	if [ "$host_os" != "darwin" ]; then
		printf 'build-daemon-release: CC_DARWIN_%s is required to cross-compile darwin/%s from %s; use a Darwin-capable compiler such as osxcross clang with an Apple SDK\n' "$(printf '%s' "$arch" | tr '[:lower:]-' '[:upper:]_')" "$arch" "$host_os" >&2
		exit 1
	fi
	command -v xcrun >/dev/null 2>&1 || {
		printf 'build-daemon-release: xcrun is required to cross-compile darwin/%s on macOS, or set CC_DARWIN_%s to a Darwin-capable compiler\n' "$arch" "$(printf '%s' "$arch" | tr '[:lower:]-' '[:upper:]_')" >&2
		exit 1
	}
	cat > "$wrapper" <<EOF
#!/usr/bin/env sh
exec xcrun clang -arch $darwin_arch "\$@"
EOF
	chmod +x "$wrapper"
}

cc_env_name_for() {
	os="$1"
	arch="$2"
	printf 'CC_%s_%s' "$(printf '%s' "$os" | tr '[:lower:]-' '[:upper:]_')" "$(printf '%s' "$arch" | tr '[:lower:]-' '[:upper:]_')"
}

rust_target_available() {
	target="$1"
	libdir="$(rustc --print target-libdir --target "$target" 2>/dev/null || true)"
	[ -n "$libdir" ] && [ -d "$libdir" ]
}

check_rust_target() {
	target="$1"
	if ! rust_target_available "$target"; then
		preflight_error "Rust target $target is not installed; install it with rustup target add $target or use a Rust toolchain that includes it"
	fi
}

preflight_platform() {
	platform="$1"
	os="${platform%/*}"
	arch="${platform#*/}"
	rust_target="$(rust_target_for "$os" "$arch")"
	env_name="$(cc_env_name_for "$os" "$arch")"
	eval "cc_value=\${$env_name:-}"

	if [ -z "$rust_target" ]; then
		preflight_error "no Rust target mapping for $platform"
		return
	fi

	if [ "$os" = "linux" ] || [ "$platform" != "$host_platform" ]; then
		check_rust_target "$rust_target"
	fi

	case "$os" in
		linux)
			if [ -z "$cc_value" ] && ! command -v zig >/dev/null 2>&1; then
				preflight_error "zig is required for native cross-compiling $platform; install zig or set $env_name to a target C compiler"
			fi
			;;
		darwin)
			if [ "$platform" != "$host_platform" ] && [ -z "$cc_value" ]; then
				if [ "$host_os" = "darwin" ]; then
					command -v xcrun >/dev/null 2>&1 || preflight_error "xcrun is required to cross-compile $platform on macOS, or set $env_name to a Darwin-capable compiler"
				else
					preflight_error "$env_name is required to cross-compile $platform from $host_os; use a Darwin-capable compiler such as osxcross clang with an Apple SDK"
				fi
			fi
			;;
	esac
}

target_cc_for() {
	os="$1"
	arch="$2"
	wrapper="$3"
	env_name="$(cc_env_name_for "$os" "$arch")"
	eval "cc_value=\${$env_name:-}"
	if [ -n "$cc_value" ]; then
		printf '%s' "$cc_value"
		return
	fi
	case "$os" in
		linux)
			make_zig_cc_wrapper "$os" "$arch" "$wrapper"
			printf '%s' "$wrapper"
			;;
		darwin)
			make_darwin_cc_wrapper "$os" "$arch" "$wrapper"
			printf '%s' "$wrapper"
			;;
		*)
			printf 'build-daemon-release: %s/%s requires %s to point at a target C compiler\n' "$os" "$arch" "$env_name" >&2
			exit 1
			;;
	esac
}

go_ldflags_for() {
	case "$1" in
		linux) printf -- "-linkmode external -extldflags '-static -lunwind' -s -w" ;;
		*) printf -- '-linkmode external -s -w' ;;
	esac
}

build_host_binaries() {
	os="$1"
	arch="$2"
	package_dir="$3"

	(
		cd "$root_dir"
		"$root_dir/scripts/build-yffi.sh"
		CGO_ENABLED=1 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$package_dir/bin/notty-daemon" ./daemon/cmd/daemon
		CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$package_dir/bin/notty-agent-tool" ./daemon/cmd/agenttool
	)
}

build_cross_binaries() {
	os="$1"
	arch="$2"
	package_dir="$3"
	rust_target="$4"

	cc_wrapper="$tmp_dir/cc-$os-$arch"
	cc="$(target_cc_for "$os" "$arch" "$cc_wrapper")"
	linker_env="$(rust_linker_env_for "$rust_target")"
	yffi_lib="$root_dir/third_party/y-crdt/target/$rust_target/release/libyrs.a"
	go_link_lib="$root_dir/third_party/y-crdt/target/release/libyrs.a"

	printf 'build-daemon-release: building yffi for %s with %s=%s\n' "$rust_target" "$linker_env" "$cc"
	(
		cd "$root_dir"
		env RUST_TARGET="$rust_target" RUSTFLAGS="${RUSTFLAGS:-} -C panic=abort" "$linker_env=$cc" "$root_dir/scripts/build-yffi.sh"
	)
	[ -f "$yffi_lib" ] || {
		printf 'build-daemon-release: missing Rust library after build: %s\n' "$yffi_lib" >&2
		exit 1
	}

	mkdir -p "$(dirname "$go_link_lib")"
	cp "$yffi_lib" "$go_link_lib"

	(
		cd "$root_dir"
		CC="$cc" CGO_ENABLED=1 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$(go_ldflags_for "$os")" -o "$package_dir/bin/notty-daemon" ./daemon/cmd/daemon
		CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$package_dir/bin/notty-agent-tool" ./daemon/cmd/agenttool
	)
}

cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

if [ ! -f "$installer" ]; then
	printf 'missing installer: %s\n' "$installer" >&2
	exit 1
fi
if [ ! -f "$uninstaller" ]; then
	printf 'missing uninstaller: %s\n' "$uninstaller" >&2
	exit 1
fi

for platform in $platforms; do
	preflight_platform "$platform"
done
[ "$preflight_status" -eq 0 ] || exit 1

rm -rf "$out_dir" "$latest_dir"
mkdir -p "$out_dir" "$latest_dir" "$tmp_dir"

printf '{\n  "version": "%s",\n  "artifacts": [\n' "$version" > "$manifest"
first=1

for platform in $platforms; do
	os="${platform%/*}"
	arch="${platform#*/}"
	name="notty-daemon_${version}_${os}_${arch}"
	package_dir="$tmp_dir/$name"
	archive="$out_dir/$name.tar.gz"

	mkdir -p "$package_dir/bin"
	rust_target="$(rust_target_for "$os" "$arch")"
	if [ "$os" = "linux" ] && [ -n "$rust_target" ]; then
		build_cross_binaries "$os" "$arch" "$package_dir" "$rust_target"
	elif [ "$platform" = "$host_platform" ]; then
		build_host_binaries "$os" "$arch" "$package_dir"
	elif [ -n "$rust_target" ]; then
		build_cross_binaries "$os" "$arch" "$package_dir" "$rust_target"
	else
		printf 'build-daemon-release: no Rust target mapping for %s\n' "$platform" >&2
		exit 1
	fi

	cat > "$package_dir/README.md" <<EOF
Notty daemon $version

This package contains:
- bin/notty-daemon
- bin/notty-agent-tool

Use the hosted install script for normal installs:
  curl -fsSL <public-origin>/daemons/install.sh | sh
EOF

	(
		cd "$tmp_dir"
		tar -czf "$archive" "$name"
	)

	sum="$(checksum "$archive")"
	printf '%s  %s\n' "$sum" "$(basename "$archive")" >> "$out_dir/SHA256SUMS"

	if [ "$first" -eq 0 ]; then
		printf ',\n' >> "$manifest"
	fi
	first=0
	printf '    {"os": "%s", "arch": "%s", "file": "%s", "sha256": "%s"}' "$os" "$arch" "$(basename "$archive")" "$sum" >> "$manifest"
done

printf '\n  ]\n}\n' >> "$manifest"

cp "$out_dir/manifest.json" "$latest_dir/manifest.json"
cp "$out_dir/SHA256SUMS" "$latest_dir/SHA256SUMS"
cp "$installer" "$dist_abs/install.sh"
cp "$installer" "$out_dir/install.sh"
cp "$uninstaller" "$dist_abs/uninstall.sh"
cp "$uninstaller" "$out_dir/uninstall.sh"

printf 'Built daemon release %s in %s\n' "$version" "$out_dir"
