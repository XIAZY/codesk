// @vitest-environment jsdom

import { describe, expect, it } from "vitest";
import * as Y from "yjs";
import {
  agentDisplayStatus,
  agentStatus,
  agentsByDaemon,
  applyReplaceToYText,
  buildDaemonInstallCommand,
  buildDaemonReinstallCommand,
  buildDaemonUninstallCommand,
  buildLineThreads,
  computeReplace,
  coworkerCount,
  workspacePeople,
  personOnline,
  documentActivity,
  recentActivity,
  activityCategory,
  relativeTime,
  daemonStatus,
  daemonLiveStatus,
  stampDaemonReceipt,
  DAEMON_ONLINE_WINDOW_MS,
  DAEMON_STALE_WINDOW_MS,
  encodeRelativeAnchor,
  handleMaxLength,
  identifierFromName,
  identifierPattern,
  lineForOffset,
  lineStartsForText,
  randomWorkspaceName,
  reduceWorkspaceEvent,
  workspaceNameAdjectives,
  workspaceNameNouns,
  workspaceSlugMaxLength,
  resolveThreadAnchorLive,
  selectionLabel,
  threadReplyCount,
  threadReplyLabel,
} from "./logic";
import { daemonFixtures, withReceipt } from "./daemonFixtures";
import type { ActivityEvent, Agent, AgentEvent, AgentRun, Daemon, PresenceItem, UserItem, WorkspaceState } from "./types";

function baseWorkspace(): WorkspaceState {
  return {
    workspaceId: "ws",
    rootDocumentId: "doc_root_ws",
    name: "Workspace",
    users: [],
    daemons: [],
    agents: [],
    agentRuns: [],
    threads: [],
    agentEvents: [],
    presences: {},
  };
}

describe("Yjs editor helpers", () => {
  it("applies keystroke-sized inserts and backspaces through Y.Text", () => {
    const doc = new Y.Doc();
    const text = doc.getText("content");
    let visible = "";
    for (const next of ["a", "ab", "abc", "ab", "ab!"]) {
      const replace = computeReplace(visible, next);
      doc.transact(() => applyReplaceToYText(text, replace));
      visible = next;
      expect(text.toString()).toBe(next);
    }
  });

  it("keeps relative anchors stable after edits before the anchor", () => {
    const doc = new Y.Doc();
    const text = doc.getText("content");
    doc.transact(() => text.insert(0, "alpha bravo charlie"));
    const relativeStart = encodeRelativeAnchor(text, 6);
    const relativeEnd = encodeRelativeAnchor(text, 11);
    doc.transact(() => text.insert(0, "intro "));

    const anchor = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "bravo" },
      doc,
      text.toString()
    );

    expect(anchor.start).toBe(12);
    expect(anchor.end).toBe(17);
    expect(anchor.line).toBe(1);
    expect(anchor.excerpt).toBe("bravo");
    expect(anchor.resolved).toBe(true);
  });
});

describe("randomWorkspaceName", () => {
  it("returns the first adjective and noun when the RNG yields 0", () => {
    expect(randomWorkspaceName(() => 0)).toBe(`${workspaceNameAdjectives[0]} ${workspaceNameNouns[0]}`);
  });

  it("returns the last adjective and noun when the RNG yields a value near 1", () => {
    const last = workspaceNameAdjectives.length - 1;
    const lastNoun = workspaceNameNouns.length - 1;
    expect(randomWorkspaceName(() => 0.999999)).toBe(`${workspaceNameAdjectives[last]} ${workspaceNameNouns[lastNoun]}`);
  });

  it("produces two Capitalized words separated by one space", () => {
    const name = randomWorkspaceName(() => 0.5);
    expect(name).toMatch(/^[A-Z][a-z]+ [A-Z][a-z]+$/);
  });

  it("survives identifierFromName to a non-empty, pattern-valid slug for every combination", () => {
    const slugRe = new RegExp(`^${identifierPattern}$`);
    for (let a = 0; a < workspaceNameAdjectives.length; a++) {
      for (let n = 0; n < workspaceNameNouns.length; n++) {
        // Drive the RNG to select adjective a, then noun n.
        const values = [a / workspaceNameAdjectives.length, n / workspaceNameNouns.length];
        let call = 0;
        const rng = () => values[call++];
        const name = randomWorkspaceName(rng);
        expect(name).toBe(`${workspaceNameAdjectives[a]} ${workspaceNameNouns[n]}`);
        const slug = identifierFromName(name, workspaceSlugMaxLength);
        expect(slug.length).toBeGreaterThan(0);
        expect(slug).toMatch(slugRe);
      }
    }
  });
});

