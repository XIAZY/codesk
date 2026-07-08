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
      relativeStart: encodeRelativeAnchor(ytext, start),
      relativeEnd: encodeRelativeAnchor(ytext, end),
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
      api={{ updateThreadAnchor: vi.fn() } as never}
      token="test-token"
      workspaceId="ws-1"
      actorName="Tester"
      actorLabel="you"
      document={{ id: "doc-1", path: "test.md", title: "test.md" }}
      threads={threads}
      focusThreadId=""
      onFocusThreadHandled={vi.fn()}
      onThreadSelected={vi.fn()}
      onThreadCreated={vi.fn()}
      titleEditing={false}
      titleDraft=""
      onTitleEditStart={vi.fn()}
      onTitleDraftChange={vi.fn()}
      onTitleEditCancel={vi.fn()}
      onTitleCommit={vi.fn()}
      onThreadAnchorInfo={onThreadAnchorInfo}
      reanchorThreadId=""
      onReanchorComplete={vi.fn()}
      onReanchorCancel={vi.fn()}
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
        api={{ updateThreadAnchor: vi.fn() } as never}
        token="test-token"
        workspaceId="ws-1"
        actorName="Tester"
        actorLabel="you"
        document={{ id: "doc-1", path: "test.md", title: "test.md" }}
        threads={[thread]}
        focusThreadId=""
        onFocusThreadHandled={vi.fn()}
        onThreadSelected={vi.fn()}
        onThreadCreated={vi.fn()}
        titleEditing={false}
        titleDraft=""
        onTitleEditStart={vi.fn()}
        onTitleDraftChange={vi.fn()}
        onTitleEditCancel={vi.fn()}
        onTitleCommit={vi.fn()}
        onThreadAnchorInfo={onInfo}
        reanchorThreadId=""
        onReanchorComplete={vi.fn()}
        onReanchorCancel={vi.fn()}
      />,
    );

    await waitFor(() => expect(onInfo).toHaveBeenCalled());

    const info = onInfo.mock.calls[onInfo.mock.calls.length - 1][0] as Record<string, { orphaned: boolean; line: number }>;
    expect(info.t1).toBeDefined();
    expect(info.t1.orphaned).toBe(false);
    expect(info.t1.line).toBeGreaterThanOrEqual(1);
  });
});
