import { describe, expect, it } from "vitest";
import { parseRoute, resolveRoot, resolveWorkspace, routePath, type AppRoute } from "./routes";

const routeCases: AppRoute[] = [
  { kind: "root" },
  { kind: "login" },
  { kind: "register" },
  { kind: "invite", token: "nottyinvite_token" },
  { kind: "newWorkspace" },
  { kind: "workspace", slug: "team", view: { kind: "home" } },
  { kind: "workspace", slug: "team", view: { kind: "document", documentId: "doc_1" } },
  { kind: "workspace", slug: "team", view: { kind: "daemons" } },
  { kind: "workspace", slug: "team", view: { kind: "agents" } },
];

describe("routes", () => {
  it("round-trips every supported application route", () => {
    for (const route of routeCases) {
      expect(parseRoute(routePath(route))).toEqual(route);
    }
  });

  it("parses unknown or malformed paths as notFound", () => {
    expect(parseRoute("/anything")).toEqual({ kind: "notFound" });
    expect(parseRoute("/w")).toEqual({ kind: "notFound" });
    expect(parseRoute("/w/team/settings")).toEqual({ kind: "notFound" });
    expect(parseRoute("/w/team/d")).toEqual({ kind: "notFound" });
    expect(parseRoute("/w/%E0%A4%A")).toEqual({ kind: "notFound" });
  });

  it("resolves root from auth and workspace membership state", () => {
    const workspaces = [
      { id: "workspace_alpha", slug: "alpha" },
      { id: "workspace_beta", slug: "beta" },
    ];

    expect(resolveRoot({ authenticated: false, account: { lastAccessedWorkspaceId: "workspace_beta" }, workspaces })).toEqual({ kind: "login" });
    expect(resolveRoot({ authenticated: true, account: { lastAccessedWorkspaceId: "workspace_beta" }, workspaces: [] })).toEqual({ kind: "newWorkspace" });
    expect(resolveRoot({ authenticated: true, account: { lastAccessedWorkspaceId: "workspace_beta" }, workspaces })).toEqual({
      kind: "workspace",
      slug: "beta",
      view: { kind: "home" },
    });
    expect(resolveRoot({ authenticated: true, account: { lastAccessedWorkspaceId: "workspace_missing" }, workspaces })).toEqual({
      kind: "workspace",
      slug: "alpha",
      view: { kind: "home" },
    });
  });

  it("resolves a workspace home route from membership document state", () => {
    const documents = [{ id: "doc_1" }, { id: "doc_2" }];
    expect(resolveWorkspace({ slug: "team", lastAccessedDocumentId: "doc_2" }, documents)).toEqual({
      kind: "workspace",
      slug: "team",
      view: { kind: "document", documentId: "doc_2" },
    });
    expect(resolveWorkspace({ slug: "team", lastAccessedDocumentId: "doc_missing" }, documents)).toEqual({
      kind: "workspace",
      slug: "team",
      view: { kind: "document", documentId: "doc_1" },
    });
    expect(resolveWorkspace({ slug: "team" }, [])).toEqual({ kind: "workspace", slug: "team", view: { kind: "home" } });
  });
});
