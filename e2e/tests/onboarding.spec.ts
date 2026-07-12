import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, type Page, type TestInfo } from "@playwright/test";

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
    invitedOwner: Credentials;
    returningUser: Credentials & {
      accountId: string;
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

async function capture(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  await page.screenshot({ path: testInfo.outputPath(name), fullPage: true });
}

function coach(page: Page) {
  return page.locator(".ob-coach");
}

async function guideOneGeometry(page: Page): Promise<{
  candidateCrowdsTarget: boolean;
  coachTop: number;
  coachOverlapsTarget: boolean;
}> {
  const coachRect = await coach(page).boundingBox();
  const targetRect = await onboardingAnchor(page, "create-document").boundingBox();
  const viewport = page.viewportSize();
  if (!coachRect || !targetRect || !viewport) {
    throw new Error("guide 1 geometry is unavailable");
  }

  const candidateLeft = Math.min(
    Math.max(viewport.width - coachRect.width - 24, 12),
    Math.max(12, viewport.width - coachRect.width - 12),
  );
  const candidateTop = Math.min(
    Math.max(72, 12),
    Math.max(12, viewport.height - coachRect.height - 12),
  );
  const overlaps = (
    candidateLeft < targetRect.x + targetRect.width + 18
    && candidateLeft + coachRect.width > targetRect.x - 18
    && candidateTop < targetRect.y + targetRect.height + 18
    && candidateTop + coachRect.height > targetRect.y - 18
  );
  const coachOverlapsTarget = !(
    coachRect.x + coachRect.width <= targetRect.x
    || targetRect.x + targetRect.width <= coachRect.x
    || coachRect.y + coachRect.height <= targetRect.y
    || targetRect.y + targetRect.height <= coachRect.y
  );

  return {
    candidateCrowdsTarget: overlaps,
    coachTop: Math.round(coachRect.y),
    coachOverlapsTarget,
  };
}

async function expectCoach(page: Page, title: string, step?: string): Promise<void> {
  await expect(coach(page)).toBeVisible({ timeout: 20_000 });
  await expect(coach(page).getByRole("heading", { name: title })).toBeVisible();
  if (step) await expect(coach(page).locator(".ob-step")).toHaveText(step);
}

async function expectAnchor(page: Page, id: typeof ONBOARDING_IDS[number]): Promise<void> {
  await expect(onboardingAnchor(page, id), `${id} must resolve exactly once in its real rendered state`).toHaveCount(1);
  await expect(onboardingAnchor(page, id)).toBeVisible();
}

async function createDocumentFromEmptyWorkspace(page: Page): Promise<void> {
  await expectAnchor(page, "create-document");
  await onboardingAnchor(page, "create-document").click();
  await expect(page.locator(".cm-editor").first()).toBeVisible({ timeout: 20_000 });
}

async function selectEditorLine(page: Page): Promise<void> {
  const editor = page.locator(".cm-editor").first();
  await editor.click();
  await page.keyboard.press("Home");
  await page.keyboard.press("Shift+End");
  await expectAnchor(page, "selection-thread");
}

async function removeAnchorAndRemeasure(page: Page, id: typeof ONBOARDING_IDS[number]): Promise<void> {
  await page.evaluate((targetId) => {
    document.querySelector<HTMLElement>(`[data-onboarding-id="${targetId}"]`)?.remove();
  }, id);
  const size = page.viewportSize() ?? { width: 1280, height: 720 };
  await page.setViewportSize({ width: size.width - 1, height: size.height });
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
    ["compose", "-p", project, "-f", composeFile, "up", "-d", "--build", "--no-deps", "daemon"],
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
  test("A: brand-new account completes the guided path and resumes honestly", async ({ page }, testInfo) => {
    test.setTimeout(180_000);
    const seed = loadSeed();
    const errors: Error[] = [];
    failOnPageError(page, errors);

    await login(page, seed.onboarding.brandNew);
    await expect(page.getByRole("heading", { name: "Create your first workspace" })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(/private|only you can see/i)).toHaveCount(0);
    await capture(page, testInfo, "01-create-workspace.png");

    const stamp = Date.now();
    await page.getByLabel("Workspace name").fill("Onboarding Alpha");
    await page.getByLabel("Workspace address").fill(`onboarding-alpha-${stamp}`);
    await page.getByLabel("Your handle").fill(`alpha-${stamp}`);
    await page.getByRole("button", { name: "Create workspace" }).click();

    await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
    await expectAnchor(page, "connect-local-env");
    await expectAnchor(page, "new-document");
    await expectAnchor(page, "document-more");
    await expectCoach(page, "These are real files", "Step 1 of 3");
    await expect(page.locator(".ob-checklist"), "blocking guide and checklist must never render together").toHaveCount(0);
    await expect.poll(() => guideOneGeometry(page)).toMatchObject({
      candidateCrowdsTarget: true,
      coachOverlapsTarget: false,
    });
    await expect.poll(async () => (await guideOneGeometry(page)).coachTop).toBeGreaterThan(72);

    // Assert the placement invariant rather than a viewport magic number. At 1600
    // the real grid clears the proposed upper-right footprint, so avoidance turns
    // off and the coach returns to its 72px baseline.
    await page.setViewportSize({ width: 1600, height: 900 });
    await expect.poll(() => guideOneGeometry(page)).toEqual({
      candidateCrowdsTarget: false,
      coachTop: 72,
      coachOverlapsTarget: false,
    });
    await page.setViewportSize({ width: 1280, height: 720 });
    await expect.poll(() => guideOneGeometry(page)).toMatchObject({
      candidateCrowdsTarget: true,
      coachOverlapsTarget: false,
    });
    await capture(page, testInfo, "02-guide-1-of-3.png");

    // The real spotlight hole must leave its target operable.
    await createDocumentFromEmptyWorkspace(page);
    await expectAnchor(page, "document-threads");
    await expectAnchor(page, "document-watchers");
    await expectCoach(page, "Every discussion has a home", "Step 2 of 3");
    await capture(page, testInfo, "03-guide-2-of-3.png");

    // Reload before acknowledging step 2: the live document completes only step 1,
    // while the unacknowledged current step resumes from durable/live state.
    await page.reload();
    await expect(page.locator(".cm-editor").first()).toBeVisible({ timeout: 20_000 });
    await expectCoach(page, "Every discussion has a home", "Step 2 of 3");
    await coach(page).locator(".ob-next").click();
    await expectCoach(page, "Let an agent keep watch", "Step 3 of 3");
    await capture(page, testInfo, "04-guide-3-of-3.png");

    // Back is a real session revisit; Next returns to the frontier without changing
    // the durable acknowledgements or the fixed 2/3 and 3/3 positions.
    await coach(page).getByRole("button", { name: "Back" }).click();
    await expectCoach(page, "Every discussion has a home", "Step 2 of 3");
    await coach(page).locator(".ob-next").click();
    await expectCoach(page, "Let an agent keep watch", "Step 3 of 3");
    await coach(page).locator(".ob-next").click();
    await expect(coach(page)).toHaveCount(0);

    const editor = page.locator(".cm-editor").first();
    await editor.click();
    await page.keyboard.type("onboarding discussion anchor");
    await expect(page.locator(".doc-meta-row .chip.ok")).toBeVisible({ timeout: 15_000 });
    await selectEditorLine(page);
    await expectCoach(page, "Talk about this exact line");
    await expect(coach(page).locator(".ob-step")).toHaveCount(0);
    await capture(page, testInfo, "05-first-selection-tip.png");

    // Tip missing-target degradation must acknowledge the tip, not advance the guide.
    await removeAnchorAndRemeasure(page, "selection-thread");
    await expect(coach(page)).toHaveCount(0);
    await page.reload();
    await expect(editor).toBeVisible({ timeout: 20_000 });
    await selectEditorLine(page);
    await expect(coach(page)).toHaveCount(0);
    await onboardingAnchor(page, "selection-thread").click();
    const drafter = page.locator(".thread-drafter");
    await expect(drafter).toBeVisible();
    await drafter.locator("textarea").fill("Please review this exact line.");
    await drafter.locator("button.accent").click();
    await expect(drafter).toBeHidden();

    const checklist = page.locator(".ob-checklist");
    await expect(checklist.getByText("2 of 6 done")).toBeVisible({ timeout: 20_000 });
    await capture(page, testInfo, "06-checklist-2-of-6.png");

    await page.getByRole("button", { name: "Manage / Settings" }).click();
    await page.getByRole("button", { name: "Local environment" }).click();
    await page.getByRole("button", { name: "New local environment" }).click();
    const daemonResponsePromise = page.waitForResponse((response) => (
      response.request().method() === "POST" && /\/api\/workspaces\/[^/]+\/daemons$/.test(new URL(response.url()).pathname)
    ));
    await page.getByRole("button", { name: "Create local environment" }).click();
    const daemonResponse = await daemonResponsePromise;
    expect(daemonResponse.ok()).toBe(true);
    const daemonBody = await daemonResponse.json() as { token: string };
    const workspaceMatch = new URL(daemonResponse.url()).pathname.match(/\/workspaces\/([^/]+)\/daemons$/);
    expect(workspaceMatch?.[1]).toBeTruthy();
    expect(daemonBody.token).toBeTruthy();
    await expect(page.getByText("Install command ready")).toBeVisible();
    // Artifact hygiene: the real token remains only in daemonBody for activation.
    // Public screenshots must never contain a live credential, even for an isolated
    // localhost compose stack that the runner tears down immediately afterward.
    await page.locator(".token-reveal pre.code").evaluate((node) => {
      node.textContent = (node.textContent ?? "").replace(
        /--daemon-token\s+\S+/,
        "--daemon-token nottyd_<redacted>",
      );
    });
    await capture(page, testInfo, "07-connect-environment-waiting.png");

    startComposeDaemon(decodeURIComponent(workspaceMatch![1]), daemonBody.token);
    await expect(page.getByText("Local environment connected. You can create an agent now.")).toBeVisible({ timeout: 60_000 });
    await page.getByRole("button", { name: "Done" }).click();

    await page.getByRole("button", { name: "Manage / Settings" }).click();
    await page.getByRole("button", { name: "Agents" }).click();
    await expectAnchor(page, "new-agent");
    await onboardingAnchor(page, "new-agent").click();
    await expect(page.getByRole("heading", { name: "Create an agent that stays with the project" })).toBeVisible();
    await expect(page.getByText(/role template|choose a template/i)).toHaveCount(0);
    await capture(page, testInfo, "08-create-agent.png");

    await page.getByLabel("Display name").fill("Review Agent");
    await page.getByLabel("Role").fill("Review document changes and explain concrete issues to the team.");
    const createAgent = page.getByRole("button", { name: "Create agent" });
    await expect(createAgent).toBeEnabled({ timeout: 20_000 });
    await createAgent.click();
    await expect(page.getByRole("button", { name: /Open @review-agent/i })).toBeVisible({ timeout: 20_000 });

    await expect(checklist.getByText("4 of 6 done")).toBeVisible({ timeout: 20_000 });
    expectNoPageErrors(errors);
  });

  test("B: invited member enters existing state without owner-only or dishonest create-first steps", async ({ page }, testInfo) => {
    const seed = loadSeed();
    const errors: Error[] = [];
    failOnPageError(page, errors);

    // Seed the invited workspace through the real owner UI. A REST-created document
    // would be pathless and cannot prove the member's rendered live-state derivation.
    await login(page, seed.onboarding.invitedOwner);
    await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
    await createDocumentFromEmptyWorkspace(page);
    await coach(page).getByRole("button", { name: "Skip" }).click();
    await page.getByRole("button", { name: "Sign out" }).click();

    await login(page, seed.onboarding.invitedMember);
    await expect(page.getByRole("heading", { name: "Create your first workspace" })).toBeVisible({ timeout: 20_000 });
    await page.goto(seed.onboarding.invitedMember.invitePath);
    await expect(page.getByRole("heading", { name: `Join ${seed.onboarding.invitedMember.workspaceName}` })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(/invited by/i)).toHaveCount(0);
    await expect(page.getByLabel("Your handle in this workspace")).toBeVisible();
    await capture(page, testInfo, "09-invite.png");
    await page.getByLabel("Your handle in this workspace").fill(seed.onboarding.invitedMember.handle);
    await page.getByRole("button", { name: "Join workspace" }).click();

    await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
    await expectCoach(page, "Every discussion has a home", "Step 2 of 3");
    await expect(coach(page).getByRole("heading", { name: "These are real files" })).toHaveCount(0);
    const checklist = page.locator(".ob-checklist");
    await expect(checklist, "member checklist stays out of the blocking spotlight guide").toHaveCount(0);
    await expectAnchor(page, "document-threads");
    await expectAnchor(page, "document-watchers");

    // Both spotlight fallback rows run against the real mounted app. A removed
    // target advances that presentation once, never paints a coach on empty space.
    await removeAnchorAndRemeasure(page, "document-threads");
    await expectCoach(page, "Let an agent keep watch", "Step 3 of 3");
    await removeAnchorAndRemeasure(page, "document-watchers");
    await expect(coach(page)).toHaveCount(0);
    await expect(checklist.getByText("1 of 5 done")).toBeVisible();
    await expect(checklist.getByText("Invite your team")).toHaveCount(0);
    expectNoPageErrors(errors);
  });

  test("E: returning user sees no welcome and only genuinely incomplete work resumes", async ({ page }) => {
    const seed = loadSeed();
    const errors: Error[] = [];
    failOnPageError(page, errors);

    // Later guide acknowledgements may exist, but a genuinely empty returning
    // workspace must still reopen at its live-incomplete first step.
    await page.addInitScript(({ accountId, workspaceId }) => {
      localStorage.setItem(
        `codesk.onboarding.account.${accountId}.ws.${workspaceId}.flags`,
        JSON.stringify(["seen:threads-intro@v1", "seen:watchers-intro@v1"]),
      );
    }, {
      accountId: seed.onboarding.returningUser.accountId,
      workspaceId: seed.onboarding.returningUser.workspaceId,
    });
    await login(page, seed.onboarding.returningUser);
    await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
    await expect(page.getByRole("heading", { name: "Create your first workspace" })).toHaveCount(0);
    await expectCoach(page, "These are real files", "Step 1 of 3");
    await expect(page.getByText(/feature tour|what's new/i)).toHaveCount(0);
    await createDocumentFromEmptyWorkspace(page);
    await expect(coach(page)).toHaveCount(0);
    await expect(page.locator(".ob-checklist").getByText("1 of 6 done")).toBeVisible({ timeout: 20_000 });
    await page.reload();
    await expect(page.locator(".cm-editor").first()).toBeVisible({ timeout: 20_000 });
    await expect(coach(page)).toHaveCount(0);
    expectNoPageErrors(errors);
  });
});
