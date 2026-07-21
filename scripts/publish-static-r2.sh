#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'publish-static-r2: usage: publish-static-r2.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

version="$("$root_dir/scripts/read-version.sh")"
target="${PUBLISH_TARGET:-all}"
static_dist_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}"
daemon_dist_dir="${DAEMON_DIST_DIR:-$static_dist_dir/daemons}"

die() {
	printf 'publish-static-r2: %s\n' "$*" >&2
	exit 1
}

need() {
	eval "value=\${$1:-}"
	[ -n "$value" ] || die "$1 is required"
}

s3_uri() {
	bucket="$1"
	prefix="$2"
	prefix="$(printf '%s' "$prefix" | sed 's:^/*::; s:/*$::')"
	if [ -n "$prefix" ]; then
		printf 's3://%s/%s' "$bucket" "$prefix"
	else
		printf 's3://%s' "$bucket"
	fi
}

aws_s3() {
	aws --endpoint-url "$R2_ENDPOINT_URL" s3 "$@"
}

wrangler_cmd() {
	if command -v wrangler >/dev/null 2>&1; then
		wrangler "$@"
	else
		npx wrangler "$@"
	fi
}

content_type_for() {
	case "$1" in
		*.html) printf 'text/html; charset=utf-8' ;;
		*.css) printf 'text/css; charset=utf-8' ;;
		*.js|*.mjs) printf 'application/javascript; charset=utf-8' ;;
		*.json) printf 'application/json; charset=utf-8' ;;
		*.svg) printf 'image/svg+xml' ;;
		*.png) printf 'image/png' ;;
		*.jpg|*.jpeg) printf 'image/jpeg' ;;
		*.webp) printf 'image/webp' ;;
		*.txt) printf 'text/plain; charset=utf-8' ;;
		*.sh) printf 'text/x-shellscript; charset=utf-8' ;;
		*.ps1) printf 'text/plain; charset=utf-8' ;;
		*.tar.gz) printf 'application/gzip' ;;
		*) printf 'application/octet-stream' ;;
	esac
}

join_key() {
	prefix="$(printf '%s' "$1" | sed 's:^/*::; s:/*$::')"
	path="$(printf '%s' "$2" | sed 's:^/*::')"
	if [ -n "$prefix" ]; then
		printf '%s/%s' "$prefix" "$path"
	else
		printf '%s' "$path"
	fi
}

wrangler_put() {
	bucket="$1"
	key="$2"
	file="$3"
	cache_control="$4"
	wrangler_cmd r2 object put "$bucket/$key" \
		--remote \
		--file "$file" \
		--content-type "$(content_type_for "$file")" \
		--cache-control "$cache_control" \
		--force
}

wrangler_upload_dir() {
	src="$1"
	bucket="$2"
	prefix="$3"
	cache_control="$4"
	find "$src" -type f | sort | while IFS= read -r file; do
		rel="${file#"$src"/}"
		key="$(join_key "$prefix" "$rel")"
		printf '  %s\n' "$key"
		wrangler_put "$bucket" "$key" "$file" "$cache_control"
	done
}

case "$target" in
	all|frontend|daemons) ;;
	*) die "PUBLISH_TARGET must be all, frontend, or daemons" ;;
esac

if [ "$target" = "all" ] || [ "$target" = "frontend" ]; then
	need R2_HOMEPAGE_BUCKET
	need R2_APP_BUCKET
fi
if [ "$target" = "all" ] || [ "$target" = "daemons" ]; then
	need R2_DAEMONS_BUCKET
fi

if command -v aws >/dev/null 2>&1 && [ -n "${R2_ENDPOINT_URL:-}" ]; then
	uploader="aws"
else
	if [ -z "${CLOUDFLARE_API_TOKEN:-}" ] && [ -n "${NOTTY_CLOUDFLARE_TOKEN:-}" ]; then
		CLOUDFLARE_API_TOKEN="$NOTTY_CLOUDFLARE_TOKEN"
		export CLOUDFLARE_API_TOKEN
	fi
	[ -n "${CLOUDFLARE_API_TOKEN:-}" ] || die "aws with R2_ENDPOINT_URL or CLOUDFLARE_API_TOKEN/NOTTY_CLOUDFLARE_TOKEN is required"
	[ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ] || die "CLOUDFLARE_ACCOUNT_ID is required for Wrangler R2 publishing with API tokens"
	command -v wrangler >/dev/null 2>&1 || command -v npx >/dev/null 2>&1 || die "wrangler or npx is required for Cloudflare API-token publishing"
	uploader="wrangler"
fi

