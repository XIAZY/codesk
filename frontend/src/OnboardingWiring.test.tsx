// @vitest-environment jsdom

// Integration coverage for the Batch-1 onboarding wiring in WorkspaceApp: the
// controller mounts the guide spotlight + getting-started checklist and feeds them
// from LIVE derived state, and the one non-derivable record site (member_invited)
// fires on a real invite. The pure derivation/adapter is covered in
// onboarding.test.ts / onboardingController.test.ts; this pins the App.tsx glue.

import { act, cleanup, render, renderHook, screen, fireEvent, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceApp, MembersAndInvite, shouldRenderOnboardingChecklist, shouldRenderOnboardingSpotlight } from "./App";
import { useOnboardingController } from "./onboardingController";
import type { OnboardingRole } from "./onboardingEngine";
import type { Account, Agent, Daemon, WorkspaceState, WorkspaceSummary } from "./types";

// A daemon that reads receipt-live "online" for the given clock (see daemonLiveStatus):
// a genuine recent check-in + a receivedAt at ~now, so liveEnvironmentCount counts it.
const NOW_MS = 1_700_000_000_000;
function liveDaemonAt(receivedAtMs: number): Daemon {
  return {
    status: "connected",
    connectionStatus: "online",
    lastSeenAt: "2026-07-01T00:00:00Z",
    lastSeenAgeSeconds: 1,
    receivedAtMs,
  } as unknown as Daemon;
}

const mocks = vi.hoisted(() => ({
  workspace: null as WorkspaceState | null,
  documents: [] as unknown[],
  ready: true,
}));

vi.mock("./useWorkspace", () => ({
  useWorkspace: () => ({
    workspace: mocks.workspace,
    connected: true,
    loading: false,
    error: "",
    reload: vi.fn(),
  }),
}));

