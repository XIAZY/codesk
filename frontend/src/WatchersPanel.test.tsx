// @vitest-environment jsdom

import { render, screen, cleanup, fireEvent, waitFor, act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WatchersPanel, useDocumentSubscribers } from "./App";
import type { Agent, WorkspaceState } from "./types";
import type { ApiClient } from "./api";

// The document-subscribers state + its async-race guards now live in useDocumentSubscribers (lifted to the
// parent so the top-bar badge count is live while the popover is unmounted). These pin the STATEFUL TRANSITION
// SEAM the boundary tests missed (QA/Deniz): reads held open across a document switch resolved out of order; a
// mutation settling after a switch; and same-document out-of-order reconciles. The scope-carrying result
// ({key, ids}, valid only on exact scope match) makes a stale A-count-on-B unrepresentable.

afterEach(cleanup);

type SubscriberResponse = { agents: { id: string; handle: string; name: string; kind: string }[] };

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

const agent = (id: string): Agent => ({
  id, daemonId: "d1", handle: id, name: id, role: "", kind: "agent",
  workspaceRoot: "", status: "idle", currentTask: "", currentActivity: "", currentRunId: "", updatedAt: "now",
});
const alpha = agent("alpha");
const beta = agent("beta");
const row = (id: string) => ({ id, handle: id, name: id, kind: "agent" });

function workspaceWith(agents: Agent[]): Pick<WorkspaceState, "currentUserId" | "agents" | "users" | "presences"> {
  return { currentUserId: "u_me", users: [], agents, presences: {} };
}

// A controllable api: reads and subscribes are parked as FIFO queues of deferreds, so a test resolves them in
// whatever order it wants — the only way to reproduce out-of-order responses deterministically.
function makeApi() {
  const reads: ReturnType<typeof deferred<SubscriberResponse>>[] = [];
  const subscribes: ReturnType<typeof deferred<{ documentIds: string[] }>>[] = [];
  const api = {
    listDocumentSubscribers: vi.fn(() => {
      const d = deferred<SubscriberResponse>();
      reads.push(d);
      return d.promise;
    }),
    subscribeAgentToDocument: vi.fn(() => {
      const d = deferred<{ documentIds: string[] }>();
      subscribes.push(d);
      return d.promise;
    }),
    unsubscribeAgentFromDocument: vi.fn(() => Promise.resolve({ documentIds: [] })),
  } as unknown as ApiClient;
  return { api, reads, subscribes };
}

