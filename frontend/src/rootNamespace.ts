import * as Y from "yjs";
import type { DocumentItem } from "./types";

const rootMapName = "root";
const rootEntriesMapName = "entriesById";
const rootEntryKindFile = "file";
const rootDeletedTrue = "true";
const rootDeletedFalse = "false";

export type RootFileEntry = {
  entryId: string;
  kind: string;
  contentDocumentId: string;
  path: string;
  deleted: boolean;
};

type RootLoc = {
  parentId?: string;
  name?: string;
};

export function normalizeRootPath(value: string) {
  const trimmed = value.trim().replace(/\\/g, "/");
  if (!trimmed) {
    return "";
  }
  const parts: string[] = [];
  for (const part of trimmed.split("/")) {
    if (!part || part === ".") {
      continue;
    }
    if (part === "..") {
      parts.pop();
      continue;
    }
    parts.push(part);
  }
  return parts.join("/");
}

export function decodeRootEntries(doc: Y.Doc): RootFileEntry[] {
  const root = doc.getMap(rootMapName);
  const entries = getExistingEntriesMap(root);
  if (!entries) {
    return [];
  }
  const result: RootFileEntry[] = [];
  entries.forEach((value, entryId) => {
    if (!(value instanceof Y.Map)) {
      return;
    }
    const entry = decodeRootEntryMap(entryId, value);
    if (entry) {
      result.push(entry);
    }
  });
  return result.sort((left, right) => left.path.localeCompare(right.path) || left.contentDocumentId.localeCompare(right.contentDocumentId));
}

export function projectRootDocuments(doc: Y.Doc, streamDocuments: DocumentItem[]): DocumentItem[] {
  const streamById = new Map(streamDocuments.map((document) => [document.id, document]));
  return decodeRootEntries(doc)
    .filter((entry) => !entry.deleted && entry.path)
    .map((entry) => {
      const stream = streamById.get(entry.contentDocumentId);
      return {
        id: entry.contentDocumentId,
        path: entry.path,
        title: titleFromPath(entry.path),
        stateVector: stream?.stateVector,
        updateId: stream?.updateId,
        updatedAt: stream?.updatedAt ?? "",
        clientIdSeed: stream?.clientIdSeed,
      };
    });
}

export function upsertRootFileEntry(doc: Y.Doc, documentId: string, path: string) {
  const normalizedPath = normalizeRootPath(path);
  if (!documentId.trim() || !normalizedPath) {
    return;
  }
  doc.transact(() => {
    const entries = getOrCreateEntriesMap(doc);
    const entryMap = getOrCreateEntryMap(entries, documentId);
    entryMap.set("kind", rootEntryKindFile);
    entryMap.set("contentDocumentId", documentId);
    entryMap.set("loc", rootEntryPathLoc(normalizedPath));
    entryMap.set("deleted", rootDeletedFalse);
  }, "root-namespace");
}

export function moveRootFileEntry(doc: Y.Doc, documentId: string, path: string) {
  const normalizedPath = normalizeRootPath(path);
  if (!documentId.trim() || !normalizedPath) {
    return;
  }
  doc.transact(() => {
    const entries = getOrCreateEntriesMap(doc);
    const entryMap = findEntryMap(entries, documentId) ?? getOrCreateEntryMap(entries, documentId);
    entryMap.set("kind", rootEntryKindFile);
    entryMap.set("contentDocumentId", documentId);
    entryMap.set("loc", rootEntryPathLoc(normalizedPath));
    entryMap.set("deleted", rootDeletedFalse);
  }, "root-namespace");
}

export function tombstoneRootFileEntry(doc: Y.Doc, documentId: string) {
  if (!documentId.trim()) {
    return;
  }
  doc.transact(() => {
    const entries = getOrCreateEntriesMap(doc);
    const entryMap = findEntryMap(entries, documentId);
    if (!entryMap) {
      return;
    }
    entryMap.set("deleted", rootDeletedTrue);
  }, "root-namespace");
}

function decodeRootEntryMap(entryId: string, entryMap: Y.Map<unknown>): RootFileEntry | null {
  const kind = stringValue(entryMap.get("kind")) || rootEntryKindFile;
  if (kind !== rootEntryKindFile) {
    return null;
  }
  const contentDocumentId = stringValue(entryMap.get("contentDocumentId"));
  if (!contentDocumentId) {
    return null;
  }
  const loc = parseLoc(stringValue(entryMap.get("loc")));
  const path = loc.parentId ? normalizeRootPath(`${loc.parentId}/${loc.name ?? ""}`) : normalizeRootPath(loc.name ?? "");
  return {
    entryId,
    kind,
    contentDocumentId,
    path,
    deleted: stringValue(entryMap.get("deleted")).toLowerCase() === rootDeletedTrue,
  };
}

function parseLoc(value: string): RootLoc {
  if (!value) {
    return {};
  }
  try {
    const parsed = JSON.parse(value) as RootLoc;
    return {
      parentId: normalizeRootPath(String(parsed.parentId ?? "")),
      name: normalizeRootPath(String(parsed.name ?? "")),
    };
  } catch {
    return {};
  }
}

function rootEntryPathLoc(path: string) {
  return JSON.stringify({ parentId: "", name: normalizeRootPath(path) });
}

function getExistingEntriesMap(root: Y.Map<unknown>) {
  const entries = root.get(rootEntriesMapName);
  return entries instanceof Y.Map ? (entries as Y.Map<unknown>) : null;
}

function getOrCreateEntriesMap(doc: Y.Doc) {
  const root = doc.getMap(rootMapName);
  const existing = getExistingEntriesMap(root);
  if (existing) {
    return existing;
  }
  const entries = new Y.Map<unknown>();
  root.set(rootEntriesMapName, entries);
  return entries;
}

function getOrCreateEntryMap(entries: Y.Map<unknown>, entryId: string) {
  const existing = entries.get(entryId);
  if (existing instanceof Y.Map) {
    return existing as Y.Map<unknown>;
  }
  const entryMap = new Y.Map<unknown>();
  entries.set(entryId, entryMap);
  return entryMap;
}

function findEntryMap(entries: Y.Map<unknown>, documentId: string) {
  const direct = entries.get(documentId);
  if (direct instanceof Y.Map) {
    return direct as Y.Map<unknown>;
  }
  let found: Y.Map<unknown> | null = null;
  entries.forEach((value) => {
    if (found || !(value instanceof Y.Map)) {
      return;
    }
    if (stringValue(value.get("contentDocumentId")) === documentId) {
      found = value;
    }
  });
  return found;
}

function titleFromPath(path: string) {
  return path.split("/").filter(Boolean).pop() || path;
}

function stringValue(value: unknown) {
  return typeof value === "string" ? value.trim() : "";
}