describe("workspace reduction", () => {
  it("derives lowercase identifier suggestions without preserving invalid characters", () => {
    expect(identifierFromName("Product Workspace!", 64)).toBe("product-workspace");
    expect(identifierFromName("Mira Editor", handleMaxLength)).toBe("mira-editor");
    expect(identifierFromName("  Already_valid-01  ", 64)).toBe("already_valid-01");
  });

  it("does not keep document namespace state from workspace events", () => {
    const state = reduceWorkspaceEvent(baseWorkspace(), {
      type: "workspace.snapshot",
      data: {
        documents: [{ id: "doc", path: "legacy/spec.md", title: "legacy" }],
        threads: [],
      },
    });
    const next = reduceWorkspaceEvent(state, {
      type: "document.updated",
      data: { documentId: "doc", path: "docs/spec.md", title: "spec.md", updateId: 42, updatedAt: "now" },
    });

    expect("documents" in state).toBe(false);
    expect("documents" in next).toBe(false);
  });

  it("prepends live activity.created events while keeping the activity window bounded", () => {
    const activityEvent = (over: Partial<ActivityEvent>): ActivityEvent => ({
      type: "document.updated", actorId: "actor", actorType: "human", summary: "", occurredAt: "now", ...over,
    });
    const existing = Array.from({ length: 50 }, (_, index) => activityEvent({ type: `old-${index}` }));
    const nextActivity = activityEvent({ type: "document.created", summary: "created" });
    const next = reduceWorkspaceEvent({ ...baseWorkspace(), activities: existing }, {
      type: "activity.created",
      data: nextActivity,
    });

    expect(next.activities).toHaveLength(50);
    expect(next.activities?.[0]).toBe(nextActivity);
    expect(next.activities?.[49]).toBe(existing[48]);
  });

  it("coerces a snapshot's null collections to empty so consumers never deref null", () => {
    // Idle-workspace snapshot: the backend marshals empty Go slices as JSON null.
    const next = reduceWorkspaceEvent(baseWorkspace(), {
      type: "workspace.snapshot",
      data: { presences: null, activities: null, users: null, agents: null, threads: null, agentEvents: null },
    });
    expect(next.presences).toEqual({});
    expect(next.activities).toEqual([]);
    expect(next.users).toEqual([]);
    expect(next.agents).toEqual([]);
    // The People online ring + Document Activity must not throw on this shape (the switch crash).
    expect(() => workspacePeople(next)).not.toThrow();
    expect(() => documentActivity(next, "doc1")).not.toThrow();
  });

  it("attaches thread messages without duplicating repeated events", () => {
    const state = {
      ...baseWorkspace(),
      threads: [
        {
          id: "thread",
          documentId: "doc",
          title: "Question",
          status: "open",
          anchor: { kind: "text-range", relativeStart: "start", relativeEnd: "end", excerpt: "a" },
          participantIds: [],
          participantHandles: [],
          messages: [],
          createdAt: "now",
          updatedAt: "now",
        },
      ],
    };
    const event = {
      type: "thread.message.created",
      data: { id: "msg", threadId: "thread", authorId: "user", authorType: "human", authorHandle: "owner", authorName: "Owner", body: "hello", kind: "comment", createdAt: "later" },
    };

    const once = reduceWorkspaceEvent(state, event);
    const twice = reduceWorkspaceEvent(once, event);

    expect(twice.threads[0].messages).toHaveLength(1);
  });

  it("applies live daemon.updated events so status reflects a check-in without a refresh", () => {
    // A disconnected daemon (dead fixture) checks in and comes online (justSeen fixture), sharing an id.
    const before: Daemon = { ...daemonFixtures.dead, id: "daemon_1" };
    const after: Daemon = { ...daemonFixtures.justSeen, id: "daemon_1" };
    const state: WorkspaceState = { ...baseWorkspace(), daemons: [before] };

    const next = reduceWorkspaceEvent(state, { type: "daemon.updated", data: after });

    expect(next.daemons).toHaveLength(1);
    expect(daemonStatus(next.daemons[0])).toBe("online");
  });
});

