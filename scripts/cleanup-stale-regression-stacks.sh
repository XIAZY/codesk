#!/usr/bin/env bash
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/test/regression/docker-compose.yml"
# This is roughly five times the longest observed legitimate regression run.
STALE_AFTER_SECONDS=7200

if ! now_epoch="$(date +%s)"; then
  echo "::warning::Regression stack janitor could not read the current time; continuing without cleanup"
  exit 0
fi
if ! container_ids="$(docker ps -aq --filter 'label=com.docker.compose.project')"; then
  echo "::warning::Regression stack janitor could not list compose containers; continuing without cleanup"
  exit 0
fi
if [[ -z "$container_ids" ]]; then
  exit 0
fi
mapfile -t container_id_list <<< "$container_ids"
if ! container_rows="$(docker inspect --format '{{.Created}} {{index .Config.Labels "com.docker.compose.project"}}' "${container_id_list[@]}")"; then
  echo "::warning::Regression stack janitor could not inspect compose containers; continuing without cleanup"
  exit 0
fi

declare -A oldest_epoch=()
declare -A invalid_project=()
while read -r created project extra; do
  [[ "$project" == notty_regression_* ]] || continue
  if [[ -n "${extra:-}" ]] || ! created_epoch="$(date -d "$created" +%s 2>/dev/null)"; then
    echo "::warning::Regression stack janitor could not parse creation time for $project: $created"
    invalid_project["$project"]=1
    continue
  fi
  if [[ -z "${oldest_epoch[$project]+set}" ]] || (( created_epoch < oldest_epoch[$project] )); then
    oldest_epoch["$project"]="$created_epoch"
  fi
done <<< "$container_rows"

for project in "${!oldest_epoch[@]}"; do
  [[ -z "${invalid_project[$project]+set}" ]] || continue
  age_seconds=$((now_epoch - oldest_epoch[$project]))
  (( age_seconds > STALE_AFTER_SECONDS )) || continue
  age_minutes=$((age_seconds / 60))
  if docker compose -p "$project" -f "$COMPOSE_FILE" down -v --remove-orphans; then
    echo "Removed stale regression project $project (age=${age_seconds}s/${age_minutes}m)"
  else
    echo "::warning::Failed to remove stale regression project $project (age=${age_seconds}s/${age_minutes}m); continuing"
  fi
done

exit 0
