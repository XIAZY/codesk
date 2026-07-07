import * as Y from "yjs";
import type { ActivityEvent, Agent, AgentEvent, AgentRun, Daemon, ThreadAnchor, ThreadItem, WorkspaceEvent, WorkspaceState } from "./types";

export const identifierPattern = "[a-z0-9_-]+";
export const identifierHelpText = "Only lowercase letters, numbers, underscores, and dashes.";
export const workspaceSlugMinLength = 2;
export const workspaceSlugMaxLength = 64;
export const handleMinLength = 2;
export const handleMaxLength = 32;

export type ReplaceOp = {
  start: number;
  end: number;
  text: string;
};

export type LineThreadGroup<T> = {
  line: number;
  threads: T[];
};

export type ResolvedThreadAnchor = ThreadAnchor & {
  start: number;
  end: number;
  line: number;
  excerpt: string;
  resolved: boolean;
};

export function shellQuote(value: string) {
  if (value.length === 0) {
    return "''";
  }
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(value)) {
    return value;
  }
  return `'${value.replace(/'/g, "'\\''")}'`;
}

export function isMarkdownDocumentPath(path: string) {
  return /\.(md|markdown)$/i.test(path);
}

export function buildDaemonInstallCommand(input: {
  backendUrl: string;
  workspaceId: string;
  daemonToken: string;
  staticBaseUrl?: string;
}) {
  const backendUrl = input.backendUrl.trim().replace(/\/+$/, "");
  const staticBaseUrl = (input.staticBaseUrl?.trim() || `${backendUrl}/daemons`).replace(/\/+$/, "");
  return [
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/install.sh`)} | sh -s -- \\`,
    `  --backend-url ${shellQuote(backendUrl)} \\`,
    `  --workspace-id ${shellQuote(input.workspaceId)} \\`,
    `  --daemon-token ${shellQuote(input.daemonToken)} \\`,
    `  --static-base ${shellQuote(staticBaseUrl)}`,
  ].join("\n");
}

export function buildDaemonReinstallCommand(input: {
  backendUrl: string;
  workspaceId: string;
  daemonToken: string;
  staticBaseUrl?: string;
}) {
  const backendUrl = input.backendUrl.trim().replace(/\/+$/, "");
  const staticBaseUrl = (input.staticBaseUrl?.trim() || `${backendUrl}/daemons`).replace(/\/+$/, "");
  return [
    "set -e",
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/uninstall.sh`)} | sh -s -- \\`,
    "  --all",
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/install.sh`)} | sh -s -- \\`,
    `  --backend-url ${shellQuote(backendUrl)} \\`,
    `  --workspace-id ${shellQuote(input.workspaceId)} \\`,
    `  --daemon-token ${shellQuote(input.daemonToken)} \\`,
    `  --static-base ${shellQuote(staticBaseUrl)}`,
  ].join("\n");
}

export function buildDaemonUninstallCommand(input: { staticBaseUrl: string }) {
  const staticBaseUrl = input.staticBaseUrl.trim().replace(/\/+$/, "");
  const lines = [
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/uninstall.sh`)} | sh -s -- \\`,
    "  --all",
  ];
  return lines.join("\n");
}

export function emptyWorkspace(): WorkspaceState {
  return {
    workspaceId: "",
    rootDocumentId: "",
    currentMembershipRole: "",
    name: "",
    users: [],
    daemons: [],
    agents: [],
    agentRuns: [],
    threads: [],
    agentEvents: [],
    presences: {},
  };
}

export const workspaceNameAdjectives = [
  "Crimson",
  "Azure",
  "Golden",
  "Silver",
  "Amber",
  "Violet",
  "Coral",
  "Jade",
  "Scarlet",
  "Cobalt",
  "Ivory",
  "Onyx",
  "Emerald",
  "Copper",
  "Indigo",
  "Hazel",
];

export const workspaceNameNouns = [
  "Otter",
  "Falcon",
  "Willow",
  "Harbor",
  "Meadow",
  "Cedar",
  "Lantern",
  "Comet",
  "Canyon",
  "Ember",
  "Ridge",
  "Beacon",
  "Maple",
  "Quartz",
  "Summit",
  "Heron",
];

