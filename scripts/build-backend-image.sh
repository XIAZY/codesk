#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="${VERSION:-$(git -C "$root_dir" rev-parse --short HEAD)}"
docker_repo="${DOCKER_REPO:-alphatoad/notty}"
backend_image="$docker_repo:backend-$version"
latest_image="$docker_repo:backend-latest"
platforms="${DOCKER_PLATFORMS:-linux/amd64,linux/arm64}"
mode="${BACKEND_IMAGE_MODE:-local}"

die() {
	printf 'build-backend-image: %s\n' "$*" >&2
	exit 1
}

command -v docker >/dev/null 2>&1 || die "docker is required"

case "$mode" in
	local)
		printf 'Building local backend image %s\n' "$backend_image"
		docker buildx build \
			--load \
			-f "$root_dir/backend/Dockerfile" \
			-t "$backend_image" \
			"$root_dir"
		;;
	push)
		printf 'Building and pushing backend image %s\n' "$backend_image"
		docker buildx build \
			--platform "$platforms" \
			-f "$root_dir/backend/Dockerfile" \
			-t "$backend_image" \
			-t "$latest_image" \
			--push \
			"$root_dir"
		;;
	*)
		die "BACKEND_IMAGE_MODE must be local or push"
		;;
esac

printf 'Backend image build complete: %s\n' "$backend_image"
