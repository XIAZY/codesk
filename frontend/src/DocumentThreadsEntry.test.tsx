// @vitest-environment jsdom

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as Y from "yjs";
import { emptyWorkspace } from "./logic";
import type { ThreadItem, WorkspaceState } from "./types";

const ydoc = new Y.Doc();

function threadFixture(overrides: Partial<ThreadItem> = {}): ThreadItem {
  return {
    id: "thread_1",
    documentId: "doc_1",
    title: "Test thread",
    status: "open",
    anchor: { kind: "range", excerpt: "selected text" },
    createdById: "user_1",
    createdByType: "human",
    createdByHandle: "ada",
    createdByName: "Ada",
    participantIds: ["user_1"],
    participantHandles: ["ada"],
    messages: [
      { id: "msg_1", threadId: "thread_1", authorId: "user_1", authorType: "human", authorHandle: "ada", authorName: "Ada", body: "First comment", kind: "comment", createdAt: "2026-07-06T12:00:00Z" },
    ],
    createdAt: "2026-07-06T12:00:00Z",
    updatedAt: "2026-07-06T12:00:00Z",
    ...overrides,
  };
}

const workspace: WorkspaceState = {
  ...emptyWorkspace(),
  workspaceId: "ws",
  rootDocumentId: "root",
  name: "Workspace",
  currentUserId: "user_1",
  users: [{ id: "user_1", handle: "ada", name: "Ada", role: "", kind: "human", status: "active", updatedAt: "now" }],
  presences: {
    user_1: { actorId: "user_1", actorType: "human", documentId: "doc_1", activity: "editing", updatedAt: new Date().toISOString() },
  },
  threads: [
    threadFixture(),
    threadFixture({ id: "thread_2", status: "resolved", title: "Resolved thread" }),
  ],
};

vi.mock("./useWorkspace", () => ({
  useWorkspace: () => ({ workspace, connected: true, loading: false, error: "", reload: vi.fn() }),
}));

vi.mock("./useRootNamespace", () => ({
  useRootNamespace: () => ({
    documents: [{ id: "doc_1", path: "docs/Product.md", title: "Product.md" }],
    ready: true,
    upsertFile: vi.fn(),
    moveFile: vi.fn(),
    tombstoneFile: vi.fn(),
  }),
}));

vi.mock("./useDocument", () => ({
  useDocumentSync: () => ({ ydoc, ytext: ydoc.getText("content"), ready: false, connected: true }),
}));

vi.mock("./DocumentSurface", () => ({
  DocumentSurface: () => <div data-testid="document-surface" />,
}));

import { WorkspaceApp } from "./App";

afterEach(() => {
  localStorage.clear();
  cleanup();
});

function renderWorkspace() {
  return render(
    <WorkspaceApp
      api={{ updateLastAccessed: vi.fn().mockResolvedValue({}) } as never}
      token="token"
      workspaceId="ws"
      workspaceSlug="workspace"
      view={{ kind: "document", documentId: "doc_1" }}
      account={{ id: "account_1", email: "you@example.com", displayName: "You" }}
      workspaces={[{ id: "ws", slug: "workspace", name: "Workspace" }]}
      onAccess={vi.fn()}
      onWorkspaceChange={vi.fn()}
      onSignOut={vi.fn()}
    />,
  );
}

describe("document Threads toolbar entry", () => {
  it("replaces the right-rail Threads tab and opens the current-document panel", async () => {
    const user = userEvent.setup();
    const { container } = renderWorkspace();

    // The right rail is gone entirely — no .ctx-tabs, and Activity now lives in the "…" menu.
    expect(container.querySelector(".ctx-tabs")).toBeNull();
    expect(container.querySelector(".ctx")).toBeNull();

    const trigger = screen.getByRole("button", { name: "Threads, 1 open" });
    const toolbarActions = container.querySelector(".doc-toolbar > .row.gap-6") as HTMLElement;
    const toolbarChildren = Array.from(toolbarActions.children);
    const collaborators = container.querySelector(".collaborator-avatars") as Element;
    const threadEntry = container.querySelector(".document-threads-entry") as Element;
    expect(collaborators).toBeTruthy();
    expect(threadEntry).toBeTruthy();
    expect(toolbarChildren.indexOf(collaborators)).toBeLessThan(toolbarChildren.indexOf(threadEntry));
    await user.click(trigger);

    const dialog = screen.getByRole("dialog", { name: "Threads on this document" });
    expect(within(dialog).getByText(/First comment/)).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: /Resolved · 1/ })).toBeTruthy();
    expect(within(dialog).queryByText("Resolved thread")).toBeNull();

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Threads on this document" })).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });

  it("only one toolbar popover is open at a time — Threads, Watchers, and Activity are mutually exclusive", async () => {
    const user = userEvent.setup();
    renderWorkspace();

    const threadsTrigger = screen.getByRole("button", { name: "Threads, 1 open" });
    const watchersTrigger = screen.getByRole("button", { name: "Watchers" });
    const moreTrigger = screen.getByRole("button", { name: "Document options" });
    const openActivity = async () => {
      await user.click(moreTrigger);
      await user.click(screen.getByRole("menuitem", { name: /Document activity/i }));
    };

    await user.click(threadsTrigger);
    expect(screen.getByRole("dialog", { name: "Threads on this document" })).toBeTruthy();

    // Opening Watchers closes Threads.
    await user.click(watchersTrigger);
    expect(screen.getByRole("dialog", { name: "Watchers on this document" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Threads on this document" })).toBeNull();

    // Opening Activity (from the "…" menu) closes Watchers.
    await openActivity();
    expect(screen.getByRole("dialog", { name: "Activity on this document" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Watchers on this document" })).toBeNull();

    // Opening Threads closes Activity.
    await user.click(threadsTrigger);
    expect(screen.getByRole("dialog", { name: "Threads on this document" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Activity on this document" })).toBeNull();
  });

  it("the \"…\" menu holds Document activity, Move, and Delete", async () => {
    const user = userEvent.setup();
    renderWorkspace();

    await user.click(screen.getByRole("button", { name: "Document options" }));
    const menu = screen.getByRole("menu", { name: "Document options" });
    expect(within(menu).getByRole("menuitem", { name: /Document activity/i })).toBeTruthy();
    expect(within(menu).getByRole("menuitem", { name: /Move document/i })).toBeTruthy();
    expect(within(menu).getByRole("menuitem", { name: /Delete document/i })).toBeTruthy();

    // Delete opens a confirmation modal naming the document, not a silent delete.
    await user.click(within(menu).getByRole("menuitem", { name: /Delete document/i }));
    expect(screen.getByRole("heading", { name: "Delete document" })).toBeTruthy();
    expect(screen.getByText(/can't be undone/i)).toBeTruthy();
  });

  it("re-opening the … menu closes an open Activity popover — they never stack", async () => {
    const user = userEvent.setup();
    renderWorkspace();
    const moreTrigger = screen.getByRole("button", { name: "Document options" });

    // Open Activity from the menu — the menu closes as Activity opens.
    await user.click(moreTrigger);
    await user.click(screen.getByRole("menuitem", { name: /Document activity/i }));
    expect(screen.getByRole("dialog", { name: "Activity on this document" })).toBeTruthy();
    expect(screen.queryByRole("menu")).toBeNull();

    // Click "…" again: the menu reopens and Activity must close, not stack behind it.
    await user.click(moreTrigger);
    expect(screen.getByRole("menu", { name: "Document options" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Activity on this document" })).toBeNull();
  });
});