export function randomWorkspaceName(random: () => number = Math.random): string {
  const adjective = workspaceNameAdjectives[Math.floor(random() * workspaceNameAdjectives.length)];
  const noun = workspaceNameNouns[Math.floor(random() * workspaceNameNouns.length)];
  return `${adjective} ${noun}`;
}

export function identifierFromName(value: string, maxLength: number) {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, maxLength);
}

export function computeReplace(before: string, after: string): ReplaceOp {
  let start = 0;
  while (start < before.length && start < after.length && before[start] === after[start]) {
    start += 1;
  }
  let beforeEnd = before.length;
  let afterEnd = after.length;
  while (beforeEnd > start && afterEnd > start && before[beforeEnd - 1] === after[afterEnd - 1]) {
    beforeEnd -= 1;
    afterEnd -= 1;
  }
  return { start, end: beforeEnd, text: after.slice(start, afterEnd) };
}

export function applyReplaceToYText(text: Y.Text, replace: ReplaceOp) {
  const deleteLength = Math.max(0, replace.end - replace.start);
  if (deleteLength > 0) {
    text.delete(replace.start, deleteLength);
  }
  if (replace.text.length > 0) {
    text.insert(replace.start, replace.text);
  }
}

export function lineStartsForText(text: string) {
  const starts = [0];
  for (let index = 0; index < text.length; index += 1) {
    if (text[index] === "\n") {
      starts.push(index + 1);
    }
  }
  return starts;
}

export function lineForOffset(lineStarts: number[], offset: number) {
  let line = 1;
  for (let index = 0; index < lineStarts.length; index += 1) {
    if (lineStarts[index] > offset) {
      break;
    }
    line = index + 1;
  }
  return line;
}

export function selectionLabel(start: number, end: number, lineStarts: number[]) {
  const startLine = lineForOffset(lineStarts, start);
  const endLine = lineForOffset(lineStarts, Math.max(start, end - 1));
  if (start === end) {
    return `Cursor on line ${startLine}`;
  }
  if (startLine === endLine) {
    return `Selection on line ${startLine}`;
  }
  return `Selection across lines ${startLine}-${endLine}`;
}

export function buildLineThreads<T extends { anchor: { line: number } }>(threads: T[]): Array<LineThreadGroup<T>> {
  const groups = new Map<number, T[]>();
  for (const thread of threads) {
    const line = Math.max(1, thread.anchor.line || 1);
    groups.set(line, [...(groups.get(line) ?? []), thread]);
  }
  return Array.from(groups.entries())
    .sort((left, right) => left[0] - right[0])
    .map(([line, threads]) => ({ line, threads }));
}

export function daemonStatus(daemon: Daemon) {
  if (daemon.status === "deleted") {
    return "deleted";
  }
  return daemon.connectionStatus || "disconnected";
}

// A daemon has genuinely checked in only if it carries a real lastSeenAt. Never-seen daemons carry
// a zero time (year 0001) — the same idiomatic guard the daemon table uses for "Last check-in".
export function hasGenuineCheckIn(daemon: Daemon): boolean {
  return daemon.lastSeenAt != null && new Date(daemon.lastSeenAt).getUTCFullYear() >= 2020;
}

// Liveness decay windows — mirror the backend's daemonOnlineWindow / daemonStaleWindow in
// backend/internal/notty/store.go (applyDaemonLiveness). A daemon that stops checking in emits
// no event, so the client must decay its status on a timer instead of trusting the last payload.
export const DAEMON_ONLINE_WINDOW_MS = 30_000;
export const DAEMON_STALE_WINDOW_MS = 120_000;

