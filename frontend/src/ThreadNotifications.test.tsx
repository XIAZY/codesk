// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThreadNotifications } from "./App";
import type { UnreadThreadReply } from "./threadUnread";
import type { DocumentItem, ThreadItem, ThreadMessage } from "./types";

function message(overrides: Partial<ThreadMessage> = {}): ThreadMessage {
  return {
    id: "message_2",
    threadId: "thread_1",
    authorId: "agent_1",
    authorType: "agent",
    authorHandle: "reviewer",
    authorName: "Reviewer",
    body: "I found a stale edge case in the retry path.",
    kind: "comment",
    createdAt: "2026-07-21T11:55:00Z",
    ...overrides,
  };
}

function unread(overrides: Partial<UnreadThreadReply> = {}): UnreadThreadReply {
  return {
    threadId: "thread_1",
    documentId: "document_1",
    count: 2,
    newestMessage: message(),
    ...overrides,
  };
}

function threadItem(): ThreadItem {
  return {
    id: "thread_1",
    documentId: "document_1",
    title: "Retry policy",
    status: "open",
    anchor: { kind: "document" },
    participantIds: [],
    participantHandles: [],
    messages: [message()],
    createdAt: "2026-07-21T11:50:00Z",
    updatedAt: "2026-07-21T11:55:00Z",
  };
}

const documentItem: DocumentItem = {
  id: "document_1",
  path: "Specs/Retry policy.md",
  title: "Retry policy.md",
};

afterEach(() => {
  vi.useRealTimers();
  cleanup();
});

describe("ThreadNotifications", () => {
  it("keeps a constant bell trigger and exposes the aggregate unread reply count", () => {
    const onToggle = vi.fn();
    const { rerender } = render(
      <ThreadNotifications
        open={false}
        unread={[]}
        unreadCount={0}
        documents={[]}
        threads={[]}
        nowMs={Date.parse("2026-07-21T12:00:00Z")}
        onToggle={onToggle}
        onClose={vi.fn()}
        onOpenThread={vi.fn()}
        onMarkRead={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "No unread thread replies" }));
    expect(onToggle).toHaveBeenCalledTimes(1);

    rerender(
      <ThreadNotifications
        open={false}
        unread={[unread({ count: 103 })]}
        unreadCount={103}
        documents={[documentItem]}
        threads={[threadItem()]}
        nowMs={Date.parse("2026-07-21T12:00:00Z")}
        onToggle={onToggle}
        onClose={vi.fn()}
        onOpenThread={vi.fn()}
        onMarkRead={vi.fn()}
      />,
    );

    const trigger = screen.getByRole("button", { name: "103 unread thread replies" });
    expect(within(trigger).getByText("99+")).toBeTruthy();
  });

  it("groups each thread into one actionable row with author, document, preview, time, and N-new metadata", async () => {
    const user = userEvent.setup();
    const onOpenThread = vi.fn();
    render(
      <ThreadNotifications
        open
        unread={[unread()]}
        unreadCount={2}
        documents={[documentItem]}
        threads={[threadItem()]}
        nowMs={Date.parse("2026-07-21T12:00:00Z")}
        onToggle={vi.fn()}
        onClose={vi.fn()}
        onOpenThread={onOpenThread}
        onMarkRead={vi.fn()}
      />,
    );

    const dialog = screen.getByRole("dialog", { name: "Unread thread replies" });
    expect(within(dialog).getByText("@reviewer")).toBeTruthy();
    expect(within(dialog).getByText("Specs/Retry policy.md")).toBeTruthy();
    expect(within(dialog).getByText("I found a stale edge case in the retry path.")).toBeTruthy();
    expect(within(dialog).getByText("2 new")).toBeTruthy();
    expect(within(dialog).getByText(/minute/)).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: "Open 2 new replies in Specs/Retry policy.md" }));
    expect(onOpenThread).toHaveBeenCalledWith("thread_1", "document_1");
  });

  it("does not offer navigation for an unavailable document and clears only on explicit Mark read", async () => {
    const user = userEvent.setup();
    const onOpenThread = vi.fn();
    const onMarkRead = vi.fn();
    render(
      <ThreadNotifications
        open
        unread={[unread()]}
        unreadCount={2}
        documents={[]}
        threads={[threadItem()]}
        nowMs={Date.parse("2026-07-21T12:00:00Z")}
        onToggle={vi.fn()}
        onClose={vi.fn()}
        onOpenThread={onOpenThread}
        onMarkRead={onMarkRead}
      />,
    );

    expect(screen.getByText("Document unavailable")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Open 2 new replies/ })).toBeNull();
    expect(onMarkRead).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Mark read" }));
    expect(onMarkRead).toHaveBeenCalledWith("thread_1");
    expect(onOpenThread).not.toHaveBeenCalled();
  });

  it("keeps a retained reply explicit when the document exists but its thread no longer does", async () => {
    const user = userEvent.setup();
    const onMarkRead = vi.fn();
    render(
      <ThreadNotifications
        open
        unread={[unread()]}
        unreadCount={2}
        documents={[documentItem]}
        threads={[]}
        nowMs={Date.parse("2026-07-21T12:00:00Z")}
        onToggle={vi.fn()}
        onClose={vi.fn()}
        onOpenThread={vi.fn()}
        onMarkRead={onMarkRead}
      />,
    );

    expect(screen.getByText("Thread unavailable")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Open 2 new replies/ })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Mark read" }));
    expect(onMarkRead).toHaveBeenCalledWith("thread_1");
  });

  it("closes on Escape and restores focus to the bell without marking anything read", async () => {
    const onClose = vi.fn();
    const onMarkRead = vi.fn();
    render(
      <ThreadNotifications
        open
        unread={[unread()]}
        unreadCount={2}
        documents={[documentItem]}
        threads={[threadItem()]}
        nowMs={Date.parse("2026-07-21T12:00:00Z")}
        onToggle={vi.fn()}
        onClose={onClose}
        onOpenThread={vi.fn()}
        onMarkRead={onMarkRead}
      />,
    );

    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close new replies" })));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "2 unread thread replies" })));
    expect(onMarkRead).not.toHaveBeenCalled();
  });
});
