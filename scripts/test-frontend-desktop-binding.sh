#!/usr/bin/env sh
# Deep-review P1 guard (#1 of 8): the compiled frontend bundle must BIND VITE_DESKTOP_STATIC_BASE
# so the desktop download modal fetches the R2 manifest host — not the same-origin fallback that
# shipped to production before this fix. Builds the app with a sentinel origin and asserts the
# sentinel appears in the compiled JS. It fails EXACTLY when the build stops forwarding the desktop
# static base (the original defect: build-frontend.sh omitted VITE_DESKTOP_STATIC_BASE, so the
# bundle contained app.getcodesk.com/desktop instead of the R2 route).
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

sentinel="https://desktop-static-binding-probe.invalid/desktop"
out_dir="$root_dir/frontend/dist"

(
	cd "$root_dir/frontend"
	VITE_DESKTOP_STATIC_BASE="$sentinel" npm run build >/dev/null 2>&1
) || fail 'frontend build failed'

if grep -rq "$sentinel" "$out_dir"; then
	pass 'compiled bundle binds VITE_DESKTOP_STATIC_BASE to the desktop static route'
else
	fail 'compiled bundle does NOT contain the desktop static base route (VITE_DESKTOP_STATIC_BASE not bound)'
fi
