#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"

printf 'Publishing frontend/homepage static assets for %s\n' "$version"
STATIC_BUILD_TARGET=frontend "$root_dir/scripts/build-static.sh"
PUBLISH_TARGET=frontend "$root_dir/scripts/publish-static-r2.sh"
printf 'Frontend/homepage publish complete: %s\n' "$version"
