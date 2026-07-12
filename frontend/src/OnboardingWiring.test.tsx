// @vitest-environment jsdom

// Integration coverage for the Batch-1 onboarding wiring in WorkspaceApp: the
// controller mounts the guide spotlight + getting-started checklist and feeds them
// from LIVE derived state, and the one non-derivable record site (member_invited)
// fires on a real invite. The pure derivation/adapter is covered in
// onboarding.test.ts / onboardingController.test.ts; this pins the App.tsx glue.

import { cleanup, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceApp, MembersAndInvite } from "./App";
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
  it("mounts the checklist + empty-workspace spotlight, honestly counted from live state", () => {
    mocks.workspace = workspaceState();
    mocks.documents = [];
    renderWorkspace();

    // Vitaliy's checklist, fed by the controller's checklistProgress.
    expect(screen.getByText("Finish setting up this workspace")).toBeTruthy();
    // Member role (no membership role set) → 5 eligible items, none done yet.
    expect(screen.getByText("0 of 5 done")).toBeTruthy();
    // The guided sequence's first step triggers on the empty workspace.
    expect(screen.getByText("These are real files")).toBeTruthy();
  });

  it("reflects a live signal: one agent marks 'create-agent' done (derived, no record)", () => {
    mocks.workspace = workspaceState({ agents: [agent("agent_1")] });
    mocks.documents = [];
    renderWorkspace();

    // agent-exists derives from workspace.agents — the checklist recomputes live.
    expect(screen.getByText("1 of 5 done")).toBeTruthy();
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
