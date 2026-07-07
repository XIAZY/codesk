#!/bin/sh
set -eu

# Reclaim test scratch older than NOTTY_TEST_TMP_MAX_AGE_HOURS (default 24) from the test tmp root.
# Safe to run anytime and from anywhere: it only ever touches entries UNDER the dedicated root
# (notty_test_tmp_root), never generic /tmp/notty-* paths, so it cannot remove an active worktree, a
# deploy config, or anything outside the convention. Pair it with a timer for hands-off hygiene
# (see task #9's docker-prune timer for the systemd pattern).
#
# Dry run: NOTTY_SWEEP_DRY_RUN=1 scripts/sweep-test-tmp.sh   (prints what it would remove, deletes nothing)

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
. "$ROOT_DIR/scripts/lib/testtmp.sh"

AGE_HOURS="${NOTTY_TEST_TMP_MAX_AGE_HOURS:-24}"
DRY_RUN="${NOTTY_SWEEP_DRY_RUN:-0}"
ROOT="$(notty_test_tmp_root)"
age_min=$((AGE_HOURS * 60))

if [ ! -d "$ROOT" ]; then
	printf 'nothing to sweep: %s does not exist\n' "$ROOT"
	exit 0
fi

# Only immediate children of the root are per-run units; -mmin +N selects those last modified more
# than N minutes ago. mtime of the dir advances while a run writes into it, so an in-progress run is
# not eligible until it has been idle for the full window.
removed=0
find "$ROOT" -mindepth 1 -maxdepth 1 -mmin "+$age_min" -print | while IFS= read -r path; do
	if [ "$DRY_RUN" = "1" ]; then
		printf 'would remove: %s\n' "$path"
	else
		rm -rf "$path"
		printf 'removed: %s\n' "$path"
	fi
	removed=$((removed + 1))
done

printf 'sweep complete: root=%s max-age=%sh%s\n' "$ROOT" "$AGE_HOURS" "$([ "$DRY_RUN" = 1 ] && printf ' (dry-run)' || printf '')"
