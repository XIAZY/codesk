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
