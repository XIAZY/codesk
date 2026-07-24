#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'read-git-sha: usage: read-git-sha.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"

die() {
	printf 'read-git-sha: %s\n' "$*" >&2
	exit 1
}

command -v git >/dev/null 2>&1 || die "git is required"

if ! git_sha="$(git -C "$root_dir" rev-parse --short HEAD 2>/dev/null)"; then
	die "cannot resolve the current Git commit"
fi
case "$git_sha" in
	''|*[!0123456789abcdef]*) die "Git returned an invalid short commit SHA" ;;
esac

printf '%s\n' "$git_sha"
