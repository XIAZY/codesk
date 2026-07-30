#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
load_notty_deploy_env "$root_dir"

app_dist_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}/app"
app_index="$app_dist_dir/index.html"

die() {
	printf 'deploy-frontend: %s\n' "$*" >&2
	exit 1
}

positive_int() {
	case "${1:-}" in
		'' | *[!0-9]*) return 1 ;;
	esac
	[ "$1" -gt 0 ]
}

# index.html ships max-age=60, so the edge can legitimately serve the previous object for up to a
# minute after a correct upload. Every probe is individually time-bounded too — without that the
# "bounded" retry is unbounded, because one hung connection outlasts any attempt budget.
live_check_attempts="${NOTTY_DEPLOY_VERIFY_ATTEMPTS:-10}"
live_check_interval="${NOTTY_DEPLOY_VERIFY_INTERVAL:-10}"
live_check_timeout="${NOTTY_DEPLOY_VERIFY_TIMEOUT:-15}"
positive_int "$live_check_attempts" || die "NOTTY_DEPLOY_VERIFY_ATTEMPTS must be a positive integer"
positive_int "$live_check_timeout" || die "NOTTY_DEPLOY_VERIFY_TIMEOUT must be a positive integer"
case "$live_check_interval" in
	'' | *[!0-9]*) die "NOTTY_DEPLOY_VERIFY_INTERVAL must be a non-negative integer" ;;
esac

# Everything a successful verification needs is knowable BEFORE anything is published. Discovering
# we cannot verify after the upload leaves prod changed and unproven — the worst of both.
frontend_origin="${NOTTY_FRONTEND_ORIGIN:-}"
[ -n "$frontend_origin" ] || die 'NOTTY_FRONTEND_ORIGIN is unset — refusing to deploy something we cannot verify'
command -v curl >/dev/null 2>&1 || die 'curl not found — refusing to deploy something we cannot verify'
if command -v sha256sum >/dev/null 2>&1; then
	digest_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
	digest_tool=shasum
else
	die 'neither sha256sum nor shasum found — refusing to deploy something we cannot verify'
fi

fetch_live() {
	curl -fsS --connect-timeout "$live_check_timeout" --max-time "$live_check_timeout" \
		-H 'Cache-Control: no-cache' "$1" 2>/dev/null
}

# Vite content-hashes the filename, so this string IS the content identity.
bundle_ref() {
	grep -oE 'index-[A-Za-z0-9_-]+\.js' "$1" 2>/dev/null | head -1
}

# Always fetch "/" — the URL users actually hit. On 2026-07-30 "/" and "/index.html" resolved to
# DIFFERENT objects, and a check against the named path would have called that deploy verified
# while every real user saw a two-day-old app.
live_bundle_ref() {
	fetch_live "$frontend_origin/?deploy-verify=$(date +%s)" |
		grep -oE 'index-[A-Za-z0-9_-]+\.js' | head -1
}

# Every operand of a pass/fail comparison must be validated before the comparison. An empty
# digest compared against an empty digest reports "match", which is this script's own failure
# domain in miniature: a check that passes while verifying nothing.
file_digest() {
	digest_value=""
	case "$digest_tool" in
		sha256sum) digest_value="$(sha256sum "$1" | cut -d' ' -f1)" ;;
		shasum) digest_value="$(shasum -a 256 "$1" | cut -d' ' -f1)" ;;
		*) die 'no digest tool resolved — refusing to compare digests that were never computed' ;;
	esac
	# A pipeline ending in `cut` exits 0 even when the hash command failed, so the exit status
	# proves nothing here; only the shape of the output does. Anchored and fully quantified on
	# purpose: a glob that pins a prefix and wildcards the rest admits 56 arbitrary trailing
	# bytes — the same "the wildcard is the hole" mistake this file's guards keep making.
	printf '%s' "$digest_value" | grep -qE '^[0-9a-f]{64}$' ||
		die "digest of $1 is not a sha256 (got '$digest_value') — refusing to compare"
	printf '%s' "$digest_value"
}

