#!/usr/bin/env sh
set -eu

version="${1:-${VERSION:-dev}}"
dist_dir="${2:-${DIST_DIR:-dist/daemons}}"
platforms="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"

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
	name="notty-daemon_${version}_${os}_${arch}"
	package_dir="$tmp_dir/$name"
	archive="$out_dir/$name.tar.gz"

	mkdir -p "$package_dir/bin"
	(
		cd "$root_dir"
		CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$package_dir/bin/notty-daemon" ./daemon/cmd/daemon
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
