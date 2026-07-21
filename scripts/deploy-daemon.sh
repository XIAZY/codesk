#!/usr/bin/env sh
set -eu

[ "$#" -eq 1 ] || { printf 'deploy-daemon: usage: deploy-daemon.sh <linux|macos|windows>\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

platform="$1"
case "$platform" in
	linux|macos|windows) ;;
	*) printf 'deploy-daemon: unsupported platform: %s\n' "$platform" >&2; exit 1 ;;
esac
version="$("$root_dir/scripts/read-version.sh")"

printf 'Deploying %s daemon release %s\n' "$platform" "$version"
DAEMON_ARCHES='amd64 arm64' "$root_dir/scripts/build-daemon-platform.sh" "$platform"
UPLOAD_TARGET=daemon UPLOAD_PLATFORM="$platform" "$root_dir/scripts/upload-r2.sh"
printf '%s daemon deploy complete: %s\n' "$platform" "$version"
