import { describe, it, expect } from "vitest";
import {
  grammarLoaderForExtension,
  CODE_FILE_EXTENSIONS,
  codeHighlightStyle,
  codeHighlightExtension,
} from "./codeHighlight";

describe("grammarLoaderForExtension", () => {
  it("returns a loader for every recognized extension", () => {
    const recognized = [
      ".js", ".jsx", ".ts", ".tsx", ".css", ".html", ".htm",
      ".py", ".json", ".go", ".rs", ".sh", ".bash",
      ".tex", ".ltx", ".sty", ".cls",
    ];
    for (const ext of recognized) {
      expect(typeof grammarLoaderForExtension(ext)).toBe("function");
    }
  });

  it("matches extensions case-insensitively", () => {
    expect(grammarLoaderForExtension(".PY")).toBe(grammarLoaderForExtension(".py"));
    expect(typeof grammarLoaderForExtension(".TeX")).toBe("function");
    expect(typeof grammarLoaderForExtension(".JSON")).toBe("function");
  });

  it("uses explicit per-variant loaders for js/ts/tsx, not one shared JS parser", () => {
    const js = grammarLoaderForExtension(".js");
    const ts = grammarLoaderForExtension(".ts");
    const tsx = grammarLoaderForExtension(".tsx");
    // Distinct loader references → each configures its own grammar (JS vs TS vs TS+JSX).
    expect(js).not.toBe(ts);
    expect(ts).not.toBe(tsx);
    expect(js).not.toBe(tsx);
  });

  it("maps the whole LaTeX family, but keeps .bib plain", () => {
    for (const ext of [".tex", ".ltx", ".sty", ".cls"]) {
      expect(typeof grammarLoaderForExtension(ext)).toBe("function");
    }
    expect(grammarLoaderForExtension(".bib")).toBeNull();
  });

  it("returns null for unmapped, markdown, or missing extensions (plain-mono fallback)", () => {
    for (const ext of [".xyz", ".md", ".markdown", ".txt", "", "."]) {
      expect(grammarLoaderForExtension(ext)).toBeNull();
    }
  });

  it("a recognized loader resolves to a usable CodeMirror extension", async () => {
    const loader = grammarLoaderForExtension(".py");
    expect(loader).not.toBeNull();
    const grammar = await loader!();
    expect(grammar).toBeTruthy();
  });

  it("resolves the LaTeX (stEx) legacy grammar", async () => {
    const grammar = await grammarLoaderForExtension(".tex")!();
    expect(grammar).toBeTruthy();
  });
});

describe("code highlight palette", () => {
  it("CODE_FILE_EXTENSIONS lists the recognized set and excludes markdown/.bib", () => {
    expect(CODE_FILE_EXTENSIONS).toContain(".py");
    expect(CODE_FILE_EXTENSIONS).toContain(".tex");
    expect(CODE_FILE_EXTENSIONS).not.toContain(".bib");
    expect(CODE_FILE_EXTENSIONS).not.toContain(".md");
  });

  it("exposes a HighlightStyle and its syntaxHighlighting extension", () => {
    expect(codeHighlightStyle).toBeTruthy();
    expect(codeHighlightExtension).toBeTruthy();
  });
});
