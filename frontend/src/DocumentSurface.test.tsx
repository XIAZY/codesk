// @vitest-environment jsdom

import { cleanup, render, screen, waitFor } from "@testing-library/react";
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

function renderSurface(input: { ydoc: Y.Doc; ytext: Y.Text; threads?: ThreadItem[] }) {
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
        relativeStart: encodeRelativeAnchor(ytext, 6),
        relativeEnd: encodeRelativeAnchor(ytext, 11),
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

    const marker = await screen.findByRole("button", { name: "1 thread on line 1" });
    expect(marker.closest(".thread-anchor-rail")).toBeTruthy();
    expect(marker.closest(".cm-content")).toBeFalsy();
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
        relativeStart: encodeRelativeAnchor(ytext, 6),
        relativeEnd: encodeRelativeAnchor(ytext, 11),
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

    expect(await screen.findByRole("button", { name: "1 thread on line 1" })).toBeTruthy();
    ytext.insert(0, "\n");

    expect(await screen.findByRole("button", { name: "1 thread on line 2" })).toBeTruthy();
  });
});