describe("presentation grouping", () => {
  it("counts replies after the starter message", () => {
    expect(threadReplyCount({ messages: [] })).toBe(0);
    expect(threadReplyCount({ messages: [{ id: "starter" }] })).toBe(0);
    expect(threadReplyCount({ messages: [{ id: "starter" }, { id: "reply" }] })).toBe(1);
  });

  it("labels thread replies without calling a starter message a reply", () => {
    expect(threadReplyLabel({ messages: [] })).toBe("No replies");
    expect(threadReplyLabel({ messages: [{ id: "starter" }] })).toBe("No replies");
    expect(threadReplyLabel({ messages: [{ id: "starter" }, { id: "reply" }] })).toBe("1 reply");
    expect(threadReplyLabel({ messages: [{ id: "starter" }, { id: "reply" }, { id: "reply2" }] })).toBe("2 replies");
  });

  it("groups all threads on a line so the UI can show all of them", () => {
    const groups = buildLineThreads([
      { id: "b", anchor: { line: 3 } },
      { id: "a", anchor: { line: 3 } },
      { id: "c", anchor: { line: 9 } },
    ]);
    expect(groups.map((group) => [group.line, group.threads.map((thread) => thread.id)])).toEqual([
      [3, ["b", "a"]],
      [9, ["c"]],
    ]);
  });

  it("maps offsets to lines for thread metadata", () => {
    const starts = lineStartsForText("one\ntwo\nthree");
    expect(lineForOffset(starts, 0)).toBe(1);
    expect(lineForOffset(starts, 4)).toBe(2);
    expect(lineForOffset(starts, 8)).toBe(3);
  });

  it("labels full-line selections without leaking into a phantom next line", () => {
    expect(selectionLabel(0, "# Untitled".length, lineStartsForText("# Untitled"))).toBe("Selection on line 1");
    expect(selectionLabel(0, 5, lineStartsForText("one\ntwo"))).toBe("Selection across lines 1-2");
  });

  it("summarizes daemon and agent status from backend state", () => {
    const daemon: Daemon = { ...daemonFixtures.stale, id: "daemon", name: "Local" };
    const agent: Agent = {
      id: "agent",
      daemonId: "daemon",
      handle: "codex",
      name: "Codex",
      role: "Review",
      kind: "codex",
      workspaceRoot: "agents/agent",
      status: "idle",
      currentTask: "",
      currentActivity: "",
      currentRunId: "run",
      updatedAt: "now",
    };

    expect(daemonStatus(daemon)).toBe("stale");
    expect(agentStatus(agent, [{ id: "run", agentId: "agent", agentHandle: "codex", agentName: "Codex", agentKind: "codex", workspaceRoot: "", workingDirectory: "", prompt: "", status: "running", desiredStatus: "running", updatedAt: "now" }])).toBe("running");
    expect(agentsByDaemon([agent], [daemon])[0].daemonName).toBe("Local");
  });

  it("derives the ratified agent status vocabulary and priority ladder", () => {
    const nowMs = 1_000_000;
    const onlineDaemon = withReceipt({ ...daemonFixtures.justSeen, id: "daemon" }, nowMs);
    const agent: Agent = {
      id: "agent",
      daemonId: "daemon",
      handle: "codex",
      name: "Codex",
      role: "Review",
      kind: "codex",
      workspaceRoot: "agents/agent",
      status: "idle",
      currentTask: "",
      currentActivity: "",
      currentRunId: "run",
      updatedAt: "now",
    };
    const run = (status: string, extra: Partial<AgentRun> = {}): AgentRun => ({
      id: "run",
      agentId: "agent",
      agentHandle: "codex",
      agentName: "Codex",
      agentKind: "codex",
      workspaceRoot: "",
      workingDirectory: "",
      prompt: "",
      status,
      desiredStatus: status === "completed" ? "completed" : "running",
      updatedAt: "2026-01-01T00:00:00Z",
      ...extra,
    });
    const review: AgentEvent = {
      id: "event",
      agentId: "agent",
      agentHandle: "codex",
      type: "review.requested",
      box: "for_me",
      status: "pending",
      summary: "Review proposed changes",
      createdAt: "2026-01-01T00:00:00Z",
      updatedAt: "2026-01-01T00:00:00Z",
    };

    expect(agentDisplayStatus(agent, [run("running", { lastMessage: "editing sidebar" })], [onlineDaemon], [], nowMs)).toMatchObject({
      key: "running",
      label: "Running · editing sidebar",
    });
    expect(agentDisplayStatus(agent, [run("queued")], [onlineDaemon], [], nowMs)).toMatchObject({ key: "queued", label: "Queued" });
    expect(agentDisplayStatus(agent, [], [onlineDaemon], [], nowMs)).toMatchObject({
      key: "idle",
      label: "Idle",
      detailLabel: "Standing by",
      title: "Standing by",
    });
    expect(agentDisplayStatus(agent, [run("completed")], [onlineDaemon], [], nowMs)).toMatchObject({
      key: "idle",
      label: "Idle",
      detailLabel: "Standing by",
    });
    expect(agentDisplayStatus(agent, [run("completed")], [onlineDaemon], [review], nowMs)).toMatchObject({ key: "needs-review", label: "Needs your review" });
    expect(agentDisplayStatus(agent, [run("failed", { error: "tool exited 1" })], [onlineDaemon], [review], nowMs)).toMatchObject({
      key: "failed",
      label: "Failed — view reason",
      reason: "tool exited 1",
    });

    const deadDaemon = withReceipt({ ...daemonFixtures.dead, id: "daemon" }, nowMs);
    expect(agentDisplayStatus(agent, [run("failed", { error: "tool exited 1" })], [deadDaemon], [review], nowMs)).toMatchObject({
      key: "waiting-env",
      label: "Waiting for local environment",
    });

    const deletedDaemon = { ...daemonFixtures.softDeleted, id: "daemon" };
    expect(agentDisplayStatus(agent, [run("queued")], [deletedDaemon], [], nowMs)).toMatchObject({ key: "waiting-env" });
  });

  it("keeps stale-but-not-dead daemon agents on their last running state until liveness decays", () => {
    const receivedAtMs = 10_000;
    const daemon = withReceipt({ ...daemonFixtures.justSeen, id: "daemon", lastSeenAgeSeconds: 0 }, receivedAtMs);
    const agent: Agent = {
      id: "agent",
      daemonId: "daemon",
      handle: "codex",
      name: "Codex",
      role: "Review",
      kind: "codex",
      workspaceRoot: "agents/agent",
      status: "idle",
      currentTask: "",
      currentActivity: "updating tests",
      currentRunId: "run",
      updatedAt: "now",
    };
    const runningRun: AgentRun = {
      id: "run",
      agentId: "agent",
      agentHandle: "codex",
      agentName: "Codex",
      agentKind: "codex",
      workspaceRoot: "",
      workingDirectory: "",
      prompt: "",
      status: "running",
      desiredStatus: "running",
      updatedAt: "2026-01-01T00:00:00Z",
    };

    expect(agentDisplayStatus(agent, [runningRun], [daemon], [], receivedAtMs + DAEMON_STALE_WINDOW_MS).key).toBe("running");
    expect(agentDisplayStatus(agent, [runningRun], [daemon], [], receivedAtMs + DAEMON_STALE_WINDOW_MS + 1).key).toBe("waiting-env");
  });

  it("counts humans and agents as coworkers", () => {
    const workspace = {
      ...baseWorkspace(),
      users: [{ id: "user_1", handle: "ada", name: "Ada", role: "", kind: "human", status: "active", updatedAt: "now" }],
      agents: [
        {
          id: "agent_1",
          daemonId: "daemon_1",
          handle: "codex",
          name: "Codex",
          role: "Review",
          kind: "codex",
          workspaceRoot: "agents/agent_1",
          status: "idle",
          currentTask: "",
          currentActivity: "",
          currentRunId: "",
          updatedAt: "now",
        },
        {
          id: "agent_2",
          daemonId: "daemon_1",
          handle: "scribe",
          name: "Scribe",
          role: "Draft",
          kind: "codex",
          workspaceRoot: "agents/agent_2",
          status: "disconnected",
          currentTask: "",
          currentActivity: "",
          currentRunId: "",
          updatedAt: "now",
        },
      ],
    };

    expect(coworkerCount(workspace)).toBe(3);
  });
});

