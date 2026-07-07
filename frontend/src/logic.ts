import * as Y from "yjs";
import type { ActivityEvent, Agent, AgentRun, Daemon, ThreadAnchor, ThreadItem, WorkspaceEvent, WorkspaceState } from "./types";

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

export function agentStatus(agent: Agent, runs: AgentRun[]) {
  const run = runs.find((item) => item.id === agent.currentRunId || item.agentId === agent.id);
  if (agent.status === "working" || (run && run.desiredStatus === "running" && !["completed", "failed", "stopped"].includes(run.status))) {
    return "working";
  }
  if (agent.status === "disconnected") {
    return "disconnected";
  }
  return "idle";
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
