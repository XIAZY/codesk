#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
JANITOR="$ROOT_DIR/scripts/cleanup-stale-regression-stacks.sh"
COMPOSE_FILE="$ROOT_DIR/test/regression/docker-compose.yml"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

mkdir -p "$TMP_DIR/bin" "$TMP_DIR/state"
now_epoch="$(date +%s)"
export FAKE_OLD_CREATED="$(date -u -d "@$((now_epoch - 10800))" +%Y-%m-%dT%H:%M:%SZ)"
export FAKE_OLD_NEWEST_CREATED="$(date -u -d "@$((now_epoch - 600))" +%Y-%m-%dT%H:%M:%SZ)"
export FAKE_STOPPED_CREATED="$(date -u -d "@$((now_epoch - 14400))" +%Y-%m-%dT%H:%M:%SZ)"
export FAKE_RECENT_CREATED="$(date -u -d "@$((now_epoch - 300))" +%Y-%m-%dT%H:%M:%SZ)"
export FAKE_FAILURE_CREATED="$(date -u -d "@$((now_epoch - 18000))" +%Y-%m-%dT%H:%M:%SZ)"
export FAKE_FOREIGN_CREATED="$FAKE_OLD_CREATED"
export FAKE_DOCKER_LOG="$TMP_DIR/docker.log"
export FAKE_DOCKER_STATE="$TMP_DIR/state"

touch \
  "$FAKE_DOCKER_STATE/volume-notty_regression_oldest_mix" \
  "$FAKE_DOCKER_STATE/volume-notty_regression_stopped" \
  "$FAKE_DOCKER_STATE/volume-notty_regression_recent" \
  "$FAKE_DOCKER_STATE/volume-notty_regression_cleanup_failure" \
  "$FAKE_DOCKER_STATE/volume-unrelated_project"

cat > "$TMP_DIR/bin/docker" <<'FAKEDOCKER'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -eq 4 && "$1" == ps && "$2" == -aq && "$3" == --filter && "$4" == label=com.docker.compose.project ]]; then
  printf '%s\n' old-a old-b stopped-a recent-a failure-a foreign-a
  exit 0
fi

if [[ "$#" -ge 4 && "$1" == inspect && "$2" == --format ]]; then
  printf '%s %s\n' \
    "$FAKE_OLD_CREATED" notty_regression_oldest_mix \
    "$FAKE_OLD_NEWEST_CREATED" notty_regression_oldest_mix \
    "$FAKE_STOPPED_CREATED" notty_regression_stopped \
    "$FAKE_RECENT_CREATED" notty_regression_recent \
    "$FAKE_FAILURE_CREATED" notty_regression_cleanup_failure \
    "$FAKE_FOREIGN_CREATED" unrelated_project
  exit 0
fi

if [[ "$#" -eq 8 && "$1" == compose && "$2" == -p && "$4" == -f && "$6" == down && "$7" == -v && "$8" == --remove-orphans ]]; then
  project="$3"
  printf '%s|%s|%s|%s|%s\n' "$project" "$5" "$6" "$7" "$8" >> "$FAKE_DOCKER_LOG"
  if [[ "$project" == notty_regression_cleanup_failure ]]; then
    echo "forced cleanup failure" >&2
    exit 17
  fi
  rm -f "$FAKE_DOCKER_STATE/volume-$project"
  exit 0
fi

printf 'unexpected docker invocation:' >&2
printf ' %q' "$@" >&2
printf '\n' >&2
exit 64
FAKEDOCKER
chmod +x "$TMP_DIR/bin/docker"

output="$(PATH="$TMP_DIR/bin:$PATH" "$JANITOR" 2>&1)"
printf '%s\n' "$output"

test ! -e "$FAKE_DOCKER_STATE/volume-notty_regression_oldest_mix"
test ! -e "$FAKE_DOCKER_STATE/volume-notty_regression_stopped"
test -e "$FAKE_DOCKER_STATE/volume-notty_regression_recent"
test -e "$FAKE_DOCKER_STATE/volume-notty_regression_cleanup_failure"
test -e "$FAKE_DOCKER_STATE/volume-unrelated_project"

grep -F "notty_regression_oldest_mix|$COMPOSE_FILE|down|-v|--remove-orphans" "$FAKE_DOCKER_LOG" >/dev/null
grep -F "notty_regression_stopped|$COMPOSE_FILE|down|-v|--remove-orphans" "$FAKE_DOCKER_LOG" >/dev/null
grep -F "notty_regression_cleanup_failure|$COMPOSE_FILE|down|-v|--remove-orphans" "$FAKE_DOCKER_LOG" >/dev/null
if grep -Fq 'notty_regression_recent|' "$FAKE_DOCKER_LOG"; then
  echo "recent regression project was removed" >&2
  exit 1
fi
if grep -Fq 'unrelated_project|' "$FAKE_DOCKER_LOG"; then
  echo "foreign compose project was removed" >&2
  exit 1
fi

grep -F 'Removed stale regression project notty_regression_oldest_mix (age=' <<< "$output" >/dev/null
grep -F 'Removed stale regression project notty_regression_stopped (age=' <<< "$output" >/dev/null
grep -F 'Failed to remove stale regression project notty_regression_cleanup_failure (age=' <<< "$output" >/dev/null
grep -F '; continuing' <<< "$output" >/dev/null

echo "Regression stack janitor contract passed."
