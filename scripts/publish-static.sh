#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="${VERSION:-$(git -C "$root_dir" rev-parse --short HEAD)}"

printf 'Publishing daemon static artifacts for %s\n' "$version"
VERSION="$version" STATIC_BUILD_TARGET=daemons "$root_dir/scripts/build-static.sh"
PUBLISH_TARGET=daemons "$root_dir/scripts/publish-static-r2.sh" "$version"
printf 'Daemon static publish complete: %s\n' "$version"
