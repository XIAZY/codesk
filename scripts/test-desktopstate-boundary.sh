#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
. "$root_dir/scripts/lib/testtmp.sh"

fail() {
	printf 'test-desktopstate-boundary: %s\n' "$*" >&2
	exit 1
}

tmp_dir="$(notty_test_mktemp desktopstate-boundary)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

for arch in amd64 arm64; do
	dependencies="$(
		cd "$root_dir"
		CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go list -deps ./daemon/internal/desktopstate
	)" || fail "cannot resolve windows/$arch dependencies"
	while IFS= read -r dependency; do
		case "$dependency" in
			notty/daemon/internal/desktop|notty/daemon/internal/syncer|notty/daemon/internal/syncer/*|notty/internal/ycrdt|notty/internal/ycrdt/*)
				fail "windows/$arch unexpectedly depends on $dependency"
				;;
		esac
	done <<EOF
$dependencies
EOF
	(
		cd "$root_dir"
		CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
			go test -c -o "$tmp_dir/desktopstate-$arch.test.exe" ./daemon/internal/desktopstate
	) || fail "windows/$arch CGO-free test construction failed"
done

printf '%s\n' 'test-desktopstate-boundary: CGO-free Windows AMD64/ARM64 dependency boundary passed'
