#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"

printf 'Publishing daemon static artifacts for %s\n' "$version"
STATIC_BUILD_TARGET=daemons "$root_dir/scripts/build-static.sh"
PUBLISH_TARGET=daemons "$root_dir/scripts/publish-static-r2.sh"
printf 'Daemon static publish complete: %s\n' "$version"
