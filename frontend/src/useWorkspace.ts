import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { ApiClient, workspaceWsUrl } from "./api";
import { emptyWorkspace, reduceWorkspaceEvent, stampDaemonReceipt } from "./logic";
import type { WorkspaceEvent, WorkspaceState } from "./types";

export function useWorkspace(workspaceId: string, token: string) {
  const api = useMemo(() => new ApiClient(token), [token]);
  const [workspace, dispatch] = useReducer(reduceWorkspaceEvent, emptyWorkspace());
  const [connected, setConnected] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const requestId = useRef(0);

  const load = useCallback(async () => {
    if (!workspaceId || !token) {
      return;
    }
    const id = requestId.current + 1;
    requestId.current = id;
    setLoading(true);
    setError("");
    try {
      const payload = await api.loadWorkspace(workspaceId);
      if (requestId.current === id) {
        dispatch(stampDaemonReceipt({ type: "workspace.snapshot", data: payload }, Date.now()));
      }
    } catch (err) {
      if (requestId.current === id) {
        setError(err instanceof Error ? err.message : String(err));
      }
    } finally {
      if (requestId.current === id) {
        setLoading(false);
      }
    }
  }, [api, token, workspaceId]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!workspaceId || !token) {
      return;
    }
    let disposed = false;
    let reconnectTimer = 0;
    let attempt = 0;
    let socket: WebSocket | null = null;

    const connect = () => {
      if (disposed) {
        return;
      }
      socket = new WebSocket(workspaceWsUrl(workspaceId, token));
      socket.onopen = () => {
        attempt = 0;
        setConnected(true);
      };
      socket.onmessage = (event) => {
        try {
          dispatch(stampDaemonReceipt(JSON.parse(event.data) as WorkspaceEvent, Date.now()));
        } catch {
          // Workspace events are JSON only. Ignore malformed frames from stale sockets.
        }
      };
      socket.onerror = () => socket?.close();
      socket.onclose = () => {
        setConnected(false);
        if (disposed) {
          return;
        }
        reconnectTimer = window.setTimeout(connect, Math.min(1000 * 2 ** attempt, 8000));
        attempt += 1;
      };
    };

    connect();
    return () => {
      disposed = true;
      window.clearTimeout(reconnectTimer);
      socket?.close();
    };
  }, [token, workspaceId]);

  return { workspace: workspace as WorkspaceState, connected, loading, error, reload: load };
}
