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
  rootReady: true,
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
    documents: mocks.rootReady ? mocks.documents : [],
    ready: mocks.rootReady,
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
    workspace_beta: "Beta Workspace",
    workspace_created: "Product Workspace",
    workspace_invited: "Invited Workspace",
    workspace_team: "Team Workspace",
  };
  return {
    workspaceId,
    rootDocumentId: "root_doc",
    currentUserId: "user_owner",
    currentMembershipRole: "owner",
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

function jsonErrorResponse(status: number, error: string) {
  return Promise.resolve(new Response(JSON.stringify({ error }), { status, headers: { "Content-Type": "application/json" } }));
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

function lastAccessedPatchBodies() {
  return vi.mocked(globalThis.fetch).mock.calls
    .filter(([url, init]) => String(url).endsWith("/last-accessed") && init?.method === "PATCH")
    .map(([, init]) => String(init?.body ?? ""));
}

beforeEach(() => {
  localStorage.clear();
  window.history.replaceState(null, "", "/");
  mocks.workspace = workspaceState();
  mocks.documents = [{ id: "doc_1", path: "docs/spec.md", title: "spec.md" }];
  mocks.rootReady = true;
  mocks.authAccount = { ...account, lastAccessedWorkspaceId: "workspace_team" };
  mocks.authWorkspaces = workspaces;
  vi.spyOn(globalThis, "fetch").mockImplementation((url, init) => {
    const path = String(url);
    if (path.endsWith("/api/auth/me")) {
      return jsonResponse({ account: mocks.authAccount, workspaces: mocks.authWorkspaces });
    }
    if (path.endsWith("/api/auth/register") && init?.method === "POST") {
      const body = typeof init.body === "string" ? JSON.parse(init.body) as { email: string; displayName: string } : { email: "", displayName: "" };
      return jsonResponse({ account: { id: "account_new", email: body.email, displayName: body.displayName, emailVerified: false } });
    }
    if (path.endsWith("/api/auth/login") && init?.method === "POST") {
      const body = typeof init.body === "string" ? JSON.parse(init.body) as { email: string } : { email: "" };
      if (body.email === "unverified@example.com") {
        return jsonErrorResponse(403, "email_not_verified");
      }
      return jsonResponse({ token: "token", account: mocks.authAccount, workspaces: mocks.authWorkspaces });
    }
    if (path.endsWith("/api/auth/verify-email") && init?.method === "POST") {
      const body = typeof init.body === "string" ? JSON.parse(init.body) as { token?: string } : {};
      if (body.token === "already-used") {
        return jsonErrorResponse(400, "email_already_verified");
      }
      return jsonResponse({ account: { ...account, emailVerified: true } });
    }
    if (path.endsWith("/api/auth/resend-verification") && init?.method === "POST") {
      return jsonResponse({ status: "ok" });
    }
    if (path.endsWith("/api/auth/forgot-password") && init?.method === "POST") {
      return jsonResponse({ status: "ok" });
    }
    if (path.endsWith("/api/auth/reset-password") && init?.method === "POST") {
      return jsonResponse({ status: "ok" });
    }
    if (path.endsWith("/api/invites/new123") && !init?.method) {
      return jsonResponse({ workspace: { name: "Invited Workspace", slug: "invited" }, expiresAt: "2026-07-06T00:00:00Z" });
    }
    if (path.endsWith("/api/invites/new123/accept") && init?.method === "POST") {
      const workspace = { id: "workspace_invited", slug: "invited", name: "Invited Workspace" };
      mocks.authAccount = mocks.authAccount ? { ...mocks.authAccount, lastAccessedWorkspaceId: workspace.id } : mocks.authAccount;
      mocks.authWorkspaces = [workspace];
      mocks.workspace = workspaceState(workspace.id);
      return jsonResponse({ workspace });
    }
    if (path.endsWith("/api/invites/team456") && !init?.method) {
      return jsonResponse({ workspace: { name: "Team Workspace", slug: "team" }, expiresAt: "2026-07-06T00:00:00Z" });
    }
    if (path.endsWith("/api/invites/expired789") && !init?.method) {
      return jsonErrorResponse(410, "This invite link has expired. Ask the workspace admin for a new one.");
    }
    if (path.endsWith("/api/workspaces/workspace_team/invites") && init?.method === "POST") {
      return jsonResponse({
        invite: { id: "invite_1", workspaceId: "workspace_team", expiresAt: "2026-07-06T00:00:00Z", createdAt: "2026-06-29T00:00:00Z" },
        url: "/invite/created321",
      });
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
  it("ignores and clears legacy Notty auth tokens", async () => {
    localStorage.setItem("notty.auth.token", "legacy-token");

    render(<App />);

    expect(screen.getByRole("heading", { name: "Welcome back" })).toBeTruthy();
    await waitFor(() => expect(localStorage.getItem("notty.auth.token")).toBeNull());
    expect(localStorage.getItem("codesk.auth.token")).toBeNull();
  });

  it("resolves root through backend last-accessed workspace and document state", async () => {
    localStorage.setItem("codesk.auth.token", "token");

    render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/w/team/d/doc_1"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_1");
  });

  it("does not resolve workspace home through stale root docs from another workspace", async () => {
    localStorage.setItem("codesk.auth.token", "token");
    window.history.replaceState(null, "", "/w/beta");
    mocks.authAccount = { ...account, lastAccessedWorkspaceId: "workspace_beta" };
    mocks.authWorkspaces = [{ id: "workspace_beta", slug: "beta", name: "Beta Workspace", lastAccessedDocumentId: "doc_beta" }];
    mocks.workspace = workspaceState("workspace_beta");
    mocks.documents = [{ id: "doc_alpha", path: "alpha.md", title: "alpha.md" }];
    mocks.rootReady = false;

    const { rerender } = render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/w/beta"));
    expect(screen.queryByTestId("document-surface")).toBeNull();

    mocks.documents = [{ id: "doc_beta", path: "beta.md", title: "beta.md" }];
    mocks.rootReady = true;
    rerender(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/w/beta/d/doc_beta"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_beta");
  });

  it("ignores legacy route storage when restoring a different account", async () => {
    localStorage.setItem("codesk.auth.token", "token");
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

  it("clears stale workspace URL intent on signout before the next account logs in", async () => {
    const user = userEvent.setup();
    localStorage.setItem("codesk.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/doc_1");

    render(<App />);

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(window.location.pathname).toBe("/login");

    mocks.authAccount = { ...account, id: "account_2", lastAccessedWorkspaceId: "workspace_alpha" };
    mocks.authWorkspaces = [{ id: "workspace_alpha", slug: "alpha", name: "Alpha Workspace", lastAccessedDocumentId: "doc_alpha" }];
    mocks.workspace = workspaceState("workspace_alpha");
    mocks.documents = [{ id: "doc_alpha", path: "alpha.md", title: "alpha.md" }];

    await user.type(screen.getByLabelText("Email"), "other@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    await waitFor(() => expect(window.location.pathname).toBe("/w/alpha/d/doc_alpha"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_alpha");
  });

  it("redirects a protected deep link to login and then routes from backend account state", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/w/team/d/doc_1");
    mocks.authAccount = { ...account, lastAccessedWorkspaceId: "workspace_alpha" };
    mocks.authWorkspaces = [{ id: "workspace_alpha", slug: "alpha", name: "Alpha Workspace", lastAccessedDocumentId: "doc_alpha" }];
    mocks.workspace = workspaceState("workspace_alpha");
    mocks.documents = [{ id: "doc_alpha", path: "alpha.md", title: "alpha.md" }];

    render(<App />);

    expect(screen.getByRole("heading", { name: "Welcome back" })).toBeTruthy();
    await waitFor(() => expect(window.location.pathname).toBe("/login"));
    expect(window.location.search).toBe("");

    await user.type(screen.getByLabelText("Email"), "owner@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    await waitFor(() => expect(window.location.pathname).toBe("/w/alpha/d/doc_alpha"));
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_alpha");
  });

  it("redirects unauthenticated workspace URLs to login", async () => {
    window.history.replaceState(null, "", "/w/team");

    render(<App />);

    expect(screen.getByRole("heading", { name: "Welcome back" })).toBeTruthy();
    await waitFor(() => expect(window.location.pathname).toBe("/login"));
    expect(window.location.search).toBe("");
  });

  it("registers without storing a token and shows the verification resend state", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/register");

    render(<App />);

    await user.type(screen.getByLabelText("Display name"), "New User");
    await user.type(screen.getByLabelText("Email"), "new@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Create account" }));

    expect(await screen.findByRole("heading", { name: "Check your email" })).toBeTruthy();
    expect(localStorage.getItem("codesk.auth.token")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Resend verification email" }));
    expect(await screen.findByText("Verification email sent.")).toBeTruthy();
  });

  it("shows the verification state instead of storing a token for unverified login", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/login");

    render(<App />);

    await user.type(screen.getByLabelText("Email"), "unverified@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    expect(await screen.findByRole("heading", { name: "Check your email" })).toBeTruthy();
    expect(localStorage.getItem("codesk.auth.token")).toBeNull();
  });

  it("verifies an email link and returns to login", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/account/verify-email?token=verify123");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Email verified" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Log in" }));
    expect(window.location.pathname).toBe("/login");
  });

  it("shows success for an already-used verification link", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/account/verify-email?token=already-used");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Email verified" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Verification failed" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Log in" }));
    expect(window.location.pathname).toBe("/login");
  });

  it("resets a password from a tokenized link", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/account/reset-password?token=reset123");

    render(<App />);

    await user.type(screen.getByLabelText("New password"), "newpassword");
    await user.click(screen.getByRole("button", { name: "Reset password" }));

    expect(await screen.findByRole("heading", { name: "Password reset" })).toBeTruthy();
  });

  it("preserves an unauthenticated invite URL through login and joins the workspace", async () => {
    const user = userEvent.setup();
    mocks.authAccount = { ...account, lastAccessedWorkspaceId: "" };
    mocks.authWorkspaces = [];
    mocks.documents = [];
    mocks.workspace = workspaceState("workspace_invited");
    window.history.replaceState(null, "", "/invite/new123");

    render(<App />);

    expect(await screen.findByText("Invited Workspace")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Join workspace" })).toBeTruthy();
    expect(window.location.pathname).toBe("/invite/new123");

    await user.type(screen.getByLabelText("Email"), "owner@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    await waitFor(() => expect(screen.getByLabelText("Your handle in this workspace")).toBeTruthy());
    expect(window.location.pathname).toBe("/invite/new123");
    await user.click(screen.getByRole("button", { name: "Join workspace" }));

    await waitFor(() => expect(window.location.pathname).toBe("/w/invited"));
  });

  it("renders expired invite links without redirecting to login", async () => {
    window.history.replaceState(null, "", "/invite/expired789");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Invite unavailable" })).toBeTruthy();
    expect(screen.getByText(/expired/)).toBeTruthy();
    expect(window.location.pathname).toBe("/invite/expired789");
  });

  it("lets existing members open the workspace from an invite link", async () => {
    const user = userEvent.setup();
    localStorage.setItem("codesk.auth.token", "token");
    window.history.replaceState(null, "", "/invite/team456");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "You are already in this workspace" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Open workspace" }));

    await waitFor(() => expect(window.location.pathname.startsWith("/w/team")).toBe(true));
  });

  it("shows share-link controls only to workspace owners and admins", async () => {
    const user = userEvent.setup();
    localStorage.setItem("codesk.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/doc_1");

    render(<App />);

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    await user.click(screen.getByRole("button", { name: "Invite" }));

    expect(await screen.findByDisplayValue(`${window.location.origin}/invite/created321`)).toBeTruthy();

    cleanup();
    mocks.workspace = { ...workspaceState(), currentMembershipRole: "member" };
    window.history.replaceState(null, "", "/w/team/d/doc_1");
    render(<App />);

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    expect(screen.queryByRole("button", { name: "Invite" })).toBeNull();
  });

  it("routes from backend account state after login from an invalid protected workspace URL", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "/w/what-the-fuck-workspace");

    render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe("/login"));
    expect(window.location.search).toBe("");

    await user.type(screen.getByLabelText("Email"), "owner@example.com");
    await user.type(screen.getByLabelText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    await waitFor(() => expect(window.location.pathname).toBe("/w/team/d/doc_1"));
    expect(screen.queryByRole("heading", { name: "Workspace not found" })).toBeNull();
    expect(screen.getByTestId("document-surface").textContent).toBe("doc_1");
  });

  it("renders bad workspace slugs as not found and clears legacy route storage", async () => {
    localStorage.setItem("codesk.auth.token", "token");
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
    localStorage.setItem("codesk.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/missing");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Document not found" })).toBeTruthy();
    expect(screen.queryByTestId("document-surface")).toBeNull();
    expect(screen.getByRole("button", { name: "Back to workspace" })).toBeTruthy();
  });

  it("updates URL from workspace navigation and responds to browser history", async () => {
    const user = userEvent.setup();
    localStorage.setItem("codesk.auth.token", "token");
    window.history.replaceState(null, "", "/w/team/d/doc_1");

    render(<App />);

    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    await user.click(screen.getByRole("button", { name: "Agents" }));
    expect(window.location.pathname).toBe("/w/team/agents");

    window.history.back();
    await waitFor(() => expect(window.location.pathname).toBe("/w/team/d/doc_1"));
    await waitFor(() => expect(screen.getByTestId("document-surface").textContent).toBe("doc_1"));
    expect(lastAccessedPatchBodies().filter((body) => body === JSON.stringify({}))).toHaveLength(1);
  });

  it("opens a document URL after creating the workspace and document", async () => {
    const user = userEvent.setup();
    localStorage.setItem("codesk.auth.token", "token");
    mocks.authWorkspaces = [];
    mocks.authAccount = { ...account, lastAccessedWorkspaceId: "" };
    mocks.documents = [];
    mocks.workspace = workspaceState("workspace_created");
    window.history.replaceState(null, "", "/new");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Create a workspace" })).toBeTruthy();
    await user.type(screen.getByLabelText("Workspace name"), "Product Workspace");
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
