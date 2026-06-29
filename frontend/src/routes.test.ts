import { describe, expect, it } from "vitest";
import { parseRoute, resolveRoot, resolveWorkspace, routePath, type AppRoute } from "./routes";

const routeCases: AppRoute[] = [
  { kind: "root" },
  { kind: "login" },
  { kind: "register" },
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
      { slug: "alpha" },
      { slug: "beta" },
    ];

    expect(resolveRoot({ authenticated: false, workspaces }, "beta")).toEqual({ kind: "login" });
    expect(resolveRoot({ authenticated: true, workspaces: [] }, "beta")).toEqual({ kind: "newWorkspace" });
    expect(resolveRoot({ authenticated: true, workspaces }, "beta")).toEqual({
      kind: "workspace",
      slug: "beta",
      view: { kind: "home" },
    });
    expect(resolveRoot({ authenticated: true, workspaces }, "missing")).toEqual({
      kind: "workspace",
      slug: "alpha",
      view: { kind: "home" },
    });
  });

  it("resolves a workspace home route from saved document state only", () => {
    expect(resolveWorkspace("team", "doc_1")).toEqual({
      kind: "workspace",
      slug: "team",
      view: { kind: "document", documentId: "doc_1" },
    });
    expect(resolveWorkspace("team", "")).toEqual({ kind: "workspace", slug: "team", view: { kind: "home" } });
    expect(resolveWorkspace("team", null)).toEqual({ kind: "workspace", slug: "team", view: { kind: "home" } });
  });
});
