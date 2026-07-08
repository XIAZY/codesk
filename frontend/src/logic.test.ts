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
  clampRailWidth,
  computeReplace,
  coworkerCount,
  workspacePeople,
  personOnline,
  documentActivity,
  emptyWorkspace,
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
  workspaceInboxSummary,
} from "./logic";
import { daemonFixtures, withReceipt } from "./daemonFixtures";
import type { ActivityEvent, Agent, AgentEvent, AgentRun, Daemon, DocumentItem, PresenceItem, UserItem, WorkspaceState } from "./types";

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

  it("returns resolved=false for valid anchors against an empty ydoc (pre-sync reproduction)", () => {
    const populatedDoc = new Y.Doc();
    const populatedText = populatedDoc.getText("content");
    populatedDoc.transact(() => populatedText.insert(0, "hello world"));
    const relativeStart = encodeRelativeAnchor(populatedText, 0);
    const relativeEnd = encodeRelativeAnchor(populatedText, 5);

    const emptyDoc = new Y.Doc();
    emptyDoc.getText("content");
    const anchor = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello" },
      emptyDoc,
      "",
    );

    expect(anchor.resolved).toBe(false);
  });

  it("pattern 1: delete exact anchored range → orphaned (collapsed)", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    doc2.getText("content").delete(11, 11);
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(false);
  });

  it("pattern 2: delete paragraph containing range → orphaned (collapsed)", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    doc2.getText("content").delete(10, 18);
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(false);
  });

  it("pattern 4: select-all and type new content → orphaned (collapsed)", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    const t2 = doc2.getText("content");
    t2.delete(0, t2.length);
    t2.insert(0, "totally new document content");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(false);
  });

  it("pattern 5: delete range and insert new text at same position → orphaned (drifted)", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    const t2 = doc2.getText("content");
    t2.delete(11, 11);
    t2.insert(11, "replaced text");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(false);
  });

  it("pattern 6a: typo fix inside range → still anchored", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    const t2 = doc2.getText("content");
    t2.delete(11, 1);
    t2.insert(11, "H");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(true);
  });

  it("pattern 6b: append at end of range → still anchored", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    doc2.getText("content").insert(22, "!");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(true);
  });

  it("pattern 6c: insert at start of range → still anchored", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    doc2.getText("content").insert(11, "> ");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(true);
  });

  it("pattern 6d: insert punctuation mid-range (comma) → still anchored", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    doc2.getText("content").insert(16, ",");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(true);
  });

  it("pattern 6e: insert word mid-range → still anchored", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "first line\nhello world here\nthird line");
    const relativeStart = encodeRelativeAnchor(t1, 11);
    const relativeEnd = encodeRelativeAnchor(t1, 22);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    doc2.getText("content").insert(17, "beautiful ");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(true);
  });

  it("adversarial: punctuation-glued excerpt token still matches after edit", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "the end.");
    const relativeStart = encodeRelativeAnchor(t1, 0);
    const relativeEnd = encodeRelativeAnchor(t1, 8);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    const t2 = doc2.getText("content");
    t2.delete(7, 1);
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "the end." },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(true);
  });

  it("adversarial: short excerpt token does not false-match unrelated text", () => {
    const doc1 = new Y.Doc(); doc1.clientID = 1;
    const t1 = doc1.getText("content");
    t1.insert(0, "a cat sat on the mat");
    const relativeStart = encodeRelativeAnchor(t1, 0);
    const relativeEnd = encodeRelativeAnchor(t1, 5);

    const doc2 = new Y.Doc(); doc2.clientID = 2;
    Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1));
    const t2 = doc2.getText("content");
    t2.delete(0, 5);
    t2.insert(0, "irrelevant paragraph");
    Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "a cat" },
      doc1, t1.toString(),
    );
    expect(result.resolved).toBe(false);
  });

  it("collapsed case shows stored excerpt, not forward-slice", () => {
    const doc = new Y.Doc();
    const text = doc.getText("content");
    doc.transact(() => text.insert(0, "first line\nhello world here\nthird line"));
    const relativeStart = encodeRelativeAnchor(text, 11);
    const relativeEnd = encodeRelativeAnchor(text, 22);

    doc.transact(() => text.delete(11, 11));

    const result = resolveThreadAnchorLive(
      { kind: "text-range", relativeStart, relativeEnd, excerpt: "hello world" },
      doc, text.toString(),
    );
    expect(result.resolved).toBe(false);
    expect(result.excerpt).toBe("hello world");
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

  it("applies workspace.updated without dropping live collections", () => {
    const state: WorkspaceState = {
      ...baseWorkspace(),
      workspaceId: "ws",
      rootDocumentId: "root",
      name: "Old name",
      users: [{ id: "u1", handle: "owner", name: "Owner", role: "Owner", kind: "human", status: "active", updatedAt: "now" }],
      agents: [{ id: "a1", daemonId: "d1", handle: "codex", name: "Codex", role: "Reviewer", kind: "codex", workspaceRoot: "agents/a1", status: "idle", currentTask: "", currentActivity: "", currentRunId: "", updatedAt: "now" }],
    };

    const next = reduceWorkspaceEvent(state, {
      type: "workspace.updated",
      data: {
        id: "ws",
        name: "New name",
        slug: "new-name",
        defaultRuntime: "codex",
        updatedAt: "later",
      },
    });

    expect(next.name).toBe("New name");
    expect(next.slug).toBe("new-name");
    expect(next.defaultRuntime).toBe("codex");
    expect(next.updatedAt).toBe("later");
    expect(next.users).toEqual(state.users);
    expect(next.agents).toEqual(state.agents);
  });

  it("ignores workspace.updated events for another workspace", () => {
    const state: WorkspaceState = { ...baseWorkspace(), workspaceId: "ws", name: "Current" };
    const next = reduceWorkspaceEvent(state, {
      type: "workspace.updated",
      data: { id: "other", name: "Other", slug: "other" },
    });

    expect(next).toBe(state);
  });

  it("clears defaultRuntime when workspace.updated omits the omitempty field", () => {
    const state: WorkspaceState = { ...baseWorkspace(), workspaceId: "ws", name: "Current", defaultRuntime: "codex" };
    const next = reduceWorkspaceEvent(state, {
      type: "workspace.updated",
      data: { id: "ws", name: "Current", slug: "current" },
    });

    expect(next.defaultRuntime).toBe("");
  });

  it("empties state when this workspace is deleted", () => {
    const state: WorkspaceState = { ...baseWorkspace(), workspaceId: "ws", name: "Current", users: [{ id: "u1", handle: "owner", name: "Owner", role: "Owner", kind: "human", status: "active", updatedAt: "now" }] };
    const next = reduceWorkspaceEvent(state, {
      type: "workspace.deleted",
      data: { workspaceId: "ws" },
    });

    expect(next).toEqual(emptyWorkspace());
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

describe("workspaceInboxSummary", () => {
  const agent = (over: Partial<Agent> = {}): Agent => ({
    id: "agent_1",
    daemonId: "daemon_1",
    handle: "codex",
    name: "Codex",
    role: "Review",
    kind: "codex",
    workspaceRoot: "/tmp/codex",
    status: "idle",
    currentTask: "",
    currentActivity: "",
    currentRunId: "",
    updatedAt: "2026-07-06T12:00:00Z",
    ...over,
  });
  const run = (over: Partial<AgentRun> = {}): AgentRun => ({
    id: "run_1",
    agentId: "agent_1",
    agentHandle: "codex",
    agentName: "Codex",
    agentKind: "codex",
    workspaceRoot: "/tmp/codex",
    workingDirectory: "/tmp/codex",
    prompt: "Do it",
    status: "completed",
    desiredStatus: "completed",
    updatedAt: "2026-07-06T12:00:00Z",
    ...over,
  });
  const event = (over: Partial<AgentEvent> = {}): AgentEvent => ({
    id: "event_1",
    agentId: "agent_1",
    agentHandle: "codex",
    type: "document.updated",
    box: "for_me",
    status: "pending",
    documentId: "doc1",
    summary: "Review changes",
    createdAt: "2026-07-06T12:00:00Z",
    updatedAt: "2026-07-06T12:00:00Z",
    ...over,
  });
  const document = (over: Partial<DocumentItem> = {}): DocumentItem => ({
    id: "doc1",
    path: "docs/spec.md",
    title: "spec.md",
    ...over,
  });

  it("builds one event-derived source for reviews and failures while dropping mention activity", () => {
    const summary = workspaceInboxSummary({
      ...baseWorkspace(),
      agents: [agent()],
      agentRuns: [run({ status: "failed", error: "tool exited 1", updatedAt: "2026-07-06T12:03:00Z" })],
      agentEvents: [
        event({ id: "mention_1", type: "thread.mentioned", threadId: "thread_1", summary: "@codex was mentioned", updatedAt: "2026-07-06T12:04:00Z" }),
        event({ id: "review_1", type: "document.updated", summary: "Review the workspace plan", updatedAt: "2026-07-06T12:02:00Z" }),
      ],
    });

    expect(summary.counts).toEqual({ needsReview: 1, failed: 1, total: 2 });
    expect(summary.badgeTone).toBe("failed");
    expect(summary.items.map((item) => item.kind)).toEqual(["failed", "needs-review"]);
    expect(summary.items.some((item) => item.summary.includes("@codex"))).toBe(false);
    expect(summary.items.find((item) => item.kind === "failed")?.reason).toBe("tool exited 1");
  });

  it("ignores mention events without hiding unresolved review and failure work", () => {
    const summary = workspaceInboxSummary({
      ...baseWorkspace(),
      agents: [agent({ status: "failed", currentActivity: "daemon stopped" })],
      agentRuns: [],
      agentEvents: [
        event({ id: "mention_1", type: "thread.mentioned", summary: "@codex was mentioned", updatedAt: "2026-07-06T12:00:00Z" }),
        event({ id: "review_1", type: "document.updated", summary: "Review even if runtime moved on", updatedAt: "2026-07-06T12:01:00Z" }),
      ],
    });

    expect(summary.counts).toEqual({ needsReview: 1, failed: 1, total: 2 });
    expect(summary.items.some((item) => item.summary.includes("@codex"))).toBe(false);
    expect(summary.items.find((item) => item.kind === "needs-review")?.countable).toBe(true);
    expect(summary.badgeTone).toBe("failed");
  });

  it("ignores resolved events and never crashes on missing collections", () => {
    const summary = workspaceInboxSummary({
      ...baseWorkspace(),
      agentEvents: [event({ status: "completed" })],
    });

    expect(summary.items).toEqual([]);
    expect(summary.counts.total).toBe(0);
    expect(summary.badgeTone).toBe("");
  });

  it("drops review events whose document id cannot resolve", () => {
    const summary = workspaceInboxSummary({
      ...baseWorkspace(),
      documents: [document()],
      agentEvents: [
        event({ id: "review_1", documentId: "doc1", summary: "Review real doc" }),
        event({ id: "missing_doc", documentId: "missing", summary: "Review missing doc" }),
      ],
    });

    expect(summary.counts).toEqual({ needsReview: 1, failed: 0, total: 1 });
    expect(summary.items.map((item) => item.summary)).toEqual(["Review real doc"]);
  });

  it("drops stale review events while preserving current failed runtime state", () => {
    const nowMs = Date.parse("2026-07-06T12:00:00Z");
    const summary = workspaceInboxSummary({
      ...baseWorkspace(),
      documents: [document()],
      agents: [agent({ status: "failed", currentActivity: "daemon stopped", updatedAt: "2026-07-06T12:00:00Z" })],
      agentRuns: [],
      agentEvents: [
        event({ id: "stale_review", summary: "Old review", updatedAt: "2026-06-01T12:00:00Z" }),
        event({ id: "fresh_review", summary: "Fresh review", updatedAt: "2026-07-06T11:00:00Z" }),
      ],
    }, { nowMs });

    expect(summary.counts).toEqual({ needsReview: 1, failed: 1, total: 2 });
    expect(summary.items.some((item) => item.summary === "Old review")).toBe(false);
    expect(summary.items.some((item) => item.summary === "Fresh review")).toBe(true);
    expect(summary.items.some((item) => item.kind === "failed" && item.reason === "daemon stopped")).toBe(true);
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

describe("clampRailWidth", () => {
  const WIDE = 1600; // roomy shell — center-min never binds

  it("clamps to the 280–520px rail bounds", () => {
    expect(clampRailWidth(100, WIDE, false)).toBe(280);
    expect(clampRailWidth(400, WIDE, false)).toBe(400);
    expect(clampRailWidth(9999, WIDE, false)).toBe(520);
  });

  it("never lets the center column drop below 380px (open sidebar)", () => {
    // 1120 shell, 248 left, 380 center-min => right can be at most 492.
    expect(clampRailWidth(520, 1120, false)).toBe(492);
    // Center stays >= 380 at the clamped width.
    expect(1120 - 248 - clampRailWidth(520, 1120, false)).toBe(380);
  });

  it("gives back the width the collapsed rail frees to the center", () => {
    // Same 1120 shell but left is 60 when collapsed => right can reach its 520 max.
    expect(clampRailWidth(520, 1120, true)).toBe(520);
    expect(1120 - 60 - clampRailWidth(520, 1120, true)).toBeGreaterThanOrEqual(380);
  });
});

// B×D integration behavior (Eva/Bill design ruling): the Needs-review chip (B) and the Inbox
// (D) must agree on the same event. A pure @mention is activity, not attention — it lights
// neither. A review event lights both. Pinned here so the resolve can't silently regress.
describe("B×D split — mention vs review agreement (chip and Inbox)", () => {
  const splitAgent: Agent = {
    id: "agent_split", daemonId: "daemon_1", handle: "codex", name: "Codex", role: "Review",
    kind: "codex", workspaceRoot: "/tmp/codex", status: "idle", currentTask: "",
    currentActivity: "", currentRunId: "", updatedAt: "2026-07-06T12:00:00Z",
  };
  const splitEvent = (over: Partial<AgentEvent>): AgentEvent => ({
    id: "event_1", agentId: "agent_split", agentHandle: "codex", type: "document.updated",
    box: "for_me", status: "pending", documentId: "doc1", summary: "Review the plan",
    createdAt: "2026-07-06T12:00:00Z", updatedAt: "2026-07-06T12:00:00Z", ...over,
  });

  it("a pure @mention lights NEITHER the Needs-review chip NOR the Inbox count", () => {
    const mention = splitEvent({ type: "thread.mentioned", summary: "@codex was mentioned" });
    // Chip (B): a bare mention must not read as Needs review (daemons=undefined skips waiting-env).
    expect(agentDisplayStatus(splitAgent, [], undefined, [mention]).key).not.toBe("needs-review");
    // Inbox (D): mention-kind counts nowhere.
    expect(workspaceInboxSummary({ ...baseWorkspace(), agents: [splitAgent], agentEvents: [mention] }).counts.total).toBe(0);
  });

  it("a review event lights the Needs-review chip AND counts in the Inbox", () => {
    const review = splitEvent({ type: "document.updated", summary: "Review the workspace plan" });
    expect(agentDisplayStatus(splitAgent, [], undefined, [review]).key).toBe("needs-review");
    expect(workspaceInboxSummary({ ...baseWorkspace(), agents: [splitAgent], agentEvents: [review] }).counts.needsReview).toBe(1);
  });
});