// Live daemon status accounting for time elapsed since the last payload landed. We add the
// server-computed lastSeenAgeSeconds to elapsed-since-receipt rather than comparing lastSeenAt to
// the client clock, which would be wrong under clock skew. Windows use <= to match the backend.
export function daemonLiveStatus(daemon: Daemon, nowMs: number): string {
  if (daemon.status === "deleted") {
    return "deleted";
  }
  // A daemon that has never checked in serializes lastSeenAgeSeconds: 0 (no omitempty) with a zero
  // lastSeenAt, so the null-guard alone would fabricate a fresh "online". Gate on a genuine check-in
  // — the same zero-time idiom the table uses — and otherwise trust the server's status.
  if (!hasGenuineCheckIn(daemon) || daemon.lastSeenAgeSeconds == null || daemon.receivedAtMs == null) {
    return daemon.connectionStatus || "disconnected";
  }
  const ageMs = daemon.lastSeenAgeSeconds * 1000 + Math.max(0, nowMs - daemon.receivedAtMs);
  if (ageMs <= DAEMON_ONLINE_WINDOW_MS) {
    return "online";
  }
  if (ageMs <= DAEMON_STALE_WINDOW_MS) {
    return "stale";
  }
  return "disconnected";
}

// Stamp the client receipt time onto daemon payloads as they land, so daemonLiveStatus can decay
// them later. Pure: the caller passes nowMs (Date.now at the socket/snapshot boundary).
export function stampDaemonReceipt(event: WorkspaceEvent, nowMs: number): WorkspaceEvent {
  if (event.type === "daemon.created" || event.type === "daemon.updated" || event.type === "daemon.deleted") {
    return { ...event, data: { ...(event.data as Daemon), receivedAtMs: nowMs } };
  }
  if (event.type === "workspace.snapshot") {
    const data = event.data as Partial<WorkspaceState> & { daemons?: Daemon[] };
    if (data.daemons) {
      return { ...event, data: { ...data, daemons: data.daemons.map((daemon) => ({ ...daemon, receivedAtMs: nowMs })) } };
    }
  }
  return event;
}

export type AgentDisplayStatusKey = "running" | "queued" | "waiting-env" | "needs-review" | "failed" | "idle";

export type AgentDisplayStatus = {
  key: AgentDisplayStatusKey;
  tone: AgentDisplayStatusKey;
  label: string;
  detailLabel?: string;
  title: string;
  action?: string;
  reason?: string;
  run?: AgentRun;
  event?: AgentEvent;
};

const terminalAgentRunStatuses = new Set(["completed", "failed", "stopped", "cancelled", "canceled"]);

function runUpdatedAtMs(run: AgentRun) {
  const value = Date.parse(run.updatedAt);
  return Number.isNaN(value) ? 0 : value;
}

function currentAgentRun(agent: Agent, runs: AgentRun[]) {
  const exact = agent.currentRunId ? runs.find((run) => run.id === agent.currentRunId) : undefined;
  if (exact) {
    return exact;
  }
  const agentRuns = runs
    .filter((run) => run.agentId === agent.id)
    .sort((left, right) => runUpdatedAtMs(right) - runUpdatedAtMs(left));
  return agentRuns.find((run) => !terminalAgentRunStatuses.has(run.status)) ?? agentRuns[0];
}

function textOrUndefined(value?: string) {
  const text = (value ?? "").trim().replace(/\s+/g, " ");
  return text || undefined;
}

function runningAction(agent: Agent, run?: AgentRun) {
  const candidates = [run?.lastMessage, agent.currentActivity, agent.currentTask];
  for (const candidate of candidates) {
    const text = textOrUndefined(candidate);
    if (!text) {
      continue;
    }
    const normalized = text.toLowerCase();
    if (normalized === "running" || normalized === "working" || normalized === "queued" || normalized === "queued in daemon") {
      continue;
    }
    return text;
  }
  return "Working";
}

function failureReason(agent: Agent, run?: AgentRun) {
  for (const candidate of [run?.error, run?.lastMessage, agent.currentActivity]) {
    const text = textOrUndefined(candidate);
    if (!text || text.toLowerCase() === "failed") {
      continue;
    }
    return text;
  }
  return "No failure reason was provided.";
}

