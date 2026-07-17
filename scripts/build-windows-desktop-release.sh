#!/usr/bin/env sh
set -eu

version="${1:-${VERSION:-dev}}"
dist_dir="${2:-${DIST_DIR:-dist/windows-desktop}}"
platforms="${PLATFORMS:-windows/amd64 windows/arm64}"
unsigned_override="${ALLOW_UNSIGNED_WINDOWS_DESKTOP:-}"
signer="${CODESK_WINDOWS_SIGNER:-}"

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
. "$root_dir/scripts/lib/testtmp.sh"
. "$root_dir/scripts/lib/desktop-release.sh"

go_toolchain='go1.26.5'
go_repro_ldflags='-buildid='
export GOTOOLCHAIN="$go_toolchain"
export GOENV=off
export GO111MODULE=on
export GOWORK=off
export GOAMD64=v1
export GOARM64=v8.0
export LC_ALL=C
export TZ=UTC
export SOURCE_DATE_EPOCH=0
unset GOFLAGS GOEXPERIMENT
unset CC CXX CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS
unset RUSTFLAGS CARGO_ENCODED_RUSTFLAGS RUSTC RUSTDOC RUSTC_WRAPPER RUSTC_WORKSPACE_WRAPPER
unset CARGO_TARGET_DIR CARGO_BUILD_TARGET CARGO_BUILD_RUSTC CARGO_BUILD_RUSTDOC
unset CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER CARGO_TARGET_X86_64_PC_WINDOWS_GNU_RUSTFLAGS
unset CARGO_TARGET_AARCH64_PC_WINDOWS_GNULLVM_LINKER CARGO_TARGET_AARCH64_PC_WINDOWS_GNULLVM_RUSTFLAGS
unset CARGO_PROFILE_RELEASE_CODEGEN_UNITS CARGO_PROFILE_RELEASE_DEBUG CARGO_PROFILE_RELEASE_INCREMENTAL
unset CARGO_PROFILE_RELEASE_LTO CARGO_PROFILE_RELEASE_OPT_LEVEL CARGO_PROFILE_RELEASE_OVERFLOW_CHECKS
unset CARGO_PROFILE_RELEASE_PANIC CARGO_PROFILE_RELEASE_RPATH CARGO_PROFILE_RELEASE_STRIP
export CARGO_INCREMENTAL=0

fail() {
	printf 'build-windows-desktop-release: %s\n' "$*" >&2
	exit 1
}

