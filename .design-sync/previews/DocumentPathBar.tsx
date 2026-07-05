import { DocumentPathBar } from "codesk-frontend";

const toolbar = (children: any) => (
  <div className="doc-toolbar" style={{ width: 560 }}>
    <div className="breadcrumb document-breadcrumb">{children}</div>
  </div>
);

export const NestedPath = () =>
  toolbar(
    <DocumentPathBar
      document={{ id: "d1", path: "notes/roadmap.md", title: "Roadmap" } as any}
      workspaceName="Acme Docs"
      editing={false}
      draft=""
    />
  );

export const RootFile = () =>
  toolbar(
    <DocumentPathBar
      document={{ id: "d2", path: "README.md", title: "README" } as any}
      workspaceName="Acme Docs"
      editing={false}
      draft=""
    />
  );

export const DeeplyNested = () =>
  toolbar(
    <DocumentPathBar
      document={{ id: "d3", path: "notes/2026/q3/planning-review.md", title: "Planning Review" } as any}
      workspaceName="Acme Docs"
      editing={false}
      draft=""
    />
  );
