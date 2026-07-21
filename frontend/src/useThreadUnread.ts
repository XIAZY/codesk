import { useCallback, useEffect, useMemo, useState } from "react";
import {
  baselineThreadNotificationState,
  loadThreadNotificationState,
  markThreadNotificationRead,
  pendingThreadReplies,
  reconcileThreadNotificationState,
  saveThreadNotificationState,
  threadReadStorageKey,
  type ThreadNotificationState,
} from "./threadUnread";
import type { ThreadItem } from "./types";

type ScopedNotificationState = {
  scopeKey: string;
  ready: boolean;
  value: ThreadNotificationState;
};

const emptyNotificationState = (): ThreadNotificationState => ({ reads: {}, pending: {} });

function browserStorage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

export function useThreadUnread({
  accountId,
  workspaceId,
  currentUserId,
  threads,
  snapshotReady,
}: {
  accountId: string;
  workspaceId: string;
  currentUserId: string;
  threads: ThreadItem[];
  snapshotReady: boolean;
}) {
  const scopeKey = `${accountId}\0${workspaceId}`;
  const [stored, setStored] = useState<ScopedNotificationState>({
    scopeKey: "",
    ready: false,
    value: emptyNotificationState(),
  });

  useEffect(() => {
    if (!snapshotReady || !accountId || !workspaceId || !currentUserId) {
      return;
    }
    const storage = browserStorage();
    const loaded = storage ? loadThreadNotificationState(storage, accountId, workspaceId) : null;
    const initial = loaded ?? baselineThreadNotificationState(threads);
    const value = loaded
      ? reconcileThreadNotificationState(initial, threads, currentUserId)
      : initial;
    setStored({ scopeKey, ready: true, value });
    if (storage) {
      // Also rewrites the legacy read-map-only format into the retained snapshot schema.
      saveThreadNotificationState(storage, accountId, workspaceId, value);
    }
    // This effect intentionally initializes once per ready scope. Later thread
    // changes reconcile against the captured watermarks instead of rebasing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [accountId, currentUserId, scopeKey, snapshotReady, workspaceId]);

  useEffect(() => {
    if (!snapshotReady || !accountId || !workspaceId || !currentUserId) {
      return;
    }
    setStored((current) => {
      if (!current.ready || current.scopeKey !== scopeKey) {
        return current;
      }
      const value = reconcileThreadNotificationState(current.value, threads, currentUserId);
      if (value === current.value) {
        return current;
      }
      const storage = browserStorage();
      if (storage) {
        saveThreadNotificationState(storage, accountId, workspaceId, value);
      }
      return { ...current, value };
    });
  }, [accountId, currentUserId, scopeKey, snapshotReady, threads, workspaceId]);

  useEffect(() => {
    if (!snapshotReady || !accountId || !workspaceId || !currentUserId) {
      return;
    }
    const key = threadReadStorageKey(accountId, workspaceId);
    const onStorage = (event: StorageEvent) => {
      if (event.key !== key) {
        return;
      }
      const storage = browserStorage();
      if (!storage) {
        return;
      }
      const loaded = loadThreadNotificationState(storage, accountId, workspaceId);
      if (!loaded) {
        return;
      }
      const value = reconcileThreadNotificationState(loaded, threads, currentUserId);
      setStored({ scopeKey, ready: true, value });
      if (value !== loaded) {
        saveThreadNotificationState(storage, accountId, workspaceId, value);
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [accountId, currentUserId, scopeKey, snapshotReady, threads, workspaceId]);

  const ready = stored.ready && stored.scopeKey === scopeKey;
  const notificationState = ready ? stored.value : emptyNotificationState();
  const unread = useMemo(
    () => (ready ? pendingThreadReplies(notificationState) : []),
    [notificationState, ready],
  );

  const markThreadRead = useCallback((threadId: string) => {
    const thread = threads.find((item) => item.id === threadId);
    setStored((current) => {
      if (!current.ready || current.scopeKey !== scopeKey) {
        return current;
      }
      const value = markThreadNotificationRead(current.value, threadId, thread);
      if (value === current.value) {
        return current;
      }
      const storage = browserStorage();
      if (storage) {
        saveThreadNotificationState(storage, accountId, workspaceId, value);
      }
      return { ...current, value };
    });
  }, [accountId, scopeKey, threads, workspaceId]);

  return {
    ready,
    unread,
    unreadCount: unread.reduce((sum, item) => sum + item.count, 0),
    markThreadRead,
  };
}
