import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { OnboardingScope, OnboardingStep } from "./Onboarding";

type UseOnboardingOptions = {
  steps: readonly OnboardingStep[];
  completedIds: ReadonlySet<string>;
  guideCompletedKey: string;
  checklistDismissedKey?: string;
  accountFlagsKey: string;
  workspaceFlagsKey: string;
  activeSpotlightId?: string | null;
  tip?: OnboardingStep | null;
  enabled?: boolean;
  onRecord?: (event: string, scope: OnboardingScope) => void;
};

type SessionState = {
  key: string;
  cursor: number;
  skipped: boolean;
};

type KeyedBoolean = {
  key: string | undefined;
  value: boolean;
};

type KeyedEvents = {
  key: string;
  events: ReadonlySet<string>;
};

function storedTrue(key: string | undefined): boolean {
  if (!key || typeof window === "undefined") return false;
  return window.localStorage.getItem(key) === "true";
}

function persistTrue(key: string | undefined) {
  if (!key || typeof window === "undefined") return;
  window.localStorage.setItem(key, "true");
}

function storedEvents(key: string): ReadonlySet<string> {
  if (!key || typeof window === "undefined") return new Set();
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(key) ?? "[]");
    return new Set(Array.isArray(value) ? value.filter((event): event is string => typeof event === "string") : []);
  } catch {
    return new Set();
  }
}

function persistEvents(key: string, events: ReadonlySet<string>) {
  if (!key || typeof window === "undefined") return;
  window.localStorage.setItem(key, JSON.stringify([...events].sort()));
}

function newSession(key: string): SessionState {
  return { key, cursor: 0, skipped: false };
}

