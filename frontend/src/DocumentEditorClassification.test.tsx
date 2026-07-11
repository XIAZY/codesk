// @vitest-environment jsdom

import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import * as Y from "yjs";
import { encodeRelativeAnchor } from "./logic";
import type { ThreadItem } from "./types";

let mockReady = false;
let mockYdoc = new Y.Doc();
let mockYtext = mockYdoc.getText("content");

vi.mock("./useDocument", () => ({
  useDocumentSync: () => ({
    ydoc: mockYdoc,
    ytext: mockYtext,
    ready: mockReady,
    connected: true,
  }),
}));

vi.mock("./DocumentSurface", () => ({
  DocumentSurface: () => <div data-testid="document-surface" />,
}));

import { DocumentEditor } from "./App";

beforeAll(() => {
  class ResizeObserverMock {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  Object.defineProperty(window, "ResizeObserver", {
    value: ResizeObserverMock,
    configurable: true,
  });
  Object.defineProperty(globalThis, "ResizeObserver", {
    value: ResizeObserverMock,
    configurable: true,
  });
});

afterEach(() => {
  cleanup();
});

function makeThread(ydoc: Y.Doc, id: string, text: string, start: number, end: number): ThreadItem {
  const ytext = ydoc.getText("content");
  return {
    id,
    documentId: "doc-1",
    title: `Thread ${id}`,
    status: "open",
    anchor: {
      kind: "text-range",
      relativeStart: encodeRelativeAnchor(ytext, start, "start"),
      relativeEnd: encodeRelativeAnchor(ytext, end, "end"),
      excerpt: text.slice(start, end),
    },
    participantIds: [],
    participantHandles: [],
    messages: [],
    createdById: "user-1",
    createdByType: "human",
    createdByHandle: "alice",
    createdByName: "Alice",
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  };
}

function renderEditor(threads: ThreadItem[], onThreadAnchorInfo: (info: Record<string, { orphaned: boolean; line: number }>) => void) {
  return render(
    <DocumentEditor
      api={{} as never}
      token="test-token"
      workspaceId="ws-1"
      actorName="Tester"
      actorLabel="you"
      document={{ id: "doc-1", path: "test.md", title: "test.md" }}
      threads={threads}
      focusThreadId=""
      onFocusThreadHandled={vi.fn()}
      onThreadCreated={vi.fn()}
      onThreadsChanged={vi.fn()}
      titleEditing={false}
      titleDraft=""
      onTitleEditStart={vi.fn()}
      onTitleDraftChange={vi.fn()}
      onTitleEditCancel={vi.fn()}
      onTitleCommit={vi.fn()}
      onThreadAnchorInfo={onThreadAnchorInfo}
    />,
  );
}

describe("DocumentEditor anchor-info classification effect", () => {
  it("does NOT propagate classifications when ready=false, then propagates correct classifications when ready=true", async () => {
    const populatedDoc = new Y.Doc();
    const populatedText = populatedDoc.getText("content");
    populatedDoc.transact(() => populatedText.insert(0, "hello world\nsecond line"));

    const thread = makeThread(populatedDoc, "t1", "hello world\nsecond line", 0, 5);

    const emptyDoc = new Y.Doc();
    emptyDoc.getText("content");

    mockYdoc = emptyDoc;
    mockYtext = emptyDoc.getText("content");
    mockReady = false;

    const onInfo = vi.fn();
    const { rerender } = renderEditor([thread], onInfo);

    await new Promise((r) => setTimeout(r, 50));
    expect(onInfo).not.toHaveBeenCalled();

    Y.applyUpdate(emptyDoc, Y.encodeStateAsUpdate(populatedDoc));
    mockYdoc = emptyDoc;
    mockYtext = emptyDoc.getText("content");
    mockReady = true;

    rerender(
      <DocumentEditor
        api={{} as never}
        token="test-token"
        workspaceId="ws-1"
        actorName="Tester"
        actorLabel="you"
        document={{ id: "doc-1", path: "test.md", title: "test.md" }}
        threads={[thread]}
        focusThreadId=""
        onFocusThreadHandled={vi.fn()}
        onThreadCreated={vi.fn()}
        onThreadsChanged={vi.fn()}
        titleEditing={false}
        titleDraft=""
        onTitleEditStart={vi.fn()}
        onTitleDraftChange={vi.fn()}
        onTitleEditCancel={vi.fn()}
        onTitleCommit={vi.fn()}
        onThreadAnchorInfo={onInfo}
      />,
    );

    await waitFor(() => expect(onInfo).toHaveBeenCalled());

    const info = onInfo.mock.calls[onInfo.mock.calls.length - 1][0] as Record<string, { orphaned: boolean; line: number }>;
    expect(info.t1).toBeDefined();
    expect(info.t1.orphaned).toBe(false);
    expect(info.t1.line).toBeGreaterThanOrEqual(1);
  });

  it("re-classifies live when a content edit deletes an anchor's text (observe-trigger, #40)", async () => {
    const doc = new Y.Doc();
    const text = doc.getText("content");
    doc.transact(() => text.insert(0, "hello world\nsecond line"));

    const thread = makeThread(doc, "t1", "hello world\nsecond line", 0, 5); // anchor on "hello"

    mockYdoc = doc;
    mockYtext = text;
    mockReady = true;

    const onInfo = vi.fn();
    renderEditor([thread], onInfo);

    // Load-time classification: the anchored text is present → anchored.
    await waitFor(() => expect(onInfo).toHaveBeenCalled());
    const initial = onInfo.mock.calls[onInfo.mock.calls.length - 1][0] as Record<string, { orphaned: boolean; line: number }>;
    expect(initial.t1.orphaned).toBe(false);

    // Live in-place edit: Y.js mutates ytext, so the effect deps do not change.
    // Only the observe-trigger re-runs classification. Deleting "hello world"
    // removes the anchored text → the anchor must flip to orphaned live.
    doc.transact(() => text.delete(0, 11));

    await waitFor(() => {
      const latest = onInfo.mock.calls[onInfo.mock.calls.length - 1][0] as Record<string, { orphaned: boolean; line: number }>;
      expect(latest.t1.orphaned).toBe(true);
    });
  });
});
