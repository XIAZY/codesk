import { expect, type Page } from "@playwright/test";

export async function dismissOnboarding(page: Page): Promise<void> {
  const guide = page.locator(".ob-layer");
  const skip = guide.locator(".ob-skip");

  // The guide measures its target after the workspace renders, so allow that
  // bounded layout pass while keeping this helper a no-op when no guide applies.
  const visible = await skip.waitFor({ state: "visible", timeout: 2_000 })
    .then(() => true, () => false);
  if (!visible) return;

  await skip.click();
  await expect(guide).toHaveCount(0);
}

export async function expectDocumentPersisted(page: Page, content: string): Promise<void> {
  const observer = await page.context().newPage();
  try {
    await observer.goto(page.url());
    await dismissOnboarding(observer);
    const editor = observer.locator(".cm-editor").first();
    await expect(editor).toBeVisible({ timeout: 20_000 });
    // A second websocket subscriber receives updates only after the backend has
    // applied them. Unlike the always-visible "Live" chip, this proves the
    // current content survived beyond the writer's local Y.Doc.
    await expect.poll(
      () => editor.locator(".cm-line").allTextContents(),
      { timeout: 20_000 },
    ).toEqual(content.split("\n"));
  } finally {
    await observer.close();
  }
}
