import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, type Page } from "@playwright/test";

type Credentials = {
  email: string;
  password: string;
};

type OnboardingSeed = {
  onboarding: {
    brandNew: Credentials;
    invitedMember: Credentials & {
      handle: string;
      invitePath: string;
      workspaceId: string;
      workspaceSlug: string;
      workspaceName: string;
    };
    returningUser: Credentials & {
      workspaceId: string;
      workspaceSlug: string;
      workspaceName: string;
    };
  };
};

const ONBOARDING_IDS = [
  "create-document",
  "connect-local-env",
  "new-document",
  "new-agent",
  "document-threads",
  "document-watchers",
  "document-more",
  "selection-thread",
] as const;

function loadSeed(): OnboardingSeed {
  return JSON.parse(readFileSync(join(__dirname, "..", "seed.json"), "utf8")) as OnboardingSeed;
}

function failOnPageError(page: Page, errors: Error[]): void {
  page.on("pageerror", (error) => errors.push(error));
}

function expectNoPageErrors(errors: Error[]): void {
  expect(errors, `uncaught page errors:\n${errors.map((error) => error.stack ?? error.message).join("\n")}`).toEqual([]);
}

async function login(page: Page, account: Credentials): Promise<void> {
  await page.goto("/");
  await page.locator('input[type="email"]').first().fill(account.email);
  await page.locator('input[type="password"]').first().fill(account.password);
  await page.getByRole("button", { name: /^log in$/i }).click();
}

function onboardingAnchor(page: Page, id: typeof ONBOARDING_IDS[number]) {
  return page.locator(`[data-onboarding-id="${id}"]`);
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`missing required env ${name}`);
  return value;
}

// Exact compose activation seam used by the regression stack. The UI creates the
// daemon record/token; this helper starts the real daemon process so completion is
// driven by receipt-elapsed liveness, never a fabricated online row.
function startComposeDaemon(workspaceId: string, daemonToken: string): void {
  const project = requiredEnv("NOTTY_E2E_COMPOSE_PROJECT");
  const composeFile = requiredEnv("NOTTY_E2E_COMPOSE_FILE");
  execFileSync(
    "docker",
    ["compose", "-p", project, "-f", composeFile, "up", "-d", "--build", "daemon"],
    {
      env: {
        ...process.env,
        NOTTY_WORKSPACE_ID: workspaceId,
        NOTTY_DAEMON_TOKEN: daemonToken,
      },
      stdio: "pipe",
    },
  );
}

test.describe("onboarding Batch 1 real-stack scenarios", () => {
  test("A: brand-new account completes the guided path and resumes honestly", async ({ page }) => {
    test.fixme(true, "activate after P2/P5 integration wiring supplies the final selectors and event seams");
    const seed = loadSeed();
    const errors: Error[] = [];
    failOnPageError(page, errors);

    await login(page, seed.onboarding.brandNew);

    // Wired-head assertions/actions:
    // 1. Create a workspace through the full-page zero-workspace entry.
    // 2. Prove the three-step spotlight targets create-document, document-threads,
    //    and document-watchers exactly once while leaving each target operable.
    // 3. Create/type/thread through the real UI. Batch 1 deliberately records no
    //    first-edit flag; document/thread progress derives from live state.
    // 4. Create a local environment through the real UI, pass its returned token to
    //    startComposeDaemon(), and wait for receipt-live completion.
    // 5. Reload mid-flow and prove resume/dismiss state without localStorage-derived
    //    false completion.
    void onboardingAnchor;
    void startComposeDaemon;
    expectNoPageErrors(errors);
  });

  test("B: invited member enters existing state without owner-only or dishonest create-first steps", async ({ page }) => {
    test.fixme(true, "activate after P2/P5 integration wiring supplies the final selectors and event seams");
    const seed = loadSeed();
    const errors: Error[] = [];
    failOnPageError(page, errors);

    await login(page, seed.onboarding.invitedMember);

    // Wired-head assertions/actions:
    // 1. Navigate to the real invitePath and accept with the seeded member handle.
    // 2. Assert role-ineligible invite-team work is removed, not disabled.
    // 3. Assert existing live documents/agents/subscribers satisfy derived state and
    //    never produce owner-only or create-first prompts.
    // 4. Exercise missing-target fallback without a bubble on empty space.
    expectNoPageErrors(errors);
  });

  test("E: returning user sees no welcome and only genuinely incomplete work resumes", async ({ page }) => {
    test.fixme(true, "activate after P2/P5 integration wiring supplies the final selectors and event seams");
    const seed = loadSeed();
    const errors: Error[] = [];
    failOnPageError(page, errors);

    // The wired test installs only versioned seen/dismissed flags before login.
    // Live completion remains derived from the returning workspace payload.
    await login(page, seed.onboarding.returningUser);

    // Wired-head assertions/actions:
    // 1. No zero-workspace welcome/full-page intro.
    // 2. No Batch-2 feature hint.
    // 3. Resume only an eligible incomplete Batch-1 item after reload and workspace
    //    switching; workspace-A flags must not suppress workspace B.
    expectNoPageErrors(errors);
  });
});
