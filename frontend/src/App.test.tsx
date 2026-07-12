// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AgentDetailModal, CreateDaemonModal, DaemonDetailModal, DaemonsManagement, documentFolders, ManageModal, MoveDocumentModal, WorkspaceApp, WorkspaceOnboarding } from "./App";
import { ApiError } from "./api";
import { emptyWorkspace, identifierFromName, identifierHelpText, identifierPattern, workspaceSlugMaxLength } from "./logic";
import { daemonFixtures, withReceipt } from "./daemonFixtures";
import type { Account, Agent, AgentRun, Daemon, DocumentItem, WorkspaceState, WorkspaceSummary } from "./types";

function workspaceFixture(overrides: Partial<WorkspaceState> = {}): WorkspaceState {
  return {
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
        updatedAt: "2026-07-06T12:00:00Z",
      },
    ],
    ...overrides,
  };
}

let workspaceMock = workspaceFixture();
let rootDocumentsMock: DocumentItem[] = [{ id: "doc_1", path: "docs/Product Plan.md", title: "Product Plan.md" }];

vi.mock("./useWorkspace", () => ({
  useWorkspace: () => ({
    workspace: workspaceMock,
    connected: true,
    loading: false,
    error: "",
    reload: vi.fn(),
  }),
}));

vi.mock("./useRootNamespace", () => ({
  useRootNamespace: () => ({
    documents: rootDocumentsMock,
    ready: true,
    upsertFile: vi.fn(),
    moveFile: vi.fn(),
    tombstoneFile: vi.fn(),
  }),
}));

afterEach(() => {
  workspaceMock = workspaceFixture();
  rootDocumentsMock = [{ id: "doc_1", path: "docs/Product Plan.md", title: "Product Plan.md" }];
  vi.useRealTimers();
  localStorage.clear();
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

    expect(name.value).toBe("");
    expect(slug.value).toBe("");
    expect(slug.getAttribute("pattern")).toBe(identifierPattern);
    expect(slug.getAttribute("title")).toBe(identifierHelpText);
    expect(handle.getAttribute("pattern")).toBe(identifierPattern);
    expect(handle.getAttribute("title")).toBe(identifierHelpText);

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
    const created: WorkspaceSummary = { id: "workspace_new", slug: "research-lab", name: "Research Lab" };
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

    await user.type(screen.getByLabelText("Workspace name"), "Research Lab");
    await user.click(screen.getByRole("button", { name: "Create and enter" }));

    await waitFor(() => expect(createWorkspace).toHaveBeenCalledWith({ name: "Research Lab", slug: "research-lab", handle: "owner" }));
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

    await user.type(screen.getByLabelText("Workspace name"), "Research Lab");
    await user.click(screen.getByRole("button", { name: "Create and enter" }));

    expect(await screen.findByText("Workspace slug is already taken.")).toBeTruthy();
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("randomizing the name fills a non-empty name and auto-derives a matching slug", async () => {
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

    await user.click(screen.getByLabelText("Generate a random name"));

    expect(name.value.length).toBeGreaterThan(0);
    expect(slug.value.length).toBeGreaterThan(0);
    expect(slug.value).toBe(identifierFromName(name.value, workspaceSlugMaxLength));
  });

  it("randomizing the name does not clobber a hand-edited slug", async () => {
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

    const slug = screen.getByLabelText("Workspace slug") as HTMLInputElement;

    await user.type(slug, "custom_slug");
    await user.click(screen.getByLabelText("Generate a random name"));

    expect(slug.value).toBe("custom_slug");
  });

  it("cannot submit a blank form because the name is required", async () => {
    const user = userEvent.setup();
    const createWorkspace = vi.fn();
    render(
      <WorkspaceOnboarding
        api={{ createWorkspace }}
        account={account()}
        workspaces={[]}
        onWorkspaces={vi.fn()}
        onSelect={vi.fn()}
        onSignOut={vi.fn()}
      />
    );

    const name = screen.getByLabelText("Workspace name") as HTMLInputElement;
    expect(name.value).toBe("");
    expect(name.hasAttribute("required")).toBe(true);

    await user.click(screen.getByRole("button", { name: "Create and enter" }));

    expect(createWorkspace).not.toHaveBeenCalled();
  });
});

