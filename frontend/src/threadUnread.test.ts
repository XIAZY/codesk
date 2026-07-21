import { describe, expect, it } from "vitest";
import {
  baselineThreadNotificationState,
  baselineThreadReads,
  loadThreadNotificationState,
  markThreadNotificationRead,
  pendingThreadReplies,
  reconcileThreadNotificationState,
  saveThreadNotificationState,
  threadReadStorageKey,
  unreadThreadReplies,
} from "./threadUnread";
import type { ThreadItem, ThreadMessage } from "./types";

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

function thread(id: string, messages: ThreadMessage[]): ThreadItem {
  return {
    id,
    documentId: `doc_${id}`,
    title: `Thread ${id}`,
    status: "open",
    anchor: { kind: "document", excerpt: "" },
    participantIds: [],
    participantHandles: [],
    messages: messages.map((item) => ({ ...item, threadId: id })),
    createdAt: messages[0]?.createdAt ?? "2026-07-21T10:00:00Z",
    updatedAt: messages[messages.length - 1]?.createdAt ?? "2026-07-21T10:00:00Z",
  };
}

function memoryStorage(): Storage {
  const values = new Map<string, string>();
  return {
    get length() {
      return values.size;
    },
    clear: () => values.clear(),
    getItem: (key) => values.get(key) ?? null,
    key: (index) => Array.from(values.keys())[index] ?? null,
    removeItem: (key) => {
      values.delete(key);
    },
    setItem: (key, value) => {
      values.set(key, value);
    },
  };
}

describe("thread unread replies", () => {
  it("counts only non-self replies after the opening message and watermark", () => {
    const item = thread("thread_1", [
      message("m1", "user_me", "2026-07-21T10:00:00Z"),
      message("m2", "agent_1", "2026-07-21T10:01:00Z"),
      message("m3", "user_me", "2026-07-21T10:02:00Z"),
      message("m4", "user_other", "2026-07-21T10:03:00Z"),
    ]);

    expect(unreadThreadReplies([item], "user_me", {
      thread_1: { createdAt: "2026-07-21T10:00:00Z", messageId: "m1" },
    })).toEqual([{
      threadId: "thread_1",
      documentId: "doc_thread_1",
      count: 2,
      newestMessage: item.messages[3],
    }]);

    expect(unreadThreadReplies([item], "user_me", {
      thread_1: { createdAt: "2026-07-21T10:02:00Z", messageId: "m3" },
    })[0]?.count).toBe(1);
  });

  it("never treats a thread opening message as an unread reply", () => {
    const item = thread("new_thread", [message("opening", "agent_1", "2026-07-21T11:00:00Z")]);
    expect(unreadThreadReplies([item], "user_me", {})).toEqual([]);
  });

  it("uses the message id to order replies with identical timestamps", () => {
    const item = thread("thread_1", [
      message("m1", "user_me", "2026-07-21T10:00:00Z"),
      message("m2", "agent_1", "2026-07-21T10:01:00Z"),
      message("m3", "agent_1", "2026-07-21T10:01:00Z"),
    ]);
    const unread = unreadThreadReplies([item], "user_me", {
      thread_1: { createdAt: "2026-07-21T10:01:00Z", messageId: "m2" },
    });
    expect(unread).toHaveLength(1);
    expect(unread[0].count).toBe(1);
    expect(unread[0].newestMessage.id).toBe("m3");
  });

  it("baselines existing history and clears only the exact thread that was viewed", () => {
    const first = thread("first", [
      message("f1", "user_me", "2026-07-21T10:00:00Z"),
      message("f2", "agent_1", "2026-07-21T10:01:00Z"),
    ]);
    const second = thread("second", [
      message("s1", "user_me", "2026-07-21T10:00:00Z"),
      message("s2", "agent_2", "2026-07-21T10:02:00Z"),
    ]);

    const baseline = baselineThreadReads([first, second]);
    expect(unreadThreadReplies([first, second], "user_me", baseline)).toEqual([]);

    const advancedFirst = thread("first", [...first.messages, message("f3", "agent_1", "2026-07-21T10:03:00Z")]);
    const advancedSecond = thread("second", [...second.messages, message("s3", "agent_2", "2026-07-21T10:04:00Z")]);
    const bothUnread = unreadThreadReplies([advancedFirst, advancedSecond], "user_me", baseline);
    expect(bothUnread.map((item) => item.threadId)).toEqual(["second", "first"]);

    const notificationState = reconcileThreadNotificationState(
      { reads: baseline, pending: {} },
      [advancedFirst, advancedSecond],
      "user_me",
    );
    const afterViewingFirst = markThreadNotificationRead(notificationState, "first", advancedFirst);
    expect(pendingThreadReplies(afterViewingFirst).map((item) => item.threadId)).toEqual(["second"]);
    expect(afterViewingFirst.reads.second).toEqual(baseline.second);
  });

  it("retains an observed unread reply after its thread disappears until explicit acknowledgement", () => {
    const initial = thread("thread_1", [
      message("m1", "user_me", "2026-07-21T10:00:00Z"),
    ]);
    const replied = thread("thread_1", [
      ...initial.messages,
      message("m2", "agent_1", "2026-07-21T10:01:00Z"),
    ]);

    const baseline = baselineThreadNotificationState([initial]);
    const observed = reconcileThreadNotificationState(baseline, [replied], "user_me");
    expect(pendingThreadReplies(observed)).toEqual([{
      threadId: "thread_1",
      documentId: "doc_thread_1",
      count: 1,
      newestMessage: replied.messages[1],
    }]);

    const removed = reconcileThreadNotificationState(observed, [], "user_me");
    expect(removed).toBe(observed);
    expect(pendingThreadReplies(removed)).toHaveLength(1);

    const acknowledged = markThreadNotificationRead(removed, "thread_1");
    expect(pendingThreadReplies(acknowledged)).toEqual([]);
    expect(acknowledged.reads.thread_1).toEqual({
      createdAt: "2026-07-21T10:01:00Z",
      messageId: "m2",
    });
    expect(pendingThreadReplies(
      reconcileThreadNotificationState(acknowledged, [replied], "user_me"),
    )).toEqual([]);
  });
});

