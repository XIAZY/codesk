#!/usr/bin/env sh
set -eu

version="${1:-${VERSION:-dev}}"
dist_dir="${2:-${DIST_DIR:-dist/static/daemons}}"
platforms="${PLATFORMS:-}"

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

checksum() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
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

rm -rf "$out_dir" "$latest_dir"
mkdir -p "$out_dir" "$latest_dir" "$tmp_dir"

printf '{\n  "version": "%s",\n  "artifacts": [\n' "$version" > "$manifest"
first=1

for platform in $platforms; do
	os="${platform%/*}"
	arch="${platform#*/}"
	if [ "$platform" != "$host_platform" ]; then
		printf 'build-daemon-release: %s requires cgo/Rust cross-compilation; this script currently supports host platform %s only\n' "$platform" "$host_platform" >&2
		exit 1
	fi
	name="notty-daemon_${version}_${os}_${arch}"
	package_dir="$tmp_dir/$name"
	archive="$out_dir/$name.tar.gz"

	mkdir -p "$package_dir/bin"
	(
		cd "$root_dir"
		"$root_dir/scripts/build-yffi.sh"
		CGO_ENABLED=1 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$package_dir/bin/notty-daemon" ./daemon/cmd/daemon
		CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$package_dir/bin/notty-agent-tool" ./daemon/cmd/agenttool
	)

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