describe("workspacePeople", () => {
  const ws = (over: Partial<WorkspaceState>): WorkspaceState => ({ ...baseWorkspace(), ...over });
  const user = (id: string, handle: string, kind = "human"): UserItem => ({ id, handle, name: handle, role: "", kind, status: "", updatedAt: "now" });
  const agent = (id: string, handle: string): Agent => ({
    id, daemonId: "d1", handle, name: handle, role: "", kind: "agent", workspaceRoot: "",
    status: "idle", currentTask: "", currentActivity: "", currentRunId: "", updatedAt: "now",
  });
  const presence = (actorId: string, documentId: string | undefined): PresenceItem => ({
    actorId, actorType: "human", documentId, activity: "editing", updatedAt: "now",
  });

  it("lists all workspace humans + agents with a workspace-level presence ring", () => {
    const workspace = ws({
      currentUserId: "u_me",
      users: [user("u_me", "me"), user("u_alice", "alice"), user("d_bot", "bot", "daemon")],
      agents: [agent("a_writer", "writer")],
      presences: { u_alice: presence("u_alice", "docX") }, // present in some doc -> workspace-level ring
    });

    const result = workspacePeople(workspace);

    // all humans (me, alice) + the agent; the non-human user (bot) is filtered out
    expect(result.map((p) => p.id)).toEqual(["u_me", "u_alice", "a_writer"]);
    expect(result.map((p) => p.kind)).toEqual(["you", "member", "agent"]);
    // presentAt is any-document presence (workspace-level), not scoped to a current doc
    expect(result.find((p) => p.id === "u_alice")?.presentAt).toBe("now");
    expect(result.find((p) => p.id === "u_me")?.presentAt).toBeUndefined();
  });

  it("personOnline shows the ring only for fresh presence and decays after the window", () => {
    const nowMs = Date.parse("2026-07-06T12:00:00Z");
    const iso = (offsetMs: number) => new Date(nowMs - offsetMs).toISOString();
    expect(personOnline({ presentAt: iso(60_000) }, nowMs)).toBe(true); // 1 min ago -> online
    expect(personOnline({ presentAt: iso(3 * 60_000) }, nowMs)).toBe(false); // 3 min ago -> decayed
    expect(personOnline({ presentAt: undefined }, nowMs)).toBe(false);
    expect(personOnline({ presentAt: "not-a-date" }, nowMs)).toBe(false);
  });
});

