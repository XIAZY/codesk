// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThreadPopover } from "./App";
import type { ApiClient } from "./api";
import type { LiveThread } from "./DocumentSurface";
import type { ThreadItem, ThreadMessage } from "./types";

function message(id: string, body: string, overrides: Partial<ThreadMessage> = {}): ThreadMessage {
  return {
    id,
    threadId: "thread_open",
    authorId: "user_1",
    authorType: "human",
    authorHandle: "ada",
    authorName: "Ada",
    body,
    kind: "comment",
    createdAt: "2026-07-10T12:00:00Z",
    ...overrides,
  };
}

function threadFixture(overrides: Partial<ThreadItem> = {}): LiveThread {
  const thread: ThreadItem = {
    id: "thread_open",
    documentId: "doc_1",
    title: "Selection on line 5",
    status: "open",
    anchor: { kind: "text-range", excerpt: "can you see me" },
    createdById: "user_1",
    createdByType: "human",
    createdByHandle: "ada",
    createdByName: "Ada",
    participantIds: ["user_1"],
    participantHandles: ["ada"],
    messages: [message("msg_1", "First comment"), message("msg_2", "Second comment")],
    createdAt: "2026-07-10T12:00:00Z",
    updatedAt: "2026-07-10T12:01:00Z",
    ...overrides,
  };
  return {
    ...thread,
    anchor: {
      ...thread.anchor,
      start: 4,
      end: 18,
      line: 5,
      excerpt: thread.anchor.excerpt || "can you see me",
      resolved: true,
    },
  };
}

function mockApi(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    replyThread: vi.fn().mockResolvedValue({ thread: threadFixture() }),
    updateThreadStatus: vi.fn().mockResolvedValue({ thread: threadFixture({ status: "resolved" }) }),
    ...overrides,
  } as unknown as ApiClient;
}

afterEach(cleanup);

describe("ThreadPopover", () => {
  it("orders open threads before dimmed resolved threads and moves list → detail → back", async () => {
    const user = userEvent.setup();
    const resolved = threadFixture({
      id: "thread_resolved",
      status: "resolved",
      createdByHandle: "lin",
      messages: [message("resolved_msg", "Already handled", { threadId: "thread_resolved", authorHandle: "lin" })],
    });
    const { container } = render(
      <ThreadPopover
        api={mockApi()}
        workspaceId="ws"
        group={{ line: 5, threads: [resolved, threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={vi.fn()}
        onJumpToThread={vi.fn()}
        onThreadsChanged={vi.fn()}
      />,
    );

    expect(screen.getByText("Line 5")).toBeTruthy();
    const rows = container.querySelectorAll(".thread-popover-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("@ada");
    expect(rows[1].textContent).toContain("@lin");
    expect(rows[1].classList.contains("resolved")).toBe(true);

    await user.click(rows[0] as HTMLElement);
    const dialog = screen.getByRole("dialog", { name: "Thread by @ada" });
    expect(within(dialog).getByText("First comment")).toBeTruthy();
    expect(within(dialog).getByText("Second comment")).toBeTruthy();
    expect(within(dialog).getByText(/can you see me/)).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: "Back to threads on this line" }));
    expect(screen.getByText("Line 5")).toBeTruthy();
  });

  it("replies in place through the existing thread API", async () => {
    const user = userEvent.setup();
    const replyThread = vi.fn().mockResolvedValue({
      thread: threadFixture({ messages: [...threadFixture().messages, message("msg_3", "Inline reply")], updatedAt: "2026-07-10T12:02:00Z" }),
    });
    const onThreadsChanged = vi.fn();
    render(
      <ThreadPopover
        api={mockApi({ replyThread })}
        workspaceId="ws"
        group={{ line: 5, threads: [threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={vi.fn()}
        onJumpToThread={vi.fn()}
        onThreadsChanged={onThreadsChanged}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Open thread by @ada" }));
    await user.type(screen.getByPlaceholderText("Reply to this thread…"), "Inline reply");
    await user.click(screen.getByRole("button", { name: "Send reply" }));

    await waitFor(() => expect(replyThread).toHaveBeenCalledWith("ws", "thread_open", "Inline reply"));
    await waitFor(() => expect(screen.getByText("Inline reply")).toBeTruthy());
    expect(onThreadsChanged).toHaveBeenCalledTimes(1);
  });

  it("marks an open thread resolved without closing, then exposes Reopen", async () => {
    const user = userEvent.setup();
    const updateThreadStatus = vi.fn().mockResolvedValue({ thread: threadFixture({ status: "resolved" }) });
    const onClose = vi.fn();
    render(
      <ThreadPopover
        api={mockApi({ updateThreadStatus })}
        workspaceId="ws"
        group={{ line: 5, threads: [threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={onClose}
        onJumpToThread={vi.fn()}
        onThreadsChanged={vi.fn()}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Open thread by @ada" }));
    await user.click(screen.getByRole("button", { name: "Mark as resolved" }));

    await waitFor(() => expect(updateThreadStatus).toHaveBeenCalledWith("ws", "thread_open", "resolved"));
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Thread by @ada" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reopen thread" })).toBeTruthy();
  });

  it("keeps selected detail stable when a status refresh reverses the same thread membership", async () => {
    const user = userEvent.setup();
    const sibling = threadFixture({
      id: "thread_sibling",
      createdByHandle: "lin",
      messages: [message("sibling_msg", "Sibling comment", { threadId: "thread_sibling", authorHandle: "lin" })],
      updatedAt: "2026-07-10T12:00:30Z",
    });
    const commonProps = {
      api: mockApi(),
      workspaceId: "ws",
      point: { x: 20, y: 30 },
      onClose: vi.fn(),
      onJumpToThread: vi.fn(),
      onThreadsChanged: vi.fn(),
    };
    const { rerender } = render(
      <ThreadPopover {...commonProps} group={{ line: 5, threads: [threadFixture(), sibling] }} />,
    );

    await user.click(screen.getByRole("button", { name: "Open thread by @ada" }));
    rerender(
      <ThreadPopover
        {...commonProps}
        group={{
          line: 5,
          threads: [sibling, threadFixture({ status: "resolved", updatedAt: "2026-07-10T12:03:00Z" })],
        }}
      />,
    );

    await waitFor(() => expect(screen.getByRole("button", { name: "Reopen thread" })).toBeTruthy());
    expect(screen.getByRole("dialog", { name: "Thread by @ada" })).toBeTruthy();
    expect(screen.getByText("First comment")).toBeTruthy();
  });

  it("jumps to the anchor and supports Escape/outside dismissal", async () => {
    const user = userEvent.setup();
    const onJumpToThread = vi.fn();
    const onClose = vi.fn();
    const { rerender } = render(
      <ThreadPopover
        api={mockApi()}
        workspaceId="ws"
        group={{ line: 5, threads: [threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={onClose}
        onJumpToThread={onJumpToThread}
        onThreadsChanged={vi.fn()}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Jump to line 5" }));
    expect(onJumpToThread).toHaveBeenCalledWith("thread_open");

    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);

    rerender(
      <ThreadPopover
        api={mockApi()}
        workspaceId="ws"
        group={{ line: 5, threads: [threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={onClose}
        onJumpToThread={onJumpToThread}
        onThreadsChanged={vi.fn()}
      />,
    );
    fireEvent.pointerDown(document.body);
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
