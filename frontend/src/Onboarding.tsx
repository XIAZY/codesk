import { useCallback, useEffect, useLayoutEffect, useRef, useState, type CSSProperties } from "react";

// Kept structurally in sync with onboardingEngine.ts's unions (the adapter projects
// OnboardingNode → OnboardingStep). The three open-* events route the "Add an AI
// teammate" chapter CTAs; "chapter" is that optional flow's presentation.
export type OnboardingActionEvent =
  | "advance"
  | "back"
  | "complete"
  | "dismiss"
  | "open-thread-draft"
  | "open-create-environment"
  | "open-create-agent"
  | "open-agent-work";
export type OnboardingScope = "account" | "workspace";
export type OnboardingPresentation = "spotlight" | "tip" | "chapter";

export type OnboardingAction = {
  label: string;
  event: OnboardingActionEvent;
};

// Structural view model for the overlay. The Phase-2 adapter maps P1's richer
// OnboardingNode into this shape without coupling the parallel branches.
export type OnboardingStep = {
  id: string;
  version: number;
  scope: OnboardingScope;
  presentation: OnboardingPresentation;
  targetOnboardingId?: string;
  eyebrow?: string; // small label above the title (chapter cards)
  title: string;
  body: string;
  caption?: string; // reassurance line, not an action (chapter member card)
  primaryAction?: OnboardingAction;
  secondaryAction?: OnboardingAction;
  skippable: boolean;
  fallback: "page-card" | "skip";
};

type Rect = {
  left: number;
  top: number;
  right: number;
  bottom: number;
  width: number;
  height: number;
};

type PanelRect = { left: number; top: number; width: number; height: number };

export type SpotlightGeometry = {
  hole: Rect;
  panels: [PanelRect, PanelRect, PanelRect, PanelRect];
  placement: { left: number; top: number };
};

const SPOTLIGHT_PAD = 12;
const COACH_MARGIN = 12;
const COACH_RIGHT = 24;
const COACH_TOP = 72;
const COACH_TARGET_GAP = 18;
const COACH_FALLBACK = { width: 312, height: 220 };

