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
import type { DocumentItem } from "./types";

export function useDocumentSync(input: {
  workspaceId: string;
  token: string;
  document: DocumentItem | null;
  actorName: string;
}) {
  const [ready, setReady] = useState(false);
  const [connected, setConnected] = useState(false);
  const ydoc = useMemo(() => new Y.Doc(), [input.document?.id]);
  const ytext = useMemo(() => ydoc.getText("content"), [ydoc]);
  const awarenessRef = useRef<Awareness | null>(null);
  const socketRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    setReady(false);
  }, [input.document?.id]);

  const documentId = input.document?.id ?? "";
  const documentPath = input.document?.path ?? "";

  useEffect(() => {
    if (!documentId || !input.workspaceId || !input.token) {
      return;
    }
    const awareness = new Awareness(ydoc);
    awareness.setLocalState({
      actorName: input.actorName,
      actorType: "human",
      activity: `Editing ${documentPath}`,
      filePath: documentPath,
    });
    awarenessRef.current = awareness;

    const handleUpdate = (update: Uint8Array, origin: unknown) => {
      if (origin === "remote") {
        return;
      }
      if (socketRef.current?.readyState === WebSocket.OPEN) {
        socketRef.current.send(encodeSyncUpdate(update));
      }
    };
    ydoc.on("update", handleUpdate);

    let disposed = false;
    let reconnectTimer = 0;
    let attempt = 0;

    const connect = () => {
      if (disposed) {
        return;
      }
      const ws = new WebSocket(documentWsUrl(input.workspaceId, documentId, input.token, ydoc.clientID));
      ws.binaryType = "arraybuffer";
      socketRef.current = ws;
      ws.onopen = () => {
        attempt = 0;
        setConnected(true);
        ws.send(encodeSyncStep1(ydoc));
        ws.send(encodeAwarenessMessage(awareness, [ydoc.clientID]));
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
        if (messageType === messageAwareness) {
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
      awareness.destroy();
      awarenessRef.current = null;
    };
  }, [documentId, documentPath, input.actorName, input.token, input.workspaceId, ydoc]);

  useEffect(() => {
    return () => {
      ydoc.destroy();
    };
  }, [ydoc]);

  return { ydoc, ytext, ready, connected };
}
