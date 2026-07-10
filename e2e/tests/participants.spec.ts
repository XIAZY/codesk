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

// Participants panel (task #4). The right tab was converted from the workspace-wide "People" list to a
// document-scoped "Participants" panel (AlphaToad's full-conversion ruling). This pins the conversion over
// the real stack: with a document open, the tab reads "Participants" and renders the doc-scoped sections
// (Here now / Watching) with the honest empty state rather than an ambient workspace member list.
//
// Seed workspace A has no agents, so the Watching section is empty and the add-watcher picker has nothing to
// offer — which is exactly the doc-scoped invariant we want to assert (no ambient every-agent list). The
// subscribe→Watching→unsubscribe write-flow is pinned at the API layer
// (TestDocumentSubscribersReadAuthzAndBehavior) and the grouping in the documentParticipants unit test; a
// full-picker e2e needs an agent seeded into workspace A (a harness extension).
test("participants panel: the right tab is document-scoped, not the workspace People list", async ({ page }) => {
  const seed = loadSeed();
  const errors: Error[] = [];
  failOnPageError(page, errors);

  await login(page, seed);
  await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
  await page.getByLabel("Workspace").selectOption(seed.slugA);

  // A document to scope the panel to (its content is written through the editor — the faithful product path).
  await page.getByRole("button", { name: "New document" }).click();
  await expect(page.locator(".cm-editor").first()).toBeVisible({ timeout: 20_000 });

  // Open the converted tab.
  await page.getByRole("button", { name: /participants/i }).click();
  const panel = page.locator(".ctx-body.people-pane");
  await expect(panel).toBeVisible();
  await expect(panel.locator(".label", { hasText: "Participants" })).toBeVisible();

  // Document-scoped structure: the two sections, and the Watching empty state (no ambient member list).
  await expect(panel.getByText("Here now")).toBeVisible();
  await expect(panel.getByText("Watching")).toBeVisible();
  await expect(panel.getByText(/no agents are watching this document yet/i)).toBeVisible();
  await expect(panel.getByText("Add watcher")).toBeVisible();

  expect(errors, `uncaught page errors during the flow:\n${errors.map((e) => e.stack ?? e.message).join("\n")}`).toEqual([]);
});
