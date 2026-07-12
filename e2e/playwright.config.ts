import { defineConfig, devices } from "@playwright/test";
import { join } from "node:path";

// Browser core-flow smoke (task #14). Drives the PRODUCTION frontend build (vite preview — never the dev
// server; the smoke must test what deploys) against the real docker-compose backend + Postgres.
//
// Orchestration lives in run-smoke.sh / the CI e2e job: it brings the compose backend up, builds the
// frontend with VITE_API_BASE pointed at that backend (VITE_ vars bake at build time), starts the preview,
// and exports the two URLs below. global-setup then waits for readiness and seeds via the API.
const PREVIEW_URL = process.env.NOTTY_E2E_PREVIEW_URL ?? "http://127.0.0.1:4173";

export default defineConfig({
  testDir: "./tests",
  // retries=0 by ruling: three assertions against a local stack are deterministic or broken — a failure is
  // a failure. Nondeterminism is answered by readiness waits in global-setup, NEVER a test retry.
  retries: 0,
  fullyParallel: false,
  workers: 1,
  timeout: 60_000,
  // The original 3-minute budget covered the 10-row core smoke. Batch-1 onboarding
  // additionally builds/starts the real daemon image before creating an agent, so a
  // cold runner needs a bounded six-minute envelope without retries.
  globalTimeout: 360_000,
  forbidOnly: !!process.env.CI,
  reporter: [["list"]],
  globalSetup: require.resolve("./global-setup"),
  use: {
    baseURL: PREVIEW_URL,
    // Written by global-setup after it knows the isolated smoke account/workspaces.
    // It suppresses onboarding only for the pre-existing non-onboarding smoke user;
    // A/B/E use different account-scoped keys and therefore remain pristine.
    storageState: join(__dirname, "storage-state.json"),
    trace: "retain-on-failure",
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
