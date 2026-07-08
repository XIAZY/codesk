import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

// Seeds the compose stack via the real API before the browser opens, per Tom's (c) ruling: the stack runs
// fake Mailgun (no inbox), so a freshly-registered account is marked verified directly in Postgres — the
// only DB touch in the whole smoke; every flow assertion afterward goes through the browser against real
// endpoints. Writes the seed (credentials + slugs + doc id) to seed.json for the test to consume.
//
// Env (set by run-smoke.sh / the CI e2e job):
//   NOTTY_E2E_BACKEND_URL      backend base (dynamic compose port)
//   NOTTY_E2E_PREVIEW_URL      vite-preview base (readiness-checked)
//   NOTTY_E2E_COMPOSE_PROJECT  docker compose -p project (for the psql exec)
//   NOTTY_E2E_COMPOSE_FILE     docker compose -f file

const BACKEND = required("NOTTY_E2E_BACKEND_URL");
const PREVIEW = required("NOTTY_E2E_PREVIEW_URL");
const COMPOSE_PROJECT = required("NOTTY_E2E_COMPOSE_PROJECT");
const COMPOSE_FILE = required("NOTTY_E2E_COMPOSE_FILE");

function required(name: string): string {
  const v = process.env[name];
  if (!v) throw new Error(`missing required env ${name}`);
  return v;
}

async function waitFor(url: string, label: string, timeoutMs = 90_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastErr = "";
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok || res.status === 401 || res.status === 404) return; // reachable = ready
      lastErr = `status ${res.status}`;
    } catch (e) {
      lastErr = String(e);
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  // Readiness is a wait, never a retry: fail loud if the stack never came up.
  throw new Error(`${label} did not become ready at ${url} within ${timeoutMs}ms (last: ${lastErr})`);
}

async function api(path: string, init: RequestInit & { token?: string } = {}): Promise<any> {
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (init.token) headers.authorization = `Bearer ${init.token}`;
  const res = await fetch(`${BACKEND}${path}`, { ...init, headers: { ...headers, ...(init.headers as any) } });
  const body = await res.text();
  if (!res.ok) throw new Error(`${init.method ?? "GET"} ${path} -> ${res.status}: ${body}`);
  return body ? JSON.parse(body) : {};
}

function markVerified(email: string): void {
  // Exact pattern from test/regression's bootstrapWorkspace: verify in Postgres because fake Mailgun has
  // no inbox to read a token from. DB touch confined to this seed step.
  const sql = `UPDATE accounts SET email_verified = TRUE WHERE email = '${email.replace(/'/g, "''")}'`;
  execFileSync(
    "docker",
    ["compose", "-p", COMPOSE_PROJECT, "-f", COMPOSE_FILE, "exec", "-T", "postgres",
     "psql", "-U", "notty", "-d", "notty", "-v", "ON_ERROR_STOP=1", "-c", sql],
    { stdio: "pipe" },
  );
}

export default async function globalSetup(): Promise<void> {
  await waitFor(`${BACKEND}/healthz`, "backend");
  await waitFor(PREVIEW, "frontend preview");

  const stamp = Date.now();
  const email = `e2e-${stamp}@example.invalid`;
  const password = "smoke-pass-12345";

  await api("/api/auth/register", { method: "POST", body: JSON.stringify({ email, password, name: "Smoke Owner" }) });
  markVerified(email);
  const login = await api("/api/auth/login", { method: "POST", body: JSON.stringify({ email, password }) });
  const token: string = login.token;
  if (!token) throw new Error("login returned no token after verification");

  // Workspace A with one document (its content is written through the editor in the flow, so opening it is
  // a real product action); workspace B left FULLY IDLE — no daemons, agents, threads, presence — so the
  // A->B switch reproduces the white-screen incident shape by construction.
  const a = await api("/api/workspaces", { method: "POST", token, body: JSON.stringify({ name: "Smoke Alpha" }) });
  const workspaceA = a.workspace ?? a;
  const doc = await api(`/api/workspaces/${workspaceA.id}/documents`, { method: "POST", token, body: JSON.stringify({}) });
  const b = await api("/api/workspaces", { method: "POST", token, body: JSON.stringify({ name: "Smoke Bravo" }) });
  const workspaceB = b.workspace ?? b;

  const seed = {
    email, password,
    slugA: workspaceA.slug, nameA: workspaceA.name, documentId: doc.id ?? doc.documentId,
    slugB: workspaceB.slug, nameB: workspaceB.name,
  };
  writeFileSync(join(__dirname, "seed.json"), JSON.stringify(seed, null, 2));
}
