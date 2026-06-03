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
  daemonStatus,
  encodeRelativeAnchor,
  lineForOffset,
  lineStartsForText,
  reduceWorkspaceEvent,
  resolveThreadAnchorLive,
  selectionLabel,
} from "./logic";
import type { Agent, Daemon, WorkspaceState } from "./types";

function baseWorkspace(): WorkspaceState {
  return {
    workspaceId: "ws",
    rootDocumentId: "doc_root_ws",
    name: "Workspace",
    documents: [],
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

describe("workspace reduction", () => {
  it("updates document metadata without requiring document content", () => {
    const state = reduceWorkspaceEvent(baseWorkspace(), {
      type: "document.updated",
      data: { documentId: "doc", path: "docs/spec.md", title: "spec.md", updateId: 42, updatedAt: "now" },
    });

    expect(state.documents).toEqual([
      { id: "doc", path: "docs/spec.md", title: "spec.md", updateId: 42, updatedAt: "now" },
    ]);
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
});

describe("presentation grouping", () => {
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
    const daemon: Daemon = { id: "daemon", workspaceId: "ws", name: "Local", status: "active", connectionStatus: "stale", createdAt: "now" };
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
