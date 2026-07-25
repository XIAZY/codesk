#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'build-homepage: usage: build-homepage.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

out_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}"
homepage_out="$out_dir/homepage"

[ -f "$root_dir/homepage/index.html" ] ||
	{ printf 'build-homepage: homepage/index.html is missing\n' >&2; exit 1; }

rm -rf "$homepage_out"
mkdir -p "$homepage_out"
cp -R "$root_dir/homepage/." "$homepage_out/"

printf 'Built homepage assets:\n'
printf '  homepage: %s\n' "$homepage_out"
