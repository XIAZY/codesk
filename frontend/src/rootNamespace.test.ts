import { describe, expect, it } from "vitest";
import * as Y from "yjs";
import {
  decodeRootEntries,
  moveRootFileEntry,
  projectRootDocuments,
  tombstoneRootFileEntry,
  upsertRootFileEntry,
} from "./rootNamespace";

describe("root namespace projection", () => {
  it("projects active root entries from the root CRDT document", () => {
    const doc = new Y.Doc();
    upsertRootFileEntry(doc, "doc_1", "docs/spec.md");

    const projected = projectRootDocuments(doc);

    expect(projected).toEqual([
      {
        id: "doc_1",
        path: "docs/spec.md",
        title: "spec.md",
      },
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
    expect(projectRootDocuments(doc)).toEqual([]);
  });

  it("converges root entry updates across Yjs replicas", () => {
    const local = new Y.Doc();
    const remote = new Y.Doc();
    upsertRootFileEntry(local, "doc_1", "docs/spec.md");

    Y.applyUpdate(remote, Y.encodeStateAsUpdate(local));
    moveRootFileEntry(remote, "doc_1", "docs/renamed.md");
    Y.applyUpdate(local, Y.encodeStateAsUpdate(remote));

    expect(projectRootDocuments(local)).toEqual([
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
    expect(projectRootDocuments(doc)).toEqual([]);
  });
});
