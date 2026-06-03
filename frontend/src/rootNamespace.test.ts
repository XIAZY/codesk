import { describe, expect, it } from "vitest";
import * as Y from "yjs";
import {
  decodeRootEntries,
  moveRootFileEntry,
  projectRootDocuments,
  tombstoneRootFileEntry,
  upsertRootFileEntry,
} from "./rootNamespace";
import type { DocumentItem } from "./types";

describe("root namespace projection", () => {
  it("projects active root entries with supplemental stream metadata", () => {
    const doc = new Y.Doc();
    upsertRootFileEntry(doc, "doc_1", "docs/spec.md");

    const projected = projectRootDocuments(doc, [
      streamDocument({ id: "doc_1", path: "legacy/path.md", title: "legacy", updateId: 7, updatedAt: "2026-06-01T00:00:00Z" }),
    ]);

    expect(projected).toEqual([
      expect.objectContaining({
        id: "doc_1",
        path: "docs/spec.md",
        title: "spec.md",
        updateId: 7,
        updatedAt: "2026-06-01T00:00:00Z",
      }),
    ]);
  });

  it("renames and tombstones through the root entry without changing content document id", () => {
    const doc = new Y.Doc();
    upsertRootFileEntry(doc, "doc_1", "docs/spec.md");
    moveRootFileEntry(doc, "doc_1", "notes/spec.md");

    expect(decodeRootEntries(doc)).toEqual([
      expect.objectContaining({ contentDocumentId: "doc_1", path: "notes/spec.md", deleted: false }),
    ]);

    tombstoneRootFileEntry(doc, "doc_1");

    expect(decodeRootEntries(doc)).toEqual([
      expect.objectContaining({ contentDocumentId: "doc_1", path: "notes/spec.md", deleted: true }),
    ]);
    expect(projectRootDocuments(doc, [streamDocument({ id: "doc_1" })])).toEqual([]);
  });

  it("converges root entry updates across Yjs replicas", () => {
    const local = new Y.Doc();
    const remote = new Y.Doc();
    upsertRootFileEntry(local, "doc_1", "docs/spec.md");

    Y.applyUpdate(remote, Y.encodeStateAsUpdate(local));
    moveRootFileEntry(remote, "doc_1", "docs/renamed.md");
    Y.applyUpdate(local, Y.encodeStateAsUpdate(remote));

    expect(projectRootDocuments(local, [streamDocument({ id: "doc_1" })])).toEqual([
      expect.objectContaining({ id: "doc_1", path: "docs/renamed.md" }),
    ]);
  });

  it("ignores malformed and non-file entries", () => {
    const doc = new Y.Doc();
    const root = doc.getMap("root");
    const entries = new Y.Map<unknown>();
    const invalid = new Y.Map<unknown>();
    invalid.set("kind", "folder");
    invalid.set("contentDocumentId", "doc_folder");
    entries.set("folder", invalid);
    entries.set("string-entry", "not a map");
    root.set("entriesById", entries);

    expect(decodeRootEntries(doc)).toEqual([]);
    expect(projectRootDocuments(doc, [])).toEqual([]);
  });
});

function streamDocument(input: Partial<DocumentItem> & { id: string }): DocumentItem {
  return {
    id: input.id,
    path: input.path ?? "legacy.md",
    title: input.title ?? "legacy.md",
    updatedAt: input.updatedAt ?? "",
    updateId: input.updateId,
    stateVector: input.stateVector,
    clientIdSeed: input.clientIdSeed,
  };
}
