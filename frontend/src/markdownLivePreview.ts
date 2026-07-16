import { syntaxTree } from "@codemirror/language";
import {
  EditorSelection,
  type ChangeSpec,
  type EditorState,
  type Extension,
  type SelectionRange,
} from "@codemirror/state";
import {
  Decoration,
  type DecorationSet,
  EditorView,
  type PluginValue,
  ViewPlugin,
  type ViewUpdate,
  type Command,
  WidgetType,
} from "@codemirror/view";

type SourceRange = {
  from: number;
  to: number;
};

export type MarkdownPreviewToken = {
  kind:
    | "heading"
    | "heading-marker"
    | "strong"
    | "strong-marker"
    | "emphasis"
    | "emphasis-marker"
    | "strike"
    | "strike-marker"
    | "inline-code"
    | "inline-code-marker"
    | "link-label"
    | "link-syntax"
    | "image-alt"
    | "image-syntax"
    | "url"
    | "quote-marker"
    | "list-marker"
    | "task-marker"
    | "code-fence"
    | "code-fence-marker"
    | "table"
    | "table-marker"
    | "horizontal-rule";
  from: number;
  to: number;
  active: boolean;
  level?: number;
  checked?: boolean;
  ordered?: boolean;
  taskLine?: boolean;
};

export type MarkdownPreviewCommandName =
  | "bold"
  | "italic"
  | "code"
  | "heading1"
  | "heading2"
  | "heading3"
  | "quote"
  | "bulletList"
  | "link";

export function nottyMarkdownLivePreview(): Extension {
  return [markdownPreviewTheme, markdownPreviewPlugin];
}

export function markdownPreviewCommand(name: MarkdownPreviewCommandName): Command {
  switch (name) {
    case "bold":
      return toggleAround("**", "**", "bold text");
    case "italic":
      return toggleAround("*", "*", "italic text");
    case "code":
      return toggleAround("`", "`", "code");
    case "heading1":
      return toggleHeading(1);
    case "heading2":
      return toggleHeading(2);
    case "heading3":
      return toggleHeading(3);
    case "quote":
      return toggleLinePrefix("> ");
    case "bulletList":
      return toggleLinePrefix("- ");
    case "link":
      return createLink;
  }
}

class MarkdownPreviewPlugin implements PluginValue {
  decorations: DecorationSet;

  constructor(view: EditorView) {
    this.decorations = buildMarkdownPreviewDecorations(view);
  }

  update(update: ViewUpdate) {
    if (update.docChanged || update.selectionSet || update.viewportChanged || update.geometryChanged) {
      this.decorations = buildMarkdownPreviewDecorations(update.view);
    }
  }
}

const markdownPreviewPlugin = ViewPlugin.fromClass(MarkdownPreviewPlugin, {
  decorations: (plugin) => plugin.decorations,
});

