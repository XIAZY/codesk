// Phase-2 wiring: bridges the pure onboarding engine (onboarding.ts) to the P2
// overlay (Onboarding.tsx / useOnboarding.ts) and to WorkspaceApp's live data.
// Kept out of both parallel lanes' files so nothing cross-imports until here.
import { useMemo, useState, useCallback } from "react";
import { daemonLiveStatus } from "./logic";
import type { WorkspaceState } from "./types";
import type { OnboardingStep, OnboardingScope } from "./Onboarding";
import { useOnboarding } from "./useOnboarding";
import {
  eligibleNodes,
  guideSteps,
  activeNode,
  activeTip,
  isComplete,
  checklistProgress,
  type OnboardingNode,
  type OnboardingContext,
  type OnboardingLiveSignals,
  type OnboardingRole,
} from "./onboarding";

// ---- Live-signal derivation (pure) -------------------------------------------

export type OnboardingSignalInput = {
  workspaceState: WorkspaceState;
  documentCount: number; // rootDocuments.length — held by WorkspaceApp, not WorkspaceState
  watchedDocumentCount: number; // document subscriptions the user has added
  nowMs: number; // for daemon liveness decay
};

// Compute the engine's live signals from WorkspaceApp state. The one honesty rule
// that matters: liveEnvironmentCount counts a daemon only when daemonLiveStatus
// reads "online" (receipt-elapsed liveness with a genuine check-in) — never a raw
// status field, so a stale-but-"online" daemon does not satisfy the local-env node.
export function deriveOnboardingSignals(input: OnboardingSignalInput): OnboardingLiveSignals {
  const { workspaceState, documentCount, watchedDocumentCount, nowMs } = input;
  const agentIds = new Set(workspaceState.agents.map((agent) => agent.id));
  return {
    documentCount,
    threadCount: workspaceState.threads.length,
    liveEnvironmentCount: workspaceState.daemons.filter((daemon) => daemonLiveStatus(daemon, nowMs) === "online").length,
    agentCount: workspaceState.agents.length,
    agentRunCount: workspaceState.agentRuns.length,
    agentThreadCount: workspaceState.threads.filter((thread) => thread.participantIds.some((id) => agentIds.has(id))).length,
    watchedDocumentCount,
  };
}

// ---- Adapter: OnboardingNode -> OnboardingStep -------------------------------

// The P1/P2 seam. OnboardingStep is a structural subset of OnboardingNode (it drops
// eligibleRoles/trigger/completion); the shared fields carry identical unions after
// the tightening, so this is a pure projection.
export function toOnboardingStep(node: OnboardingNode): OnboardingStep {
  return {
    id: node.id,
    version: node.version,
    scope: node.scope,
    presentation: node.presentation,
    targetOnboardingId: node.targetOnboardingId,
    title: node.title,
    body: node.body,
    primaryAction: node.primaryAction,
    secondaryAction: node.secondaryAction,
    skippable: node.skippable,
    fallback: node.fallback,
  };
}

// ---- Persistence keys (plan §6.1) --------------------------------------------

// Account flags persist per user across workspaces; workspace flags per workspace.
// Scope-partitioned storage is the P2 acceptance contract (no cross-workspace leak).
export function onboardingFlagKeys(workspaceId: string) {
  return {
    accountFlagsKey: "codesk.onboarding.account.flags",
    workspaceFlagsKey: `codesk.onboarding.ws.${workspaceId}.flags`,
    guideCompletedKey: `codesk.onboarding.ws.${workspaceId}.guideCompleted`,
    checklistDismissedKey: `codesk.onboarding.ws.${workspaceId}.checklistDismissed`,
  };
}

function readStored(key: string): ReadonlySet<string> {
  if (typeof window === "undefined") return new Set();
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(key) ?? "[]");
    return new Set(Array.isArray(value) ? value.filter((event): event is string => typeof event === "string") : []);
  } catch {
    return new Set();
  }
}

// ---- Controller hook ---------------------------------------------------------

export type OnboardingControllerInput = {
  enabled: boolean;
  workspaceId: string;
  roles: OnboardingRole[];
  route: string; // active view, e.g. "document"
  selectionActive: boolean;
  workspaceState: WorkspaceState;
  documentCount: number;
  watchedDocumentCount: number;
  nowMs: number;
};

// Builds the engine context from live data, derives which nodes are complete and
// which step/tip is active, and drives useOnboarding. WorkspaceApp calls this once
// and renders <Onboarding step={active} .../> plus the checklist.
export function useOnboardingController(input: OnboardingControllerInput) {
  const { enabled, workspaceId, roles, route, selectionActive, workspaceState, documentCount, watchedDocumentCount, nowMs } = input;
  const keys = useMemo(() => onboardingFlagKeys(workspaceId), [workspaceId]);

  // The engine needs the recorded events to derive completion; the hook records and
  // persists them, and echoes each write back through onRecord so this recompute
  // stays in sync without a second read of localStorage.
  const [recordedEvents, setRecordedEvents] = useState<ReadonlySet<string>>(
    () => new Set([...readStored(keys.accountFlagsKey), ...readStored(keys.workspaceFlagsKey)]),
  );
  const syncEvents = useCallback((event: string) => {
    setRecordedEvents((current) => (current.has(event) ? current : new Set([...current, event])));
  }, []);

  const signals = useMemo(
    () => deriveOnboardingSignals({ workspaceState, documentCount, watchedDocumentCount, nowMs }),
    [workspaceState, documentCount, watchedDocumentCount, nowMs],
  );

  const ctx: OnboardingContext = useMemo(
    () => ({ roles, events: recordedEvents, route, selectionActive, signals }),
    [roles, recordedEvents, route, selectionActive, signals],
  );

  const steps = useMemo(() => guideSteps(ctx).map(toOnboardingStep), [ctx]);
  const completedIds = useMemo(
    () => new Set(eligibleNodes(ctx).filter((node) => isComplete(node, ctx)).map((node) => node.id)),
    [ctx],
  );
  const activeSpotlightId = useMemo(() => activeNode(ctx)?.id ?? null, [ctx]);
  const tip = useMemo(() => {
    const node = activeTip(ctx);
    return node ? toOnboardingStep(node) : null;
  }, [ctx]);

  const hook = useOnboarding({
    steps,
    completedIds,
    guideCompletedKey: keys.guideCompletedKey,
    checklistDismissedKey: keys.checklistDismissedKey,
    accountFlagsKey: keys.accountFlagsKey,
    workspaceFlagsKey: keys.workspaceFlagsKey,
    activeSpotlightId,
    tip,
    enabled,
    // Pass syncEvents directly (not wrapped) so the hook's `record` stays a stable
    // reference — WorkspaceApp's createDocument callback depends on it.
    onRecord: syncEvents,
  });

  // The getting-started checklist for Vitaliy's OnboardingChecklist to render —
  // each item with its live-derived done state.
  const checklist = useMemo(() => checklistProgress(ctx), [ctx]);

  return { ...hook, checklist };
}

export type { OnboardingScope };
