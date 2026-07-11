// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import * as Y from "yjs";
import { DocumentSurface } from "./DocumentSurface";
import { encodeRelativeAnchor } from "./logic";
import type { ThreadItem } from "./types";

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
  HTMLElement.prototype.getBoundingClientRect = function () {
    return {
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: 900,
      bottom: 700,
      width: 900,
      height: 700,
      toJSON: () => ({}),
    } as DOMRect;
  };
  document.createRange = () =>
    ({
      setStart: vi.fn(),
      setEnd: vi.fn(),
      setStartBefore: vi.fn(),
      setStartAfter: vi.fn(),
      setEndBefore: vi.fn(),
      setEndAfter: vi.fn(),
      collapse: vi.fn(),
      selectNode: vi.fn(),
      selectNodeContents: vi.fn(),
      getClientRects: () => [] as unknown as DOMRectList,
      getBoundingClientRect: () =>
        ({
          x: 0,
          y: 0,
          top: 0,
          left: 0,
          right: 0,
          bottom: 0,
          width: 0,
          height: 0,
          toJSON: () => ({}),
        }) as DOMRect,
    }) as unknown as Range;
});

afterEach(() => {
  cleanup();
});

function renderSurface(input: { ydoc: Y.Doc; ytext: Y.Text; threads?: ThreadItem[]; enableMarkdownLivePreview?: boolean }) {
  return render(
    <DocumentSurface
      documentId="doc"
      ydoc={input.ydoc}
      ytext={input.ytext}
      ready
      threads={input.threads ?? []}
      focusThreadId=""
      onFocusThreadHandled={vi.fn()}
      onSelectionChange={vi.fn()}
      onLineThreadsOpen={vi.fn()}
      enableMarkdownLivePreview={input.enableMarkdownLivePreview}
    />
  );
}

