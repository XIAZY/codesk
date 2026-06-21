#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

out_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}"
app_out="$out_dir/app"
homepage_out="$out_dir/homepage"
daemons_out="$out_dir/daemons"
target="${STATIC_BUILD_TARGET:-all}"
version="${VERSION:-$(git -C "$root_dir" rev-parse --short HEAD 2>/dev/null || printf dev)}"

case "$target" in
	all|frontend|daemons) ;;
	*) printf 'build-static: STATIC_BUILD_TARGET must be all, frontend, or daemons\n' >&2; exit 1 ;;
esac

frontend_origin="${NOTTY_FRONTEND_ORIGIN:-https://app.nottyai.co}"
backend_origin="${NOTTY_BACKEND_ORIGIN:-https://api.nottyai.co}"
static_origin="${NOTTY_STATIC_ORIGIN:-https://static.nottyai.co}"
vite_public_origin="${VITE_PUBLIC_ORIGIN:-$frontend_origin}"
vite_api_base="${VITE_API_BASE:-$backend_origin}"
vite_daemon_static_base="${VITE_DAEMON_STATIC_BASE:-${NOTTY_DAEMON_STATIC_BASE:-$static_origin/daemons}}"

if [ "$target" = "all" ] || [ "$target" = "frontend" ]; then
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
fi

if [ "$target" = "all" ] || [ "$target" = "daemons" ]; then
	daemon_platforms="${DAEMON_PLATFORMS:-${PLATFORMS:-}}"
	if [ -n "$daemon_platforms" ]; then
		VERSION="$version" DIST_DIR="$daemons_out" PLATFORMS="$daemon_platforms" "$root_dir/scripts/build-daemon-release.sh" "$version" "$daemons_out"
	else
		VERSION="$version" DIST_DIR="$daemons_out" "$root_dir/scripts/build-daemon-release.sh" "$version" "$daemons_out"
	fi
fi

printf 'Built static assets:\n'
if [ "$target" = "all" ] || [ "$target" = "frontend" ]; then
	printf '  app: %s\n' "$app_out"
	printf '  homepage: %s\n' "$homepage_out"
fi
if [ "$target" = "all" ] || [ "$target" = "daemons" ]; then
	printf '  daemons: %s\n' "$daemons_out"
fi
