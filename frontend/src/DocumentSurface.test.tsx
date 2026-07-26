// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { Compartment, EditorState } from "@codemirror/state";
import * as Y from "yjs";
import { DocumentSurface } from "./DocumentSurface";
import * as codeHighlight from "./codeHighlight";
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

function renderSurface(input: { ydoc: Y.Doc; ytext: Y.Text; threads?: ThreadItem[]; enableMarkdownLivePreview?: boolean; codeFileExtension?: string; documentId?: string }) {
  return render(
    <DocumentSurface
      documentId={input.documentId ?? "doc"}
      ydoc={input.ydoc}
      ytext={input.ytext}
      ready
      threads={input.threads ?? []}
      focusThreadId=""
      onFocusThreadHandled={vi.fn()}
      onSelectionChange={vi.fn()}
      onLineThreadsOpen={vi.fn()}
      enableMarkdownLivePreview={input.enableMarkdownLivePreview}
      codeFileExtension={input.codeFileExtension}
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

  describe("code-file highlighting", () => {
    const CODE = 'def greet(name):\n    return f"hi {name}"\n';

    it("renders a code file and preserves the source byte-for-byte (highlighting is decoration-only)", async () => {
      const ydoc = new Y.Doc();
      const ytext = ydoc.getText("content");
      ytext.insert(0, CODE);
      const { container } = renderSurface({ ydoc, ytext, codeFileExtension: ".py" });
      await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
      // Zero character mutation: the Y.Text (source of truth for thread-anchor offsets) is unchanged.
      expect(ytext.toString()).toBe(CODE);
      expect(container.querySelector(".cm-content")?.textContent).toContain("def greet");
    });

    it("falls back to plain text for an unrecognized extension without crashing", async () => {
      const ydoc = new Y.Doc();
      const ytext = ydoc.getText("content");
      ytext.insert(0, CODE);
      const { container } = renderSurface({ ydoc, ytext, codeFileExtension: ".unknownext" });
      await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
      expect(ytext.toString()).toBe(CODE);
    });

    it("leaves a Markdown document on the live-preview path, untouched by the code grammar", async () => {
      const ydoc = new Y.Doc();
      const ytext = ydoc.getText("content");
      const md = "# Title\n\nBody with `inline`.";
      ytext.insert(0, md);
      const { container } = renderSurface({ ydoc, ytext, enableMarkdownLivePreview: true });
      await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
      expect(ytext.toString()).toBe(md);
    });

    it("survives a rename and rapid extension switches without crashing or mutating the buffer", async () => {
      const ydoc = new Y.Doc();
      const ytext = ydoc.getText("content");
      ytext.insert(0, CODE);
      const props = (ext?: string, md = false) => ({
        documentId: "doc", ydoc, ytext, ready: true as const, threads: [],
        focusThreadId: "", onFocusThreadHandled: vi.fn(), onSelectionChange: vi.fn(),
        onLineThreadsOpen: vi.fn(), codeFileExtension: ext, enableMarkdownLivePreview: md,
      });
      const { container, rerender } = render(<DocumentSurface {...props(".ts")} />);
      await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
      // A late-resolving grammar import for a prior extension must never paint the wrong file.
      for (const ext of [".py", ".rs", ".sh", ".tex", ".unknownext"]) {
        rerender(<DocumentSurface {...props(ext)} />);
      }
      await waitFor(() => expect(container.querySelector(".cm-editor")).toBeTruthy());
      expect(ytext.toString()).toBe(CODE);
    });

    // Anton's causal gate: controllable loaders + a reconfigure spy prove the lifecycle, since the
    // rendered highlight classes are not reliably assertable in jsdom. A non-empty reconfigure arg =
    // a grammar installed; an empty-array arg = cleared to plain.
    function codeProps(ext?: string, md = false) {
      const ydoc = new Y.Doc();
      const ytext = ydoc.getText("content");
      ytext.insert(0, "x = 1\n");
      return {
        documentId: "doc", ydoc, ytext, ready: true as const, threads: [] as ThreadItem[],
        focusThreadId: "", onFocusThreadHandled: vi.fn(), onSelectionChange: vi.fn(),
        onLineThreadsOpen: vi.fn(), codeFileExtension: ext, enableMarkdownLivePreview: md,
      };
    }

    it("clears the old grammar synchronously on switch and stays plain when the new import rejects", async () => {
      const GRAMMAR_A = EditorState.tabSize.of(4); // valid, harmless, non-empty stand-in for grammar A
      let resolveA!: (g: unknown) => void;
      let rejectB!: (e: unknown) => void;
      vi.spyOn(codeHighlight, "grammarLoaderForExtension").mockImplementation((ext: string) => {
        if (ext === ".py") return () => new Promise((res) => { resolveA = res as never; });
        if (ext === ".ts") return () => new Promise((_res, rej) => { rejectB = rej as never; });
        return null;
      });
      const reconfig = vi.spyOn(Compartment.prototype, "reconfigure");
      const isClear = (arg: unknown) => Array.isArray(arg) && arg.length === 0;
      const clears = () => reconfig.mock.calls.filter((c) => isClear(c[0])).length;
      const grammars = () => reconfig.mock.calls.filter((c) => !isClear(c[0])).length;

      const props = codeProps(".py");
      const { rerender } = render(<DocumentSurface {...props} />);
      await waitFor(() => expect(document.querySelector(".cm-editor")).toBeTruthy());

      // Mount cleared to plain synchronously, before A resolves.
      expect(clears()).toBeGreaterThanOrEqual(1);
      expect(grammars()).toBe(0);

      // A resolves → grammar A installed.
      await act(async () => { resolveA(GRAMMAR_A); await Promise.resolve(); });
      expect(grammars()).toBe(1);

      reconfig.mockClear();
      // Switch .py -> .ts clears synchronously (immediate plain) before B settles.
      rerender(<DocumentSurface {...props} codeFileExtension=".ts" />);
      expect(clears()).toBeGreaterThanOrEqual(1);

      // B rejects → stays plain; no grammar reconfigure.
      await act(async () => { rejectB(new Error("import failed")); await Promise.resolve(); });
      expect(grammars()).toBe(0);
    });

    it("ignores a late grammar resolve for a file that has been switched away", async () => {
      const GRAMMAR = EditorState.tabSize.of(2);
      let resolveLate!: (g: unknown) => void;
      vi.spyOn(codeHighlight, "grammarLoaderForExtension").mockImplementation((ext: string) => {
        if (ext === ".py") return () => new Promise((res) => { resolveLate = res as never; });
        return null; // .ts here is "unmapped" → plain, so switching away is clean
      });
      const reconfig = vi.spyOn(Compartment.prototype, "reconfigure");
      const grammars = () => reconfig.mock.calls.filter((c) => !(Array.isArray(c[0]) && c[0].length === 0)).length;

      const props = codeProps(".py");
      const { rerender } = render(<DocumentSurface {...props} />);
      await waitFor(() => expect(document.querySelector(".cm-editor")).toBeTruthy());
      // Switch away BEFORE .py's grammar resolves (its effect is now superseded/cancelled).
      rerender(<DocumentSurface {...props} codeFileExtension=".ts" />);
      reconfig.mockClear();
      // The stale .py import resolves late — the cancelled guard must drop it.
      await act(async () => { resolveLate(GRAMMAR); await Promise.resolve(); });
      expect(grammars()).toBe(0);
    });

    it("re-routes across the .md <-> .ts boundary (grammar only on the code side)", async () => {
      const GRAMMAR = EditorState.tabSize.of(4);
      vi.spyOn(codeHighlight, "grammarLoaderForExtension").mockImplementation((ext: string) =>
        ext === ".ts" ? () => Promise.resolve(GRAMMAR) : null);
      const reconfig = vi.spyOn(Compartment.prototype, "reconfigure");
      const grammars = () => reconfig.mock.calls.filter((c) => !(Array.isArray(c[0]) && c[0].length === 0)).length;

      const props = codeProps(undefined, true); // start as Markdown
      const { rerender } = render(<DocumentSurface {...props} />);
      await waitFor(() => expect(document.querySelector(".cm-editor")).toBeTruthy());
      // Markdown side: the grammar effect early-returns, so no grammar is installed.
      expect(grammars()).toBe(0);

      // -> .ts (code): grammar installs.
      rerender(<DocumentSurface {...props} codeFileExtension=".ts" enableMarkdownLivePreview={false} />);
      await act(async () => { await Promise.resolve(); });
      expect(grammars()).toBeGreaterThanOrEqual(1);

      // -> .md again (back to Markdown): the code branch no longer runs; ytext is intact.
      reconfig.mockClear();
      rerender(<DocumentSurface {...props} codeFileExtension={undefined} enableMarkdownLivePreview />);
      await waitFor(() => expect(document.querySelector(".cm-editor")).toBeTruthy());
      expect(grammars()).toBe(0);
    });
  });
});