function buildMarkdownPreviewDecorations(view: EditorView): DecorationSet {
  const activeRanges = activeRangesFromState(view.state);
  const visibleRanges = view.visibleRanges.length ? view.visibleRanges : [{ from: 0, to: view.state.doc.length }];
  const tokens = collectMarkdownPreviewTokens(view.state, visibleRanges, activeRanges);
  const decorations: ReturnType<Decoration["range"]>[] = [];

  for (const token of tokens) {
    if (token.to <= token.from) {
      continue;
    }
    switch (token.kind) {
      case "heading":
        decorations.push(Decoration.line({ class: `cm-md-heading cm-md-heading-${token.level ?? 1}` }).range(lineStart(view.state, token.from)));
        break;
      case "heading-marker":
        decorations.push(markerDecoration(token, "cm-md-heading-marker"));
        break;
      case "strong":
        decorations.push(Decoration.mark({ class: "cm-md-strong" }).range(token.from, token.to));
        break;
      case "strong-marker":
        decorations.push(markerDecoration(token, "cm-md-format-marker"));
        break;
      case "emphasis":
        decorations.push(Decoration.mark({ class: "cm-md-emphasis" }).range(token.from, token.to));
        break;
      case "emphasis-marker":
        decorations.push(markerDecoration(token, "cm-md-format-marker"));
        break;
      case "strike":
        decorations.push(Decoration.mark({ class: "cm-md-strike" }).range(token.from, token.to));
        break;
      case "strike-marker":
        decorations.push(markerDecoration(token, "cm-md-format-marker"));
        break;
      case "inline-code":
        decorations.push(Decoration.mark({ class: "cm-md-inline-code" }).range(token.from, token.to));
        break;
      case "inline-code-marker":
        decorations.push(markerDecoration(token, "cm-md-code-marker"));
        break;
      case "link-label":
        decorations.push(Decoration.mark({ class: "cm-md-link-label" }).range(token.from, token.to));
        break;
      case "link-syntax":
        decorations.push(markerDecoration(token, "cm-md-link-syntax"));
        break;
      case "image-alt":
        decorations.push(Decoration.mark({ class: "cm-md-image-alt" }).range(token.from, token.to));
        break;
      case "image-syntax":
        decorations.push(markerDecoration(token, "cm-md-image-syntax"));
        break;
      case "url":
        decorations.push(Decoration.mark({ class: "cm-md-url" }).range(token.from, token.to));
        break;
      case "quote-marker":
        decorations.push(Decoration.mark({ class: "cm-md-block-marker" }).range(token.from, token.to));
        decorations.push(Decoration.line({ class: "cm-md-quote-line" }).range(lineStart(view.state, token.from)));
        break;
      case "list-marker":
        decorations.push(listMarkerDecoration(token).range(token.from, token.to));
        decorations.push(Decoration.line({ class: "cm-md-list-line" }).range(lineStart(view.state, token.from)));
        break;
      case "task-marker":
        decorations.push(taskMarkerDecoration(token).range(token.from, token.to));
        break;
      case "code-fence":
        for (let pos = lineStart(view.state, token.from); pos <= token.to; ) {
          decorations.push(Decoration.line({ class: "cm-md-code-fence-line" }).range(pos));
          const line = view.state.doc.lineAt(pos);
          if (line.to >= token.to || line.number >= view.state.doc.lines) {
            break;
          }
          pos = view.state.doc.line(line.number + 1).from;
        }
        break;
      case "code-fence-marker":
        decorations.push(Decoration.mark({ class: "cm-md-code-fence-marker" }).range(token.from, token.to));
        break;
      case "table":
        for (let pos = lineStart(view.state, token.from); pos <= token.to; ) {
          decorations.push(Decoration.line({ class: "cm-md-table-line" }).range(pos));
          const line = view.state.doc.lineAt(pos);
          if (line.to >= token.to || line.number >= view.state.doc.lines) {
            break;
          }
          pos = view.state.doc.line(line.number + 1).from;
        }
        break;
      case "table-marker":
        decorations.push(Decoration.mark({ class: "cm-md-table-marker" }).range(token.from, token.to));
        break;
      case "horizontal-rule":
        decorations.push(Decoration.line({ class: "cm-md-horizontal-rule" }).range(lineStart(view.state, token.from)));
        break;
    }
  }

  return Decoration.set(decorations, true);
}

function markerDecoration(token: MarkdownPreviewToken, className: string) {
  if (!token.active) {
    return Decoration.replace({}).range(token.from, token.to);
  }
  return Decoration.mark({ class: `${className} cm-md-visible-marker` }).range(token.from, token.to);
}

function listMarkerDecoration(token: MarkdownPreviewToken) {
  if (token.active) {
    return Decoration.mark({ class: "cm-md-block-marker" });
  }
  if (token.taskLine) {
    return Decoration.replace({});
  }
  // Keep the source marker in place (never replace it) so it always occupies its
  // natural width — the rendered form takes exactly the same space as the raw
  // "- "/"1." and the line cannot shift horizontally when the row is clicked.
  // Unordered: hide the dash and paint "•" as an out-of-flow overlay. Ordered:
  // show the real number, just styled.
  return Decoration.mark({
    class: token.ordered ? "cm-md-list-number" : "cm-md-list-bullet",
  });
}

function taskMarkerDecoration(token: MarkdownPreviewToken) {
  if (token.active) {
    return Decoration.mark({ class: "cm-md-task-marker cm-md-visible-marker" });
  }
  return Decoration.replace({
    widget: new TaskCheckboxWidget(token.from, token.to, Boolean(token.checked)),
  });
}

class TaskCheckboxWidget extends WidgetType {
  constructor(
    private from: number,
    private to: number,
    private checked: boolean
  ) {
    super();
  }

  eq(other: TaskCheckboxWidget) {
    return this.from === other.from && this.to === other.to && this.checked === other.checked;
  }

