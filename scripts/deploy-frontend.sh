#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

app_index="${STATIC_DIST_DIR:-$root_dir/dist/static}/app/index.html"
# index.html ships with max-age=60, so the edge can legitimately serve the previous object for
# up to a minute after a correct upload. Poll past that before calling it a failure — a check
# that cries wolf gets switched off, which returns us to shipping nothing silently.
live_check_attempts="${NOTTY_DEPLOY_VERIFY_ATTEMPTS:-10}"
live_check_interval="${NOTTY_DEPLOY_VERIFY_INTERVAL:-10}"

die() {
	printf 'deploy-frontend: %s\n' "$*" >&2
	exit 1
}

# Which bundle does an index.html point at? Vite content-hashes the filename, so this string
# IS the content identity — same name means same bytes.
bundle_ref() {
	grep -oE 'index-[A-Za-z0-9_-]+\.js' "$1" 2>/dev/null | head -1
}

live_bundle_ref() {
	live_url="$1"
	# Cache-bust so we read the origin's current object rather than our own earlier request.
	curl -fsS -H 'Cache-Control: no-cache' "$live_url?deploy-verify=$(date +%s)" 2>/dev/null |
		grep -oE 'index-[A-Za-z0-9_-]+\.js' | head -1
}

# What is about to ship, stated before it ships, so the log answers "what shipped?" without
# archaeology. A checkout behind origin/main rebuilds OLD code into a byte-identical bundle and
# uploads it successfully — every symptom of a good deploy, none of the effect.
deploy_sha="$(git -C "$root_dir" rev-parse --short HEAD 2>/dev/null || echo unknown)"
printf 'Deploying frontend/homepage static assets from %s\n' "$deploy_sha"
if git -C "$root_dir" rev-parse --git-dir >/dev/null 2>&1; then
	git -C "$root_dir" fetch --quiet origin main 2>/dev/null || true
	if git -C "$root_dir" rev-parse --verify --quiet origin/main >/dev/null 2>&1 &&
		! git -C "$root_dir" merge-base --is-ancestor origin/main HEAD; then
		behind="$(git -C "$root_dir" rev-list --count HEAD..origin/main 2>/dev/null || echo '?')"
		die "checkout is $behind commit(s) behind origin/main — this would rebuild and ship stale code. Run: git pull"
	fi
fi

# Read the live bundle BEFORE uploading, so "the hash did not change" can be reported as a fact
# rather than inferred from its absence.
frontend_origin="${NOTTY_FRONTEND_ORIGIN:-}"
before_ref=
if [ -n "$frontend_origin" ]; then
	before_ref="$(live_bundle_ref "$frontend_origin/" || true)"
fi

"$root_dir/scripts/build-frontend.sh"
UPLOAD_TARGET=frontend "$root_dir/scripts/upload-r2.sh"

# The upload exiting 0 is not evidence it reached users: on 2026-07-30 `rclone sync
# --no-check-dest` transferred nothing for days while reporting success (fixed in #231). Only
# the served object proves a deploy happened.
if [ -z "$frontend_origin" ]; then
	printf 'deploy-frontend: NOTTY_FRONTEND_ORIGIN unset — cannot verify the deploy reached users\n' >&2
	exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
	printf 'deploy-frontend: curl not found — cannot verify the deploy reached users\n' >&2
	exit 1
fi

expected_ref="$(bundle_ref "$app_index")"
[ -n "$expected_ref" ] || die "no bundle reference found in $app_index — the build did not produce an app index"

attempt=1
while [ "$attempt" -le "$live_check_attempts" ]; do
	served_ref="$(live_bundle_ref "$frontend_origin/" || true)"
	if [ "$served_ref" = "$expected_ref" ]; then
		if [ "$before_ref" = "$expected_ref" ]; then
			printf 'deploy-frontend: live bundle is %s — UNCHANGED from before this deploy.\n' "$served_ref"
			printf 'deploy-frontend: the upload succeeded and the content is identical, so either you redeployed the same commit or this checkout (%s) does not contain the change you expected.\n' "$deploy_sha"
		else
			printf 'deploy-frontend: verified live at %s — bundle %s -> %s\n' \
				"$frontend_origin" "${before_ref:-unknown}" "$served_ref"
		fi
		printf 'Frontend/homepage deploy complete\n'
		exit 0
	fi
	attempt=$((attempt + 1))
	[ "$attempt" -le "$live_check_attempts" ] && sleep "$live_check_interval"
done

die "upload reported success but $frontend_origin still serves ${served_ref:-<none>}, expected $expected_ref after $((live_check_attempts * live_check_interval))s — the deploy did NOT reach users"
