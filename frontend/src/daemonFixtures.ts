// Canonical daemon fixtures — the single source of truth for daemon objects in tests. These are the
// SAME six lifecycle states the backend emits, imported directly from the Go contract-pin golden
// file (backend/internal/notty/testdata/daemon_states.json, produced by TestDaemonStatesGoldenContract).
// Importing the real wire bytes means a backend wire-format change breaks these fixtures — and the
// tests built on them — instead of letting the frontend drift silently against a hand-copied shape.
//
// The JSON lives outside frontend/src, so it is opted into the build in three places:
//   - frontend/tsconfig.json     -> "include" lists the testdata file so tsc type-checks the import
//   - frontend/vite.config.ts    -> server.fs.allow grants the dev server access outside the root
//   - resolveJsonModule (already on) makes the import type-safe.
import daemonStates from "../../backend/internal/notty/testdata/daemon_states.json";
import type { Daemon } from "./types";

// The golden file is a JSON object keyed by state name; each value is the real json.Marshal of the
// constructed *Daemon, so it structurally satisfies the frontend Daemon type.
const states = daemonStates as Record<string, Daemon>;

export const daemonFixtures = {
  neverSeen: states.neverSeen,
  justSeen: states.justSeen,
  aging: states.aging,
  stale: states.stale,
  dead: states.dead,
  softDeleted: states.softDeleted,
} satisfies Record<string, Daemon>;

export type DaemonFixtureName = keyof typeof daemonFixtures;

// The frontend-only receipt stamp: daemonLiveStatus decays a payload relative to when it landed, so
// tests attach a receivedAtMs the same way stampDaemonReceipt does at the socket/snapshot boundary.
export function withReceipt(daemon: Daemon, receivedAtMs: number): Daemon {
  return { ...daemon, receivedAtMs };
}

// All six fixtures as an array, for tests that render a non-trivial mix.
export function allDaemonFixtures(): Daemon[] {
  return Object.values(daemonFixtures);
}
