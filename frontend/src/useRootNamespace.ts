import { useCallback, useEffect, useMemo, useState } from "react";
import {
  moveRootFileEntry,
  projectRootDocuments,
  tombstoneRootFileEntry,
  upsertRootFileEntry,
} from "./rootNamespace";
import type { DocumentItem } from "./types";
import { useYDocSync } from "./useYDocSync";

export function useRootNamespace(input: {
  workspaceId: string;
  token: string;
  rootDocumentId: string;
  streamDocuments: DocumentItem[];
}) {
  const { ydoc, ready, connected } = useYDocSync({
    workspaceId: input.workspaceId,
    token: input.token,
    documentId: input.rootDocumentId,
  });
  const [version, setVersion] = useState(0);

  useEffect(() => {
    setVersion((value) => value + 1);
    const bump = () => setVersion((value) => value + 1);
    ydoc.on("update", bump);
    return () => {
      ydoc.off("update", bump);
    };
  }, [ydoc]);

  const documents = useMemo(
    () => projectRootDocuments(ydoc, input.streamDocuments),
    [input.streamDocuments, version, ydoc],
  );

  const upsertFile = useCallback(
    (documentId: string, path: string) => {
      upsertRootFileEntry(ydoc, documentId, path);
    },
    [ydoc],
  );

  const moveFile = useCallback(
    (documentId: string, path: string) => {
      moveRootFileEntry(ydoc, documentId, path);
    },
    [ydoc],
  );

  const tombstoneFile = useCallback(
    (documentId: string) => {
      tombstoneRootFileEntry(ydoc, documentId);
    },
    [ydoc],
  );

  return {
    rootDoc: ydoc,
    documents,
    ready,
    connected,
    upsertFile,
    moveFile,
    tombstoneFile,
  };
}
