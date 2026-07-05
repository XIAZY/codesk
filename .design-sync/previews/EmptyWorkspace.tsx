import { EmptyWorkspace } from "codesk-frontend";

export const Default = () => (
  <EmptyWorkspace
    onCreateDocument={() => {}}
    onCreateDaemon={() => {}}
    creatingDocument={false}
    canCreateDocument
  />
);

export const CreatingDocument = () => (
  <EmptyWorkspace
    onCreateDocument={() => {}}
    onCreateDaemon={() => {}}
    creatingDocument
    canCreateDocument
  />
);

export const DocCreationDisabled = () => (
  <EmptyWorkspace
    onCreateDocument={() => {}}
    onCreateDaemon={() => {}}
    creatingDocument={false}
    canCreateDocument={false}
  />
);
