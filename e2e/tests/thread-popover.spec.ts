import { test, expect, type Locator, type Page, type TestInfo } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

type Seed = {
  email: string;
  password: string;
  slugA: string;
  nameA: string;
};

function loadSeed(): Seed {
  return JSON.parse(readFileSync(join(__dirname, "..", "seed.json"), "utf8")) as Seed;
}

function failOnPageError(page: Page, errors: Error[]): void {
  page.on("pageerror", (err) => errors.push(err));
}

async function loginToWorkspace(page: Page, seed: Seed): Promise<void> {
  await page.goto("/");
  await page.locator('input[type="email"]').first().fill(seed.email);
  await page.locator('input[type="password"]').first().fill(seed.password);
  await page.getByRole("button", { name: /^log in$/i }).click();
  await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
  await page.getByLabel("Workspace").selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
}

async function createDocument(page: Page, content: string): Promise<Locator> {
  await page.getByRole("button", { name: "New document" }).click();
  const editor = page.locator(".cm-editor").first();
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await editor.click();
  if (content.includes("\n")) {
    await page.keyboard.insertText(content);
    const lines = content.split("\n");
    await expect(editor.locator(".cm-line")).toHaveCount(lines.length);
    await expect(editor.locator(".cm-line").first()).toHaveText(lines[0]);
    await expect(editor.locator(".cm-line").last()).toHaveText(lines.at(-1)!);
  } else {
    await page.keyboard.type(content);
    await expect(editor).toContainText(content);
  }
  await expect(page.locator(".doc-meta-row .chip.ok")).toBeVisible({ timeout: 15_000 });
  return editor;
}

async function createAnchoredThread(page: Page, editor: Locator, body: string, line = 1): Promise<void> {
  await editor.click();
  if (line > 1) await page.keyboard.press("Control+End");
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
}

async function setupThreadDocument(
  page: Page,
  bodies: string[],
  content = `thread popover regression ${Date.now()}`,
  line = 1,
): Promise<{ editor: Locator; errors: Error[] }> {
  const seed = loadSeed();
  const errors: Error[] = [];
  failOnPageError(page, errors);
  await loginToWorkspace(page, seed);
  const editor = await createDocument(page, content);
  for (let index = 0; index < bodies.length; index += 1) {
    await createAnchoredThread(page, editor, bodies[index], line);
    await expect(page.getByRole("button", { name: `${index + 1} open thread${index ? "s" : ""} on line ${line}` })).toBeVisible({ timeout: 20_000 });
  }
  return { editor, errors };
}

function assertNoPageErrors(errors: Error[]): void {
  expect(errors, `uncaught page errors during the flow:\n${errors.map((error) => error.stack ?? error.message).join("\n")}`).toEqual([]);
}

async function captureRender(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  const path = testInfo.outputPath(`${name}.png`);
  await page.screenshot({ path });
  await testInfo.attach(name, { path, contentType: "image/png" });
}

async function openThreadDetail(page: Page, markerCount: number, body: string, line = 1): Promise<Locator> {
  await page.getByRole("button", {
    name: `${markerCount} open thread${markerCount === 1 ? "" : "s"} on line ${line}`,
  }).click();
  const list = page.getByRole("dialog", { name: `Threads on line ${line}` });
  await expect(list).toBeVisible();
  const row = list.locator(".thread-popover-row", { hasText: body });
  await expect(row).toBeVisible();
  await row.click();
  const detail = page.getByRole("dialog", { name: /Thread by @/ });
  await expect(detail).toBeVisible();
  return detail;
}

async function expectInsideViewport(page: Page, locator: Locator, margin = 12): Promise<void> {
  await expect.poll(async () => {
    const box = await locator.boundingBox();
    const viewport = page.viewportSize();
    if (!box || !viewport) return false;
    return box.x >= margin - 1
      && box.y >= margin - 1
      && box.x + box.width <= viewport.width - margin + 1
      && box.y + box.height <= viewport.height - margin + 1;
  }).toBe(true);
}

