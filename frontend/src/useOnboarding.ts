import { useCallback, useEffect, useMemo, useState } from "react";
import type { OnboardingScope, OnboardingStep } from "./Onboarding";

type UseOnboardingOptions = {
  steps: readonly OnboardingStep[];
  completedIds: ReadonlySet<string>;
  // The event set + writer are owned by useScopedEventFlags (one keyed store) and
  // passed in — the hook never keeps its own copy, so a scope switch can't leak.
  events: ReadonlySet<string>;
  record: (event: string, scope: OnboardingScope) => void;
  scopeKey: string; // account×workspace identity — resets the session on a switch
  checklistDismissedKey: string;
  activeSpotlightId?: string | null;
  tip?: OnboardingStep | null;
  enabled?: boolean;
};

type SessionState = {
  key: string;
  // An original step index the user Backed to (session-only presentation), or null =
  // track the engine frontier. Never persisted; durable completion is never moved by it.
  revisit: number | null;
  skipped: boolean;
};

function storedTrue(key: string): boolean {
  if (!key || typeof window === "undefined") return false;
  return window.localStorage.getItem(key) === "true";
}

function persistTrue(key: string) {
  if (!key || typeof window === "undefined") return;
  try {
    window.localStorage.setItem(key, "true");
  } catch {
    // ignore storage failures (private mode) — dismissal just doesn't persist this session.
  }
}

function newSession(key: string): SessionState {
  return { key, revisit: null, skipped: false };
}

export function useOnboarding({
  steps,
  completedIds,
  events,
  record,
  scopeKey,
  checklistDismissedKey,
  activeSpotlightId,
  tip = null,
  enabled = true,
}: UseOnboardingOptions) {
  const [sessionState, setSessionState] = useState<SessionState>(() => newSession(scopeKey));
  // checklistDismissed is the one genuinely non-derivable durable flag (the user's
  // state can't answer "did they dismiss the card"), so it stays stored — keyed by
  // account×workspace like every other durable value.
  const [checklistState, setChecklistState] = useState<{ key: string; value: boolean }>(() => ({
    key: checklistDismissedKey,
    value: storedTrue(checklistDismissedKey),
  }));

  // WorkspaceApp keeps this hook mounted across account/workspace switches. Key the
  // session and the dismissed flag so the first render in the new scope is already honest.
  const freshSession = useMemo(() => newSession(scopeKey), [scopeKey]);
  const session = sessionState.key === scopeKey ? sessionState : freshSession;
  const checklistDismissed = checklistState.key === checklistDismissedKey
    ? checklistState.value
    : storedTrue(checklistDismissedKey);

  useEffect(() => {
    setSessionState(newSession(scopeKey));
  }, [scopeKey]);
  useEffect(() => {
    setChecklistState({ key: checklistDismissedKey, value: storedTrue(checklistDismissedKey) });
  }, [checklistDismissedKey]);

  const updateSession = useCallback((update: (current: SessionState) => SessionState) => {
    setSessionState((current) => update(current.key === scopeKey ? current : newSession(scopeKey)));
  }, [scopeKey]);

  // The frontier: the first step whose completion condition doesn't hold yet — where the
  // guide honestly "wants" to be. Guide completion is DERIVED from this (all steps
  // complete ⇒ frontier past the end ⇒ nothing shows); no stored boolean exists to defeat
  // a version bump — bump any node and its versioned flag stops matching, reopening it.
  const frontierIndex = useMemo(() => {
    const index = steps.findIndex((step) => !completedIds.has(step.id));
    return index < 0 ? steps.length : index;
  }, [steps, completedIds]);

  // A Back revisit is honored only while it sits BEHIND the frontier (a completed prior
  // step); once the frontier catches or passes it the revisit clears and we track the
  // frontier again. Revisit is session-only presentation and never moves completion.
  const revisitIndex = session.revisit !== null && session.revisit < frontierIndex ? session.revisit : null;

  // The spotlight to show. An explicit Back revisit wins over the engine's activeNode
  // (Juan's session precedence); otherwise defer to activeSpotlightId, which is null when
  // the frontier step isn't triggered yet or the whole guide is complete (wait-don't-skip).
  const spotlightIndex = revisitIndex !== null
    ? revisitIndex
    : activeSpotlightId != null
      ? steps.findIndex((step) => step.id === activeSpotlightId)
      : -1;
  const spotlightStep = !session.skipped && spotlightIndex >= 0 ? steps[spotlightIndex] ?? null : null;
  // P1 can expose a spotlight and a tip at once. The guide wins; the still-triggered tip
  // is not consumed and resurfaces once the guide is done.
  const active = enabled ? spotlightStep ?? tip : null;

  const acknowledge = useCallback((node: Pick<OnboardingStep, "id" | "version" | "scope">) => {
    record(`seen:${node.id}@v${node.version}`, node.scope);
  }, [record]);

  const next = useCallback(() => {
    // From a revisited step, Next returns FORWARD to the frontier — no re-acknowledgement,
    // durable completion untouched.
    if (revisitIndex !== null) {
      updateSession((current) => ({ ...current, revisit: null }));
      return;
    }
    // At the frontier, Next acknowledges the current spotlight; completing it advances the
    // frontier (derived) on the next render. The guide ends when the last step completes.
    if (spotlightStep && spotlightStep.presentation === "spotlight") {
      acknowledge(spotlightStep);
    }
  }, [acknowledge, revisitIndex, spotlightStep, updateSession]);

  const back = useCallback(() => {
    if (spotlightIndex <= 0) return;
    const previous = spotlightIndex - 1;
    updateSession((current) => ({ ...current, revisit: previous }));
  }, [spotlightIndex, updateSession]);

  // Guide Skip is session-only and resumable. A contextual tip's Skip/Escape is a durable
  // acknowledgement in that node's own account/workspace scope.
  const skip = useCallback(() => {
    if (active?.presentation === "tip") {
      acknowledge(active);
      return;
    }
    updateSession((current) => ({ ...current, skipped: true }));
  }, [acknowledge, active, updateSession]);
  const resume = useCallback(() => {
    updateSession((current) => ({ ...current, skipped: false }));
  }, [updateSession]);

  const dismissChecklist = useCallback(() => {
    persistTrue(checklistDismissedKey);
    setChecklistState({ key: checklistDismissedKey, value: true });
  }, [checklistDismissedKey]);

  return {
    active,
    // Presentation binds to the ORIGINAL guide position + total step count, never the
    // completion-filtered remaining list — a stable 1/3 → 2/3 → 3/3 even after Back.
    stepIndex: spotlightIndex >= 0 ? spotlightIndex : 0,
    total: steps.length,
    next,
    back,
    skip,
    resume,
    record,
    acknowledge,
    recordedEvents: events,
    checklistDismissed,
    dismissChecklist,
  };
}
