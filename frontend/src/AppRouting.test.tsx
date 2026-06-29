// @vitest-environment jsdom

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as Y from "yjs";
import { App } from "./App";
import type { Account, DocumentItem, WorkspaceState, WorkspaceSummary } from "./types";

const mocks = vi.hoisted(() => ({
  workspace: null as WorkspaceState | null,
  documents: [] as DocumentItem[],
  authAccount: null as Account | null,
  authWorkspaces: [] as WorkspaceSummary[],
}));

vi.mock("./DocumentSurface", () => ({
  DocumentSurface: ({ documentId }: { documentId: string }) => <div data-testid="document-surface">{documentId}</div>,
}));

vi.mock("./useDocument", () => ({
  useDocumentSync: () => {
    const doc = new Y.Doc();
    return {
      ydoc: doc,
      ytext: doc.getText("content"),
      ready: true,
      connected: true,
    };
  },
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
    ready: true,
    connected: true,
    upsertFile: vi.fn((id: string, path: string) => {
      const title = path.split("/").filter(Boolean).pop() || path;
      const existing = mocks.documents.find((document) => document.id === id);
      if (existing) {
        existing.path = path;
        existing.title = title;
        return;
      }
      mocks.documents = [...mocks.documents, { id, path, title }];
    }),
    moveFile: vi.fn(),
    tombstoneFile: vi.fn(),
  }),
}));

const account: Account = {
  id: "account_1",
  email: "owner@example.com",
  displayName: "Owner",
};

const workspaces: WorkspaceSummary[] = [
  { id: "workspace_alpha", slug: "alpha", name: "Alpha Workspace" },
  { id: "workspace_team", slug: "team", name: "Team Workspace", lastAccessedDocumentId: "doc_1" },
];

