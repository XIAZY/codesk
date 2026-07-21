#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"

printf 'Deploying frontend/homepage static assets for %s\n' "$version"
"$root_dir/scripts/publish-frontend.sh"
printf 'Frontend/homepage deploy complete: %s\n' "$version"