  toDOM(view: EditorView) {
    const label = document.createElement("label");
    label.className = "cm-md-task-checkbox";
    label.title = this.checked ? "Mark task incomplete" : "Mark task complete";

    const input = document.createElement("input");
    input.type = "checkbox";
    input.checked = this.checked;
    input.setAttribute("aria-label", label.title);
    input.addEventListener("mousedown", (event) => event.preventDefault());
    input.addEventListener("click", (event) => {
      event.preventDefault();
      view.dispatch({
        changes: {
          from: this.from,
          to: this.to,
          insert: this.checked ? "[ ]" : "[x]",
        },
        selection: { anchor: this.to },
        scrollIntoView: true,
      });
      view.focus();
    });

    label.append(input);
    return label;
  }

  ignoreEvent() {
    return false;
  }
}

export function collectMarkdownPreviewTokens(
  state: EditorState,
  visibleRanges: readonly SourceRange[] = [{ from: 0, to: state.doc.length }],
  activeRanges: readonly SourceRange[] = activeRangesFromState(state)
): MarkdownPreviewToken[] {
  const tokens: MarkdownPreviewToken[] = [];
  const seen = new Set<string>();
  const tree = syntaxTree(state);

  for (const range of visibleRanges) {
    tree.iterate({
      from: range.from,
      to: range.to,
      enter(node) {
        const key = `${node.name}:${node.from}:${node.to}`;
        if (seen.has(key)) {
          return;
        }
        seen.add(key);
        const active = rangeIsActive(state, node.from, node.to, activeRanges);
        const source = state.doc.sliceString(node.from, node.to);

        if (/^ATXHeading([1-6])$/.test(node.name)) {
          const level = Number(node.name.replace("ATXHeading", ""));
          tokens.push({ kind: "heading", from: node.from, to: node.to, active, level });
          const marker = /^(#{1,6})([ \t]+|$)/.exec(source);
          if (marker) {
            tokens.push({ kind: "heading-marker", from: node.from, to: node.from + marker[0].length, active, level });
          }
          return;
        }

        if (node.name === "SetextHeading1" || node.name === "SetextHeading2") {
          tokens.push({ kind: "heading", from: node.from, to: node.to, active, level: node.name === "SetextHeading1" ? 1 : 2 });
          return;
        }

        if (node.name === "StrongEmphasis") {
          addWrappedInlineTokens(tokens, "strong", "strong-marker", node.from, node.to, source, active, ["**", "__"]);
          return;
        }

        if (node.name === "Emphasis") {
          addWrappedInlineTokens(tokens, "emphasis", "emphasis-marker", node.from, node.to, source, active, ["*", "_"]);
          return;
        }

        if (node.name === "Strikethrough") {
          addWrappedInlineTokens(tokens, "strike", "strike-marker", node.from, node.to, source, active, ["~~"]);
          return;
        }

        if (node.name === "InlineCode") {
          const tickCount = leadingRun(source, "`");
          if (tickCount > 0 && source.endsWith("`".repeat(tickCount)) && source.length > tickCount * 2) {
            tokens.push({ kind: "inline-code-marker", from: node.from, to: node.from + tickCount, active });
            tokens.push({ kind: "inline-code", from: node.from + tickCount, to: node.to - tickCount, active });
            tokens.push({ kind: "inline-code-marker", from: node.to - tickCount, to: node.to, active });
          }
          return;
        }

        if (node.name === "Link") {
          addInlineLinkTokens(tokens, node.from, node.to, source, active);
          return;
        }

        if (node.name === "Image") {
          addImageTokens(tokens, node.from, node.to, source, active);
          return;
        }

        if (node.name === "Autolink") {
          addAutolinkTokens(tokens, node.from, node.to, source, active);
          return;
        }

        if (node.name === "URL") {
          tokens.push({ kind: "url", from: node.from, to: node.to, active });
          return;
        }

        if (node.name === "QuoteMark") {
          tokens.push({ kind: "quote-marker", from: node.from, to: node.to, active });
          return;
        }

        if (node.name === "ListMark") {
          const line = state.doc.lineAt(node.from);
          const lineText = line.text;
          const ordered = /^\d+[.)]$/.test(source);
          const taskLine = hasTaskMarkerAfterListMarker(lineText, node.from - line.from, node.to - line.from);
          tokens.push({ kind: "list-marker", from: node.from, to: node.to, active, ordered, taskLine });
          return;
        }

        if (node.name === "TaskMarker") {
          tokens.push({ kind: "task-marker", from: node.from, to: node.to, active, checked: /^\[[xX]\]$/.test(source) });
          return;
        }

        if (node.name === "FencedCode") {
          tokens.push({ kind: "code-fence", from: node.from, to: node.to, active });
          return;
        }

        if ((node.name === "CodeMark" && (/^`{3,}$/.test(source) || /^~{3,}$/.test(source))) || node.name === "CodeInfo") {
          tokens.push({ kind: "code-fence-marker", from: node.from, to: node.to, active });
          return;
        }

        if (node.name === "Table") {
          tokens.push({ kind: "table", from: node.from, to: node.to, active });
          return;
        }

        if (node.name === "TableDelimiter") {
          tokens.push({ kind: "table-marker", from: node.from, to: node.to, active });
          return;
        }

        if (node.name === "HorizontalRule") {
          tokens.push({ kind: "horizontal-rule", from: node.from, to: node.to, active });
        }
      },
    });
  }

  return tokens.sort((left, right) => left.from - right.from || left.to - right.to || left.kind.localeCompare(right.kind));
}

function addWrappedInlineTokens(
  tokens: MarkdownPreviewToken[],
  contentKind: MarkdownPreviewToken["kind"],
  markerKind: MarkdownPreviewToken["kind"],
  from: number,
  to: number,
  source: string,
  active: boolean,
  markers: string[]
) {
  const marker = markers.find((candidate) => source.startsWith(candidate) && source.endsWith(candidate) && source.length > candidate.length * 2);
  if (!marker) {
    return;
  }
  tokens.push({ kind: markerKind, from, to: from + marker.length, active });
  tokens.push({ kind: contentKind, from: from + marker.length, to: to - marker.length, active });
  tokens.push({ kind: markerKind, from: to - marker.length, to, active });
}

function addInlineLinkTokens(tokens: MarkdownPreviewToken[], from: number, to: number, source: string, active: boolean) {
  const match = /^\[([^\]\n]+)\]\(([^)\n]+)\)$/.exec(source);
  if (!match) {
    return;
  }
  const labelStart = from + 1;
  const labelEnd = labelStart + match[1].length;
  tokens.push({ kind: "link-syntax", from, to: labelStart, active });
  tokens.push({ kind: "link-label", from: labelStart, to: labelEnd, active });
  tokens.push({ kind: "link-syntax", from: labelEnd, to, active });
}

function addImageTokens(tokens: MarkdownPreviewToken[], from: number, to: number, source: string, active: boolean) {
  const match = /^!\[([^\]\n]*)\]\(([^)\n]+)\)$/.exec(source);
  if (!match) {
    return;
  }
  const altStart = from + 2;
  const altEnd = altStart + match[1].length;
  tokens.push({ kind: "image-syntax", from, to: altStart, active });
  if (altEnd > altStart) {
    tokens.push({ kind: "image-alt", from: altStart, to: altEnd, active });
  }
  tokens.push({ kind: "image-syntax", from: altEnd, to, active });
}

function addAutolinkTokens(tokens: MarkdownPreviewToken[], from: number, to: number, source: string, active: boolean) {
  if (!source.startsWith("<") || !source.endsWith(">") || source.length <= 2) {
    return;
  }
  tokens.push({ kind: "link-syntax", from, to: from + 1, active });
  tokens.push({ kind: "url", from: from + 1, to: to - 1, active });
  tokens.push({ kind: "link-syntax", from: to - 1, to, active });
}

function hasTaskMarkerAfterListMarker(lineText: string, markerFrom: number, markerTo: number) {
  const beforeMarker = lineText.slice(0, markerFrom);
  if (beforeMarker.trim().length > 0) {
    return false;
  }
  return /^[ \t]+\[[ xX]\]/.test(lineText.slice(markerTo));
}

function activeRangesFromState(state: EditorState): SourceRange[] {
  return state.selection.ranges.map((range) => {
    if (!range.empty) {
      return { from: range.from, to: range.to };
    }
    const line = state.doc.lineAt(range.from);
    return { from: line.from, to: line.to };
  });
}

function rangeIsActive(state: EditorState, from: number, to: number, activeRanges: readonly SourceRange[]) {
  if (activeRanges.length === 0) {
    return false;
  }
  return activeRanges.some((range) => {
    if (range.from === range.to) {
      return range.from >= from && range.from <= to;
    }
    if (rangesOverlap(range.from, range.to, from, to)) {
      return true;
    }
    const activeLine = state.doc.lineAt(range.from);
    return activeLine.from <= from && activeLine.to >= to;
  });
}

function rangesOverlap(leftFrom: number, leftTo: number, rightFrom: number, rightTo: number) {
  return leftFrom < rightTo && rightFrom < leftTo;
}

function lineStart(state: EditorState, pos: number) {
  return state.doc.lineAt(Math.max(0, Math.min(state.doc.length, pos))).from;
}

function leadingRun(source: string, char: string) {
  let count = 0;
  while (source[count] === char) {
    count += 1;
  }
  return count;
}

function toggleAround(prefix: string, suffix: string, placeholder: string): Command {
  return (view) => {
    const changes: ChangeSpec[] = [];
    const selections: SelectionRange[] = [];

    for (const range of view.state.selection.ranges) {
      const from = range.from;
      const to = range.to;
      const before = view.state.doc.sliceString(Math.max(0, from - prefix.length), from);
      const after = view.state.doc.sliceString(to, Math.min(view.state.doc.length, to + suffix.length));
      if (!range.empty && before === prefix && after === suffix) {
        changes.push({ from: to, to: to + suffix.length, insert: "" }, { from: from - prefix.length, to: from, insert: "" });
        selections.push(EditorSelection.range(from - prefix.length, to - prefix.length));
      } else if (!range.empty) {
        changes.push({ from: to, insert: suffix }, { from, insert: prefix });
        selections.push(EditorSelection.range(from + prefix.length, to + prefix.length));
      } else {
        const insert = `${prefix}${placeholder}${suffix}`;
        changes.push({ from, insert });
        selections.push(EditorSelection.range(from + prefix.length, from + prefix.length + placeholder.length));
      }
    }

    view.dispatch({ changes, selection: EditorSelection.create(selections), scrollIntoView: true });
    view.focus();
    return true;
  };
}

function toggleHeading(level: 1 | 2 | 3): Command {
  return (view) => {
    const marker = `${"#".repeat(level)} `;
    const changes: ChangeSpec[] = [];
    forEachSelectedLine(view.state, (line) => {
      const text = line.text;
      const existing = /^(#{1,6})[ \t]+/.exec(text);
      if (existing?.[1].length === level) {
        changes.push({ from: line.from, to: line.from + existing[0].length, insert: "" });
      } else if (existing) {
        changes.push({ from: line.from, to: line.from + existing[0].length, insert: marker });
      } else {
        changes.push({ from: line.from, insert: marker });
      }
    });
    view.dispatch({ changes, scrollIntoView: true });
    view.focus();
    return true;
  };
}

function toggleLinePrefix(prefix: string): Command {
  return (view) => {
    const lines: Array<{ from: number; text: string }> = [];
    forEachSelectedLine(view.state, (line) => lines.push({ from: line.from, text: line.text }));
    const allPrefixed = lines.length > 0 && lines.every((line) => line.text.startsWith(prefix));
    const changes = lines.map((line) =>
      allPrefixed
        ? { from: line.from, to: line.from + prefix.length, insert: "" }
        : { from: line.from, insert: prefix }
    );
    view.dispatch({ changes, scrollIntoView: true });
    view.focus();
    return true;
  };
}

function createLink(view: EditorView) {
  const changes: ChangeSpec[] = [];
  const selections: SelectionRange[] = [];
  for (const range of view.state.selection.ranges) {
    const selected = view.state.doc.sliceString(range.from, range.to) || "link text";
    const insert = `[${selected}](https://example.com)`;
    changes.push({ from: range.from, to: range.to, insert });
    const urlStart = range.from + selected.length + 3;
    selections.push(EditorSelection.range(urlStart, urlStart + "https://example.com".length));
  }
  view.dispatch({ changes, selection: EditorSelection.create(selections), scrollIntoView: true });
  view.focus();
  return true;
}

function forEachSelectedLine(state: EditorState, callback: (line: { from: number; to: number; text: string; number: number }) => void) {
  const visited = new Set<number>();
  for (const range of state.selection.ranges) {
    const fromLine = state.doc.lineAt(range.from);
    const toLine = state.doc.lineAt(Math.max(range.from, range.to - (range.to > range.from ? 1 : 0)));
    for (let lineNumber = fromLine.number; lineNumber <= toLine.number; lineNumber += 1) {
      if (visited.has(lineNumber)) {
        continue;
      }
      visited.add(lineNumber);
      const line = state.doc.line(lineNumber);
      callback({ from: line.from, to: line.to, text: line.text, number: line.number });
    }
  }
}

const markdownPreviewTheme = EditorView.theme({
  ".cm-md-heading": {
    fontFamily: "var(--editor-font)",
    color: "var(--ink)",
    fontWeight: "700",
  },
  ".cm-md-heading-1": {
    fontSize: "1.82em",
    lineHeight: "1.28",
    letterSpacing: "-0.035em",
  },
  ".cm-md-heading-2": {
    fontSize: "1.42em",
    lineHeight: "1.35",
    letterSpacing: "-0.026em",
  },
  ".cm-md-heading-3": {
    fontSize: "1.18em",
    lineHeight: "1.45",
  },
  ".cm-md-strong": {
    fontWeight: "750",
    color: "var(--ink)",
  },
  ".cm-md-emphasis": {
    fontStyle: "italic",
  },
  ".cm-md-strike": {
    textDecoration: "line-through",
    color: "var(--ink-3)",
  },
  ".cm-md-inline-code": {
    borderRadius: "6px",
    background: "rgba(38, 82, 68, 0.09)",
    color: "var(--ink)",
    fontFamily: "var(--mono)",
    fontSize: "0.92em",
    padding: "0 0.24em",
  },
  ".cm-md-link-label": {
    color: "var(--accent-700)",
    textDecoration: "underline",
    textUnderlineOffset: "3px",
    fontWeight: "620",
  },
  ".cm-md-url": {
    color: "var(--accent-700)",
    textDecoration: "underline",
    textUnderlineOffset: "3px",
  },
  ".cm-md-image-alt": {
    borderRadius: "7px",
    background: "rgba(215, 138, 75, 0.12)",
    color: "var(--ink)",
    fontWeight: "650",
    padding: "0 0.25em",
  },
  ".cm-md-visible-marker": {
    color: "rgba(91, 76, 58, 0.5)",
    fontFamily: "var(--editor-font)",
    fontSize: "0.88em",
  },
  ".cm-md-hidden-marker": {
    display: "none",
  },
  ".cm-md-block-marker": {
    color: "var(--accent)",
    fontWeight: "700",
  },
  // Unordered bullet: the source dash stays in place but is painted transparent so
  // it keeps its exact width; "•" is drawn as an out-of-flow overlay. Result: the
  // rendered bullet occupies the same space as the raw "- " and the line does not
  // shift when the row is clicked into edit view.
  ".cm-md-list-bullet": {
    color: "transparent",
    position: "relative",
  },
  ".cm-md-list-bullet::before": {
    content: '"\\2022"',
    position: "absolute",
    left: "0",
    top: "50%",
    transform: "translateY(-50%)",
    color: "var(--accent-700)",
    fontWeight: "800",
    pointerEvents: "none",
  },
  // Ordered marker: the real "1."/"2)" stays in place (identical to the raw form),
  // only recolored — so nothing moves between rendered and edit views.
  ".cm-md-list-number": {
    color: "var(--accent-700)",
    fontWeight: "700",
  },
  ".cm-md-quote-line": {
    borderLeft: "3px solid rgba(215, 138, 75, 0.5)",
    background: "rgba(215, 138, 75, 0.07)",
  },
  ".cm-md-list-line": {
    color: "var(--ink-2)",
  },
  ".cm-md-task-marker": {
    color: "var(--accent-700)",
    fontWeight: "700",
  },
  ".cm-md-task-checkbox": {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: "1.2em",
    marginRight: "0.36em",
    verticalAlign: "-0.12em",
  },
  ".cm-md-task-checkbox input": {
    width: "14px",
    height: "14px",
    margin: "0",
    accentColor: "var(--accent)",
    cursor: "pointer",
  },
  ".cm-md-code-fence-line": {
    background: "rgba(38, 82, 68, 0.08)",
    color: "var(--ink)",
    fontFamily: "var(--mono)",
  },
  ".cm-md-code-fence-marker": {
    color: "rgba(91, 76, 58, 0.58)",
    fontWeight: "600",
  },
  ".cm-md-table-line": {
    background: "rgba(38, 82, 68, 0.045)",
    fontFamily: "var(--editor-font)",
  },
  ".cm-md-table-marker": {
    color: "rgba(91, 76, 58, 0.45)",
  },
  ".cm-md-horizontal-rule": {
    color: "transparent",
    borderBottom: "1px solid var(--border)",
    height: "0.8em",
  },
});
