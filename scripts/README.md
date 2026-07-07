# scripts/

Build, deploy, and test helper scripts. This note documents the one cross-cutting convention every
script (and every human/agent working on the box) is expected to follow.

## Test-artifact tmp discipline

Test harnesses and throwaway worktrees write scratch state to disk. Left unmanaged it accumulates —
on 2026-07-06 the box's root filesystem hit 100% and broke tooling for multiple agents. The rule that
prevents a repeat has two halves:

1. **Everything scratch lives under one root.** `scripts/lib/testtmp.sh` defines it:
   - `notty_test_tmp_root` → `${NOTTY_TEST_TMP_ROOT:-${TMPDIR:-/tmp}/notty-test}`
   - `notty_test_mktemp <label>` → creates and prints a fresh per-run dir under that root.

   Harnesses source the lib and use the helper instead of `mktemp -d /tmp/...` directly (see
   `test-daemon-installer.sh`, `test-daemon-uninstall.sh`, `build-daemon-release.sh`). **If you spin
   up a worktree or scratch dir by hand, put it under the root too** — e.g.
   `git worktree add "$(sh -c '. scripts/lib/testtmp.sh; notty_test_mktemp worktree')" <branch>` —
   so the sweep can reclaim it when you abandon it.

2. **A sweep reclaims what outlived its run.** `scripts/sweep-test-tmp.sh` removes entries under the
   root older than `NOTTY_TEST_TMP_MAX_AGE_HOURS` (default 24):

   ```sh
   scripts/sweep-test-tmp.sh                      # reclaim >24h scratch
   NOTTY_SWEEP_DRY_RUN=1 scripts/sweep-test-tmp.sh   # show what it would remove
   NOTTY_TEST_TMP_MAX_AGE_HOURS=6 scripts/sweep-test-tmp.sh
   ```

   It only ever touches entries **under the dedicated root** — never generic `/tmp/notty-*` — so it
   cannot eat an active worktree, a deploy config, or anything outside the convention. A run's dir
   mtime advances while it writes, so an in-progress run is not eligible until it has been idle for
   the full window.

For hands-off hygiene, wire the sweep to a timer the same way the weekly `docker system prune` timer
is set up (tech-debts task #9); a daily `sweep-test-tmp.sh` is enough at current volume.
