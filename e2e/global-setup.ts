import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

// ARCHITECTURAL CONSTRAINT for E2E authors: document content has NO REST seed path. A document's PATH lives
// in the workspace root-namespace CRDT and its TEXT in the per-document CRDT — POST /documents only allocates
// an empty pathless stream, so an API-created doc is unlistable and unopenable (the UI shows "Document not
// found", which reads like a harness bug but is the architecture). Faithful seeding is UI-driven (the "New
// document" action) or via a Yjs WS client — never REST. This is why the flow creates its doc through the UI.
//
// Seeds the compose stack via the real API before the browser opens, per Tom's (c) ruling: the stack runs
// fake Mailgun (no inbox), so a freshly-registered account is marked verified directly in Postgres — the
// only DB touch in the whole smoke; every flow assertion afterward goes through the browser against real
// endpoints. Writes the seed (credentials + workspace slugs/names) to seed.json for the test to consume.
//
// Env (set by run-smoke.sh / the CI e2e job):
//   NOTTY_E2E_BACKEND_URL      backend base (dynamic compose port)
//   NOTTY_E2E_PREVIEW_URL      vite-preview base (readiness-checked)
//   NOTTY_E2E_COMPOSE_PROJECT  docker compose -p project (for the psql exec)
//   NOTTY_E2E_COMPOSE_FILE     docker compose -f file

const BACKEND = required("NOTTY_E2E_BACKEND_URL");
const PREVIEW = required("NOTTY_E2E_PREVIEW_URL");
// Two mark-verified transports. CI/compose path resolves the psql via `docker compose exec`
// (COMPOSE_PROJECT + COMPOSE_FILE). Native-local path (fast selector iteration against a native
// backend + a standalone postgres container) sets NOTTY_E2E_PG_CONTAINER and runs `docker exec`
// directly. Exactly one must be configured; the container form wins when both are present.
const PG_CONTAINER = process.env.NOTTY_E2E_PG_CONTAINER || "";
const COMPOSE_PROJECT = PG_CONTAINER ? "" : required("NOTTY_E2E_COMPOSE_PROJECT");
const COMPOSE_FILE = PG_CONTAINER ? "" : required("NOTTY_E2E_COMPOSE_FILE");

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
  const psql = ["psql", "-U", "notty", "-d", "notty", "-v", "ON_ERROR_STOP=1", "-c", sql];
  const args = PG_CONTAINER
    ? ["exec", "-i", PG_CONTAINER, ...psql] // plain `docker exec` uses -i, not compose's -T
    : ["compose", "-p", COMPOSE_PROJECT, "-f", COMPOSE_FILE, "exec", "-T", "postgres", ...psql];
  execFileSync("docker", args, { stdio: "pipe" });
}

type VerifiedAccount = {
  email: string;
  password: string;
  token: string;
};

async function registerVerifiedAccount(stamp: number, slug: string, name: string): Promise<VerifiedAccount> {
  const email = `e2e-${slug}-${stamp}@example.invalid`;
  const password = "smoke-pass-12345";
  await api("/api/auth/register", { method: "POST", body: JSON.stringify({ email, password, name }) });
  markVerified(email);
  const login = await api("/api/auth/login", { method: "POST", body: JSON.stringify({ email, password }) });
  if (!login.token) throw new Error(`login returned no token for ${slug} after verification`);
  return { email, password, token: login.token };
}

async function createWorkspace(token: string, stamp: number, slug: string, name: string): Promise<any> {
  const response = await api("/api/workspaces", {
    method: "POST",
    token,
    body: JSON.stringify({ name, slug: `${slug}-${stamp}`, handle: `ob-${slug.slice(0, 10)}-${stamp}` }),
  });
  return response.workspace ?? response;
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
  // Workspace A is where the flow creates a document through the UI (its path lives in A's root-namespace
  // CRDT); workspace B is left FULLY IDLE — no daemons, agents, threads, presence — so the A->B switch
  // reproduces the white-screen incident shape by construction.
  const a = await api("/api/workspaces", { method: "POST", token,
    body: JSON.stringify({ name: "Smoke Alpha", slug: `smoke-alpha-${stamp}`, handle: `smoke-owner-a-${stamp}` }) });
  const workspaceA = a.workspace ?? a;
  const b = await api("/api/workspaces", { method: "POST", token,
    body: JSON.stringify({ name: "Smoke Bravo", slug: `smoke-bravo-${stamp}`, handle: `smoke-owner-b-${stamp}` }) });
  const workspaceB = b.workspace ?? b;

  // Onboarding scenarios use dedicated identities so completion and localStorage state
  // never leak from the long-lived core smoke owner:
  //   A: verified account with zero workspaces;
  //   B: verified account with zero workspaces plus a real invite to an existing workspace;
  //   E: verified returning account with an existing workspace.
  // Phase-2 assertions are deliberately activated only after the integration wiring lands.
  const onboardingNew = await registerVerifiedAccount(stamp, "onboarding-new", "Onboarding New User");

  const inviteOwner = await registerVerifiedAccount(stamp, "onboarding-invite-owner", "Onboarding Invite Owner");
  const invitedWorkspace = await createWorkspace(inviteOwner.token, stamp, "onboarding-invited", "Onboarding Invited Workspace");
  const invite = await api(`/api/workspaces/${encodeURIComponent(invitedWorkspace.id)}/invites`, {
    method: "POST",
    token: inviteOwner.token,
  });
  if (!invite.url) throw new Error("onboarding invite creation returned no URL");
  const onboardingInvited = await registerVerifiedAccount(stamp, "onboarding-invitee", "Onboarding Invited Member");

  const onboardingReturning = await registerVerifiedAccount(stamp, "onboarding-returning", "Onboarding Returning User");
  const returningWorkspace = await createWorkspace(
    onboardingReturning.token,
    stamp,
    "onboarding-returning",
    "Onboarding Returning Workspace",
  );

  const seed = {
    email, password,
    slugA: workspaceA.slug, nameA: workspaceA.name,
    slugB: workspaceB.slug, nameB: workspaceB.name,
    onboarding: {
      brandNew: {
        email: onboardingNew.email,
        password: onboardingNew.password,
      },
      invitedMember: {
        email: onboardingInvited.email,
        password: onboardingInvited.password,
        handle: `ob-invitee-${stamp}`,
        invitePath: invite.url,
        workspaceId: invitedWorkspace.id,
        workspaceSlug: invitedWorkspace.slug,
        workspaceName: invitedWorkspace.name,
      },
      returningUser: {
        email: onboardingReturning.email,
        password: onboardingReturning.password,
        workspaceId: returningWorkspace.id,
        workspaceSlug: returningWorkspace.slug,
        workspaceName: returningWorkspace.name,
      },
    },
  };
  writeFileSync(join(__dirname, "seed.json"), JSON.stringify(seed, null, 2));
}