describe("DocumentSurface", () => {
  it("does not render one DOM node per large document line", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    const lines = Array.from({ length: 50_000 }, (_, index) => `${index}: codex-agent log line`);
    ytext.insert(0, lines.join("\n"));

    const { container } = renderSurface({ ydoc, ytext });

    await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
    expect(container.querySelectorAll("*").length).toBeLessThan(5_000);
    expect(container.textContent?.length ?? 0).toBeLessThan(ytext.length / 10);
  });

  it("keeps Markdown live preview virtualized for large markdown documents", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    const lines = Array.from({ length: 50_000 }, (_, index) => `## Heading ${index}\nThis has **bold** and [link](https://example.com/${index}).`);
    ytext.insert(0, lines.join("\n"));

    const { container } = renderSurface({ ydoc, ytext, enableMarkdownLivePreview: true });

    await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
    expect(container.querySelectorAll("*").length).toBeLessThan(5_000);
    expect(container.textContent?.length ?? 0).toBeLessThan(ytext.length / 10);
  });

  it("renders document surfaces without code-editor gutters", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    ytext.insert(0, "# Markdown document\n\nPlain writing surface.");

    const markdownRender = renderSurface({ ydoc, ytext, enableMarkdownLivePreview: true });

    await waitFor(() => expect(markdownRender.container.querySelector(".cm-editor")).toBeTruthy());
    expect(markdownRender.container.querySelector(".cm-gutters")).toBeNull();
    markdownRender.unmount();

    const textDoc = new Y.Doc();
    const plainText = textDoc.getText("content");
    plainText.insert(0, "Plain text document.");
    const plainRender = renderSurface({ ydoc: textDoc, ytext: plainText });

    await waitFor(() => expect(plainRender.container.querySelector(".cm-editor")).toBeTruthy());
    expect(plainRender.container.querySelector(".cm-gutters")).toBeNull();
  });

  it("renders a CRDT-relative thread marker without full-document rendering", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    ytext.insert(0, "alpha bravo charlie");
    const thread: ThreadItem = {
      id: "thread",
      documentId: "doc",
      title: "Selection on line 1",
      status: "open",
      anchor: {
        kind: "text-range",
        relativeStart: encodeRelativeAnchor(ytext, 6, "start"),
        relativeEnd: encodeRelativeAnchor(ytext, 11, "end"),
        excerpt: "bravo",
      },
      participantIds: [],
      participantHandles: [],
      messages: [],
      createdById: "user",
      createdByType: "human",
      createdByHandle: "hello",
      createdByName: "Hello",
      createdAt: "now",
      updatedAt: "now",
    };

    renderSurface({ ydoc, ytext, threads: [thread] });

    const marker = await screen.findByRole("button", { name: "1 open thread on line 1" });
    expect(marker.closest(".thread-anchor-rail")).toBeTruthy();
    expect(marker.closest(".cm-content")).toBeFalsy();
    expect(marker.querySelector(".thread-rail-icon")).toBeTruthy();
    expect(marker.querySelector(".thread-rail-dot")).toBeNull();
  });

  it("moves a rail marker when CRDT-relative anchors move after text edits", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    ytext.insert(0, "alpha bravo charlie");
    const thread: ThreadItem = {
      id: "thread",
      documentId: "doc",
      title: "Selection on line 1",
      status: "open",
      anchor: {
        kind: "text-range",
        relativeStart: encodeRelativeAnchor(ytext, 6, "start"),
        relativeEnd: encodeRelativeAnchor(ytext, 11, "end"),
        excerpt: "bravo",
      },
      participantIds: [],
      participantHandles: [],
      messages: [],
      createdById: "user",
      createdByType: "human",
      createdByHandle: "hello",
      createdByName: "Hello",
      createdAt: "now",
      updatedAt: "now",
    };

    renderSurface({ ydoc, ytext, threads: [thread] });

    expect(await screen.findByRole("button", { name: "1 open thread on line 1" })).toBeTruthy();
    ytext.insert(0, "\n");

    expect(await screen.findByRole("button", { name: "1 open thread on line 2" })).toBeTruthy();
  });

  it("counts only open threads while passing the full mixed-line group to the popup", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    ytext.insert(0, "alpha bravo charlie");
    const anchor = {
      kind: "text-range",
      relativeStart: encodeRelativeAnchor(ytext, 6, "start"),
      relativeEnd: encodeRelativeAnchor(ytext, 11, "end"),
      excerpt: "bravo",
    };
    const base = {
      documentId: "doc",
      title: "Selection on line 1",
      anchor,
      participantIds: [],
      participantHandles: [],
      messages: [],
      createdById: "user",
      createdByType: "human",
      createdByHandle: "hello",
      createdByName: "Hello",
      createdAt: "now",
      updatedAt: "now",
    };
    const threads: ThreadItem[] = [
      { ...base, id: "open", status: "open" },
      { ...base, id: "resolved", status: "resolved" },
    ];
    const onLineThreadsOpen = vi.fn();

    const { rerender } = render(
      <DocumentSurface
        documentId="doc"
        ydoc={ydoc}
        ytext={ytext}
        ready
        threads={threads}
        focusThreadId=""
        onFocusThreadHandled={vi.fn()}
        onSelectionChange={vi.fn()}
        onLineThreadsOpen={onLineThreadsOpen}
      />,
    );

    const marker = await screen.findByRole("button", { name: "1 open thread on line 1" });
    fireEvent.click(marker);
    expect(onLineThreadsOpen).toHaveBeenCalledTimes(1);
    expect(onLineThreadsOpen.mock.calls[0][0].threads.map((thread: ThreadItem) => thread.id)).toEqual(["open", "resolved"]);

    rerender(
      <DocumentSurface
        documentId="doc"
        ydoc={ydoc}
        ytext={ytext}
        ready
        threads={threads.map((thread) => ({ ...thread, status: "resolved" }))}
        focusThreadId=""
        onFocusThreadHandled={vi.fn()}
        onSelectionChange={vi.fn()}
        onLineThreadsOpen={onLineThreadsOpen}
      />,
    );
    await waitFor(() => expect(screen.queryByRole("button", { name: /open threads? on line 1/ })).toBeNull());
  });

  it("does not render a marker for a fully resolved line", async () => {
    const ydoc = new Y.Doc();
    const ytext = ydoc.getText("content");
    ytext.insert(0, "alpha bravo charlie");
    const thread: ThreadItem = {
      id: "resolved",
      documentId: "doc",
      title: "Selection on line 1",
      status: "resolved",
      anchor: {
        kind: "text-range",
        relativeStart: encodeRelativeAnchor(ytext, 6, "start"),
        relativeEnd: encodeRelativeAnchor(ytext, 11, "end"),
        excerpt: "bravo",
      },
      participantIds: [],
      participantHandles: [],
      messages: [],
      createdById: "user",
      createdByType: "human",
      createdByHandle: "hello",
      createdByName: "Hello",
      createdAt: "now",
      updatedAt: "now",
    };

    const { container } = renderSurface({ ydoc, ytext, threads: [thread] });
    await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
    expect(container.querySelector(".thread-rail-marker")).toBeNull();
  });
});
