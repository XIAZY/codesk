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

describe("ThreadsPanel grouped status rendering", () => {
  it("starts open rows with the avatar and no redundant status dot", () => {
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

    expect(container.querySelector(".thread-list-status-dot")).toBeNull();
    expect(container.querySelector(".titem > .avi")).toBeTruthy();
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
    expect(foldHeader?.querySelector(".thread-badge-dot")).toBeTruthy();
    expect(foldHeader?.textContent).toContain("Resolved");
    expect(foldHeader?.textContent).toContain("1");

    expect(container.querySelector(".titem.resolved")).toBeNull();

    fireEvent.click(foldHeader!);

    const card = container.querySelector(".titem.resolved");
    expect(card).toBeTruthy();
    expect(card?.querySelector(".thread-list-status-dot")).toBeNull();
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

    expect(container.querySelectorAll(".thread-list-status-dot")).toHaveLength(0);
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

    expect(container.querySelector(".tdetail .thread-popover-status-dot.open")).toBeTruthy();
    expect(container.querySelector(".tdetail .thread-popover-detail-head")?.textContent).toContain("open");

    const resolveBtn = screen.getByRole("button", { name: "Mark as resolved" });
    expect(resolveBtn).toBeTruthy();
    expect(resolveBtn.textContent).toContain("Mark as resolved");
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

    expect(container.querySelector(".tdetail .thread-popover-status-dot.resolved")).toBeTruthy();
    expect(container.querySelector(".tdetail .thread-popover-detail-head")?.textContent).toContain("resolved");

    const reopenBtn = screen.getByRole("button", { name: "Reopen thread" });
    expect(reopenBtn).toBeTruthy();
    expect(reopenBtn.textContent).toContain("Reopen");
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

    await user.click(screen.getByRole("button", { name: "Mark as resolved" }));

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

    const btn = screen.getByRole("button", { name: "Mark as resolved" });
    fireEvent.click(btn);

    await waitFor(() => expect(btn.textContent).toContain("Updating…"));
    expect((btn as HTMLButtonElement).disabled).toBe(true);

    resolvePromise!({ thread: threadFixture({ status: "resolved" }) });
    await waitFor(() => expect(btn.textContent).toContain("Mark as resolved"));
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

    await user.click(screen.getByRole("button", { name: "Mark as resolved" }));

    await waitFor(() => expect(screen.getByText("Network error")).toBeTruthy());
  });
});