function pendingReviewEvent(agent: Agent, events: AgentEvent[]) {
  // Share the split with the Inbox (isNeedsReviewEvent): a for_me event only lights the
  // "Needs your review" chip if it is a real review, not a pure mention. Without the
  // isReliableMentionEvent guard a bare @mention would light Needs-review on the chip while
  // the Inbox (which drops mentions) counts nothing — B×D would disagree on the same event.
  return events.find((event) => event.agentId === agent.id && event.box === "for_me" && !isReliableMentionEvent(event) && event.status !== "completed" && event.status !== "dismissed");
}

function hasQueuedWork(agent: Agent, run?: AgentRun) {
  return !!run && !terminalAgentRunStatuses.has(run.status) && (run.status === "queued" || agent.status === "queued");
}

function status(key: AgentDisplayStatusKey, label: string, title = label, extra: Omit<AgentDisplayStatus, "key" | "tone" | "label" | "title"> = {}): AgentDisplayStatus {
  return { key, tone: key, label, title, ...extra };
}

export function agentDisplayStatus(
  agent: Agent,
  runs: AgentRun[],
  daemons?: Daemon[],
  events: AgentEvent[] = [],
  nowMs = Date.now(),
): AgentDisplayStatus {
  const daemon = daemons?.find((item) => item.id === agent.daemonId);
  const daemonState = daemon ? daemonLiveStatus(daemon, nowMs) : daemons ? "disconnected" : "";
  if (daemonState === "disconnected" || daemonState === "deleted") {
    return status("waiting-env", "Waiting for local environment", "Waiting for local environment", { run: currentAgentRun(agent, runs) });
  }

  const run = currentAgentRun(agent, runs);
  if (agent.status === "failed" || run?.status === "failed") {
    const reason = failureReason(agent, run);
    return status("failed", "Failed — view reason", `Failed — view reason: ${reason}`, { reason, run });
  }

  const reviewEvent = pendingReviewEvent(agent, events);
  if (reviewEvent) {
    return status("needs-review", "Needs your review", reviewEvent.summary ? `Needs your review: ${reviewEvent.summary}` : "Needs your review", { event: reviewEvent, run });
  }

  if (agent.status === "working" || run?.status === "running" || (run?.desiredStatus === "running" && run && !terminalAgentRunStatuses.has(run.status) && run.status !== "queued")) {
    const action = runningAction(agent, run);
    return status("running", `Running · ${action}`, `Running · ${action}`, { action, run });
  }

  if (hasQueuedWork(agent, run)) {
    return status("queued", "Queued", "Queued", { run });
  }

  return status("idle", "Idle", "Standing by", { detailLabel: "Standing by", run });
}

export function agentStatus(agent: Agent, runs: AgentRun[]) {
  return agentDisplayStatus(agent, runs).key;
}

export type InboxItemKind = "needs-review" | "failed";
export type InboxBadgeTone = InboxItemKind | "";

export type WorkspaceInboxItem = {
  id: string;
  kind: InboxItemKind;
  actorLabel: string;
  action: string;
  summary: string;
  documentId?: string;
  threadId?: string;
  threadMessageId?: string;
  agentId?: string;
  agentHandle?: string;
  occurredAt: string;
  unread: boolean;
  countable: boolean;
  reason?: string;
};

export type WorkspaceInboxSummary = {
  items: WorkspaceInboxItem[];
  counts: {
    needsReview: number;
    failed: number;
    total: number;
  };
  badgeTone: InboxBadgeTone;
};

const resolvedInboxStatuses = new Set(["completed", "dismissed"]);
const terminalInboxRunStatuses = new Set(["completed", "failed", "stopped", "cancelled", "canceled"]);

function inboxEventOpen(event: AgentEvent) {
  return !resolvedInboxStatuses.has((event.status || "").toLowerCase());
}

function inboxEventTime(event: AgentEvent) {
  return event.updatedAt || event.createdAt || "";
}

function inboxTimeValue(value: string) {
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? 0 : ms;
}

function inboxAgentLabel(handle: string | undefined) {
  return handle ? `@${handle}` : "Agent";
}

function inboxText(value?: string) {
  const text = (value ?? "").trim().replace(/\s+/g, " ");
  return text || "";
}