if [ "$target" = "all" ] || [ "$target" = "frontend" ]; then
	homepage_prefix="${R2_HOMEPAGE_PREFIX:-}"
	app_prefix="${R2_APP_PREFIX:-}"

	[ -f "$static_dist_dir/homepage/index.html" ] || die "missing homepage build at $static_dist_dir/homepage/index.html; run scripts/build-static.sh"
	[ -f "$static_dist_dir/app/index.html" ] || die "missing app build at $static_dist_dir/app/index.html; run scripts/build-static.sh"

	homepage_dest="$(s3_uri "$R2_HOMEPAGE_BUCKET" "$homepage_prefix")"
	app_dest="$(s3_uri "$R2_APP_BUCKET" "$app_prefix")"

	printf 'Publishing homepage to %s\n' "$homepage_dest"
	if [ "$uploader" = "aws" ]; then
		aws_s3 sync "$static_dist_dir/homepage/" "$homepage_dest/" --delete
	else
		wrangler_upload_dir "$static_dist_dir/homepage" "$R2_HOMEPAGE_BUCKET" "$homepage_prefix" "public, max-age=300"
		if [ -z "$homepage_prefix" ]; then
			wrangler_put "$R2_HOMEPAGE_BUCKET" "" "$static_dist_dir/homepage/index.html" "public, max-age=300"
		fi
	fi

	printf 'Publishing app to %s\n' "$app_dest"
	if [ "$uploader" = "aws" ]; then
		aws_s3 sync "$static_dist_dir/app/" "$app_dest/" --delete
	else
		wrangler_upload_dir "$static_dist_dir/app" "$R2_APP_BUCKET" "$app_prefix" "public, max-age=31536000, immutable"
		wrangler_put "$R2_APP_BUCKET" "$(join_key "$app_prefix" "index.html")" "$static_dist_dir/app/index.html" "public, max-age=60"
		if [ -z "$app_prefix" ]; then
			wrangler_put "$R2_APP_BUCKET" "" "$static_dist_dir/app/index.html" "public, max-age=60"
		fi
	fi
fi

if [ "$target" = "all" ] || [ "$target" = "daemons" ]; then
	daemons_prefix="${R2_DAEMONS_PREFIX:-daemons}"

	[ -d "$daemon_dist_dir/$version" ] || die "missing daemon release $daemon_dist_dir/$version; run scripts/build-daemon-release.sh"
	[ -f "$daemon_dist_dir/$version/SHA256SUMS" ] || die "missing daemon SHA256SUMS for $version"
	[ -f "$daemon_dist_dir/$version/manifest.json" ] || die "missing daemon manifest for $version"

	daemon_dest="$(s3_uri "$R2_DAEMONS_BUCKET" "$daemons_prefix")"
	release_cache_control="${DAEMON_RELEASE_CACHE_CONTROL:-public, max-age=31536000, immutable}"
	printf 'Publishing daemon release %s to %s/%s\n' "$version" "$daemon_dest" "$version"
	if [ "$uploader" = "aws" ]; then
		aws_s3 sync "$daemon_dist_dir/$version/" "$daemon_dest/$version/" --cache-control "$release_cache_control"
	else
		wrangler_upload_dir "$daemon_dist_dir/$version" "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "$version")" "$release_cache_control"
	fi

	if [ -f "$daemon_dist_dir/install.sh" ]; then
		installer="$daemon_dist_dir/install.sh"
	else
		installer="$root_dir/deploy/daemons/install.sh"
	fi
	if [ -f "$daemon_dist_dir/uninstall.sh" ]; then
		uninstaller="$daemon_dist_dir/uninstall.sh"
	else
		uninstaller="$root_dir/deploy/daemons/uninstall.sh"
	fi
	if [ -f "$daemon_dist_dir/install.ps1" ]; then
		powershell_installer="$daemon_dist_dir/install.ps1"
	else
		powershell_installer="$root_dir/deploy/daemons/install.ps1"
	fi
	if [ -f "$daemon_dist_dir/uninstall.ps1" ]; then
		powershell_uninstaller="$daemon_dist_dir/uninstall.ps1"
	else
		powershell_uninstaller="$root_dir/deploy/daemons/uninstall.ps1"
	fi

	# Publish latest metadata only after the complete versioned release is present.
	if [ "$uploader" = "aws" ]; then
		aws_s3 cp "$installer" "$daemon_dest/install.sh" --cache-control "public, max-age=300"
		aws_s3 cp "$uninstaller" "$daemon_dest/uninstall.sh" --cache-control "public, max-age=300"
		aws_s3 cp "$powershell_installer" "$daemon_dest/install.ps1" --content-type "$(content_type_for "$powershell_installer")" --cache-control "public, max-age=300"
		aws_s3 cp "$powershell_uninstaller" "$daemon_dest/uninstall.ps1" --content-type "$(content_type_for "$powershell_uninstaller")" --cache-control "public, max-age=300"
		aws_s3 cp "$daemon_dist_dir/$version/SHA256SUMS" "$daemon_dest/latest/SHA256SUMS" --cache-control "public, max-age=60"
		aws_s3 cp "$daemon_dist_dir/$version/manifest.json" "$daemon_dest/latest/manifest.json" --cache-control "public, max-age=60"
	else
		wrangler_put "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "install.sh")" "$installer" "public, max-age=300"
		wrangler_put "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "uninstall.sh")" "$uninstaller" "public, max-age=300"
		wrangler_put "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "install.ps1")" "$powershell_installer" "public, max-age=300"
		wrangler_put "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "uninstall.ps1")" "$powershell_uninstaller" "public, max-age=300"
		wrangler_put "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "latest/SHA256SUMS")" "$daemon_dist_dir/$version/SHA256SUMS" "public, max-age=60"
		wrangler_put "$R2_DAEMONS_BUCKET" "$(join_key "$daemons_prefix" "latest/manifest.json")" "$daemon_dist_dir/$version/manifest.json" "public, max-age=60"
	fi
fi

printf 'Published %s assets for %s\n' "$target" "$version"
