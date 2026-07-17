#!/usr/bin/env sh

notty_desktop_release_source_revision() (
	if [ "$#" -ne 1 ]; then
		printf '%s\n' 'desktop-release: source root argument is required' >&2
		return 2
	fi
	notty_release_root="$1"
	notty_release_revision="$(git -C "$notty_release_root" rev-parse --verify HEAD 2>/dev/null || true)"
	case "$notty_release_revision" in
		????????????????????????????????????????) ;;
		*)
			printf '%s\n' 'desktop-release: source revision must be a full lowercase Git SHA' >&2
			return 1
			;;
	esac
	case "$notty_release_revision" in
		*[!0-9a-f]*|0000000000000000000000000000000000000000)
			printf '%s\n' 'desktop-release: source revision must be a full lowercase Git SHA' >&2
			return 1
			;;
	esac
	notty_release_status="$(git -C "$notty_release_root" status --porcelain=v1 --untracked-files=all)"
	if [ -n "$notty_release_status" ]; then
		printf '%s\n' 'desktop-release: source checkout must have no tracked, staged, or untracked changes before building a release' >&2
		return 1
	fi
	printf '%s\n' "$notty_release_revision"
)
