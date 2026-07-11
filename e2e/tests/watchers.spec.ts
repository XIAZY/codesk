import { test, expect, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

type Seed = {
  email: string; password: string;
  slugA: string; nameA: string;
  slugB: string; nameB: string;
};

function loadSeed(): Seed {
  return JSON.parse(readFileSync(join(__dirname, "..", "seed.json"), "utf8")) as Seed;
}

function failOnPageError(page: Page, errors: Error[]): void {
  page.on("pageerror", (err) => errors.push(err));
}

async function login(page: Page, seed: Seed): Promise<void> {
  await page.goto("/");
  await page.locator('input[type="email"]').first().fill(seed.email);
  await page.locator('input[type="password"]').first().fill(seed.password);
  await page.getByRole("button", { name: /^log in$/i }).click();
}

// Watchers popover. The right-rail "Participants" tab (task #4) was removed; its view/add/remove of document
// subscribers moved to a top-bar Watchers button (👥 + subscriber count), mirroring the #118 Threads move.
// This pins over the real stack: with a document open, the rail no longer has a Participants tab, the top-bar
// Watchers button opens a doc-scoped popover with the Watching section + honest empty state + Add-watcher, and
// the "Here now" presence display is gone. Seed workspace A has no agents, so Watching is empty and the picker
// has nothing to offer — exactly the doc-scoped, no-ambient-list invariant. The subscribe→Watching→unsubscribe
// write-flow is pinned at the API layer (TestDocumentSubscribersReadAuthzAndBehavior) and the grouping in the
// documentParticipants unit test; a full-picker e2e needs an agent seeded into workspace A (a harness extension).
test("watchers popover: top-bar entry replaces the Participants rail tab and is document-scoped", async ({ page }) => {
  const seed = loadSeed();
  const errors: Error[] = [];
  failOnPageError(page, errors);

  await login(page, seed);
  await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
  await page.getByLabel("Workspace").selectOption(seed.slugA);

  // A document to scope the popover to (its content is written through the editor — the faithful product path).
  await page.getByRole("button", { name: "New document" }).click();
  await expect(page.locator(".cm-editor").first()).toBeVisible({ timeout: 20_000 });

  // The Participants rail tab is gone; only Document Activity remains in the rail.
  const railTabs = page.locator(".ctx-tabs");
  await expect(railTabs.getByRole("button", { name: /participants/i })).toHaveCount(0);
  await expect(railTabs.getByRole("button", { name: /Document Activity/i })).toBeVisible();

  // Open the top-bar Watchers popover.
  const trigger = page.locator(".document-watchers-trigger");
  await expect(trigger).toBeVisible();
  await trigger.click();

  const dialog = page.getByRole("dialog", { name: "Watchers on this document" });
  await expect(dialog).toBeVisible();

  // Subscriber-only structure: a Watching section (exact match — "Watching" is also a substring of the empty
  // copy), the honest empty state (no ambient every-agent list), an Add-watcher control, and NO Here-now
  // presence section (removed with the tab).
  await expect(dialog.getByText("Watching", { exact: true }).first()).toBeVisible();
  await expect(dialog.getByText(/no agents are watching this document yet/i)).toBeVisible();
  await expect(dialog.getByText("Add watcher", { exact: true })).toBeVisible();
  await expect(dialog.getByText("Here now", { exact: true })).toHaveCount(0);

  expect(errors, `uncaught page errors during the flow:\n${errors.map((e) => e.stack ?? e.message).join("\n")}`).toEqual([]);
});
