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
  it("uses the open thread's complete stored anchor excerpt in the list header", () => {
    const excerpt = "The original sentence stays intact, including text beyond a visual two-line clamp.\nSecond line remains in the DOM.";
    const orphaned = threadFixture({ anchor: { kind: "text-range", excerpt } });
    orphaned.anchor.resolved = false;
    const resolved = threadFixture({
      id: "thread_resolved",
      status: "resolved",
      anchor: { kind: "text-range", excerpt: "An older resolved anchor" },
    });
    const { container } = render(
      <ThreadPopover
        api={mockApi()}
        workspaceId="ws"
        group={{ line: 5, threads: [resolved, orphaned] }}
        point={{ x: 20, y: 30 }}
        onClose={vi.fn()}
        onThreadsChanged={vi.fn()}
      />,
    );

    const headerExcerpt = container.querySelector(".thread-popover-head-excerpt");
    expect(headerExcerpt?.textContent).toBe(excerpt);
    expect(screen.queryByText("Line 5")).toBeNull();
    expect(screen.getByText("· 1 thread")).toBeTruthy();
  });

  it("lists only open threads and moves list → detail → back without status dots", async () => {
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
        onThreadsChanged={vi.fn()}
      />,
    );

    expect(screen.getByText("can you see me")).toBeTruthy();
    expect(screen.getByText("· 1 thread")).toBeTruthy();
    const rows = container.querySelectorAll(".thread-popover-row");
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain("@ada");
    expect(container.textContent).not.toContain("@lin");
    expect(container.querySelector(".thread-popover-status-dot")).toBeNull();
    expect(screen.queryByRole("button", { name: "Jump to line 5" })).toBeNull();

    await user.click(rows[0] as HTMLElement);
    const dialog = screen.getByRole("dialog", { name: "Thread by @ada" });
    expect(dialog.querySelector(".thread-popover-status-dot")).toBeNull();
    expect(within(dialog).getByText("First comment")).toBeTruthy();
    expect(within(dialog).getByText("Second comment")).toBeTruthy();
    expect(within(dialog).getByText(/can you see me/)).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: "Back to threads on this line" }));
    expect(screen.getByText("can you see me")).toBeTruthy();
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

  it("keeps resolved detail open, then Back returns an empty open-only list", async () => {
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
        onThreadsChanged={vi.fn()}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Open thread by @ada" }));
    await user.click(screen.getByRole("button", { name: "Mark as resolved" }));

    await waitFor(() => expect(updateThreadStatus).toHaveBeenCalledWith("ws", "thread_open", "resolved"));
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Thread by @ada" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reopen thread" })).toBeTruthy();
    expect(document.querySelector(".thread-popover-status-dot")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Back to threads on this line" }));
    expect(screen.getByRole("dialog", { name: "Threads anchored to can you see me" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open thread by @ada" })).toBeNull();
    expect(screen.getByText("· 0 open")).toBeTruthy();
    expect(screen.getByText("No open threads on this line")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Jump to line 5" })).toBeNull();
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

    await user.click(screen.getByRole("button", { name: "Back to threads on this line" }));
    expect(screen.queryByRole("button", { name: "Open thread by @ada" })).toBeNull();
    expect(screen.getByRole("button", { name: "Open thread by @lin" })).toBeTruthy();
  });

  it("omits redundant Jump and supports Escape/outside dismissal", () => {
    const onClose = vi.fn();
    const { rerender } = render(
      <ThreadPopover
        api={mockApi()}
        workspaceId="ws"
        group={{ line: 5, threads: [threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={onClose}
        onThreadsChanged={vi.fn()}
      />,
    );

    expect(screen.queryByRole("button", { name: "Jump to line 5" })).toBeNull();

    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);

    rerender(
      <ThreadPopover
        api={mockApi()}
        workspaceId="ws"
        group={{ line: 5, threads: [threadFixture()] }}
        point={{ x: 20, y: 30 }}
        onClose={onClose}
        onThreadsChanged={vi.fn()}
      />,
    );
    fireEvent.pointerDown(document.body);
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
