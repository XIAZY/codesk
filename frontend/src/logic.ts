import * as Y from "yjs";
import type { Agent, AgentRun, Daemon, ThreadAnchor, ThreadItem, WorkspaceEvent, WorkspaceState } from "./types";

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
    return { ...state, ...data };
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
  if (event.type === "daemon.created" || event.type === "daemon.deleted") {
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
