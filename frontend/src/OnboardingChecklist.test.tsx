// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CHECKLIST_ITEMS } from "./onboardingEngine";
import { OnboardingChecklist } from "./OnboardingChecklist";

afterEach(cleanup);

const byId = (id: string) => CHECKLIST_ITEMS.find((i) => i.id === id)!;
// An owner/admin-shaped checklist (#56 single-entry restructure): two plain tasks + the
// ONE "Add an AI teammate" chapter entry (counts in the denominator) + invite.
const ownerProgress = [
  { item: byId("create-document"), done: true },
  { item: byId("start-discussion"), done: false },
  { item: byId("add-teammate-entry"), done: false },
  { item: byId("invite-team"), done: false },
];

describe("OnboardingChecklist", () => {
  it("counts the chapter entry in the denominator and renders in order", () => {
    render(
      <OnboardingChecklist progress={ownerProgress} dismissed={false} onDismiss={vi.fn()} chapterStepIndex={0} chapterTotal={2} />,
    );
    expect(screen.getByRole("complementary", { name: "Finish setting up this workspace" })).toBeTruthy();
    // The entry participates honestly: 1 of 4, not a side action (Anton).
    expect(screen.getByText("1 of 4 done")).toBeTruthy();
    const meter = screen.getByRole("progressbar", { name: "1 of 4 onboarding tasks done" });
    expect(meter.getAttribute("value")).toBe("1");
    expect(meter.getAttribute("max")).toBe("4");
    expect(screen.getByText("Create your first document")).toBeTruthy();
    expect(screen.getByText("Invite your team")).toBeTruthy();
  });

  it("renders the chapter entry as a launcher: subtitle, owner/admin 'N of M' badge, opens on click", () => {
    const onOpenChapter = vi.fn();
    render(
      <OnboardingChecklist
        progress={ownerProgress}
        dismissed={false}
        onDismiss={vi.fn()}
        onOpenChapter={onOpenChapter}
        chapterStepIndex={0}
        chapterTotal={2}
      />,
    );
    expect(screen.getByText("Add an AI teammate")).toBeTruthy();
    // #90: subtitle drops any "put it to work" wording (2 steps: connect + create).
    expect(screen.getByText("Connect an environment and create an agent")).toBeTruthy();
    expect(screen.getByText("1 of 2")).toBeTruthy(); // derived owner/admin progress badge
    fireEvent.click(screen.getByText("Add an AI teammate"));
    expect(onOpenChapter).toHaveBeenCalledOnce();
  });

  it("completed chapter entry uses historical past-tense copy + a 'Done' badge, still a launcher", () => {
    const onOpenChapter = vi.fn();
    const done = ownerProgress.map((row) => (row.item.id === "add-teammate-entry" ? { ...row, done: true } : row));
    render(
      <OnboardingChecklist
        progress={done}
        dismissed={false}
        onDismiss={vi.fn()}
        onOpenChapter={onOpenChapter}
        chapterStepIndex={-1}
        chapterTotal={2}
      />,
    );
    // #90: the done label is the Ready framing, never a work/"started" line.
    expect(screen.getByText("Your AI teammate is ready")).toBeTruthy();
    expect(screen.getByText("Done")).toBeTruthy();
    expect(screen.queryByText("1 of 2")).toBeNull(); // no progress badge once done
    fireEvent.click(screen.getByText("Your AI teammate is ready"));
    expect(onOpenChapter).toHaveBeenCalledOnce();
  });

  it("uses an accessible icon-only dismiss action and honors durable dismissal", () => {
    const onDismiss = vi.fn();
    const { rerender } = render(<OnboardingChecklist progress={ownerProgress} dismissed={false} onDismiss={onDismiss} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss checklist" }));
    expect(onDismiss).toHaveBeenCalledOnce();
    rerender(<OnboardingChecklist progress={ownerProgress} dismissed onDismiss={onDismiss} />);
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("stays visible in a quiet all-done state", () => {
    const completed = ownerProgress.map((row) => ({ ...row, done: true }));
    render(
      <OnboardingChecklist progress={completed} dismissed={false} onDismiss={vi.fn()} chapterStepIndex={-1} chapterTotal={2} />,
    );
    expect(screen.getByText("All done")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "All 4 onboarding tasks done" }).getAttribute("value")).toBe("4");
    // The completed entry shows its historical (Ready) label, never a present-tense action.
    expect(screen.getByText("Your AI teammate is ready")).toBeTruthy();
  });

  it("does not render an empty role-filtered checklist", () => {
    render(<OnboardingChecklist progress={[]} dismissed={false} onDismiss={vi.fn()} />);
    expect(screen.queryByRole("complementary")).toBeNull();
  });
});