function inboxCurrentAgentRun(agent: Agent, runs: AgentRun[]) {
  const exact = agent.currentRunId ? runs.find((run) => run.id === agent.currentRunId) : undefined;
  if (exact) {
    return exact;
  }
  const agentRuns = runs
    .filter((run) => run.agentId === agent.id)
    .sort((left, right) => inboxTimeValue(right.updatedAt) - inboxTimeValue(left.updatedAt));
  return agentRuns.find((run) => !terminalInboxRunStatuses.has((run.status || "").toLowerCase())) ?? agentRuns[0];
}

function inboxFailureReason(agent: Agent, run?: AgentRun) {
  for (const candidate of [run?.error, run?.lastMessage, agent.currentActivity]) {
    const text = inboxText(candidate);
    if (!text || text.toLowerCase() === "failed") {
      continue;
    }
    return text;
  }
  return "No failure reason was provided.";
}

function isReliableMentionEvent(event: AgentEvent) {
  return event.type === "thread.mentioned";
}

function isNeedsReviewEvent(event: AgentEvent) {
  return event.box === "for_me" && !isReliableMentionEvent(event) && inboxEventOpen(event);
}

// B×D resolved: the Inbox (below) and the Needs-review chip (agentDisplayStatus /
// pendingReviewEvent) share one split predicate — isReliableMentionEvent — so a for_me event
// lights both or neither, never one alone. That agreement is pinned by the "B×D split" test.
export function workspaceInboxSummary(
  workspace: Pick<WorkspaceState, "agents" | "agentRuns" | "agentEvents">,
): WorkspaceInboxSummary {
  const items: WorkspaceInboxItem[] = [];

  for (const event of workspace.agentEvents ?? []) {
    if (!inboxEventOpen(event)) {
      continue;
    }
    const occurredAt = inboxEventTime(event);
    if (isReliableMentionEvent(event)) {
      // Agent mention events are activity-only until human-targeted mention events exist.
      continue;
    }
    if (isNeedsReviewEvent(event)) {
      items.push({
        id: `review:${event.id}`,
        kind: "needs-review",
        actorLabel: inboxAgentLabel(event.agentHandle),
        action: "needs your review",
        summary: inboxText(event.summary) || inboxText(event.prompt) || "Review requested",
        documentId: event.documentId || undefined,
        threadId: event.threadId || undefined,
        threadMessageId: event.threadMessageId || undefined,
        agentId: event.agentId,
        agentHandle: event.agentHandle,
        occurredAt,
        unread: true,
        countable: true,
      });
    }
  }

  for (const agent of workspace.agents ?? []) {
    const run = inboxCurrentAgentRun(agent, workspace.agentRuns ?? []);
    if (agent.status !== "failed" && run?.status !== "failed") {
      continue;
    }
    const reason = inboxFailureReason(agent, run);
    items.push({
      id: `failed:${agent.id}:${run?.id ?? "agent"}`,
      kind: "failed",
      actorLabel: inboxAgentLabel(agent.handle),
      action: "failed",
      summary: reason,
      agentId: agent.id,
      agentHandle: agent.handle,
      occurredAt: run?.updatedAt || agent.updatedAt,
      unread: true,
      countable: true,
      reason,
    });
  }

  items.sort((left, right) => inboxTimeValue(right.occurredAt) - inboxTimeValue(left.occurredAt) || left.id.localeCompare(right.id));
  const needsReview = items.filter((item) => item.kind === "needs-review" && item.countable).length;
  const failed = items.filter((item) => item.kind === "failed" && item.countable).length;
  const total = needsReview + failed;
  return {
    items,
    counts: { needsReview, failed, total },
    badgeTone: failed ? "failed" : needsReview ? "needs-review" : "",
  };
}

export function coworkerCount(workspace: Pick<WorkspaceState, "agents" | "users">) {
  return workspace.agents.length + workspace.users.length;
}

// Matches the backend's 2-minute presence read cutoff (see PR #50), so the
// client-side ring window agrees with what the server would still serve.
export const PRESENCE_ONLINE_WINDOW_MS = 120_000;

