#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"
docker_repo="${DOCKER_REPO:-alphatoad/notty}"
backend_image="$docker_repo:backend-$version"

die() {
	printf 'publish-backend: %s\n' "$*" >&2
	exit 1
}

BACKEND_IMAGE_MODE=push "$root_dir/scripts/build-backend-image.sh"

printf 'Published backend image: %s\n' "$backend_image"