describe("documentActivity", () => {
  const activity = (over: Partial<ActivityEvent>): ActivityEvent => ({
    type: "document.updated", documentId: "doc1", actorId: "u1", actorType: "human",
    summary: "", occurredAt: "2026-07-06T12:00:00Z", ...over,
  });

  it("scopes to the current document, newest first, and empties with no document", () => {
    const activities = [
      activity({ documentId: "doc1", occurredAt: "2026-07-06T12:00:00Z", summary: "a" }),
      activity({ documentId: "doc2", occurredAt: "2026-07-06T13:00:00Z", summary: "b" }),
      activity({ documentId: "doc1", occurredAt: "2026-07-06T14:00:00Z", summary: "c" }),
    ];
    expect(documentActivity({ activities }, "doc1").map((a) => a.summary)).toEqual(["c", "a"]);
    expect(documentActivity({ activities }, undefined)).toEqual([]);
    expect(documentActivity({ activities: undefined }, "doc1")).toEqual([]);
  });

  it("classifies categories from type + actor, unknown → neutral, no fabricated done", () => {
    expect(activityCategory("document.updated", "human")).toBe("human-edit");
    expect(activityCategory("document.created", "agent")).toBe("agent-change");
    expect(activityCategory("thread.created", "human")).toBe("comment");
    expect(activityCategory("thread.replied", "agent")).toBe("comment");
    expect(activityCategory("agent.run.updated", "agent")).toBe("agent-change");
    expect(activityCategory("workspace.snapshot", "human")).toBe("neutral");
  });

  it("relativeTime formats past timestamps and guards bad input", () => {
    const nowMs = Date.parse("2026-07-06T12:00:00Z");
    expect(relativeTime("2026-07-06T11:59:30Z", nowMs)).toContain("second");
    expect(relativeTime("2026-07-06T11:30:00Z", nowMs)).toContain("minute");
    expect(relativeTime("2026-07-06T09:00:00Z", nowMs)).toContain("hour");
    expect(relativeTime("2026-07-04T12:00:00Z", nowMs)).toContain("day");
    expect(relativeTime("not-a-date", nowMs)).toBe("");
  });
});

