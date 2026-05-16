// @vitest-environment jsdom

import { useEffect } from "react";
import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type * as Y from "yjs";
import type { DocumentItem } from "./types";
import { useDocumentSync } from "./useDocument";

const originalWebSocket = globalThis.WebSocket;

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  binaryType: BinaryType = "blob";
  readyState = FakeWebSocket.CONNECTING;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  sent: unknown[] = [];

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }

  send(data: unknown) {
    this.sent.push(data);
  }

  close() {
    if (this.readyState === FakeWebSocket.CLOSED) {
      return;
    }
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close"));
  }
}

function document(id: string, path = `${id}.md`): DocumentItem {
  return {
    id,
    path,
    title: path,
    updatedAt: "now",
  };
}

function Probe({
  document,
  token,
  onDoc,
}: {
  document: DocumentItem;
  token: string;
  onDoc: (doc: Y.Doc) => void;
}) {
  const { ydoc } = useDocumentSync({
    workspaceId: "workspace",
    token,
    document,
    actorName: "Tester",
  });

  useEffect(() => {
    onDoc(ydoc);
  }, [onDoc, ydoc]);

  return null;
}

beforeEach(() => {
  FakeWebSocket.instances = [];
  Object.defineProperty(globalThis, "WebSocket", {
    value: FakeWebSocket,
    configurable: true,
  });
});

afterEach(() => {
  cleanup();
  Object.defineProperty(globalThis, "WebSocket", {
    value: originalWebSocket,
    configurable: true,
  });
});

describe("useDocumentSync", () => {
  it("destroys the old Y.Doc when switching to another document", async () => {
    const destroyed = vi.fn();
    const seenDocs: Y.Doc[] = [];
    const handleDoc = (doc: Y.Doc) => {
      if (seenDocs.length === 0) {
        doc.on("destroy", destroyed);
      }
      seenDocs.push(doc);
    };

    const { rerender } = render(
      <Probe
        document={document("doc-a")}
        token="token-a"
        onDoc={handleDoc}
      />
    );

    await waitFor(() => expect(seenDocs).toHaveLength(1));

    rerender(
      <Probe
        document={document("doc-b")}
        token="token-a"
        onDoc={handleDoc}
      />
    );

    await waitFor(() => expect(destroyed).toHaveBeenCalledTimes(1));
    expect(seenDocs[seenDocs.length - 1]).not.toBe(seenDocs[0]);
    expect(FakeWebSocket.instances[0].readyState).toBe(FakeWebSocket.CLOSED);
  });

  it("does not destroy the active Y.Doc for same-document websocket reconnect inputs", async () => {
    const destroyed = vi.fn();
    const seenDocs: Y.Doc[] = [];
    const handleDoc = (doc: Y.Doc) => {
      doc.on("destroy", destroyed);
      seenDocs.push(doc);
    };

    const { rerender } = render(
      <Probe
        document={document("doc-a")}
        token="token-a"
        onDoc={handleDoc}
      />
    );

    await waitFor(() => expect(seenDocs).toHaveLength(1));

    rerender(
      <Probe
        document={document("doc-a")}
        token="token-b"
        onDoc={handleDoc}
      />
    );

    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    expect(destroyed).not.toHaveBeenCalled();
    expect(seenDocs).toHaveLength(1);
    expect(FakeWebSocket.instances[0].readyState).toBe(FakeWebSocket.CLOSED);
  });
});
