import { describe, it, expect } from "vitest";
import { deriveOnboardingSignals, toOnboardingStep, resolveAgentWorkDestination } from "./onboardingController";
import { NODES, acknowledgeFlag } from "./onboardingEngine";
import type { WorkspaceState, Daemon, Agent, ThreadItem } from "./types";

// Minimal fixtures — only the fields deriveOnboardingSignals reads. Cast so the
// tests stay focused on the derivation, not on constructing full domain objects.
const NOW = 1_700_000_000_000;
const daemon = (over: Partial<Daemon>): Daemon => ({ status: "connected", receivedAtMs: NOW, ...over }) as Daemon;
const agent = (id: string): Agent => ({ id }) as Agent;
const thread = (participantIds: string[]): ThreadItem => ({ participantIds }) as ThreadItem;

function ws(partial: Partial<WorkspaceState>): WorkspaceState {
  return {
    workspaceId: "w",
    rootDocumentId: "",
    name: "",
    users: [],
    daemons: [],
    agents: [],
    agentRuns: [],
    threads: [],
    agentEvents: [],
    presences: {},
    ...partial,
  };
}

describe("deriveOnboardingSignals", () => {
  it("passes through the live counts the engine still consumes (#90 trimmed agent run/thread inputs)", () => {
    const signals = deriveOnboardingSignals({
      workspaceState: ws({ threads: [thread([]), thread([])], agents: [agent("a1")] }),
      documentCount: 3,
      nowMs: NOW,
    });
    expect(signals.documentCount).toBe(3);
    expect(signals.threadCount).toBe(2);
    expect(signals.agentCount).toBe(1);
    // agentRunCount / agentThreadCount were removed — no onboarding consumer after #90.
    expect("agentRunCount" in signals).toBe(false);
    expect("agentThreadCount" in signals).toBe(false);
  });

  it("counts a live environment by receipt-elapsed liveness, not by a raw 'online' status", () => {
    const online = daemon({ lastSeenAt: "2026-07-01T00:00:00Z", lastSeenAgeSeconds: 5 }); // fresh -> online
    // The row still says online, but the last check-in is stale by receipt-elapsed —
    // daemonLiveStatus decays it, so it must NOT satisfy "connect a local environment".
    const staleButRawOnline = daemon({ connectionStatus: "online", lastSeenAt: "2026-07-01T00:00:00Z", lastSeenAgeSeconds: 200 });
    const signals = deriveOnboardingSignals({
      workspaceState: ws({ daemons: [online, staleButRawOnline] }),
      documentCount: 0,
      nowMs: NOW,
    });
    expect(signals.liveEnvironmentCount).toBe(1);
  });
});

describe("P1->P2 adapter (toOnboardingStep) — the seam", () => {
  it("projects every NODE to a structural OnboardingStep, preserving the shared fields", () => {
    for (const node of NODES) {
      const step = toOnboardingStep(node);
      expect(step.id).toBe(node.id);
      expect(step.version).toBe(node.version);
      expect(step.scope).toBe(node.scope);
      expect(step.presentation).toBe(node.presentation);
      expect(step.targetOnboardingId).toBe(node.targetOnboardingId);
      expect(step.eyebrow).toBe(node.eyebrow);
      expect(step.title).toBe(node.title);
      expect(step.body).toBe(node.body);
      expect(step.caption).toBe(node.caption);
      expect(step.primaryAction).toEqual(node.primaryAction);
      expect(step.secondaryAction).toEqual(node.secondaryAction);
      expect(step.skippable).toBe(node.skippable);
      expect(step.fallback).toBe(node.fallback);
    }
  });

  it("pins the acknowledgeFlag delegation: the hook's seen-flag format equals P1's", () => {
    // useOnboarding records `seen:${step.id}@v${step.version}` inline; P1 owns the
    // canonical acknowledgeFlag(node). If either drifts, this breaks — which is the
    // whole point of pinning string equality rather than importing across the seam.
    for (const node of NODES) {
      const step = toOnboardingStep(node);
      expect(`seen:${step.id}@v${step.version}`).toBe(acknowledgeFlag(node));
    }
  });
});

describe("resolveAgentWorkDestination (#56 §2e — one rule, two surfaces)", () => {
  it("exactly one agent → that agent's Start run surface directly (no list hop)", () => {
    expect(resolveAgentWorkDestination([{ id: "agent-7" }])).toEqual({
      label: "Start a run",
      kind: "start-run",
      agentId: "agent-7",
    });
  });

  it("two or more agents → the Agents list chooser", () => {
    expect(resolveAgentWorkDestination([{ id: "a" }, { id: "b" }])).toEqual({
      label: "Choose an agent",
      kind: "agents-list",
    });
  });

  it("the label tracks the destination (never a static label hiding the outcome)", () => {
    expect(resolveAgentWorkDestination([{ id: "a" }]).label).toBe("Start a run");
    expect(resolveAgentWorkDestination([{ id: "a" }, { id: "b" }, { id: "c" }]).label).toBe("Choose an agent");
  });
});
