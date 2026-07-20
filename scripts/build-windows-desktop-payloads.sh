#!/usr/bin/env sh
set -eu
set -f

fail() {
	printf 'build-windows-desktop-payloads: %s\n' "$*" >&2
	exit 1
}

[ "$#" -le 2 ] || fail 'usage: build-windows-desktop-payloads.sh [payload-dir [test-dir]]'

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
payload_dir="${1:-$root_dir/dist/windows-gui/payload}"
test_dir="${2:-$root_dir/dist/windows-gui/tests}"
safe_parent="${WINDOWS_GUI_SAFE_PARENT_DIRECTORY:-$root_dir/dist/windows-gui}"
architectures="${WINDOWS_GUI_ARCHES-amd64 arm64}"
build_version="$(cat "$root_dir/VERSION" 2>/dev/null || printf dev)"
required_zig_version="${WINDOWS_GUI_ZIG_VERSION:-0.16.0}"
host_yffi_link="$root_dir/third_party/y-crdt/target/release/libyrs.a"
host_yffi_backup=
host_yffi_existed=0
yffi_touched=0

snapshot_host_yffi() {
	[ ! -L "$host_yffi_link" ] || fail "host yffi link must not be a symbolic link: $host_yffi_link"
	if [ ! -e "$host_yffi_link" ]; then
		return 0
	fi
	[ -f "$host_yffi_link" ] || fail "host yffi link is not a regular file: $host_yffi_link"
	host_yffi_backup="$(mktemp "${TMPDIR:-/tmp}/notty-windows-yffi.XXXXXX")"
	cp -p "$host_yffi_link" "$host_yffi_backup"
	host_yffi_existed=1
}

restore_host_yffi() {
	if [ "$yffi_touched" -eq 0 ]; then
		return 0
	fi
	printf '%s\n' 'build-windows-desktop-payloads: restoring host yffi library'
	if [ "$host_yffi_existed" -eq 1 ]; then
		restore_tmp="${host_yffi_link}.restore.$$"
		rm -f "$restore_tmp"
		if ! cp -p "$host_yffi_backup" "$restore_tmp" || ! mv -f "$restore_tmp" "$host_yffi_link"; then
			rm -f "$restore_tmp"
			return 1
		fi
	elif ! rm -f "$host_yffi_link"; then
		return 1
	fi
	yffi_touched=0
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ "$yffi_touched" -ne 0 ]; then
		if ! restore_host_yffi; then
			printf '%s\n' 'build-windows-desktop-payloads: failed to restore the host yffi library' >&2
			status=1
		fi
	fi
	if [ -n "$host_yffi_backup" ] && ! rm -f "$host_yffi_backup"; then
		status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

set -- $architectures
[ "$#" -gt 0 ] || fail 'WINDOWS_GUI_ARCHES must contain amd64, arm64, or both'
validated_architectures=
seen=' '
for architecture in "$@"; do
	case "$architecture" in
		amd64|arm64) ;;
		*) fail "unsupported Windows GUI architecture: $architecture" ;;
	esac
	case "$seen" in
		*" $architecture "*) fail "duplicate Windows GUI architecture: $architecture" ;;
	esac
	seen="$seen$architecture "
	validated_architectures="${validated_architectures}${validated_architectures:+ }$architecture"
done

for command in go cargo rustc zig; do
	command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done
actual_zig_version="$(zig version)"
[ "$actual_zig_version" = "$required_zig_version" ] ||
	fail "Zig $required_zig_version is required (got $actual_zig_version)"

