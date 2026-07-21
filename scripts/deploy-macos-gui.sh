#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'deploy-macos-gui: usage: deploy-macos-gui.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"
dist_dir="${MACOS_GUI_DIST_DIR:-$root_dir/dist/macos-desktop}"

unset ALLOW_UNSIGNED_MACOS_DESKTOP
printf 'Building signed and notarized macOS GUI release %s\n' "$version"
"$root_dir/scripts/build-macos-desktop-release.sh" "$dist_dir"
MACOS_GUI_DIST_DIR="$dist_dir" UPLOAD_TARGET=macos-gui "$root_dir/scripts/upload-r2.sh"
printf 'macOS GUI deploy complete: %s\n' "$version"
