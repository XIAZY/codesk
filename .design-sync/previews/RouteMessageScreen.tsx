import { RouteMessageScreen } from "codesk-frontend";

export const PageNotFound = () => (
  <RouteMessageScreen title="Page not found" body="That link does not match a Codesk route." />
);

export const InviteUnavailable = () => (
  <RouteMessageScreen
    title="Invite unavailable"
    body="This invite link has expired or was revoked. Ask a workspace member to send you a fresh one."
  />
);
