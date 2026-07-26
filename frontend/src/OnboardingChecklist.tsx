import type { OnboardingChecklistItem } from "./onboardingEngine";

export type OnboardingChecklistProgress = {
  item: OnboardingChecklistItem;
  done: boolean;
};

type OnboardingChecklistProps = {
  progress: readonly OnboardingChecklistProgress[];
  dismissed: boolean;
  onDismiss: () => void;
  // The chapter-entry row (item.opensChapter) opens the "Add an AI teammate" chapter and
  // shows the owner/admin derived progress badge ("N of M"); the member entry has no badge.
  onOpenChapter?: () => void;
  chapterStepIndex?: number; // 0-based active chapter step (for the owner/admin "N of M" badge)
  chapterTotal?: number; // number of chapter steps for this role (>1 owner/admin, 1 member)
};

export function OnboardingChecklist({
  progress,
  dismissed,
  onDismiss,
  onOpenChapter,
  chapterStepIndex = 0,
  chapterTotal = 0,
}: OnboardingChecklistProps) {
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
        {progress.map(({ item, done }) => {
          if (item.opensChapter) {
            // Resumable chapter entry: opens the chapter; past-tense label once complete,
            // an owner/admin "N of M" derived badge while in progress (the member entry is a
            // single action → no badge). Still clickable when done (reopens the done card).
            const label = done && item.doneLabel ? item.doneLabel : item.label;
            const sub = done ? item.doneSubtitle : item.subtitle;
            const showBadge = !done && chapterTotal > 1;
            return (
              <li key={item.id} data-done={done} className="ob-checklist-entry-item">
                <button
                  type="button"
                  className="ob-checklist-entry"
                  onClick={onOpenChapter}
                  aria-label={`${done ? "Done: " : ""}${label} — open`}
                >
                  <span className="ob-checklist-check" aria-hidden="true">{done ? "✓" : "☆"}</span>
                  <span className="ob-checklist-entry-text">
                    <span className="ob-checklist-entry-label">{label}</span>
                    {sub ? <span className="ob-checklist-entry-sub">{sub}</span> : null}
                  </span>
                  {done ? (
                    <span className="ob-checklist-entry-badge done">Done</span>
                  ) : showBadge ? (
                    <span className="ob-checklist-entry-badge">{chapterStepIndex + 1} of {chapterTotal}</span>
                  ) : null}
                  <span className="ob-checklist-entry-chev" aria-hidden="true">›</span>
                </button>
              </li>
            );
          }
          return (
            <li key={item.id} data-done={done}>
              <span className="ob-checklist-check" aria-hidden="true">{done ? "✓" : ""}</span>
              <span>
                <span className="sr-only">{done ? "Done: " : "Not done: "}</span>
                {item.label}
              </span>
            </li>
          );
        })}
      </ul>
    </aside>
  );
}
