// @vitest-environment jsdom

// Integration coverage for the Batch-1 onboarding wiring in WorkspaceApp: the
// controller mounts the guide spotlight + getting-started checklist and feeds them
// from LIVE derived state, and the one non-derivable record site (member_invited)
// fires on a real invite. The pure derivation/adapter is covered in
// onboarding.test.ts / onboardingController.test.ts; this pins the App.tsx glue.

import { cleanup, render, renderHook, screen, fireEvent, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceApp, MembersAndInvite, shouldRenderOnboardingChecklist } from "./App";
import { useOnboardingController } from "./onboardingController";
import type { OnboardingRole } from "./onboardingEngine";
import type { Account, Agent, WorkspaceState, WorkspaceSummary } from "./types";

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

  it("shows honest live checklist progress after the guide is complete", () => {
    window.localStorage.setItem(
      "codesk.onboarding.account.account_1.ws.workspace_1.flags",
      JSON.stringify(["seen:threads-intro@v1", "seen:watchers-intro@v1"]),
    );
    mocks.workspace = workspaceState({ agents: [agent("agent_1")] });
    mocks.documents = [{ id: "doc_1", path: "Product.md", title: "Product.md" }];
    renderWorkspace();

    // Member with an agent: the checklist is create-document (done) + start-discussion +
    // the "Work with an agent" entry (shown because an agent exists; completes on
    // agent-at-work) — all live-derived, no stored "done" flags. #56 replace-not-coexist
    // folded the old connect/create/agent-at-work rows into that single entry.
    expect(screen.getByText("1 of 3 done")).toBeTruthy();
    expect(screen.getByText("Work with an agent")).toBeTruthy();
    expect(screen.queryByText("These are real files")).toBeNull();
  });

  it("gates the checklist only for blocking guide spotlights, never for contextual tips", () => {
    expect(shouldRenderOnboardingChecklist(true, "spotlight")).toBe(false);
    expect(shouldRenderOnboardingChecklist(true, "tip")).toBe(true);
    expect(shouldRenderOnboardingChecklist(true, null)).toBe(true);
    expect(shouldRenderOnboardingChecklist(false, null)).toBe(false);
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
    watchedDocumentCount: 0,
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
