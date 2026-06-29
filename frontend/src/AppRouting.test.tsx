// @vitest-environment jsdom

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as Y from "yjs";
import { App } from "./App";
import { workspaceLastDocumentStorageKey, workspaceSlugStorageKey } from "./routes";
import type { Account, DocumentItem, WorkspaceState, WorkspaceSummary } from "./types";

const mocks = vi.hoisted(() => ({
  workspace: null as WorkspaceState | null,
  documents: [] as DocumentItem[],
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
    upsertFile: vi.fn(),
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
  { id: "workspace_team", slug: "team", name: "Team Workspace" },
];

function workspaceState(workspaceId = "workspace_team"): WorkspaceState {
  return {
    workspaceId,
    rootDocumentId: "root_doc",
    currentUserId: "user_owner",
    name: workspaceId === "workspace_alpha" ? "Alpha Workspace" : "Team Workspace",
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

beforeEach(() => {
  localStorage.clear();
  window.history.replaceState(null, "", "/");
  mocks.workspace = workspaceState();
  mocks.documents = [{ id: "doc_1", path: "docs/spec.md", title: "spec.md" }];
  vi.spyOn(globalThis, "fetch").mockImplementation((url, init) => {
    const path = String(url);
    if (path.endsWith("/api/auth/me")) {
      return jsonResponse({ account, workspaces });
    }
    if (path.endsWith("/api/auth/login") && init?.method === "POST") {
      return jsonResponse({ token: "token", account, workspaces });
    }
    throw new Error(`Unexpected fetch ${path}`);
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("App URL routing", () => {
  it("resolves root through saved workspace and saved document state", async () => {
    localStorage.setItem("notty.auth.token", "token");
    localStorage.setItem(workspaceSlugStorageKey, "team");
    localStorage.setItem(workspaceLastDocumentStorageKey("team"), "doc_1");

    render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/w/team/d/doc_1"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_1");
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

  it("does not rewrite unauthenticated workspace URLs from saved last-document state", () => {
    localStorage.setItem(workspaceLastDocumentStorageKey("team"), "doc_1");
    window.history.replaceState(null, "", "/w/team");

    render(<App />);

    expect(screen.getByRole("heading", { name: "Welcome back" })).toBeTruthy();
    expect(window.location.pathname).toBe("/w/team");
  });

  it("renders bad workspace slugs as not found without touching saved workspace state", async () => {
    localStorage.setItem("notty.auth.token", "token");
    localStorage.setItem(workspaceSlugStorageKey, "alpha");
    window.history.replaceState(null, "", "/w/missing");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Workspace not found" })).toBeTruthy();
    expect(localStorage.getItem(workspaceSlugStorageKey)).toBe("alpha");
    expect(window.location.pathname).toBe("/w/missing");
  });

  it("renders a missing document inside the workspace shell without falling back to another document", async () => {
    localStorage.setItem("notty.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/missing");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Document not found" })).toBeTruthy();
    expect(screen.queryByTestId("document-surface")).toBeNull();
    expect(localStorage.getItem(workspaceLastDocumentStorageKey("team"))).toBeNull();
    expect(screen.getByRole("button", { name: "Home" })).toBeTruthy();
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
});