describe("thread notification storage", () => {
  it("isolates versioned retained state by account and workspace", () => {
    const storage = memoryStorage();
    const item = thread("thread_1", [
      message("m1", "user_me", "2026-07-21T10:00:00Z"),
      message("m2", "agent_1", "2026-07-21T10:01:00Z"),
    ]);
    const state = reconcileThreadNotificationState(
      { reads: { thread_1: { createdAt: "2026-07-21T10:00:00Z", messageId: "m1" } }, pending: {} },
      [item],
      "user_me",
    );
    saveThreadNotificationState(storage, "account/a", "workspace/a", state);

    expect(loadThreadNotificationState(storage, "account/a", "workspace/a")).toEqual(state);
    expect(loadThreadNotificationState(storage, "account/b", "workspace/a")).toBeNull();
    expect(loadThreadNotificationState(storage, "account/a", "workspace/b")).toBeNull();
    expect(threadReadStorageKey("account/a", "workspace/a")).toContain("v1");
  });

  it("migrates the legacy read-map-only format without manufacturing unread replies", () => {
    const storage = memoryStorage();
    storage.setItem(threadReadStorageKey("account", "workspace"), JSON.stringify({
      thread_1: { createdAt: "2026-07-21T10:00:00Z", messageId: "m1" },
    }));

    expect(loadThreadNotificationState(storage, "account", "workspace")).toEqual({
      reads: { thread_1: { createdAt: "2026-07-21T10:00:00Z", messageId: "m1" } },
      pending: {},
    });
  });

  it("fails closed to a missing baseline for malformed or unavailable storage", () => {
    const storage = memoryStorage();
    storage.setItem(threadReadStorageKey("account", "workspace"), "{broken");
    expect(loadThreadNotificationState(storage, "account", "workspace")).toBeNull();

    storage.setItem(threadReadStorageKey("account", "workspace"), JSON.stringify({
      reads: {},
      pending: { thread_1: { threadId: "thread_1", count: -1 } },
    }));
    expect(loadThreadNotificationState(storage, "account", "workspace")).toBeNull();

    const unavailable = {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
    };
    expect(loadThreadNotificationState(unavailable, "account", "workspace")).toBeNull();
    expect(() => saveThreadNotificationState(unavailable, "account", "workspace", {
      reads: {},
      pending: {},
    })).not.toThrow();
  });
});