absolute_path() {
	case "$1" in
		/*|[A-Za-z]:/*) printf '%s\n' "$1" ;;
		*) printf '%s\n' "$root_dir/$1" ;;
	esac
}

canonical_output_path() {
	path="$(absolute_path "$1")"
	name="$(basename -- "$path")"
	case "$name" in
		''|.|..) fail "unsafe output directory: $1" ;;
	esac
	parent="$(dirname -- "$path")"
	mkdir -p "$parent"
	parent="$(CDPATH= cd -- "$parent" && pwd -P)"
	path="$parent/$name"
	[ ! -L "$path" ] || fail "output directory must not be a symbolic link: $path"
	if [ -e "$path" ] && [ ! -d "$path" ]; then
		fail "output path exists and is not a directory: $path"
	fi
	printf '%s\n' "$path"
}

safe_parent="$(canonical_output_path "$safe_parent")"
mkdir -p "$safe_parent"
safe_parent="$(CDPATH= cd -- "$safe_parent" && pwd -P)"
case "$safe_parent" in
	/|"$root_dir") fail "unsafe safe parent directory: $safe_parent" ;;
esac
payload_dir="$(canonical_output_path "$payload_dir")"
test_dir="$(canonical_output_path "$test_dir")"
case "$payload_dir/" in
	"$safe_parent"/*/) ;;
	*) fail "payload directory must be below safe parent $safe_parent" ;;
esac
case "$test_dir/" in
	"$safe_parent"/*/) ;;
	*) fail "test directory must be below safe parent $safe_parent" ;;
esac
[ "$payload_dir" != "$test_dir" ] || fail 'payload and test directories must be distinct'
case "$payload_dir" in
	"$test_dir"/*) fail 'payload and test directories must not overlap' ;;
esac
case "$test_dir" in
	"$payload_dir"/*) fail 'payload and test directories must not overlap' ;;
esac

rm -rf "$payload_dir" "$test_dir"
mkdir -p "$payload_dir" "$test_dir"
cd "$root_dir"
snapshot_host_yffi

for architecture in $validated_architectures; do
	case "$architecture" in
		amd64)
			rust_target=x86_64-pc-windows-gnu
			zig_target=x86_64-windows-gnu
			;;
		arm64)
			rust_target=aarch64-pc-windows-gnullvm
			zig_target=aarch64-windows-gnu
			;;
	esac
	arch_payload_dir="$payload_dir/$architecture"
	mkdir -p "$arch_payload_dir"

	yffi_touched=1
	RUST_TARGET="$rust_target" RUSTFLAGS='-C panic=abort' scripts/build-yffi.sh
	CC="zig cc -target $zig_target" CGO_ENABLED=1 GOOS=windows GOARCH="$architecture" \
		go vet ./daemon/internal/syncer ./internal/ycrdt
	CC="zig cc -target $zig_target" CGO_ENABLED=1 GOOS=windows GOARCH="$architecture" \
		go test -c -o "$test_dir/notty-syncer-$architecture.test.exe" ./daemon/internal/syncer
	CC="zig cc -target $zig_target" CGO_ENABLED=1 GOOS=windows GOARCH="$architecture" \
		go vet ./daemon/internal/desktopstate ./daemon/internal/desktop ./daemon/internal/desktopapp ./daemon/cmd/codesk-desktop
	CC="zig cc -target $zig_target" CGO_ENABLED=1 GOOS=windows GOARCH="$architecture" \
		go test -c -o "$test_dir/codesk-desktop-$architecture.test.exe" ./daemon/cmd/codesk-desktop
	CC="zig cc -target $zig_target" CGO_ENABLED=1 GOOS=windows GOARCH="$architecture" \
		go build -trimpath -buildvcs=false \
			-ldflags="-H=windowsgui -extldflags=-Wl,--subsystem,windows -X main.desktopVersion=$build_version" \
			-o "$arch_payload_dir/Codesk.exe" ./daemon/cmd/codesk-desktop
	CGO_ENABLED=0 GOOS=windows GOARCH="$architecture" \
		go build -trimpath -buildvcs=false -ldflags='-s -w' \
			-o "$arch_payload_dir/notty-agent-tool.exe" ./daemon/cmd/agenttool
	go run ./scripts/verify-windows-desktop-pe.go "$arch_payload_dir/Codesk.exe" "$architecture" gui
	go run ./scripts/verify-windows-desktop-pe.go "$arch_payload_dir/notty-agent-tool.exe" "$architecture" console
done

restore_host_yffi
printf 'Built Windows GUI payloads for %s in %s\n' "$validated_architectures" "$payload_dir"