// Phase E right-rail resize bounds. The rail clamps to [280, 520]px, and never
// so wide that the center document column drops below 380px — so a drag can't
// crush or overlap the document ("不塌、不错位"). The left rail is 248px open,
// 60px collapsed, which changes how much width the center still needs.
export const RAIL_RIGHT_MIN = 280;
export const RAIL_RIGHT_MAX = 520;
export const RAIL_CENTER_MIN = 380;
export const RAIL_LEFT_OPEN = 248;
export const RAIL_LEFT_COLLAPSED = 60;

export function clampRailWidth(desired: number, shellWidth: number, sidebarCollapsed: boolean): number {
  const leftWidth = sidebarCollapsed ? RAIL_LEFT_COLLAPSED : RAIL_LEFT_OPEN;
  const maxByCenter = shellWidth - leftWidth - RAIL_CENTER_MIN;
  return Math.max(RAIL_RIGHT_MIN, Math.min(Math.min(desired, RAIL_RIGHT_MAX), maxByCenter));
}

export type WorkspacePerson = {
  id: string;
  handle: string;
  name: string;
  kind: "you" | "agent" | "member";
  // updatedAt of this actor's most recent presence row (any document =
  // workspace-level). Whether it counts as *online* is a freshness decision
  // left to personOnline, so the ring decays instead of lying after the window.
  presentAt?: string;
};

// Everyone in the workspace — humans + agents — for the People panel. The online
// ring is workspace-level: presentAt is the actor's presence row (any document),
// and personOnline decays it on the same 2-minute window. Presence is never
// fabricated: no row means no ring.
export function workspacePeople(
  workspace: Pick<WorkspaceState, "currentUserId" | "agents" | "users" | "presences">,
): WorkspacePerson[] {
  const currentUserId = workspace.currentUserId;
  const presentAt = (id: string) => workspace.presences[id]?.updatedAt;

  const people: WorkspacePerson[] = [];
  for (const user of workspace.users) {
    if (user.kind !== "human") {
      continue;
    }
    people.push({
      id: user.id,
      handle: user.handle,
      name: user.name,
      kind: user.id === currentUserId ? "you" : "member",
      presentAt: presentAt(user.id),
    });
  }
  for (const agent of workspace.agents) {
    people.push({ id: agent.id, handle: agent.handle, name: agent.name, kind: "agent", presentAt: presentAt(agent.id) });
  }

  // Deterministic base order — You first, then by handle. The panel re-orders
  // online-first using a live freshness check (personOnline).
  const rank = (person: WorkspacePerson) => (person.kind === "you" ? 0 : 1);
  people.sort((a, b) => rank(a) - rank(b) || a.handle.localeCompare(b.handle));
  return people;
}

// Online only when the actor has a presence row still within the freshness
// window at nowMs — the same decay daemon liveness uses, so a closed laptop
// drops the ring instead of lying. Workspace-level: any document counts.
export function personOnline(person: Pick<WorkspacePerson, "presentAt">, nowMs: number): boolean {
  if (!person.presentAt) {
    return false;
  }
  const presentMs = Date.parse(person.presentAt);
  return !Number.isNaN(presentMs) && nowMs - presentMs <= PRESENCE_ONLINE_WINDOW_MS;
}

export type ActivityCategory = "human-edit" | "comment" | "agent-change" | "done" | "neutral";

// Map a Document Activity event to one of 2a-3's semantic categories from its
// type + actor. Only the known shapes get a color; anything else is neutral
// (no guessed color). No completion activity type exists yet, so the "done"
// category stays dormant until one does — we don't fabricate it.
export function activityCategory(type: string, actorType: string): ActivityCategory {
  if (type.startsWith("thread.")) {
    return "comment";
  }
  if (type.startsWith("document.")) {
    return actorType === "agent" ? "agent-change" : "human-edit";
  }
  if (type.startsWith("agent.")) {
    return "agent-change";
  }
  return "neutral";
}

