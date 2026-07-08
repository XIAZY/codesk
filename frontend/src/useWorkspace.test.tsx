// @vitest-environment jsdom

// Suite 5 of the corpus: the useWorkspace hook — the workspace realtime lifeline, with zero tests before
// this. It loads an initial snapshot over REST, opens a WebSocket, dispatches every frame into the reducer,
// ignores malformed frames, and reconnects with exponential backoff on close. Each is pinned here so a
// regression in the hook (a dropped snapshot, a crash on a bad frame, a broken backoff, a leaked socket on
// unmount) is caught in isolation, not only when the whole app white-screens.

import { render, cleanup, waitFor, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WorkspaceState } from "./types";

// Controllable snapshot payload the mocked ApiClient.loadWorkspace resolves.
const hoisted = vi.hoisted(() => ({
  snapshot: {} as Record<string, unknown>,
  loadError: null as Error | null,
}));

vi.mock("./api", () => ({
  ApiClient: class {
    constructor(_token: string) {}
    async loadWorkspace(_id: string) {
      if (hoisted.loadError) throw hoisted.loadError;
      return hoisted.snapshot;
    }
  },
  workspaceWsUrl: (workspaceId: string) => `ws://test/ws/${workspaceId}`,
}));

import { useWorkspace } from "./useWorkspace";

const originalWebSocket = globalThis.WebSocket;

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  readyState = FakeWebSocket.CONNECTING;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }
  send() {}
  close() {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close"));
  }
  // test helpers
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event("open"));
  }
  emit(data: string) {
    this.onmessage?.(new MessageEvent("message", { data }));
  }
}

let latest: { workspace: WorkspaceState; connected: boolean } = { workspace: {} as WorkspaceState, connected: false };

function Probe({ workspaceId, token }: { workspaceId: string; token: string }) {
  const { workspace, connected } = useWorkspace(workspaceId, token);
  latest = { workspace, connected };
  return null;
}

beforeEach(() => {
  FakeWebSocket.instances = [];
  hoisted.snapshot = { workspaceId: "ws-1", name: "Snapshot Name" };
  hoisted.loadError = null;
  (globalThis as unknown as { WebSocket: unknown }).WebSocket = FakeWebSocket;
});

afterEach(() => {
  cleanup();
  (globalThis as unknown as { WebSocket: unknown }).WebSocket = originalWebSocket;
  vi.useRealTimers();
});

describe("useWorkspace", () => {
  it("loads the initial snapshot over REST and dispatches it into workspace state", async () => {
    render(<Probe workspaceId="ws-1" token="t" />);
    await waitFor(() => expect(latest.workspace.name).toBe("Snapshot Name"));
  });

  it("marks connected once the socket opens and dispatches frames it receives", async () => {
    render(<Probe workspaceId="ws-1" token="t" />);
    await waitFor(() => expect(FakeWebSocket.instances.length).toBe(1));
    const socket = FakeWebSocket.instances[0];

    expect(latest.connected).toBe(false);
    act(() => socket.open());
    expect(latest.connected).toBe(true);

    act(() => socket.emit(JSON.stringify({ type: "workspace.updated", data: { name: "Live Name" } })));
    await waitFor(() => expect(latest.workspace.name).toBe("Live Name"));
  });

  it("ignores a malformed frame without crashing or mutating state", async () => {
    render(<Probe workspaceId="ws-1" token="t" />);
    await waitFor(() => expect(latest.workspace.name).toBe("Snapshot Name"));
    const socket = FakeWebSocket.instances[0];
    act(() => socket.open());

    // A non-JSON frame must be swallowed by the hook's try/catch — no throw, state unchanged.
    act(() => socket.emit("this is not json {"));
    expect(latest.workspace.name).toBe("Snapshot Name");
    expect(latest.connected).toBe(true);
  });

  it("reconnects with exponential backoff after the socket closes", async () => {
    vi.useFakeTimers();
    render(<Probe workspaceId="ws-1" token="t" />);
    // one socket created synchronously by the connect effect
    expect(FakeWebSocket.instances.length).toBe(1);
    const first = FakeWebSocket.instances[0];
    act(() => first.open());
    expect(latest.connected).toBe(true);

    // Close → connected clears and a reconnect is scheduled; the first backoff is 1000ms (attempt 0).
    act(() => first.close());
    expect(latest.connected).toBe(false);
    expect(FakeWebSocket.instances.length).toBe(1);
    act(() => {
      vi.advanceTimersByTime(999);
    });
    expect(FakeWebSocket.instances.length).toBe(1); // not yet
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(FakeWebSocket.instances.length).toBe(2); // reconnected at 1000ms

    // Second close backs off further (2000ms = 1000 * 2^1), proving the exponential schedule.
    act(() => FakeWebSocket.instances[1].close());
    act(() => {
      vi.advanceTimersByTime(1999);
    });
    expect(FakeWebSocket.instances.length).toBe(2);
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(FakeWebSocket.instances.length).toBe(3);
  });

  it("closes the socket and stops reconnecting on unmount", async () => {
    vi.useFakeTimers();
    const view = render(<Probe workspaceId="ws-1" token="t" />);
    expect(FakeWebSocket.instances.length).toBe(1);
    act(() => FakeWebSocket.instances[0].open());

    view.unmount();
    // Any pending reconnect must be cancelled — advancing time creates no new socket.
    act(() => {
      vi.advanceTimersByTime(10000);
    });
    expect(FakeWebSocket.instances.length).toBe(1);
  });
});
