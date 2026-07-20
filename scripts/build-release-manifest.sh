#!/usr/bin/env sh
# build-release-manifest.sh — emit the frozen manifest.json v1 that is the
# download resolver contract consumed by task #61 (and handed to the frontend).
#
# Contract (frozen with Vitaliy):
#   - Exactly three asset keys are required: macos-universal, windows-amd64,
#     windows-arm64. All three asset files must exist. If any is missing this
#     script fails and emits nothing — the release is all-three-or-nothing, so a
#     partial manifest that would produce a dead download link is never written.
#   - Published (versionless) filenames are fixed:
#       macos-universal -> Codesk-macos-universal.dmg   (universal .dmg, not .zip)
#       windows-amd64   -> Codesk-windows-amd64.msi
#       windows-arm64   -> Codesk-windows-arm64.msi
#   - Each asset records exact filename, 64-hex sha256 (computed on the actual
#     published bytes), positive byte size, and explicit platform-appropriate
#     signing state: macOS "signed_and_notarized", Windows "signed". Both default
#     false until code signing lands (task #41).
#   - The manifest carries schema_version, the normalized version, and the
#     immutable release_tag (desktop-vX.Y.Z) so a consumer reading the rolling
#     desktop-latest manifest still learns the exact version it resolved.
#
# Usage:
#   build-release-manifest.sh \
#     --version 1.2.3 --release-tag desktop-v1.2.3 \
#     --macos-universal path/to/Codesk-macos-universal.dmg \
#     --windows-amd64  path/to/Codesk-windows-amd64.msi \
#     --windows-arm64  path/to/Codesk-windows-arm64.msi \
#     [--macos-signed-and-notarized true|false] [--windows-signed true|false] \
#     --out path/to/manifest.json
set -eu

fail() {
	printf 'build-release-manifest: %s\n' "$1" >&2
	exit 1
}

version=""
release_tag=""
macos_path=""
win_amd64_path=""
win_arm64_path=""
macos_signed="false"
windows_signed="false"
out=""

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version) version="${2:?--version needs a value}"; shift 2 ;;
		--release-tag) release_tag="${2:?--release-tag needs a value}"; shift 2 ;;
		--macos-universal) macos_path="${2:?}"; shift 2 ;;
		--windows-amd64) win_amd64_path="${2:?}"; shift 2 ;;
		--windows-arm64) win_arm64_path="${2:?}"; shift 2 ;;
		--macos-signed-and-notarized) macos_signed="${2:?}"; shift 2 ;;
		--windows-signed) windows_signed="${2:?}"; shift 2 ;;
		--out) out="${2:?--out needs a value}"; shift 2 ;;
		*) fail "unknown argument: $1" ;;
	esac
done

[ -n "$version" ] || fail 'missing --version'
[ -n "$release_tag" ] || fail 'missing --release-tag'
[ -n "$out" ] || fail 'missing --out'

# Version must be canonical X.Y.Z (defense in depth: the workflow normalizes the
# tag first, but the manifest must never record a non-canonical version).
case "$version" in
	*[!0-9.]*) fail "version is not canonical numeric X.Y.Z: $version" ;;
esac

# Booleans must be literally true/false so the emitted JSON is valid and the
# signing-state types are stable for the resolver.
validate_bool() {
	case "$2" in
		true|false) : ;;
		*) fail "$1 must be 'true' or 'false', got: $2" ;;
	esac
}
validate_bool --macos-signed-and-notarized "$macos_signed"
validate_bool --windows-signed "$windows_signed"

# Portable sha256 (Linux sha256sum / macOS-style shasum).
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		fail 'neither sha256sum nor shasum is available'
	fi
}

# Emit one asset object. Requires the file to exist and be non-empty, computes
# its digest/size, and prints the JSON fragment (no trailing comma).
asset_json() {
	key="$1"
	filename="$2"
	path="$3"
	signing_field="$4"
	signing_value="$5"
	[ -n "$path" ] || fail "missing path for required asset $key"
	[ -f "$path" ] || fail "required asset $key not found: $path"
	size="$(wc -c < "$path" | tr -d ' ')"
	[ "$size" -gt 0 ] || fail "required asset $key is empty: $path"
	digest="$(sha256_of "$path")"
	case "$digest" in
		[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) : ;;
		*) fail "computed sha256 for $key is not lowercase hex: $digest" ;;
	esac
	printf '    "%s": {\n' "$key"
	printf '      "filename": "%s",\n' "$filename"
	printf '      "sha256": "%s",\n' "$digest"
	printf '      "size_bytes": %s,\n' "$size"
	printf '      "signing": { "%s": %s }\n' "$signing_field" "$signing_value"
	printf '    }'
}

tmp="$(mktemp "${TMPDIR:-/tmp}/release-manifest.XXXXXX")" || fail 'mktemp failed'
# Clean up the temp file on any exit so a failed run never leaves a partial file.
trap 'rm -f "$tmp"' EXIT INT TERM

{
	printf '{\n'
	printf '  "schema_version": 1,\n'
	printf '  "version": "%s",\n' "$version"
	printf '  "release_tag": "%s",\n' "$release_tag"
	printf '  "assets": {\n'
	asset_json "macos-universal" "Codesk-macos-universal.dmg" "$macos_path" "signed_and_notarized" "$macos_signed"
	printf ',\n'
	asset_json "windows-amd64" "Codesk-windows-amd64.msi" "$win_amd64_path" "signed" "$windows_signed"
	printf ',\n'
	asset_json "windows-arm64" "Codesk-windows-arm64.msi" "$win_arm64_path" "signed" "$windows_signed"
	printf '\n'
	printf '  }\n'
	printf '}\n'
} > "$tmp"

# Only publish the manifest atomically once every required asset was present and
# hashed — never leave a half-written manifest behind.
mv "$tmp" "$out"
trap - EXIT INT TERM
printf '%s\n' "$out"