// The current document's activity, newest first. Snapshot-fresh: reflects
// workspace.activities as of the last snapshot — there is no per-event live
// update yet (tracked separately), so the renderer must not imply live.
export function documentActivity(
  workspace: Pick<WorkspaceState, "activities">,
  documentId: string | undefined,
  limit = 12,
): ActivityEvent[] {
  if (!documentId) {
    return [];
  }
  return (workspace.activities ?? [])
    .filter((activity) => activity.documentId === documentId)
    .slice()
    .sort((a, b) => (a.occurredAt < b.occurredAt ? 1 : a.occurredAt > b.occurredAt ? -1 : 0))
    .slice(0, limit);
}

const SEVEN_DAYS_MS = 7 * 24 * 60 * 60 * 1000;

export function recentActivity(
  workspace: Pick<WorkspaceState, "activities">,
  documentId: string | undefined,
  nowMs: number,
): ActivityEvent[] {
  if (!documentId) return [];
  const cutoff = nowMs - SEVEN_DAYS_MS;
  return (workspace.activities ?? [])
    .filter((a) => a.documentId === documentId && Date.parse(a.occurredAt) >= cutoff)
    .slice()
    .sort((a, b) => (a.occurredAt < b.occurredAt ? 1 : a.occurredAt > b.occurredAt ? -1 : 0));
}

const relativeTimeFormat = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

// Human relative time for an ISO timestamp, e.g. "5 minutes ago". Empty for an
// unparseable timestamp so a bad value never renders a misleading "now".
export function relativeTime(iso: string, nowMs: number): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) {
    return "";
  }
  const diffSec = Math.round((ms - nowMs) / 1000);
  if (Math.abs(diffSec) < 60) {
    return relativeTimeFormat.format(diffSec, "second");
  }
  const diffMin = Math.round(diffSec / 60);
  if (Math.abs(diffMin) < 60) {
    return relativeTimeFormat.format(diffMin, "minute");
  }
  const diffHour = Math.round(diffSec / 3600);
  if (Math.abs(diffHour) < 24) {
    return relativeTimeFormat.format(diffHour, "hour");
  }
  return relativeTimeFormat.format(Math.round(diffSec / 86400), "day");
}

export function threadReplyCount(thread: { messages: readonly unknown[] }) {
  return Math.max(0, thread.messages.length - 1);
}

export function threadReplyLabel(thread: { messages: readonly unknown[] }) {
  const count = threadReplyCount(thread);
  if (count === 0) {
    return "No replies";
  }
  return `${count} ${count === 1 ? "reply" : "replies"}`;
}

export function agentsByDaemon(agents: Agent[], daemons: Daemon[]) {
  const names = new Map(daemons.map((daemon) => [daemon.id, daemon.name]));
  return agents.reduce<Array<{ daemonId: string; daemonName: string; agents: Agent[] }>>((groups, agent) => {
    const daemonId = agent.daemonId || "unassigned";
    let group = groups.find((item) => item.daemonId === daemonId);
    if (!group) {
      group = { daemonId, daemonName: names.get(daemonId) ?? "Unassigned daemon", agents: [] };
      groups.push(group);
    }
    group.agents.push(agent);
    return groups;
  }, []);
}

