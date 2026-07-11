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

    const railTabs = container.querySelector(".ctx-tabs") as HTMLElement;
    expect(within(railTabs).queryByRole("button", { name: /Threads/i })).toBeNull();
    expect(within(railTabs).getByRole("button", { name: /Document Activity/i })).toBeTruthy();
    expect(within(railTabs).getByRole("button", { name: /Participants/i })).toBeTruthy();

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
});