describe("documentFolders", () => {
  it("derives root + nested folders from document paths, pre-ordered with depth", () => {
    const docs = [
      { id: "1", path: "Drafts/a.md", title: "a" },
      { id: "2", path: "Specs/API/v3/b.md", title: "b" },
      { id: "3", path: "c.md", title: "c" },
    ] as DocumentItem[];
    // Folders are path prefixes only (no folder entities): root first, each parent before its children, with
    // depth for indentation. The root-level file contributes no folder.
    expect(documentFolders(docs).map((folder) => `${folder.depth}:${folder.path}`)).toEqual([
      "0:",
      "1:Drafts",
      "1:Specs",
      "2:Specs/API",
      "3:Specs/API/v3",
    ]);
  });

  it("orders segment-wise so a child never renders under a prefix-overlapping sibling", () => {
    const docs = [
      { id: "1", path: "notes/a.md", title: "a" },
      { id: "2", path: "notes-old/b.md", title: "b" },
      { id: "3", path: "notes/x/c.md", title: "c" },
    ] as DocumentItem[];
    // A flat string sort puts `notes-old` before `notes/x` ('-' < '/'), mis-parenting the depth-2 child under
    // `notes-old`. Segment-wise keeps `notes/x` directly under `notes`, then `notes-old` after the subtree.
    expect(documentFolders(docs).map((folder) => folder.path)).toEqual([
      "",
      "notes",
      "notes/x",
      "notes-old",
    ]);
  });
});

