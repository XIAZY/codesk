#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="${VERSION:-$(git -C "$root_dir" rev-parse --short HEAD)}"

printf 'Deploying daemon static artifacts for %s\n' "$version"
VERSION="$version" "$root_dir/scripts/publish-static.sh"
printf 'Daemon static deploy complete: %s\n' "$version"
