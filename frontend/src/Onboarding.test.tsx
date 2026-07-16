// @vitest-environment jsdom

import { useState } from "react";
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Onboarding, OnboardingChapterCard, contextualTipPlacement, spotlightGeometry, type OnboardingStep } from "./Onboarding";
import { useOnboarding } from "./useOnboarding";

const step: OnboardingStep = {
  id: "create-first-document",
  version: 1,
  scope: "workspace",
  presentation: "spotlight",
  targetOnboardingId: "create-document",
  title: "Create your first document",
  body: "It saves as you type.",
  primaryAction: { label: "Next", event: "advance" },
  skippable: true,
  fallback: "page-card",
};

const tip: OnboardingStep = {
  ...step,
  id: "tip-first-selection",
  scope: "account",
  presentation: "tip",
  targetOnboardingId: "selection-thread",
  title: "Talk about this exact line",
  primaryAction: { label: "Start thread", event: "open-thread-draft" },
  secondaryAction: { label: "Got it", event: "dismiss" },
};

class ResizeObserverMock {
  static callback: ResizeObserverCallback | null = null;
  observe = vi.fn();
  disconnect = vi.fn();

  constructor(callback: ResizeObserverCallback) {
    ResizeObserverMock.callback = callback;
  }
}

let animationFrames: FrameRequestCallback[] = [];

function flushAnimationFrames() {
  const callbacks = animationFrames;
  animationFrames = [];
  callbacks.forEach((callback) => callback(0));
}

beforeEach(() => {
  animationFrames = [];
  ResizeObserverMock.callback = null;
  vi.stubGlobal("ResizeObserver", ResizeObserverMock);
  vi.spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    animationFrames.push(callback);
    return animationFrames.length;
  });
  vi.spyOn(window, "cancelAnimationFrame").mockImplementation(() => undefined);
});

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  cleanup();
});

describe("spotlightGeometry", () => {
  it("forms four panels around a padded true hole and places the coach at upper right", () => {
    const geometry = spotlightGeometry(
      { left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 },
      { width: 1000, height: 800 },
      { width: 312, height: 180 },
    );

    expect(geometry.hole).toEqual({ left: 88, top: 68, right: 212, bottom: 132, width: 124, height: 64 });
    expect(geometry.panels).toEqual([
      { left: 0, top: 0, width: 1000, height: 68 },
      { left: 0, top: 68, width: 88, height: 64 },
      { left: 212, top: 68, width: 788, height: 64 },
      { left: 0, top: 132, width: 1000, height: 668 },
    ]);
    expect(geometry.placement).toEqual({ left: 664, top: 72 });
  });

  it("clamps a target near the viewport edge without negative panel dimensions", () => {
    const geometry = spotlightGeometry(
      { left: -8, top: 760, right: 40, bottom: 820, width: 48, height: 60 },
      { width: 360, height: 800 },
      { width: 312, height: 220 },
    );

    expect(geometry.hole.left).toBe(0);
    expect(geometry.hole.bottom).toBe(800);
    expect(geometry.panels.every((panel) => panel.width >= 0 && panel.height >= 0)).toBe(true);
    expect(geometry.placement.left).toBeGreaterThanOrEqual(12);
    expect(geometry.placement.top).toBeGreaterThanOrEqual(12);
  });

  it("keeps the coach at upper right when a wide centered target only nears its padded spotlight", () => {
    const geometry = spotlightGeometry(
      { left: 460, top: 250, right: 1080, bottom: 540, width: 620, height: 290 },
      { width: 1440, height: 900 },
      { width: 312, height: 220 },
    );

    expect(geometry.placement).toEqual({ left: 1104, top: 72 });
  });

  it("moves below a top-right target cluster instead of covering it", () => {
    const geometry = spotlightGeometry(
      { left: 1080, top: 20, right: 1240, bottom: 56, width: 160, height: 36 },
      { width: 1280, height: 800 },
      { width: 312, height: 220 },
    );

    expect(geometry.placement.left).toBe(944);
    expect(geometry.placement.top).toBe(86);
    expect(geometry.placement.top).toBeGreaterThanOrEqual(geometry.hole.bottom + 18);
  });
});

