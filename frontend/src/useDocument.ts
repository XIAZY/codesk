import { useMemo } from "react";
import { useYDocSync } from "./useYDocSync";
import type { DocumentItem } from "./types";

export function useDocumentSync(input: {
  workspaceId: string;
  token: string;
  document: DocumentItem | null;
  actorName: string;
}) {
  const documentId = input.document?.id ?? "";
  const documentPath = input.document?.path ?? "";
  const awareness = useMemo(
    () =>
      documentId
        ? {
            actorName: input.actorName,
            actorType: "human",
            activity: `Editing ${documentPath}`,
            filePath: documentPath,
          }
        : null,
    [documentId, documentPath, input.actorName],
  );
  const { ydoc, ready, connected } = useYDocSync({
    workspaceId: input.workspaceId,
    token: input.token,
    documentId,
    awareness,
  });
  const ytext = useMemo(() => ydoc.getText("content"), [ydoc]);

  return { ydoc, ytext, ready, connected };
}
