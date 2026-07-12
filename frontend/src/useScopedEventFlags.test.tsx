// @vitest-environment jsdom

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { useScopedEventFlags } from "./useScopedEventFlags";

afterEach(() => {
  localStorage.clear();
  cleanup();
});

const acctA = "codesk.onboarding.account.A.flags";
const acctB = "codesk.onboarding.account.B.flags";
const wsA = "codesk.onboarding.account.A.ws.a.flags";
const wsB = "codesk.onboarding.account.A.ws.b.flags";

describe("useScopedEventFlags", () => {
  it("records into the scope-appropriate key, dedups, and merges both scopes into events", () => {
    const { result } = renderHook(() => useScopedEventFlags(acctA, wsA));
    act(() => result.current.record("member_invited", "workspace"));
    act(() => result.current.record("member_invited", "workspace")); // dedup — no double write
    act(() => result.current.record("first_thread_created", "account"));
    expect(JSON.parse(localStorage.getItem(wsA) ?? "[]")).toEqual(["member_invited"]);
    expect(JSON.parse(localStorage.getItem(acctA) ?? "[]")).toEqual(["first_thread_created"]);
    expect(result.current.events.has("member_invited")).toBe(true);
    expect(result.current.events.has("first_thread_created")).toBe(true);
  });

  it("rehydrates on a workspace switch — A's workspace flag can't suppress B; B's own flag loads", () => {
    localStorage.setItem(wsB, JSON.stringify(["member_invited"])); // B already recorded it
    const { result, rerender } = renderHook(
      ({ workspaceKey }: { workspaceKey: string }) => useScopedEventFlags(acctA, workspaceKey),
      { initialProps: { workspaceKey: wsA } },
    );
    act(() => result.current.record("member_invited", "workspace"));
    expect(result.current.events.has("member_invited")).toBe(true);
    // Switch to a fresh workspace: A's flag must NOT leak.
    rerender({ workspaceKey: "codesk.onboarding.account.A.ws.c.flags" });
    expect(result.current.events.has("member_invited")).toBe(false);
    expect(JSON.parse(localStorage.getItem(wsA) ?? "[]")).toEqual(["member_invited"]); // A untouched
    // Switch to workspace B, which persisted its own: it loads immediately.
    rerender({ workspaceKey: wsB });
    expect(result.current.events.has("member_invited")).toBe(true);
  });

  it("rehydrates on an account switch — account A's flag can't suppress a fresh account; B's own loads", () => {
    localStorage.setItem(acctB, JSON.stringify(["first_thread_created"]));
    const { result, rerender } = renderHook(
      ({ accountKey }: { accountKey: string }) => useScopedEventFlags(accountKey, wsA),
      { initialProps: { accountKey: acctA } },
    );
    act(() => result.current.record("first_thread_created", "account"));
    expect(result.current.events.has("first_thread_created")).toBe(true);
    // Sign out A, sign in a fresh account C on the same browser: A's account flag does NOT leak.
    rerender({ accountKey: "codesk.onboarding.account.C.flags" });
    expect(result.current.events.has("first_thread_created")).toBe(false);
    // Account B, which persisted its own, loads on switch.
    rerender({ accountKey: acctB });
    expect(result.current.events.has("first_thread_created")).toBe(true);
  });

  it("keeps an account flag across a workspace switch within the same account", () => {
    const { result, rerender } = renderHook(
      ({ workspaceKey }: { workspaceKey: string }) => useScopedEventFlags(acctA, workspaceKey),
      { initialProps: { workspaceKey: wsA } },
    );
    act(() => result.current.record("first_thread_created", "account"));
    rerender({ workspaceKey: wsB }); // same account, different workspace
    expect(result.current.events.has("first_thread_created")).toBe(true); // account-durable
  });
});
