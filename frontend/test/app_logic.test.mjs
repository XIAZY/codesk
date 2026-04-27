import test from "node:test";
import assert from "node:assert/strict";
import * as Y from "yjs";

import {
  buildLineThreads,
  computeReplace,
  findMentionQuery,
  isFreshPresence,
  rebaseSelection,
  rebaseReplace,
  resolveWorkspaceLoad,
  summarizeAgentStatus,
} from "../.test-dist/app_logic.js";

function applyReplace(value, replace) {
  return value.slice(0, replace.start) + replace.text + value.slice(replace.end);
}

test("resolveWorkspaceLoad rejects stale responses", () => {
  const result = resolveWorkspaceLoad(
    1,
    2,
    [{ id: "doc-new" }],
    "doc-current"
  );
  assert.deepEqual(result, { shouldApply: false, selectedId: "doc-current" });
});

test("resolveWorkspaceLoad preserves the preferred document when it still exists", () => {
  const result = resolveWorkspaceLoad(
    3,
    3,
    [{ id: "doc-a" }, { id: "doc-b" }],
    "doc-b"
  );
  assert.deepEqual(result, { shouldApply: true, selectedId: "doc-b" });
});

test("resolveWorkspaceLoad falls back to the first document when the preference disappeared", () => {
  const result = resolveWorkspaceLoad(
    4,
    4,
    [{ id: "doc-a" }, { id: "doc-b" }],
    "missing"
  );
  assert.deepEqual(result, { shouldApply: true, selectedId: "doc-a" });
});

test("computeReplace returns the smallest changed span", () => {
  const result = computeReplace("hello world", "hello brave world");
  assert.deepEqual(result, { start: 6, end: 6, text: "brave " });
});

test("rebaseReplace shifts a local insert after a remote prepend", () => {
  const local = computeReplace("ab", "abc");
  const remote = computeReplace("ab", "Xab");
  assert.deepEqual(rebaseReplace(local, remote), { start: 3, end: 3, text: "c" });
});

test("rebaseReplace shifts a local insert when a remote edit lands earlier in the line", () => {
  const local = computeReplace("abcd", "abcXd");
  const remote = computeReplace("abcd", "abZZcd");
  assert.deepEqual(rebaseReplace(local, remote), { start: 5, end: 5, text: "X" });
});

test("rebaseReplace preserves a local keystroke when remote sync is ahead of the visible draft", () => {
  const visibleDraft = "hello";
  const remoteCurrent = "remote hello";
  const local = computeReplace(visibleDraft, "hello!");
  const remote = computeReplace(visibleDraft, remoteCurrent);
  const rebased = rebaseReplace(local, remote);

  assert.equal(applyReplace(remoteCurrent, rebased), "remote hello!");
});

test("rebaseSelection keeps the caret after a remote insertion at the same position", () => {
  const remote = computeReplace("abcd", "abZZcd");
  assert.deepEqual(rebaseSelection({ start: 2, end: 2 }, remote), { start: 4, end: 4 });
});

test("rebaseSelection preserves a span that was replaced remotely", () => {
  const remote = computeReplace("abcdef", "abZZef");
  assert.deepEqual(rebaseSelection({ start: 2, end: 4 }, remote), { start: 2, end: 4 });
});

test("rebaseSelection collapses a caret that was inside a remote replacement", () => {
  const remote = computeReplace("abcdef", "abZZef");
  assert.deepEqual(rebaseSelection({ start: 3, end: 3 }, remote), { start: 4, end: 4 });
});

test("findMentionQuery only resolves a live mention token at the cursor", () => {
  assert.equal(findMentionQuery("hi @rev", 7, 7)?.query, "rev");
  assert.equal(findMentionQuery("hi @codex-agent", 15, 15)?.query, "codex-agent");
  assert.equal(findMentionQuery("hi @rev there", 7, 12), null);
  assert.equal(findMentionQuery("hi rev", 6, 6), null);
});

