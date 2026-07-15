// Phase-2 wiring: bridges the pure onboarding engine (onboarding.ts) to the P2
// overlay (Onboarding.tsx / useOnboarding.ts) and to WorkspaceApp's live data.
// Kept out of both parallel lanes' files so nothing cross-imports until here.
import { useMemo } from "react";
import { daemonLiveStatus } from "./logic";
import type { WorkspaceState } from "./types";
import type { OnboardingStep, OnboardingScope } from "./Onboarding";
import { useOnboarding } from "./useOnboarding";
import { useScopedEventFlags } from "./useScopedEventFlags";
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

// ---- Persistence keys (plan §4.3/§6.1) ---------------------------------------

// Every durable value is keyed PER USER: account flags by accountId (they follow the
// user across workspaces but never across accounts on a shared browser); workspace
// flags + the checklist-dismissed flag by accountId × workspaceId (two members of a
// shared workspace keep separate onboarding state). The browser is not the user.
export function onboardingFlagKeys(accountId: string, workspaceId: string) {
  const account = `codesk.onboarding.account.${accountId}`;
  const workspace = `${account}.ws.${workspaceId}`;
  return {
    accountFlagsKey: `${account}.flags`,
    workspaceFlagsKey: `${workspace}.flags`,
    checklistDismissedKey: `${workspace}.checklistDismissed`,
  };
}

// ---- Controller hook ---------------------------------------------------------

export type OnboardingControllerInput = {
  enabled: boolean;
  accountId: string;
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
  const { enabled, accountId, workspaceId, roles, route, selectionActive, workspaceState, documentCount, watchedDocumentCount, nowMs } = input;
  const keys = useMemo(() => onboardingFlagKeys(accountId, workspaceId), [accountId, workspaceId]);

  // ONE keyed store owns the recorded event flags. The engine context and
  // useOnboarding both read `flags.events` and write through `flags.record`, so a
  // scope switch rehydrates from the current keys — there is no duplicate copy to go
  // stale (the leak this replaces) and completion stays a single source of truth.
  const flags = useScopedEventFlags(keys.accountFlagsKey, keys.workspaceFlagsKey);

  const signals = useMemo(
    () => deriveOnboardingSignals({ workspaceState, documentCount, watchedDocumentCount, nowMs }),
    [workspaceState, documentCount, watchedDocumentCount, nowMs],
  );

  const ctx: OnboardingContext = useMemo(
    () => ({ roles, events: flags.events, route, selectionActive, signals }),
    [roles, flags.events, route, selectionActive, signals],
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
    events: flags.events,
    record: flags.record,
    scopeKey: keys.workspaceFlagsKey,
    checklistDismissedKey: keys.checklistDismissedKey,
    activeSpotlightId,
    tip,
    enabled,
  });

  // The getting-started checklist for Vitaliy's OnboardingChecklist to render —
  // each item with its live-derived done state.
  const checklist = useMemo(() => checklistProgress(ctx), [ctx]);

  return { ...hook, checklist };
}

export type { OnboardingScope };