vi.mock("./useRootNamespace", () => ({
  useRootNamespace: () => ({
    documents: mocks.documents,
    ready: mocks.ready,
    connected: true,
    upsertFile: vi.fn(),
    moveFile: vi.fn(),
    tombstoneFile: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
  window.localStorage.clear();
  mocks.workspace = null;
  mocks.documents = [];
  mocks.ready = true;
});

function agent(id: string): Agent {
  return {
    id,
    daemonId: "daemon_1",
    handle: id,
    name: id,
    role: "assistant",
    kind: "agent",
    model: "",
    reasoningEffort: "",
    workspaceRoot: "/root",
    status: "idle",
    currentTask: "",
    currentActivity: "",
    currentRunId: "",
    updatedAt: "2026-06-28T00:00:00Z",
  };
}

function workspaceState(overrides: Partial<WorkspaceState> = {}): WorkspaceState {
  return {
    workspaceId: "workspace_1",
    rootDocumentId: "root_doc",
    currentUserId: "user_owner",
    name: "Product Workspace",
    users: [
      {
        id: "user_owner",
        handle: "owner_handle",
        name: "Owner In Workspace",
        role: "Workspace owner",
        kind: "human",
        status: "active",
        updatedAt: "2026-06-28T00:00:00Z",
      },
    ],
    daemons: [],
    agents: [],
    agentRuns: [],
    threads: [],
    agentEvents: [],
    presences: {},
    ...overrides,
  };
}

function account(): Account {
  return { id: "account_1", email: "real.email@example.com", displayName: "Real Email" };
}

function renderWorkspace() {
  const workspaces: WorkspaceSummary[] = [{ id: "workspace_1", slug: "product-workspace", name: "Product Workspace" }];
  return render(
    <WorkspaceApp
      api={{ updateLastAccessed: vi.fn().mockResolvedValue({ status: "ok" }) } as never}
      token="token"
      workspaceId="workspace_1"
      workspaceSlug="product-workspace"
      view={{ kind: "home" }}
      account={account()}
      workspaces={workspaces}
      onAccess={vi.fn()}
      onWorkspaceChange={vi.fn()}
      onSignOut={vi.fn()}
    />,
  );
}

describe("onboarding wiring in WorkspaceApp", () => {
  it("mounts the empty-workspace spotlight without placing the checklist under its scrim", () => {
    mocks.workspace = workspaceState();
    mocks.documents = [];
    renderWorkspace();

    // The guided sequence's first step triggers on the empty workspace.
    expect(screen.getByText("These are real files")).toBeTruthy();
    expect(screen.queryByText("Finish setting up this workspace")).toBeNull();
    expect(screen.queryByText("0 of 5 done")).toBeNull();
  });

  it("shows honest live checklist progress after the guide is complete — a member gets NO AI entry (#90)", () => {
    window.localStorage.setItem(
      "codesk.onboarding.account.account_1.ws.workspace_1.flags",
      JSON.stringify(["seen:threads-intro@v1", "seen:watchers-intro@v1"]),
    );
    mocks.workspace = workspaceState({ agents: [agent("agent_1")] });
    mocks.documents = [{ id: "doc_1", path: "Product.md", title: "Product.md" }];
    renderWorkspace();

    // Member (no owner/admin role): the checklist is create-document (done) +
    // start-discussion — and NO AI onboarding entry, even with an agent present (#90 removed
    // work-with-agent-entry). Denominator is 2, all live-derived. Members never auto-open.
    expect(screen.getByText("1 of 2 done")).toBeTruthy();
    expect(screen.queryByText("Work with an agent")).toBeNull();
    expect(screen.queryByText("Add an AI teammate")).toBeNull();
    expect(screen.queryByText("These are real files")).toBeNull();
  });

  it("gates the checklist only for blocking guide spotlights, never for contextual tips", () => {
    expect(shouldRenderOnboardingChecklist(true, "spotlight")).toBe(false);
    expect(shouldRenderOnboardingChecklist(true, "tip")).toBe(true);
    expect(shouldRenderOnboardingChecklist(true, null)).toBe(true);
    expect(shouldRenderOnboardingChecklist(false, null)).toBe(false);
  });

  it("suppresses the discussion spotlight while a promotion is pending or the chapter is open (#90 bug 2)", () => {
    // Normal: an active surface with no chapter/promotion → the spotlight renders.
    expect(shouldRenderOnboardingSpotlight(true, false, false)).toBe(true);
    // A promotion is about to auto-open this frame → the discussion spotlight must NOT paint,
    // so it never flashes before the teammate chapter takes the screen.
    expect(shouldRenderOnboardingSpotlight(true, false, true)).toBe(false);
    // Chapter already open (interlude) → suppressed.
    expect(shouldRenderOnboardingSpotlight(true, true, false)).toBe(false);
    // No active surface → nothing to render regardless.
    expect(shouldRenderOnboardingSpotlight(false, false, false)).toBe(false);
  });
});

describe("member_invited record site", () => {
  it("fires onMemberInvited after a successful invite (the only non-derivable completion path)", async () => {
    const onMemberInvited = vi.fn();
    const api = {
      createWorkspaceInvite: vi
        .fn()
        .mockResolvedValue({ url: "/join/abc", invite: { expiresAt: "2026-07-13T00:00:00Z" } }),
    };

    render(
      <MembersAndInvite
        api={api as never}
        workspaceId="workspace_1"
        users={[]}
        canInvite
        onMemberInvited={onMemberInvited}
      />,
    );

    fireEvent.click(screen.getByText("Generate invite link"));

    await waitFor(() => expect(onMemberInvited).toHaveBeenCalledTimes(1));
    expect(api.createWorkspaceInvite).toHaveBeenCalledWith("workspace_1");
  });
});

describe("useOnboardingController scope isolation (per user × workspace)", () => {
  const baseInput = {
    enabled: true,
    roles: ["owner"] as OnboardingRole[],
    workspaceState: workspaceState(),
    documentCount: 1, // create-first-document already complete → guide sits past it
    nowMs: 0,
  };

  it("rehydrates workspace acknowledgements on a workspace switch — no cross-workspace leak", () => {
    window.localStorage.setItem(
      "codesk.onboarding.account.acct_1.ws.wsA.flags",
      JSON.stringify(["seen:threads-intro@v1"]),
    );
    const { result, rerender } = renderHook(
      (props: { workspaceId: string }) =>
        useOnboardingController({ ...baseInput, accountId: "acct_1", route: "document", selectionActive: false, ...props }),
      { initialProps: { workspaceId: "wsA" } },
    );
    // A: threads-intro acknowledged → the 2-step guide is finished, nothing active
    // (watchers left the sequence in #56; no agent here so its tip is inert too).
    expect(result.current.active).toBeNull();
    // Switch the still-mounted controller to workspace B (no flags): threads-intro reappears.
    rerender({ workspaceId: "wsB" });
    expect(result.current.active?.id).toBe("threads-intro");
  });

  it("isolates account acknowledgements per account — A's tip-completion can't suppress B", () => {
    // Guide already finished in this workspace for BOTH accounts (workspace flags are per
    // account × workspace), so the only thing left to show is the account-scoped tip.
    for (const account of ["acctA", "acctB"]) {
      window.localStorage.setItem(
        `codesk.onboarding.account.${account}.ws.shared.flags`,
        JSON.stringify(["seen:threads-intro@v1", "seen:watchers-intro@v1"]),
      );
    }
    // Account A has used threads (account-durable flag) → its first-selection tip is done.
    window.localStorage.setItem("codesk.onboarding.account.acctA.flags", JSON.stringify(["first_thread_created"]));
    const { result, rerender } = renderHook(
      (props: { accountId: string }) =>
        useOnboardingController({ ...baseInput, workspaceId: "shared", route: "document", selectionActive: true, ...props }),
      { initialProps: { accountId: "acctA" } },
    );
    // A: guide done + tip account-complete → nothing shows.
    expect(result.current.active).toBeNull();
    // Account B on the same browser: guide done, but B never used threads → the tip shows,
    // NOT suppressed by A's account flag.
    rerender({ accountId: "acctB" });
    expect(result.current.active?.id).toBe("tip-first-selection");
  });
});

describe("useOnboardingController — chapter open/close + card exposure (#56, reshaped #90)", () => {
  // documentCount 0 → no auto-open fires (needs a document), so these exercise the MANUAL
  // open/close path in isolation. Auto-open (promotion) is covered in its own describe below.
  const chapterBase = {
    enabled: true,
    accountId: "acct_c",
    workspaceId: "ws_c",
    route: "home",
    selectionActive: false,
    documentCount: 0,
    nowMs: 0,
  };

  it("owner: closed by default, opens to step 1, and 'Not now'/Close closes without completing", () => {
    const { result } = renderHook(() =>
      useOnboardingController({ ...chapterBase, roles: ["owner"], workspaceState: workspaceState() }),
    );
    // Closed by default — no card rendered, though the shape (2 owner/admin steps) is known.
    expect(result.current.chapterActive).toBeNull();
    expect(result.current.chapterTotal).toBe(2);

    act(() => result.current.openChapter());
    expect(result.current.chapterActive?.id).toBe("add-teammate-connect");
    expect(result.current.chapterStepIndex).toBe(0);
    // A MANUAL open shows normal copy — never the promotion bridge line.
    expect(result.current.chapterBridge).toBe(false);

    // Close dismisses only — no completion recorded, so reopening resumes from live state.
    act(() => result.current.closeChapter());
    expect(result.current.chapterActive).toBeNull();
    act(() => result.current.openChapter());
    expect(result.current.chapterActive?.id).toBe("add-teammate-connect");
  });

  it("member has no chapter — opening yields no card, even with an agent (#90)", () => {
    const { result: none } = renderHook(() =>
      useOnboardingController({ ...chapterBase, roles: ["member"], workspaceState: workspaceState() }),
    );
    act(() => none.current.openChapter());
    expect(none.current.chapterActive).toBeNull();
    expect(none.current.chapterTotal).toBe(0);

    const { result: withAgent } = renderHook(() =>
      useOnboardingController({ ...chapterBase, roles: ["member"], workspaceState: workspaceState({ agents: [agent("a1")] }) }),
    );
    act(() => withAgent.current.openChapter());
    expect(withAgent.current.chapterActive).toBeNull();
    expect(withAgent.current.chapterTotal).toBe(0);
  });
});

const BRIDGE_LINE = "Your first document's ready. Now bring in an AI teammate to work on it with you.";
const PROMO_KEY = "codesk.onboarding.account.account_1.ws.workspace_1.teammatePromoDismissed";
const FLAGS_KEY = "codesk.onboarding.account.account_1.ws.workspace_1.flags";

describe("chapter integration in WorkspaceApp (#56 render head, promoted #90)", () => {
  const guideDone = () => window.localStorage.setItem(FLAGS_KEY, JSON.stringify(["seen:threads-intro@v1"]));

  it("owner CATCH-UP: an existing incomplete workspace auto-opens the chapter with NORMAL copy (no bridge)", () => {
    guideDone();
    // A workspace that already has a document + incomplete activation (no env/agent).
    mocks.workspace = workspaceState({ currentMembershipRole: "owner" });
    mocks.documents = [{ id: "doc_1", path: "P.md", title: "P.md" }];
    renderWorkspace();

    // Auto-opened to the true first-incomplete card (connect), NEVER a spotlight, and with
    // NORMAL copy — the bridge is a fresh-only TITLE, so catch-up shows the plain title and
    // never the bridge line.
    expect(screen.getByText("Bring in an AI teammate")).toBeTruthy();
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
    expect(screen.queryByText(BRIDGE_LINE)).toBeNull();
    // One surface owns attention: the checklist launcher is suspended while the chapter is up.
    expect(screen.queryByText("Finish setting up this workspace")).toBeNull();

    const flagsBefore = window.localStorage.getItem(FLAGS_KEY);

    // "Not now" on an AUTO-opened chapter records the per-workspace promo-dismiss flag (a
    // presentation flag — NOT a completion), closes, and restores the checklist entry.
    fireEvent.click(screen.getByText("Not now"));
    expect(screen.queryByText("Bring in an AI teammate")).toBeNull();
    expect(window.localStorage.getItem(PROMO_KEY)).toBe("true");
    // No completion/seen flag was written — only the presentation promo-dismiss key.
    expect(window.localStorage.getItem(FLAGS_KEY)).toBe(flagsBefore);

    // The checklist row is intact; a MANUAL reopen still works and shows NORMAL copy.
    expect(screen.getByText("Add an AI teammate")).toBeTruthy();
    expect(screen.getByText("1 of 2")).toBeTruthy();
    fireEvent.click(screen.getByText("Add an AI teammate"));
    expect(screen.getByText("Bring in an AI teammate")).toBeTruthy();
    expect(screen.queryByText(BRIDGE_LINE)).toBeNull();
  });

  it("owner with promo-dismiss already set: never auto-opens, but the checklist entry still opens it manually", () => {
    guideDone();
    window.localStorage.setItem(PROMO_KEY, "true");
    mocks.workspace = workspaceState({ currentMembershipRole: "owner" });
    mocks.documents = [{ id: "doc_1", path: "P.md", title: "P.md" }];
    renderWorkspace();

    // Suppressed: the chapter does not auto-open; the checklist launcher is present.
    expect(screen.queryByText("Bring in an AI teammate")).toBeNull();
    expect(screen.getByText("Add an AI teammate")).toBeTruthy();
    // Manual open still works (dismissal suppresses AUTO-open only) — normal copy.
    fireEvent.click(screen.getByText("Add an AI teammate"));
    expect(screen.getByText("Bring in an AI teammate")).toBeTruthy();
    expect(screen.queryByText(BRIDGE_LINE)).toBeNull();
  });

  it("owner with setup already complete: never auto-opens; the entry shows the Ready completion", () => {
    guideDone();
    // env live + an agent → activation complete → no promotion, ever.
    mocks.workspace = workspaceState({
      currentMembershipRole: "owner",
      daemons: [liveDaemonAt(Date.now())],
      agents: [agent("agent_1")],
    });
    mocks.documents = [{ id: "doc_1", path: "P.md", title: "P.md" }];
    renderWorkspace();

    expect(screen.queryByText("Bring in an AI teammate")).toBeNull(); // no auto-open
    // The entry reads done with the Ready (historical) label and a View-completion reopen.
    expect(screen.getByText("Your AI teammate is ready")).toBeTruthy();
    expect(screen.getByText("View completion")).toBeTruthy();
  });

  it("member: never auto-opens and has no AI entry at all (#90)", () => {
    guideDone();
    mocks.workspace = workspaceState({ agents: [agent("agent_1")] }); // no role → member
    mocks.documents = [{ id: "doc_1", path: "P.md", title: "P.md" }];
    renderWorkspace();

    expect(screen.queryByText("Bring in an AI teammate")).toBeNull();
    expect(screen.queryByText("Work with an agent")).toBeNull();
    expect(screen.queryByText("Add an AI teammate")).toBeNull();
    // The checklist is present (create-document done + start-discussion), no promotion.
    expect(screen.getByText("Finish setting up this workspace")).toBeTruthy();
  });
});

// The auto-open promotion matrix, driven through the real controller (which computes
// `promotable` from the engine) so the audience gate + activation-incomplete oracle are
// exercised end-to-end. Fresh vs catch-up is controlled via documentCount/route across
// rerenders; the two owner-flagged rows (A2 catch-up NORMAL copy, acknowledged-Done) are
// the cut-corner traps and get their own explicit cases.
describe("useOnboardingController — teammate promotion auto-open (#90)", () => {
  const promoBase = {
    enabled: true,
    accountId: "acct_promo",
    workspaceId: "ws_promo",
    selectionActive: false,
    nowMs: NOW_MS,
  };
  const PROMO_DISMISS_KEY = "codesk.onboarding.account.acct_promo.ws.ws_promo.teammatePromoDismissed";

  it("FRESH: first document 0→1 + navigated in auto-opens at connect WITH the bridge line", () => {
    const { result, rerender } = renderHook(
      (props: { documentCount: number; route: string }) =>
        useOnboardingController({ ...promoBase, roles: ["owner"], workspaceState: workspaceState(), ...props }),
      { initialProps: { documentCount: 0, route: "home" } },
    );
    // No document yet → nothing auto-opens.
    expect(result.current.chapterOpen).toBe(false);
    // Document created but NOT navigated into → still waits (fresh needs the navigation).
    rerender({ documentCount: 1, route: "home" });
    expect(result.current.chapterOpen).toBe(false);
    // Navigated into the new document → auto-open at the first incomplete card (connect) WITH
    // the fresh bridge line.
    rerender({ documentCount: 1, route: "document" });
    expect(result.current.chapterActive?.id).toBe("add-teammate-connect");
    expect(result.current.chapterOpen).toBe(true);
    expect(result.current.chapterBridge).toBe(true);
  });

  it("CATCH-UP (MANDATORY): a doc already present auto-opens with NORMAL copy — never the bridge", () => {
    const { result } = renderHook(() =>
      useOnboardingController({ ...promoBase, roles: ["owner"], route: "home", documentCount: 1, workspaceState: workspaceState() }),
    );
    // Auto-opens at the true first-incomplete card, but with NORMAL copy — the distinguishing
    // assertion is that the fresh bridge line is NEVER shown on catch-up.
    expect(result.current.chapterActive?.id).toBe("add-teammate-connect");
    expect(result.current.chapterOpen).toBe(true);
    expect(result.current.chapterBridge).toBe(false);
  });

  it("acknowledged-Done (MANDATORY): the Done card STAYS visible on completion; resumes ONLY on Close", () => {
    const { result, rerender } = renderHook(
      (props: { workspaceState: WorkspaceState }) =>
        useOnboardingController({ ...promoBase, roles: ["owner"], route: "document", documentCount: 1, ...props }),
      { initialProps: { workspaceState: workspaceState() } },
    );
    // Auto-opened at connect (catch-up).
    expect(result.current.chapterActive?.id).toBe("add-teammate-connect");
    expect(result.current.chapterOpen).toBe(true);

    // Setup completes while the chapter is open (env live + agent). The completion moment
    // renders the Done card and it STAYS visible — chapterOpen must NOT flip and the suspended
    // discussion spotlight must NOT silently swap in.
    rerender({ workspaceState: workspaceState({ daemons: [liveDaemonAt(NOW_MS)], agents: [agent("a1")] }) });
    expect(result.current.chapterActive?.id).toBe("add-teammate-done");
    expect(result.current.chapterOpen).toBe(true);

    // Only the user's Close flips it shut → the host then resumes the discussion spotlight.
    act(() => result.current.closeChapter());
    expect(result.current.chapterOpen).toBe(false);
    expect(result.current.chapterActive).toBeNull();
  });

  it("A1: obeys live truth — env already present auto-opens at Create (not an assumed Connect)", () => {
    const { result } = renderHook(() =>
      useOnboardingController({
        ...promoBase,
        roles: ["owner"],
        route: "document",
        documentCount: 1,
        workspaceState: workspaceState({ daemons: [liveDaemonAt(NOW_MS)] }), // env live, no agent
      }),
    );
    expect(result.current.chapterActive?.id).toBe("add-teammate-create");
    expect(result.current.chapterOpen).toBe(true);
  });

  it("A1 fresh: the bridge shows on the ACTUAL entry card (Create) when env already exists", () => {
    const { result, rerender } = renderHook(
      (props: { documentCount: number; route: string }) =>
        useOnboardingController({
          ...promoBase,
          roles: ["owner"],
          workspaceState: workspaceState({ daemons: [liveDaemonAt(NOW_MS)] }), // env live from the start
          ...props,
        }),
      { initialProps: { documentCount: 0, route: "home" } },
    );
    rerender({ documentCount: 1, route: "document" }); // fresh 0→1 + navigated
    expect(result.current.chapterActive?.id).toBe("add-teammate-create");
    expect(result.current.chapterBridge).toBe(true); // bridge on the entry card, whichever it is
  });

  it("member: never auto-opens (audience gate), even after creating and opening a document", () => {
    const { result, rerender } = renderHook(
      (props: { documentCount: number; route: string }) =>
        useOnboardingController({ ...promoBase, roles: ["member"], workspaceState: workspaceState({ agents: [agent("a1")] }), ...props }),
      { initialProps: { documentCount: 0, route: "home" } },
    );
    rerender({ documentCount: 1, route: "document" });
    expect(result.current.chapterOpen).toBe(false);
    expect(result.current.chapterActive).toBeNull();
  });

  it("complete: an already-activated owner workspace never auto-opens", () => {
    const { result } = renderHook(() =>
      useOnboardingController({
        ...promoBase,
        roles: ["owner"],
        route: "document",
        documentCount: 1,
        workspaceState: workspaceState({ daemons: [liveDaemonAt(NOW_MS)], agents: [agent("a1")] }), // env + agent
      }),
    );
    expect(result.current.chapterOpen).toBe(false);
  });

  it("'Not now' on an auto-open records promo-dismiss + suppresses a later reload; manual open still works", () => {
    const first = renderHook(() =>
      useOnboardingController({ ...promoBase, roles: ["owner"], route: "document", documentCount: 1, workspaceState: workspaceState() }),
    );
    expect(first.result.current.chapterOpen).toBe(true); // catch-up auto-open
    act(() => first.result.current.closeChapter());
    expect(first.result.current.chapterOpen).toBe(false);
    expect(window.localStorage.getItem(PROMO_DISMISS_KEY)).toBe("true");
    first.unmount();

    // Simulate a reload: a fresh controller in the same workspace must NOT auto-open again.
    const second = renderHook(() =>
      useOnboardingController({ ...promoBase, roles: ["owner"], route: "document", documentCount: 1, workspaceState: workspaceState() }),
    );
    expect(second.result.current.chapterOpen).toBe(false);
    // But a manual open from the checklist still works (dismissal suppresses AUTO-open only).
    act(() => second.result.current.openChapter());
    expect(second.result.current.chapterActive?.id).toBe("add-teammate-connect");
    expect(second.result.current.chapterBridge).toBe(false); // manual → normal copy
  });

  it("counter untouched across the interlude: the guide 2-of-2 is separate from the chapter's steps", () => {
    const { result } = renderHook(() =>
      useOnboardingController({ ...promoBase, roles: ["owner"], route: "document", documentCount: 1, workspaceState: workspaceState() }),
    );
    // The chapter auto-opens (interlude), but the guide counter reflects the SPOTLIGHT steps
    // only — threads-intro is step 2 of 2, untouched by the chapter's own 2-step counter.
    expect(result.current.chapterOpen).toBe(true);
    expect(result.current.total).toBe(2); // guide = 2 spotlight steps
    expect(result.current.stepIndex).toBe(1); // threads-intro = "2 of 2" (0-based 1)
    expect(result.current.chapterTotal).toBe(2); // the chapter's OWN, separate step count
    // The discussion spotlight is SUSPENDED behind the chapter, not lost — activeSpotlightId
    // still resolves to threads-intro, ready to resume the instant the chapter is closed (#90 bug 2).
    expect(result.current.active?.id).toBe("threads-intro");
  });

  it("FRESH mislabel guard (#90 bug 1): a startup 0→1 seen while DISABLED is catch-up, not fresh", () => {
    const { result, rerender } = renderHook(
      (props: { enabled: boolean; documentCount: number; route: string }) =>
        useOnboardingController({ ...promoBase, roles: ["owner"], workspaceState: workspaceState(), ...props }),
      { initialProps: { enabled: false, documentCount: 0, route: "home" } },
    );
    // Disabled startup (namespace not ready): no auto-open, and the 0-doc snapshot must NOT be
    // recorded as a baseline that a later doc would look like a "fresh" transition against.
    expect(result.current.chapterOpen).toBe(false);
    // Namespace becomes ready with a document ALREADY present — the app finishing startup for an
    // EXISTING workspace. This must auto-open CATCH-UP (normal title), never FRESH (bridge).
    rerender({ enabled: true, documentCount: 1, route: "document" });
    expect(result.current.chapterActive?.id).toBe("add-teammate-connect");
    expect(result.current.chapterOpen).toBe(true);
    expect(result.current.chapterBridge).toBe(false);
  });

  it("manual-open persistence (#90 bug 3): a manual open persists promo-dismiss; a reload does NOT auto-open", () => {
    // documentCount 0 so nothing auto-opens — the only open here is the deliberate manual one.
    const first = renderHook(() =>
      useOnboardingController({ ...promoBase, roles: ["owner"], route: "home", documentCount: 0, workspaceState: workspaceState() }),
    );
    expect(first.result.current.chapterOpen).toBe(false);
    act(() => first.result.current.openChapter());
    expect(first.result.current.chapterActive?.id).toBe("add-teammate-connect");
    // A deliberate open persists the browser-local promo-dismiss flag (they've seen it now).
    expect(window.localStorage.getItem(PROMO_DISMISS_KEY)).toBe("true");
    first.unmount();

    // Simulate a reload with a document now present (would otherwise catch-up auto-open):
    // because the user already opened it manually, it must NOT auto-open.
    const second = renderHook(() =>
      useOnboardingController({ ...promoBase, roles: ["owner"], route: "document", documentCount: 1, workspaceState: workspaceState() }),
    );
    expect(second.result.current.chapterOpen).toBe(false);
    // Manual reopen via the checklist stays available afterward.
    act(() => second.result.current.openChapter());
    expect(second.result.current.chapterActive?.id).toBe("add-teammate-connect");
  });
});
