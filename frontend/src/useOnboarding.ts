import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
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
  // #90: per account×workspace browser-local flag suppressing future auto-opens of the
  // teammate chapter (distinct from checklistDismissed; set by "Not now"/Close on an
  // AUTO-opened chapter — never a manual open).
  teammatePromoDismissedKey?: string;
  activeSpotlightId?: string | null;
  tip?: OnboardingStep | null;
  // The current "Add an AI teammate" chapter card for this role + live state (the engine's
  // activeChapter, already resolved to the true next step / done card / null), plus its
  // position for the step dots. The hook only owns whether the chapter is OPEN.
  chapter?: OnboardingStep | null;
  chapterStepIndex?: number; // 0-based index of the active step among the chapter's steps
  chapterTotal?: number; // number of steps in the chapter for this role
  // #90 auto-open (promotion) inputs. `promotable` = owner/admin + activation-incomplete
  // (host-computed). `documentCount` + `documentOpen` drive the fresh (0→1 + navigated in)
  // vs catch-up (doc already present) discrimination — fresh shows the bridge line.
  promotable?: boolean;
  documentCount?: number;
  documentOpen?: boolean;
  enabled?: boolean;
};

type SessionState = {
  key: string;
  // An original step index the user Backed to (session-only presentation), or null =
  // track the engine frontier. Never persisted; durable completion is never moved by it.
  revisit: number | null;
  skipped: boolean;
  // Whether the "Add an AI teammate" chapter is currently open. Session-only: opened via a
  // checklist click OR the #90 auto-open promotion; "Not now"/Close closes it; reopening
  // resumes from live state (the engine picks the true next card). Never persisted, never
  // moves completion — completion is live-derived (env + agent).
  chapterOpen: boolean;
  // Was the CURRENT open an auto-open (vs a manual checklist open)? Closing an auto-opened
  // chapter records the promo-dismiss flag; a manual open never does.
  chapterAuto: boolean;
  // Is the current auto-open a FRESH one (first-document 0→1)? Fresh opens show the bridge
  // line on their entry card; catch-up and manual opens show normal copy.
  chapterFresh: boolean;
  // The card id a fresh auto-open landed on — the bridge line shows ONLY on that entry card
  // (once the user advances past it, the bridge is gone).
  chapterFreshEntryId: string | null;
  // Fire-once guard: the auto-open promotion fires at most once per session (even after it
  // is closed/dismissed). Reset on a scope switch (new workspace gets its own one-time try).
  autoOpenFired: boolean;
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
  return {
    key,
    revisit: null,
    skipped: false,
    chapterOpen: false,
    chapterAuto: false,
    chapterFresh: false,
    chapterFreshEntryId: null,
    autoOpenFired: false,
  };
}