describe("useDocumentSubscribers scope-carrying guards", () => {
  it("a slow read for the previous document cannot become the current one", async () => {
    const { api, reads } = makeApi();
    const { result, rerender } = renderHook(({ docId }) => useDocumentSubscribers(api, "ws", docId), {
      initialProps: { docId: "docA" },
    });

    // A's read is outstanding → unknown (null), never a false empty [].
    expect(result.current.subscriberIds).toBeNull();

    // Switch to B before A resolves. reads[0] = A, reads[1] = B.
    rerender({ docId: "docB" });
    expect(result.current.subscriberIds).toBeNull();

    await act(async () => reads[1].resolve({ agents: [] }));
    expect(result.current.subscriberIds).toEqual([]);

    // A resolves LATE with alpha — dropped by both the readSeq guard and the scope key mismatch, never onto B.
    await act(async () => reads[0].resolve({ agents: [row("alpha")] }));
    expect(result.current.subscriberIds).toEqual([]);
  });

  it("reads back as unknown the instant the document switches — never the previous doc's ids", async () => {
    const { api, reads } = makeApi();
    const { result, rerender } = renderHook(({ docId }) => useDocumentSubscribers(api, "ws", docId), {
      initialProps: { docId: "docA" },
    });

    await act(async () => reads[0].resolve({ agents: [row("alpha")] }));
    expect(result.current.subscriberIds).toEqual(["alpha"]);
    expect(result.current.loaded).toBe(true);

    // Scope-carrying: on switch the stored {key: ws/docA} no longer matches ws/docB, so ids read null AT ONCE,
    // before B's read resolves — no one-frame alpha-on-B.
    rerender({ docId: "docB" });
    expect(result.current.subscriberIds).toBeNull();
    expect(result.current.loaded).toBe(false);
  });

  it("a mutation settling after a document switch does not alter the new document", async () => {
    const { api, reads, subscribes } = makeApi();
    const { result, rerender } = renderHook(({ docId }) => useDocumentSubscribers(api, "ws", docId), {
      initialProps: { docId: "docA" },
    });

    await act(async () => reads[0].resolve({ agents: [] }));
    expect(result.current.subscriberIds).toEqual([]);

    // Begin subscribing alpha on A (subscribe still pending) — optimistic on A's scope.
    act(() => void result.current.mutate("alpha", true));
    expect(result.current.subscriberIds).toEqual(["alpha"]);

    // Switch to B before the subscribe settles; B resolves empty (reads[1]).
    rerender({ docId: "docB" });
    await act(async () => reads[1].resolve({ agents: [] }));
    expect(result.current.subscriberIds).toEqual([]);

    // A's subscribe settles: its reconcile targets scope ws/docA, so it can never surface alpha on B.
    await act(async () => subscribes[0].resolve({ documentIds: ["alpha"] }));
    expect(result.current.subscriberIds).toEqual([]);
  });

  it("same-document out-of-order reconciles keep the latest-started read (no dropped change)", async () => {
    const { api, reads, subscribes } = makeApi();
    const { result } = renderHook(() => useDocumentSubscribers(api, "ws", "docA"));

    await act(async () => reads[0].resolve({ agents: [] }));
    expect(result.current.subscriberIds).toEqual([]);

    // Subscribe alpha, then beta — two concurrent mutations on the SAME document.
    act(() => void result.current.mutate("alpha", true));
    act(() => void result.current.mutate("beta", true));

    // Both subscribes settle → each triggers a reconcile read (reads[1] = alpha's, reads[2] = beta's).
    await act(async () => subscribes[0].resolve({ documentIds: ["alpha"] }));
    await act(async () => subscribes[1].resolve({ documentIds: ["alpha", "beta"] }));

    // Reconciles return OUT OF ORDER: beta's (latest-started, full truth) first, alpha's (older, stale) late.
    await act(async () => reads[2].resolve({ agents: [row("alpha"), row("beta")] }));
    await act(async () => reads[1].resolve({ agents: [row("alpha")] })); // stale — must be dropped

    // Latest-started read wins: both alpha AND beta remain; the stale [alpha] did not drop beta.
    await waitFor(() => expect(result.current.subscriberIds).toEqual(["alpha", "beta"]));
  });
});

describe("WatchersPanel (presentational, subscriber-only)", () => {
  const onMutate = vi.fn();
  afterEach(() => onMutate.mockReset());

  function renderPanel(over: Partial<Parameters<typeof WatchersPanel>[0]> = {}) {
    return render(
      <WatchersPanel
        documentId="docA"
        workspace={workspaceWith([alpha, beta])}
        subscriberIds={["alpha"]}
        loaded
        busyIds={new Set()}
        error=""
        onMutate={onMutate}
        {...over}
      />,
    );
  }

  it("lists watching agents with Remove, and has no Here-now presence section", () => {
    renderPanel();
    expect(screen.getByText("@alpha")).toBeTruthy();
    // "Watching" appears as the section head and as each row's label; both present.
    expect(screen.getAllByText("Watching").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("Remove")).toBeTruthy();
    // "Here now" presence display was intentionally removed with the tab.
    expect(screen.queryByText(/here now/i)).toBeNull();
  });

  it("a watching row is not a link into an agent-detail modal (no row jump)", () => {
    const { container } = renderPanel();
    // The only interactive control on a watching row is Remove — the handle/avatar are not a button.
    const rowButtons = Array.from(container.querySelectorAll(".agent-card button")).map((b) => b.textContent);
    expect(rowButtons).toEqual(["Remove"]);
  });

  it("Remove calls onMutate(id, false)", () => {
    renderPanel();
    fireEvent.click(screen.getByText("Remove"));
    expect(onMutate).toHaveBeenCalledWith("alpha", false);
  });

  it("Add reveals only non-subscribed agents and Subscribe calls onMutate(id, true)", () => {
    renderPanel();
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    // beta is the only non-subscriber.
    expect(screen.getByText("@beta")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Subscribe" }));
    expect(onMutate).toHaveBeenCalledWith("beta", true);
  });

  it("shows loading and disables Add while the count is unknown", () => {
    renderPanel({ subscriberIds: null, loaded: false });
    expect(screen.getByText(/loading watchers/i)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