case "$dist_dir" in
	/*) dist_abs="$dist_dir" ;;
	*) dist_abs="$root_dir/$dist_dir" ;;
esac

command -v go >/dev/null 2>&1 || fail 'go is required'
command -v git >/dev/null 2>&1 || fail 'git is required'
actual_go_version="$(go env GOVERSION 2>/dev/null || true)"
[ "$actual_go_version" = "$go_toolchain" ] || fail "$go_toolchain is required (got ${actual_go_version:-unknown})"
"$root_dir/scripts/test-desktopstate-boundary.sh"
resource_version="$(
	cd "$root_dir"
	go run -buildvcs=false ./daemon/cmd/codesk-desktop-release resource-version "$version"
)" || fail 'invalid release version'
source_revision="$(notty_desktop_release_source_revision "$root_dir")" || exit 1

tmp_dir="$(notty_test_mktemp codesk-windows-desktop-release)"
export CARGO_HOME="$tmp_dir/cargo-home"
export CARGO_TARGET_DIR="$tmp_dir/cargo-target"
rust_flag_separator="$(printf '\037')"
export CARGO_ENCODED_RUSTFLAGS="-C${rust_flag_separator}panic=abort${rust_flag_separator}--remap-path-prefix=$root_dir=.${rust_flag_separator}--remap-path-prefix=$tmp_dir=/build"
publish_dir="$tmp_dir/publish/$version"
release_tool="$tmp_dir/codesk-desktop-release"
winres_tool="$tmp_dir/bin/go-winres"
icon="$root_dir/daemon/cmd/codesk-desktop/assets/codesk.ico"
desktop_resource_prefix="$root_dir/daemon/cmd/codesk-desktop/zz_codesk_release_rsrc"
setup_resource_prefix="$root_dir/daemon/cmd/codesk-desktop-setup/zz_codesk_release_rsrc"
yffi_touched=0

restore_host_yffi() {
	if [ "$yffi_touched" -eq 0 ]; then
		return 0
	fi
	printf '%s\n' 'build-windows-desktop-release: restoring host yffi library'
	if ! (
		unset RUST_TARGET RUSTFLAGS CARGO_ENCODED_RUSTFLAGS CARGO_HOME CARGO_TARGET_DIR RUSTC
		cd "$root_dir"
		"$root_dir/scripts/build-yffi.sh"
	); then
		return 1
	fi
	yffi_touched=0
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	rm -f \
		"${desktop_resource_prefix}_windows_amd64.syso" \
		"${desktop_resource_prefix}_windows_arm64.syso" \
		"${setup_resource_prefix}_windows_amd64.syso" \
		"${setup_resource_prefix}_windows_arm64.syso"
	if [ "$yffi_touched" -ne 0 ]; then
		if ! restore_host_yffi; then
			printf '%s\n' 'build-windows-desktop-release: failed to restore the host yffi library' >&2
			status=1
		fi
	fi
	chmod -R u+w "$tmp_dir" 2>/dev/null || true
	rm -rf "$tmp_dir"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rust_target_for() {
	case "$1" in
		amd64) printf '%s' 'x86_64-pc-windows-gnu' ;;
		arm64) printf '%s' 'aarch64-pc-windows-gnullvm' ;;
		*) fail "unsupported architecture $1" ;;
	esac
}

zig_target_for() {
	case "$1" in
		amd64) printf '%s' 'x86_64-windows-gnu' ;;
		arm64) printf '%s' 'aarch64-windows-gnu' ;;
		*) fail "unsupported architecture $1" ;;
	esac
}

setup_filename() {
	printf 'CodeskSetup_%s_windows_%s.exe' "$version" "$1"
}

sign_file() {
	path="$1"
	printf 'build-windows-desktop-release: signing %s\n' "$(basename "$path")"
	"$signer" "$path"
}

platforms="$(printf '%s' "$platforms" | tr ',' ' ')"
seen_amd64=0
seen_arm64=0
for platform in $platforms; do
	case "$platform" in
		windows/amd64)
			[ "$seen_amd64" -eq 0 ] || fail 'duplicate windows/amd64 platform'
			seen_amd64=1
			;;
		windows/arm64)
			[ "$seen_arm64" -eq 0 ] || fail 'duplicate windows/arm64 platform'
			seen_arm64=1
			;;
		*) fail "only windows/amd64 and windows/arm64 are supported (got $platform)" ;;
	esac
done
[ "$seen_amd64" -eq 1 ] && [ "$seen_arm64" -eq 1 ] || fail 'both windows/amd64 and windows/arm64 are required'

case "$unsigned_override" in
	"") signed=true ;;
	1) signed=false ;;
	*) fail 'ALLOW_UNSIGNED_WINDOWS_DESKTOP must be unset or exactly 1' ;;
esac
if [ "$signed" = true ]; then
	[ -n "$signer" ] || fail 'CODESK_WINDOWS_SIGNER is required; set ALLOW_UNSIGNED_WINDOWS_DESKTOP=1 only for construction evidence'
	if [ ! -x "$signer" ]; then
		signer_path="$(command -v "$signer" 2>/dev/null || true)"
		[ -n "$signer_path" ] || fail "CODESK_WINDOWS_SIGNER is not executable: $signer"
		signer="$signer_path"
	fi
else
	[ -z "$signer" ] || fail 'do not combine CODESK_WINDOWS_SIGNER with ALLOW_UNSIGNED_WINDOWS_DESKTOP=1'
	printf '%s\n' 'build-windows-desktop-release: UNSIGNED CONSTRUCTION-ONLY MODE' >&2
fi

command -v cargo >/dev/null 2>&1 || fail 'cargo is required'
command -v rustc >/dev/null 2>&1 || fail 'rustc is required'
command -v zig >/dev/null 2>&1 || fail 'Zig 0.16.0 is required'
rustc_version="$(rustc --version 2>/dev/null || true)"
[ "$rustc_version" = 'rustc 1.97.0 (2d8144b78 2026-07-07)' ] || fail "rustc 1.97.0 is required (got ${rustc_version:-unknown})"
export RUSTC="$(command -v rustc)"
[ "$("$RUSTC" --version 2>/dev/null || true)" = "$rustc_version" ] || fail 'resolved Rust compiler changed during validation'
cargo_version="$(cargo --version 2>/dev/null || true)"
[ "$cargo_version" = 'cargo 1.97.0 (c980f4866 2026-06-30)' ] || fail "cargo 1.97.0 is required (got ${cargo_version:-unknown})"
zig_version="$(zig version 2>/dev/null || true)"
[ "$zig_version" = 0.16.0 ] || fail "Zig 0.16.0 is required (got ${zig_version:-unknown})"
[ -f "$icon" ] || fail "missing icon: $icon"

for arch in amd64 arm64; do
	rust_target="$(rust_target_for "$arch")"
	libdir="$(rustc --print target-libdir --target "$rust_target" 2>/dev/null || true)"
	[ -n "$libdir" ] && [ -d "$libdir" ] || fail "Rust target $rust_target is not installed"
done

mkdir -p "$dist_abs"
dist_abs="$(CDPATH= cd -- "$dist_abs" && pwd -P)"
[ "$dist_abs" != / ] || fail 'release output directory must not be the filesystem root'

mkdir -p "$tmp_dir/bin" "$publish_dir"
(
	cd "$root_dir"
	go build -buildvcs=false -trimpath -o "$release_tool" ./daemon/cmd/codesk-desktop-release
)
[ "$($release_tool resource-version "$version")" = "$resource_version" ] || fail 'release version validation changed across the pinned Go toolchain'

generated_icon="$tmp_dir/generated-codesk.ico"
(
	cd "$root_dir"
	go run -buildvcs=false ./scripts/generate-codesk-icon.go "$generated_icon"
)
cmp "$generated_icon" "$icon" || fail 'committed Codesk icon is not reproducible'

GOBIN="$tmp_dir/bin" \
	GOMODCACHE="$tmp_dir/gomodcache" \
	GOCACHE="$tmp_dir/gocache" \
	go install -buildvcs=false -trimpath github.com/tc-hib/go-winres@v0.3.1

"$winres_tool" simply \
	--arch amd64,arm64 \
	--out "$desktop_resource_prefix" \
	--product-version "$resource_version" \
	--file-version "$resource_version" \
	--manifest gui \
	--file-description 'Codesk Desktop' \
	--product-name 'Codesk' \
	--copyright 'Copyright (c) 2026 Codesk' \
	--original-filename 'Codesk.exe' \
	--icon "$icon"

"$winres_tool" simply \
	--arch amd64,arm64 \
	--out "$setup_resource_prefix" \
	--product-version "$resource_version" \
	--file-version "$resource_version" \
	--manifest gui \
	--file-description 'Codesk Setup' \
	--product-name 'Codesk' \
	--copyright 'Copyright (c) 2026 Codesk' \
	--original-filename 'CodeskSetup.exe' \
	--icon "$icon"

for resource in \
	"${desktop_resource_prefix}_windows_amd64.syso" \
	"${desktop_resource_prefix}_windows_arm64.syso" \
	"${setup_resource_prefix}_windows_amd64.syso" \
	"${setup_resource_prefix}_windows_arm64.syso"
do
	[ -s "$resource" ] || fail "resource generator did not create $resource"
done

for arch in amd64 arm64; do
	rust_target="$(rust_target_for "$arch")"
	zig_target="$(zig_target_for "$arch")"
	arch_dir="$tmp_dir/$arch"
	desktop="$arch_dir/Codesk.exe"
	agent="$arch_dir/notty-agent-tool.exe"
	stub="$arch_dir/CodeskSetup.stub.exe"
	payload="$arch_dir/payload.zip"
	setup="$publish_dir/$(setup_filename "$arch")"
	mkdir -p "$arch_dir"

	printf 'build-windows-desktop-release: building yffi for %s\n' "$rust_target"
	yffi_touched=1
	(
		cd "$root_dir"
		RUST_TARGET="$rust_target" scripts/build-yffi.sh
	)

	cc="zig cc -target $zig_target"
	(
		cd "$root_dir"
		CC="$cc" CGO_ENABLED=1 GOOS=windows GOARCH="$arch" go build -buildvcs=false -trimpath \
			-ldflags "$go_repro_ldflags -linkmode external -extldflags '-static -Wl,-s,--subsystem,windows' -H=windowsgui -s -w -X main.desktopVersion=$version" \
			-o "$desktop" ./daemon/cmd/codesk-desktop
		CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build -buildvcs=false -trimpath \
			-ldflags "$go_repro_ldflags -s -w" \
			-o "$agent" ./daemon/cmd/agenttool
		CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build -buildvcs=false -trimpath \
			-ldflags "$go_repro_ldflags -H=windowsgui -s -w -X main.setupVersion=$version -X main.setupArch=$arch" \
			-o "$stub" ./daemon/cmd/codesk-desktop-setup
	)

	if [ "$signed" = true ]; then
		sign_file "$desktop"
		sign_file "$agent"
	fi
	"$release_tool" archive \
		--output "$payload" \
		--version "$version" \
		--arch "$arch" \
		--desktop "$desktop" \
		--agent "$agent" \
		--icon "$icon"
	"$release_tool" append --stub "$stub" --payload "$payload" --output "$setup"
	if [ "$signed" = true ]; then
		sign_file "$setup"
	fi
done

restore_host_yffi

"$release_tool" manifest \
	--output "$publish_dir" \
	--version "$version" \
	--source-revision "$source_revision" \
	--signed="$signed" \
	--amd64 "$publish_dir/$(setup_filename amd64)" \
	--arm64 "$publish_dir/$(setup_filename arm64)"

if [ "$signed" = true ]; then
	"$release_tool" verify "$publish_dir" "$version"
else
	"$release_tool" verify --allow-unsigned "$publish_dir" "$version"
fi

out_dir="$dist_abs/$version"
rm -rf "$out_dir"
mv "$publish_dir" "$out_dir"
if [ "$signed" = true ]; then
	printf 'Built signed Codesk Windows desktop release %s in %s\n' "$version" "$out_dir"
else
	printf 'Built unsigned construction-only Codesk Windows desktop release %s in %s\n' "$version" "$out_dir"
fi
