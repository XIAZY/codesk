// Code-file syntax highlighting (task #2). Applies ONLY to non-Markdown documents — the
// DocumentSurface !enableMarkdownLivePreview branch. Markdown is never touched here.
//
// Detection is by explicit file extension (documentExtension), never guessed from content.
// Grammars are dynamic-imported so they stay out of the main bundle and load on demand; the
// caller (DocumentSurface) owns race-safety (never apply a resolved grammar to a doc that has
// since changed) and the plain-monospace fallback for an unmapped extension or a failed import.
import {
  HighlightStyle,
  StreamLanguage,
  syntaxHighlighting,
} from "@codemirror/language";
import type { Extension } from "@codemirror/state";
import { tags as t } from "@lezer/highlight";

// Extension (lowercased, incl. leading dot) → async grammar loader. Explicit language variants:
// .js/.jsx are JavaScript, .ts/.tsx are TypeScript (with JSX for the x variants). The LaTeX
// family shares the legacy stEx StreamLanguage; .bib is intentionally absent (stays plain unless
// a real BibTeX grammar is added). Any extension not here → plain monospace, no colours.
const GRAMMAR_LOADERS: Record<string, () => Promise<Extension>> = {
  ".js": () => import("@codemirror/lang-javascript").then((m) => m.javascript()),
  ".jsx": () => import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true })),
  ".ts": () => import("@codemirror/lang-javascript").then((m) => m.javascript({ typescript: true })),
  ".tsx": () => import("@codemirror/lang-javascript").then((m) => m.javascript({ typescript: true, jsx: true })),
  ".css": () => import("@codemirror/lang-css").then((m) => m.css()),
  ".html": () => import("@codemirror/lang-html").then((m) => m.html()),
  ".htm": () => import("@codemirror/lang-html").then((m) => m.html()),
  ".py": () => import("@codemirror/lang-python").then((m) => m.python()),
  ".json": () => import("@codemirror/lang-json").then((m) => m.json()),
  ".go": () => import("@codemirror/lang-go").then((m) => m.go()),
  ".rs": () => import("@codemirror/lang-rust").then((m) => m.rust()),
  ".sh": () => import("@codemirror/legacy-modes/mode/shell").then((m) => StreamLanguage.define(m.shell)),
  ".bash": () => import("@codemirror/legacy-modes/mode/shell").then((m) => StreamLanguage.define(m.shell)),
  ".tex": () => import("@codemirror/legacy-modes/mode/stex").then((m) => StreamLanguage.define(m.stex)),
  ".ltx": () => import("@codemirror/legacy-modes/mode/stex").then((m) => StreamLanguage.define(m.stex)),
  ".sty": () => import("@codemirror/legacy-modes/mode/stex").then((m) => StreamLanguage.define(m.stex)),
  ".cls": () => import("@codemirror/legacy-modes/mode/stex").then((m) => StreamLanguage.define(m.stex)),
};

// The set of recognised extensions (lowercased). Exposed for tests + the caller's fast check.
export const CODE_FILE_EXTENSIONS = Object.freeze(Object.keys(GRAMMAR_LOADERS));

/** Returns the async grammar loader for a path's extension, or null for an unmapped type
 *  (which the caller renders as plain monospace). Case-insensitive: `.PY` resolves like `.py`. */
export function grammarLoaderForExtension(extension: string): (() => Promise<Extension>) | null {
  return GRAMMAR_LOADERS[extension.toLowerCase()] ?? null;
}

// One HighlightStyle for every language, so a semantic role gets the SAME colour everywhere.
// Colours are `--code-*` tokens (defined per theme in styles.css, each ≥4.5:1 on that theme's
// editor background). Comments carry italic in addition to colour, so colour is never the sole
// legibility cue. Chroma is kept low — a code file should still read like a warm document.
export const codeHighlightStyle = HighlightStyle.define([
  { tag: [t.comment, t.lineComment, t.blockComment, t.docComment], color: "var(--code-comment)", fontStyle: "italic" },
  { tag: [t.keyword, t.controlKeyword, t.moduleKeyword, t.operatorKeyword, t.definitionKeyword], color: "var(--code-kw)" },
  { tag: [t.string, t.special(t.string), t.regexp, t.character], color: "var(--code-str)" },
  { tag: [t.number, t.bool, t.null, t.atom, t.constant(t.name)], color: "var(--code-num)" },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.definition(t.function(t.variableName))], color: "var(--code-fn)" },
  { tag: [t.typeName, t.className, t.namespace, t.tagName, t.attributeName], color: "var(--code-type)" },
  { tag: [t.operator, t.punctuation, t.bracket, t.separator, t.derefOperator, t.paren, t.brace, t.squareBracket], color: "var(--code-punc)" },
  { tag: [t.meta, t.processingInstruction], color: "var(--code-comment)" },
]);

/** The highlight extension for the code-file editor. Pair with a language grammar (added via a
 *  Compartment) — on its own it colours nothing, which is exactly the plain-mono fallback. */
export const codeHighlightExtension: Extension = syntaxHighlighting(codeHighlightStyle);
