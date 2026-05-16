#!/bin/sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/test/postgres/docker-compose.yml"
PROJECT="${NOTTY_POSTGRES_TEST_PROJECT:-notty-postgres-test-$$}"
KEEP="${NOTTY_POSTGRES_TEST_KEEP:-0}"

cleanup() {
  if [ "$KEEP" != "1" ]; then
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up -d postgres >/dev/null

ready=0
attempt=0
while [ "$attempt" -lt 60 ]; do
  if docker compose -p "$PROJECT" -f "$COMPOSE_FILE" exec -T postgres pg_isready -U notty -d notty_test >/dev/null 2>&1; then
    ready=1
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done

if [ "$ready" != "1" ]; then
  echo "postgres test database did not become ready" >&2
  exit 1
fi

PORT="$(docker compose -p "$PROJECT" -f "$COMPOSE_FILE" port postgres 5432 | awk -F: '{print $NF}' | tr -d '\r')"
if [ -z "$PORT" ]; then
  echo "could not resolve postgres test database port" >&2
  exit 1
fi

export NOTTY_DATABASE_TEST_URL="postgres://notty:notty@127.0.0.1:${PORT}/notty_test?sslmode=disable"
export NOTTY_DATABASE_TEST_ISOLATED=1

cd "$ROOT_DIR"
go test ./backend/internal/notty "$@"
