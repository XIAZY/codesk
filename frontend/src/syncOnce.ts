import * as Y from "yjs";
import { documentWsUrl } from "./api";
import { encodeSyncStep1, encodeSyncUpdate, handleSyncPayload, messageSync, readProtocolMessage } from "./yProtocol";

export function sendYjsUpdateOnce(input: {
  workspaceId: string;
  token: string;
  documentId: string;
  update: Uint8Array;
  timeoutMs?: number;
}) {
  if (!input.documentId || input.update.length === 0) {
    return Promise.resolve();
  }
  const doc = new Y.Doc({ guid: `push-${input.documentId}` });
  return new Promise<void>((resolve, reject) => {
    let settled = false;
    let timeout = 0;
    const ws = new WebSocket(documentWsUrl(input.workspaceId, input.documentId, input.token, doc.clientID));
    ws.binaryType = "arraybuffer";

    const finish = (error?: Error) => {
      if (settled) {
        return;
      }
      settled = true;
      window.clearTimeout(timeout);
      ws.close();
      doc.destroy();
      if (error) {
        reject(error);
      } else {
        resolve();
      }
    };
    timeout = window.setTimeout(() => {
      finish(new Error("timed out sending CRDT update"));
    }, input.timeoutMs ?? 5000);

    ws.onopen = () => {
      ws.send(encodeSyncStep1(doc));
      ws.send(encodeSyncUpdate(input.update));
    };
    ws.onmessage = (event) => {
      try {
        const { messageType, payload } = readProtocolMessage(new Uint8Array(event.data as ArrayBuffer));
        if (messageType !== messageSync) {
          return;
        }
        const reply = handleSyncPayload(doc, payload);
        if (reply && ws.readyState === WebSocket.OPEN) {
          ws.send(reply);
        }
        finish();
      } catch (err) {
        finish(err instanceof Error ? err : new Error(String(err)));
      }
    };
    ws.onerror = () => finish(new Error("websocket failed while sending CRDT update"));
  });
}

export function buildContentInsertUpdate(content: string) {
  const doc = new Y.Doc();
  try {
    const text = doc.getText("content");
    let update = new Uint8Array();
    doc.on("update", (next: Uint8Array) => {
      update = Uint8Array.from(next);
    });
    doc.transact(() => {
      if (content) {
        text.insert(0, content);
      }
    }, "initial-content");
    return update;
  } finally {
    doc.destroy();
  }
}
