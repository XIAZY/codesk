import type { Account, DocumentItem, WorkspaceSummary } from "./types";

export type WorkspaceView =
  | { kind: "home" }
  | { kind: "document"; documentId: string };

export type AppRoute =
  | { kind: "root" }
  | { kind: "login" }
  | { kind: "register" }
  | { kind: "verifyEmail"; token: string }
  | { kind: "forgotPassword" }
  | { kind: "resetPassword"; token: string }
  | { kind: "invite"; token: string }
  | { kind: "newWorkspace" }
  | { kind: "workspace"; slug: string; view: WorkspaceView }
  | { kind: "notFound" };

export function parseRoute(pathname: string): AppRoute {
  const [rawPath, rawQuery = ""] = pathname.split("?", 2);
  const path = normalizePathname(rawPath);
  const query = new URLSearchParams(rawQuery);
  if (path === "/") {
    return { kind: "root" };
  }
  if (path === "/login") {
    return { kind: "login" };
  }
  if (path === "/register") {
    return { kind: "register" };
  }
  if (path === "/account/verify-email") {
    return { kind: "verifyEmail", token: query.get("token") ?? "" };
  }
  if (path === "/account/forgot-password") {
    return { kind: "forgotPassword" };
  }
  if (path === "/account/reset-password") {
    return { kind: "resetPassword", token: query.get("token") ?? "" };
  }
  if (path === "/new") {
    return { kind: "newWorkspace" };
  }

  const segments = path.split("/").filter(Boolean);
  if (segments[0] === "invite" && segments.length === 2) {
    const token = decodePathSegment(segments[1]);
    return token ? { kind: "invite", token } : { kind: "notFound" };
  }
  if (segments[0] !== "w" || segments.length < 2) {
    return { kind: "notFound" };
  }

  const slug = decodePathSegment(segments[1]);
  if (!slug) {
    return { kind: "notFound" };
  }
  if (segments.length === 2) {
    return { kind: "workspace", slug, view: { kind: "home" } };
  }
  if (segments.length === 4 && segments[2] === "d") {
    const documentId = decodePathSegment(segments[3]);
    return documentId ? { kind: "workspace", slug, view: { kind: "document", documentId } } : { kind: "notFound" };
  }
  return { kind: "notFound" };
}

export function routePath(route: AppRoute): string {
  switch (route.kind) {
    case "root":
      return "/";
    case "login":
      return "/login";
    case "register":
      return "/register";
    case "verifyEmail":
      return `/account/verify-email?token=${encodeURIComponent(route.token)}`;
    case "forgotPassword":
      return "/account/forgot-password";
    case "resetPassword":
      return `/account/reset-password?token=${encodeURIComponent(route.token)}`;
    case "invite":
      return `/invite/${encodeURIComponent(route.token)}`;
    case "newWorkspace":
      return "/new";
    case "workspace": {
      const slug = encodeURIComponent(route.slug);
      switch (route.view.kind) {
        case "home":
          return `/w/${slug}`;
        case "document":
          return `/w/${slug}/d/${encodeURIComponent(route.view.documentId)}`;
      }
      break;
    }
    case "notFound":
      return "/404";
  }
  return "/404";
}

export function resolveRoot(
  auth: { authenticated: boolean; account?: Pick<Account, "lastAccessedWorkspaceId"> | null; workspaces: Pick<WorkspaceSummary, "id" | "slug">[] },
): AppRoute {
  if (!auth.authenticated) {
    return { kind: "login" };
  }
  if (auth.workspaces.length === 0) {
    return { kind: "newWorkspace" };
  }
  const lastWorkspaceId = auth.account?.lastAccessedWorkspaceId?.trim() ?? "";
  const lastWorkspace = lastWorkspaceId ? auth.workspaces.find((workspace) => workspace.id === lastWorkspaceId) : null;
  return { kind: "workspace", slug: (lastWorkspace ?? auth.workspaces[0]).slug, view: { kind: "home" } };
}

export function resolveWorkspace(
  workspace: Pick<WorkspaceSummary, "slug" | "lastAccessedDocumentId">,
  documents: Pick<DocumentItem, "id">[],
): AppRoute {
  const lastDocumentId = workspace.lastAccessedDocumentId?.trim() ?? "";
  const document = (lastDocumentId ? documents.find((item) => item.id === lastDocumentId) : null) ?? documents[0] ?? null;
  if (document) {
    return { kind: "workspace", slug: workspace.slug, view: { kind: "document", documentId: document.id } };
  }
  return { kind: "workspace", slug: workspace.slug, view: { kind: "home" } };
}

function normalizePathname(pathname: string) {
  const path = pathname.split(/[?#]/, 1)[0] || "/";
  if (path === "/") {
    return path;
  }
  return path.replace(/\/+$/, "") || "/";
}

function decodePathSegment(value: string) {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}
