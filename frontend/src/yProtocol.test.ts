import { describe, expect, it } from "vitest";
import * as Y from "yjs";
import { encodeSyncStep1, handleSyncPayload, messageSync, readProtocolMessage } from "./yProtocol";

describe("document websocket protocol", () => {
  it("does not send canonical empty sync-step-2 replies", () => {
    const local = new Y.Doc();
    const remote = new Y.Doc();
    const step1 = readProtocolMessage(encodeSyncStep1(local));

    expect(step1.messageType).toBe(messageSync);
    expect(handleSyncPayload(remote, step1.payload)).toBeNull();
  });
});
