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
