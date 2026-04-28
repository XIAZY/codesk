export type ReplaceOp = {
  start: number;
  end: number;
  text: string;
};

export type TextSelection = {
  start: number;
  end: number;
};

export type LineThreadGroup<T> = {
  line: number;
  threads: T[];
};

export type AgentStatusSummary = {
  label: "working" | "idle" | "disconnected";
  tone: "running" | "idle" | "failed";
  copy: string;
};

export function resolveWorkspaceLoad<T extends { id: string }>(
  requestToken: number,
  latestToken: number,
  documents: T[],
  preferredSelectedId: string
) {
  if (requestToken !== latestToken) {
    return { shouldApply: false, selectedId: preferredSelectedId };
  }
  if (preferredSelectedId && documents.some((document) => document.id === preferredSelectedId)) {
    return { shouldApply: true, selectedId: preferredSelectedId };
  }
  return { shouldApply: true, selectedId: documents[0]?.id ?? "" };
}

export function computeReplace(before: string, after: string): ReplaceOp {
  let start = 0;
  while (start < before.length && start < after.length && before[start] === after[start]) {
    start += 1;
  }

  let beforeEnd = before.length;
  let afterEnd = after.length;
  while (
    beforeEnd > start &&
    afterEnd > start &&
    before[beforeEnd - 1] === after[afterEnd - 1]
  ) {
    beforeEnd -= 1;
    afterEnd -= 1;
  }

  return { start, end: beforeEnd, text: after.slice(start, afterEnd) };
}

export function mapIndexThroughReplace(
  index: number,
  replace: ReplaceOp,
  affinity: "left" | "right" = "left"
) {
  if (index < replace.start) {
    return index;
  }
  if (index > replace.end) {
    return index + replace.text.length - (replace.end - replace.start);
  }
  if (index === replace.start) {
    if (replace.start === replace.end && affinity === "right") {
      return replace.start + replace.text.length;
    }
    return replace.start;
  }
  if (index === replace.end) {
    return replace.start + replace.text.length;
  }
  return replace.start + replace.text.length;
}

export function rebaseReplace(local: ReplaceOp, remoteBaseToCurrent: ReplaceOp): ReplaceOp {
  const start = mapIndexThroughReplace(local.start, remoteBaseToCurrent, "left");
  const end = mapIndexThroughReplace(local.end, remoteBaseToCurrent, "left");
  return {
    start,
    end: Math.max(start, end),
    text: local.text,
  };
}

export function rebaseSelection(selection: TextSelection, remoteBaseToCurrent: ReplaceOp): TextSelection {
  const start = mapIndexThroughReplace(selection.start, remoteBaseToCurrent, "right");
  const end = mapIndexThroughReplace(selection.end, remoteBaseToCurrent, "right");
  return {
    start,
    end: Math.max(start, end),
  };
}

export function buildLineThreads<T extends { anchor: { line: number } }>(threads: T[]): Array<LineThreadGroup<T>> {
  const grouped = new Map<number, T[]>();
  for (const thread of threads) {
    const line = Math.max(1, thread.anchor.line || 1);
    const current = grouped.get(line) ?? [];
    current.push(thread);
    grouped.set(line, current);
  }
  return Array.from(grouped.entries())
    .sort((left, right) => left[0] - right[0])
    .map(([line, groupedThreads]) => ({ line, threads: groupedThreads }));
}

export function isFreshPresence(value?: string) {
  if (!value) {
    return false;
  }
  return Date.now() - new Date(value).getTime() < 20_000;
}

export function summarizeAgentStatus(
  workspaceConnected: boolean,
  agent: { status?: string; currentActivity?: string },
  currentRun: { status: string; desiredStatus: string; lastMessage?: string; error?: string } | null,
  presence: { activity?: string } | null
): AgentStatusSummary {
  if (!workspaceConnected) {
    return {
      label: "disconnected",
      tone: "failed",
      copy: "Workspace connection unavailable.",
    };
  }
  if (agent.status === "working") {
    return {
      label: "working",
      tone: "running",
      copy: presence?.activity || agent.currentActivity || "Thinking through the current task.",
    };
  }
  if (agent.status === "disconnected") {
    return {
      label: "disconnected",
      tone: "failed",
      copy: agent.currentActivity || "Agent session disconnected.",
    };
  }
  if (currentRun && currentRun.desiredStatus === "running" && !["completed", "failed", "stopped"].includes(currentRun.status)) {
    return {
      label: "working",
      tone: "running",
      copy: presence?.activity || agent.currentActivity || "Thinking through the current task.",
    };
  }
  return {
    label: "idle",
    tone: "idle",
    copy: currentRun?.error || currentRun?.lastMessage || presence?.activity || agent.currentActivity || "Ready for the next task.",
  };
}