function workspaceState(workspaceId = "workspace_team"): WorkspaceState {
  const workspaceNames: Record<string, string> = {
    workspace_alpha: "Alpha Workspace",
    workspace_created: "Product Workspace",
    workspace_team: "Team Workspace",
  };
  return {
    workspaceId,
    rootDocumentId: "root_doc",
    currentUserId: "user_owner",
    name: workspaceNames[workspaceId] ?? "Team Workspace",
    users: [
      {
        id: "user_owner",
        handle: "owner",
        name: "Owner",
        role: "Workspace owner",
        kind: "human",
        status: "active",
        updatedAt: "2026-06-29T00:00:00Z",
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

function jsonResponse(payload: unknown) {
  return Promise.resolve(new Response(JSON.stringify(payload), { status: 200, headers: { "Content-Type": "application/json" } }));
}

function patchLastAccessed(path: string, init: RequestInit | undefined) {
  if (!path.includes("/api/workspaces/") || !path.endsWith("/last-accessed") || init?.method !== "PATCH") {
    return false;
  }
  const [, workspaceID = ""] = path.match(/\/api\/workspaces\/([^/]+)\/last-accessed$/) ?? [];
  const body = typeof init.body === "string" && init.body ? JSON.parse(init.body) as { documentId?: string } : {};
  if (mocks.authAccount) {
    mocks.authAccount = { ...mocks.authAccount, lastAccessedWorkspaceId: decodeURIComponent(workspaceID) };
  }
  if (body.documentId) {
    mocks.authWorkspaces = mocks.authWorkspaces.map((workspace) => (
      workspace.id === decodeURIComponent(workspaceID) ? { ...workspace, lastAccessedDocumentId: body.documentId } : workspace
    ));
  }
  return true;
}

beforeEach(() => {
  localStorage.clear();
  window.history.replaceState(null, "", "/");
  mocks.workspace = workspaceState();
  mocks.documents = [{ id: "doc_1", path: "docs/spec.md", title: "spec.md" }];
  mocks.authAccount = { ...account, lastAccessedWorkspaceId: "workspace_team" };
  mocks.authWorkspaces = workspaces;
  vi.spyOn(globalThis, "fetch").mockImplementation((url, init) => {
    const path = String(url);
    if (path.endsWith("/api/auth/me")) {
      return jsonResponse({ account: mocks.authAccount, workspaces: mocks.authWorkspaces });
    }
    if (path.endsWith("/api/auth/login") && init?.method === "POST") {
      return jsonResponse({ token: "token", account: mocks.authAccount, workspaces: mocks.authWorkspaces });
    }
    if (path.endsWith("/api/workspaces") && init?.method === "POST") {
      const workspace = { id: "workspace_created", slug: "product-workspace", name: "Product Workspace" };
      mocks.authWorkspaces = [workspace];
      mocks.workspace = workspaceState(workspace.id);
      return jsonResponse({ workspace });
    }
    if (path.endsWith("/api/workspaces/workspace_created/documents") && init?.method === "POST") {
      return jsonResponse({ id: "doc_created", updatedAt: "2026-06-29T00:00:00Z" });
    }
    if (patchLastAccessed(path, init)) {
      return jsonResponse({ status: "ok" });
    }
    throw new Error(`Unexpected fetch ${path}`);
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("App URL routing", () => {
  it("resolves root through backend last-accessed workspace and document state", async () => {
    localStorage.setItem("notty.auth.token", "token");

    render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/w/team/d/doc_1"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_1");
  });

  it("ignores legacy route storage when restoring a different account", async () => {
    localStorage.setItem("notty.auth.token", "token");
    localStorage.setItem("notty.workspace.slug", "team");
    localStorage.setItem("notty.workspace.team.lastDoc", "doc_old");
    mocks.authAccount = { ...account, id: "account_2", lastAccessedWorkspaceId: "workspace_alpha" };
    mocks.authWorkspaces = [{ id: "workspace_alpha", slug: "alpha", name: "Alpha Workspace", lastAccessedDocumentId: "doc_alpha" }];
    mocks.workspace = workspaceState("workspace_alpha");
    mocks.documents = [{ id: "doc_alpha", path: "alpha.md", title: "alpha.md" }];

    render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/w/alpha/d/doc_alpha"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_alpha");
    expect(localStorage.getItem("notty.workspace.slug")).toBeNull();
    expect(localStorage.getItem("notty.workspace.team.lastDoc")).toBeNull();
  });

  it("keeps a protected deep link URL through login and then renders the workspace", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/w/team/d/doc_1");

    render(<App />);

    expect(screen.getByRole("heading", { name: "Welcome back" })).toBeTruthy();
    expect(window.location.pathname).toBe("/w/team/d/doc_1");

    await user.type(screen.getByLabelText("Email"), "owner@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    expect(window.location.pathname).toBe("/w/team/d/doc_1");
  });

  it("does not rewrite unauthenticated workspace URLs from route preference state", () => {
    window.history.replaceState(null, "", "/w/team");

    render(<App />);

    expect(screen.getByRole("heading", { name: "Welcome back" })).toBeTruthy();
    expect(window.location.pathname).toBe("/w/team");
  });

  it("renders bad workspace slugs as not found and clears legacy route storage", async () => {
    localStorage.setItem("notty.auth.token", "token");
    localStorage.setItem("notty.workspace.slug", "alpha");
    localStorage.setItem("notty.workspace.alpha.lastDoc", "old_doc");
    window.history.replaceState(null, "", "/w/missing");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Workspace not found" })).toBeTruthy();
    expect(localStorage.getItem("notty.workspace.slug")).toBeNull();
    expect(localStorage.getItem("notty.workspace.alpha.lastDoc")).toBeNull();
    expect(window.location.pathname).toBe("/w/missing");
  });

  it("renders a missing document inside the workspace shell without falling back to another document", async () => {
    localStorage.setItem("notty.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/missing");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Document not found" })).toBeTruthy();
    expect(screen.queryByTestId("document-surface")).toBeNull();
    expect(screen.getByRole("button", { name: "Back to workspace" })).toBeTruthy();
  });

  it("updates URL from workspace navigation and responds to browser history", async () => {
    const user = userEvent.setup();
    localStorage.setItem("notty.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/doc_1");

    render(<App />);

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    await user.click(screen.getByRole("button", { name: "Daemons" }));
    expect(window.location.pathname).toBe("/w/team/daemons");
    await user.click(screen.getByRole("button", { name: "Agents" }));
    expect(window.location.pathname).toBe("/w/team/agents");

    window.history.back();
    await waitFor(() => expect(window.location.pathname).toBe("/w/team/daemons"));
    expect(screen.getAllByText("Daemons").length).toBeGreaterThan(0);
  });

  it("opens a document URL after creating the workspace and document", async () => {
    const user = userEvent.setup();
    localStorage.setItem("notty.auth.token", "token");
    mocks.authWorkspaces = [];
    mocks.authAccount = { ...account, lastAccessedWorkspaceId: "" };
    mocks.documents = [];
    mocks.workspace = workspaceState("workspace_created");
    window.history.replaceState(null, "", "/new");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Create a workspace" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Create and enter" }));
    await waitFor(() => expect(window.location.pathname).toBe("/w/product-workspace"));

    await user.click(screen.getByRole("button", { name: /Create your first doc/ }));
    await waitFor(() => expect(window.location.pathname).toBe("/w/product-workspace/d/doc_created"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_created");

    cleanup();
    window.history.replaceState(null, "", "/w/product-workspace/d/doc_created");
    render(<App />);

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_created"));
    expect(window.location.pathname).toBe("/w/product-workspace/d/doc_created");
  });
});
