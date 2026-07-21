#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"

printf 'Deploying full Notty release %s\n' "$version"
"$root_dir/scripts/deploy-frontend.sh"
"$root_dir/scripts/deploy-static.sh"
"$root_dir/scripts/deploy-backend.sh"
printf 'Full Notty deploy complete: %s\n' "$version"
