// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./api";

describe("ApiClient document create", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("allocates an empty content document without namespace path or content payload", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ id: "doc_1", updatedAt: "now" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      })
    );

    const document = await new ApiClient("token").createDocument("workspace/1");

    expect(document.id).toBe("doc_1");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/workspaces/workspace%2F1/documents");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBe("{}");
  });
});

describe("ApiClient workspace create", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("surfaces backend JSON error messages", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ error: "Workspace slug is already taken." }), {
        status: 409,
        headers: { "Content-Type": "application/json" },
      })
    );

    await expect(
      new ApiClient("token").createWorkspace({
        name: "Product Workspace",
        slug: "product-workspace",
        handle: "owner",
      })
    ).rejects.toThrow("Workspace slug is already taken.");
  });
});

describe("ApiClient workspace invites", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("uses the workspace API to create invite links", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ invite: { id: "invite_1", workspaceId: "workspace/1", expiresAt: "2026-07-06T00:00:00Z", createdAt: "2026-06-29T00:00:00Z" }, url: "/invite/nottyinvite_abc" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      })
    );

    const response = await new ApiClient("token").createWorkspaceInvite("workspace/1");

    expect(response.url).toBe("/invite/nottyinvite_abc");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/workspaces/workspace%2F1/invites");
    expect(init?.method).toBe("POST");
  });

  it("previews and accepts invite tokens through token-scoped routes", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ workspace: { name: "Team Workspace", slug: "team" }, expiresAt: "2026-07-06T00:00:00Z" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        })
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ workspace: { id: "workspace_team", slug: "team", name: "Team Workspace" } }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        })
      );

    await new ApiClient("").previewWorkspaceInvite("nottyinvite/a");
    await new ApiClient("token").acceptWorkspaceInvite("nottyinvite/a", { handle: "member" });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(String(fetchMock.mock.calls[0][0])).toContain("/api/invites/nottyinvite%2Fa");
    expect(fetchMock.mock.calls[0][1]?.method).toBeUndefined();
    expect(String(fetchMock.mock.calls[1][0])).toContain("/api/invites/nottyinvite%2Fa/accept");
    expect(fetchMock.mock.calls[1][1]?.method).toBe("POST");
    expect(fetchMock.mock.calls[1][1]?.body).toBe(JSON.stringify({ handle: "member" }));
  });
});

describe("ApiClient last-accessed route state", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("updates workspace document preference through the workspace API", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ status: "ok" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })
    );

    await new ApiClient("token").updateLastAccessed("workspace/1", { documentId: "doc_1" });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/workspaces/workspace%2F1/last-accessed");
    expect(init?.method).toBe("PATCH");
    expect(init?.body).toBe(JSON.stringify({ documentId: "doc_1" }));
  });
});
