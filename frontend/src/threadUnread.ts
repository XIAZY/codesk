import type { ThreadItem, ThreadMessage } from "./types";

export type ThreadReadWatermark = {
  createdAt: string;
  messageId: string;
};

export type ThreadReadState = Record<string, ThreadReadWatermark>;

export type UnreadThreadReply = {
  threadId: string;
  documentId: string;
  count: number;
  newestMessage: ThreadMessage;
};

export type ThreadNotificationState = {
  reads: ThreadReadState;
  pending: Record<string, UnreadThreadReply>;
};

type ThreadNotificationStorage = Pick<Storage, "getItem" | "setItem">;

const storagePrefix = "codesk.threadReads.v1";

export function threadReadStorageKey(accountId: string, workspaceId: string) {
  return `${storagePrefix}.${encodeURIComponent(accountId)}.${encodeURIComponent(workspaceId)}`;
}

function compareWatermarks(left: ThreadReadWatermark, right: ThreadReadWatermark) {
  const leftTime = Date.parse(left.createdAt);
  const rightTime = Date.parse(right.createdAt);
  if (Number.isFinite(leftTime) && Number.isFinite(rightTime) && leftTime !== rightTime) {
    return leftTime - rightTime;
  }
  if (left.createdAt !== right.createdAt) {
    return left.createdAt.localeCompare(right.createdAt);
  }
  return left.messageId.localeCompare(right.messageId);
}

function messageWatermark(message: ThreadMessage): ThreadReadWatermark {
  return { createdAt: message.createdAt, messageId: message.id };
}

function newestMessage(thread: ThreadItem) {
  return thread.messages.reduce<ThreadMessage | null>((latest, message) => {
    if (!latest || compareWatermarks(messageWatermark(message), messageWatermark(latest)) > 0) {
      return message;
    }
    return latest;
  }, null);
}

export function baselineThreadReads(threads: ThreadItem[]): ThreadReadState {
  const state: ThreadReadState = {};
  for (const thread of threads) {
    const latest = newestMessage(thread);
    if (latest) {
      state[thread.id] = messageWatermark(latest);
    }
  }
  return state;
}

export function baselineThreadNotificationState(threads: ThreadItem[]): ThreadNotificationState {
  return { reads: baselineThreadReads(threads), pending: {} };
}

function advanceThreadRead(state: ThreadReadState, threadId: string, next: ThreadReadWatermark) {
  const current = state[threadId];
  if (current && compareWatermarks(current, next) >= 0) {
    return state;
  }
  return { ...state, [threadId]: next };
}

export function unreadThreadReplies(
  threads: ThreadItem[],
  currentUserId: string,
  state: ThreadReadState,
): UnreadThreadReply[] {
  const unread: UnreadThreadReply[] = [];
  for (const thread of threads) {
    const watermark = state[thread.id];
    const replies = thread.messages.slice(1).filter((message) => {
      if (message.authorId === currentUserId) {
        return false;
      }
      return !watermark || compareWatermarks(messageWatermark(message), watermark) > 0;
    });
    if (!replies.length) {
      continue;
    }
    replies.sort((left, right) => compareWatermarks(messageWatermark(left), messageWatermark(right)));
    unread.push({
      threadId: thread.id,
      documentId: thread.documentId,
      count: replies.length,
      newestMessage: replies[replies.length - 1],
    });
  }
  return sortUnreadThreadReplies(unread);
}

function sortUnreadThreadReplies(unread: UnreadThreadReply[]) {
  return unread.sort((left, right) => (
    compareWatermarks(messageWatermark(right.newestMessage), messageWatermark(left.newestMessage))
  ));
}

function sameThreadMessage(left: ThreadMessage, right: ThreadMessage) {
  return left.id === right.id
    && left.threadId === right.threadId
    && left.authorId === right.authorId
    && left.authorType === right.authorType
    && left.authorHandle === right.authorHandle
    && left.authorName === right.authorName
    && left.body === right.body
    && left.kind === right.kind
    && left.createdAt === right.createdAt;
}

function sameUnreadThreadReply(left: UnreadThreadReply, right: UnreadThreadReply) {
  return left.threadId === right.threadId
    && left.documentId === right.documentId
    && left.count === right.count
    && sameThreadMessage(left.newestMessage, right.newestMessage);
}

function samePendingReplies(
  left: Record<string, UnreadThreadReply>,
  right: Record<string, UnreadThreadReply>,
) {
  const leftKeys = Object.keys(left);
  const rightKeys = Object.keys(right);
  return leftKeys.length === rightKeys.length
    && leftKeys.every((threadId) => right[threadId] && sameUnreadThreadReply(left[threadId], right[threadId]));
}

