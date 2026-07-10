// @vitest-environment jsdom

import { render, screen, cleanup, fireEvent, waitFor, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ParticipantsPanel } from "./App";
import type { Agent, WorkspaceState } from "./types";
import type { ApiClient } from "./api";

// These pin the STATEFUL TRANSITION SEAM the boundary tests missed (QA/Deniz): reads held open across a
// document switch resolved out of order; a mutation settling after a switch; and same-document out-of-order
// reconciles. The pure grouping, the no-document panel, and the settled empty panel were all green while
// these transition states were unpinned — every steady state correct, every transition unproven.

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

// A controllable api: reads and subscribes are parked as FIFO queues of deferreds, so a test resolves them
// in whatever order it wants — the only way to reproduce out-of-order responses deterministically.
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

// Mirror the real usage: the parent keys the panel on workspace+document, so a scope switch REMOUNTS it.
// The tests rerender with this same key so a document switch discards the old scope's state, exactly as it
// does in the app — the make-invalid-states-unrepresentable fix Tom ruled for the switch race.
function panel(api: ApiClient, documentId: string, agents: Agent[]) {
  return (
    <ParticipantsPanel key={"ws/" + documentId} documentId={documentId} workspace={workspaceWith(agents)} agents={agents} api={api} workspaceId="ws" onAgent={() => {}} />
  );
}

describe("ParticipantsPanel stale-response races", () => {
  it("a slow read for the previous document cannot overwrite the current one", async () => {
    const { api, reads } = makeApi();
    const { rerender } = render(panel(api, "docA", [alpha]));

    // A's read is outstanding → loading, never a false empty/all-addable panel.
    expect(screen.getByText(/loading participants/i)).toBeTruthy();

    // Switch to B before A resolves. reads[0] = A, reads[1] = B.
    rerender(panel(api, "docB", [alpha]));

    // B resolves first: no watchers.
    await act(async () => reads[1].resolve({ agents: [] }));
    await waitFor(() => expect(screen.getByText(/no agents are watching this document yet/i)).toBeTruthy());

    // A resolves LATE with alpha — on the unmounted A instance, so its setState is a no-op, never onto B.
    await act(async () => reads[0].resolve({ agents: [row("alpha")] }));

    expect(screen.getByText(/no agents are watching this document yet/i)).toBeTruthy();
    expect(screen.queryByText("Remove")).toBeNull();
  });

  it("resets to loading on switch — never the previous doc's rows nor a false empty panel", async () => {
    const { api, reads } = makeApi();
    const { rerender } = render(panel(api, "docA", [alpha]));

    await act(async () => reads[0].resolve({ agents: [row("alpha")] }));
    await waitFor(() => expect(screen.getByText("Remove")).toBeTruthy());

    rerender(panel(api, "docB", [alpha]));
    expect(screen.getByText(/loading participants/i)).toBeTruthy();
    expect(screen.queryByText("Remove")).toBeNull();
    expect(screen.queryByText(/no agents are watching this document yet/i)).toBeNull();
    expect((screen.getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("a mutation settling after a document switch does not alter the new document", async () => {
    const { api, reads, subscribes } = makeApi();
    const { rerender } = render(panel(api, "docA", [alpha]));

    await act(async () => reads[0].resolve({ agents: [] }));
    await waitFor(() => expect(screen.getByText(/no agents are watching this document yet/i)).toBeTruthy());

    // Begin subscribing alpha on A (subscribe still pending).
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    fireEvent.click(screen.getByRole("button", { name: /subscribe/i }));

    // Switch to B before the subscribe settles; B resolves empty (reads[1]).
    rerender(panel(api, "docB", [alpha]));
    await act(async () => reads[1].resolve({ agents: [] }));

    // A's subscribe now settles on the UNMOUNTED A instance — its reconcile setState is a no-op and cannot
    // reach B (the remount discarded A's state tree).
    await act(async () => subscribes[0].resolve({ documentIds: ["alpha"] }));

    expect(screen.getByText(/no agents are watching this document yet/i)).toBeTruthy();
    expect(screen.queryByText("Remove")).toBeNull();
  });

  it("same-document out-of-order reconciles keep the latest-started read (no dropped change)", async () => {
    const { api, reads, subscribes } = makeApi();
    render(panel(api, "docA", [alpha, beta]));

    // A loaded empty → both agents addable. reads[0] = initial.
    await act(async () => reads[0].resolve({ agents: [] }));
    await waitFor(() => expect(screen.getByText(/no agents are watching this document yet/i)).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    // Subscribe alpha, then beta — two concurrent mutations on the SAME document. After each click the just-
    // subscribed agent leaves the picker optimistically, so the first remaining Subscribe button is the next.
    const subscribeButtons = () => screen.getAllByRole("button", { name: "Subscribe" });
    fireEvent.click(subscribeButtons()[0]);
    fireEvent.click(subscribeButtons()[0]);

    // Both subscribes settle → each triggers a reconcile read (reads[1] = alpha's, reads[2] = beta's).
    await act(async () => subscribes[0].resolve({ documentIds: ["alpha"] }));
    await act(async () => subscribes[1].resolve({ documentIds: ["alpha", "beta"] }));

    // Reconciles return OUT OF ORDER: beta's (latest-started, full truth) first, alpha's (older, stale) late.
    await act(async () => reads[2].resolve({ agents: [row("alpha"), row("beta")] }));
    await act(async () => reads[1].resolve({ agents: [row("alpha")] })); // stale — must be dropped

    // Latest-started read wins: both alpha AND beta remain watched; the stale [alpha] did not drop beta.
    await waitFor(() => expect(screen.getAllByText("Remove")).toHaveLength(2));
  });
});
