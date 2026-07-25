#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

printf 'Deploying homepage static assets\n'
"$root_dir/scripts/build-homepage.sh"
UPLOAD_TARGET=homepage "$root_dir/scripts/upload-r2.sh"
printf 'Homepage deploy complete\n'
