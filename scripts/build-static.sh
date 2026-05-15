#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

out_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}"
app_out="$out_dir/app"
homepage_out="$out_dir/homepage"

vite_public_origin="${VITE_PUBLIC_ORIGIN:-https://app.nottyai.co}"
vite_api_base="${VITE_API_BASE:-https://api.nottyai.co}"
vite_daemon_static_base="${VITE_DAEMON_STATIC_BASE:-https://static.nottyai.co/daemons}"

rm -rf "$app_out" "$homepage_out"
mkdir -p "$app_out" "$homepage_out"

(
	cd "$root_dir/frontend"
	if [ "${RUN_NPM_CI:-0}" = "1" ] || [ ! -d node_modules ]; then
		npm ci
	fi
	VITE_PUBLIC_ORIGIN="$vite_public_origin" \
		VITE_API_BASE="$vite_api_base" \
		VITE_DAEMON_STATIC_BASE="$vite_daemon_static_base" \
		npm run build
)

cp -R "$root_dir/frontend/dist/." "$app_out/"
cp -R "$root_dir/homepage/." "$homepage_out/"

printf 'Built static assets:\n'
printf '  app: %s\n' "$app_out"
printf '  homepage: %s\n' "$homepage_out"
