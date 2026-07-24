#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'deploy-macos-gui: usage: deploy-macos-gui.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-daemon-version.sh")"
dist_dir="${MACOS_GUI_DIST_DIR:-$root_dir/dist/macos-desktop}"
unsigned_override="${ALLOW_UNSIGNED_MACOS_DESKTOP:-}"

case "$unsigned_override" in
	"") release_description='signed and notarized' ;;
	1)
		release_description='UNSIGNED construction-only'
		printf '%s\n' 'deploy-macos-gui: WARNING: publishing without signing, notarization, stapling, or Gatekeeper trust' >&2
		;;
	*)
		printf '%s\n' 'deploy-macos-gui: ALLOW_UNSIGNED_MACOS_DESKTOP must be unset or exactly 1' >&2
		exit 1
		;;
esac

printf 'Building %s macOS GUI release %s\n' "$release_description" "$version"
"$root_dir/scripts/build-macos-desktop-release.sh" "$dist_dir"
MACOS_GUI_DIST_DIR="$dist_dir" UPLOAD_TARGET=macos-gui "$root_dir/scripts/upload-r2.sh"
printf 'macOS GUI deploy complete: %s (%s)\n' "$version" "$release_description"
