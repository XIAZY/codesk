import { expect, type Page } from "@playwright/test";

export async function dismissOnboarding(page: Page): Promise<void> {
  const guide = page.locator(".ob-layer");
  const skip = guide.locator(".ob-skip");

  // The guide measures its target after the workspace renders, so allow that
  // bounded layout pass while keeping this helper a no-op when no guide applies.
  const guideVisible = await skip.waitFor({ state: "visible", timeout: 2_000 })
    .then(() => true, () => false);
  if (guideVisible) {
    await skip.click();
    await expect(guide).toHaveCount(0);
  }

  // #90: the promoted "Add an AI teammate" chapter auto-opens for owner/admins after the
  // first document, and its "Not now" (.ob-notnow) overlays and intercepts editor clicks —
  // so dismiss it too. Also a no-op when the chapter never opens (e.g. member, or complete).
  const chapter = page.locator(".ob-chapter");
  const notNow = chapter.locator(".ob-notnow");
  const chapterVisible = await notNow.waitFor({ state: "visible", timeout: 2_000 })
    .then(() => true, () => false);
  if (chapterVisible) {
    await notNow.click();
    await expect(chapter).toHaveCount(0);
  }
}