export function useOnboarding({
  steps,
  completedIds,
  events,
  record,
  scopeKey,
  checklistDismissedKey,
  teammatePromoDismissedKey = "",
  activeSpotlightId,
  tip = null,
  chapter = null,
  chapterStepIndex = 0,
  chapterTotal = 0,
  promotable = false,
  documentCount = 0,
  documentOpen = false,
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
  // The #90 promo-dismiss flag — same durable, browser-local, account×workspace keying as
  // checklistDismissed, but a distinct key and meaning (suppress auto-open only).
  const [promoState, setPromoState] = useState<{ key: string; value: boolean }>(() => ({
    key: teammatePromoDismissedKey,
    value: storedTrue(teammatePromoDismissedKey),
  }));

  // WorkspaceApp keeps this hook mounted across account/workspace switches. Key the
  // session and the dismissed flag so the first render in the new scope is already honest.
  const freshSession = useMemo(() => newSession(scopeKey), [scopeKey]);
  const session = sessionState.key === scopeKey ? sessionState : freshSession;
  const checklistDismissed = checklistState.key === checklistDismissedKey
    ? checklistState.value
    : storedTrue(checklistDismissedKey);
  const promoDismissed = promoState.key === teammatePromoDismissedKey
    ? promoState.value
    : storedTrue(teammatePromoDismissedKey);

  useEffect(() => {
    setSessionState(newSession(scopeKey));
  }, [scopeKey]);
  useEffect(() => {
    setChecklistState({ key: checklistDismissedKey, value: storedTrue(checklistDismissedKey) });
  }, [checklistDismissedKey]);
  useEffect(() => {
    setPromoState({ key: teammatePromoDismissedKey, value: storedTrue(teammatePromoDismissedKey) });
  }, [teammatePromoDismissedKey]);

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

  // Opening the chapter from the checklist is a MANUAL open: normal copy (no bridge). A
  // deliberate open means the user has already seen the chapter, so it ALSO persists the
  // promo-dismiss flag (#90 bug 3) — a reload must not re-auto-open a chapter they opened
  // themselves. Manual reopen via the checklist stays available (dismissal suppresses
  // AUTO-open only). It also trips the session fire-once guard for the current session.
  const openChapter = useCallback(() => {
    if (teammatePromoDismissedKey) {
      persistTrue(teammatePromoDismissedKey);
      setPromoState({ key: teammatePromoDismissedKey, value: true });
    }
    updateSession((current) => ({
      ...current,
      chapterOpen: true,
      chapterAuto: false,
      chapterFresh: false,
      chapterFreshEntryId: null,
      autoOpenFired: true,
    }));
  }, [teammatePromoDismissedKey, updateSession]);
  // "Not now"/Close closes without recording any completion (Anton: dismiss only) —
  // completion is live-derived, so reopening resumes from the true next card. If the chapter
  // was AUTO-opened, closing also records the promo-dismiss flag (permanently suppress future
  // auto-opens in this workspace/profile); a manual open never does.
  const closeChapter = useCallback(() => {
    if (session.chapterAuto && teammatePromoDismissedKey) {
      persistTrue(teammatePromoDismissedKey);
      setPromoState({ key: teammatePromoDismissedKey, value: true });
    }
    updateSession((current) => ({
      ...current,
      chapterOpen: false,
      chapterAuto: false,
      chapterFresh: false,
      chapterFreshEntryId: null,
    }));
  }, [session.chapterAuto, teammatePromoDismissedKey, updateSession]);

  // #90 auto-open (promotion). Owner/admin only (promotable is false for members and once
  // activation is complete). Fires ONCE per session when a document exists and either a
  // fresh 0→1 transition + navigated-into-doc (FRESH → bridge) or the doc was already
  // present at first eligible load (CATCH-UP → normal copy). The engine picks WHICH card
  // (activeChapter) — this only flips the session open. `promoRef` tracks the previous
  // document count so we can tell fresh from catch-up; it resets on a scope switch.
  const promoRef = useRef<{ key: string; prevDocCount: number | null; sawFresh: boolean }>({
    key: scopeKey,
    prevDocCount: null,
    sawFresh: false,
  });
  // A promotion is "pending" this frame when it is eligible AND a discussion spotlight could
  // otherwise paint (documentOpen). The App suppresses the spotlight synchronously on this,
  // and the layout effect below flips chapterOpen before paint — together the locked order
  // (document → teammate chapter → discussion) never shows a one-frame discussion flash
  // (#90 bug 2). No ref read here: when documentOpen, both fresh and catch-up fire this frame.
  const promotionPending =
    enabled
    && promotable
    && !promoDismissed
    && !session.chapterOpen
    && !session.autoOpenFired
    && documentCount >= 1
    && documentOpen;
  // useLayoutEffect (not useEffect): React flushes it synchronously before the browser paints,
  // so the chapter takes the screen in the same visual frame the document opens — the discussion
  // spotlight never becomes visible in between.
  useLayoutEffect(() => {
    if (promoRef.current.key !== scopeKey) {
      promoRef.current = { key: scopeKey, prevDocCount: null, sawFresh: false };
    }
    // Only track document-count transitions once onboarding is actually enabled (namespace/
    // workspace ready). A 0→1 seen during disabled startup is the app loading an EXISTING
    // workspace, NOT a user creating a fresh document — counting it would mislabel catch-up as
    // fresh and show the bridge title on an existing workspace (#90 bug 1).
    if (!enabled) return;
    const ref = promoRef.current;
    const prev = ref.prevDocCount;
    ref.prevDocCount = documentCount;
    if (prev === 0 && documentCount > 0) ref.sawFresh = true;

    if (!promotable || promoDismissed) return;
    if (session.chapterOpen || session.autoOpenFired) return;
    if (documentCount < 1) return;
    const fresh = ref.sawFresh;
    // A fresh open waits until the user has actually navigated into the new document;
    // catch-up (doc already there) opens on the first eligible load regardless of route.
    if (fresh && !documentOpen) return;
    const entryId = chapter?.id ?? null;
    updateSession((current) => ({
      ...current,
      chapterOpen: true,
      chapterAuto: true,
      chapterFresh: fresh,
      chapterFreshEntryId: fresh ? entryId : null,
      autoOpenFired: true,
    }));
  }, [
    enabled,
    promotable,
    promoDismissed,
    session.chapterOpen,
    session.autoOpenFired,
    documentCount,
    documentOpen,
    chapter,
    scopeKey,
    updateSession,
  ]);

  // If the chapter is open but live state leaves no card, close it — never linger half-open
  // with the tip/checklist suspended behind nothing (Deniz's live-transition edge).
  // Session-only, records no flag. (Owner/admin always have a card, so this is a safety net.)
  useEffect(() => {
    if (enabled && session.chapterOpen && !chapter) {
      updateSession((current) => ({
        ...current,
        chapterOpen: false,
        chapterAuto: false,
        chapterFresh: false,
        chapterFreshEntryId: null,
      }));
    }
  }, [enabled, session.chapterOpen, chapter, updateSession]);

  // The chapter card to render right now: only when enabled, opened this session, and the
  // engine has a card for this role+state.
  const chapterActive = enabled && session.chapterOpen ? chapter ?? null : null;
  // The bridge line shows ONLY on a fresh auto-open, and ONLY while the user is still on the
  // entry card it landed on (catch-up + manual + any later card → normal copy).
  const chapterBridge = Boolean(
    chapterActive && session.chapterFresh && chapterActive.id === session.chapterFreshEntryId,
  );

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
    // Chapter ("Add an AI teammate"): the card to render when open, its step position for
    // the dots, whether it's open, whether to show the promotion bridge line, and the
    // open/close handlers (checklist entry → open; auto-open sets it internally).
    chapterActive,
    chapterStepIndex,
    chapterTotal,
    chapterOpen: session.chapterOpen,
    chapterBridge,
    // True on the frame a promotion is about to auto-open (before the layout effect flips
    // chapterOpen) — the host uses it to keep the discussion spotlight from flashing.
    promotionPending,
    promoDismissed,
    openChapter,
    closeChapter,
  };
}
