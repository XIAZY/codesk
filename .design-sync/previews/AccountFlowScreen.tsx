import { AccountFlowScreen } from "codesk-frontend";

export const SignedOut = () => (
  <AccountFlowScreen
    title="You're signed out"
    body="Sign back in to pick up where you left off in your Codesk workspaces."
    actionLabel="Sign in"
    onAction={() => {}}
  />
);

export const SessionExpired = () => (
  <AccountFlowScreen
    title="Session expired"
    body="Your session timed out while you were away. Sign in again to keep editing."
    actionLabel="Sign in again"
    onAction={() => {}}
  />
);
