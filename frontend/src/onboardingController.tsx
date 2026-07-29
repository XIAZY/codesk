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
  activeChapter,
  chapterSteps,
  nextChapterStep,
  audienceMatches,
  isComplete,
  checklistProgress,
  type OnboardingNode,
  type OnboardingContext,
  type OnboardingLiveSignals,
  type OnboardingRole,
} from "./onboardingEngine";

// ---- Live-signal derivation (pure) -------------------------------------------

export type OnboardingSignalInput = {
  workspaceState: WorkspaceState;
  documentCount: number; // rootDocuments.length — held by WorkspaceApp, not WorkspaceState
  nowMs: number; // for daemon liveness decay
};

// Compute the engine's live signals from WorkspaceApp state. The one honesty rule
// that matters: liveEnvironmentCount counts a daemon only when daemonLiveStatus
// reads "online" (receipt-elapsed liveness with a genuine check-in) — never a raw
// status field, so a stale-but-"online" daemon does not satisfy the local-env node.
export function deriveOnboardingSignals(input: OnboardingSignalInput): OnboardingLiveSignals {
  const { workspaceState, documentCount, nowMs } = input;
  return {
    documentCount,
    threadCount: workspaceState.threads.length,
    liveEnvironmentCount: workspaceState.daemons.filter((daemon) => daemonLiveStatus(daemon, nowMs) === "online").length,
    agentCount: workspaceState.agents.length,
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
    eyebrow: node.eyebrow,
    title: node.title,
    body: node.body,
    caption: node.caption,
    primaryAction: node.primaryAction,
    secondaryAction: node.secondaryAction,
    skippable: node.skippable,
    fallback: node.fallback,
  };
}

// ---- Shared "work with an agent" destination (#56 §2e) -----------------------

// One rule for BOTH the owner/admin chapter step 3 CTA and the member work card so the
// two surfaces can't diverge (Anton/Juan): exactly 1 agent → that agent's Start run
// surface directly (no list hop); 2+ → the Agents list. The label is state-dependent for
// the same reason — a static label would hide the materially different outcome. 0 agents
// can't reach here (the card requires agent-exists), but falls back to the list safely.
export type AgentWorkDestination =
  | { label: "Start a run"; kind: "start-run"; agentId: string }
  | { label: "Choose an agent"; kind: "agents-list" };

export function resolveAgentWorkDestination(agents: readonly { id: string }[]): AgentWorkDestination {
  if (agents.length === 1) return { label: "Start a run", kind: "start-run", agentId: agents[0].id };
  return { label: "Choose an agent", kind: "agents-list" };
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
    // #90 promotion-dismiss: distinct from checklistDismissed. A per account×workspace,
    // browser-local presentation flag (no DB write) — "Not now"/Close on an AUTO-opened
    // teammate chapter sets it, permanently suppressing future auto-opens in this
    // workspace/profile. It records NO completion; the checklist row + manual open stay.
    teammatePromoDismissedKey: `${workspace}.teammatePromoDismissed`,
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
  nowMs: number;
};

// Builds the engine context from live data, derives which nodes are complete and
// which step/tip is active, and drives useOnboarding. WorkspaceApp calls this once
// and renders <Onboarding step={active} .../> plus the checklist.
export function useOnboardingController(input: OnboardingControllerInput) {
  const { enabled, accountId, workspaceId, roles, route, selectionActive, workspaceState, documentCount, nowMs } = input;
  const keys = useMemo(() => onboardingFlagKeys(accountId, workspaceId), [accountId, workspaceId]);

  // ONE keyed store owns the recorded event flags. The engine context and
  // useOnboarding both read `flags.events` and write through `flags.record`, so a
  // scope switch rehydrates from the current keys — there is no duplicate copy to go
  // stale (the leak this replaces) and completion stays a single source of truth.
  const flags = useScopedEventFlags(keys.accountFlagsKey, keys.workspaceFlagsKey);

  const signals = useMemo(
    () => deriveOnboardingSignals({ workspaceState, documentCount, nowMs }),
    [workspaceState, documentCount, nowMs],
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

  // The chapter card for the current role + live state (true next step / done card /
  // null), plus its position among the role's steps for the step dots + entry badge.
  const chapterNode = useMemo(() => activeChapter(ctx), [ctx]);
  const chapter = useMemo(() => (chapterNode ? toOnboardingStep(chapterNode) : null), [chapterNode]);
  const chapterStepsList = useMemo(() => chapterSteps(ctx), [ctx]);
  const chapterTotal = chapterStepsList.length;
  const chapterStepIndex = chapterNode ? chapterStepsList.findIndex((s) => s.id === chapterNode.id) : -1;

  // #90 promotion eligibility: owner/admin AUDIENCE (never members — the member-exclusion
  // gate is `audience`, not eligibleRoles) AND activation is incomplete (there is a genuine
  // live-incomplete chapter step to auto-open at; null once env+agent both hold → never
  // auto-open a complete workspace). The hook owns the fire timing (fresh vs catch-up).
  const promotable = useMemo(
    () => audienceMatches({ audience: "owner-admin" }, ctx) && nextChapterStep(ctx) !== null,
    [ctx],
  );
  const documentOpen = route === "document";

  const hook = useOnboarding({
    steps,
    completedIds,
    events: flags.events,
    record: flags.record,
    scopeKey: keys.workspaceFlagsKey,
    checklistDismissedKey: keys.checklistDismissedKey,
    teammatePromoDismissedKey: keys.teammatePromoDismissedKey,
    activeSpotlightId,
    tip,
    chapter,
    chapterStepIndex,
    chapterTotal,
    promotable,
    documentCount,
    documentOpen,
    enabled,
  });

  // The getting-started checklist for Vitaliy's OnboardingChecklist to render —
  // each item with its live-derived done state.
  const checklist = useMemo(() => checklistProgress(ctx), [ctx]);

  return { ...hook, checklist };
}

export type { OnboardingScope };
