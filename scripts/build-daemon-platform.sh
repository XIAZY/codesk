#!/usr/bin/env sh
set -eu

[ "$#" -eq 1 ] || { printf 'build-daemon-platform: usage: build-daemon-platform.sh <linux|macos|windows>\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
platform="$1"
arches="${DAEMON_ARCHES:-amd64 arm64}"
dist_root="${DAEMON_DIST_ROOT:-$root_dir/dist/static/daemons}"
version="$("$root_dir/scripts/read-version.sh")"

case "$platform" in
	linux) goos=linux ;;
	macos) goos=darwin ;;
	windows) goos=windows ;;
	*) printf 'build-daemon-platform: unsupported platform: %s\n' "$platform" >&2; exit 1 ;;
esac

platforms=
for arch in $arches; do
	case "$arch" in
		amd64|arm64) ;;
		*) printf 'build-daemon-platform: unsupported architecture: %s\n' "$arch" >&2; exit 1 ;;
	esac
	case " $platforms " in
		*" $goos/$arch "*) printf 'build-daemon-platform: duplicate architecture: %s\n' "$arch" >&2; exit 1 ;;
	esac
	platforms="${platforms:+$platforms }$goos/$arch"
done
[ -n "$platforms" ] || { printf 'build-daemon-platform: DAEMON_ARCHES must not be empty\n' >&2; exit 1; }

case "$dist_root" in
	/*) dist_abs="$dist_root" ;;
	*) dist_abs="$root_dir/$dist_root" ;;
esac
DIST_DIR="$dist_abs" PLATFORMS="$platforms" \
	"$root_dir/scripts/build-daemon-release.sh" "$dist_abs"

printf 'Built local %s daemon artifacts for %s in %s\n' "$platform" "$version" "$dist_abs/$version"
