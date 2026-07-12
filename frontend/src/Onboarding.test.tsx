// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Onboarding, spotlightGeometry, type OnboardingStep } from "./Onboarding";
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
    const tip = {
      ...step,
      id: "tip-first-selection",
      scope: "account" as const,
      presentation: "tip" as const,
      title: "Talk about this exact line",
      primaryAction: { label: "Start thread", event: "open-thread-draft" as const },
      secondaryAction: { label: "Got it", event: "dismiss" as const },
    };
    const onAction = vi.fn();
    const onNext = vi.fn();
    const onSkip = vi.fn();
    render(
      <>
        <button
          data-onboarding-id="create-document"
          ref={(node) => {
            if (node) node.getBoundingClientRect = () => ({ left: 100, top: 80, right: 200, bottom: 120, width: 100, height: 40 }) as DOMRect;
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

    await screen.findByRole("dialog", { name: "Talk about this exact line" });
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
});

describe("useOnboarding", () => {
  const steps: OnboardingStep[] = [step, { ...step, id: "connect-local", title: "Connect your local environment" }];
  const flagKeys = {
    accountFlagsKey: "codesk.onboarding.account.flags",
    workspaceFlagsKey: "codesk.onboarding.ws.ws.flags",
  };

  it("derives incomplete steps, keeps Skip resumable, and persists only final completion", () => {
    const { result } = renderHook(() => useOnboarding({
      steps,
      completedIds: new Set(["create-first-document"]),
      guideCompletedKey: "codesk.onboarding.ws.ws.guideCompleted",
      ...flagKeys,
    }));

    expect(result.current.active?.id).toBe("connect-local");
    act(() => result.current.skip());
    expect(result.current.active).toBeNull();
    expect(localStorage.getItem("codesk.onboarding.ws.ws.guideCompleted")).toBeNull();
    act(() => result.current.resume());
    expect(result.current.active?.id).toBe("connect-local");
    act(() => result.current.next());
    expect(result.current.active).toBeNull();
    expect(localStorage.getItem("codesk.onboarding.ws.ws.guideCompleted")).toBe("true");
  });

  it("records events through the caller and persists checklist dismissal", () => {
    const onRecord = vi.fn();
    const { result } = renderHook(() => useOnboarding({
      steps,
      completedIds: new Set(),
      guideCompletedKey: "codesk.onboarding.ws.ws.guideCompleted",
      checklistDismissedKey: "codesk.onboarding.ws.ws.checklistDismissed",
      onRecord,
      ...flagKeys,
    }));

    act(() => result.current.record("first_document_created", "workspace"));
    expect(onRecord).toHaveBeenCalledWith("first_document_created", "workspace");
    expect(result.current.recordedEvents.has("first_document_created")).toBe(true);
    act(() => result.current.dismissChecklist());
    expect(localStorage.getItem("codesk.onboarding.ws.ws.checklistDismissed")).toBe("true");
    expect(result.current.checklistDismissed).toBe(true);
  });

  it("does not lose same-scope events recorded before React rerenders", () => {
    const onRecord = vi.fn();
    const { result } = renderHook(() => useOnboarding({
      steps,
      completedIds: new Set(),
      guideCompletedKey: "codesk.onboarding.ws.ws.guideCompleted",
      onRecord,
      ...flagKeys,
    }));

    act(() => {
      result.current.record("first_thread_created", "workspace");
      result.current.record("first_thread_replied", "workspace");
    });
    expect(JSON.parse(localStorage.getItem(flagKeys.workspaceFlagsKey) ?? "[]")).toEqual([
      "first_thread_created",
      "first_thread_replied",
    ]);
    expect(result.current.recordedEvents.has("first_thread_created")).toBe(true);
    expect(result.current.recordedEvents.has("first_thread_replied")).toBe(true);
    expect(onRecord).toHaveBeenCalledTimes(2);
  });

  it("keeps the current step stable when an earlier live completion arrives", () => {
    const threeSteps = [step, { ...step, id: "connect-local" }, { ...step, id: "create-agent" }];
    const { result, rerender } = renderHook(
      ({ completedIds }) => useOnboarding({
        steps: threeSteps,
        completedIds,
        guideCompletedKey: "codesk.onboarding.ws.ws.guideCompleted",
        ...flagKeys,
      }),
      { initialProps: { completedIds: new Set<string>() } },
    );

    act(() => result.current.next());
    expect(result.current.active?.id).toBe("connect-local");
    rerender({ completedIds: new Set(["create-first-document"]) });
    expect(result.current.active?.id).toBe("connect-local");
    expect(result.current.stepIndex).toBe(0);
  });

  it("scopes durable flags and session-only state to the caller's workspace key", () => {
    localStorage.setItem("codesk.onboarding.ws.a.guideCompleted", "true");
    localStorage.setItem("codesk.onboarding.ws.a.checklistDismissed", "true");
    const { result, rerender } = renderHook(
      ({ workspace }) => useOnboarding({
        steps,
        completedIds: new Set(),
        guideCompletedKey: `codesk.onboarding.ws.${workspace}.guideCompleted`,
        checklistDismissedKey: `codesk.onboarding.ws.${workspace}.checklistDismissed`,
        accountFlagsKey: "codesk.onboarding.account.flags",
        workspaceFlagsKey: `codesk.onboarding.ws.${workspace}.flags`,
      }),
      { initialProps: { workspace: "a" } },
    );

    expect(result.current.active).toBeNull();
    expect(result.current.guideCompleted).toBe(true);
    expect(result.current.checklistDismissed).toBe(true);

    rerender({ workspace: "b" });
    expect(result.current.active?.id).toBe("create-first-document");
    expect(result.current.guideCompleted).toBe(false);
    expect(result.current.checklistDismissed).toBe(false);
    act(() => {
      result.current.skip();
      result.current.record("seen:tip-first-selection@v1", "account");
      result.current.record("first_document_created", "workspace");
    });
    expect(result.current.active).toBeNull();
    expect(result.current.recordedEvents.has("first_document_created")).toBe(true);
    expect(JSON.parse(localStorage.getItem("codesk.onboarding.account.flags") ?? "[]")).toContain("seen:tip-first-selection@v1");
    expect(JSON.parse(localStorage.getItem("codesk.onboarding.ws.b.flags") ?? "[]")).toContain("first_document_created");

    rerender({ workspace: "c" });
    expect(result.current.active?.id).toBe("create-first-document");
    expect(result.current.recordedEvents.has("seen:tip-first-selection@v1")).toBe(true);
    expect(result.current.recordedEvents.has("first_document_created")).toBe(false);
  });

  it("gives an active spotlight precedence and defers the still-triggered account tip", () => {
    const tip = { ...step, id: "tip-first-selection", scope: "account" as const, presentation: "tip" as const };
    const { result, rerender } = renderHook(
      ({ activeSpotlightId }) => useOnboarding({
        steps,
        completedIds: new Set(),
        guideCompletedKey: "codesk.onboarding.ws.ws.guideCompleted",
        activeSpotlightId,
        tip,
        ...flagKeys,
      }),
      { initialProps: { activeSpotlightId: "create-first-document" as string | null } },
    );

    expect(result.current.active?.id).toBe("create-first-document");
    rerender({ activeSpotlightId: null });
    expect(result.current.active?.id).toBe("tip-first-selection");
    act(() => result.current.skip());
    expect(JSON.parse(localStorage.getItem(flagKeys.accountFlagsKey) ?? "[]")).toContain("seen:tip-first-selection@v1");
  });

  it("acknowledges each node version in the node's own scope", () => {
    const { result } = renderHook(() => useOnboarding({
      steps,
      completedIds: new Set(),
      guideCompletedKey: "codesk.onboarding.ws.ws.guideCompleted",
      ...flagKeys,
    }));
    const tip = { ...step, id: "tip-first-selection", scope: "account" as const, presentation: "tip" as const };

    act(() => result.current.acknowledge(tip));
    act(() => result.current.acknowledge({ ...tip, version: 2 }));
    expect(JSON.parse(localStorage.getItem(flagKeys.accountFlagsKey) ?? "[]")).toEqual([
      "seen:tip-first-selection@v1",
      "seen:tip-first-selection@v2",
    ]);
    expect(localStorage.getItem(flagKeys.workspaceFlagsKey)).toBeNull();
  });
});
