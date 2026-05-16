#!/bin/sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/test/regression/docker-compose.yml"
PROJECT="${NOTTY_LIVE_TEST_PROJECT:-notty-live-test-$$}"
KEEP="${NOTTY_LIVE_TEST_KEEP:-0}"

cleanup() {
  if [ "$KEEP" != "1" ]; then
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up -d --build postgres backend >/dev/null

PORT="$(docker compose -p "$PROJECT" -f "$COMPOSE_FILE" port backend 8080 | awk -F: '{print $NF}' | tr -d '\r')"
if [ -z "$PORT" ]; then
  echo "could not resolve live test backend port" >&2
  exit 1
fi
BASE_URL="http://127.0.0.1:${PORT}"

ready=0
attempt=0
while [ "$attempt" -lt 90 ]; do
  if node -e "fetch(process.argv[1] + '/healthz').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))" "$BASE_URL" >/dev/null 2>&1; then
    ready=1
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done

if [ "$ready" != "1" ]; then
  echo "live test backend did not become ready" >&2
  exit 1
fi

node - "$BASE_URL" <<'NODE'
const baseUrl = process.argv[2];
const suffix = `${Date.now()}-${Math.random().toString(16).slice(2)}`;

async function request(path, options = {}) {
  const response = await fetch(`${baseUrl}${path}`, options);
  const text = await response.text();
  let body = null;
  if (text) {
    body = JSON.parse(text);
  }
  if (!response.ok) {
    throw new Error(`${options.method ?? 'GET'} ${path} failed ${response.status}: ${text}`);
  }
  return body;
}

function authHeaders(token) {
  return {
    authorization: `Bearer ${token}`,
    'content-type': 'application/json'
  };
}

const auth = await request('/api/auth/register', {
  method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify({
    email: `live-${suffix}@example.test`,
    password: 'live-test-password',
    displayName: 'Live Test User'
  })
});
if (!auth.token) {
  throw new Error('register did not return a token');
}

const workspaceResponse = await request('/api/workspaces', {
  method: 'POST',
  headers: authHeaders(auth.token),
  body: JSON.stringify({ name: 'Live Smoke Workspace' })
});
const workspaceId = workspaceResponse.workspace?.id;
if (!workspaceId) {
  throw new Error(`workspace create response did not include id: ${JSON.stringify(workspaceResponse)}`);
}

const document = await request(`/api/workspaces/${workspaceId}/documents`, {
  method: 'POST',
  headers: authHeaders(auth.token),
  body: JSON.stringify({
    path: 'docs/live-smoke.md',
    content: '# Live smoke\n'
  })
});
if (!document.id) {
  throw new Error(`document create response did not include id: ${JSON.stringify(document)}`);
}

const workspace = await request(`/api/workspaces/${workspaceId}/workspace`, {
  headers: { authorization: `Bearer ${auth.token}` }
});
if (!Array.isArray(workspace.documents) || !workspace.documents.some((item) => item.id === document.id)) {
  throw new Error('workspace snapshot did not include created document');
}

console.log(`live smoke passed: ${workspaceId}`);
NODE