describe("contextualTipPlacement", () => {
  it("attaches below the target instead of using the guided-step upper-right clamp", () => {
    expect(contextualTipPlacement(
      { left: 420, top: 280, right: 580, bottom: 320, width: 160, height: 40 },
      { width: 1200, height: 800 },
      { width: 312, height: 180 },
    )).toEqual({ left: 344, top: 332 });
  });

  it("flips above the target and clamps horizontally at viewport edges", () => {
    expect(contextualTipPlacement(
      { left: 310, top: 700, right: 370, bottom: 740, width: 60, height: 40 },
      { width: 380, height: 760 },
      { width: 312, height: 180 },
    )).toEqual({ left: 56, top: 508 });
  });
});

describe("Onboarding", () => {
  it("leaves the real target clickable through four scrim panels", async () => {
    const onTarget = vi.fn();
    const targetRect = { left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 };
    const { container } = render(
      <>
        <button
          data-onboarding-id="create-document"
          onClick={onTarget}
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => targetRect as DOMRect;
          }}
        >
          New document
        </button>
        <Onboarding step={step} stepIndex={0} total={2} onNext={vi.fn()} onBack={vi.fn()} onSkip={vi.fn()} />
      </>,
    );

    await waitFor(() => expect(container.querySelectorAll(".ob-scrim-panel")).toHaveLength(4));
    await userEvent.click(screen.getByRole("button", { name: "New document" }));
    expect(onTarget).toHaveBeenCalledOnce();
    expect(container.querySelector(".ob-window")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Create your first document" })).toBeTruthy();
  });

  it("rAF-throttles scroll measurement and observes target resizes", async () => {
    let targetRect = { left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 };
    const { container } = render(
      <>
        <button
          data-onboarding-id="create-document"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => targetRect as DOMRect;
          }}
        >
          New document
        </button>
        <Onboarding step={step} stepIndex={0} total={1} onNext={vi.fn()} onBack={vi.fn()} onSkip={vi.fn()} />
      </>,
    );
    await waitFor(() => expect(container.querySelector<HTMLElement>(".ob-window")?.style.left).toBe("88px"));
    expect(ResizeObserverMock.callback).toBeTypeOf("function");
    act(flushAnimationFrames);

    targetRect = { left: 200, top: 180, right: 300, bottom: 220, width: 100, height: 40 };
    fireEvent.scroll(document);
    fireEvent.scroll(document);
    expect(animationFrames).toHaveLength(1);
    act(flushAnimationFrames);
    expect(container.querySelector<HTMLElement>(".ob-window")?.style.left).toBe("188px");

    targetRect = { left: 240, top: 180, right: 340, bottom: 220, width: 100, height: 40 };
    act(() => ResizeObserverMock.callback?.([], {} as ResizeObserver));
    expect(animationFrames).toHaveLength(1);
    act(flushAnimationFrames);
    expect(container.querySelector<HTMLElement>(".ob-window")?.style.left).toBe("228px");
  });

  it("forwards the action union before advancing", async () => {
    const onAction = vi.fn();
    const onNext = vi.fn();
    const onBack = vi.fn();
    render(
      <>
        <button
          data-onboarding-id="create-document"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 }) as DOMRect;
          }}
        >
          New document
        </button>
        <Onboarding
          step={{ ...step, secondaryAction: { label: "Back", event: "back" } }}
          stepIndex={1}
          total={2}
          onNext={onNext}
          onBack={onBack}
          onSkip={vi.fn()}
          onAction={onAction}
        />
      </>,
    );

    await userEvent.click(await screen.findByRole("button", { name: "Next" }));
    expect(onAction).toHaveBeenCalledWith("advance");
    expect(onNext).toHaveBeenCalledOnce();
    await userEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(onAction).toHaveBeenCalledWith("back");
    expect(onBack).toHaveBeenCalledOnce();
    expect(onNext).toHaveBeenCalledOnce();
  });

  it("renders a contextual tip without guide counter, dots, Back, or a duplicate Skip", async () => {
    const onAction = vi.fn();
    const onNext = vi.fn();
    const onSkip = vi.fn();
    render(
      <>
        <button
          data-onboarding-id="selection-thread"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 420, top: 280, right: 580, bottom: 320, width: 160, height: 40 }) as DOMRect;
          }}
        >
          Start a thread
        </button>
        <Onboarding
          step={tip}
          stepIndex={0}
          total={3}
          onNext={onNext}
          onBack={vi.fn()}
          onSkip={onSkip}
          onAction={onAction}
        />
      </>,
    );

    const dialog = await screen.findByRole("dialog", { name: "Talk about this exact line" });
    expect(dialog.style.left).toBe("344px");
    expect(dialog.style.top).toBe("332px");
    expect(screen.queryByText("Step 1 of 3")).toBeNull();
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Skip" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Start thread" }));
    expect(onAction).toHaveBeenCalledWith("open-thread-draft");
    expect(onNext).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Got it" }));
    expect(onAction).toHaveBeenCalledWith("dismiss");
    expect(onSkip).toHaveBeenCalledOnce();
  });

  it("renders a contextual tip without a blocking layer and leaves its live target clickable", async () => {
    const onTarget = vi.fn();
    const onSkip = vi.fn();
    const { container } = render(
      <>
        <button
          data-onboarding-id="selection-thread"
          onClick={onTarget}
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 420, top: 280, right: 580, bottom: 320, width: 160, height: 40 }) as DOMRect;
          }}
        >
          Start a thread
        </button>
        <Onboarding step={tip} stepIndex={0} total={3} onNext={vi.fn()} onBack={vi.fn()} onSkip={onSkip} />
      </>,
    );

    await screen.findByRole("dialog", { name: "Talk about this exact line" });
    expect(container.querySelectorAll(".ob-scrim-panel")).toHaveLength(0);
    expect(container.querySelector(".ob-window")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Start a thread" }));
    expect(onTarget).toHaveBeenCalledOnce();
    expect(onSkip).not.toHaveBeenCalled();
  });

  it("does not intercept editor arrow keys while a contextual tip is open", async () => {
    const onNext = vi.fn();
    const onSkip = vi.fn();
    render(
      <>
        <button
          data-onboarding-id="selection-thread"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 420, top: 280, right: 580, bottom: 320, width: 160, height: 40 }) as DOMRect;
          }}
        >
          Start a thread
        </button>
        <Onboarding step={tip} stepIndex={0} total={3} onNext={onNext} onBack={vi.fn()} onSkip={onSkip} />
      </>,
    );

    await screen.findByRole("dialog", { name: "Talk about this exact line" });
    const arrowRight = new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true, cancelable: true });
    expect(document.dispatchEvent(arrowRight)).toBe(true);
    expect(arrowRight.defaultPrevented).toBe(false);
    expect(onNext).not.toHaveBeenCalled();
    expect(onSkip).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Talk about this exact line" })).toBeTruthy();
  });

  it("renders a centered page card when the target is missing", async () => {
    const onSkip = vi.fn();
    const { container } = render(
      <Onboarding step={step} stepIndex={0} total={1} onNext={vi.fn()} onBack={vi.fn()} onSkip={onSkip} />,
    );

    await waitFor(() => expect(container.querySelector(".ob-page-fallback")).toBeTruthy());
    expect(container.querySelector(".ob-window")).toBeNull();
    expect(container.querySelectorAll(".ob-scrim-panel")).toHaveLength(0);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Next" })));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onSkip).toHaveBeenCalledOnce();
  });

  it("skips and logs a missing skip-fallback target without rendering a bubble", async () => {
    const onMissingTarget = vi.fn();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const { container } = render(
      <Onboarding
        step={{ ...step, fallback: "skip" }}
        stepIndex={0}
        total={1}
        onNext={vi.fn()}
        onBack={vi.fn()}
        onSkip={vi.fn()}
        onMissingTarget={onMissingTarget}
      />,
    );

    await waitFor(() => expect(onMissingTarget).toHaveBeenCalledOnce());
    expect(warn).toHaveBeenCalled();
    expect(container.querySelector(".ob-coach")).toBeNull();
  });

  it("cycles focus through the live target and callout, handles shortcuts, and yields to a real modal", async () => {
    const user = userEvent.setup();
    const onNext = vi.fn();
    const onBack = vi.fn();
    const onSkip = vi.fn();
    render(
      <>
        <button
          data-onboarding-id="create-document"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 }) as DOMRect;
          }}
        >
          New document
        </button>
        <Onboarding step={step} stepIndex={1} total={3} onNext={onNext} onBack={onBack} onSkip={onSkip} />
      </>,
    );
    await screen.findByRole("dialog", { name: "Create your first document" });

    const target = screen.getByRole("button", { name: "New document" });
    target.focus();
    await user.tab({ shift: true });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Next" }));
    await user.tab();
    expect(document.activeElement).toBe(target);

    fireEvent.keyDown(document, { key: "ArrowLeft" });
    fireEvent.keyDown(document, { key: "ArrowRight" });
    expect(onBack).toHaveBeenCalledOnce();
    expect(onNext).toHaveBeenCalledOnce();

    const modal = document.createElement("div");
    modal.className = "modal-backdrop";
    const modalButton = document.createElement("button");
    modalButton.textContent = "Modal action";
    modal.appendChild(modalButton);
    document.body.appendChild(modal);
    modalButton.focus();
    fireEvent.focusIn(modalButton);
    expect(document.activeElement).toBe(modalButton);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onSkip).not.toHaveBeenCalled();
    modal.remove();

    fireEvent.keyDown(document, { key: "Escape" });
    expect(onSkip).toHaveBeenCalledOnce();
  });

  it("does not let Escape dismiss a non-skippable step", async () => {
    const onSkip = vi.fn();
    const { container } = render(
      <>
        <button
          data-onboarding-id="create-document"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 }) as DOMRect;
          }}
        >
          New document
        </button>
        <Onboarding step={{ ...step, skippable: false }} stepIndex={0} total={1} onNext={vi.fn()} onBack={vi.fn()} onSkip={onSkip} />
      </>,
    );
    await screen.findByRole("dialog", { name: "Create your first document" });

    fireEvent.keyDown(document, { key: "Escape" });
    expect(onSkip).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Skip" })).toBeNull();
    fireEvent.click(container.querySelector(".ob-scrim-panel")!);
    expect(onSkip).not.toHaveBeenCalled();
  });

  it("routes a missing tip target to onSkip (durable acknowledge), never onNext", async () => {
    const onSkip = vi.fn();
    const onNext = vi.fn();
    // No element carries data-onboarding-id="selection-thread" → the tip target is missing.
    render(<Onboarding step={{ ...tip, fallback: "skip" }} stepIndex={0} total={1} onNext={onNext} onBack={vi.fn()} onSkip={onSkip} />);
    await waitFor(() => expect(onSkip).toHaveBeenCalledTimes(1));
    expect(onNext).not.toHaveBeenCalled();
  });

  it("routes a missing spotlight target to onNext (advance the sequence)", async () => {
    const onSkip = vi.fn();
    const onNext = vi.fn();
    render(<Onboarding step={{ ...step, id: "watchers-intro", targetOnboardingId: "document-watchers", fallback: "skip" }} stepIndex={2} total={3} onNext={onNext} onBack={vi.fn()} onSkip={onSkip} />);
    await waitFor(() => expect(onNext).toHaveBeenCalledTimes(1));
    expect(onSkip).not.toHaveBeenCalled();
  });

  it("does not cascade a missing-step advance into the next present guide step", async () => {
    const onNext = vi.fn();
    const threads = {
      ...step,
      id: "threads-intro",
      targetOnboardingId: "document-threads",
      title: "Every discussion has a home",
      fallback: "skip" as const,
    };
    const watchers = {
      ...step,
      id: "watchers-intro",
      targetOnboardingId: "document-watchers",
      title: "Let an agent keep watch",
      fallback: "skip" as const,
    };

    function ConsecutiveSteps() {
      const [active, setActive] = useState(threads);
      return (
        <>
          <button
            data-onboarding-id="document-watchers"
            ref={(node) => {
              if (node) node.getBoundingClientRect = () => ({ left: 1080, top: 20, right: 1240, bottom: 56, width: 160, height: 36 }) as DOMRect;
            }}
          >
            Watchers
          </button>
          <Onboarding
            step={active}
            stepIndex={active.id === "threads-intro" ? 1 : 2}
            total={3}
            onNext={() => {
              onNext();
              setActive(watchers);
            }}
            onBack={vi.fn()}
            onSkip={vi.fn()}
          />
        </>
      );
    }

    render(<ConsecutiveSteps />);

    expect(await screen.findByRole("dialog", { name: "Let an agent keep watch" })).toBeTruthy();
    expect(screen.getByText("Step 3 of 3")).toBeTruthy();
    expect(onNext).toHaveBeenCalledTimes(1);
  });
});

