// @vitest-environment jsdom

import { describe, expect, it } from "vitest";
import * as Y from "yjs";
import {
  agentStatus,
  agentsByDaemon,
  applyReplaceToYText,
  buildDaemonInstallCommand,
  buildDaemonReinstallCommand,
  buildDaemonUninstallCommand,
  buildLineThreads,
  computeReplace,
  coworkerCount,
  documentParticipants,
  participantOnline,
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
import type { Agent, AgentEvent, Daemon, PresenceItem, ThreadItem, UserItem, WorkspaceState } from "./types";

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
    expect(agentStatus(agent, [{ id: "run", agentId: "agent", agentHandle: "codex", agentName: "Codex", agentKind: "codex", workspaceRoot: "", workingDirectory: "", prompt: "", status: "running", desiredStatus: "running", updatedAt: "now" }])).toBe("working");
    expect(agentsByDaemon([agent], [daemon])[0].daemonName).toBe("Local");
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

describe("documentParticipants", () => {
  const ws = (over: Partial<WorkspaceState>): WorkspaceState => ({ ...baseWorkspace(), ...over });
  const user = (id: string, handle: string): UserItem => ({ id, handle, name: handle, role: "", kind: "human", status: "", updatedAt: "now" });
  const agent = (id: string, handle: string): Agent => ({
    id, daemonId: "d1", handle, name: handle, role: "", kind: "agent", workspaceRoot: "",
    status: "idle", currentTask: "", currentActivity: "", currentRunId: "", updatedAt: "now",
  });
  const thread = (documentId: string, participantIds: string[]): ThreadItem => ({
    id: `t_${documentId}_${participantIds.join("_")}`, documentId, title: "", status: "open",
    anchor: { kind: "line" }, participantIds, participantHandles: [], messages: [], createdAt: "now", updatedAt: "now",
  });
  const event = (agentId: string, documentId: string): AgentEvent => ({
    id: `e_${agentId}_${documentId}`, agentId, agentHandle: agentId, type: "change", status: "done",
    summary: "", documentId, createdAt: "now", updatedAt: "now",
  });
  const presence = (actorId: string, documentId: string | undefined): PresenceItem => ({
    actorId, actorType: "human", documentId, activity: "editing", updatedAt: "now",
  });

  it("builds the durable current-document set with a doc-level online ring", () => {
    const workspace = ws({
      currentUserId: "u_me",
      users: [user("u_me", "me"), user("u_alice", "alice"), user("u_bob", "bob")],
      agents: [agent("a_writer", "writer"), agent("a_other", "other")],
      threads: [thread("doc1", ["u_alice"]), thread("doc2", ["u_bob"])],
      agentEvents: [event("a_writer", "doc1"), event("a_other", "doc2")],
      presences: {
        u_alice: presence("u_alice", "doc1"), // online in this doc -> ring
        u_me: presence("u_me", "docX"), // present in another doc -> no ring
        a_writer: presence("a_writer", undefined), // workspace-level -> no ring
      },
    });

    const result = documentParticipants(workspace, "doc1");

    // durable set = me + alice (thread) + writer (event); bob/other are on doc2, excluded
    expect(result.map((p) => p.id)).toEqual(["u_me", "u_alice", "a_writer"]);
    expect(result.map((p) => p.kind)).toEqual(["you", "collaborator", "agent"]);
    expect(result.find((p) => p.id === "u_alice")?.presentAt).toBe("now"); // fresh doc-level presence
    expect(result.find((p) => p.id === "u_me")?.presentAt).toBeUndefined(); // present in another document
    expect(result.find((p) => p.id === "a_writer")?.presentAt).toBeUndefined(); // workspace-level presence, not here
  });

  it("participantOnline shows the ring only for fresh presence and decays after the window", () => {
    const nowMs = Date.parse("2026-07-06T12:00:00Z");
    const iso = (offsetMs: number) => new Date(nowMs - offsetMs).toISOString();
    expect(participantOnline({ presentAt: iso(60_000) }, nowMs)).toBe(true); // 1 min ago -> online
    expect(participantOnline({ presentAt: iso(3 * 60_000) }, nowMs)).toBe(false); // 3 min ago -> decayed
    expect(participantOnline({ presentAt: undefined }, nowMs)).toBe(false);
    expect(participantOnline({ presentAt: "not-a-date" }, nowMs)).toBe(false);
  });

  it("keeps the current user with no document and never derives membership from presence", () => {
    const workspace = ws({
      currentUserId: "u_me",
      users: [user("u_me", "me")],
      presences: { u_ghost: presence("u_ghost", "doc1") }, // presence for a non-member is ignored
    });
    expect(documentParticipants(workspace, undefined).map((p) => p.id)).toEqual(["u_me"]);
    expect(documentParticipants(workspace, "doc1").map((p) => p.id)).toEqual(["u_me"]);
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
