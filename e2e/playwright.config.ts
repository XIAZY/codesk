import { defineConfig, devices } from "@playwright/test";

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
  globalTimeout: 180_000, // ≤3min total budget (Tom's spec).
  forbidOnly: !!process.env.CI,
  reporter: [["list"]],
  globalSetup: require.resolve("./global-setup"),
  use: {
    baseURL: PREVIEW_URL,
    trace: "retain-on-failure",
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
