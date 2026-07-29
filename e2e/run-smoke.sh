#!/bin/sh
set -eu

# Browser core-flow smoke runner (task #14). Brings up the real compose backend + Postgres, builds the
# PRODUCTION frontend pointed at that backend, serves it via vite preview, and runs the Playwright smoke.
# Fail-loud: if any piece of the stack doesn't come up, the run errors — never a silent skip.
#
#   e2e/run-smoke.sh                 # bring up, run, tear down
#   NOTTY_E2E_KEEP=1 e2e/run-smoke.sh  # leave the stack up for debugging

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
PROJECT="${NOTTY_E2E_PROJECT:-notty-e2e-$$}"
# A kept stack is deliberately long-lived, so it carries its intent in its own name: everything
# without the -keep suffix is machine-owned and safe for a future sweep to treat as abandoned.
if [ "${NOTTY_E2E_KEEP:-0}" = "1" ]; then PROJECT="$PROJECT-keep"; fi
# The regression compose is self-contained (inline JWT secret + fake Mailgun, no secrets file) — the
# stack the (c) seeding pattern targets; the main compose needs deploy secrets we don't want here.
COMPOSE_FILE="$ROOT_DIR/test/regression/docker-compose.yml"
PREVIEW_PORT="${NOTTY_E2E_PREVIEW_PORT:-4173}"
DC="docker compose -p $PROJECT -f $COMPOSE_FILE"

# Announced up front, not at teardown: a run that is killed never reaches its trap, and the whole
# point is that you know which project you now own.
if [ "${NOTTY_E2E_KEEP:-0}" = "1" ]; then
  echo "e2e: NOTTY_E2E_KEEP=1 — you own teardown of $PROJECT"
  echo "e2e: clean up with: docker compose -p $PROJECT -f $COMPOSE_FILE down -v --remove-orphans"
fi

cleanup() {
  status=$?
  trap - EXIT INT TERM
  [ -n "${PREVIEW_PID:-}" ] && kill "$PREVIEW_PID" >/dev/null 2>&1 || true
  if [ "${NOTTY_E2E_KEEP:-0}" != "1" ]; then
    # A green suite that leaked its stack is not a green run — the cost lands on the next person to
    # use this box. Report what docker said and fail, rather than swallowing it into a silent leak.
    if ! down_output="$($DC down -v --remove-orphans 2>&1)"; then
      printf 'e2e: TEARDOWN FAILED for project %s — the stack may still be running\n' "$PROJECT" >&2
      printf '%s\n' "$down_output" >&2
      printf 'e2e: clean up with: docker compose -p %s -f %s down -v --remove-orphans\n' \
        "$PROJECT" "$COMPOSE_FILE" >&2
      if [ "$status" -eq 0 ]; then status=1; fi
    fi
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

echo "e2e: bringing up backend + postgres…"
$DC up -d --build postgres backend

# Discover the backend's mapped port and wait for it (readiness, not retry).
BACKEND_PORT="$($DC port backend 8080 | awk -F: '{print $NF}' | tr -d '\r')"
[ -n "$BACKEND_PORT" ] || { echo "e2e: backend port never mapped" >&2; exit 1; }
BACKEND_URL="http://127.0.0.1:$BACKEND_PORT"
echo "e2e: backend at $BACKEND_URL"

echo "e2e: building production frontend (VITE_API_BASE=$BACKEND_URL)…"
( cd "$ROOT_DIR/frontend" && VITE_API_BASE="$BACKEND_URL" VITE_PUBLIC_ORIGIN="http://127.0.0.1:$PREVIEW_PORT" npm run build )
( cd "$ROOT_DIR/frontend" && npx vite preview --port "$PREVIEW_PORT" --strictPort --host 127.0.0.1 >/tmp/e2e-preview.log 2>&1 & echo $! > /tmp/e2e-preview.pid )
PREVIEW_PID="$(cat /tmp/e2e-preview.pid)"

export NOTTY_E2E_BACKEND_URL="$BACKEND_URL"
export NOTTY_E2E_PREVIEW_URL="http://127.0.0.1:$PREVIEW_PORT"
export NOTTY_E2E_PREVIEW_PORT="$PREVIEW_PORT"
export NOTTY_E2E_COMPOSE_PROJECT="$PROJECT"
export NOTTY_E2E_COMPOSE_FILE="$COMPOSE_FILE"

echo "e2e: running Playwright smoke…"
( cd "$ROOT_DIR/e2e" && npx playwright test )
