// @vitest-environment jsdom

import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { forceParsing } from "@codemirror/language";
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
  const view = new EditorView({
    state: EditorState.create({
      doc,
      selection,
      extensions: [markdown({ base: markdownLanguage }), nottyMarkdownLivePreview()],
    }),
  });
  // collectMarkdownPreviewTokens reads syntaxTree(view.state). forceParsing both completes the
  // mutable parse context and dispatches a state update that publishes that tree. Calling
  // ensureSyntaxTree alone is insufficient: it returns the completed tree while view.state still
  // exposes the old partial tree, which caused the load-dependent missing-token failure.
  if (!forceParsing(view, doc.length, 5000)) {
    view.destroy();
    throw new Error("markdown live preview test: syntax tree did not fully parse within the budget");
  }
  const state = view.state;
  view.destroy();
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
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "list-marker", taskLine: true, ordered: false }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "task-marker", checked: true }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "list-marker", taskLine: false, ordered: true }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "code-fence" }));
    expect(tokens).toContainEqual(expect.objectContaining({ kind: "table" }));
  });

  it("publishes a complete tree past the parser's initial viewport", () => {
    const doc = `${"plain text\n".repeat(400)}> late quote`;
    const quoteFrom = doc.lastIndexOf(">");
    expect(quoteFrom).toBeGreaterThan(3000);

    const state = markdownState(doc);
    const tokens = collectMarkdownPreviewTokens(state, [{ from: quoteFrom, to: doc.length }]);

    expect(tokens).toContainEqual(expect.objectContaining({ kind: "quote-marker", from: quoteFrom }));
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

  it("renders an inactive unordered marker in place (no replace) so the source keeps its width", () => {
    // Cursor on line 1 leaves line 2 inactive. A 2-line doc keeps the tested row
    // inside jsdom's rendered viewport (which has no real height).
    const view = editor("intro\n- bullet item", EditorSelection.cursor(0));

    // The dash is never replaced — it stays in the DOM (painted transparent, with
    // "•" drawn as a CSS overlay), so it keeps its exact width and the row can't
    // shift horizontally when clicked into edit view.
    const bullets = Array.from(view.dom.querySelectorAll<HTMLElement>(".cm-md-list-bullet")).map((item) => item.textContent);
    expect(bullets).toEqual(["-"]);
    // No replace widget remains for the bullet marker.
    expect(view.dom.querySelectorAll(".cm-md-list-widget").length).toBe(0);
    expect(view.state.doc.toString()).toBe("intro\n- bullet item");

    view.destroy();
  });

  it("renders an inactive ordered marker in place, showing the real source number", () => {
    const view = editor("intro\n1. ordered item", EditorSelection.cursor(0));

    // Ordered marker keeps the real "1." in place (just restyled), so nothing moves
    // between rendered and edit views.
    const numbers = Array.from(view.dom.querySelectorAll<HTMLElement>(".cm-md-list-number")).map((item) => item.textContent);
    expect(numbers).toEqual(["1."]);
    expect(view.dom.querySelectorAll(".cm-md-list-widget").length).toBe(0);
    expect(view.state.doc.toString()).toBe("intro\n1. ordered item");

    view.destroy();
  });

  it("keeps the heading '#' marker rendered in both states so the line doesn't shift", () => {
    // Inactive heading (cursor on the intro line): "# " is muted, never collapsed/replaced.
    const inactive = editor("intro\n# Heading", EditorSelection.cursor(0));
    expect(inactive.dom.querySelector<HTMLElement>(".cm-md-heading-marker.cm-md-muted-marker")?.textContent).toBe("# ");
    expect(inactive.dom.querySelectorAll(".cm-md-hidden-marker").length).toBe(0);
    expect(inactive.state.doc.toString()).toBe("intro\n# Heading");
    inactive.destroy();

    // Active heading (cursor on the heading line): the same "# " shows the full-ink active
    // style — same width, only the colour changes, so the text keeps its position.
    const active = editor("# Heading", EditorSelection.cursor(3));
    expect(active.dom.querySelector<HTMLElement>(".cm-md-heading-marker.cm-md-heading-active")?.textContent).toBe("# ");
    active.destroy();
  });
});
