// @vitest-environment jsdom

import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { ensureSyntaxTree } from "@codemirror/language";
import { EditorSelection, EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { afterEach, describe, expect, it } from "vitest";
import {
  collectMarkdownPreviewTokens,
  markdownPreviewCommand,
  nottyMarkdownLivePreview,
} from "./markdownLivePreview";

afterEach(() => {
  document.body.innerHTML = "";
});

function markdownState(doc: string, selection = EditorSelection.cursor(doc.length)) {
  const state = EditorState.create({
    doc,
    selection,
    extensions: [markdown({ base: markdownLanguage }), nottyMarkdownLivePreview()],
  });
  // Force a complete parse before the test reads tokens. collectMarkdownPreviewTokens calls
  // syntaxTree(), which advances the Lezer parse within a real-time work budget and returns whatever
  // is parsed so far — correct for the live editor's viewport, but under CPU load the parse can halt
  // mid-document in a test, dropping tokens for later lines (the observed flake: the ordered "1."
  // list marker, and everything after it, went missing). ensureSyntaxTree parses to the document end
  // deterministically, so token collection no longer races the parser's wall-clock budget. It returns
  // null if it can't finish within the budget — fail loudly rather than silently falling back to the
  // flaky partial-parse path.
  if (!ensureSyntaxTree(state, doc.length, 5000)) {
    throw new Error("markdown live preview test: syntax tree did not fully parse within the budget");
  }
  return state;
}

function editor(doc: string, selection = EditorSelection.cursor(doc.length)) {
  return new EditorView({
    parent: document.body,
    state: markdownState(doc, selection),
  });
}

describe("markdown live preview", () => {
  it("finds source-native spans for common markdown without changing offsets", () => {
    const doc = [
      "# Title",
      "",
      "This is **bold**, *em*, ~~strike~~, `code`, and [link](https://example.com).",
      "Bare URL: https://example.com/bare and <https://example.com/angle>.",
      "Image: ![Alt text](https://example.com/image.png)",
      "",
      "> quote",
      "- [x] task",
      "- plain bullet",
      "1. ordered item",
      "",
      "```ts",
      "console.log(1)",
      "```",
      "",
      "| A | B |",
      "| - | - |",
      "| 1 | 2 |",
    ].join("\n");
    const state = markdownState(doc, EditorSelection.cursor(doc.length));
    const tokens = collectMarkdownPreviewTokens(state, [{ from: 0, to: doc.length }], [{ from: doc.length, to: doc.length }]);

    expect(tokens).toContainEqual(expect.objectContaining({ kind: "heading", from: 0, to: 7, level: 1 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "heading-marker", from: 0, to: 2 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "strong", from: 19, to: 23 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "emphasis", from: 28, to: 30 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "strike", from: 35, to: 41 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "inline-code", from: 46, to: 50 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "link-label", from: 58, to: 62 }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "url" }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "image-alt" }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "image-syntax" }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "quote-marker" }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "list-marker", marker: "-", taskLine: true, ordered: false }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "task-marker", checked: true }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "list-marker", marker: "1.", taskLine: false, ordered: true }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "code-fence" }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "table" }));
  });

  it("reveals source syntax for the active line", () => {
    const doc = "# Title\n\nOutside **bold**";
    const state = markdownState(doc, EditorSelection.cursor(3));
    const headingMarker = collectMarkdownPreviewTokens(state).find((token) => token.kind === "heading-marker");
    const strongMarker = collectMarkdownPreviewTokens(state).find((token) => token.kind === "strong-marker");

    expect(headingMarker?.active).toBe(true);
    expect(strongMarker?.active).toBe(false);
  });

  it("formats selected source text directly", () => {
    const view = editor("alpha bravo charlie", EditorSelection.range(6, 11));

    expect(markdownPreviewCommand("bold")(view)).toBe(true);
    expect(view.state.doc.toString()).toBe("alpha **bravo** charlie");

    view.destroy();
  });

  it("formats selected lines without converting the document model", () => {
    const view = editor("alpha\nbravo", EditorSelection.range(0, "alpha\nbravo".length));

    expect(markdownPreviewCommand("quote")(view)).toBe(true);
    expect(view.state.doc.toString()).toBe("> alpha\n> bravo");
    expect(markdownPreviewCommand("bulletList")(view)).toBe(true);
    expect(view.state.doc.toString()).toBe("- > alpha\n- > bravo");

    view.destroy();
  });

  it("creates markdown links as raw source text", () => {
    const view = editor("read docs", EditorSelection.range(5, 9));

    expect(markdownPreviewCommand("link")(view)).toBe(true);
    expect(view.state.doc.toString()).toBe("read [docs](https://example.com)");

    view.destroy();
  });

  it("renders inactive GFM task markers as clickable checkboxes over raw source", () => {
    const view = editor("- [ ] todo\n- [x] done\n- plain item", EditorSelection.cursor("- [ ] todo\n- [x] done\n- plain item".length));

    const boxes = Array.from(view.dom.querySelectorAll<HTMLInputElement>(".cm-md-task-checkbox input"));
    expect(boxes).toHaveLength(2);
    expect(boxes[0].checked).toBe(false);
    expect(boxes[1].checked).toBe(true);

    boxes[0].click();
    expect(view.state.doc.toString()).toBe("- [x] todo\n- [x] done\n- plain item");

    view.destroy();
  });

  it("reveals GFM task source markers when the task line is active", () => {
    const view = editor("- [x] active task\n- [ ] inactive task", EditorSelection.cursor(4));

    const boxes = Array.from(view.dom.querySelectorAll<HTMLInputElement>(".cm-md-task-checkbox input"));
    expect(boxes).toHaveLength(1);
    expect(boxes[0].checked).toBe(false);
    expect(view.dom.textContent).toContain("- [x] active task");

    boxes[0].click();
    expect(view.state.doc.toString()).toBe("- [x] active task\n- [x] inactive task");

    view.destroy();
  });

  it("renders inactive unordered and ordered list markers without changing source text", () => {
    const view = editor("- plain item\n1. ordered item\n2) ordered paren", EditorSelection.cursor(0));

    const listMarkers = Array.from(view.dom.querySelectorAll<HTMLElement>(".cm-md-list-widget")).map((item) => item.textContent);
    expect(listMarkers).toEqual(["1.", "2)"]);
    expect(view.state.doc.toString()).toBe("- plain item\n1. ordered item\n2) ordered paren");

    view.destroy();
  });
});