describe("recentActivity", () => {
  const nowMs = Date.parse("2026-07-06T12:00:00Z");
  const activity = (over: Partial<ActivityEvent>): ActivityEvent => ({
    type: "document.updated", documentId: "doc1", actorId: "u1", actorType: "human",
    summary: "", occurredAt: "2026-07-06T12:00:00Z", ...over,
  });

  it("returns only events within the 7-day window for the given document", () => {
    const activities = [
      activity({ occurredAt: "2026-07-06T10:00:00Z", summary: "recent" }),
      activity({ occurredAt: "2026-06-28T12:00:00Z", summary: "old" }),
      activity({ documentId: "doc2", occurredAt: "2026-07-06T11:00:00Z", summary: "other-doc" }),
    ];
    const result = recentActivity({ activities }, "doc1", nowMs);
    expect(result.map((a) => a.summary)).toEqual(["recent"]);
  });

  it("returns the full windowed list newest first (render slices to 5)", () => {
    const activities = Array.from({ length: 8 }, (_, i) =>
      activity({ occurredAt: `2026-07-06T0${i}:00:00Z`, summary: `e${i}` }),
    );
    const result = recentActivity({ activities }, "doc1", nowMs);
    expect(result).toHaveLength(8);
    expect(result[0].summary).toBe("e7");
  });

  it("tolerates null/undefined activities and undefined documentId", () => {
    expect(recentActivity({ activities: undefined }, "doc1", nowMs)).toEqual([]);
    expect(recentActivity({ activities: null as unknown as ActivityEvent[] }, "doc1", nowMs)).toEqual([]);
    expect(recentActivity({ activities: [] }, undefined, nowMs)).toEqual([]);
  });
});

