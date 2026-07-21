// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { threadReadStorageKey } from "./threadUnread";
import type { ThreadItem, ThreadMessage } from "./types";
import { useThreadUnread } from "./useThreadUnread";

function message(id: string, authorId: string, createdAt: string): ThreadMessage {
  return {
    id,
    threadId: "thread_1",
    authorId,
    authorType: authorId.startsWith("agent") ? "agent" : "human",
    authorHandle: authorId,
    authorName: authorId,
    body: `body ${id}`,
    kind: "comment",
    createdAt,
  };
}

function thread(messages: ThreadMessage[]): ThreadItem {
  return {
    id: "thread_1",
    documentId: "document_1",
    title: "Retry behavior",
    status: "open",
    anchor: { kind: "document" },
    participantIds: [],
    participantHandles: [],
    messages,
    createdAt: messages[0].createdAt,
    updatedAt: messages[messages.length - 1].createdAt,
  };
}

function Probe({ threads }: { threads: ThreadItem[] }) {
  const unread = useThreadUnread({
    accountId: "account_1",
    workspaceId: "workspace_1",
    currentUserId: "user_me",
    threads,
    snapshotReady: true,
  });
  return (
    <>
      <output aria-label="Unread count">{unread.unreadCount}</output>
      <output aria-label="Unread thread ids">{unread.unread.map((item) => item.threadId).join(",")}</output>
      <button type="button" onClick={() => unread.markThreadRead("thread_1")}>Mark read</button>
    </>
  );
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

describe("useThreadUnread", () => {
  it("baselines the first complete snapshot without flooding existing history", async () => {
    const replied = thread([
      message("m1", "user_me", "2026-07-21T10:00:00Z"),
      message("m2", "agent_1", "2026-07-21T10:01:00Z"),
    ]);

    render(<Probe threads={[replied]} />);

    await waitFor(() => expect(screen.getByLabelText("Unread count").textContent).toBe("0"));
    const stored = JSON.parse(localStorage.getItem(threadReadStorageKey("account_1", "workspace_1"))!);
    expect(stored).toEqual({
      reads: { thread_1: { createdAt: "2026-07-21T10:01:00Z", messageId: "m2" } },
      pending: {},
    });
  });

  it("retains an observed reply when its thread disappears and lets explicit Mark read clear it", async () => {
    const replied = thread([
      message("m1", "user_me", "2026-07-21T10:00:00Z"),
      message("m2", "agent_1", "2026-07-21T10:01:00Z"),
    ]);
    const storageKey = threadReadStorageKey("account_1", "workspace_1");
    localStorage.setItem(storageKey, JSON.stringify({
      thread_1: { createdAt: "2026-07-21T10:00:00Z", messageId: "m1" },
    }));

    const { rerender } = render(<Probe threads={[replied]} />);
    await waitFor(() => expect(screen.getByLabelText("Unread count").textContent).toBe("1"));
    expect(screen.getByLabelText("Unread thread ids").textContent).toBe("thread_1");

    rerender(<Probe threads={[]} />);
    await waitFor(() => expect(screen.getByLabelText("Unread count").textContent).toBe("1"));

    fireEvent.click(screen.getByRole("button", { name: "Mark read" }));
    await waitFor(() => expect(screen.getByLabelText("Unread count").textContent).toBe("0"));
    expect(JSON.parse(localStorage.getItem(storageKey)!)).toEqual({
      reads: { thread_1: { createdAt: "2026-07-21T10:01:00Z", messageId: "m2" } },
      pending: {},
    });
  });
});
