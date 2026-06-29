import type { WorkspaceSummary } from "./types";

export const workspaceSlugStorageKey = "notty.workspace.slug";

export type WorkspaceView =
  | { kind: "home" }
  | { kind: "document"; documentId: string }
  | { kind: "daemons" }
  | { kind: "agents" };

export type AppRoute =
  | { kind: "root" }
  | { kind: "login" }
  | { kind: "register" }
  | { kind: "newWorkspace" }
  | { kind: "workspace"; slug: string; view: WorkspaceView }
  | { kind: "notFound" };

export function workspaceLastDocumentStorageKey(slug: string) {
  return `notty.workspace.${slug}.lastDoc`;
}

export function parseRoute(pathname: string): AppRoute {
  const path = normalizePathname(pathname);
  if (path === "/") {
    return { kind: "root" };
  }
  if (path === "/login") {
    return { kind: "login" };
  }
  if (path === "/register") {
    return { kind: "register" };
  }
  if (path === "/new") {
    return { kind: "newWorkspace" };
  }

  const segments = path.split("/").filter(Boolean);
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
  if (segments.length === 3 && segments[2] === "daemons") {
    return { kind: "workspace", slug, view: { kind: "daemons" } };
  }
  if (segments.length === 3 && segments[2] === "agents") {
    return { kind: "workspace", slug, view: { kind: "agents" } };
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
    case "newWorkspace":
      return "/new";
    case "workspace": {
      const slug = encodeURIComponent(route.slug);
      switch (route.view.kind) {
        case "home":
          return `/w/${slug}`;
        case "document":
          return `/w/${slug}/d/${encodeURIComponent(route.view.documentId)}`;
        case "daemons":
          return `/w/${slug}/daemons`;
        case "agents":
          return `/w/${slug}/agents`;
      }
      break;
    }
    case "notFound":
      return "/404";
  }
  return "/404";
}

export function resolveRoot(
  auth: { authenticated: boolean; workspaces: Pick<WorkspaceSummary, "slug">[] },
  savedSlug: string | null,
): AppRoute {
  if (!auth.authenticated) {
    return { kind: "login" };
  }
  if (auth.workspaces.length === 0) {
    return { kind: "newWorkspace" };
  }
  const savedWorkspace = savedSlug ? auth.workspaces.find((workspace) => workspace.slug === savedSlug) : null;
  return { kind: "workspace", slug: (savedWorkspace ?? auth.workspaces[0]).slug, view: { kind: "home" } };
}

export function resolveWorkspace(slug: string, savedDocId: string | null): AppRoute {
  const documentId = savedDocId?.trim();
  if (documentId) {
    return { kind: "workspace", slug, view: { kind: "document", documentId } };
  }
  return { kind: "workspace", slug, view: { kind: "home" } };
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
