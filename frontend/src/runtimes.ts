import type { Daemon } from "./types";

// notty's supported-runtime registry. A runtime is "supported" when notty knows
// how to drive it; only those can actually host an agent. Everything else is a
// roadmap entry shown as "coming soon" — independent of any individual daemon.
// Today notty only integrates Codex.
export type RuntimeRegistryEntry = {
  kind: string;
  label: string;
  monogram: string;
  tile?: "codex" | "claude" | "gemini" | "pi";
  supported: boolean;
};

export const SUPPORTED_RUNTIME_REGISTRY: RuntimeRegistryEntry[] = [
  { kind: "codex", label: "Codex", monogram: "Cx", tile: "codex", supported: true },
  { kind: "claude-code", label: "Claude Code", monogram: "CC", tile: "claude", supported: false },
  { kind: "pi", label: "pi", monogram: "π", tile: "pi", supported: false },
  { kind: "opencode", label: "opencode", monogram: "oc", supported: false },
];

// available     — supported by notty and installed/available on the selected daemon (selectable)
// not_installed — supported by notty but the daemon's host doesn't have the CLI (host-fixable)
// update_required — supported, present, but the CLI is below the supported floor (host-fixable)
// coming_soon   — not yet supported by notty; same on every daemon
export type RuntimeAvailability = "available" | "not_installed" | "update_required" | "coming_soon";

export type RuntimeTile = {
  entry: RuntimeRegistryEntry;
  availability: RuntimeAvailability;
  meta: string;
};

export function normalizeRuntimeKind(kind: string): string {
  return kind.trim().toLowerCase();
}

export function resolveRuntimeTiles(daemon?: Daemon): RuntimeTile[] {
  const detections = daemon?.runtimes ?? [];
  const registryKinds = new Set(SUPPORTED_RUNTIME_REGISTRY.map((entry) => entry.kind));

  const tiles: RuntimeTile[] = SUPPORTED_RUNTIME_REGISTRY.map((entry) => {
    if (!entry.supported) {
      return { entry, availability: "coming_soon", meta: "Coming soon to notty" };
    }
    const detection = detections.find((runtime) => normalizeRuntimeKind(runtime.kind) === entry.kind);
    if (detection?.available) {
      return { entry, availability: "available", meta: detection.version?.trim() || "Installed" };
    }
    const reason = detection?.reason?.trim();
    if (reason && /update|version|outdated|older|requires|floor/i.test(reason)) {
      return { entry, availability: "update_required", meta: reason };
    }
    return { entry, availability: "not_installed", meta: reason || "Not installed on host" };
  });

  // Surface any runtimes the daemon reports that notty doesn't support yet, so an
  // operator who installed e.g. a CLI we don't integrate understands why it's unavailable.
  for (const detection of detections) {
    const kind = normalizeRuntimeKind(detection.kind);
    if (!kind || registryKinds.has(kind)) {
      continue;
    }
    registryKinds.add(kind);
    tiles.push({
      entry: { kind, label: detection.kind.trim(), monogram: kind.slice(0, 2), supported: false },
      availability: "coming_soon",
      meta: "Coming soon to notty",
    });
  }

  return tiles;
}

export function selectableRuntimeKinds(tiles: RuntimeTile[]): string[] {
  return tiles.filter((tile) => tile.availability === "available").map((tile) => tile.entry.kind);
}