describe("useOnboarding", () => {
  // The 3-step guided sequence (Eva's locked shape). Teaching steps fall back to "skip".
  const guide: OnboardingStep[] = [
    step, // create-first-document, index 0, fallback "page-card"
    { ...step, id: "threads-intro", targetOnboardingId: "document-threads", title: "Every discussion has a home", fallback: "skip" },
    { ...step, id: "watchers-intro", targetOnboardingId: "document-watchers", title: "Let an agent keep watch", fallback: "skip" },
  ];

  // Stand-in for the one keyed event store (useScopedEventFlags): a mutable set + a
  // spyable writer. The hook no longer owns event storage.
  function store(initial: string[] = []) {
    const events = new Set<string>(initial);
    return { events, record: vi.fn((event: string) => { events.add(event); }) };
  }

  const base = { scopeKey: "acct.A.ws.W", checklistDismissedKey: "acct.A.ws.W.checklistDismissed" };

  it("derives guide completion from live per-node state — no stored boolean, version bump reopens", () => {
    const s = store();
    const { result, rerender } = renderHook(
      (props: { completedIds: Set<string>; activeSpotlightId: string | null }) =>
        useOnboarding({ steps: guide, events: s.events, record: s.record, ...base, ...props }),
      { initialProps: { completedIds: new Set(guide.map((g) => g.id)), activeSpotlightId: null as string | null } },
    );
    // All steps complete → the engine offers no spotlight → guide done, nothing shows,
    // and no guideCompleted boolean was ever written.
    expect(result.current.active).toBeNull();
    expect(localStorage.getItem("acct.A.ws.W.guideCompleted")).toBeNull();
    // A version bump leaves that node incomplete + active again → it reappears; no cached
    // boolean exists to override the versioned source of truth.
    rerender({ completedIds: new Set(["create-first-document", "watchers-intro"]), activeSpotlightId: "threads-intro" });
    expect(result.current.active?.id).toBe("threads-intro");
  });

  it("binds the counter to the original position and Back revisits prior original steps", () => {
    const s = store();
    const { result, rerender } = renderHook(
      (props: { completedIds: Set<string>; activeSpotlightId: string | null }) =>
        useOnboarding({ steps: guide, events: s.events, record: s.record, ...base, ...props }),
      { initialProps: { completedIds: new Set(["create-first-document"]), activeSpotlightId: "threads-intro" } },
    );
    // create-first-document complete → threads at 2 of 3 (not 1 of a shrunken 2).
    expect(result.current.active?.id).toBe("threads-intro");
    expect(result.current.stepIndex).toBe(1);
    expect(result.current.total).toBe(3);
    // Back → revisit the completed create-first-document at 1 of 3; completion untouched.
    act(() => result.current.back());
    expect(result.current.active?.id).toBe("create-first-document");
    expect(result.current.stepIndex).toBe(0);
    expect(s.record).not.toHaveBeenCalled();
    // Next from a revisit returns FORWARD to the frontier, re-recording nothing.
    act(() => result.current.next());
    expect(result.current.active?.id).toBe("threads-intro");
    expect(result.current.stepIndex).toBe(1);
    expect(s.record).not.toHaveBeenCalled();
    // Advance to watchers (3 of 3); Back revisits threads (2 of 3).
    rerender({ completedIds: new Set(["create-first-document", "threads-intro"]), activeSpotlightId: "watchers-intro" });
    expect(result.current.stepIndex).toBe(2);
    act(() => result.current.back());
    expect(result.current.active?.id).toBe("threads-intro");
    expect(result.current.stepIndex).toBe(1);
  });

  it("Next at the frontier acknowledges the current spotlight in its own scope", () => {
    const s = store();
    const { result } = renderHook(() => useOnboarding({
      steps: guide, events: s.events, record: s.record, ...base,
      completedIds: new Set(["create-first-document"]), activeSpotlightId: "threads-intro",
    }));
    act(() => result.current.next());
    expect(s.record).toHaveBeenCalledWith("seen:threads-intro@v1", "workspace");
  });

  it("keeps guide Skip session-only and resumable; a tip Skip is a durable acknowledge", () => {
    const s = store();
    const { result, rerender } = renderHook(
      (props: { completedIds: Set<string>; activeSpotlightId: string | null; tip: OnboardingStep | null }) =>
        useOnboarding({ steps: guide, events: s.events, record: s.record, ...base, ...props }),
      { initialProps: { completedIds: new Set<string>(), activeSpotlightId: "create-first-document" as string | null, tip: null as OnboardingStep | null } },
    );
    expect(result.current.active?.id).toBe("create-first-document");
    act(() => result.current.skip());
    expect(result.current.active).toBeNull();
    expect(s.record).not.toHaveBeenCalled(); // session-only, nothing durable
    act(() => result.current.resume());
    expect(result.current.active?.id).toBe("create-first-document");
    // A contextual tip's Skip acknowledges durably in the tip's account scope.
    rerender({ completedIds: new Set(guide.map((g) => g.id)), activeSpotlightId: null, tip });
    expect(result.current.active?.id).toBe("tip-first-selection");
    act(() => result.current.skip());
    expect(s.record).toHaveBeenCalledWith("seen:tip-first-selection@v1", "account");
  });

  it("gives an active spotlight precedence and defers the still-triggered account tip", () => {
    const s = store();
    const { result, rerender } = renderHook(
      (props: { activeSpotlightId: string | null }) =>
        useOnboarding({ steps: guide, events: s.events, record: s.record, ...base, completedIds: new Set(), tip, ...props }),
      { initialProps: { activeSpotlightId: "create-first-document" as string | null } },
    );
    expect(result.current.active?.id).toBe("create-first-document");
    rerender({ activeSpotlightId: null });
    expect(result.current.active?.id).toBe("tip-first-selection");
  });

  it("resets session and rereads the dismissed flag when the scope key changes", () => {
    localStorage.setItem("acct.A.ws.a.checklistDismissed", "true");
    const s = store();
    const { result, rerender } = renderHook(
      (props: { scopeKey: string; checklistDismissedKey: string }) =>
        useOnboarding({ steps: guide, events: s.events, record: s.record, completedIds: new Set<string>(), activeSpotlightId: "create-first-document", ...props }),
      { initialProps: { scopeKey: "acct.A.ws.a", checklistDismissedKey: "acct.A.ws.a.checklistDismissed" } },
    );
    act(() => result.current.skip());
    expect(result.current.active).toBeNull();
    expect(result.current.checklistDismissed).toBe(true);
    // New workspace scope: the session-only skip clears and the dismissed flag is reread.
    rerender({ scopeKey: "acct.A.ws.b", checklistDismissedKey: "acct.A.ws.b.checklistDismissed" });
    expect(result.current.active?.id).toBe("create-first-document");
    expect(result.current.checklistDismissed).toBe(false);
  });

  it("exposes the store's event set and writer, and persists checklist dismissal", () => {
    const s = store(["member_invited"]);
    const { result } = renderHook(() => useOnboarding({
      steps: guide, events: s.events, record: s.record, ...base,
      completedIds: new Set<string>(), activeSpotlightId: null,
    }));
    expect(result.current.recordedEvents.has("member_invited")).toBe(true);
    act(() => result.current.record("first_thread_created", "account"));
    expect(s.record).toHaveBeenCalledWith("first_thread_created", "account");
    expect(result.current.checklistDismissed).toBe(false);
    act(() => result.current.dismissChecklist());
    expect(localStorage.getItem("acct.A.ws.W.checklistDismissed")).toBe("true");
    expect(result.current.checklistDismissed).toBe(true);
  });

  it("acknowledges each node version in the node's own scope", () => {
    const s = store();
    const { result } = renderHook(() => useOnboarding({
      steps: guide, events: s.events, record: s.record, ...base,
      completedIds: new Set<string>(), activeSpotlightId: null,
    }));
    act(() => result.current.acknowledge({ id: "tip-first-selection", version: 1, scope: "account" }));
    act(() => result.current.acknowledge({ id: "tip-first-selection", version: 2, scope: "account" }));
    expect(s.record).toHaveBeenNthCalledWith(1, "seen:tip-first-selection@v1", "account");
    expect(s.record).toHaveBeenNthCalledWith(2, "seen:tip-first-selection@v2", "account");
  });
});

