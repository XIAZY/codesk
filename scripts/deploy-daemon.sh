#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'deploy-daemon: usage: deploy-daemon.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"

printf 'Building complete daemon release %s\n' "$version"
DIST_DIR="${DAEMON_DIST_ROOT:-$root_dir/dist/static/daemons}" PLATFORMS=all \
	"$root_dir/scripts/build-daemon-release.sh" "${DAEMON_DIST_ROOT:-$root_dir/dist/static/daemons}"
DAEMON_DIST_ROOT="${DAEMON_DIST_ROOT:-$root_dir/dist/static/daemons}" \
	UPLOAD_TARGET=daemon "$root_dir/scripts/upload-r2.sh"
printf 'Daemon deploy complete: %s\n' "$version"
