#!/usr/bin/env sh
set -eu

release_dir="${1:-}"
version="${2:-}"
allow_unsigned="${ALLOW_UNSIGNED_MACOS_DESKTOP:-}"

[ -n "$release_dir" ] && [ -n "$version" ] || {
	printf '%s\n' 'usage: scripts/verify-macos-desktop-release.sh <release-dir> <version>' >&2
	exit 2
}

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
. "$root_dir/scripts/lib/testtmp.sh"

go_toolchain='go1.26.5'
export GOTOOLCHAIN="$go_toolchain"
export GOENV=off
export GO111MODULE=on
export GOWORK=off
unset GOFLAGS GOEXPERIMENT

fail() {
	printf 'verify-macos-desktop-release: %s\n' "$*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || fail 'a macOS verification host is required'
for command in codesign go hdiutil plutil spctl xcrun; do
	command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done
actual_go_version="$(go env GOVERSION 2>/dev/null || true)"
[ "$actual_go_version" = "$go_toolchain" ] || fail "$go_toolchain is required (got ${actual_go_version:-unknown})"
case "$allow_unsigned" in
	""|1) ;;
	*) fail 'ALLOW_UNSIGNED_MACOS_DESKTOP must be unset or exactly 1' ;;
esac
case "$release_dir" in
	/*) ;;
	*) release_dir="$(pwd)/$release_dir" ;;
esac
[ -d "$release_dir" ] || fail "release directory does not exist: $release_dir"
release_dir="$(CDPATH= cd -- "$release_dir" && pwd -P)"
manifest="$release_dir/manifest.json"
[ -f "$manifest" ] && [ ! -L "$manifest" ] || fail 'manifest.json must be a real file'
manifest_signed="$(plutil -extract signed_and_notarized raw -o - "$manifest" 2>/dev/null)" ||
	fail 'manifest.json does not contain a valid signed_and_notarized claim'
case "$manifest_signed" in
	true) signed=true ;;
	false)
		[ "$allow_unsigned" = 1 ] || fail 'artifact is unsigned; set ALLOW_UNSIGNED_MACOS_DESKTOP=1 only for construction evidence'
		signed=false
		;;
	*) fail 'manifest.json signed_and_notarized must be true or false' ;;
esac

tmp_dir="$(notty_test_mktemp codesk-macos-desktop-verify)"
mount_dir="$tmp_dir/mount"
release_tool="$tmp_dir/codesk-macos-release"
mounted=0
cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ "$mounted" -eq 1 ]; then
		hdiutil detach "$mount_dir" -quiet >/dev/null 2>&1 || status=1
	fi
	rm -rf "$tmp_dir"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

(
	cd "$root_dir"
	go build -buildvcs=false -trimpath -ldflags '-buildid=' \
		-o "$release_tool" ./daemon/cmd/codesk-macos-release
)
if [ "$signed" = true ]; then
	"$release_tool" verify "$release_dir" "$version"
else
	"$release_tool" verify --allow-unsigned "$release_dir" "$version"
fi

app="$release_dir/Codesk.app"
dmg="$release_dir/Codesk_${version}_macos_universal.dmg"
hdiutil verify "$dmg" >/dev/null
if [ "$signed" = true ]; then
	codesign --verify --strict --verbose=4 "$app/Contents/Helpers/notty-agent-tool"
	codesign --verify --strict --verbose=4 "$app"
	codesign --verify --verbose=4 "$dmg"
	xcrun stapler validate "$app"
	xcrun stapler validate "$dmg"
	spctl --assess --type execute --verbose=4 "$app"
	spctl --assess --type open --context context:primary-signature --verbose=4 "$dmg"
fi

mkdir -p "$mount_dir"
hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mount_dir" "$dmg"
mounted=1
"$release_tool" verify-volume --mount "$mount_dir"
development_arg=''
[ "$version" != dev ] || development_arg='--development'
source_hash="$("$release_tool" verify-app --print-tree-hash --app "$app" --version "$version" $development_arg)"
mounted_hash="$("$release_tool" verify-app --print-tree-hash --app "$mount_dir/Codesk.app" --version "$version" $development_arg)"
[ "$source_hash" = "$mounted_hash" ] || fail 'DMG application tree differs from the release app'
if [ "$signed" = true ]; then
	codesign --verify --strict --verbose=4 "$mount_dir/Codesk.app"
	spctl --assess --type execute --verbose=4 "$mount_dir/Codesk.app"
fi
hdiutil detach "$mount_dir" -quiet
mounted=0

if [ "$signed" = true ]; then
	printf 'Verified signed and notarized Codesk macOS desktop release %s in %s\n' "$version" "$release_dir"
else
	printf 'Verified unsigned construction-only Codesk macOS desktop release %s in %s (artifact trust NOT ESTABLISHED)\n' "$version" "$release_dir"
fi
