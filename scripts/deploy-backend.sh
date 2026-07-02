#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="${VERSION:-$(git -C "$root_dir" rev-parse --short HEAD)}"
docker_repo="${DOCKER_REPO:-alphatoad/notty}"
backend_image="$docker_repo:backend-$version"
ssh_host="${NOTTY_DEPLOY_SSH_HOST:-notty}"
remote_dir="${NOTTY_REMOTE_DIR:-/opt/notty}"

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

ssh "$ssh_host" "docker ps >/dev/null 2>&1" || die "remote user cannot access Docker on $ssh_host; add the SSH user to the docker group or configure passwordless Docker access before deploying"

VERSION="$version" "$root_dir/scripts/publish-backend.sh"

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
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env config >/tmp/notty-compose-config.yml && \
	grep -q 'NOTTY_DATABASE_URL:' /tmp/notty-compose-config.yml || { echo 'missing NOTTY_DATABASE_URL in $remote_dir/.env' >&2; exit 1; } && \
	grep -q 'NOTTY_JWT_SECRET:' /tmp/notty-compose-config.yml || { echo 'missing NOTTY_JWT_SECRET in $remote_dir/.env' >&2; exit 1; } && \
	grep -q 'NOTTY_MAILGUN_DOMAIN:' /tmp/notty-compose-config.yml || { echo 'missing NOTTY_MAILGUN_DOMAIN in $remote_dir/.env' >&2; exit 1; } && \
	grep -q 'NOTTY_MAILGUN_API_KEY:' /tmp/notty-compose-config.yml || { echo 'missing NOTTY_MAILGUN_API_KEY in $remote_dir/.env' >&2; exit 1; } && \
	grep -q 'NOTTY_MAILGUN_FROM:' /tmp/notty-compose-config.yml || { echo 'missing NOTTY_MAILGUN_FROM in $remote_dir/.env' >&2; exit 1; } && \
	test -f /opt/notty/cert.pem || { echo 'missing TLS certificate /opt/notty/cert.pem' >&2; exit 1; } && \
	test -f /opt/notty/private.pem || { echo 'missing TLS private key /opt/notty/private.pem' >&2; exit 1; } && \
	test -f /opt/notty/codesk_origin.pem || { echo 'missing TLS certificate /opt/notty/codesk_origin.pem' >&2; exit 1; } && \
	test -f /opt/notty/codesk_private.pem || { echo 'missing TLS private key /opt/notty/codesk_private.pem' >&2; exit 1; } && \
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env pull && \
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env up -d --remove-orphans && \
	NOTTY_BACKEND_IMAGE=$quoted_backend_image docker compose -f compose.prod.yml --env-file notty.server.env --env-file .env ps"

printf 'Backend deploy complete: %s\n' "$backend_image"
