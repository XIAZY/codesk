// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CreateDaemonModal, WorkspaceApp, WorkspaceOnboarding } from "./App";
import { emptyWorkspace, identifierHelpText, identifierPattern } from "./logic";
import type { Account, WorkspaceSummary } from "./types";

vi.mock("./useWorkspace", () => ({
  useWorkspace: () => ({
    workspace: {
      ...emptyWorkspace(),
      workspaceId: "ws",
      rootDocumentId: "doc_root",
      name: "Workspace",
      currentUserId: "user_1",
      users: [
        { id: "user_1", handle: "ada", name: "Ada", role: "", kind: "human", status: "active", updatedAt: "now" },
        { id: "user_2", handle: "grace", name: "Grace", role: "", kind: "human", status: "active", updatedAt: "now" },
      ],
      daemons: [
        { id: "daemon_1", workspaceId: "ws", name: "Local", status: "active", connectionStatus: "online", createdAt: "now" },
      ],
      agents: [
        {
          id: "agent_1",
          daemonId: "daemon_1",
          handle: "codex",
          name: "Codex",
          role: "Review",
          kind: "codex",
          workspaceRoot: "agents/agent_1",
          status: "idle",
          currentTask: "",
          currentActivity: "",
          currentRunId: "",
          updatedAt: "now",
        },
      ],
    },
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
    upsertFile: vi.fn(),
    moveFile: vi.fn(),
    tombstoneFile: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
});

function account(): Account {
  return {
    id: "account_1",
    email: "owner@example.com",
    displayName: "Owner",
  };
}

describe("WorkspaceOnboarding", () => {
  it("shows create workspace and invite-link entry points", () => {
    render(
      <WorkspaceOnboarding
        api={{ createWorkspace: vi.fn() }}
        account={account()}
        workspaces={[]}
        onWorkspaces={vi.fn()}
        onSelect={vi.fn()}
        onSignOut={vi.fn()}
      />
    );

    expect(screen.getByRole("heading", { name: "Create a workspace" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Join with invite link" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Join with invite link" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Choose a workspace" })).toBeNull();
  });

  it("opens an invite route from a pasted invite link", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/new");

    render(
      <WorkspaceOnboarding
        api={{ createWorkspace: vi.fn() }}
        account={account()}
        workspaces={[]}
        onWorkspaces={vi.fn()}
        onSelect={vi.fn()}
        onSignOut={vi.fn()}
      />
    );

    await user.type(screen.getByLabelText("Invite link"), "https://notty.example/invite/abc123");
    await user.click(screen.getByRole("button", { name: "Join with invite link" }));

    expect(window.location.pathname).toBe("/invite/abc123");
  });

  it("auto-fills workspace slug until the slug is manually edited", async () => {
    const user = userEvent.setup();
    render(
      <WorkspaceOnboarding
        api={{ createWorkspace: vi.fn() }}
        account={account()}
        workspaces={[]}
        onWorkspaces={vi.fn()}
        onSelect={vi.fn()}
        onSignOut={vi.fn()}
      />
    );

    const name = screen.getByLabelText("Workspace name") as HTMLInputElement;
    const slug = screen.getByLabelText("Workspace slug") as HTMLInputElement;
    const handle = screen.getByLabelText("Your handle in this workspace") as HTMLInputElement;

    expect(slug.value).toBe("product-workspace");
    expect(slug.getAttribute("pattern")).toBe(identifierPattern);
    expect(slug.getAttribute("title")).toBe(identifierHelpText);
    expect(handle.getAttribute("pattern")).toBe(identifierPattern);
    expect(handle.getAttribute("title")).toBe(identifierHelpText);

    await user.clear(name);
    await user.type(name, "Research Lab!");
    expect(slug.value).toBe("research-lab");

    await user.clear(slug);
    await user.type(slug, "custom_slug");
    await user.clear(name);
    await user.type(name, "Changed Workspace");
    expect(slug.value).toBe("custom_slug");
  });

  it("creates the first workspace and selects it from onboarding", async () => {
    const user = userEvent.setup();
    const created: WorkspaceSummary = { id: "workspace_new", slug: "product-workspace", name: "Product Workspace" };
    const createWorkspace = vi.fn().mockResolvedValue({ workspace: created });
    const onWorkspaces = vi.fn();
    const onSelect = vi.fn();

    render(
      <WorkspaceOnboarding
        api={{ createWorkspace }}
        account={account()}
        workspaces={[]}
        onWorkspaces={onWorkspaces}
        onSelect={onSelect}
        onSignOut={vi.fn()}
      />
    );

    await user.click(screen.getByRole("button", { name: "Create and enter" }));

    await waitFor(() => expect(createWorkspace).toHaveBeenCalledWith({ name: "Product Workspace", slug: "product-workspace", handle: "owner" }));
    expect(onWorkspaces).toHaveBeenCalledWith([created]);
    expect(onSelect).toHaveBeenCalledWith(created);
  });

  it("shows backend creation errors inline without selecting a workspace", async () => {
    const user = userEvent.setup();
    const createWorkspace = vi.fn().mockRejectedValue(new Error("Workspace slug is already taken."));
    const onSelect = vi.fn();

    render(
      <WorkspaceOnboarding
        api={{ createWorkspace }}
        account={account()}
        workspaces={[]}
        onWorkspaces={vi.fn()}
        onSelect={onSelect}
        onSignOut={vi.fn()}
      />
    );

    await user.click(screen.getByRole("button", { name: "Create and enter" }));

    expect(await screen.findByText("Workspace slug is already taken.")).toBeTruthy();
    expect(onSelect).not.toHaveBeenCalled();
  });
});

describe("WorkspaceApp coworkers rail", () => {
  it("renames people to coworkers, counts humans plus agents, and keeps agents flat", async () => {
    render(
      <WorkspaceApp
        api={{ updateLastAccessed: vi.fn().mockResolvedValue({}) } as never}
        token="token"
        workspaceId="ws"
        workspaceSlug="workspace"
        view={{ kind: "home" }}
        account={{ id: "account_1", email: "you@example.com", displayName: "You" }}
        workspaces={[{ id: "ws", slug: "workspace", name: "Workspace" }]}
        onAccess={vi.fn()}
        onWorkspaceChange={vi.fn()}
        onSignOut={vi.fn()}
      />,
    );

    const tab = screen.getByRole("button", { name: /coworkers/i });
    expect(within(tab).getByText("3")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /people/i })).toBeNull();

    fireEvent.click(tab);

    await waitFor(() => expect(document.querySelector(".ctx-body.people-pane")).toBeTruthy());
    const coworkersPanel = document.querySelector(".ctx-body.people-pane") as HTMLElement;
    expect(within(coworkersPanel).getByText("People")).toBeTruthy();
    expect(within(coworkersPanel).getByText("@ada")).toBeTruthy();
    expect(within(coworkersPanel).getByText("@grace")).toBeTruthy();
    expect(within(coworkersPanel).getByText("Agents")).toBeTruthy();
    expect(within(coworkersPanel).queryByText("Daemons")).toBeNull();
    expect(coworkersPanel.querySelector(".grp-head")).toBeNull();
    expect(within(coworkersPanel).getByRole("button", { name: /@codex/i })).toBeTruthy();
    expect(within(coworkersPanel).queryByText("Local")).toBeNull();
  });
});

describe("CreateDaemonModal install status", () => {
  it("flips the install chip from waiting to connected when the daemon checks in live", async () => {
    const user = userEvent.setup();
    const created = { id: "daemon_new", workspaceId: "ws", name: "Local daemon", status: "active", connectionStatus: "disconnected", createdAt: "now" };
    const api = { createDaemon: vi.fn().mockResolvedValue({ daemon: created, token: "nottyd_secret" }) };

    const { rerender } = render(
      <CreateDaemonModal api={api as never} workspaceId="ws" daemons={[]} onClose={vi.fn()} onDone={vi.fn()} />
    );

    await user.click(screen.getByRole("button", { name: "Create daemon" }));

    // Daemon created but has not checked in yet: the chip must say waiting.
    expect(await screen.findByText("Waiting for daemon to check in…")).toBeTruthy();
    expect(screen.queryByText("Daemon connected")).toBeNull();

    // A daemon.updated event lands via the workspace socket, so live state now reports
    // the daemon online. The chip must flip without a manual refresh.
    rerender(
      <CreateDaemonModal api={api as never} workspaceId="ws" daemons={[{ ...created, connectionStatus: "online" }]} onClose={vi.fn()} onDone={vi.fn()} />
    );

    expect(screen.getByText("Daemon connected")).toBeTruthy();
    expect(screen.queryByText("Waiting for daemon to check in…")).toBeNull();
  });
});