function bounded(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

export function spotlightGeometry(
  target: Rect,
  viewport: { width: number; height: number },
  coach: { width: number; height: number },
): SpotlightGeometry {
  const left = bounded(target.left - SPOTLIGHT_PAD, 0, viewport.width);
  const top = bounded(target.top - SPOTLIGHT_PAD, 0, viewport.height);
  const right = bounded(target.right + SPOTLIGHT_PAD, left, viewport.width);
  const bottom = bounded(target.bottom + SPOTLIGHT_PAD, top, viewport.height);
  const hole = { left, top, right, bottom, width: right - left, height: bottom - top };
  const panels: SpotlightGeometry["panels"] = [
    { left: 0, top: 0, width: viewport.width, height: top },
    { left: 0, top, width: left, height: hole.height },
    { left: right, top, width: Math.max(0, viewport.width - right), height: hole.height },
    { left: 0, top: bottom, width: viewport.width, height: Math.max(0, viewport.height - bottom) },
  ];

  const maxLeft = Math.max(COACH_MARGIN, viewport.width - coach.width - COACH_MARGIN);
  const maxTop = Math.max(COACH_MARGIN, viewport.height - coach.height - COACH_MARGIN);
  const coachLeft = bounded(viewport.width - coach.width - COACH_RIGHT, COACH_MARGIN, maxLeft);
  let coachTop = bounded(COACH_TOP, COACH_MARGIN, maxTop);
  const crowdsTarget = (candidateTop: number) => (
    coachLeft < target.right + COACH_TARGET_GAP
    && coachLeft + coach.width > target.left - COACH_TARGET_GAP
    && candidateTop < target.bottom + COACH_TARGET_GAP
    && candidateTop + coach.height > target.top - COACH_TARGET_GAP
  );
  if (crowdsTarget(coachTop)) {
    const below = hole.bottom + COACH_TARGET_GAP;
    const above = hole.top - coach.height - COACH_TARGET_GAP;
    if (below <= maxTop) coachTop = below;
    else if (above >= COACH_MARGIN) coachTop = above;
  }
  return { hole, panels, placement: { left: coachLeft, top: coachTop } };
}

export function contextualTipPlacement(
  target: Rect,
  viewport: { width: number; height: number },
  coach: { width: number; height: number },
): { left: number; top: number } {
  const maxLeft = Math.max(COACH_MARGIN, viewport.width - coach.width - COACH_MARGIN);
  const maxTop = Math.max(COACH_MARGIN, viewport.height - coach.height - COACH_MARGIN);
  const left = bounded(
    target.left + target.width / 2 - coach.width / 2,
    COACH_MARGIN,
    maxLeft,
  );
  const below = target.bottom + COACH_MARGIN;
  const above = target.top - coach.height - COACH_MARGIN;
  const top = below <= maxTop
    ? below
    : above >= COACH_MARGIN
      ? above
      : bounded(below, COACH_MARGIN, maxTop);
  return { left, top };
}

function targetFor(onboardingId: string | undefined): HTMLElement | null {
  if (!onboardingId) return null;
  return Array.from(document.querySelectorAll<HTMLElement>("[data-onboarding-id]"))
    .find((candidate) => candidate.dataset.onboardingId === onboardingId) ?? null;
}

function viewportSize() {
  return {
    width: window.visualViewport?.width ?? window.innerWidth,
    height: window.visualViewport?.height ?? window.innerHeight,
  };
}

function focusable(node: HTMLElement | null): node is HTMLElement {
  if (!node) return false;
  if (node.matches("button:disabled, input:disabled, select:disabled, textarea:disabled")) return false;
  return node.matches("button, a[href], input, select, textarea, [tabindex]:not([tabindex='-1'])");
}

type OnboardingProps = {
  step: OnboardingStep;
  stepIndex: number;
  total: number;
  onNext: () => void;
  onBack: () => void;
  onSkip: () => void;
  onAction?: (event: OnboardingActionEvent) => void;
  onMissingTarget?: () => void;
};

export function Onboarding({
  step,
  stepIndex,
  total,
  onNext,
  onBack,
  onSkip,
  onAction,
  onMissingTarget,
}: OnboardingProps) {
  const isTip = step.presentation === "tip";
  const coachRef = useRef<HTMLElement | null>(null);
  const missingNotifiedRef = useRef("");
  const [target, setTarget] = useState<HTMLElement | null>(null);
  const [targetResolution, setTargetResolution] = useState({ stepId: step.id, missing: false });
  const [geometry, setGeometry] = useState<SpotlightGeometry | null>(null);
  const missing = targetResolution.stepId === step.id && targetResolution.missing;

  const measure = useCallback((node: HTMLElement) => {
    if (!node.isConnected) {
      setTarget(null);
      setGeometry(null);
      setTargetResolution({ stepId: step.id, missing: true });
      return;
    }
    const targetRect = node.getBoundingClientRect();
    const coachRect = coachRef.current?.getBoundingClientRect();
    const coachSize = coachRect?.width && coachRect?.height
      ? { width: coachRect.width, height: coachRect.height }
      : COACH_FALLBACK;
    const viewport = viewportSize();
    const nextGeometry = spotlightGeometry(targetRect, viewport, coachSize);
    setGeometry(isTip
      ? { ...nextGeometry, placement: contextualTipPlacement(targetRect, viewport, coachSize) }
      : nextGeometry);
  }, [isTip, step.id]);

  useLayoutEffect(() => {
    const node = targetFor(step.targetOnboardingId);
    setTarget(node);
    setTargetResolution({ stepId: step.id, missing: !node });
    setGeometry(null);
    if (!node) return;

    let frame = 0;
    const schedule = () => {
      if (frame) return;
      frame = window.requestAnimationFrame(() => {
        frame = 0;
        measure(node);
      });
    };
    measure(node);
    schedule();
    window.addEventListener("resize", schedule);
    window.visualViewport?.addEventListener("resize", schedule);
    window.visualViewport?.addEventListener("scroll", schedule);
    document.addEventListener("scroll", schedule, true);
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(schedule);
    observer?.observe(node);
    return () => {
      if (frame) window.cancelAnimationFrame(frame);
      observer?.disconnect();
      window.removeEventListener("resize", schedule);
      window.visualViewport?.removeEventListener("resize", schedule);
      window.visualViewport?.removeEventListener("scroll", schedule);
      document.removeEventListener("scroll", schedule, true);
    };
  }, [measure, step.id, step.targetOnboardingId]);

  useEffect(() => {
    if (!missing || step.fallback !== "skip" || missingNotifiedRef.current === step.id) return;
    missingNotifiedRef.current = step.id;
    console.warn(`[onboarding] skipped ${step.id}: target ${step.targetOnboardingId ?? "(none)"} is missing`);
    // Presentation-correct dispatch: a tip's durable completion lives in onSkip (it
    // acknowledges the account node), so a missing tip target must NOT call onNext —
    // onNext drives the guide machinery and could advance/complete the whole guide off
    // an absent anchor. Spotlight steps still skip forward via onNext.
    (onMissingTarget ?? (isTip ? onSkip : onNext))();
  }, [isTip, missing, onMissingTarget, onNext, onSkip, step.fallback, step.id, step.targetOnboardingId]);

  useEffect(() => {
    if ((!target || !geometry) && !missing) return;
    const coach = coachRef.current;
    const modalOpen = () => Boolean(document.querySelector(".modal-backdrop"));

    if (isTip) {
      const onKeyDown = (event: KeyboardEvent) => {
        if (modalOpen() || event.key !== "Escape" || !step.skippable) return;
        event.preventDefault();
        onSkip();
      };
      document.addEventListener("keydown", onKeyDown);
      return () => document.removeEventListener("keydown", onKeyDown);
    }

    const coachNodes = () => Array.from(coach?.querySelectorAll<HTMLElement>(
      "button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex='-1'])",
    ) ?? []);
    const cycle = () => [...(focusable(target) ? [target] : []), ...coachNodes()];

    const onFocusIn = (event: FocusEvent) => {
      if (modalOpen() || !(event.target instanceof Node)) return;
      if (target?.contains(event.target) || coach?.contains(event.target)) return;
      cycle()[0]?.focus();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (modalOpen()) return;
      if (event.key === "Escape") {
        if (step.skippable) {
          event.preventDefault();
          onSkip();
        }
        return;
      }
      if (event.key === "ArrowLeft" && stepIndex > 0) {
        event.preventDefault();
        onBack();
        return;
      }
      if (event.key === "ArrowRight") {
        event.preventDefault();
        onNext();
        return;
      }
      if (event.key === "Enter" && (document.activeElement === document.body || document.activeElement === coach)) {
        event.preventDefault();
        onNext();
        return;
      }
      if (event.key !== "Tab") return;
      const nodes = cycle();
      if (!nodes.length) return;
      const current = nodes.indexOf(document.activeElement as HTMLElement);
      const nextIndex = event.shiftKey
        ? (current <= 0 ? nodes.length - 1 : current - 1)
        : (current < 0 || current === nodes.length - 1 ? 0 : current + 1);
      event.preventDefault();
      nodes[nextIndex]?.focus();
    };
    document.addEventListener("focusin", onFocusIn);
    document.addEventListener("keydown", onKeyDown);
    const initialFocusTimer = window.setTimeout(() => {
      if (modalOpen()) return;
      const nodes = coachNodes();
      nodes[nodes.length - 1]?.focus();
    }, 0);
    return () => {
      window.clearTimeout(initialFocusTimer);
      document.removeEventListener("focusin", onFocusIn);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [geometry, isTip, missing, onBack, onNext, onSkip, step.skippable, stepIndex, target]);

  const takeAction = (action: OnboardingAction | undefined, fallback?: () => void) => {
    if (action) onAction?.(action.event);
    fallback?.();
  };

  if (missing && step.fallback === "skip") return null;
  if (!missing && (!target || !geometry)) return null;

  const coach = (
    <section
      ref={coachRef}
      className={`ob-coach${missing ? " ob-page-fallback" : ""}${isTip ? " ob-tip" : ""}`}
      style={missing ? undefined : {
        left: geometry?.placement.left,
        top: geometry?.placement.top,
      } as CSSProperties}
      role="dialog"
      aria-modal="false"
      aria-labelledby={`ob-title-${step.id}`}
      aria-describedby={`ob-body-${step.id}`}
    >
      {isTip ? null : (
        <div className="ob-coach-meta">
          <span className="ob-gmark" aria-hidden="true" />
          <span className="ob-step">Step {stepIndex + 1} of {Math.max(total, 1)}</span>
        </div>
      )}
      <h4 id={`ob-title-${step.id}`}>{step.title}</h4>
      <p id={`ob-body-${step.id}`}>{step.body}</p>
      {isTip ? null : (
        <div className="ob-dots" aria-hidden="true">
          {Array.from({ length: Math.max(total, 1) }, (_, index) => <i className={index === stepIndex ? "on" : ""} key={index} />)}
        </div>
      )}
      <div className="ob-actions">
        {!isTip && step.skippable ? <button type="button" className="ob-skip" onClick={onSkip}>Skip</button> : <span />}
        <div className="ob-actions-main">
          {isTip || step.secondaryAction?.event === "back"
            ? null
            : <button type="button" className="ob-back" onClick={onBack} disabled={stepIndex === 0}>Back</button>}
          {step.secondaryAction ? (
            <button
              type="button"
              className="ob-secondary"
              onClick={() => takeAction(
                step.secondaryAction,
                step.secondaryAction?.event === "back" ? onBack : step.secondaryAction?.event === "dismiss" ? onSkip : onNext,
              )}
            >
              {step.secondaryAction.label}
            </button>
          ) : null}
          <button type="button" className="ob-next" onClick={() => takeAction(step.primaryAction, isTip ? undefined : onNext)}>
            {step.primaryAction?.label ?? "Next"}
          </button>
        </div>
      </div>
      <span className="ob-live" aria-live="polite">{step.title}. {step.body}</span>
    </section>
  );

  if (missing || isTip) return coach;

  return (
    <div className="ob-layer" data-onboarding-step={step.id}>
      {geometry!.panels.map((panel, index) => (
        <div
          className="ob-scrim-panel"
          onClick={step.skippable ? onSkip : undefined}
          key={index}
          style={panel as CSSProperties}
        />
      ))}
      <div
        className="ob-window"
        aria-hidden="true"
        style={{
          left: geometry!.hole.left,
          top: geometry!.hole.top,
          width: geometry!.hole.width,
          height: geometry!.hole.height,
        }}
      />
      {coach}
    </div>
  );
}

type ChapterCardProps = {
  step: OnboardingStep;
  stepIndex: number; // among the chapter's steps; -1 for the terminal done card
  total: number; // number of chapter steps for this role (owner/admin 3, member 1)
  onAction?: (event: OnboardingActionEvent) => void;
  onDismiss: () => void; // "Not now" / "Close" — dismiss only, never completes
};

// The opt-in "Add an AI teammate" chapter card — a quiet page card, NEVER a spotlight.
// One real CTA (connect / create / start-a-run), a "Not now" that only dismisses, and
// (for the owner/admin path) step dots. Kept separate from <Onboarding> so it doesn't
// inherit the spotlight geometry + Enter-advances keyboard model — the chapter advances
// from live state, not from Next.
export function OnboardingChapterCard({ step, stepIndex, total, onAction, onDismiss }: ChapterCardProps) {
  const cardRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    cardRef.current?.querySelector<HTMLElement>("button")?.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onDismiss();
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [onDismiss]);

  const primary = step.primaryAction;
  // A primary "Close" (done card) has event "dismiss" — it isn't a real CTA button; it
  // becomes the footer dismiss control. Only open-* primaries render the dark CTA.
  const hasCta = primary != null && primary.event !== "dismiss";
  const dismissAction = step.secondaryAction ?? (primary?.event === "dismiss" ? primary : undefined);
  const showSteps = total > 1 && stepIndex >= 0; // owner/admin 3-step path only (not member/done)

  return (
    <section
      ref={cardRef}
      className="ob-coach ob-chapter"
      role="dialog"
      aria-modal="false"
      aria-labelledby={`ob-title-${step.id}`}
      aria-describedby={`ob-body-${step.id}`}
    >
      {step.eyebrow ? <span className="ob-eyebrow">{step.eyebrow}</span> : null}
      <h4 id={`ob-title-${step.id}`}>{step.title}</h4>
      <p id={`ob-body-${step.id}`}>{step.body}</p>
      {hasCta ? (
        <button type="button" className="ob-cta" onClick={() => onAction?.(primary!.event)}>
          {primary!.label}
        </button>
      ) : null}
      {hasCta && step.caption ? <span className="ob-caption">{step.caption}</span> : null}
      <div className="ob-chapter-foot">
        {showSteps ? (
          <span className="ob-chapter-progress">
            <span className="ob-dots" aria-hidden="true">
              {Array.from({ length: total }, (_, index) => (
                <i className={index === stepIndex ? "on" : ""} key={index} />
              ))}
            </span>
            <span className="ob-step">Step {stepIndex + 1} of {total}</span>
          </span>
        ) : !hasCta && step.caption ? (
          <span className="ob-chapter-status">{step.caption}</span>
        ) : (
          <span />
        )}
        {dismissAction ? (
          <button type="button" className="ob-notnow" onClick={onDismiss}>
            {dismissAction.label}
          </button>
        ) : null}
      </div>
      <span className="ob-live" aria-live="polite">{step.title}. {step.body}</span>
    </section>
  );
}
