import { useEffect, useMemo, useRef, useState } from "react";
import { Awareness } from "y-protocols/awareness.js";
import * as Y from "yjs";
import { documentWsUrl } from "./api";
import {
  encodeAwarenessMessage,
  encodeSyncStep1,
  encodeSyncUpdate,
  handleAwarenessPayload,
  handleSyncPayload,
  messageAwareness,
  messageSync,
  readProtocolMessage,
} from "./yProtocol";

export type AwarenessInput = {
  actorName: string;
  actorType: string;
  activity: string;
  filePath: string;
};

export function useYDocSync(input: {
  workspaceId: string;
  token: string;
  documentId: string;
  awareness?: AwarenessInput | null;
}) {
  const [ready, setReady] = useState(false);
  const [connected, setConnected] = useState(false);
  const ydoc = useMemo(() => new Y.Doc(), [input.documentId]);
  const awarenessRef = useRef<Awareness | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const pendingUpdatesRef = useRef<Uint8Array[]>([]);

  useEffect(() => {
    setReady(false);
    pendingUpdatesRef.current = [];
  }, [input.documentId]);

  useEffect(() => {
    if (!input.documentId || !input.workspaceId || !input.token) {
      return;
    }
    const awareness = input.awareness ? new Awareness(ydoc) : null;
    if (awareness && input.awareness) {
      awareness.setLocalState(input.awareness);
      awarenessRef.current = awareness;
    }

    const sendUpdate = (update: Uint8Array) => {
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(encodeSyncUpdate(update));
        return;
      }
      pendingUpdatesRef.current = [...pendingUpdatesRef.current, update];
    };

    const handleUpdate = (update: Uint8Array, origin: unknown) => {
      if (origin === "remote") {
        return;
      }
      sendUpdate(update);
    };
    ydoc.on("update", handleUpdate);

    let disposed = false;
    let reconnectTimer = 0;
    let attempt = 0;

    const connect = () => {
      if (disposed) {
        return;
      }
      const ws = new WebSocket(documentWsUrl(input.workspaceId, input.documentId, input.token, ydoc.clientID));
      ws.binaryType = "arraybuffer";
      socketRef.current = ws;
      ws.onopen = () => {
        attempt = 0;
        setConnected(true);
        ws.send(encodeSyncStep1(ydoc));
        for (const update of pendingUpdatesRef.current) {
          ws.send(encodeSyncUpdate(update));
        }
        pendingUpdatesRef.current = [];
        if (awareness) {
          ws.send(encodeAwarenessMessage(awareness, [ydoc.clientID]));
        }
      };
      ws.onmessage = (event) => {
        const { messageType, payload } = readProtocolMessage(new Uint8Array(event.data as ArrayBuffer));
        if (messageType === messageSync) {
          const reply = handleSyncPayload(ydoc, payload);
          if (reply && ws.readyState === WebSocket.OPEN) {
            ws.send(reply);
          }
          setReady(true);
        }
        if (messageType === messageAwareness && awareness) {
          handleAwarenessPayload(awareness, payload);
        }
      };
      ws.onerror = () => ws.close();
      ws.onclose = () => {
        if (socketRef.current === ws) {
          socketRef.current = null;
        }
        setConnected(false);
        if (disposed) {
          return;
        }
        reconnectTimer = window.setTimeout(connect, Math.min(1000 * 2 ** attempt, 5000));
        attempt += 1;
      };
    };

    connect();
    return () => {
      disposed = true;
      window.clearTimeout(reconnectTimer);
      ydoc.off("update", handleUpdate);
      socketRef.current?.close();
      socketRef.current = null;
      awareness?.destroy();
      awarenessRef.current = null;
    };
  }, [input.awareness, input.documentId, input.token, input.workspaceId, ydoc]);

  useEffect(() => {
    return () => {
      ydoc.destroy();
    };
  }, [ydoc]);

  return { ydoc, ready, connected };
}
