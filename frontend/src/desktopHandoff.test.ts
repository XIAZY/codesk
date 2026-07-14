// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createDesktopHandoffForm,
  parseDesktopCallback,
  parseDesktopHandoffPageURL,
  submitDesktopHandoff,
  type DesktopHandoffPayload,
} from "./desktopHandoff";

const nonce = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8";
const callback = `http://127.0.0.1:49152/desktop/connect/${nonce}`;
const token = "nottyd_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8";
const payload: DesktopHandoffPayload = {
  daemonId: "daemon-123",
  token,
  workspaceId: "workspace-123",
  workspaceName: "Desktop QA",
  workspaceSlug: "desktop-qa",
  workspaceUrl: "https://app.example.test/w/desktop-qa",
};

const invalidPayloads: Array<[string, Partial<DesktopHandoffPayload>]> = [
  ["empty daemon id", { daemonId: "" }],
  ["empty token", { token: "" }],
  ["token control character", { token: "opaque\ntoken" }],
  ["unpaired surrogate", { token: "opaque\ud800token" }],
  ["control character", { workspaceName: "Desktop\nQA" }],
  ["C1 control character", { workspaceName: "Desktop\u0085QA" }],
  ["credentialed workspace URL", { workspaceUrl: "https://user:pass@app.example.test/w/desktop-qa" }],
  ["workspace URL query", { workspaceUrl: "https://app.example.test/w/desktop-qa?token=bad" }],
  ["workspace URL empty query", { workspaceUrl: "https://app.example.test/w/desktop-qa?" }],
  ["workspace URL empty fragment", { workspaceUrl: "https://app.example.test/w/desktop-qa#" }],
];

afterEach(() => {
  document.body.replaceChildren();
  vi.restoreAllMocks();
});

describe("parseDesktopCallback", () => {
  it("accepts only the canonical literal IPv4 loopback callback", () => {
    const parsed = parseDesktopCallback(callback);
    expect(parsed.href).toBe(callback);
    expect(parsed.hostname).toBe("127.0.0.1");
    expect(parsed.port).toBe("49152");
    expect(parsed.search).toBe("");
    expect(parsed.hash).toBe("");
  });

  it.each([
    `https://127.0.0.1:49152/desktop/connect/${nonce}`,
    `http://localhost:49152/desktop/connect/${nonce}`,
    `http://[::1]:49152/desktop/connect/${nonce}`,
    `http://2130706433:49152/desktop/connect/${nonce}`,
    `http://0x7f000001:49152/desktop/connect/${nonce}`,
    `http://127.0.0.1:0/desktop/connect/${nonce}`,
    `http://127.0.0.1:65536/desktop/connect/${nonce}`,
    `http://127.0.0.1:049152/desktop/connect/${nonce}`,
    `http://user@127.0.0.1:49152/desktop/connect/${nonce}`,
    `http://127.0.0.1:49152/desktop/connect/short`,
    `http://127.0.0.1:49152/desktop/connect/${nonce.slice(0, -1)}-`,
    `http://127.0.0.1:49152/wrong/${nonce}`,
    `${callback}?token=redacted`,
    `${callback}#fragment`,
  ])("rejects %s", (candidate) => {
    expect(() => parseDesktopCallback(candidate)).toThrow("Invalid Codesk Desktop callback URL.");
  });
});

describe("parseDesktopHandoffPageURL", () => {
  it("extracts one callback from the page query", () => {
    const parsed = parseDesktopHandoffPageURL(
      `https://app.example.test/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}`,
    );
    expect(parsed.href).toBe(callback);
  });

  it.each([
    "https://app.example.test/desktop-handoff-spike.html",
    `file:///tmp/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}`,
    `https://user:pass@app.example.test/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}`,
    `https://app.example.test/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}&extra=1`,
    `https://app.example.test/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}&callback=${encodeURIComponent(callback)}`,
    `https://app.example.test/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}#fragment`,
    `https://app.example.test/desktop-handoff-spike.html?callback=${encodeURIComponent(callback)}#`,
  ])("rejects an ambiguous launch URL", (candidate) => {
    expect(() => parseDesktopHandoffPageURL(candidate)).toThrow("Invalid Codesk Desktop handoff page URL.");
  });
});

describe("createDesktopHandoffForm", () => {
  it("puts every credential field in a POST body and never in the action URL", () => {
    const form = createDesktopHandoffForm(document, callback, payload);
    expect(form.method).toBe("post");
    expect(form.enctype).toBe("application/x-www-form-urlencoded");
    expect(form.action).toBe(callback);
    expect(form.action).not.toContain(token);
    expect(form.acceptCharset).toBe("UTF-8");
    expect(form.hidden).toBe(true);

    const values = new FormData(form);
    expect(Object.fromEntries(values.entries())).toEqual({
      daemon_id: payload.daemonId,
      token: payload.token,
      workspace_id: payload.workspaceId,
      workspace_name: payload.workspaceName,
      workspace_slug: payload.workspaceSlug,
      workspace_url: payload.workspaceUrl,
    });
  });

  it("preserves the daemon token as an opaque credential", () => {
    const opaqueToken = "future.credential-format:v2/with spaces";
    const form = createDesktopHandoffForm(document, callback, { ...payload, token: opaqueToken });
    expect(new FormData(form).get("token")).toBe(opaqueToken);
  });

  it.each(invalidPayloads)("rejects %s", (_name, overrides) => {
    expect(() => createDesktopHandoffForm(document, callback, { ...payload, ...overrides })).toThrow(
      "Invalid Codesk Desktop handoff payload.",
    );
  });
});

describe("submitDesktopHandoff", () => {
  it("submits once and clears the sensitive form from the page", async () => {
    const submit = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => undefined);
    submitDesktopHandoff(callback, payload);
    expect(submit).toHaveBeenCalledTimes(1);
    const submitted = submit.mock.instances[0] as HTMLFormElement;
    expect(submitted.action).toBe(callback);
    expect(new FormData(submitted).get("token")).toBe(token);

    await Promise.resolve();
    expect(document.body.contains(submitted)).toBe(false);
    expect(Array.from(submitted.elements).every((element) => !(element instanceof HTMLInputElement) || element.value === "")).toBe(true);
  });
});
