#!/usr/bin/env sh
# normalize-release-version.sh — resolve the canonical release version from the
# repository VERSION file and verify the release tag agrees with it.
#
# The VERSION file at repo root is the single source of truth for the version
# (read by the build, baked into every daemon via ldflags, and used for MSI
# ProductVersion and manifest.json). A `desktop-v<X.Y.Z>` tag is only the publish
# trigger — it must MATCH the file, never override it. This script enforces that
# so a release can never publish a tag whose version disagrees with the baked /
# packaged version.
#
# It:
#   - reads the VERSION file (default: repo-root VERSION),
#   - validates the file contents are a canonical numeric X.Y.Z within Windows
#     Installer ProductVersion ranges (major<=255, minor<=255, patch<=65535),
#   - strips a single `desktop-v` prefix from the tag and requires the remainder
#     to equal the file version exactly,
#   - prints the canonical X.Y.Z on success (exit 0), or fails closed (exit 1).
#
# Usage:
#   normalize-release-version.sh <tag> [<version-file>]
#   normalize-release-version.sh "$GITHUB_REF_NAME"
set -eu

fail() {
	printf 'normalize-release-version: %s\n' "$1" >&2
	exit 1
}

tag="${1:-}"
[ -n "$tag" ] || fail 'missing tag argument (expected desktop-v<X.Y.Z>)'

version_file="${2:-}"
if [ -z "$version_file" ]; then
	root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
	version_file="$root_dir/VERSION"
fi
[ -f "$version_file" ] || fail "VERSION file not found: $version_file"

# The file may have a trailing newline; take the first line and trim surrounding
# whitespace so "0.0.1\n" resolves to "0.0.1".
file_version="$(sed -n '1p' "$version_file" | tr -d '[:space:]')"
[ -n "$file_version" ] || fail "VERSION file is empty: $version_file"

# validate_canonical enforces the canonical + MSI-range contract on the version
# string (applied to the VERSION file, the source of truth).
validate_canonical() {
	value="$1"
	case "$value" in
		*-*) fail "prerelease versions are not accepted for v1: $value" ;;
		*+*) fail "build-metadata versions are not accepted for v1: $value" ;;
	esac
	old_ifs="$IFS"
	IFS='.'
	# shellcheck disable=SC2086
	set -- $value
	IFS="$old_ifs"
	[ "$#" -eq 3 ] || fail "version must be exactly MAJOR.MINOR.PATCH: $value"
	major="$1"; minor="$2"; patch="$3"
	for pair in "major=$major" "minor=$minor" "patch=$patch"; do
		name="${pair%%=*}"; field="${pair#*=}"
		case "$field" in
			'' ) fail "$name is empty: $value" ;;
			*[!0-9]* ) fail "$name is not numeric: $value" ;;
		esac
		case "$field" in
			0) : ;;
			0*) fail "$name has a leading zero (not canonical): $value" ;;
		esac
	done
	[ "$major" -le 255 ] || fail "major must be <= 255 for MSI ProductVersion: $value"
	[ "$minor" -le 255 ] || fail "minor must be <= 255 for MSI ProductVersion: $value"
	[ "$patch" -le 65535 ] || fail "patch must be <= 65535 for MSI ProductVersion: $value"
}

validate_canonical "$file_version"

# Strip a single desktop-v prefix from the tag and require an exact match with
# the file version. A doubled prefix or a mismatched version fails closed.
tag_version="$tag"
case "$tag_version" in
	desktop-v*) tag_version="${tag_version#desktop-v}" ;;
	*) fail "tag must start with desktop-v: $tag" ;;
esac

[ "$tag_version" = "$file_version" ] || fail "tag $tag does not match VERSION file ($file_version); bump VERSION or fix the tag"

printf '%s\n' "$file_version"
