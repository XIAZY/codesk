import { expect, type Page } from "@playwright/test";

export async function dismissOnboarding(page: Page): Promise<void> {
  const chapter = page.locator(".ob-chapter");
  const notNow = chapter.locator(".ob-notnow");
  const guide = page.locator(".ob-layer");
  const skip = guide.locator(".ob-skip");

  // Lifecycle-aware teardown: the promoted #90 chapter auto-opens after the first document and
  // its "Not now" (.ob-notnow) overlays the editor; dismissing it HANDS BACK to the guided tour,
  // whose next spotlight (.ob-layer) then resumes — and either can surface behind the other. So
  // dismiss whichever onboarding surface is up and re-check, until NEITHER remains. Bounded so it
  // can never spin; the first pass tolerates a surface still mounting, later passes settle fast.
  for (let pass = 0; pass < 4; pass++) {
    const timeout = pass === 0 ? 2_000 : 500;
    const chapterVisible = await notNow.waitFor({ state: "visible", timeout })
      .then(() => true, () => false);
    if (chapterVisible) {
      await notNow.click();
      await expect(chapter).toHaveCount(0);
      continue;
    }
    const guideVisible = await skip.waitFor({ state: "visible", timeout })
      .then(() => true, () => false);
    if (guideVisible) {
      await skip.click();
      await expect(guide).toHaveCount(0);
      continue;
    }
    return; // neither onboarding surface visible — teardown complete
  }
}