describe("daemon liveness decay", () => {
  // Every daemon comes from the canonical fixtures (the real backend wire shape) stamped with a
  // fixed receipt time; each row probes daemonLiveStatus at receipt + elapsed. Table-driven over the
  // six lifecycle states × probe offsets so the boundary/age-folding semantics stay explicit.
  const RECEIPT = 1_000_000;

  type Row = { name: string; daemon: Daemon; elapsedMs: number; expected: string };
  const rows: Row[] = [
    // neverSeen must be "disconnected" at every probe — never a transient "online" (root of bug 1).
    { name: "neverSeen @ 0s", daemon: withReceipt(daemonFixtures.neverSeen, RECEIPT), elapsedMs: 0, expected: "disconnected" },
    { name: "neverSeen @ 10s", daemon: withReceipt(daemonFixtures.neverSeen, RECEIPT), elapsedMs: 10_000, expected: "disconnected" },
    { name: "neverSeen @ 31s", daemon: withReceipt(daemonFixtures.neverSeen, RECEIPT), elapsedMs: 31_000, expected: "disconnected" },
    { name: "neverSeen @ 3min", daemon: withReceipt(daemonFixtures.neverSeen, RECEIPT), elapsedMs: 180_000, expected: "disconnected" },
    // softDeleted is "deleted" at every probe (root of bug 2).
    { name: "softDeleted @ 0s", daemon: withReceipt(daemonFixtures.softDeleted, RECEIPT), elapsedMs: 0, expected: "deleted" },
    { name: "softDeleted @ 10s", daemon: withReceipt(daemonFixtures.softDeleted, RECEIPT), elapsedMs: 10_000, expected: "deleted" },
    { name: "softDeleted @ 31s", daemon: withReceipt(daemonFixtures.softDeleted, RECEIPT), elapsedMs: 31_000, expected: "deleted" },
    { name: "softDeleted @ 3min", daemon: withReceipt(daemonFixtures.softDeleted, RECEIPT), elapsedMs: 180_000, expected: "deleted" },
    // Boundary rows off justSeen (age 0) — <= semantics matching the backend windows.
    { name: "justSeen @ 0s → online", daemon: withReceipt(daemonFixtures.justSeen, RECEIPT), elapsedMs: 0, expected: "online" },
    { name: "justSeen @ exactly 30s → online", daemon: withReceipt(daemonFixtures.justSeen, RECEIPT), elapsedMs: DAEMON_ONLINE_WINDOW_MS, expected: "online" },
    { name: "justSeen @ 30.001s → stale", daemon: withReceipt(daemonFixtures.justSeen, RECEIPT), elapsedMs: DAEMON_ONLINE_WINDOW_MS + 1, expected: "stale" },
    { name: "justSeen @ exactly 2m → stale", daemon: withReceipt(daemonFixtures.justSeen, RECEIPT), elapsedMs: DAEMON_STALE_WINDOW_MS, expected: "stale" },
    { name: "justSeen @ 2m+1ms → disconnected", daemon: withReceipt(daemonFixtures.justSeen, RECEIPT), elapsedMs: DAEMON_STALE_WINDOW_MS + 1, expected: "disconnected" },
    // Age-folding: aging carries a server age of 25s; 6s of elapsed pushes the effective age to 31s → stale.
    { name: "aging @ 0s → online", daemon: withReceipt(daemonFixtures.aging, RECEIPT), elapsedMs: 0, expected: "online" },
    { name: "aging @ +6s (age 25+6=31s) → stale", daemon: withReceipt(daemonFixtures.aging, RECEIPT), elapsedMs: 6_000, expected: "stale" },
  ];

  it.each(rows)("$name", ({ daemon, elapsedMs, expected }) => {
    expect(daemonLiveStatus(daemon, RECEIPT + elapsedMs)).toBe(expected);
  });

  it("falls back to the server snapshot when age/receipt are absent", () => {
    // Strip the receipt/age off a genuine fixture so the null-guard branch is exercised; status is
    // then trusted verbatim from connectionStatus.
    const { lastSeenAgeSeconds: _age, receivedAtMs: _receipt, ...noReceipt } = daemonFixtures.stale;
    expect(daemonLiveStatus({ ...noReceipt, connectionStatus: "stale" }, 9e9)).toBe("stale");
    expect(daemonLiveStatus({ ...noReceipt, connectionStatus: undefined }, 9e9)).toBe("disconnected");
  });
});

describe("stampDaemonReceipt", () => {
  it("stamps the receipt time on daemon events and every daemon in a snapshot, and passes others through", () => {
    const updated = stampDaemonReceipt({ type: "daemon.updated", data: daemonFixtures.justSeen }, 42);
    expect((updated.data as Daemon).receivedAtMs).toBe(42);

    const snapshot = stampDaemonReceipt({ type: "workspace.snapshot", data: { daemons: [daemonFixtures.justSeen] } }, 99);
    expect((snapshot.data as { daemons: Daemon[] }).daemons[0].receivedAtMs).toBe(99);

    const other = { type: "agent.updated", data: { id: "a1" } } as const;
    expect(stampDaemonReceipt(other, 7)).toBe(other);
  });
});