# A checkout behind origin/main rebuilds OLD code into a byte-identical bundle and uploads it
# successfully — every symptom of a good deploy, none of the effect. Nothing downstream can see
# that, so it must be caught here, and it must fail CLOSED: an unverifiable remote is not evidence
# the checkout is current.
deploy_sha="$(git -C "$root_dir" rev-parse --short HEAD 2>/dev/null || echo unknown)"
printf 'Deploying frontend/homepage static assets from %s\n' "$deploy_sha"
if git -C "$root_dir" rev-parse --git-dir >/dev/null 2>&1; then
	if ! git -C "$root_dir" fetch --quiet origin main 2>/dev/null; then
		[ "${NOTTY_DEPLOY_ALLOW_STALE_REMOTE:-0}" = "1" ] ||
			die 'could not fetch origin/main, so the checkout cannot be proven current. Set NOTTY_DEPLOY_ALLOW_STALE_REMOTE=1 to deploy anyway'
		printf 'deploy-frontend: WARNING - origin/main not refreshed; staleness unchecked by explicit override\n' >&2
	fi
	if git -C "$root_dir" rev-parse --verify --quiet origin/main >/dev/null 2>&1 &&
		! git -C "$root_dir" merge-base --is-ancestor origin/main HEAD; then
		behind="$(git -C "$root_dir" rev-list --count HEAD..origin/main 2>/dev/null || echo '?')"
		die "checkout is $behind commit(s) behind origin/main — this would rebuild and ship stale code. Run: git pull"
	fi
fi

# Read the live bundle BEFORE uploading, so "the content did not change" can be reported as a
# stated fact rather than inferred from silence.
before_ref="$(live_bundle_ref || true)"

"$root_dir/scripts/build-frontend.sh"

expected_ref="$(bundle_ref "$app_index")"
[ -n "$expected_ref" ] || die "no bundle reference found in $app_index — the build did not produce an app index"
expected_asset="$app_dist_dir/assets/$expected_ref"
[ -f "$expected_asset" ] || die "built index references $expected_ref but $expected_asset does not exist"

# Computed before publishing: if the built asset cannot be hashed, nothing should ship, and
# discovering that after the upload leaves prod changed and unverifiable.
expected_digest="$(file_digest "$expected_asset")"

UPLOAD_TARGET=frontend "$root_dir/scripts/upload-r2.sh"

# The uploader exiting 0 is not evidence it reached users: on 2026-07-30 `rclone sync
# --no-check-dest` transferred zero bytes for two days while reporting success. Only the served
# object proves a deploy happened — and only fetching the referenced bundle proves the app LOADS.
# A live index pointing at a missing asset is a white screen that a filename comparison calls fine.
attempt=1
while :; do
	served_ref="$(live_bundle_ref || true)"
	if [ "$served_ref" = "$expected_ref" ]; then
		served_asset="$(mktemp)"
		if ! fetch_live "$frontend_origin/assets/$expected_ref" >"$served_asset"; then
			rm -f "$served_asset"
			die "live index references $expected_ref but that asset is not retrievable from $frontend_origin — users would get a blank page"
		fi
		served_digest="$(file_digest "$served_asset")"
		rm -f "$served_asset"
		[ "$served_digest" = "$expected_digest" ] ||
			die "live $expected_ref does not match the built asset (served $served_digest, built $expected_digest) — a partial sync is live"
		if [ "$before_ref" = "$expected_ref" ]; then
			printf 'deploy-frontend: live bundle is %s — UNCHANGED from before this deploy.\n' "$served_ref"
			printf 'deploy-frontend: upload succeeded and content is identical, so either you redeployed the same commit or this checkout (%s) does not contain the change you expected.\n' "$deploy_sha"
		else
			printf 'deploy-frontend: verified live at %s — bundle %s -> %s, asset digest matches\n' \
				"$frontend_origin" "${before_ref:-unknown}" "$served_ref"
		fi
		printf 'Frontend/homepage deploy complete\n'
		exit 0
	fi
	attempt=$((attempt + 1))
	[ "$attempt" -le "$live_check_attempts" ] || break
	[ "$live_check_interval" -eq 0 ] || sleep "$live_check_interval"
done

die "upload reported success but $frontend_origin/ still serves ${served_ref:-<none>}, expected $expected_ref after $live_check_attempts attempts — the deploy did NOT reach users"
