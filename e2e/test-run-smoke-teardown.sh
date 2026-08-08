#!/usr/bin/env bash
# Contract for run-smoke.sh's teardown, the leak the four abandoned notty-e2e-* stacks came from.
# The property under test is that a failed teardown CANNOT be silent: a suite that passes but
# leaves its stack running must fail the run, because the cost lands on the next user of the box.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

mkdir -p "$TMP_DIR/e2e" "$TMP_DIR/frontend" "$TMP_DIR/test/regression" "$TMP_DIR/bin"
cp "$ROOT_DIR/e2e/run-smoke.sh" "$TMP_DIR/e2e/run-smoke.sh"
touch "$TMP_DIR/test/regression/docker-compose.yml"

cat > "$TMP_DIR/bin/docker" <<'FAKEDOCKER'
#!/bin/sh
case "$*" in
  *"up -d"*) exit 0 ;;
  *"port backend"*) echo "0.0.0.0:5432"; exit 0 ;;
  *"ps -a"*)
    echo "backend Exited (1)"
    [ "${FAKE_DIAGNOSTICS_FAIL:-0}" = "1" ] && exit 1
    exit 0 ;;
  *"logs --no-color --tail=500"*)
    echo "backend | fatal startup"
    [ "${FAKE_DIAGNOSTICS_FAIL:-0}" = "1" ] && exit 1
    exit 0 ;;
  *down*)
    echo "$*" >> "$FAKE_DOWN_LOG"
    if [ "${FAKE_DOWN_FAIL:-0}" = "1" ]; then
      echo "Error: network has active endpoints" >&2
      exit 1
    fi
    exit 0 ;;
esac
exit 0
FAKEDOCKER
printf '#!/bin/sh\nexit 0\n' > "$TMP_DIR/bin/npm"
cat > "$TMP_DIR/bin/npx" <<'FAKENPX'
#!/bin/sh
if [ "${FAKE_SUITE_FAIL:-0}" = "1" ] && [ "$1" = "playwright" ]; then exit 3; fi
exit 0
FAKENPX
chmod +x "$TMP_DIR/bin/docker" "$TMP_DIR/bin/npm" "$TMP_DIR/bin/npx"

run_case() {
  FAKE_DOWN_LOG="$TMP_DIR/down.log"; : > "$FAKE_DOWN_LOG"
  export FAKE_DOWN_LOG
  set +e
  CASE_OUT="$(env PATH="$TMP_DIR/bin:$PATH" "$@" sh "$TMP_DIR/e2e/run-smoke.sh" 2>&1)"
  CASE_RC=$?
  set -e
}

fail() { echo "run-smoke teardown contract: $1" >&2; echo "$CASE_OUT" >&2; exit 1; }

# A green suite whose teardown succeeds is a clean, quiet pass.
run_case FAKE_DOWN_FAIL=0
[ "$CASE_RC" -eq 0 ] || fail "green suite + good teardown should exit 0, got $CASE_RC"
grep -q "TEARDOWN FAILED" <<< "$CASE_OUT" && fail "clean run must not warn"

# The core property: a green suite that leaks its stack is NOT a green run.
run_case FAKE_DOWN_FAIL=1
[ "$CASE_RC" -ne 0 ] || fail "green suite + failed teardown must fail the run, got 0"
grep -q "TEARDOWN FAILED" <<< "$CASE_OUT" || fail "failed teardown must say so"
grep -q "network has active endpoints" <<< "$CASE_OUT" || fail "docker's own error must be shown"

# A real suite failure must not be masked or renumbered by teardown handling.
run_case FAKE_SUITE_FAIL=1 FAKE_DOWN_FAIL=0
[ "$CASE_RC" -eq 3 ] || fail "suite failure must propagate unchanged, got $CASE_RC"
grep -q "FAILURE DIAGNOSTICS" <<< "$CASE_OUT" || fail "suite failure must emit diagnostics"
grep -q "backend Exited (1)" <<< "$CASE_OUT" || fail "suite failure must emit compose status"
grep -q "backend | fatal startup" <<< "$CASE_OUT" || fail "suite failure must emit compose logs"

run_case FAKE_SUITE_FAIL=1 FAKE_DOWN_FAIL=1
[ "$CASE_RC" -eq 3 ] || fail "suite failure must survive a failed teardown, got $CASE_RC"
grep -q "TEARDOWN FAILED" <<< "$CASE_OUT" || fail "teardown failure must still be reported"

# Diagnostic collection is evidence only: it must never mask or renumber the primary failure.
run_case FAKE_SUITE_FAIL=1 FAKE_DIAGNOSTICS_FAIL=1
[ "$CASE_RC" -eq 3 ] || fail "diagnostic failure must preserve suite exit 3, got $CASE_RC"
grep -q "docker compose ps failed" <<< "$CASE_OUT" || fail "failed status collection must be explicit"
grep -q "docker compose logs failed" <<< "$CASE_OUT" || fail "failed log collection must be explicit"

# KEEP is a deliberate long-lived stack: named for it, announced, and never torn down.
run_case NOTTY_E2E_KEEP=1 FAKE_DOWN_FAIL=1
[ "$CASE_RC" -eq 0 ] || fail "KEEP run should exit 0, got $CASE_RC"
grep -qE "you own teardown of notty-e2e-[0-9]+-keep" <<< "$CASE_OUT" || fail "KEEP must name the project it hands over"
[ -s "$TMP_DIR/down.log" ] && fail "KEEP must not tear the stack down"

echo "run-smoke teardown contract passed."