export function reconcileThreadNotificationState(
  state: ThreadNotificationState,
  threads: ThreadItem[],
  currentUserId: string,
): ThreadNotificationState {
  const liveThreadIds = new Set(threads.map((thread) => thread.id));
  const pending: Record<string, UnreadThreadReply> = {};

  // Once observed, an unread reply survives a later document/thread removal so
  // failed navigation cannot silently acknowledge it.
  for (const [threadId, reply] of Object.entries(state.pending)) {
    if (!liveThreadIds.has(threadId)) {
      pending[threadId] = reply;
    }
  }
  for (const reply of unreadThreadReplies(threads, currentUserId, state.reads)) {
    pending[reply.threadId] = reply;
  }

  if (samePendingReplies(state.pending, pending)) {
    return state;
  }
  return { ...state, pending };
}

export function pendingThreadReplies(state: ThreadNotificationState): UnreadThreadReply[] {
  return sortUnreadThreadReplies(Object.values(state.pending));
}

export function markThreadNotificationRead(
  state: ThreadNotificationState,
  threadId: string,
  thread?: ThreadItem,
): ThreadNotificationState {
  const pending = state.pending[threadId];
  const latest = thread ? newestMessage(thread) : pending?.newestMessage ?? null;
  let reads = state.reads;
  if (latest) {
    reads = advanceThreadRead(reads, threadId, messageWatermark(latest));
  }
  if (!pending && reads === state.reads) {
    return state;
  }
  const nextPending = { ...state.pending };
  delete nextPending[threadId];
  return { reads, pending: nextPending };
}

function isWatermark(value: unknown): value is ThreadReadWatermark {
  if (!value || typeof value !== "object") {
    return false;
  }
  const candidate = value as Partial<ThreadReadWatermark>;
  return typeof candidate.createdAt === "string"
    && candidate.createdAt.length > 0
    && typeof candidate.messageId === "string"
    && candidate.messageId.length > 0;
}

function parseReadState(value: unknown): ThreadReadState | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  const state: ThreadReadState = {};
  for (const [threadId, watermark] of Object.entries(value)) {
    if (!threadId || !isWatermark(watermark)) {
      return null;
    }
    state[threadId] = watermark;
  }
  return state;
}

function isThreadMessage(value: unknown): value is ThreadMessage {
  if (!value || typeof value !== "object") {
    return false;
  }
  const candidate = value as Partial<ThreadMessage>;
  return typeof candidate.id === "string" && candidate.id.length > 0
    && typeof candidate.threadId === "string" && candidate.threadId.length > 0
    && typeof candidate.authorId === "string"
    && typeof candidate.authorType === "string"
    && typeof candidate.authorHandle === "string"
    && typeof candidate.authorName === "string"
    && typeof candidate.body === "string"
    && typeof candidate.kind === "string"
    && typeof candidate.createdAt === "string" && candidate.createdAt.length > 0;
}

function parsePendingReplies(value: unknown): Record<string, UnreadThreadReply> | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  const pending: Record<string, UnreadThreadReply> = {};
  for (const [threadId, rawReply] of Object.entries(value)) {
    if (!threadId || !rawReply || typeof rawReply !== "object") {
      return null;
    }
    const reply = rawReply as Partial<UnreadThreadReply>;
    if (reply.threadId !== threadId
      || typeof reply.documentId !== "string"
      || reply.documentId.length === 0
      || !Number.isSafeInteger(reply.count)
      || (reply.count ?? 0) <= 0
      || !isThreadMessage(reply.newestMessage)
      || reply.newestMessage.threadId !== threadId) {
      return null;
    }
    pending[threadId] = reply as UnreadThreadReply;
  }
  return pending;
}

export function loadThreadNotificationState(
  storage: ThreadNotificationStorage,
  accountId: string,
  workspaceId: string,
): ThreadNotificationState | null {
  try {
    const raw = storage.getItem(threadReadStorageKey(accountId, workspaceId));
    if (raw === null) {
      return null;
    }
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return null;
    }
    const candidate = parsed as Partial<ThreadNotificationState>;
    if (Object.prototype.hasOwnProperty.call(candidate, "reads")
      || Object.prototype.hasOwnProperty.call(candidate, "pending")) {
      const reads = parseReadState(candidate.reads);
      const pending = parsePendingReplies(candidate.pending);
      return reads && pending ? { reads, pending } : null;
    }

    // Migrate the first browser-local format, which stored only the read map.
    const reads = parseReadState(parsed);
    return reads ? { reads, pending: {} } : null;
  } catch {
    return null;
  }
}

export function saveThreadNotificationState(
  storage: ThreadNotificationStorage,
  accountId: string,
  workspaceId: string,
  state: ThreadNotificationState,
) {
  try {
    storage.setItem(threadReadStorageKey(accountId, workspaceId), JSON.stringify(state));
  } catch {
    // Keep the in-memory notification state usable when browser storage is unavailable.
  }
}
