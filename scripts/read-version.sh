#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
version_file="$root_dir/VERSION"

fail() {
	printf 'read-version: %s\n' "$*" >&2
	exit 1
}

[ -f "$version_file" ] || fail "VERSION must be a regular file: $version_file"
[ "$(wc -l < "$version_file" | tr -d '[:space:]')" = 1 ] ||
	fail 'VERSION must contain exactly one LF- or CRLF-terminated line'
[ "$(tail -c 1 "$version_file" | wc -l | tr -d '[:space:]')" = 1 ] ||
	fail 'VERSION must end with exactly one LF or CRLF'
[ "$(LC_ALL=C tr -d '0123456789.\r\n' < "$version_file" | wc -c | tr -d '[:space:]')" = 0 ] ||
	fail 'VERSION contains bytes outside ASCII digits, dots, CR, and LF'

version="$(cat "$version_file")"
cr="$(printf '\r')"
case "$version" in
	*"$cr") version="${version%"$cr"}" ;;
esac
printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
	fail 'VERSION must be canonical X.Y.Z without whitespace or leading zeros'
printf '%s\n' "$version" | awk -F. '
	$1 <= 255 && $2 <= 255 && $3 <= 65535 { ok = 1 }
	END { exit(ok ? 0 : 1) }
' || fail 'VERSION exceeds MSI limits (major/minor <= 255, build <= 65535)'

printf '%s\n' "$version"
