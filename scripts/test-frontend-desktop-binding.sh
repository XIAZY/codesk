#!/usr/bin/env sh
# Deep-review guard (#1 of 8): the PRODUCTION build path — scripts/build-frontend.sh — must forward
# the desktop static base into the compiled bundle so the download modal fetches the R2 manifest
# host, not the same-origin fallback that shipped before this fix.
#
# This test INVOKES scripts/build-frontend.sh (NOT vite directly — an earlier version bypassed the
# script and so survived deleting the forwarding line, manufacturing false confidence) with a
# sentinel NOTTY_STATIC_ORIGIN and an isolated STATIC_DIST_DIR, then greps the emitted app bundle for
# the sentinel desktop route. It is a class-killer: deleting the `VITE_DESKTOP_STATIC_BASE=...`
# forwarding line from build-frontend.sh MUST turn this RED (that mutation makes the bundle fall back
# to <public-origin>/desktop, so the sentinel desktop route disappears while sentinel/daemons stays —
# which is why we grep the desktop route specifically).
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

sentinel_origin="https://desktop-static-binding-probe.invalid"
sentinel_route="$sentinel_origin/desktop"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT INT TERM

NOTTY_STATIC_ORIGIN="$sentinel_origin" STATIC_DIST_DIR="$work_dir" \
	"$root_dir/scripts/build-frontend.sh" >/dev/null 2>&1 || fail 'scripts/build-frontend.sh failed'

if grep -rq "$sentinel_route" "$work_dir/app"; then
	pass 'build-frontend.sh binds the desktop static base into the compiled app bundle'
else
	fail 'compiled app bundle lacks the desktop static route — build-frontend.sh is not forwarding VITE_DESKTOP_STATIC_BASE'
fi
