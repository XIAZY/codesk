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
ssh_host="${NOTTY_DEPLOY_SSH_HOST:-notty}"
remote_dir="${NOTTY_REMOTE_DIR:-/opt/notty}"
install_nginx="${INSTALL_NGINX:-0}"

die() {
	printf 'deploy-backend: %s\n' "$*" >&2
	exit 1
}

shell_quote() {
	printf "'"
	printf '%s' "$1" | sed "s/'/'\\\\''/g"
	printf "'"
}

command -v docker >/dev/null 2>&1 || die "docker is required"
command -v ssh >/dev/null 2>&1 || die "ssh is required"
command -v scp >/dev/null 2>&1 || die "scp is required"

printf 'Building and pushing backend image %s\n' "$backend_image"
docker buildx build \
	--platform "$platforms" \
	-f "$root_dir/backend/Dockerfile" \
	-t "$backend_image" \
	-t "$latest_image" \
	--push \
	"$root_dir"

quoted_remote_dir="$(shell_quote "$remote_dir")"
quoted_backend_image="$(shell_quote "$backend_image")"

printf 'Uploading production compose file to %s:%s\n' "$ssh_host" "$remote_dir"
ssh "$ssh_host" "mkdir -p $quoted_remote_dir"
scp "$root_dir/compose.prod.yml" "$ssh_host:$remote_dir/compose.prod.yml"
scp "${NOTTY_PROD_SERVER_ENV_FILE:-$root_dir/deploy/env/prod.server.env}" "$ssh_host:$remote_dir/notty.server.env"
scp "$root_dir/deploy/nginx/notty-api.conf" "$ssh_host:$remote_dir/notty-api.nginx.conf"

printf 'Restarting backend on %s\n' "$ssh_host"
ssh "$ssh_host" "cd $quoted_remote_dir && \
	test -f notty.server.env || { echo 'missing $remote_dir/notty.server.env' >&2; exit 1; } && \
	test -f .env || { echo 'missing $remote_dir/.env' >&2; exit 1; } && \
	set -a && . ./notty.server.env && . ./.env && set +a && \
	: \"\${NOTTY_DATABASE_URL:?missing NOTTY_DATABASE_URL in $remote_dir/.env}\" && \
	: \"\${NOTTY_JWT_SECRET:?missing NOTTY_JWT_SECRET in $remote_dir/.env}\" && \
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env pull && \
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env up -d --remove-orphans && \
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env ps"

if [ "$install_nginx" = "1" ]; then
	printf 'Installing nginx API proxy on %s\n' "$ssh_host"
	ssh "$ssh_host" "sudo cp $quoted_remote_dir/notty-api.nginx.conf /etc/nginx/conf.d/notty-api.conf && sudo nginx -t && sudo systemctl reload nginx"
else
	printf 'Uploaded nginx API proxy config to %s/notty-api.nginx.conf. Set INSTALL_NGINX=1 to install and reload nginx during deploy.\n' "$remote_dir"
fi

printf 'Backend deploy complete: %s\n' "$backend_image"
