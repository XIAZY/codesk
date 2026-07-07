// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThreadsPanel } from "./App";
import type { ApiClient } from "./api";
import type { ThreadItem } from "./types";

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

function mockApi(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    replyThread: vi.fn().mockResolvedValue({ thread: threadFixture() }),
    updateThreadStatus: vi.fn().mockResolvedValue({ thread: threadFixture({ status: "resolved" }) }),
    ...overrides,
  } as unknown as ApiClient;
}

afterEach(() => {
  cleanup();
});

describe("ThreadsPanel badge rendering", () => {
  it("renders an Open badge with iris dot for open threads", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ status: "open" })]}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    const badge = container.querySelector(".thread-badge.open");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("Open");
    expect(badge?.querySelector(".thread-badge-dot")).toBeTruthy();
  });

  it("renders a Resolved badge with ok dot and dims the card for resolved threads", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ status: "resolved" })]}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    const badge = container.querySelector(".thread-badge.resolved");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("Resolved");
    expect(badge?.querySelector(".thread-badge-dot")).toBeTruthy();

    const card = container.querySelector(".titem.resolved");
    expect(card).toBeTruthy();
  });

  it("renders both open and resolved badges in a mixed thread list", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[
          threadFixture({ id: "t1", status: "open" }),
          threadFixture({ id: "t2", status: "resolved" }),
        ]}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelectorAll(".thread-badge.open")).toHaveLength(1);
    expect(container.querySelectorAll(".thread-badge.resolved")).toHaveLength(1);
    expect(container.querySelectorAll(".titem.resolved")).toHaveLength(1);
  });
});

describe("ThreadsPanel detail view badge and resolve", () => {
  it("shows Open badge and Resolve button in detail view for open thread", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ status: "open" })]}
        selectedThreadId="thread_1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    const badge = container.querySelector(".tdetail .thread-badge.open");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("Open");

    const resolveBtn = screen.getByRole("button", { name: "Resolve thread" });
    expect(resolveBtn).toBeTruthy();
    expect(resolveBtn.textContent).toBe("Resolve");
  });

  it("shows Resolved badge and Reopen button in detail view for resolved thread", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ status: "resolved" })]}
        selectedThreadId="thread_1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    const badge = container.querySelector(".tdetail .thread-badge.resolved");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("Resolved");

    const reopenBtn = screen.getByRole("button", { name: "Reopen thread" });
    expect(reopenBtn).toBeTruthy();
    expect(reopenBtn.textContent).toBe("Reopen");
  });

  it("calls updateThreadStatus on Resolve click and then refreshes via onReply", async () => {
    const user = userEvent.setup();
    const updateThreadStatus = vi.fn().mockResolvedValue({ thread: threadFixture({ status: "resolved" }) });
    const onReply = vi.fn();

    render(
      <ThreadsPanel
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        threads={[threadFixture({ status: "open" })]}
        selectedThreadId="thread_1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={onReply}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Resolve thread" }));

    await waitFor(() => expect(updateThreadStatus).toHaveBeenCalledWith("ws", "thread_1", "resolved"));
    await waitFor(() => expect(onReply).toHaveBeenCalled());
  });

  it("calls updateThreadStatus with 'open' on Reopen click", async () => {
    const user = userEvent.setup();
    const updateThreadStatus = vi.fn().mockResolvedValue({ thread: threadFixture({ status: "open" }) });
    const onReply = vi.fn();

    render(
      <ThreadsPanel
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        threads={[threadFixture({ status: "resolved" })]}
        selectedThreadId="thread_1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={onReply}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Reopen thread" }));

    await waitFor(() => expect(updateThreadStatus).toHaveBeenCalledWith("ws", "thread_1", "open"));
    await waitFor(() => expect(onReply).toHaveBeenCalled());
  });

  it("shows busy state during status toggle", async () => {
    let resolvePromise: (value: unknown) => void;
    const pending = new Promise((resolve) => { resolvePromise = resolve; });
    const updateThreadStatus = vi.fn().mockReturnValue(pending);

    render(
      <ThreadsPanel
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        threads={[threadFixture({ status: "open" })]}
        selectedThreadId="thread_1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    const btn = screen.getByRole("button", { name: "Resolve thread" });
    fireEvent.click(btn);

    await waitFor(() => expect(btn.textContent).toBe("…"));
    expect((btn as HTMLButtonElement).disabled).toBe(true);

    resolvePromise!({ thread: threadFixture({ status: "resolved" }) });
    await waitFor(() => expect(btn.textContent).toBe("Resolve"));
  });

  it("shows inline error on status toggle failure", async () => {
    const user = userEvent.setup();
    const updateThreadStatus = vi.fn().mockRejectedValue(new Error("Network error"));

    const { container } = render(
      <ThreadsPanel
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        threads={[threadFixture({ status: "open" })]}
        selectedThreadId="thread_1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Resolve thread" }));

    await waitFor(() => expect(screen.getByText("Network error")).toBeTruthy());
  });
});

describe("ThreadsPanel empty state", () => {
  it("shows empty note when no threads exist", () => {
    render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[]}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(screen.getByText(/No threads on this document yet/)).toBeTruthy();
  });
});
