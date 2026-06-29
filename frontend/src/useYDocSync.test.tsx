// @vitest-environment jsdom

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as Y from "yjs";
import { encodeSyncStep1 } from "./yProtocol";
import { useYDocSync } from "./useYDocSync";

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];

  binaryType = "";
  readyState = MockWebSocket.OPEN;
  sent: unknown[] = [];
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<ArrayBuffer>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;

  constructor(readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(data: unknown) {
    this.sent.push(data);
  }

  close() {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.({} as CloseEvent);
  }

  emitMessage(data: Uint8Array) {
    this.onmessage?.({ data: toArrayBuffer(data) } as MessageEvent<ArrayBuffer>);
  }
}

function toArrayBuffer(data: Uint8Array) {
  return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength) as ArrayBuffer;
}

function syncFrame() {
  return encodeSyncStep1(new Y.Doc());
}

describe("useYDocSync", () => {
  beforeEach(() => {
    MockWebSocket.instances = [];
    vi.stubGlobal("WebSocket", MockWebSocket);
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("resets readiness immediately when the workspace changes", async () => {
    const { result, rerender } = renderHook(
      ({ workspaceId }) => useYDocSync({ workspaceId, token: "token", documentId: "root_doc" }),
      { initialProps: { workspaceId: "workspace_alpha" } },
    );

    act(() => {
      MockWebSocket.instances[0].emitMessage(syncFrame());
    });
    await waitFor(() => expect(result.current.ready).toBe(true));

    rerender({ workspaceId: "workspace_beta" });

    expect(result.current.ready).toBe(false);
  });
});