describe("MoveDocumentModal", () => {
  const doc = { id: "d1", path: "Docs/Product.md", title: "Product.md" } as DocumentItem;
  function renderMove(others: DocumentItem[], onMove = vi.fn()) {
    render(<MoveDocumentModal document={doc} documents={[doc, ...others]} onClose={vi.fn()} onMove={onMove} />);
    return onMove;
  }
  const moveButton = () => screen.getByRole("button", { name: "Move document" }) as HTMLButtonElement;

  it("rejects a \"..\" folder name whose committed path would differ from the preview", () => {
    renderMove([]);
    fireEvent.change(screen.getByLabelText(/New folder in/i), { target: { value: ".." } });
    expect(moveButton().disabled).toBe(true);
    expect(screen.getByText(/isn't allowed/i)).toBeTruthy();
    // WYSIWYG: the preview shows the normalized committed path, never the raw "Docs/../Product.md".
    expect(screen.queryByText(/\.\.\//)).toBeNull();
  });

  it("rejects a dot-prefixed folder name the daemon's visible-root contract would refuse", () => {
    renderMove([]);
    fireEvent.change(screen.getByLabelText(/New folder in/i), { target: { value: ".secret" } });
    expect(moveButton().disabled).toBe(true);
    expect(screen.getByText(/isn't allowed/i)).toBeTruthy();
  });

  it("blocks a move to a case-insensitively occupied path", () => {
    // Another doc occupies Other/PRODUCT.md; selecting Other targets Other/Product.md — occupied regardless of case.
    renderMove([{ id: "d2", path: "Other/PRODUCT.md", title: "PRODUCT.md" } as DocumentItem]);
    fireEvent.click(screen.getByRole("option", { name: /Other/i }));
    expect(moveButton().disabled).toBe(true);
  });

  it("commits the exact selected target path via moveFile", async () => {
    const onMove = renderMove([{ id: "d2", path: "Specs/x.md", title: "x.md" } as DocumentItem]);
    fireEvent.click(screen.getByRole("option", { name: /Specs/i }));
    fireEvent.click(moveButton());
    await waitFor(() => expect(onMove).toHaveBeenCalledWith("Specs/Product.md"));
  });
});

describe("WorkspaceApp right rail", () => {
  it("has no right rail at all — Activity moved into the … menu, subscribers into the Watchers popover", () => {
    const { container } = render(
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

    // The whole right rail (.ctx / .ctx-tabs) is gone — the kill-the-sidebar finish. Activity lives in the "…"
    // menu, subscribers in the top-bar Watchers popover; neither a Participants nor a Document Activity tab
    // remains in the chrome.
    expect(container.querySelector(".ctx")).toBeNull();
    expect(container.querySelector(".ctx-tabs")).toBeNull();
    expect(screen.queryByRole("button", { name: /participants/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /document activity/i })).toBeNull();
  });
});

describe("WorkspaceApp agent status rail", () => {
  it("renders left-rail agent status as readable copy, not only a colored dot", () => {
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

    const agentButton = screen.getByRole("button", { name: "Open @codex. Status: Idle" });
    expect(within(agentButton).getByText("@codex")).toBeTruthy();
    const status = within(agentButton).getByLabelText("Status: Idle");
    expect(status.textContent).toContain("Idle");
    expect(status.getAttribute("title")).toBe("Standing by");
  });
});

describe("WorkspaceApp workspace management", () => {
  it("updates the workspace list before navigating to a changed slug", async () => {
    const user = userEvent.setup();
    const calls: string[] = [];
    const updateWorkspaceSettings = vi.fn().mockResolvedValue({
      workspace: { id: "ws", slug: "new-slug", name: "Workspace", defaultRuntime: "codex" },
    });
    workspaceMock = workspaceFixture({ currentMembershipRole: "owner", slug: "workspace", defaultRuntime: "" });
    window.history.replaceState(null, "", "/w/workspace");

    render(
      <WorkspaceApp
        api={{ updateLastAccessed: vi.fn().mockResolvedValue({}), updateWorkspaceSettings } as never}
        token="token"
        workspaceId="ws"
        workspaceSlug="workspace"
        view={{ kind: "home" }}
        account={{ id: "account_1", email: "you@example.com", displayName: "You" }}
        workspaces={[{ id: "ws", slug: "workspace", name: "Workspace" }]}
        onAccess={vi.fn()}
        onWorkspaceUpdated={(workspace) => calls.push(`updated:${workspace.slug}`)}
        onWorkspaceChange={vi.fn()}
        onSignOut={vi.fn()}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Manage / Settings" }));
    await user.click(screen.getByRole("button", { name: "Workspace settings" }));
    await user.clear(screen.getByLabelText("Workspace URL slug"));
    await user.type(screen.getByLabelText("Workspace URL slug"), "new-slug");
    await user.selectOptions(screen.getByLabelText("Default agent runtime"), "codex");
    await user.click(screen.getByRole("button", { name: "Save settings" }));

    await waitFor(() => expect(window.location.pathname).toBe("/w/new-slug"));
    expect(calls[0]).toBe("updated:new-slug");
  });

  it("navigates away only after workspace.deleted clears the live workspace state", async () => {
    const onWorkspaceDeleted = vi.fn();
    const props = {
      api: { updateLastAccessed: vi.fn().mockResolvedValue({}) } as never,
      token: "token",
      workspaceId: "ws",
      workspaceSlug: "workspace",
      view: { kind: "home" } as const,
      account: { id: "account_1", email: "you@example.com", displayName: "You" },
      workspaces: [
        { id: "ws", slug: "workspace", name: "Workspace" },
        { id: "other", slug: "other", name: "Other" },
      ],
      onAccess: vi.fn(),
      onWorkspaceDeleted,
      onWorkspaceChange: vi.fn(),
      onSignOut: vi.fn(),
    };
    workspaceMock = workspaceFixture({ workspaceId: "ws", name: "Workspace" });
    window.history.replaceState(null, "", "/w/workspace");

    const { rerender } = render(<WorkspaceApp {...props} />);
    await waitFor(() => expect(screen.getByText("Manage / Settings")).toBeTruthy());
    expect(onWorkspaceDeleted).not.toHaveBeenCalled();

    workspaceMock = emptyWorkspace();
    rerender(<WorkspaceApp {...props} />);

    await waitFor(() => expect(onWorkspaceDeleted).toHaveBeenCalledWith("ws"));
    expect(window.location.pathname).toBe("/w/other");
  });
});

describe("WorkspaceApp coming-soon controls", () => {
  it("shows the sidebar search as a non-actionable Coming soon affordance", () => {
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

    const label = screen.getByText("Search — coming soon");
    expect(label.closest("[aria-disabled='true']")).toBeTruthy();
    expect(screen.queryByText("Search or jump…")).toBeNull(); // old placeholder gone
    expect(screen.queryByText("⌘K")).toBeNull(); // no fake keyboard shortcut
  });
});

describe("CreateDaemonModal install status", () => {
  it("flips the install chip from waiting to connected when the daemon checks in live", async () => {
    const user = userEvent.setup();
    const created: Daemon = { ...daemonFixtures.dead, id: "daemon_new", name: "Local daemon" };
    const api = { createDaemon: vi.fn().mockResolvedValue({ daemon: created, token: "nottyd_secret" }) };

    const { rerender } = render(
      <CreateDaemonModal api={api as never} workspaceId="ws" daemons={[]} onClose={vi.fn()} onDone={vi.fn()} />
    );

    await user.click(screen.getByRole("button", { name: "Create local environment" }));

    // Local environment created but has not checked in yet: the chip must say waiting.
    expect(await screen.findByText("Waiting for local environment to check in…")).toBeTruthy();
    expect(screen.queryByText("Local environment connected")).toBeNull();

    // A daemon.updated event lands via the workspace socket, so live state now reports
    // the daemon online. The chip must flip without a manual refresh.
    rerender(
      <CreateDaemonModal api={api as never} workspaceId="ws" daemons={[{ ...daemonFixtures.justSeen, id: "daemon_new", name: "Local daemon" }]} onClose={vi.fn()} onDone={vi.fn()} />
    );

    expect(screen.getByText("Local environment connected")).toBeTruthy();
    expect(screen.queryByText("Waiting for local environment to check in…")).toBeNull();
  });
});

describe("ManageModal", () => {
  const baseProps = {
    api: {} as never,
    workspaceId: "ws",
    workspaceSlug: "acme",
    canInvite: true,
    groupedAgents: [],
    onTabChange: vi.fn(),
    onClose: vi.fn(),
    onRefresh: vi.fn(),
    onNewDaemon: vi.fn(),
    onDaemon: vi.fn(),
    onNewAgent: vi.fn(),
    onAgent: vi.fn(),
  };

  it("renders all five tabs, shows the Local environment surface, delegates tab clicks, and closes on Escape", async () => {
    const user = userEvent.setup();
    const onTabChange = vi.fn();
    const onClose = vi.fn();
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", daemons: [daemonFixtures.justSeen] };
    render(
      <ManageModal {...baseProps} workspace={workspace as never} activeTab="local-env" onTabChange={onTabChange} onClose={onClose} />
    );

    for (const label of ["Members & Invite", "Agents", "Local environment", "Workspace settings", "Danger zone"]) {
      expect(screen.getByRole("button", { name: label })).toBeTruthy();
    }
    // Local environment tab hosts the migrated management surface (renamed heading).
    expect(screen.getAllByText("Local environments").length).toBeGreaterThan(0);

    await user.click(screen.getByRole("button", { name: "Danger zone" }));
    expect(onTabChange).toHaveBeenCalledWith("danger");

    await user.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("saves workspace name, slug, and default runtime with owner-only slug editing", async () => {
    const user = userEvent.setup();
    const updateWorkspaceSettings = vi.fn().mockResolvedValue({
      workspace: { id: "ws", name: "Acme Labs", slug: "acme-labs", defaultRuntime: "claude" },
    });
    const onWorkspaceSaved = vi.fn();
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", name: "Acme", slug: "acme", defaultRuntime: "", currentMembershipRole: "owner" };
    render(
      <ManageModal
        {...baseProps}
        api={{ updateWorkspaceSettings } as never}
        onWorkspaceSaved={onWorkspaceSaved}
        workspace={workspace as never}
        workspaceSlug="acme"
        activeTab="workspace"
      />
    );

    await user.clear(screen.getByLabelText("Workspace name"));
    await user.type(screen.getByLabelText("Workspace name"), "Acme Labs");
    await user.clear(screen.getByLabelText("Workspace URL slug"));
    await user.type(screen.getByLabelText("Workspace URL slug"), "acme-labs");
    await user.selectOptions(screen.getByLabelText("Default agent runtime"), "claude");
    await user.click(screen.getByRole("button", { name: "Save settings" }));

    expect(updateWorkspaceSettings).toHaveBeenCalledWith("ws", {
      name: "Acme Labs",
      slug: "acme-labs",
      defaultRuntime: "claude",
    });
    expect(onWorkspaceSaved).toHaveBeenCalledWith({ id: "ws", name: "Acme Labs", slug: "acme-labs", defaultRuntime: "claude" });
    expect(await screen.findByText("Workspace settings saved.")).toBeTruthy();
  });

  it("lets admins change name/runtime but not the slug", async () => {
    const user = userEvent.setup();
    const updateWorkspaceSettings = vi.fn().mockResolvedValue({
      workspace: { id: "ws", name: "Admin name", slug: "acme", defaultRuntime: "codex" },
    });
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", name: "Acme", slug: "acme", defaultRuntime: "", currentMembershipRole: "admin" };
    render(
      <ManageModal
        {...baseProps}
        api={{ updateWorkspaceSettings } as never}
        workspace={workspace as never}
        workspaceSlug="acme"
        activeTab="workspace"
      />
    );

    expect((screen.getByLabelText("Workspace URL slug") as HTMLInputElement).disabled).toBe(true);
    await user.clear(screen.getByLabelText("Workspace name"));
    await user.type(screen.getByLabelText("Workspace name"), "Admin name");
    await user.selectOptions(screen.getByLabelText("Default agent runtime"), "codex");
    await user.click(screen.getByRole("button", { name: "Save settings" }));

    expect(updateWorkspaceSettings).toHaveBeenCalledWith("ws", {
      name: "Admin name",
      defaultRuntime: "codex",
    });
  });

  it("surfaces slug conflicts distinctly from validation errors", async () => {
    const user = userEvent.setup();
    const updateWorkspaceSettings = vi.fn().mockRejectedValue(new ApiError(409, "Workspace slug is already taken."));
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", name: "Acme", slug: "acme", currentMembershipRole: "owner" };
    render(
      <ManageModal
        {...baseProps}
        api={{ updateWorkspaceSettings } as never}
        workspace={workspace as never}
        workspaceSlug="acme"
        activeTab="workspace"
      />
    );

    await user.clear(screen.getByLabelText("Workspace URL slug"));
    await user.type(screen.getByLabelText("Workspace URL slug"), "taken");
    await user.click(screen.getByRole("button", { name: "Save settings" }));

    expect(await screen.findByText("Slug taken. Choose another workspace URL.")).toBeTruthy();
  });

  it("requires exact workspace-name confirmation before deleting", async () => {
    const user = userEvent.setup();
    const deleteWorkspace = vi.fn().mockResolvedValue(undefined);
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", name: "Acme", currentMembershipRole: "owner" };
    render(<ManageModal {...baseProps} api={{ deleteWorkspace } as never} workspace={workspace as never} activeTab="danger" />);

    const deleteButton = screen.getByRole("button", { name: "Delete workspace" });
    expect((deleteButton as HTMLButtonElement).disabled).toBe(true);

    await user.type(screen.getByLabelText("Type Acme"), "acme");
    expect((deleteButton as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("Workspace name must match exactly.")).toBeTruthy();

    await user.clear(screen.getByLabelText("Type Acme"));
    await user.type(screen.getByLabelText("Type Acme"), "Acme");
    await user.click(deleteButton);

    expect(deleteWorkspace).toHaveBeenCalledWith("ws", "Acme");
    expect(await screen.findByText("Deletion requested. Waiting for workspace removal...")).toBeTruthy();
  });

  it("lists workspace members and offers invite generation on the Members tab", () => {
    const workspace = {
      ...emptyWorkspace(),
      workspaceId: "ws",
      users: [
        { id: "u1", handle: "ada", name: "Ada Lovelace", role: "Owner", kind: "human", status: "active", updatedAt: "now" },
        { id: "u2", handle: "grace", name: "Grace Hopper", role: "Member", kind: "human", status: "active", updatedAt: "now" },
        { id: "a1", handle: "codex", name: "Codex", role: "Agent", kind: "agent", status: "active", updatedAt: "now" },
      ],
    };
    render(<ManageModal {...baseProps} workspace={workspace as never} activeTab="members" />);
    // Human members are listed; the agent is not (agents live in the Agents tab).
    expect(screen.getByText("Ada Lovelace")).toBeTruthy();
    expect(screen.getByText("Grace Hopper")).toBeTruthy();
    expect(screen.queryByText("Codex")).toBeNull();
    // Owners/admins see invite generation.
    expect(screen.getByRole("button", { name: "Generate invite link" })).toBeTruthy();
  });

  it("hides invite generation from members without permission", () => {
    const workspace = { ...emptyWorkspace(), workspaceId: "ws" };
    render(<ManageModal {...baseProps} canInvite={false} workspace={workspace as never} activeTab="members" />);
    expect(screen.queryByRole("button", { name: "Generate invite link" })).toBeNull();
    expect(screen.getByText("Only workspace owners and admins can invite new members.")).toBeTruthy();
  });

  it("hosts the agents configuration surface on the Agents tab", () => {
    const workspace = {
      ...emptyWorkspace(),
      workspaceId: "ws",
      agents: [
        { id: "a1", daemonId: "d1", handle: "codex", name: "Codex", role: "Reviewer", kind: "codex", workspaceRoot: "agents/a1", status: "idle", currentTask: "", currentActivity: "", currentRunId: "", updatedAt: "now" },
      ],
    };
    const grouped = [{ daemonId: "d1", daemonName: "Local", agents: workspace.agents }];
    render(<ManageModal {...baseProps} workspace={workspace as never} groupedAgents={grouped as never} activeTab="agents" />);
    // AgentsManagement renders in the tab (its subtitle + the agent handle).
    expect(screen.getByText(/Codex collaborators in this workspace/)).toBeTruthy();
    expect(screen.getByText("@codex")).toBeTruthy();
  });

  it("simplifies the roster card to identity + status; full handle rendered, meta moved to the detail view", () => {
    const longAgent = {
      id: "a1",
      daemonId: "d1",
      handle: "codex-super-long-collaborator-name",
      name: "Codex",
      role: "Reviewer with a very long workspace collaboration role",
      kind: "codex",
      workspaceRoot: "agents/a1",
      status: "idle",
      currentTask: "",
      currentActivity: "Waiting for local environment diagnostics",
      currentRunId: "",
      updatedAt: "2026-07-06T12:00:00Z",
    };
    const workspace = {
      ...emptyWorkspace(),
      workspaceId: "ws",
      daemons: [{ ...daemonFixtures.justSeen, id: "d1" }],
      agents: [longAgent],
      agentEvents: [
        { id: "event_1", agentId: "a1", agentHandle: longAgent.handle, type: "document.updated", box: "for_me", status: "pending", documentId: "doc_1", summary: "Review this very long workspace document", createdAt: "2026-07-06T12:00:00Z", updatedAt: "2026-07-06T12:00:00Z" },
      ],
    };
    const grouped = [{ daemonId: "d1", daemonName: "Local", agents: workspace.agents }];
    render(<ManageModal {...baseProps} workspace={workspace as never} groupedAgents={grouped as never} activeTab="agents" />);

    const card = screen.getByRole("button", { name: /Open @codex-super-long-collaborator-name/ }) as HTMLElement;
    expect(card.querySelector(".agent-roster-top")).toBeTruthy();
    expect(card.querySelector(".agent-roster-status .agent-chip-text")).toBeTruthy();
    // Full @handle is present in the DOM (one row per agent — no longer squeezed into a
    // 3-col grid that truncated names to "@…").
    expect(card.querySelector(".agent-roster-identity b")?.textContent).toBe("@codex-super-long-collaborator-name");
    // threads / for-me chip are moved into the agent detail (the whole row opens it),
    // so they are no longer rendered on the roster card.
    expect(card.querySelector(".agent-roster-meta")).toBeNull();
    expect(card.querySelector(".agent-for-me-chip")).toBeNull();
  });
});

describe("DaemonsManagement liveness decay", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  // Read the status chip text of every table row (one .chip.sm per row, in the Status cell).
  const rowChips = (container: HTMLElement) =>
    Array.from(container.querySelectorAll("td .chip.sm")).map((chip) => (chip.textContent ?? "").trim());
  // The "Last check-in" cell is the only `small muted` td that is not the `mono` fingerprint cell.
  const lastCheckIn = (container: HTMLElement) =>
    (container.querySelector("td.small.muted:not(.mono)")?.textContent ?? "").trim();
  // MetricCard renders a `.label` and a `.metric-value`; look the value up by its card label.
  const metricValue = (container: HTMLElement, label: string) => {
    const card = Array.from(container.querySelectorAll(".metric-card")).find(
      (node) => node.querySelector(".label")?.textContent === label
    );
    return Number(card?.querySelector(".metric-value")?.textContent);
  };

  it("decays a silent daemon online -> stale -> disconnected with no further events", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-06T00:00:00Z"));
    const start = Date.now();
    // justSeen (age 0, online) stamped with the receipt time — the same online snapshot as before.
    const daemon = withReceipt(daemonFixtures.justSeen, start);
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", daemons: [daemon] };
    const { container } = render(
      <DaemonsManagement workspace={workspace as never} onRefresh={vi.fn()} onNew={vi.fn()} onDaemon={vi.fn()} />
    );
    const statusChip = () => container.querySelector("td .chip.sm")?.textContent ?? "";

    expect(statusChip()).toContain("online");

    // No events arrive; only time passes. The ticker (12s cadence) must re-derive the status
    // once elapsed crosses each window — advance past a tick boundary beyond the threshold.
    act(() => { vi.advanceTimersByTime(36_000); });
    expect(statusChip()).toContain("stale");

    act(() => { vi.advanceTimersByTime(120_000); });
    expect(statusChip()).toContain("disconnected");
  });

  it("shows a never-seen daemon as disconnected with 'never' check-in from the first frame and across ticks", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-06T00:00:00Z"));
    const now = Date.now();
    // neverSeen carries lastSeenAgeSeconds 0 and a zero lastSeenAt — the exact shape that used to
    // fabricate a transient "online" (bug 1). It must read disconnected/never from the very first paint.
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", daemons: [withReceipt(daemonFixtures.neverSeen, now)] };
    const { container } = render(
      <DaemonsManagement workspace={workspace as never} onRefresh={vi.fn()} onNew={vi.fn()} onDaemon={vi.fn()} />
    );

    expect(rowChips(container)).toEqual(["disconnected"]);
    expect(lastCheckIn(container)).toBe("never");

    // Two 12s ticker cycles pass with no events — it must never flip to online.
    act(() => { vi.advanceTimersByTime(24_000); });
    expect(rowChips(container)).toEqual(["disconnected"]);
    expect(lastCheckIn(container)).toBe("never");
  });

  it("keeps the metric cards in agreement with the row status chips at first paint", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-06T00:00:00Z"));
    const now = Date.now();
    // A non-trivial mix: justSeen -> online, stale -> stale, dead + neverSeen -> disconnected.
    const daemons = [daemonFixtures.justSeen, daemonFixtures.stale, daemonFixtures.dead, daemonFixtures.neverSeen].map(
      (daemon) => withReceipt(daemon, now)
    );
    const workspace = { ...emptyWorkspace(), workspaceId: "ws", daemons };
    const { container } = render(
      <DaemonsManagement workspace={workspace as never} onRefresh={vi.fn()} onNew={vi.fn()} onDaemon={vi.fn()} />
    );

    const chips = rowChips(container);
    const countChips = (status: string) => chips.filter((chip) => chip === status).length;

    // Chips derived from the fixtures at first paint.
    expect(countChips("online")).toBe(1);
    expect(countChips("stale")).toBe(1);
    expect(countChips("disconnected")).toBe(2);

    // Metric cards must equal the row chip counts — same source of truth, no drift.
    expect(metricValue(container, "Online")).toBe(countChips("online"));
    expect(metricValue(container, "Stale")).toBe(countChips("stale"));
    expect(metricValue(container, "Offline")).toBe(countChips("disconnected"));
  });
});

describe("DaemonDetailModal live status", () => {
  it("reflects live daemon updates on the open modal instead of a click-time snapshot", () => {
    const nowMs = Date.now();
    // Same-id states derived from the fixtures: justSeen (online) then dead (disconnected).
    const online = withReceipt({ ...daemonFixtures.justSeen, id: "d1" }, nowMs);
    const props = { api: {} as never, workspaceId: "ws", daemonId: "d1", agents: [], runs: [], agentEvents: [], onClose: vi.fn(), onChanged: vi.fn() };
    const { container, rerender } = render(<DaemonDetailModal {...props} daemons={[online]} />);
    const statusChip = () => container.querySelector(".deploy-block .chip.sm")?.textContent ?? "";

    expect(statusChip()).toContain("online");

    // A daemon.updated event lands reporting the daemon long-silent — the open modal must move.
    const silent = withReceipt({ ...daemonFixtures.dead, id: "d1" }, Date.now());
    rerender(<DaemonDetailModal {...props} daemons={[silent]} />);
    expect(statusChip()).toContain("disconnected");
  });

  it("closes when the deleted daemon stays in the array as status 'deleted' (reducer upsert path)", () => {
    const onClose = vi.fn();
    // The daemon.deleted reducer upserts the daemon with status "deleted" — it stays in the array.
    const live = withReceipt({ ...daemonFixtures.justSeen, id: "d1" }, Date.now());
    const props = { api: {} as never, workspaceId: "ws", daemonId: "d1", agents: [], runs: [], agentEvents: [], onChanged: vi.fn() };
    const { rerender } = render(<DaemonDetailModal {...props} daemons={[live]} onClose={onClose} />);
    expect(onClose).not.toHaveBeenCalled();

    const deleted: Daemon = { ...daemonFixtures.softDeleted, id: "d1" };
    rerender(<DaemonDetailModal {...props} daemons={[deleted]} onClose={onClose} />);
    expect(onClose).toHaveBeenCalled();
  });

  it("closes when the deleted daemon is removed from the array (snapshot reload path)", () => {
    const onClose = vi.fn();
    const live = withReceipt({ ...daemonFixtures.justSeen, id: "d1" }, Date.now());
    const props = { api: {} as never, workspaceId: "ws", daemonId: "d1", agents: [], runs: [], agentEvents: [], onChanged: vi.fn() };
    const { rerender } = render(<DaemonDetailModal {...props} daemons={[live]} onClose={onClose} />);
    expect(onClose).not.toHaveBeenCalled();

    rerender(<DaemonDetailModal {...props} daemons={[]} onClose={onClose} />);
    expect(onClose).toHaveBeenCalled();
  });
});

describe("AgentDetailModal live status", () => {
  // Online daemon (from the canonical fixture) so visibleAgentStatus uses the agent/run ladder
  // rather than forcing "Waiting for local environment".
  const daemon: Daemon = { ...daemonFixtures.justSeen, id: "d1" };
  const baseAgent: Agent = {
    id: "a1", daemonId: "d1", handle: "codex", name: "Codex", role: "Review", kind: "codex",
    workspaceRoot: "agents/a1", status: "idle", currentTask: "", currentActivity: "", currentRunId: "run1", updatedAt: "now",
  };
  const run = (status: string): AgentRun => ({
    id: "run1", agentId: "a1", agentHandle: "codex", agentName: "Codex", agentKind: "codex",
    workspaceRoot: "", workingDirectory: "", prompt: "", status, desiredStatus: "running", updatedAt: "now",
  });
  const props = { api: {} as never, workspaceId: "ws", agentId: "a1", daemons: [daemon], agentEvents: [], onChanged: vi.fn() };
  const modalStatus = (container: HTMLElement) => (container.querySelector(".modal-identity .col span")?.textContent ?? "").trim();

  it("reflects a live agent.updated status change on the open modal instead of a click-time snapshot", () => {
    // No active run, so the online daemon falls through to the Idle vocabulary row.
    const { container, rerender } = render(<AgentDetailModal {...props} agents={[baseAgent]} runs={[]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("Standing by");

    // An agent.updated event flips the agent's own status to working — the open modal must move.
    rerender(<AgentDetailModal {...props} agents={[{ ...baseAgent, status: "working", currentActivity: "checking tests" }]} runs={[]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("Running · checking tests");
  });

  it("reflects a live agentRuns change — a run finishing while the modal is open moves the status", () => {
    const { container, rerender } = render(<AgentDetailModal {...props} agents={[baseAgent]} runs={[run("running")]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("Running · Working");

    // The run completes in live state (no agent.updated). Status derives from runs too, so it moves to Idle.
    rerender(<AgentDetailModal {...props} agents={[baseAgent]} runs={[run("completed")]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("Standing by");
  });

  it("closes when the agent is removed from the array (the reducer's agent.deleted shape)", () => {
    const onClose = vi.fn();
    const { rerender } = render(<AgentDetailModal {...props} agents={[baseAgent]} runs={[run("running")]} onClose={onClose} />);
    expect(onClose).not.toHaveBeenCalled();

    // agent.deleted FILTERS the agent out of workspace.agents (unlike daemons' soft-delete upsert).
    rerender(<AgentDetailModal {...props} agents={[]} runs={[run("running")]} onClose={onClose} />);
    expect(onClose).toHaveBeenCalled();
  });
});