test("thread popover: marker → detail → reply persists through a real reload", async ({ page }, testInfo) => {
  const initial = `initial thread ${Date.now()}`;
  const reply = `persisted reply ${Date.now()}`;
  const { editor, errors } = await setupThreadDocument(page, [initial]);

  let detail = await openThreadDetail(page, 1, initial);
  await expect(detail.getByText(initial)).toBeVisible();
  await captureRender(page, testInfo, "thread-popover-open-detail");
  await detail.getByRole("textbox", { name: "Reply to this thread" }).fill(reply);
  await detail.getByRole("button", { name: "Send reply" }).click();
  await expect(detail.locator(".thread-popover-message-body").getByText(reply, { exact: true })).toBeVisible({ timeout: 20_000 });

  await page.reload();
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await expect(editor).toContainText("thread popover regression", { timeout: 20_000 });
  await expect(page.locator(".doc-meta-row .chip.ok")).toBeVisible({ timeout: 15_000 });
  detail = await openThreadDetail(page, 1, initial);
  await expect(detail.locator(".thread-popover-message-body").getByText(reply, { exact: true })).toBeVisible();

  assertNoPageErrors(errors);
});

test("thread popover: resolving the last open thread removes only the marker", async ({ page }, testInfo) => {
  const body = `last open thread ${Date.now()}`;
  const { errors } = await setupThreadDocument(page, [body]);
  let detail = await openThreadDetail(page, 1, body);

  await detail.getByRole("button", { name: "Mark as resolved" }).click();
  await expect(detail.getByRole("button", { name: "Reopen thread" })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole("button", { name: /open threads? on line 1/ })).toHaveCount(0);
  await expect(detail).toBeVisible();
  await expect(detail.getByText(body)).toBeVisible();
  await captureRender(page, testInfo, "thread-popover-resolved-detail");

  await detail.getByRole("button", { name: "Back to threads on this line" }).click();
  const list = page.getByRole("dialog", { name: "Threads on line 1" });
  await expect(list).toBeVisible();
  await expect(list.locator(".thread-popover-row.resolved", { hasText: body })).toBeVisible();
  await list.getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("button", { name: /open threads? on line 1/ })).toHaveCount(0);

  assertNoPageErrors(errors);
});

test("thread popover: mixed line orders resolved last and supports Reopen", async ({ page }, testInfo) => {
  const resolvedBody = `resolve me ${Date.now()}`;
  const openBody = `keep me open ${Date.now()}`;
  const { errors } = await setupThreadDocument(page, [resolvedBody, openBody]);
  let detail = await openThreadDetail(page, 2, resolvedBody);

  await detail.getByRole("button", { name: "Mark as resolved" }).click();
  await expect(detail.getByRole("button", { name: "Reopen thread" })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole("button", { name: "1 open thread on line 1" })).toBeVisible();
  await detail.getByRole("button", { name: "Back to threads on this line" }).click();

  let list = page.getByRole("dialog", { name: "Threads on line 1" });
  let rows = list.locator(".thread-popover-row");
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toContainText(openBody);
  await expect(rows.nth(0)).not.toHaveClass(/resolved/);
  await expect(rows.nth(1)).toContainText(resolvedBody);
  await expect(rows.nth(1)).toHaveClass(/resolved/);
  await captureRender(page, testInfo, "thread-popover-list");

  await rows.nth(1).click();
  detail = page.getByRole("dialog", { name: /Thread by @/ });
  await detail.getByRole("button", { name: "Reopen thread" }).click();
  await expect(detail.getByRole("button", { name: "Mark as resolved" })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole("button", { name: "2 open threads on line 1" })).toBeVisible();
  await detail.getByRole("button", { name: "Back to threads on this line" }).click();

  list = page.getByRole("dialog", { name: "Threads on line 1" });
  rows = list.locator(".thread-popover-row");
  await expect(rows).toHaveCount(2);
  await expect(list.locator(".thread-popover-row.resolved")).toHaveCount(0);

  assertNoPageErrors(errors);
});

