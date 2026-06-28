// @vitest-environment jsdom

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceApp } from "./App";
import type { Account, WorkspaceState, WorkspaceSummary } from "./types";

const mocks = vi.hoisted(() => ({
  workspace: null as WorkspaceState | null,
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
    documents: [],
    ready: true,
    connected: true,
    upsertFile: vi.fn(),
    moveFile: vi.fn(),
    tombstoneFile: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
  mocks.workspace = null;
});

function workspaceState(): WorkspaceState {
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
  };
}

function account(): Account {
  return {
    id: "account_1",
    email: "real.email@example.com",
    displayName: "Real Email",
  };
}

describe("WorkspaceApp identity display", () => {
  it("renders the current workspace user handle instead of an email-derived handle", () => {
    mocks.workspace = workspaceState();
    const workspaces: WorkspaceSummary[] = [{ id: "workspace_1", slug: "product-workspace", name: "Product Workspace" }];

    render(
      <WorkspaceApp
        api={{} as never}
        token="token"
        workspaceId="workspace_1"
        account={account()}
        workspaces={workspaces}
        onWorkspaceChange={vi.fn()}
        onSignOut={vi.fn()}
      />
    );

    expect(screen.getAllByText("@owner_handle").length).toBeGreaterThan(0);
    expect(screen.queryByText("@real.email")).toBeNull();
  });
});
