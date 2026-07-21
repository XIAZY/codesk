import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { dismissOnboarding } from "../utils";

type Seed = {
  email: string;
  password: string;
  workspaceAId: string;
  slugA: string;
  memberToken: string;
  memberHandle: string;
};

type WorkspaceThread = {
  id: string;
  messages: Array<{ body: string }>;
};

function loadSeed(): Seed {
  return JSON.parse(readFileSync(join(__dirname, "..", "seed.json"), "utf8")) as Seed;
}

async function loginToWorkspace(page: Page, seed: Seed): Promise<void> {
  await page.goto("/");
  await page.locator('input[type="email"]').first().fill(seed.email);
  await page.locator('input[type="password"]').first().fill(seed.password);
  await page.getByRole("button", { name: /^log in$/i }).click();
  const workspacePicker = page.getByLabel("Workspace", { exact: true });
  await expect(workspacePicker).toBeVisible({ timeout: 20_000 });
  await workspacePicker.selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
  await dismissOnboarding(page);
}

async function createDocument(page: Page, content: string): Promise<Locator> {
  await page.getByRole("button", { name: "New document" }).click();
  const editor = page.locator(".cm-editor").first();
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await dismissOnboarding(page);
  await editor.click();
  await page.keyboard.type(content);
  await expect(editor).toContainText(content);
  await expect(page.locator(".doc-meta-row .chip.ok")).toBeVisible({ timeout: 15_000 });
  return editor;
}

async function createAnchoredThread(page: Page, editor: Locator, body: string): Promise<void> {
  await editor.click();
  await page.keyboard.press("Control+Home");
  await page.keyboard.press("Home");
  await page.keyboard.press("Shift+End");
  const startThread = page.locator(".selection-toolbar button.primary").first();
  await expect(startThread).toBeVisible();
  await startThread.click();
  const drafter = page.locator(".thread-drafter");
  await expect(drafter).toBeVisible();
  await drafter.locator("textarea").fill(body);
  await drafter.locator("button.accent").click();
  await expect(drafter).toBeHidden();
  await expect(page.getByRole("button", { name: "1 open thread on line 1" })).toBeVisible({ timeout: 20_000 });
}

async function findThread(page: Page, seed: Seed, openingBody: string): Promise<WorkspaceThread> {
  const backendURL = process.env.NOTTY_E2E_BACKEND_URL;
  expect(backendURL).toBeTruthy();
  const response = await page.request.get(`${backendURL}/api/workspaces/${seed.workspaceAId}/workspace`, {
    headers: { Authorization: `Bearer ${seed.memberToken}` },
  });
  expect(response.ok()).toBeTruthy();
  const workspace = await response.json() as { threads: WorkspaceThread[] };
  const thread = workspace.threads.find((candidate) => candidate.messages[0]?.body === openingBody);
  expect(thread, `thread with opening body ${openingBody}`).toBeTruthy();
  return thread!;
}

async function replyAsMember(page: Page, seed: Seed, threadId: string, body: string): Promise<void> {
  const backendURL = process.env.NOTTY_E2E_BACKEND_URL;
  expect(backendURL).toBeTruthy();
  const response = await page.request.post(
    `${backendURL}/api/workspaces/${seed.workspaceAId}/threads/${threadId}/messages`,
    {
      headers: { Authorization: `Bearer ${seed.memberToken}` },
      data: { body, kind: "comment" },
    },
  );
  expect(response.ok()).toBeTruthy();
}

async function capture(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  const path = testInfo.outputPath(`${name}.png`);
  await page.screenshot({ path });
  await testInfo.attach(name, { path, contentType: "image/png" });
}

test("thread reply notifications remain unread until exact detail opens and fit mobile", async ({ page }, testInfo) => {
  const seed = loadSeed();
  const pageErrors: Error[] = [];
  page.on("pageerror", (error) => pageErrors.push(error));
  await loginToWorkspace(page, seed);

  const stamp = Date.now();
  const openingBody = `notification opening ${stamp}`;
  const firstReply = `notification reply ${stamp}`;
  const secondReply = `mobile notification reply ${stamp}`;
  const editor = await createDocument(page, `notification document ${stamp}`);
  await createAnchoredThread(page, editor, openingBody);
  const thread = await findThread(page, seed, openingBody);

  await replyAsMember(page, seed, thread.id, firstReply);
  const bell = page.getByRole("button", { name: "1 unread thread reply" });
  await expect(bell).toBeVisible({ timeout: 20_000 });
  await bell.click();

  let notifications = page.getByRole("dialog", { name: "Unread thread replies" });
  await expect(notifications).toBeVisible();
  await expect(notifications.getByText(`@${seed.memberHandle}`)).toBeVisible();
  await expect(notifications.getByText(firstReply, { exact: true })).toBeVisible();
  await expect(notifications.getByText("1 new", { exact: true })).toBeVisible();
  const openReply = notifications.getByRole("button", { name: /^Open 1 new reply in / });
  const openLabel = await openReply.getAttribute("aria-label");
  expect(openLabel).toMatch(/^Open 1 new reply in .+\.md$/);
  await capture(page, testInfo, "thread-notifications-desktop-unread");

  await notifications.getByRole("button", { name: "Close new replies" }).click();
  await expect(bell).toBeVisible();
  await bell.click();
  notifications = page.getByRole("dialog", { name: "Unread thread replies" });
  await notifications.getByRole("button", { name: /^Open 1 new reply in / }).click();

  const threadDialog = page.getByRole("dialog", { name: "Threads on this document" });
  await expect(threadDialog).toBeVisible({ timeout: 20_000 });
  await expect(threadDialog.getByText(firstReply, { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "No unread thread replies" })).toBeVisible();

  await threadDialog.getByRole("button", { name: "Close" }).click();
  await expect(threadDialog).toBeHidden();
  await replyAsMember(page, seed, thread.id, secondReply);
  await expect(page.getByRole("button", { name: "1 unread thread reply" })).toBeVisible({ timeout: 20_000 });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "1 unread thread reply" }).click();
  notifications = page.getByRole("dialog", { name: "Unread thread replies" });
  await expect(notifications.getByText(secondReply, { exact: true })).toBeVisible();
  const box = await notifications.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.y).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  expect(box!.y + box!.height).toBeLessThanOrEqual(844);
  await capture(page, testInfo, "thread-notifications-mobile-unread");

  expect(pageErrors, `uncaught page errors:\n${pageErrors.map((error) => error.stack ?? error.message).join("\n")}`).toEqual([]);
});
