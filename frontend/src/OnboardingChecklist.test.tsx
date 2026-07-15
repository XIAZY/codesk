// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CHECKLIST_ITEMS } from "./onboarding";
import { OnboardingChecklist } from "./OnboardingChecklist";

afterEach(cleanup);

const progress = CHECKLIST_ITEMS.slice(0, 3).map((item, index) => ({ item, done: index === 0 }));

describe("OnboardingChecklist", () => {
  it("shows honest visible-item progress and text status in configured order", () => {
    const { container } = render(<OnboardingChecklist progress={progress} dismissed={false} onDismiss={vi.fn()} />);

    expect(screen.getByRole("complementary", { name: "Finish setting up this workspace" })).toBeTruthy();
    expect(screen.getByText("Work through these as you go — you don't have to do them all at once.")).toBeTruthy();
    expect(screen.getByText("1 of 3 done")).toBeTruthy();
    const meter = screen.getByRole("progressbar", { name: "1 of 3 onboarding tasks done" });
    expect(meter.getAttribute("value")).toBe("1");
    expect(meter.getAttribute("max")).toBe("3");
    expect(screen.getAllByRole("listitem").map((row) => row.lastElementChild?.textContent)).toEqual([
      "Done: Create your first document",
      "Not done: Start a discussion",
      "Not done: Connect a local environment",
    ]);
    expect(container.querySelectorAll('[data-done="true"]')).toHaveLength(1);
  });

  it("uses an accessible icon-only dismiss action and honors durable dismissal", () => {
    const onDismiss = vi.fn();
    const { rerender } = render(<OnboardingChecklist progress={progress} dismissed={false} onDismiss={onDismiss} />);

    fireEvent.click(screen.getByRole("button", { name: "Dismiss checklist" }));
    expect(onDismiss).toHaveBeenCalledOnce();
    rerender(<OnboardingChecklist progress={progress} dismissed onDismiss={onDismiss} />);
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("stays visible in a quiet all-done state", () => {
    const completed = progress.map(({ item }) => ({ item, done: true }));
    render(<OnboardingChecklist progress={completed} dismissed={false} onDismiss={vi.fn()} />);

    expect(screen.getByText("All done")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "All 3 onboarding tasks done" }).getAttribute("value")).toBe("3");
    expect(screen.getAllByText(/^Done:/)).toHaveLength(3);
  });

  it("does not render an empty role-filtered checklist", () => {
    render(<OnboardingChecklist progress={[]} dismissed={false} onDismiss={vi.fn()} />);
    expect(screen.queryByRole("complementary")).toBeNull();
  });
});
