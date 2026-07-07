# Test-artifact tmp discipline. Every harness (and every agent worktree) that writes scratch state to
# disk should place it UNDER a single well-known root, so a periodic sweep can reclaim orphans from
# crashed or abandoned runs without having to guess which /tmp entries are ours. The root hit 100% on
# 2026-07-06 and broke tooling for multiple agents; this is the guard against a repeat.
#
# See scripts/sweep-test-tmp.sh for the age-based reclaim and scripts/README.md for the convention.

# notty_test_tmp_root: the single directory all test scratch lives under. Override with
# NOTTY_TEST_TMP_ROOT (e.g. to point at a bigger volume); otherwise ${TMPDIR:-/tmp}/notty-test.
notty_test_tmp_root() {
	printf '%s\n' "${NOTTY_TEST_TMP_ROOT:-${TMPDIR:-/tmp}/notty-test}"
}

# notty_test_mktemp <label>: create and print a fresh per-run directory under the root. The label
# only aids humans reading `ls` during an incident; uniqueness comes from mktemp.
notty_test_mktemp() {
	label="${1:-run}"
	root="$(notty_test_tmp_root)"
	mkdir -p "$root"
	mktemp -d "$root/${label}.XXXXXX"
}