test("buildLineThreads groups multiple threads on the same line", () => {
  const groups = buildLineThreads([
    { id: "thread-2", anchor: { line: 3 } },
    { id: "thread-1", anchor: { line: 3 } },
    { id: "thread-3", anchor: { line: 5 } },
  ]);
  assert.equal(groups.length, 2);
  assert.equal(groups[0].line, 3);
  assert.deepEqual(groups[0].threads.map((thread) => thread.id), ["thread-2", "thread-1"]);
  assert.equal(groups[1].line, 5);
});

test("isFreshPresence treats recent timestamps as online", () => {
  assert.equal(isFreshPresence(new Date(Date.now() - 5_000).toISOString()), true);
  assert.equal(isFreshPresence(new Date(Date.now() - 25_000).toISOString()), false);
  assert.equal(isFreshPresence(undefined), false);
});

test("summarizeAgentStatus returns working for active runs", () => {
  const result = summarizeAgentStatus(
    true,
    { status: "idle", currentActivity: "Queued" },
    { status: "running", desiredStatus: "running" },
    { activity: "Reviewing the spec" }
  );
  assert.deepEqual(result, {
    label: "working",
    tone: "running",
    copy: "Reviewing the spec",
  });
});

test("summarizeAgentStatus uses resident agent session status before legacy runs", () => {
  const result = summarizeAgentStatus(
    true,
    { status: "working", currentActivity: "Processing inbox" },
    null,
    null
  );
  assert.deepEqual(result, {
    label: "working",
    tone: "running",
    copy: "Processing inbox",
  });
});

test("summarizeAgentStatus returns disconnected for resident agent sessions", () => {
  const result = summarizeAgentStatus(
    true,
    { status: "disconnected", currentActivity: "app-server exited" },
    null,
    null
  );
  assert.deepEqual(result, {
    label: "disconnected",
    tone: "failed",
    copy: "app-server exited",
  });
});

test("summarizeAgentStatus returns disconnected when the workspace socket is down", () => {
  const result = summarizeAgentStatus(
    false,
    { status: "working", currentActivity: "Idle" },
    { status: "completed", desiredStatus: "stopped" },
    null
  );
  assert.deepEqual(result, {
    label: "disconnected",
    tone: "failed",
    copy: "Workspace connection unavailable.",
  });
});

test("summarizeAgentStatus collapses terminal runs into idle and keeps useful detail copy", () => {
  const result = summarizeAgentStatus(
    true,
    { status: "idle", currentActivity: "Idle" },
    { status: "failed", desiredStatus: "stopped", error: "argument list too long" },
    null
  );
  assert.deepEqual(result, {
    label: "idle",
    tone: "idle",
    copy: "argument list too long",
  });
});

test("keystroke-sized yjs updates converge across two tabs with delayed delivery", () => {
  const left = new Y.Doc();
  const right = new Y.Doc();
  const leftUpdates = [];
  const rightUpdates = [];

  left.on("update", (update) => leftUpdates.push(update));
  right.on("update", (update) => rightUpdates.push(update));

  const leftText = left.getText("content");
  for (const char of "abcdefghijklmnopqrstuvwxyz") {
    left.transact(() => {
      leftText.insert(leftText.length, char);
    }, "left-keystroke");
  }

  const rightText = right.getText("content");
  for (const char of "0123456789") {
    right.transact(() => {
      rightText.insert(rightText.length, char);
    }, "right-keystroke");
  }

  const mixedLeftUpdates = leftUpdates.filter((_, index) => index % 2 === 1).concat(leftUpdates.filter((_, index) => index % 2 === 0));
  const mixedRightUpdates = rightUpdates.filter((_, index) => index % 2 === 1).concat(rightUpdates.filter((_, index) => index % 2 === 0));

  for (const update of mixedLeftUpdates.concat(mixedLeftUpdates.slice(0, 3))) {
    Y.applyUpdate(right, update, "remote");
  }
  for (const update of mixedRightUpdates.concat(mixedRightUpdates.slice(0, 3))) {
    Y.applyUpdate(left, update, "remote");
  }

  assert.equal(left.getText("content").toString(), right.getText("content").toString());
  for (const char of "abcdefghijklmnopqrstuvwxyz0123456789") {
    assert.equal(left.getText("content").toString().includes(char), true);
  }
});