describe("ThreadsPanel obsolete grouping", () => {
  it("folds definitively orphaned open threads into Obsolete without warning copy", () => {
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

    const foldHeader = screen.getByRole("button", { name: /Obsolete · 1/ });
    expect(foldHeader.getAttribute("aria-expanded")).toBe("false");
    expect(foldHeader.querySelector(".thread-badge-dot")).toBeTruthy();
    expect(container.querySelector(".titem.obsolete")).toBeNull();
    expect(container.textContent).not.toMatch(/anchor lost|original text deleted/i);

    fireEvent.click(foldHeader);
    const card = container.querySelector(".titem.obsolete");
    expect(card).toBeTruthy();
    expect(card?.querySelector(".thread-list-status-dot")).toBeNull();
    expect(card?.textContent).not.toMatch(/anchor lost|original text deleted|jump to line/i);
  });

  it("keeps unknown pre-sync range threads in Open until anchor info arrives", () => {
    const threads = [threadFixture({ id: "t1", status: "open" })];
    const updateThreadStatus = vi.fn();
    const props = {
      api: mockApi({ updateThreadStatus }),
      workspaceId: "ws",
      threads,
      selectedThreadId: "",
      onSelectThread: vi.fn(),
      onJumpToThread: vi.fn(),
      onReply: vi.fn(),
    };
    const { container, rerender } = render(<ThreadsPanel {...props} threadAnchorInfo={{}} />);

    expect(container.querySelector(".titem .thread-list-status-dot")).toBeNull();
    expect(screen.queryByRole("button", { name: /Obsolete/ })).toBeNull();
    expect(container.querySelector(".titem")?.textContent).not.toMatch(/document|anchored|jump to line/i);

    rerender(<ThreadsPanel {...props} threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }} />);
    expect(container.querySelector(".titem .thread-list-status-dot")).toBeNull();
    expect(screen.getByRole("button", { name: /Obsolete · 1/ })).toBeTruthy();
    expect(updateThreadStatus).not.toHaveBeenCalled();
  });

  it("keeps resolved status in Resolved even when the anchor is orphaned", () => {
    render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "resolved" })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(screen.getByRole("button", { name: /Resolved · 1/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Obsolete/ })).toBeNull();
  });

  it("keeps document-level open threads in Open even if stale classifier data says orphaned", () => {
    const { container } = render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open", anchor: { kind: "document" } })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId=""
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(container.querySelector(".titem .thread-list-status-dot")).toBeNull();
    expect(screen.queryByRole("button", { name: /Obsolete/ })).toBeNull();
  });

  it("remembers when the Obsolete fold is expanded", () => {
    const props = {
      api: mockApi(),
      workspaceId: "ws",
      threads: [threadFixture({ id: "t1", status: "open" })],
      threadAnchorInfo: { t1: { orphaned: true, line: 0 } },
      selectedThreadId: "",
      onSelectThread: vi.fn(),
      onJumpToThread: vi.fn(),
      onReply: vi.fn(),
    };
    const { unmount } = render(<ThreadsPanel {...props} />);
    fireEvent.click(screen.getByRole("button", { name: /Obsolete · 1/ }));
    expect(localStorage.getItem("codesk.threads.obsoleteFolded")).toBe("false");
    unmount();

    render(<ThreadsPanel {...props} />);
    expect(screen.getByRole("button", { name: /Obsolete · 1/ }).getAttribute("aria-expanded")).toBe("true");
    expect(document.querySelector(".titem.obsolete")).toBeTruthy();
  });

  it("opens an obsolete thread detail with manual Resolve and no lost-anchor wording", async () => {
    const user = userEvent.setup();
    const updateThreadStatus = vi.fn().mockResolvedValue({ thread: threadFixture({ status: "resolved" }) });
    render(
      <ThreadsPanel
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open" })]}
        threadAnchorInfo={{ t1: { orphaned: true, line: 0 } }}
        selectedThreadId="t1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={vi.fn()}
      />,
    );

    expect(document.querySelector(".tdetail .thread-popover-status-dot.obsolete")).toBeTruthy();
    expect(document.querySelector(".tdetail .thread-popover-detail-head")?.textContent).toContain("obsolete");
    expect(document.body.textContent).not.toMatch(/anchor lost|original text deleted/i);
    await user.click(screen.getByRole("button", { name: "Mark as resolved" }));
    await waitFor(() => expect(updateThreadStatus).toHaveBeenCalledWith("ws", "t1", "resolved"));
  });

  it("reuses the Phase 1 reply composer and refreshes after a trimmed reply", async () => {
    const user = userEvent.setup();
    const replyThread = vi.fn().mockResolvedValue({ thread: threadFixture() });
    const onReply = vi.fn();
    render(
      <ThreadsPanel
        api={mockApi({ replyThread })}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1" })]}
        selectedThreadId="t1"
        onSelectThread={vi.fn()}
        onJumpToThread={vi.fn()}
        onReply={onReply}
      />,
    );

    const composer = screen.getByLabelText("Reply to this thread");
    await user.type(composer, "  Follow up  ");
    await user.click(screen.getByRole("button", { name: "Send reply" }));
    await waitFor(() => expect(replyThread).toHaveBeenCalledWith("ws", "t1", "Follow up"));
    expect(onReply).toHaveBeenCalled();
  });
});

describe("ThreadsPanel jump to anchor", () => {
  it("keeps anchored list-row metadata to the reply count", () => {
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

    const row = container.querySelector(".titem");
    expect(row?.textContent).toContain("No replies");
    expect(row?.textContent).not.toMatch(/anchored|document|jump to line/i);
    expect(container.querySelector(".thread-jump-link")).toBeNull();
  });

  it("keeps jump-to-line navigation in the thread detail", () => {
    const onJumpToThread = vi.fn();
    render(
      <ThreadsPanel
        api={mockApi()}
        workspaceId="ws"
        threads={[threadFixture({ id: "t1", status: "open" })]}
        threadAnchorInfo={{ t1: { orphaned: false, line: 42 } }}
        selectedThreadId="t1"
        onSelectThread={vi.fn()}
        onJumpToThread={onJumpToThread}
        onReply={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Jump to line 42" }));
    expect(onJumpToThread).toHaveBeenCalledWith("t1");
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
