import { DocumentTree } from "codesk-frontend";

const doc = (id: string, path: string, title: string) => ({ id, path, title });

const nodes = [
  {
    kind: "folder",
    name: "notes",
    path: "notes",
    fileCount: 2,
    children: [
      { kind: "file", name: "roadmap.md", path: "notes/roadmap.md", document: doc("d1", "notes/roadmap.md", "Roadmap") },
      { kind: "file", name: "meeting-notes.md", path: "notes/meeting-notes.md", document: doc("d2", "notes/meeting-notes.md", "Meeting notes") },
    ],
  },
  { kind: "file", name: "README.md", path: "README.md", document: doc("d3", "README.md", "README") },
] as any;

// Mirrors the app's `.sb` sidebar surface (styles.css) with inline styles:
// the `.sb` class itself is display:none under the 900px capture viewport
// media query, so we replicate its look instead of using the class.
const Sidebar = ({ children }: any) => (
  <div
    style={{
      width: 248,
      display: "flex",
      flexDirection: "column",
      background: "var(--paper-2)",
      border: "1px solid var(--border)",
      borderRadius: 10,
      padding: "8px 0 12px",
    }}
  >
    <section className="sb-section doc-tree">
      <div className="lab">
        <span className="label">Documents</span>
      </div>
      <div className="doc-tree-body">{children}</div>
    </section>
  </div>
);

const noop = () => {};
const base = {
  renamingDocumentId: "",
  freshDocumentId: "",
  renamingDraft: "",
  onToggleFolder: noop,
  onSelectDocument: noop,
} as any;

export const ExpandedWithActive = () => (
  <Sidebar>
    <DocumentTree
      {...base}
      nodes={nodes}
      activeDocumentId="d1"
      collapsedFolders={new Set<string>()}
      threadCountFor={(id: string) => (id === "d2" ? 3 : 0)}
    />
  </Sidebar>
);

export const CollapsedFolder = () => (
  <Sidebar>
    <DocumentTree
      {...base}
      nodes={nodes}
      activeDocumentId="d3"
      collapsedFolders={new Set(["notes"])}
      threadCountFor={() => 0}
    />
  </Sidebar>
);

export const FreshDocument = () => (
  <Sidebar>
    <DocumentTree
      {...base}
      nodes={
        [
          ...nodes,
          { kind: "file", name: "untitled.md", path: "untitled.md", document: doc("d4", "untitled.md", "untitled") },
        ] as any
      }
      activeDocumentId="d4"
      freshDocumentId="d4"
      collapsedFolders={new Set<string>()}
      threadCountFor={(id: string) => (id === "d1" ? 5 : 0)}
    />
  </Sidebar>
);
