#!/usr/bin/env sh
set -eu

version="${1:-${VERSION:-dev}}"
dist_dir="${2:-${DIST_DIR:-dist/macos-desktop}}"
unsigned_override="${ALLOW_UNSIGNED_MACOS_DESKTOP:-}"
sign_identity="${CODESK_MACOS_SIGN_IDENTITY:-}"
notary_profile="${CODESK_MACOS_NOTARY_PROFILE:-}"

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
binary_version="$(cat "$root_dir/VERSION")" || { printf 'build-macos-desktop-release: VERSION file is required\n' >&2; exit 1; }
[ -n "$binary_version" ] || { printf 'build-macos-desktop-release: VERSION file must not be empty\n' >&2; exit 1; }
. "$root_dir/scripts/lib/testtmp.sh"

go_toolchain='go1.26.5'
minimum_macos='13.0'
export GOTOOLCHAIN="$go_toolchain"
export GOENV=off
export GO111MODULE=on
export GOWORK=off
export GOAMD64=v1
export GOARM64=v8.0
export LC_ALL=C
export TZ=UTC
export SOURCE_DATE_EPOCH=0
unset GOFLAGS GOEXPERIMENT
unset CC CXX CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS
unset RUSTFLAGS CARGO_ENCODED_RUSTFLAGS RUSTC RUSTDOC RUSTC_WRAPPER RUSTC_WORKSPACE_WRAPPER
unset CARGO_TARGET_DIR CARGO_BUILD_TARGET CARGO_BUILD_RUSTC CARGO_BUILD_RUSTDOC
unset CARGO_PROFILE_RELEASE_CODEGEN_UNITS CARGO_PROFILE_RELEASE_DEBUG CARGO_PROFILE_RELEASE_INCREMENTAL
unset CARGO_PROFILE_RELEASE_LTO CARGO_PROFILE_RELEASE_OPT_LEVEL CARGO_PROFILE_RELEASE_OVERFLOW_CHECKS
unset CARGO_PROFILE_RELEASE_PANIC CARGO_PROFILE_RELEASE_RPATH CARGO_PROFILE_RELEASE_STRIP
export CARGO_INCREMENTAL=0
umask 022

