import { test, expect, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { dismissOnboarding } from "../utils";

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
  const workspacePicker = page.getByLabel("Workspace", { exact: true });
  await expect(workspacePicker).toBeVisible({ timeout: 20_000 });
  await workspacePicker.selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
  await expect(page.locator(".workspace-switcher b").first()).toContainText(seed.nameA);
  await dismissOnboarding(page);

  // 2. Switch A→B where B is fully idle — the white-screen incident, walked. Assert the navigation itself
  //    (URL slug + B-name header marker), not just the absence of a crash, so both failure modes are caught.
  await workspacePicker.selectOption(seed.slugB);
  await expect(page).toHaveURL(new RegExp(seed.slugB));
  await expect(page.locator(".workspace-switcher b").first()).toContainText(seed.nameB);
  await dismissOnboarding(page);

  // 3. Switch back B→A (symmetric).
  await workspacePicker.selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
  await expect(page.locator(".workspace-switcher b").first()).toContainText(seed.nameA);
  await dismissOnboarding(page);

  // 4. Create a document via the real "New document" action → the CodeMirror editor mounts, then type a line
  //    and assert it renders. A document's path lives in the workspace root-namespace CRDT and its text in the
  //    per-document CRDT — there is no REST seed path, so creating through the UI is the faithful way to reach a
  //    mounted editor, and typing exercises the CRDT content-write end to end.
  await page.getByRole("button", { name: "New document" }).click();
  const editor = page.locator(".cm-editor").first();
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await dismissOnboarding(page);
  await editor.click();
  await page.keyboard.type("smoke content line");
  await expect(editor).toContainText("smoke content line");

  // The whole flow must have been exception-free (the incident class is a page error on switch).
  expect(errors, `uncaught page errors during the flow:\n${errors.map((e) => e.stack ?? e.message).join("\n")}`).toEqual([]);
});

// False-orphan pin (Tom's amendment, from prod incident PR #100). A thread on a HEALTHY Yjs anchor must
// NOT show an "anchor lost" warning after the document is reopened and re-synced. The bug: the anchor
// classification effect ran against a pre-sync EMPTY ydoc and stuck healthy threads as orphaned; #100 guards
// it on the ydoc `ready` state so it only classifies after sync. This is the ONLY tier that walks the async
// sync window e2e — content arrives over the real WS after first render, which is exactly the point. The
// healthy anchor is created the product way: type real content, select a range, open a thread (a real Yjs
// relative position into live content — no external Yjs client, no REST seed, which cannot carry content).
test("false-orphan pin: a healthy-anchored thread shows no 'anchor lost' after reopen + sync", async ({ page }) => {
  const seed = loadSeed();
  const errors: Error[] = [];
  failOnPageError(page, errors);

  await login(page, seed);
  const workspacePicker = page.getByLabel("Workspace", { exact: true });
  await expect(workspacePicker).toBeVisible({ timeout: 20_000 });
  await workspacePicker.selectOption(seed.slugA);
  await expect(page).toHaveURL(new RegExp(seed.slugA));
  await dismissOnboarding(page);

  // Create a document with real content, then anchor a thread to a selected range of it.
  await page.getByRole("button", { name: "New document" }).click();
  const editor = page.locator(".cm-editor").first();
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await dismissOnboarding(page);
  await editor.click();
  await page.keyboard.type("anchored content for a healthy thread");
  // Wait for the document to report synced before creating the anchor, so the relative position is written
  // against content the backend has persisted (the anchor must survive the reopen below).
  await expect(page.locator(".doc-meta-row .chip.ok")).toBeVisible({ timeout: 15_000 });
  await page.keyboard.press("Home");
  await page.keyboard.press("Shift+End"); // select the line -> a real text-range anchor
  await page.locator(".selection-toolbar button.primary").first().click();
  const drafter = page.locator(".thread-drafter");
  await expect(drafter).toBeVisible();
  await drafter.locator("textarea").fill("is this section right?");
  await drafter.locator("button.accent").click();
  await expect(drafter).toBeHidden();

  // Reopen the document: reload so the ydoc starts EMPTY and re-syncs the content over the WS — the async
  // window the false-orphan bug lived in. Once the content is back and the doc reports synced, the healthy
  // thread must show NO orphan warning (pre-#100 it stuck on "⚠ anchor lost" against the pre-sync empty doc).
  await page.reload();
  await dismissOnboarding(page);
  await expect(editor).toBeVisible({ timeout: 20_000 });
  await expect(editor).toContainText("anchored content for a healthy thread", { timeout: 20_000 });
  await expect(page.locator(".doc-meta-row .chip.ok")).toBeVisible({ timeout: 15_000 });
  await expect(page.locator(".thread-orphan-warning")).toHaveCount(0);
  await expect(page.getByText(/anchor lost/i)).toHaveCount(0);

  expect(errors, `uncaught page errors during the flow:\n${errors.map((e) => e.stack ?? e.message).join("\n")}`).toEqual([]);
});