test("thread popover: Back restores the row and Escape/outside click dismiss", async ({ page }) => {
  const body = `dismissal thread ${Date.now()}`;
  const { errors } = await setupThreadDocument(page, [body]);
  let detail = await openThreadDetail(page, 1, body);

  await detail.getByRole("button", { name: "Back to threads on this line" }).click();
  let list = page.getByRole("dialog", { name: "Threads on line 1" });
  const row = list.locator(".thread-popover-row", { hasText: body });
  await expect(row).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toHaveCount(0);

  await page.getByRole("button", { name: "1 open thread on line 1" }).click();
  list = page.getByRole("dialog", { name: "Threads on line 1" });
  await expect(list).toBeVisible();
  await page.locator(".document-title-row").click({ position: { x: 4, y: 4 } });
  await expect(page.getByRole("dialog")).toHaveCount(0);

  detail = await openThreadDetail(page, 1, body);
  await page.keyboard.press("Escape");
  await expect(detail).toBeHidden();

  assertNoPageErrors(errors);
});

test("thread popover: mobile sheet keeps fixed regions reachable and scrolls messages only", async ({ page }, testInfo) => {
  const longBody = `mobile overflow ${"message ".repeat(220)}`;
  const { errors } = await setupThreadDocument(page, [longBody]);
  await page.setViewportSize({ width: 390, height: 844 });

  const detail = await openThreadDetail(page, 1, "mobile overflow");
  const dialogBox = await detail.boundingBox();
  expect(dialogBox).not.toBeNull();
  expect(dialogBox!.x).toBeGreaterThanOrEqual(0);
  expect(dialogBox!.x + dialogBox!.width).toBeLessThanOrEqual(390);
  expect(dialogBox!.y + dialogBox!.height).toBeLessThanOrEqual(844);

  for (const region of [
    detail.locator(".thread-popover-head"),
    detail.locator(".thread-popover-anchor"),
    detail.locator(".thread-popover-actions"),
    detail.locator(".thread-popover-reply"),
  ]) {
    await expect(region).toBeVisible();
    const box = await region.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.y).toBeGreaterThanOrEqual(dialogBox!.y);
    expect(box!.y + box!.height).toBeLessThanOrEqual(dialogBox!.y + dialogBox!.height + 1);
  }

  const messages = detail.locator(".thread-popover-messages");
  await expect(messages).toBeVisible();
  expect(await messages.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);
  await expect(detail.getByRole("button", { name: "Mark as resolved" })).toBeVisible();
  await expect(detail.getByRole("button", { name: "Jump to line 1" })).toBeVisible();
  await expect(detail.getByRole("textbox", { name: "Reply to this thread" })).toBeVisible();
  await captureRender(page, testInfo, "thread-popover-mobile-detail");

  assertNoPageErrors(errors);
});

test("thread popover: desktop float stays contained near viewport edges and in a short window", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const lineCount = 28;
  const content = Array.from({ length: lineCount }, (_, index) => `containment line ${index + 1}`).join("\n");
  const body = `desktop overflow ${"message ".repeat(220)}`;
  const { errors } = await setupThreadDocument(page, [body], content, lineCount);
  await page.setViewportSize({ width: 720, height: 360 });
  const marker = page.getByRole("button", { name: `1 open thread on line ${lineCount}` });
  const markerBox = await marker.boundingBox();
  expect(markerBox).not.toBeNull();
  expect(markerBox!.x + markerBox!.width).toBeGreaterThan(600);

  await marker.click();
  const list = page.getByRole("dialog", { name: `Threads on line ${lineCount}` });
  await expect(list).toBeVisible();
  await expectInsideViewport(page, list);
  await captureRender(page, testInfo, "thread-popover-desktop-edge-list");
  await list.locator(".thread-popover-row", { hasText: "desktop overflow" }).click();

  const detail = page.getByRole("dialog", { name: /Thread by @/ });
  await expect(detail).toBeVisible();
  await expectInsideViewport(page, detail);
  for (const region of [
    detail.locator(".thread-popover-head"),
    detail.locator(".thread-popover-anchor"),
    detail.locator(".thread-popover-actions"),
    detail.locator(".thread-popover-reply"),
  ]) {
    await expect(region).toBeVisible();
    await expectInsideViewport(page, region, 0);
  }

  const messages = detail.locator(".thread-popover-messages");
  await expect(messages).toBeVisible();
  expect(await messages.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);
  expect(await detail.evaluate((element) => element.scrollHeight <= element.clientHeight + 1)).toBe(true);
  await expect(detail.getByRole("textbox", { name: "Reply to this thread" })).toBeVisible();
  await captureRender(page, testInfo, "thread-popover-desktop-short-detail");

  assertNoPageErrors(errors);
});