fail() {
	printf 'build-macos-desktop-release: %s\n' "$*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || fail 'a macOS build host is required'

case "$dist_dir" in
	/*) dist_abs="$dist_dir" ;;
	*) dist_abs="$root_dir/$dist_dir" ;;
esac

case "$unsigned_override" in
	"") signed=true ;;
	1) signed=false ;;
	*) fail 'ALLOW_UNSIGNED_MACOS_DESKTOP must be unset or exactly 1' ;;
esac
if [ "$signed" = true ]; then
	[ -n "$sign_identity" ] || fail 'CODESK_MACOS_SIGN_IDENTITY is required; use ALLOW_UNSIGNED_MACOS_DESKTOP=1 only for construction evidence'
	case "$sign_identity" in
		'Developer ID Application:'*) ;;
		*) fail 'CODESK_MACOS_SIGN_IDENTITY must name a Developer ID Application identity' ;;
	esac
	[ -n "$notary_profile" ] || fail 'CODESK_MACOS_NOTARY_PROFILE is required'
	[ "$version" != dev ] || fail 'signed releases require a numeric X.Y.Z version'
else
	[ -z "$sign_identity" ] || fail 'do not combine CODESK_MACOS_SIGN_IDENTITY with ALLOW_UNSIGNED_MACOS_DESKTOP=1'
	[ -z "$notary_profile" ] || fail 'do not combine CODESK_MACOS_NOTARY_PROFILE with ALLOW_UNSIGNED_MACOS_DESKTOP=1'
	printf '%s\n' 'build-macos-desktop-release: UNSIGNED CONSTRUCTION-ONLY MODE' >&2
fi

for command in go cargo rustc xcrun codesign hdiutil ditto plutil spctl git; do
	command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done
actual_go_version="$(go env GOVERSION 2>/dev/null || true)"
[ "$actual_go_version" = "$go_toolchain" ] || fail "$go_toolchain is required (got ${actual_go_version:-unknown})"
rustc_version="$(rustc --version 2>/dev/null || true)"
[ "$rustc_version" = 'rustc 1.97.0 (2d8144b78 2026-07-07)' ] || fail "rustc 1.97.0 is required (got ${rustc_version:-unknown})"
cargo_version="$(cargo --version 2>/dev/null || true)"
[ "$cargo_version" = 'cargo 1.97.0 (c980f4866 2026-06-30)' ] || fail "cargo 1.97.0 is required (got ${cargo_version:-unknown})"
export RUSTC="$(command -v rustc)"

sdk_root="$(xcrun --sdk macosx --show-sdk-path)"
clang="$(xcrun --sdk macosx --find clang)"
clangxx="$(xcrun --sdk macosx --find clang++)"
lipo="$(xcrun --sdk macosx --find lipo)"
[ -d "$sdk_root" ] || fail 'the macOS SDK is unavailable'
[ -x "$clang" ] || fail 'the macOS clang toolchain is unavailable'
[ -x "$clangxx" ] || fail 'the macOS clang++ toolchain is unavailable'
[ -x "$lipo" ] || fail 'lipo is unavailable'

case "$version" in
	dev) development_arg='--development' ;;
	*) development_arg='' ;;
esac

mkdir -p "$dist_abs"
dist_abs="$(CDPATH= cd -- "$dist_abs" && pwd -P)"
[ "$dist_abs" != / ] || fail 'release output directory must not be the filesystem root'

tmp_dir="$(notty_test_mktemp codesk-macos-desktop-release)"
export CARGO_HOME="$tmp_dir/cargo-home"
rust_flag_separator="$(printf '\037')"
export CARGO_ENCODED_RUSTFLAGS="-C${rust_flag_separator}panic=abort${rust_flag_separator}--remap-path-prefix=$root_dir=.${rust_flag_separator}--remap-path-prefix=$tmp_dir=/build"
publish_dir="$tmp_dir/publish/$version"
app="$publish_dir/Codesk.app"
contents="$app/Contents"
desktop="$contents/MacOS/Codesk"
agent_tool="$contents/Helpers/notty-agent-tool"
icon="$root_dir/daemon/cmd/codesk-desktop/assets/Codesk.icns"
template_icon="$root_dir/daemon/cmd/codesk-desktop/assets/codesk-tray-template.png"
entitlements="$root_dir/daemon/cmd/codesk-desktop/codesk.entitlements"
release_tool="$tmp_dir/codesk-macos-release"
dmg_name="Codesk_${version}_macos_universal.dmg"
dmg="$publish_dir/$dmg_name"
mount_dir="$tmp_dir/mount"
mounted=0
yffi_touched=0

restore_host_yffi() {
	if [ "$yffi_touched" -eq 0 ]; then
		return 0
	fi
	printf '%s\n' 'build-macos-desktop-release: restoring host yffi library'
	if ! (
		unset RUST_TARGET RUSTFLAGS CARGO_ENCODED_RUSTFLAGS CARGO_HOME CARGO_TARGET_DIR RUSTC MACOSX_DEPLOYMENT_TARGET SDKROOT
		cd "$root_dir"
		"$root_dir/scripts/build-yffi.sh"
	); then
		return 1
	fi
	yffi_touched=0
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ "$mounted" -eq 1 ]; then
		hdiutil detach "$mount_dir" -quiet >/dev/null 2>&1 || status=1
	fi
	if [ "$yffi_touched" -ne 0 ]; then
		if ! restore_host_yffi; then
			printf '%s\n' 'build-macos-desktop-release: failed to restore the host yffi library' >&2
			status=1
		fi
	fi
	chmod -R u+w "$tmp_dir" 2>/dev/null || true
	rm -rf "$tmp_dir"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rust_target_for() {
	case "$1" in
		amd64) printf '%s' 'x86_64-apple-darwin' ;;
		arm64) printf '%s' 'aarch64-apple-darwin' ;;
		*) fail "unsupported architecture $1" ;;
	esac
}

clang_arch_for() {
	case "$1" in
		amd64) printf '%s' 'x86_64' ;;
		arm64) printf '%s' 'arm64' ;;
		*) fail "unsupported architecture $1" ;;
	esac
}

notarize_and_wait() {
	artifact="$1"
	label="$2"
	result="$tmp_dir/notary-$label.json"
	xcrun notarytool submit "$artifact" --keychain-profile "$notary_profile" --wait --output-format json >"$result"
	status="$(plutil -extract status raw -o - "$result" 2>/dev/null || true)"
	[ "$status" = Accepted ] || fail "$label notarization status is ${status:-unknown}"
}

source_revision="$(git -C "$root_dir" rev-parse --verify HEAD)"
case "$source_revision" in
	????????????????????????????????????????) ;;
	*) fail 'could not resolve a full source revision' ;;
esac
source_status="$(git -C "$root_dir" status --porcelain=v1 --untracked-files=all)"
[ -z "$source_status" ] || fail 'source checkout must have no tracked, staged, or untracked changes before building a release'

printf '%s\n' 'build-macos-desktop-release: staging host yffi library from a clean locked build'
(
	unset RUST_TARGET RUSTFLAGS CARGO_TARGET_DIR RUSTC MACOSX_DEPLOYMENT_TARGET SDKROOT
	cd "$root_dir"
	"$root_dir/scripts/build-yffi.sh"
)

(
	cd "$root_dir"
	go test ./daemon/cmd/codesk-macos-release ./daemon/internal/macosapp ./daemon/internal/desktop ./daemon/internal/desktopapp ./daemon/cmd/codesk-desktop
	go build -buildvcs=false -trimpath -ldflags '-buildid=' -o "$release_tool" ./daemon/cmd/codesk-macos-release
)
"$release_tool" validate-version $development_arg "$version" >/dev/null
mkdir -p "$publish_dir" "$contents/MacOS" "$contents/Helpers" "$contents/Resources"

generated_icon="$tmp_dir/generated-Codesk.icns"
generated_template="$tmp_dir/generated-codesk-tray-template.png"
(
	cd "$root_dir"
	go run -buildvcs=false ./scripts/generate-codesk-macos-assets.go "$generated_icon" "$generated_template"
)
cmp "$generated_icon" "$icon" || fail 'committed Codesk.icns is not reproducible'
cmp "$generated_template" "$template_icon" || fail 'committed tray template icon is not reproducible'
[ -f "$entitlements" ] || fail "missing entitlements: $entitlements"

for arch in amd64 arm64; do
	rust_target="$(rust_target_for "$arch")"
	clang_arch="$(clang_arch_for "$arch")"
	arch_dir="$tmp_dir/$arch"
	mkdir -p "$arch_dir"
	libdir="$(rustc --print target-libdir --target "$rust_target" 2>/dev/null || true)"
	[ -n "$libdir" ] && [ -d "$libdir" ] || fail "Rust target $rust_target is not installed"
	printf 'build-macos-desktop-release: building yffi for %s\n' "$rust_target"
	yffi_touched=1
	(
		cd "$root_dir"
		SDKROOT="$sdk_root" MACOSX_DEPLOYMENT_TARGET="$minimum_macos" RUST_TARGET="$rust_target" scripts/build-yffi.sh
	)
	printf 'build-macos-desktop-release: building application for %s\n' "$arch"
	(
		cd "$root_dir"
		SDKROOT="$sdk_root" MACOSX_DEPLOYMENT_TARGET="$minimum_macos" \
			CC="$clang" CXX="$clangxx" \
			CGO_CFLAGS="-arch $clang_arch -mmacosx-version-min=$minimum_macos" \
			CGO_CXXFLAGS="-arch $clang_arch -mmacosx-version-min=$minimum_macos" \
			CGO_LDFLAGS="-arch $clang_arch -mmacosx-version-min=$minimum_macos" \
			CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" \
			go build -buildvcs=false -trimpath \
			-ldflags "-buildid= -linkmode external -s -w -X notty/daemon/internal/buildinfo.Version=$binary_version" \
			-o "$arch_dir/Codesk" ./daemon/cmd/codesk-desktop
		SDKROOT="$sdk_root" MACOSX_DEPLOYMENT_TARGET="$minimum_macos" \
			CC="$clang" CXX="$clangxx" \
			CGO_CFLAGS="-arch $clang_arch -mmacosx-version-min=$minimum_macos" \
			CGO_CXXFLAGS="-arch $clang_arch -mmacosx-version-min=$minimum_macos" \
			CGO_LDFLAGS="-arch $clang_arch -mmacosx-version-min=$minimum_macos" \
			CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" \
			go build -buildvcs=false -trimpath \
			-ldflags "-buildid= -linkmode external -s -w -X notty/daemon/internal/buildinfo.Version=$binary_version" \
			-o "$arch_dir/notty-agent-tool" ./daemon/cmd/agenttool
	)
done

"$lipo" -create "$tmp_dir/amd64/Codesk" "$tmp_dir/arm64/Codesk" -output "$desktop"
"$lipo" -create "$tmp_dir/amd64/notty-agent-tool" "$tmp_dir/arm64/notty-agent-tool" -output "$agent_tool"
"$lipo" "$desktop" -verify_arch x86_64 arm64
"$lipo" "$agent_tool" -verify_arch x86_64 arm64
chmod 0755 "$desktop" "$agent_tool"
cp "$icon" "$contents/Resources/Codesk.icns"
chmod 0644 "$contents/Resources/Codesk.icns"
"$release_tool" plist --output "$contents/Info.plist" --version "$version" $development_arg
"$release_tool" verify-app --app "$app" --version "$version" $development_arg

restore_host_yffi

if [ "$signed" = true ]; then
	printf '%s\n' 'build-macos-desktop-release: signing nested helper'
	codesign --force --timestamp --options runtime --sign "$sign_identity" "$agent_tool"
	printf '%s\n' 'build-macos-desktop-release: signing application bundle'
	codesign --force --timestamp --options runtime --entitlements "$entitlements" --sign "$sign_identity" "$app"
	codesign --verify --strict --verbose=4 "$agent_tool"
	codesign --verify --strict --verbose=4 "$app"
	"$release_tool" verify-app --app "$app" --version "$version"

	notary_zip="$tmp_dir/Codesk.app.zip"
	ditto -c -k --sequesterRsrc --keepParent "$app" "$notary_zip"
	notarize_and_wait "$notary_zip" app
	xcrun stapler staple "$app"
	xcrun stapler validate "$app"
	codesign --verify --strict --verbose=4 "$app"
	spctl --assess --type execute --verbose=4 "$app"
fi

dmg_root="$tmp_dir/dmg-root"
mkdir -p "$dmg_root"
ditto "$app" "$dmg_root/Codesk.app"
ln -s /Applications "$dmg_root/Applications"
hdiutil create -quiet -format UDZO -fs HFS+ -volname Codesk -srcfolder "$dmg_root" "$dmg"
hdiutil verify "$dmg" >/dev/null

if [ "$signed" = true ]; then
	codesign --force --timestamp --sign "$sign_identity" "$dmg"
	notarize_and_wait "$dmg" dmg
	xcrun stapler staple "$dmg"
	xcrun stapler validate "$dmg"
	codesign --verify --verbose=4 "$dmg"
	spctl --assess --type open --context context:primary-signature --verbose=4 "$dmg"
	hdiutil verify "$dmg" >/dev/null
fi

mkdir -p "$mount_dir"
hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mount_dir" "$dmg"
mounted=1
"$release_tool" verify-volume --mount "$mount_dir"
source_tree_hash="$("$release_tool" verify-app --print-tree-hash --app "$app" --version "$version" $development_arg)"
mounted_tree_hash="$("$release_tool" verify-app --print-tree-hash --app "$mount_dir/Codesk.app" --version "$version" $development_arg)"
[ "$source_tree_hash" = "$mounted_tree_hash" ] || fail 'DMG application tree differs from the verified source app'
if [ "$signed" = true ]; then
	codesign --verify --strict --verbose=4 "$mount_dir/Codesk.app"
	spctl --assess --type execute --verbose=4 "$mount_dir/Codesk.app"
fi
hdiutil detach "$mount_dir" -quiet
mounted=0

"$release_tool" manifest --output "$publish_dir" --version "$version" --source-revision "$source_revision" --signed="$signed" $development_arg
if [ "$signed" = true ]; then
	"$release_tool" verify "$publish_dir" "$version"
else
	"$release_tool" verify --allow-unsigned "$publish_dir" "$version"
fi

out_dir="$dist_abs/$version"
rm -rf "$out_dir"
mv "$publish_dir" "$out_dir"
if [ "$signed" = true ]; then
	printf 'Built signed and notarized Codesk macOS desktop release %s in %s\n' "$version" "$out_dir"
else
	printf 'Built unsigned construction-only Codesk macOS desktop release %s in %s\n' "$version" "$out_dir"
fi