describe("OnboardingChapterCard (#56)", () => {
  const connectStep: OnboardingStep = {
    id: "add-teammate-connect",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eyebrow: "ADD AN AI TEAMMATE · OPTIONAL",
    title: "Connect a local environment",
    body: "A local environment is the computer where your agents run.",
    primaryAction: { label: "Connect environment", event: "open-create-environment" },
    secondaryAction: { label: "Not now", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  };
  const memberCard: OnboardingStep = {
    id: "add-teammate-member",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eyebrow: "WORK WITH AN AGENT",
    title: "Put an agent to work",
    body: "This workspace has agents. Choose one and start a run.",
    caption: "No setup needed",
    primaryAction: { label: "Start a run", event: "open-agent-work" },
    secondaryAction: { label: "Not now", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  };
  const doneCard: OnboardingStep = {
    id: "add-teammate-done",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eyebrow: "STARTED",
    title: "You've started working with an agent",
    body: "You can start more work anytime from Agents.",
    caption: "Chapter complete",
    primaryAction: { label: "Close", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  };

  it("owner step: eyebrow + real CTA + step dots + Not now; primary fires the action, Not now only dismisses", () => {
    const onAction = vi.fn();
    const onDismiss = vi.fn();
    render(<OnboardingChapterCard step={connectStep} stepIndex={0} total={3} onAction={onAction} onDismiss={onDismiss} />);
    expect(screen.getByText("ADD AN AI TEAMMATE · OPTIONAL")).toBeTruthy();
    expect(screen.getByText("Step 1 of 3")).toBeTruthy();
    fireEvent.click(screen.getByText("Connect environment"));
    expect(onAction).toHaveBeenCalledWith("open-create-environment");
    fireEvent.click(screen.getByText("Not now"));
    expect(onDismiss).toHaveBeenCalledTimes(1);
    // "Not now" dismisses only — it never fires an action (Anton's invariant).
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it("member card: single action with a caption, no step counter", () => {
    const onAction = vi.fn();
    render(<OnboardingChapterCard step={memberCard} stepIndex={0} total={1} onAction={onAction} onDismiss={vi.fn()} />);
    expect(screen.getByText(/No setup needed/)).toBeTruthy();
    expect(screen.queryByText(/Step 1 of/)).toBeNull(); // single action → no dots/counter
    fireEvent.click(screen.getByText("Start a run"));
    expect(onAction).toHaveBeenCalledWith("open-agent-work");
  });

  it("done card: no CTA button; 'Close' dismisses and never fires an action; shows completion status", () => {
    const onAction = vi.fn();
    const onDismiss = vi.fn();
    render(<OnboardingChapterCard step={doneCard} stepIndex={-1} total={3} onAction={onAction} onDismiss={onDismiss} />);
    expect(screen.getByText("Chapter complete")).toBeTruthy();
    expect(screen.queryByText(/Step .* of/)).toBeNull();
    fireEvent.click(screen.getByText("Close"));
    expect(onDismiss).toHaveBeenCalledTimes(1);
    expect(onAction).not.toHaveBeenCalled();
  });

  it("Escape dismisses the chapter", () => {
    const onDismiss = vi.fn();
    render(<OnboardingChapterCard step={connectStep} stepIndex={0} total={3} onDismiss={onDismiss} />);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onDismiss).toHaveBeenCalled();
  });
});
