// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AgentDetailModal, CreateDaemonModal, DaemonDetailModal, DaemonsManagement, WorkspaceApp, WorkspaceOnboarding } from "./App";
import { emptyWorkspace, identifierFromName, identifierHelpText, identifierPattern, workspaceSlugMaxLength } from "./logic";
import { daemonFixtures, withReceipt } from "./daemonFixtures";
import type { Account, Agent, AgentRun, Daemon, WorkspaceSummary } from "./types";

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

describe("WorkspaceApp participants rail", () => {
  it("renames to Participants and narrows to the current document's participants", async () => {
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

    const tab = screen.getByRole("button", { name: /participants/i });
    expect(within(tab).getByText("1")).toBeTruthy(); // only You, with no active document
    expect(screen.queryByRole("button", { name: /people/i })).toBeNull();

    fireEvent.click(tab);

    await waitFor(() => expect(document.querySelector(".ctx-body.people-pane")).toBeTruthy());
    const panel = document.querySelector(".ctx-body.people-pane") as HTMLElement;
    expect(within(panel).getByText("Participants")).toBeTruthy();
    expect(within(panel).getByText("@ada")).toBeTruthy(); // the current user
    expect(within(panel).getByText("You")).toBeTruthy();
    // workspace members/agents who don't participate in the current document do not leak in
    expect(within(panel).queryByText("@grace")).toBeNull();
    expect(within(panel).queryByText("@codex")).toBeNull();
    expect(within(panel).queryByText("Agents")).toBeNull();
    expect(within(panel).getByText("No other participants yet.")).toBeTruthy();
    // membership management no longer lives in this tab
    expect(within(panel).queryByRole("button", { name: "Share" })).toBeNull();
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

describe("WorkspaceApp document activity single source", () => {
  it("carries no count on the sidebar Activity nav, so nav and panel cannot disagree", () => {
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

    // The sidebar Activity nav is a pure jump affordance — no count element means
    // no second source that could drift from the right-rail Document Activity panel.
    const navButton = screen.getByText("Activity").closest("button");
    expect(navButton).toBeTruthy();
    expect(navButton?.querySelector(".ct")).toBeNull();
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

    await user.click(screen.getByRole("button", { name: "Create daemon" }));

    // Daemon created but has not checked in yet: the chip must say waiting.
    expect(await screen.findByText("Waiting for daemon to check in…")).toBeTruthy();
    expect(screen.queryByText("Daemon connected")).toBeNull();

    // A daemon.updated event lands via the workspace socket, so live state now reports
    // the daemon online. The chip must flip without a manual refresh.
    rerender(
      <CreateDaemonModal api={api as never} workspaceId="ws" daemons={[{ ...daemonFixtures.justSeen, id: "daemon_new", name: "Local daemon" }]} onClose={vi.fn()} onDone={vi.fn()} />
    );

    expect(screen.getByText("Daemon connected")).toBeTruthy();
    expect(screen.queryByText("Waiting for daemon to check in…")).toBeNull();
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
    const props = { api: {} as never, workspaceId: "ws", daemonId: "d1", agents: [], runs: [], onClose: vi.fn(), onChanged: vi.fn() };
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
    const props = { api: {} as never, workspaceId: "ws", daemonId: "d1", agents: [], runs: [], onChanged: vi.fn() };
    const { rerender } = render(<DaemonDetailModal {...props} daemons={[live]} onClose={onClose} />);
    expect(onClose).not.toHaveBeenCalled();

    const deleted: Daemon = { ...daemonFixtures.softDeleted, id: "d1" };
    rerender(<DaemonDetailModal {...props} daemons={[deleted]} onClose={onClose} />);
    expect(onClose).toHaveBeenCalled();
  });

  it("closes when the deleted daemon is removed from the array (snapshot reload path)", () => {
    const onClose = vi.fn();
    const live = withReceipt({ ...daemonFixtures.justSeen, id: "d1" }, Date.now());
    const props = { api: {} as never, workspaceId: "ws", daemonId: "d1", agents: [], runs: [], onChanged: vi.fn() };
    const { rerender } = render(<DaemonDetailModal {...props} daemons={[live]} onClose={onClose} />);
    expect(onClose).not.toHaveBeenCalled();

    rerender(<DaemonDetailModal {...props} daemons={[]} onClose={onClose} />);
    expect(onClose).toHaveBeenCalled();
  });
});

describe("AgentDetailModal live status", () => {
  // Online daemon (from the canonical fixture) so visibleAgentStatus falls through to agentStatus
  // rather than forcing "disconnected".
  const daemon: Daemon = { ...daemonFixtures.justSeen, id: "d1" };
  const baseAgent: Agent = {
    id: "a1", daemonId: "d1", handle: "codex", name: "Codex", role: "Review", kind: "codex",
    workspaceRoot: "agents/a1", status: "idle", currentTask: "", currentActivity: "", currentRunId: "run1", updatedAt: "now",
  };
  const run = (status: string): AgentRun => ({
    id: "run1", agentId: "a1", agentHandle: "codex", agentName: "Codex", agentKind: "codex",
    workspaceRoot: "", workingDirectory: "", prompt: "", status, desiredStatus: "running", updatedAt: "now",
  });
  const props = { api: {} as never, workspaceId: "ws", agentId: "a1", daemons: [daemon], onChanged: vi.fn() };
  const modalStatus = (container: HTMLElement) => (container.querySelector(".modal-identity .col span")?.textContent ?? "").trim();

  it("reflects a live agent.updated status change on the open modal instead of a click-time snapshot", () => {
    // No active run, so the agent's own status field drives visibleAgentStatus.
    const { container, rerender } = render(<AgentDetailModal {...props} agents={[baseAgent]} runs={[]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("idle");

    // An agent.updated event flips the agent's own status — the open modal must move.
    rerender(<AgentDetailModal {...props} agents={[{ ...baseAgent, status: "disconnected" }]} runs={[]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("disconnected");
  });

  it("reflects a live agentRuns change — a run finishing while the modal is open moves the status", () => {
    const { container, rerender } = render(<AgentDetailModal {...props} agents={[baseAgent]} runs={[run("running")]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("working");

    // The run completes in live state (no agent.updated). Status derives from runs too, so it moves to idle.
    rerender(<AgentDetailModal {...props} agents={[baseAgent]} runs={[run("completed")]} onClose={vi.fn()} />);
    expect(modalStatus(container)).toContain("idle");
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
