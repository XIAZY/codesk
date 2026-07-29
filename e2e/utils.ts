import { expect, type Page } from "@playwright/test";

export async function dismissOnboarding(page: Page): Promise<void> {
  const chapter = page.locator(".ob-chapter");
  const notNow = chapter.locator(".ob-notnow");
  const guide = page.locator(".ob-layer");
  const skip = guide.locator(".ob-skip");

  // Dismiss the promoted #90 chapter FIRST, then the guided tour. The chapter auto-opens for
  // owner/admins after the first document and its "Not now" (.ob-notnow) overlays the editor;
  // dismissing it HANDS BACK to the tour, which resumes its next spotlight — so the guide is
  // dismissed AFTER the chapter, catching that resume (Tomas's teardown-order note). Each
  // waitFor is a bounded no-op when its surface isn't present (member, complete setup, or no guide).
  const chapterVisible = await notNow.waitFor({ state: "visible", timeout: 2_000 })
    .then(() => true, () => false);
  if (chapterVisible) {
    await notNow.click();
    await expect(chapter).toHaveCount(0);
  }

  // The guide measures its target after the workspace renders (or resumes after the chapter
  // closes), so allow that bounded layout pass.
  const guideVisible = await skip.waitFor({ state: "visible", timeout: 2_000 })
    .then(() => true, () => false);
  if (guideVisible) {
    await skip.click();
    await expect(guide).toHaveCount(0);
  }
}
