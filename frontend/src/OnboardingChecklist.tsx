import type { OnboardingChecklistItem } from "./onboarding";

export type OnboardingChecklistProgress = {
  item: OnboardingChecklistItem;
  done: boolean;
};

type OnboardingChecklistProps = {
  progress: readonly OnboardingChecklistProgress[];
  dismissed: boolean;
  onDismiss: () => void;
};

export function OnboardingChecklist({ progress, dismissed, onDismiss }: OnboardingChecklistProps) {
  if (dismissed || progress.length === 0) return null;

  const doneCount = progress.filter(({ done }) => done).length;
  const allDone = doneCount === progress.length;
  const progressText = allDone ? "All done" : `${doneCount} of ${progress.length} done`;
  const progressLabel = allDone
    ? `All ${progress.length} onboarding tasks done`
    : `${doneCount} of ${progress.length} onboarding tasks done`;

  return (
    <aside
      className={`ob-checklist${allDone ? " complete" : ""}`}
      aria-labelledby="ob-checklist-title"
    >
      <header className="ob-checklist-header">
        <div>
          <h3 id="ob-checklist-title">Finish setting up this workspace</h3>
          <p>Work through these as you go — you don't have to do them all at once.</p>
        </div>
        <button type="button" className="ob-checklist-dismiss" onClick={onDismiss} aria-label="Dismiss checklist">
          ×
        </button>
      </header>
      <div className="ob-checklist-progress">
        <span aria-live="polite">{progressText}</span>
        <progress value={doneCount} max={progress.length} aria-label={progressLabel} />
      </div>
      <ul className="ob-checklist-list">
        {progress.map(({ item, done }) => (
          <li key={item.id} data-done={done}>
            <span className="ob-checklist-check" aria-hidden="true">{done ? "✓" : ""}</span>
            <span>
              <span className="sr-only">{done ? "Done: " : "Not done: "}</span>
              {item.label}
            </span>
          </li>
        ))}
      </ul>
    </aside>
  );
}
