#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'build-frontend: usage: build-frontend.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

out_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}"
app_out="$out_dir/app"
homepage_out="$out_dir/homepage"

frontend_origin="${NOTTY_FRONTEND_ORIGIN:-https://app.nottyai.co}"
backend_origin="${NOTTY_BACKEND_ORIGIN:-https://api.nottyai.co}"
static_origin="${NOTTY_STATIC_ORIGIN:-https://static.nottyai.co}"
vite_public_origin="${VITE_PUBLIC_ORIGIN:-$frontend_origin}"
vite_api_base="${VITE_API_BASE:-$backend_origin}"
vite_daemon_static_base="${VITE_DAEMON_STATIC_BASE:-${NOTTY_DAEMON_STATIC_BASE:-$static_origin/daemons}}"
vite_desktop_static_base="${VITE_DESKTOP_STATIC_BASE:-${NOTTY_DESKTOP_STATIC_BASE:-$static_origin/desktop}}"

rm -rf "$app_out"
mkdir -p "$app_out"

(
	cd "$root_dir/frontend"
	npm ci
	VITE_PUBLIC_ORIGIN="$vite_public_origin" \
		VITE_API_BASE="$vite_api_base" \
		VITE_DAEMON_STATIC_BASE="$vite_daemon_static_base" \
		VITE_DESKTOP_STATIC_BASE="$vite_desktop_static_base" \
		npm run build
)

cp -R "$root_dir/frontend/dist/." "$app_out/"

STATIC_DIST_DIR="$out_dir" "$root_dir/scripts/build-homepage.sh" >/dev/null

printf 'Built frontend assets:\n'
printf '  app: %s\n' "$app_out"
printf '  homepage: %s\n' "$homepage_out"