export function reduceWorkspaceEvent(state: WorkspaceState, event: WorkspaceEvent): WorkspaceState {
  if (event.type === "workspace.snapshot") {
    const { documents: _documents, ...data } = event.data as Partial<WorkspaceState> & { documents?: unknown };
    // The backend marshals empty collections as JSON null (Go nil slices) — e.g. an
    // idle workspace's presences after the 2-minute freshness cutoff, or a workspace
    // with no activities. Coerce them to empty at this seam so no consumer (the People
    // online ring, Document Activity, …) dereferences null when switching workspaces.
    return {
      ...state,
      ...data,
      users: data.users ?? [],
      daemons: data.daemons ?? [],
      agents: data.agents ?? [],
      agentRuns: data.agentRuns ?? [],
      threads: data.threads ?? [],
      agentEvents: data.agentEvents ?? [],
      presences: data.presences ?? {},
      activities: data.activities ?? [],
    };
  }
  if (event.type === "activity.created") {
    const activities = Array.isArray(state.activities) ? state.activities : [];
    // TODO: runtime-validate envelope payloads (unvalidated WS-payload casts are a class, not a one-off)
    return { ...state, activities: [event.data as ActivityEvent, ...activities].slice(0, 50) };
  }
  if (event.type === "thread.created" || event.type === "thread.updated") {
    return { ...state, threads: upsertById(state.threads, event.data as ThreadItem) };
  }
  if (event.type === "thread.message.created") {
    const message = event.data as ThreadItem["messages"][number];
    return {
      ...state,
      threads: state.threads.map((thread) =>
        thread.id === message.threadId && !thread.messages.some((current) => current.id === message.id)
          ? { ...thread, messages: [...thread.messages, message], updatedAt: message.createdAt }
          : thread
      ),
    };
  }
  if (event.type === "daemon.created" || event.type === "daemon.updated" || event.type === "daemon.deleted") {
    return { ...state, daemons: upsertById(state.daemons, event.data as Daemon) };
  }
  if (event.type === "agent.created" || event.type === "agent.updated") {
    return { ...state, agents: upsertById(state.agents, event.data as Agent) };
  }
  if (event.type === "agent.deleted") {
    const agent = event.data as Agent;
    return { ...state, agents: state.agents.filter((item) => item.id !== agent.id) };
  }
  if (event.type === "agent.run.updated") {
    return { ...state, agentRuns: upsertById(state.agentRuns, event.data as AgentRun) };
  }
  if (event.type === "agent.event.updated") {
    return { ...state, agentEvents: upsertById(state.agentEvents, event.data as WorkspaceState["agentEvents"][number]) };
  }
  if (event.type === "presence.updated") {
    const presence = event.data as WorkspaceState["presences"][string];
    return { ...state, presences: { ...state.presences, [presence.actorId]: presence } };
  }
  return state;
}

export function resolveThreadAnchorLive(anchor: ThreadAnchor, ydoc: Y.Doc | null, content: string) {
  const fallback: ResolvedThreadAnchor = {
    ...anchor,
    start: 0,
    end: 0,
    line: 1,
    excerpt: (anchor.excerpt || "").slice(0, 140),
    resolved: anchor.kind === "document",
  };
  if (!ydoc || !anchor.relativeStart || !anchor.relativeEnd) {
    return fallback;
  }
  try {
    const startPosition = Y.createAbsolutePositionFromRelativePosition(decodeRelativePosition(anchor.relativeStart), ydoc);
    const endPosition = Y.createAbsolutePositionFromRelativePosition(decodeRelativePosition(anchor.relativeEnd), ydoc);
    if (!startPosition || !endPosition) {
      return fallback;
    }
    const start = Math.max(0, startPosition.index);
    const end = Math.max(start, endPosition.index);
    const lineStarts = lineStartsForText(content);
    const previewEnd = end === start ? Math.min(content.length, start + 80) : end;
    return {
      ...anchor,
      start,
      end,
      line: lineForOffset(lineStarts, start),
      excerpt: (content.slice(start, previewEnd).trim() || anchor.excerpt || "").slice(0, 140),
      resolved: true,
    };
  } catch {
    return fallback;
  }
}

export function encodeRelativeAnchor(text: Y.Text, index: number) {
  const safeIndex = Math.max(0, Math.min(index, text.length));
  const assoc = safeIndex >= text.length ? -1 : 0;
  return uint8ArrayToBase64(Y.encodeRelativePosition(Y.createRelativePositionFromTypeIndex(text, safeIndex, assoc)));
}

function decodeRelativePosition(value: string) {
  return Y.decodeRelativePosition(base64ToUint8Array(value));
}

function upsertById<T extends { id: string }>(items: T[], next: T) {
  const seen = new Set<string>();
  const result = items.map((item) => {
    if (item.id !== next.id) {
      return item;
    }
    seen.add(item.id);
    return next;
  });
  if (!seen.has(next.id)) {
    result.push(next);
  }
  return result;
}

export function base64ToUint8Array(value: string) {
  const binary = window.atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

export function uint8ArrayToBase64(bytes: Uint8Array) {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return window.btoa(binary);
}