export function useOnboarding({
  steps,
  completedIds,
  guideCompletedKey,
  checklistDismissedKey,
  accountFlagsKey,
  workspaceFlagsKey,
  activeSpotlightId,
  tip = null,
  enabled = true,
  onRecord,
}: UseOnboardingOptions) {
  const [sessionState, setSessionState] = useState<SessionState>(() => newSession(guideCompletedKey));
  const [guideState, setGuideState] = useState<KeyedBoolean>(() => ({
    key: guideCompletedKey,
    value: storedTrue(guideCompletedKey),
  }));
  const [checklistState, setChecklistState] = useState<KeyedBoolean>(() => ({
    key: checklistDismissedKey,
    value: storedTrue(checklistDismissedKey),
  }));
  const [accountEventState, setAccountEventState] = useState<KeyedEvents>(() => ({
    key: accountFlagsKey,
    events: storedEvents(accountFlagsKey),
  }));
  const [workspaceEventState, setWorkspaceEventState] = useState<KeyedEvents>(() => ({
    key: workspaceFlagsKey,
    events: storedEvents(workspaceFlagsKey),
  }));
  const accountEventsRef = useRef(accountEventState);
  const workspaceEventsRef = useRef(workspaceEventState);

  // WorkspaceApp keeps this hook mounted while its workspace changes. Key every
  // durable and session value so the first render in the new scope is already honest.
  const freshSession = useMemo(() => newSession(guideCompletedKey), [guideCompletedKey]);
  const session = sessionState.key === guideCompletedKey ? sessionState : freshSession;
  const guideCompleted = guideState.key === guideCompletedKey
    ? guideState.value
    : storedTrue(guideCompletedKey);
  const checklistDismissed = checklistState.key === checklistDismissedKey
    ? checklistState.value
    : storedTrue(checklistDismissedKey);
  const accountEvents = accountEventState.key === accountFlagsKey
    ? accountEventState.events
    : storedEvents(accountFlagsKey);
  const workspaceEvents = workspaceEventState.key === workspaceFlagsKey
    ? workspaceEventState.events
    : storedEvents(workspaceFlagsKey);
  accountEventsRef.current = { key: accountFlagsKey, events: accountEvents };
  workspaceEventsRef.current = { key: workspaceFlagsKey, events: workspaceEvents };
  const recordedEvents = useMemo(
    () => new Set([...accountEvents, ...workspaceEvents]),
    [accountEvents, workspaceEvents],
  );

  useEffect(() => {
    setSessionState(newSession(guideCompletedKey));
    setGuideState({ key: guideCompletedKey, value: storedTrue(guideCompletedKey) });
  }, [guideCompletedKey]);

  useEffect(() => {
    setChecklistState({ key: checklistDismissedKey, value: storedTrue(checklistDismissedKey) });
  }, [checklistDismissedKey]);

  useEffect(() => {
    setAccountEventState({ key: accountFlagsKey, events: storedEvents(accountFlagsKey) });
  }, [accountFlagsKey]);

  useEffect(() => {
    setWorkspaceEventState({ key: workspaceFlagsKey, events: storedEvents(workspaceFlagsKey) });
  }, [workspaceFlagsKey]);

  const remaining = useMemo(
    () => steps
      .map((candidate, originalIndex) => ({ candidate, originalIndex }))
      .filter(({ candidate }) => !completedIds.has(candidate.id)),
    [completedIds, steps],
  );
  const cursorRemainingIndex = remaining.findIndex(({ originalIndex }) => originalIndex >= session.cursor);
  const fallbackRemainingIndex = cursorRemainingIndex >= 0 ? cursorRemainingIndex : (remaining.length ? 0 : -1);
  const requestedRemainingIndex = activeSpotlightId === undefined
    ? fallbackRemainingIndex
    : remaining.findIndex(({ candidate }) => candidate.id === activeSpotlightId);
  const activeEntry = requestedRemainingIndex >= 0 ? remaining[requestedRemainingIndex] : undefined;
  const spotlight = !session.skipped && !guideCompleted ? activeEntry?.candidate ?? null : null;
  // P1 can expose an active spotlight and active tip simultaneously. The guide
  // wins; the still-triggered tip is not consumed and resurfaces afterwards.
  const active = enabled ? spotlight ?? tip : null;

  const updateSession = useCallback((update: (current: SessionState) => SessionState) => {
    setSessionState((current) => update(current.key === guideCompletedKey ? current : newSession(guideCompletedKey)));
  }, [guideCompletedKey]);

  const record = useCallback((event: string, scope: OnboardingScope) => {
    const key = scope === "account" ? accountFlagsKey : workspaceFlagsKey;
    const eventRef = scope === "account" ? accountEventsRef : workspaceEventsRef;
    const currentEvents = eventRef.current.key === key ? eventRef.current.events : storedEvents(key);
    if (currentEvents.has(event)) return;
    const events = new Set(currentEvents);
    events.add(event);
    const nextState = { key, events };
    eventRef.current = nextState;
    persistEvents(key, events);
    if (scope === "account") {
      setAccountEventState(nextState);
    } else {
      setWorkspaceEventState(nextState);
    }
    onRecord?.(event, scope);
  }, [accountFlagsKey, onRecord, workspaceFlagsKey]);

  const acknowledge = useCallback((node: Pick<OnboardingStep, "id" | "version" | "scope">) => {
    record(`seen:${node.id}@v${node.version}`, node.scope);
  }, [record]);

  const next = useCallback(() => {
    if (spotlight) acknowledge(spotlight);
    if (requestedRemainingIndex >= 0 && requestedRemainingIndex < remaining.length - 1) {
      const nextCursor = remaining[requestedRemainingIndex + 1].originalIndex;
      updateSession((current) => ({ ...current, cursor: nextCursor }));
      return;
    }
    persistTrue(guideCompletedKey);
    setGuideState({ key: guideCompletedKey, value: true });
  }, [acknowledge, guideCompletedKey, remaining, requestedRemainingIndex, spotlight, updateSession]);

  const back = useCallback(() => {
    const previousCursor = requestedRemainingIndex > 0
      ? remaining[requestedRemainingIndex - 1].originalIndex
      : 0;
    updateSession((current) => ({ ...current, cursor: previousCursor }));
  }, [remaining, requestedRemainingIndex, updateSession]);

  // Guide Skip is session-only and resumable. A contextual tip's Skip/Escape is
  // a durable acknowledgement in that node's own account/workspace scope.
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
    stepIndex: Math.max(0, requestedRemainingIndex),
    total: remaining.length,
    next,
    back,
    skip,
    resume,
    record,
    acknowledge,
    recordedEvents,
    guideCompleted,
    checklistDismissed,
    dismissChecklist,
  };
}
