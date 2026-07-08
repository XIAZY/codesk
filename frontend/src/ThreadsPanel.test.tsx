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
  localStorage.clear();
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

  it("folds resolved threads by default and shows them on expand", () => {
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

    const foldHeader = container.querySelector(".thread-fold-header");
    expect(foldHeader).toBeTruthy();
    expect(foldHeader?.textContent).toContain("Resolved");
    expect(foldHeader?.textContent).toContain("1");

    expect(container.querySelector(".titem.resolved")).toBeNull();

    fireEvent.click(foldHeader!);

    const card = container.querySelector(".titem.resolved");
    expect(card).toBeTruthy();
    const badge = card?.querySelector(".thread-badge.resolved");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("Resolved");
    expect(badge?.querySelector(".thread-badge-dot")).toBeTruthy();
  });

  it("separates open and resolved threads with resolved folded by default", () => {
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
    expect(container.querySelectorAll(".titem.resolved")).toHaveLength(0);

    const foldHeader = container.querySelector(".thread-fold-header");
    expect(foldHeader?.textContent).toContain("Resolved · 1");

    fireEvent.click(foldHeader!);
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

describe("ThreadsPanel orphan warning", () => {
  it("shows orphan warning on thread card when anchor is lost", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "deleted text" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".titem.orphaned")).toBeTruthy();
    expect(container.querySelector(".thread-orphan-warning")).toBeTruthy();
    expect(container.querySelector(".thread-orphan-warning")?.textContent).toContain("Anchor lost");
    const metaRow = container.querySelector(".titem.orphaned .row.gap-4.tiny.muted");
    expect(metaRow?.textContent).not.toContain("anchored");
  });

  it("shows resolve action on orphan card and fires updateThreadStatus on click", async () => {
    const updateThreadStatus = vi.fn().mockResolvedValue({ thread: threadFixture({ status: "resolved" }) });
    const onReply = vi.fn();
    const { container } = render(
      <ThreadsPanel
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "deleted text" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={onReply}
      />,
    );

    const resolveLink = container.querySelector(".thread-resolve-link");
    expect(resolveLink).toBeTruthy();
    expect(resolveLink?.textContent).toBe("Resolve");

    fireEvent.click(resolveLink!);
    await waitFor(() => expect(updateThreadStatus).toHaveBeenCalledWith("ws", "t1", "resolved"));
    await waitFor(() => expect(onReply).toHaveBeenCalled());
  });

  it("shows orphan warning in detail view for orphaned thread", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "deleted text" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId="t1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".thread-orphan-warning")).toBeTruthy();
    expect(container.querySelector(".quoted-range .tiny")?.textContent).toContain("anchor lost");
  });

  it("does not show orphan warning for non-orphaned anchored thread", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open" })]}
        threadAnchorInfo={{ t1: { orphaned: false, line: 12 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".titem.orphaned")).toBeNull();
    expect(container.querySelector(".thread-orphan-warning")).toBeNull();
  });

  it("shows no orphan warning or re-anchor button when threadAnchorInfo is empty (pre-sync state)", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "some text" } })]}
        threadAnchorInfo={{}}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
        onReanchorThread={vi.fn()}
      />,
    );

    expect(container.querySelector(".titem.orphaned")).toBeNull();
    expect(container.querySelector(".thread-orphan-warning")).toBeNull();
    expect(container.querySelector(".thread-reanchor-link")).toBeNull();
  });

  it("shows no orphan warning when threadAnchorInfo is omitted (pre-mount state)", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "some text" } })]}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".titem.orphaned")).toBeNull();
    expect(container.querySelector(".thread-orphan-warning")).toBeNull();
  });

  it("transitions from no warning to correct state when anchor info arrives", () => {
    const { container, rerender } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[
          threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "alive text" } }),
          threadFixture({ id: "t2", status: "open", anchor: { kind: "range", excerpt: "dead text" } }),
        ]}
        threadAnchorInfo={{}}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".titem.orphaned")).toBeNull();
    expect(container.querySelector(".thread-orphan-warning")).toBeNull();

    rerender(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[
          threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "alive text" } }),
          threadFixture({ id: "t2", status: "open", anchor: { kind: "range", excerpt: "dead text" } }),
        ]}
        threadAnchorInfo={{ t1: { orphaned: false, line: 5 }, t2: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelectorAll(".titem.orphaned")).toHaveLength(1);
    expect(container.querySelector(".thread-orphan-warning")).toBeTruthy();
    expect(container.querySelector(".thread-jump-link")?.textContent).toContain("Jump to line 5");
  });

  it("shows re-anchor link on orphan card when onReanchorThread is provided", () => {
    const onReanchorThread = vi.fn();
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "deleted text" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
        onReanchorThread={onReanchorThread}
      />,
    );

    const reanchorLink = container.querySelector(".thread-reanchor-link");
    expect(reanchorLink).toBeTruthy();
    expect(reanchorLink?.textContent).toBe("Re-anchor");

    fireEvent.click(reanchorLink!);
    expect(onReanchorThread).toHaveBeenCalledWith("t1");
  });

  it("does not show re-anchor link when onReanchorThread is not provided", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "deleted text" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".thread-reanchor-link")).toBeNull();
  });

  it("shows re-anchor button in detail view for orphaned thread", () => {
    const onReanchorThread = vi.fn();
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "range", excerpt: "deleted text" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId="t1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
        onReanchorThread={onReanchorThread}
      />,
    );

    const reanchorBtn = container.querySelector(".thread-orphan-actions .btn.accent");
    expect(reanchorBtn).toBeTruthy();
    expect(reanchorBtn?.textContent).toBe("Re-anchor");

    fireEvent.click(reanchorBtn!);
    expect(onReanchorThread).toHaveBeenCalledWith("t1");
  });
});

describe("ThreadsPanel jump to anchor", () => {
  it("shows jump-to-line link for anchored thread with line info", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open" })]}
        threadAnchorInfo={{ t1: { orphaned: false, line: 42 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    const jumpLink = container.querySelector(".thread-jump-link");
    expect(jumpLink).toBeTruthy();
    expect(jumpLink?.textContent).toContain("Jump to line 42");
  });

  it("calls onJumpToThread when jump link is clicked", () => {
    const onJumpToThread = vi.fn();
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open" })]}
        threadAnchorInfo={{ t1: { orphaned: false, line: 42 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={onJumpToThread}
        onReply={vi.fn()}
      />,
    );

    fireEvent.click(container.querySelector(".thread-jump-link")!);
    expect(onJumpToThread).toHaveBeenCalledWith("t1");
  });

  it("does not show jump link for orphaned thread", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open" })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".thread-jump-link")).toBeNull();
  });

  it("does not show jump link for document-level thread", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "document" } })]}
        threadAnchorInfo={{ t1: { orphaned: false, line: 1 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".thread-jump-link")).toBeNull();
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
