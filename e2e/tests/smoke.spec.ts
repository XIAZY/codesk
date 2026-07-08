import { test, expect, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

type Seed = {
  email: string; password: string;
  slugA: string; nameA: string;
  slugB: string; nameB: string;
};

// Read lazily at run time, not module-load: seed.json is written by global-setup after the stack is up.
function loadSeed(): Seed {
  return JSON.parse(readFileSync(join(__dirname, "..", "seed.json"), "utf8")) as Seed;
}

// The global assertion worth more than the three: ANY uncaught exception anywhere in the flow fails the
// run. With no React error boundary in the app, this is the automated stand-in for it — the white-screen
// class fails CI even where explicit assertions are thin. Attached per-page so it also covers the switch.
function failOnPageError(page: Page, errors: Error[]): void {
  page.on("pageerror", (err) => errors.push(err));
}

async function login(page: Page, seed: Seed): Promise<void> {
  await page.goto("/");
  await page.locator('input[type="email"]').first().fill(seed.email);
  await page.locator('input[type="password"]').first().fill(seed.password);
  await page.getByRole("button", { name: /^log in$/i }).click();
}

test("core flow: load → auth → switch A↔B (idle B) → open document, no page errors", async ({ page }) => {
  const seed = loadSeed();
  const errors: Error[] = [];
  failOnPageError(page, errors);

  // 1. Load → authenticate → app shell renders (a workspace route, non-empty root). The app lands on the
  //    most-recently-created workspace (B here), so normalize to A via the real switcher before the flow —
  //    this makes the A→B switch in step 2 the controlled starting point regardless of default-landing order.
  await login(page, seed);
  await expect(page.getByLabel("Workspace")).toBeVisible({ timeout: 20_000 });
  await page.getByLabel("Workspace").selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
  await expect(page.locator(".workspace-switcher b").first()).toContainText(seed.nameA);

  // 2. Switch A→B where B is fully idle — the white-screen incident, walked. Assert the navigation itself
  //    (URL slug + B-name header marker), not just the absence of a crash, so both failure modes are caught.
  await page.getByLabel("Workspace").selectOption(seed.slugB);
  await expect(page).toHaveURL(new RegExp(seed.slugB));
  await expect(page.locator(".workspace-switcher b").first()).toContainText(seed.nameB);

  // 3. Switch back B→A (symmetric).
  await page.getByLabel("Workspace").selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
  await expect(page.locator(".workspace-switcher b").first()).toContainText(seed.nameA);

  // 4. Create a document via the real "New document" action → the CodeMirror editor mounts, then type a line
  //    and assert it renders. A document's path lives in the workspace root-namespace CRDT and its text in the
  //    per-document CRDT — there is no REST seed path, so creating through the UI is the faithful way to reach a
  //    mounted editor, and typing exercises the CRDT content-write end to end.
  await page.getByRole("button", { name: "New document" }).click();
  const editor = page.locator(".cm-editor").first();
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await editor.click();
  await page.keyboard.type("smoke content line");
  await expect(editor).toContainText("smoke content line");

  // The whole flow must have been exception-free (the incident class is a page error on switch).
  expect(errors, `uncaught page errors during the flow:\n${errors.map((e) => e.stack ?? e.message).join("\n")}`).toEqual([]);
});
