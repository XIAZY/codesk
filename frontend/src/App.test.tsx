// @vitest-environment jsdom

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceOnboarding } from "./App";
import { identifierHelpText, identifierPattern } from "./logic";
import type { Account, WorkspaceSummary } from "./types";

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

    await user.type(screen.getByLabelText("Invite link"), "https://notty.example/invite/nottyinvite_abc123");
    await user.click(screen.getByRole("button", { name: "Join with invite link" }));

    expect(window.location.pathname).toBe("/invite/nottyinvite_abc123");
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
