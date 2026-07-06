import { describe, expect, it } from "vitest";
import { resolveRuntimeTiles, selectableRuntimeKinds } from "./runtimes";
import type { Daemon } from "./types";

function daemon(runtimes: Daemon["runtimes"]): Daemon {
  return {
    id: "d1",
    workspaceId: "w1",
    name: "daemon-prod",
    status: "active",
    runtimes,
    createdAt: "2026-01-01T00:00:00Z",
  };
}

describe("resolveRuntimeTiles", () => {
  it("marks codex available and selectable when the daemon reports it", () => {
    const tiles = resolveRuntimeTiles(daemon([{ kind: "codex", available: true, version: "codex-cli 0.134.0" }]));
    const codex = tiles.find((tile) => tile.entry.kind === "codex");
    expect(codex?.availability).toBe("available");
    expect(codex?.meta).toBe("codex-cli 0.134.0");
    expect(selectableRuntimeKinds(tiles)).toEqual(["codex"]);
  });

  it("marks claude-code available and selectable when the daemon reports it", () => {
    const tiles = resolveRuntimeTiles(daemon([{ kind: "claude-code", available: true, version: "2.1.201 (Claude Code)" }]));
    const claude = tiles.find((tile) => tile.entry.kind === "claude-code");
    expect(claude?.availability).toBe("available");
    expect(claude?.meta).toBe("2.1.201 (Claude Code)");
    expect(selectableRuntimeKinds(tiles)).toEqual(["claude-code"]);
  });

  it("treats unsupported registry runtimes as coming_soon regardless of daemon report", () => {
    const tiles = resolveRuntimeTiles(daemon([{ kind: "pi", available: true, version: "1.8.0" }]));
    const pi = tiles.find((tile) => tile.entry.kind === "pi");
    expect(pi?.availability).toBe("coming_soon");
    // A roadmap runtime never becomes selectable, even if a daemon has it.
    expect(selectableRuntimeKinds(tiles)).toEqual([]);
  });

  it("marks codex not_installed when the daemon does not report it", () => {
    const tiles = resolveRuntimeTiles(daemon([]));
    const codex = tiles.find((tile) => tile.entry.kind === "codex");
    expect(codex?.availability).toBe("not_installed");
    expect(selectableRuntimeKinds(tiles)).toEqual([]);
  });

  it("classifies a version-floor reason as update_required", () => {
    const tiles = resolveRuntimeTiles(
      daemon([{ kind: "codex", available: false, reason: "codex CLI version 0.1.0 below required floor" }]),
    );
    const codex = tiles.find((tile) => tile.entry.kind === "codex");
    expect(codex?.availability).toBe("update_required");
  });

  it("surfaces an unknown reported runtime as coming_soon", () => {
    const tiles = resolveRuntimeTiles(daemon([{ kind: "mystery-cli", available: true }]));
    const mystery = tiles.find((tile) => tile.entry.kind === "mystery-cli");
    expect(mystery?.availability).toBe("coming_soon");
    expect(selectableRuntimeKinds(tiles)).toEqual([]);
  });

  it("returns only roadmap tiles when no daemon is selected", () => {
    const tiles = resolveRuntimeTiles(undefined);
    expect(selectableRuntimeKinds(tiles)).toEqual([]);
    expect(tiles.find((tile) => tile.entry.kind === "codex")?.availability).toBe("not_installed");
  });
});