describe("daemon install command", () => {
  it("builds a hosted installer command instead of a Docker Compose command", () => {
    const command = buildDaemonInstallCommand({
      backendUrl: "https://api.example.com/",
      workspaceId: "ws_123",
      daemonToken: "nottyd_abc",
      staticBaseUrl: "https://static.example.com/daemons/",
    });

    expect(command).toContain("curl -fsSL https://static.example.com/daemons/install.sh | sh -s --");
    expect(command).toContain("--backend-url https://api.example.com");
    expect(command).toContain("--workspace-id ws_123");
    expect(command).toContain("--daemon-token nottyd_abc");
    expect(command).toContain("--static-base https://static.example.com/daemons");
    expect(command).not.toContain("docker compose");
  });

  it("shell-quotes unsafe command values", () => {
    const command = buildDaemonInstallCommand({
      backendUrl: "https://api.example.com/notty prod",
      workspaceId: "ws bad'id",
      daemonToken: "nottyd token",
      staticBaseUrl: "https://static.example.com/daemons",
    });

    expect(command).toContain("--backend-url 'https://api.example.com/notty prod'");
    expect(command).toContain("--workspace-id 'ws bad'\\''id'");
    expect(command).toContain("--daemon-token 'nottyd token'");
  });

  it("derives static artifact base from backend URL when no static base is provided", () => {
    const command = buildDaemonInstallCommand({
      backendUrl: "https://notty.example.com/",
      workspaceId: "ws",
      daemonToken: "nottyd_token",
    });

    expect(command).toContain("curl -fsSL https://notty.example.com/daemons/install.sh | sh -s --");
    expect(command).toContain("--static-base https://notty.example.com/daemons");
  });
});

describe("daemon uninstall command", () => {
  it("builds a hosted global uninstaller command", () => {
    const command = buildDaemonUninstallCommand({
      staticBaseUrl: "https://static.example.com/daemons/",
    });

    expect(command).toContain("curl -fsSL https://static.example.com/daemons/uninstall.sh | sh -s --");
    expect(command).toContain("--all");
    expect(command).not.toContain("--daemon-token");
  });

  it("does not expose a workspace-scoped uninstall command", () => {
    const command = buildDaemonUninstallCommand({
      staticBaseUrl: "https://static.example.com/daemons",
    });

    expect(command).not.toContain("--workspace-id");
  });
});

describe("daemon reinstall command", () => {
  it("builds a reinstall script that reuses the daemon token", () => {
    const command = buildDaemonReinstallCommand({
      backendUrl: "https://notty.example.com/",
      workspaceId: "ws_123",
      daemonToken: "nottyd_abc",
      staticBaseUrl: "https://static.example.com/daemons/",
    });

    expect(command.startsWith("set -e\n")).toBe(true);
    expect(command).toContain("curl -fsSL https://static.example.com/daemons/uninstall.sh | sh -s -- \\");
    expect(command).toContain("--all");
    expect(command).toContain("curl -fsSL https://static.example.com/daemons/install.sh | sh -s -- \\");
    expect(command).toContain("--backend-url https://notty.example.com \\");
    expect(command).toContain("--workspace-id ws_123 \\");
    expect(command).toContain("--daemon-token nottyd_abc \\");
    expect(command).toContain("--static-base https://static.example.com/daemons");
  });

  it("shell-quotes unsafe reinstall values", () => {
    const command = buildDaemonReinstallCommand({
      backendUrl: "https://notty.example.com/app path",
      workspaceId: "ws bad'id",
      daemonToken: "nottyd token",
      staticBaseUrl: "https://static.example.com/daemon files",
    });

    expect(command).toContain("--backend-url 'https://notty.example.com/app path' \\");
    expect(command).toContain("--workspace-id 'ws bad'\\''id' \\");
    expect(command).toContain("--daemon-token 'nottyd token' \\");
    expect(command).toContain("--static-base 'https://static.example.com/daemon files'");
  });
});
