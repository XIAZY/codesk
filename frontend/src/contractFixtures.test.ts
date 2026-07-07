import { describe, it, expect } from "vitest";
import { contractGoldens } from "./contractFixtures";
import { emptyWorkspace, reduceWorkspaceEvent, workspacePeople } from "./logic";
import type { WorkspaceEvent } from "./types";

// The frontend half of the contract loop: feed the SAME goldens the backend pins through the real
// reducer and assert the app ingests them without crashing and in the shape consumers expect. A backend
// response-shape change regenerates the golden and lands here as a red frontend test too — one wire
// contract, pinned on both sides.

function snapshot(golden: unknown): WorkspaceEvent {
  return { type: "workspace.snapshot", data: golden as WorkspaceEvent["data"] };
}

describe("workspace contract goldens (backend↔frontend loop)", () => {
  it("ingests the empty-workspace golden without nulls or crashes", () => {
    const state = reduceWorkspaceEvent(emptyWorkspace(), snapshot(contractGoldens.workspaceGetEmpty));
    // A1 frontend-side: every collection consumers dereference is present and non-null.
    expect(Array.isArray(state.users)).toBe(true);
    expect(Array.isArray(state.daemons)).toBe(true);
    expect(Array.isArray(state.agents)).toBe(true);
    expect(Array.isArray(state.threads)).toBe(true);
    expect(state.users).toHaveLength(1); // the owner is always a member
    expect(state.daemons).toHaveLength(0);
    // The workspace-switch crash consumers must not throw on the real empty contract.
    expect(() => workspacePeople(state)).not.toThrow();
  });

  it("ingests the populated-workspace golden into the shape consumers expect", () => {
    const state = reduceWorkspaceEvent(emptyWorkspace(), snapshot(contractGoldens.workspaceGetPopulated));
    expect(state.daemons).toHaveLength(1);
    expect(state.agents).toHaveLength(1);
    expect(state.threads).toHaveLength(1);
    expect(() => workspacePeople(state)).not.toThrow();

    // presences is a map keyed by actorId (the presence.updated contract, logic.ts). A presence
    // delivered in the initial snapshot must be retrievable by its actorId, or the online ring
    // silently misses everyone who was already present when the workspace loaded.
    const golden = contractGoldens.workspaceGetPopulated as { presences: { actorId: string }[] };
    const presenceActorId = golden.presences[0].actorId;
    expect(state.presences[presenceActorId]).toBeDefined();
  });
});
