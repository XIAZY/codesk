import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, ReactNode, RefObject } from "react";
import { ApiClient, ApiError, apiBase, daemonStaticBase, publicOrigin, type UpdateWorkspaceSettingsInput } from "./api";
import { DocumentSurface, type LiveThread, type SurfaceSelection } from "./DocumentSurface";
import type { MarkdownPreviewCommandName } from "./markdownLivePreview";
import {
  agentDisplayStatus,
  agentsByDaemon,
  clampRailWidth,
  buildDaemonInstallCommand,
  buildDaemonReinstallCommand,
  buildDaemonUninstallCommand,
  workspacePeople,
  documentParticipants,
  personOnline,
  documentActivity,
  activityCategory,
  relativeTime,
  daemonStatus,
  daemonLiveStatus,
  handleMaxLength,
  handleMinLength,
  identifierFromName,
  identifierHelpText,
  identifierPattern,
  isMarkdownDocumentPath,
  randomWorkspaceName,
  threadReplyLabel,
  workspaceInboxSummary,
  workspaceSlugMaxLength,
  workspaceSlugMinLength,
  type ActivityCategory,
  type WorkspaceInboxItem,
  type WorkspaceInboxSummary,
  type WorkspacePerson,
  type LineThreadGroup,
  resolveThreadAnchorLive,
} from "./logic";
import { resolveRoot, resolveWorkspace, type WorkspaceView } from "./routes";
import { navigate, useRoute } from "./useRoute";
import { useRootNamespace } from "./useRootNamespace";
import { useDocumentSync } from "./useDocument";
import { useWorkspace } from "./useWorkspace";
import type { Account, ActivityEvent, Agent, AgentEvent, Daemon, DocumentItem, ThreadItem, UserItem, WorkspaceInvitePreview, WorkspaceState, WorkspaceSummary } from "./types";
import { resolveRuntimeTiles, selectableRuntimeKinds, type RuntimeTile } from "./runtimes";
import "./styles.css";

const tokenStorageKey = "codesk.auth.token";
const rightTabLabels = { threads: "Threads", activity: "Document Activity", coworkers: "Participants" } as const;
const portableFileNameIllegalChars = /[\u0000-\u001F<>:"\/\\|?*]/g;
const windowsReservedBaseName = /^(con|prn|aux|nul|com[1-9]|lpt[1-9])$/i;

function clearLegacyClientStorage() {
  for (let index = localStorage.length - 1; index >= 0; index -= 1) {
    const key = localStorage.key(index);
    if (!key) {
      continue;
    }
    if (
      key === "notty.auth.token" ||
      key === "notty.workspace.id" ||
      key === "notty.document.id" ||
      key === "notty.activeDocumentId" ||
      key.startsWith("notty.workspace.")
    ) {
      localStorage.removeItem(key);
    }
  }
}

function initials(value?: string) {
  const source = (value || "N").trim();
  const words = source.split(/[\s@._/-]+/).filter(Boolean);
  const letters = words.length > 1 ? `${words[0][0]}${words[1][0]}` : source.slice(0, 2);
  return letters.toUpperCase();
}

function fileName(path?: string) {
  if (!path) {
    return "Untitled";
  }
  return path.split("/").filter(Boolean).pop() || path;
}

function parentPath(path?: string) {
  const parts = (path || "").split("/").filter(Boolean);
  if (parts.length <= 1) {
    return "";
  }
  return parts.slice(0, -1).join("/");
}

function documentExtension(path?: string) {
  const name = fileName(path);
  const dotIndex = name.lastIndexOf(".");
  if (dotIndex > 0 && dotIndex < name.length - 1) {
    return name.slice(dotIndex);
  }
  return ".md";
}

function documentStem(path?: string) {
  const name = fileName(path);
  const extension = documentExtension(path);
  return name.endsWith(extension) ? name.slice(0, -extension.length) || "Untitled" : name || "Untitled";
}

function editableFileStem(name: string) {
  const extension = documentExtension(name);
  return name.endsWith(extension) ? name.slice(0, -extension.length) : name;
}

function splitVisibleFileName(name: string) {
  const dotIndex = name.lastIndexOf(".");
  if (dotIndex > 0) {
    return { stem: name.slice(0, dotIndex), extension: name.slice(dotIndex) };
  }
  return { stem: name, extension: "" };
}

function filterDocumentFileNameInput(value: string) {
  return value.replace(portableFileNameIllegalChars, "").slice(0, 160);
}

function normalizeDocumentFileName(value: string, fallbackExtension: string) {
  const fallback = `Untitled${fallbackExtension}`;
  const trimmed = filterDocumentFileNameInput(value).trim().replace(/[. ]+$/g, "");
  if (!trimmed) {
    return fallback;
  }
  const { stem, extension } = splitVisibleFileName(trimmed);
  if (windowsReservedBaseName.test(stem || trimmed)) {
    return `${stem || trimmed}-file${extension}`;
  }
  return trimmed;
}

function joinDocumentPath(folder: string, name: string) {
  return folder ? `${folder}/${name}` : name;
}

function uniqueDocumentPath(documents: DocumentItem[], path: string, currentDocumentId = "") {
  const occupied = new Set(
    documents
      .filter((document) => document.id !== currentDocumentId)
      .map((document) => document.path.toLowerCase()),
  );
  if (!occupied.has(path.toLowerCase())) {
    return path;
  }
  const folder = parentPath(path);
  const extension = documentExtension(path);
  const stem = documentStem(path);
  for (let suffix = 2; suffix < 10_000; suffix += 1) {
    const candidate = joinDocumentPath(folder, `${stem}-${suffix}${extension}`);
    if (!occupied.has(candidate.toLowerCase())) {
      return candidate;
    }
  }
  return joinDocumentPath(folder, `${stem}-${Date.now()}${extension}`);
}

function untitledDocumentPath(documents: DocumentItem[], activePath?: string) {
  const folder = parentPath(activePath);
  return uniqueDocumentPath(documents, joinDocumentPath(folder, "Untitled.md"));
}

function documentPathFromFileName(document: DocumentItem, documents: DocumentItem[], draftFileName: string) {
  const name = normalizeDocumentFileName(draftFileName, documentExtension(document.path));
  const nextPath = joinDocumentPath(parentPath(document.path), name);
  return uniqueDocumentPath(documents, nextPath, document.id);
}

function displayFileName(document: DocumentItem, renamingDocumentId: string, titleDraft: string) {
  if (document.id === renamingDocumentId) {
    return filterDocumentFileNameInput(titleDraft) || `Untitled${documentExtension(document.path)}`;
  }
  return fileName(document.path);
}

function folderBreadcrumb(path?: string) {
  const parts = (path || "").split("/").filter(Boolean);
  if (parts.length <= 1) {
    return "Workspace";
  }
  return parts.slice(0, -1).join(" / ");
}

type DocTreeFile = { kind: "file"; name: string; path: string; document: DocumentItem };
type DocTreeFolder = { kind: "folder"; name: string; path: string; children: DocTreeNode[]; fileCount: number };
type DocTreeNode = DocTreeFile | DocTreeFolder;

function sortDocNodes(nodes: DocTreeNode[]): DocTreeNode[] {
  for (const node of nodes) {
    if (node.kind === "folder") {
      sortDocNodes(node.children);
      node.fileCount = node.children.reduce(
        (sum, child) => sum + (child.kind === "file" ? 1 : child.fileCount),
        0,
      );
    }
  }
  return nodes.sort((left, right) => {
    if (left.kind !== right.kind) {
      return left.kind === "folder" ? -1 : 1;
    }
    return left.name.localeCompare(right.name);
  });
}

// Builds a real nested tree from document paths. Root-level files sit at the top
// level (no synthetic folder), and intermediate path segments become folders so
// that e.g. docs/api/auth.md nests under docs › api.
function buildDocumentTree(documents: DocumentItem[]): DocTreeNode[] {
  const root: DocTreeFolder = { kind: "folder", name: "", path: "", children: [], fileCount: 0 };
  const folderIndex = new Map<string, DocTreeFolder>([["", root]]);

  const ensureFolder = (segments: string[]) => {
    let current = root;
    let prefix = "";
    for (const segment of segments) {
      prefix = prefix ? `${prefix}/${segment}` : segment;
      let next = folderIndex.get(prefix);
      if (!next) {
        next = { kind: "folder", name: segment, path: prefix, children: [], fileCount: 0 };
        folderIndex.set(prefix, next);
        current.children.push(next);
      }
      current = next;
    }
    return current;
  };

  for (const document of documents) {
    const segments = (document.path || "").split("/").filter(Boolean);
    const name = segments.length ? segments[segments.length - 1] : document.title || "Untitled";
    const parent = ensureFolder(segments.slice(0, -1));
    parent.children.push({ kind: "file", name, path: document.path, document });
  }

  return sortDocNodes(root.children);
}

function folderAncestors(path?: string) {
  const parts = (path || "").split("/").filter(Boolean);
  const ancestors: string[] = [];
  let prefix = "";
  for (const segment of parts.slice(0, -1)) {
    prefix = prefix ? `${prefix}/${segment}` : segment;
    ancestors.push(prefix);
  }
  return ancestors;
}

function shortTime(value?: string) {
  if (!value) {
    return "now";
  }
  const date = new Date(value);
  if (Number.isNaN(date.valueOf()) || date.getUTCFullYear() < 2020) {
    return "now";
  }
  const seconds = Math.max(0, Math.round((Date.now() - date.valueOf()) / 1000));
  if (seconds < 60) {
    return `${seconds}s`;
  }
  if (seconds < 3600) {
    return `${Math.round(seconds / 60)}m`;
  }
  if (seconds < 86400) {
    return `${Math.round(seconds / 3600)}h`;
  }
  return `${Math.round(seconds / 86400)}d`;
}

function visibleAgentStatus(
  agent: Agent,
  runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"],
  daemons: Daemon[],
  events: AgentEvent[],
  nowMs: number,
) {
  return agentDisplayStatus(agent, runs, daemons, events, nowMs);
}

function detailStatusLabel(status: ReturnType<typeof visibleAgentStatus>) {
  return status.detailLabel ?? status.label;
}

function clamp(value: number, min: number, max: number) {
  return Math.min(Math.max(value, min), max);
}

function inboxBadgeText(count: number) {
  return count > 9 ? "9+" : String(count);
}

function inboxBadgeLabel(summary: WorkspaceInboxSummary) {
  const { total, failed, needsReview } = summary.counts;
  if (!total) {
    return "Inbox";
  }
  const parts = [
    failed ? `${failed} failed` : "",
    needsReview ? `${needsReview} needs review` : "",
  ].filter(Boolean);
  return `Inbox, ${total} need attention: ${parts.join(", ")}`;
}

function inboxDocumentLabel(documents: DocumentItem[], documentId?: string) {
  if (!documentId) {
    return "";
  }
  const document = documents.find((item) => item.id === documentId);
  return document ? fileName(document.path) : "";
}

function focusableElements(root: HTMLElement) {
  return Array.from(
    root.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((element) => !element.hasAttribute("disabled") && element.getAttribute("aria-hidden") !== "true");
}

function upsertWorkspaceSummary(workspaces: WorkspaceSummary[], workspace: WorkspaceSummary) {
  const index = workspaces.findIndex((item) => item.id === workspace.id);
  if (index === -1) {
    return [...workspaces, workspace];
  }
  const next = workspaces.slice();
  next[index] = { ...next[index], ...workspace };
  return next;
}

export function App() {
  const route = useRoute();
  const [token, setToken] = useState(() => localStorage.getItem(tokenStorageKey) ?? "");
  const [account, setAccount] = useState<Account | null>(null);
  const [workspaces, setWorkspaces] = useState<WorkspaceSummary[]>([]);
  const [restoringSession, setRestoringSession] = useState(false);
  const api = useMemo(() => new ApiClient(token), [token]);

  const rememberWorkspaceAccess = useCallback((workspaceId: string, documentId = "") => {
    setAccount((current) => {
      if (!current || current.lastAccessedWorkspaceId === workspaceId) {
        return current;
      }
      return { ...current, lastAccessedWorkspaceId: workspaceId };
    });
    if (!documentId) {
      return;
    }
    setWorkspaces((current) => {
      let changed = false;
      const next = current.map((workspace) => {
        if (workspace.id !== workspaceId || workspace.lastAccessedDocumentId === documentId) {
          return workspace;
        }
        changed = true;
        return { ...workspace, lastAccessedDocumentId: documentId };
      });
      return changed ? next : current;
    });
  }, []);

  const saveAuth = (nextToken: string, nextAccount: Account, nextWorkspaces: WorkspaceSummary[] = []) => {
    const safeWorkspaces = Array.isArray(nextWorkspaces) ? nextWorkspaces : [];
    clearLegacyClientStorage();
    localStorage.setItem(tokenStorageKey, nextToken);
    setToken(nextToken);
    setAccount(nextAccount);
    setWorkspaces(safeWorkspaces);
    if (route.kind === "root" || route.kind === "login" || route.kind === "register") {
      navigate(resolveRoot({ authenticated: true, account: nextAccount, workspaces: safeWorkspaces }), {
        replace: true,
      });
    }
  };

  const signOut = () => {
    localStorage.removeItem(tokenStorageKey);
    clearLegacyClientStorage();
    setToken("");
    setAccount(null);
    setWorkspaces([]);
    navigate({ kind: "login" }, { replace: true });
  };

  const acceptInviteWorkspace = useCallback(
    async (workspace: WorkspaceSummary) => {
      try {
        const response = await api.me();
        const safeWorkspaces = response.workspaces ?? [];
        setAccount(response.account);
        setWorkspaces(safeWorkspaces);
      } catch {
        setAccount((current) => current ? { ...current, lastAccessedWorkspaceId: workspace.id } : current);
        setWorkspaces((current) => {
          const exists = current.some((item) => item.id === workspace.id);
          return exists ? current.map((item) => (item.id === workspace.id ? { ...item, ...workspace } : item)) : [...current, workspace];
        });
      }
      navigate({ kind: "workspace", slug: workspace.slug, view: { kind: "home" } }, { replace: true });
    },
    [api],
  );

  useEffect(() => {
    if (!token) {
      return;
    }
    let disposed = false;
    setRestoringSession(true);
    new ApiClient(token)
      .me()
      .then((response) => {
        if (disposed) {
          return;
        }
        const safeWorkspaces = response.workspaces ?? [];
        clearLegacyClientStorage();
        setAccount(response.account);
        setWorkspaces(safeWorkspaces);
      })
      .catch(() => {
        if (!disposed) {
          signOut();
        }
      })
      .finally(() => {
        if (!disposed) {
          setRestoringSession(false);
        }
      });
    return () => {
      disposed = true;
    };
  }, [token]);

  useEffect(() => {
    clearLegacyClientStorage();
  }, []);

  useEffect(() => {
    if (!token && (route.kind === "workspace" || route.kind === "newWorkspace")) {
      navigate({ kind: "login" }, { replace: true });
      return;
    }
  }, [route, token]);

  useEffect(() => {
    if (route.kind !== "root") {
      return;
    }
    if (token && !account) {
      return;
    }
    navigate(
      resolveRoot({ authenticated: Boolean(token), account, workspaces }),
      { replace: true },
    );
  }, [account, route, token, workspaces]);

  useEffect(() => {
    if (route.kind !== "login" && route.kind !== "register") {
      return;
    }
    if (!token || !account) {
      return;
    }
    navigate(
      resolveRoot({ authenticated: true, account, workspaces }),
      { replace: true },
    );
  }, [account, route, token, workspaces]);

  const routeWorkspace = route.kind === "workspace" ? workspaces.find((workspace) => workspace.slug === route.slug) ?? null : null;

  if (route.kind === "verifyEmail") {
    return <VerifyEmailPage api={api} token={route.token} />;
  }
  if (route.kind === "forgotPassword") {
    return <ForgotPasswordPage api={api} />;
  }
  if (route.kind === "resetPassword") {
    return <ResetPasswordPage api={api} token={route.token} />;
  }

  if (!token) {
    if (route.kind === "invite") {
      return <InvitePage api={api} inviteToken={route.token} account={null} workspaces={[]} onAuth={saveAuth} onAccepted={acceptInviteWorkspace} />;
    }
    if (route.kind === "notFound") {
      return <RouteMessageScreen title="Page not found" body="That link does not match a Codesk route." />;
    }
    return <AuthScreen api={api} mode={route.kind === "register" ? "register" : "login"} onAuth={saveAuth} />;
  }

  if (!account) {
    return <RestoringSessionScreen />;
  }

  if (route.kind === "root" || route.kind === "login" || route.kind === "register") {
    return <RestoringSessionScreen />;
  }

  if (route.kind === "invite") {
    return <InvitePage api={api} inviteToken={route.token} account={account} workspaces={workspaces} onAuth={saveAuth} onAccepted={acceptInviteWorkspace} />;
  }

  if (route.kind === "notFound") {
    return <RouteMessageScreen title="Page not found" body="That link does not match a Codesk route." />;
  }

  if (route.kind === "newWorkspace") {
    return (
      <WorkspaceOnboarding
        api={api}
        account={account}
        workspaces={workspaces}
        onWorkspaces={setWorkspaces}
        onSelect={(workspace) => {
          navigate({ kind: "workspace", slug: workspace.slug, view: { kind: "home" } }, { replace: true });
        }}
        onSignOut={signOut}
      />
    );
  }

  if (!routeWorkspace) {
    return (
      <RouteMessageScreen
        title="Workspace not found"
        body="This workspace is unavailable for the signed-in account."
      />
    );
  }

  const handleWorkspaceUpdated = (workspace: WorkspaceSummary) => {
    setWorkspaces((current) => upsertWorkspaceSummary(current, workspace));
  };

  const handleWorkspaceDeleted = (workspaceId: string) => {
    setWorkspaces((current) => current.filter((workspace) => workspace.id !== workspaceId));
    setAccount((current) =>
      current?.lastAccessedWorkspaceId === workspaceId ? { ...current, lastAccessedWorkspaceId: "" } : current
    );
  };

  return (
    <WorkspaceApp
      api={api}
      token={token}
      workspaceId={routeWorkspace.id}
      workspaceSlug={routeWorkspace.slug}
      view={route.view}
      account={account}
      workspaces={workspaces}
      onAccess={rememberWorkspaceAccess}
      onWorkspaceUpdated={handleWorkspaceUpdated}
      onWorkspaceDeleted={handleWorkspaceDeleted}
      onWorkspaceChange={(slug) => {
        navigate({ kind: "workspace", slug, view: { kind: "home" } });
      }}
      onSignOut={signOut}
    />
  );
}

export function RestoringSessionScreen() {
  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">Restoring your session</h1>
        <p className="small muted">Loading account and workspace membership.</p>
      </section>
    </main>
  );
}

export function RouteMessageScreen({ title, body }: { title: string; body: string }) {
  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">{title}</h1>
        <p className="small muted auth-copy">{body}</p>
        <button className="btn accent full lg" type="button" onClick={() => navigate({ kind: "root" })}>
          Go home
        </button>
      </section>
    </main>
  );
}

function InvitePage({
  api,
  inviteToken,
  account,
  workspaces,
  onAuth,
  onAccepted,
}: {
  api: ApiClient;
  inviteToken: string;
  account: Account | null;
  workspaces: WorkspaceSummary[];
  onAuth: (token: string, account: Account, workspaces: WorkspaceSummary[]) => void;
  onAccepted: (workspace: WorkspaceSummary) => Promise<void>;
}) {
  const [preview, setPreview] = useState<WorkspaceInvitePreview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [handle, setHandle] = useState(() => defaultWorkspaceHandle(account));
  const [joining, setJoining] = useState(false);
  const [joinError, setJoinError] = useState("");

  useEffect(() => {
    setHandle(defaultWorkspaceHandle(account));
  }, [account]);

  useEffect(() => {
    let disposed = false;
    setLoading(true);
    setError("");
    setPreview(null);
    api.previewWorkspaceInvite(inviteToken)
      .then((response) => {
        if (!disposed) {
          setPreview(response);
        }
      })
      .catch((err) => {
        if (!disposed) {
          setError(err instanceof Error ? err.message : String(err));
        }
      })
      .finally(() => {
        if (!disposed) {
          setLoading(false);
        }
      });
    return () => {
      disposed = true;
    };
  }, [api, inviteToken]);

  if (loading) {
    return <RouteMessageScreen title="Opening invite" body="Checking this workspace invite." />;
  }
  if (error || !preview?.workspace) {
    return <RouteMessageScreen title="Invite unavailable" body={error || "This invite link is no longer available."} />;
  }

  const existingWorkspace = workspaces.find((workspace) => workspace.slug === preview.workspace.slug) ?? null;
  const previewCard = <InvitePreviewCard preview={preview} />;

  if (!account) {
    return (
      <AuthScreen
        api={api}
        mode="login"
        onAuth={onAuth}
        title="Join workspace"
        copy="Log in or create an account to accept this invite."
        preserveRoute
      >
        {previewCard}
      </AuthScreen>
    );
  }

  if (existingWorkspace) {
    return (
      <main className="auth-screen">
        <section className="card p-24 auth-panel">
          <Logo />
          {previewCard}
          <h1 className="auth-title">You are already in this workspace</h1>
          <button className="btn accent full lg" type="button" onClick={() => navigate({ kind: "workspace", slug: existingWorkspace.slug, view: { kind: "home" } }, { replace: true })}>
            Open workspace
          </button>
        </section>
      </main>
    );
  }

  const accept = async (event: FormEvent) => {
    event.preventDefault();
    setJoining(true);
    setJoinError("");
    try {
      const response = await api.acceptWorkspaceInvite(inviteToken, { handle });
      await onAccepted(response.workspace);
    } catch (err) {
      setJoinError(err instanceof Error ? err.message : String(err));
    } finally {
      setJoining(false);
    }
  };

  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        {previewCard}
        <h1 className="auth-title">Join workspace</h1>
        <form onSubmit={accept} className="form-stack">
          <label className="field">
            <span className="lab">Your handle in this workspace</span>
            <input
              aria-label="Your handle in this workspace"
              value={handle}
              onChange={(event) => setHandle(event.target.value)}
              pattern={identifierPattern}
              minLength={handleMinLength}
              maxLength={handleMaxLength}
              title={identifierHelpText}
              required
            />
            <span className="hint">{identifierHelpText}</span>
          </label>
          {joinError ? <p className="error-text">{joinError}</p> : null}
          <button className="btn accent full lg" disabled={joining}>{joining ? "Joining..." : "Join workspace"}</button>
        </form>
      </section>
    </main>
  );
}

export function InvitePreviewCard({ preview }: { preview: WorkspaceInvitePreview }) {
  return (
    <div className="invite-preview">
      <div className="avi workspace-avi">{initials(preview.workspace.name)}</div>
      <div className="col gap-2 min-0">
        <b className="truncate">{preview.workspace.name}</b>
        <span className="tiny muted truncate">/{preview.workspace.slug} · expires {formatInviteDate(preview.expiresAt)}</span>
      </div>
    </div>
  );
}

function defaultWorkspaceHandle(account: Account | null) {
  return identifierFromName(account?.email.split("@")[0] ?? account?.displayName ?? "member", handleMaxLength) || "member";
}

function formatInviteDate(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) {
    return "soon";
  }
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(date);
}

export function AuthScreen({
  api,
  mode,
  onAuth,
  title,
  copy,
  children,
  preserveRoute = false,
}: {
  api: ApiClient;
  mode: "login" | "register";
  onAuth: (token: string, account: Account, workspaces: WorkspaceSummary[]) => void;
  title?: string;
  copy?: string;
  children?: ReactNode;
  preserveRoute?: boolean;
}) {
  const [localMode, setLocalMode] = useState(mode);
  const [displayName, setDisplayName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [verificationEmail, setVerificationEmail] = useState("");
  const [resendNotice, setResendNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [resending, setResending] = useState(false);
  const activeMode = preserveRoute ? localMode : mode;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const response =
        activeMode === "register"
          ? await api.register({ email, password, displayName })
          : await api.login({ email, password });
      if (!response.token) {
        setVerificationEmail(response.account?.email ?? email);
        setResendNotice("");
        return;
      }
      onAuth(response.token, response.account, response.workspaces ?? []);
    } catch (err) {
      if (err instanceof ApiError && err.status === 403 && err.details === "email_not_verified") {
        setVerificationEmail(email);
        setResendNotice("");
        return;
      }
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const resendVerification = async () => {
    setResending(true);
    setError("");
    setResendNotice("");
    try {
      await api.resendVerification(verificationEmail || email);
      setResendNotice("Verification email sent.");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setResending(false);
    }
  };

  if (verificationEmail) {
    return (
      <main className="auth-screen">
        <section className="card p-24 auth-panel">
          <Logo />
          {children}
          <h1 className="auth-title">Check your email</h1>
          <p className="small muted auth-copy">
            We sent a verification link to {verificationEmail}. Verify your email before logging in.
          </p>
          {error ? <p className="error-text">{error}</p> : null}
          {resendNotice ? <p className="small muted">{resendNotice}</p> : null}
          <div className="form-stack">
            <button className="btn accent full lg" onClick={resendVerification} disabled={resending}>
              {resending ? "Sending..." : "Resend verification email"}
            </button>
            <button
              className="btn ghost full"
              onClick={() => {
                setVerificationEmail("");
                setResendNotice("");
                setError("");
                if (!preserveRoute) {
                  navigate({ kind: "login" });
                }
              }}
            >
              Back to login
            </button>
          </div>
        </section>
      </main>
    );
  }

  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        {children}
        <h1 className="auth-title">{title ?? (activeMode === "login" ? "Welcome back" : "Create your account")}</h1>
        <p className="small muted auth-copy">
          {copy ?? (activeMode === "login" ? "Log in to your workspaces." : "You'll set up your workspace next.")}
        </p>
        <form onSubmit={submit} className="form-stack">
          {activeMode === "register" ? (
            <label className="field">
              <span className="lab">Display name</span>
              <input value={displayName} onChange={(event) => setDisplayName(event.target.value)} placeholder="Ada Lovelace" required />
            </label>
          ) : null}
          <label className="field">
            <span className="lab">Email</span>
            <input type="email" value={email} onChange={(event) => setEmail(event.target.value)} placeholder="you@example.com" required />
          </label>
          <label className="field">
            <span className="lab">Password</span>
            <input type="password" value={password} onChange={(event) => setPassword(event.target.value)} placeholder="At least 8 characters" required />
          </label>
          {error ? <p className="error-text">{error}</p> : null}
          <button className="btn accent full lg" disabled={busy}>{busy ? "Working..." : activeMode === "login" ? "Log in" : "Create account"}</button>
        </form>
        <div className="auth-switch">
          {activeMode === "login" ? "No account?" : "Already have an account?"}
          <button
            className="btn-link"
            onClick={() => {
              if (preserveRoute) {
                setLocalMode(activeMode === "login" ? "register" : "login");
                return;
              }
              navigate({ kind: activeMode === "login" ? "register" : "login" });
            }}
          >
            {activeMode === "login" ? "Create one" : "Log in"}
          </button>
        </div>
        {activeMode === "login" ? (
          <div className="auth-switch">
            <button className="btn-link" onClick={() => navigate({ kind: "forgotPassword" })}>
              Forgot password?
            </button>
          </div>
        ) : null}
      </section>
    </main>
  );
}

function VerifyEmailPage({ api, token }: { api: ApiClient; token: string }) {
  const [status, setStatus] = useState<"verifying" | "verified" | "error">(token ? "verifying" : "error");
  const [error, setError] = useState(token ? "" : "This verification link is missing a token.");

  useEffect(() => {
    if (!token) {
      return;
    }
    let disposed = false;
    api.verifyEmail(token)
      .then(() => {
        if (!disposed) {
          setStatus("verified");
        }
      })
      .catch((err) => {
        if (!disposed) {
          if (err instanceof Error && err.message === "email_already_verified") {
            setStatus("verified");
            return;
          }
          setStatus("error");
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => {
      disposed = true;
    };
  }, [api, token]);

  return (
    <AccountFlowScreen
      title={status === "verified" ? "Email verified" : status === "verifying" ? "Verifying email" : "Verification failed"}
      body={status === "verified" ? "Your account is active. You can log in now." : status === "verifying" ? "Checking your verification link." : error}
      actionLabel={status === "verified" ? "Log in" : "Back to login"}
      onAction={() => navigate({ kind: "login" }, { replace: status === "verified" })}
    />
  );
}

function ForgotPasswordPage({ api }: { api: ApiClient }) {
  const [email, setEmail] = useState("");
  const [sent, setSent] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.forgotPassword(email);
      setSent(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (sent) {
    return (
      <AccountFlowScreen
        title="Check your email"
        body="If that account can reset its password, a reset link has been sent."
        actionLabel="Back to login"
        onAction={() => navigate({ kind: "login" })}
      />
    );
  }

  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">Reset your password</h1>
        <p className="small muted auth-copy">Enter your email and we will send a reset link if the account is eligible.</p>
        <form onSubmit={submit} className="form-stack">
          <label className="field">
            <span className="lab">Email</span>
            <input type="email" value={email} onChange={(event) => setEmail(event.target.value)} placeholder="you@example.com" required />
          </label>
          {error ? <p className="error-text">{error}</p> : null}
          <button className="btn accent full lg" disabled={busy}>{busy ? "Sending..." : "Send reset link"}</button>
        </form>
        <div className="auth-switch">
          <button className="btn-link" onClick={() => navigate({ kind: "login" })}>Back to login</button>
        </div>
      </section>
    </main>
  );
}

function ResetPasswordPage({ api, token }: { api: ApiClient; token: string }) {
  const [password, setPassword] = useState("");
  const [done, setDone] = useState(false);
  const [error, setError] = useState(token ? "" : "This reset link is missing a token.");
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!token) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      await api.resetPassword(token, password);
      setDone(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (done) {
    return (
      <AccountFlowScreen
        title="Password reset"
        body="Your password has been updated. Log in with the new password."
        actionLabel="Log in"
        onAction={() => navigate({ kind: "login" }, { replace: true })}
      />
    );
  }

  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">Choose a new password</h1>
        <p className="small muted auth-copy">Use at least 6 characters.</p>
        <form onSubmit={submit} className="form-stack">
          <label className="field">
            <span className="lab">New password</span>
            <input type="password" value={password} onChange={(event) => setPassword(event.target.value)} placeholder="At least 6 characters" required />
          </label>
          {error ? <p className="error-text">{error}</p> : null}
          <button className="btn accent full lg" disabled={busy || !token}>{busy ? "Saving..." : "Reset password"}</button>
        </form>
        <div className="auth-switch">
          <button className="btn-link" onClick={() => navigate({ kind: "login" })}>Back to login</button>
        </div>
      </section>
    </main>
  );
}

export function AccountFlowScreen({ title, body, actionLabel, onAction }: { title: string; body: string; actionLabel: string; onAction: () => void }) {
  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">{title}</h1>
        <p className="small muted auth-copy">{body}</p>
        <button className="btn accent full lg" onClick={onAction}>{actionLabel}</button>
      </section>
    </main>
  );
}

export function WorkspaceOnboarding({
  api,
  account,
  workspaces,
  onWorkspaces,
  onSelect,
  onSignOut,
}: {
  api: Pick<ApiClient, "createWorkspace">;
  account: Account | null;
  workspaces: WorkspaceSummary[];
  onWorkspaces: (workspaces: WorkspaceSummary[]) => void;
  onSelect: (workspace: WorkspaceSummary) => void;
  onSignOut: () => void;
}) {
  const initialHandle = identifierFromName(account?.email.split("@")[0] ?? "owner", handleMaxLength) || "owner";

  return (
    <main className="auth-screen picker-screen">
      <section className="card p-24 picker-panel">
        <div className="row between gap-12">
          <Logo />
          <button className="btn ghost sm" onClick={onSignOut}>Sign out</button>
        </div>
        <div className="picker-head">
          <h1 className="auth-title">Create a workspace</h1>
          <p className="small muted">Workspaces are where documents, local environments, agents, and members live.</p>
        </div>
        <CreateWorkspaceForm
          api={api}
          initialHandle={initialHandle}
          workspaces={workspaces}
          onWorkspaces={onWorkspaces}
          onSelect={onSelect}
        />
        <div className="divider" />
        <JoinInviteLinkForm />
      </section>
    </main>
  );
}

function CreateWorkspaceForm({
  api,
  initialHandle,
  workspaces,
  onWorkspaces,
  onSelect,
}: {
  api: Pick<ApiClient, "createWorkspace">;
  initialHandle: string;
  workspaces: WorkspaceSummary[];
  onWorkspaces: (workspaces: WorkspaceSummary[]) => void;
  onSelect: (workspace: WorkspaceSummary) => void;
}) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);

  const randomizeName = () => {
    const generated = randomWorkspaceName();
    setName(generated);
    if (!slugEdited) {
      setSlug(identifierFromName(generated, workspaceSlugMaxLength));
    }
  };
  const [handle, setHandle] = useState(initialHandle);
  const [error, setError] = useState("");

  const createWorkspace = async (event: FormEvent) => {
    event.preventDefault();
    setError("");
    try {
      const response = await api.createWorkspace({ name, slug, handle });
      const next = [...workspaces, response.workspace];
      onWorkspaces(next);
      onSelect(response.workspace);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <form onSubmit={createWorkspace} className="form-stack create-workspace-card">
      <div>
        <h2 className="modal-title">Create workspace</h2>
        <p className="small muted">You will become the workspace owner.</p>
      </div>
      <label className="field">
        <span className="lab">Workspace name</span>
        <div className="name-row">
          <input
            value={name}
            placeholder="ACME Inc"
            onChange={(event) => {
              const next = event.target.value;
              setName(next);
              if (!slugEdited) {
                setSlug(identifierFromName(next, workspaceSlugMaxLength));
              }
            }}
            required
          />
          <button
            className="btn icon"
            type="button"
            aria-label="Generate a random name"
            title="Generate a random name"
            onClick={randomizeName}
          >
            <Icon name="refresh" />
          </button>
        </div>
      </label>
      <label className="field">
        <span className="lab">Workspace slug</span>
        <input
          aria-label="Workspace slug"
          value={slug}
          placeholder="acme-inc"
          onChange={(event) => {
            setSlug(event.target.value);
            setSlugEdited(true);
          }}
          pattern={identifierPattern}
          minLength={workspaceSlugMinLength}
          maxLength={workspaceSlugMaxLength}
          title={identifierHelpText}
          required
        />
        <span className="hint">{identifierHelpText}</span>
      </label>
      <label className="field">
        <span className="lab">Your handle in this workspace</span>
        <input
          aria-label="Your handle in this workspace"
          value={handle}
          onChange={(event) => setHandle(event.target.value)}
          pattern={identifierPattern}
          minLength={handleMinLength}
          maxLength={handleMaxLength}
          title={identifierHelpText}
          required
        />
        <span className="hint">{identifierHelpText}</span>
      </label>
      {error ? <p className="error-text">{error}</p> : null}
      <button className="btn accent full lg">Create and enter</button>
    </form>
  );
}

function JoinInviteLinkForm() {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");

  const openInvite = (event: FormEvent) => {
    event.preventDefault();
    const token = inviteTokenFromInput(value);
    if (!token) {
      setError("Enter a valid invite link.");
      return;
    }
    navigate({ kind: "invite", token });
  };

  return (
    <form onSubmit={openInvite} className="form-stack invite-link-card">
      <div>
        <h2 className="modal-title">Join with invite link</h2>
      </div>
      <label className="field">
        <span className="lab">Invite link</span>
        <input
          value={value}
          onChange={(event) => {
            setValue(event.target.value);
            setError("");
          }}
          placeholder={`${publicOrigin}/invite/abc123...`}
        />
      </label>
      {error ? <p className="error-text">{error}</p> : null}
      <button className="btn full lg" type="submit">Join with invite link</button>
    </form>
  );
}

function inviteTokenFromInput(value: string) {
  const trimmed = value.trim();
  if (!trimmed) {
    return "";
  }
  const pathToken = tokenFromInvitePath(trimmed);
  if (pathToken) {
    return pathToken;
  }
  try {
    const url = new URL(trimmed);
    return tokenFromInvitePath(url.pathname);
  } catch {
    return /^[^\s/]+$/.test(trimmed) ? trimmed : "";
  }
}

function tokenFromInvitePath(value: string) {
  const parts = value.split(/[?#]/, 1)[0].split("/").filter(Boolean);
  if (parts.length === 2 && parts[0] === "invite") {
    try {
      return decodeURIComponent(parts[1]);
    } catch {
      return "";
    }
  }
  return "";
}

export function WorkspaceApp({
  api,
  token,
  workspaceId,
  workspaceSlug,
  view,
  account,
  workspaces,
  onAccess,
  onWorkspaceUpdated = () => {},
  onWorkspaceDeleted = () => {},
  onWorkspaceChange,
  onSignOut,
}: {
  api: ApiClient;
  token: string;
  workspaceId: string;
  workspaceSlug: string;
  view: WorkspaceView;
  account: Account | null;
  workspaces: WorkspaceSummary[];
  onAccess: (workspaceId: string, documentId?: string) => void;
  onWorkspaceUpdated?: (workspace: WorkspaceSummary) => void;
  onWorkspaceDeleted?: (workspaceId: string) => void;
  onWorkspaceChange: (slug: string) => void;
  onSignOut: () => void;
}) {
  const { workspace, connected, loading, error, reload } = useWorkspace(workspaceId, token);
  const rootNamespace = useRootNamespace({
    workspaceId,
    token,
    rootDocumentId: workspace.rootDocumentId,
  });
  const rootDocuments = rootNamespace.documents;
  const [rightTab, setRightTab] = useState<"threads" | "activity" | "coworkers">("threads");
  const [modal, setModal] = useState<"daemon" | "agent" | "rename" | "agent-detail" | "daemon-detail" | "manage" | null>(null);
  // Default tab is Members & Invite (plan tab order). Integration protocol (Juan's
  // single-flip rule): A2 fills Members before integration, so nothing ships showing
  // a placeholder. If A2 slips out of the batch, flipping this default to "local-env"
  // (the tab with real content) becomes a REQUIRED pre-integration change.
  const [manageTab, setManageTab] = useState<ManageTab>("members");
  // Phase E: collapsible left rail + resizable right rail, both remembered in localStorage.
  const shellRef = useRef<HTMLElement>(null);
  const [sidebarCollapsed, setSidebarCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem("codesk.sb.collapsed") === "1";
    } catch {
      return false;
    }
  });
  const [rightWidth, setRightWidth] = useState<number | null>(() => {
    try {
      const stored = parseInt(localStorage.getItem("codesk.right.width") || "", 10);
      return Number.isFinite(stored) ? stored : null;
    } catch {
      return null;
    }
  });
  const [draggingRail, setDraggingRail] = useState(false);
  const toggleSidebar = useCallback(() => {
    setSidebarCollapsed((collapsed) => {
      const next = !collapsed;
      try {
        localStorage.setItem("codesk.sb.collapsed", next ? "1" : "0");
      } catch {
        // ignore storage failures (private mode) — collapse still works this session.
      }
      return next;
    });
  }, []);
  // Clamp the right rail so a drag can never crush or overlap the document
  // (plan §4.1 "不塌、不错位"). Pure bounds live in logic.ts for direct testing.
  const clampRightWidth = useCallback(
    (width: number) => {
      const shellWidth = shellRef.current?.getBoundingClientRect().width ?? 1120;
      return clampRailWidth(width, shellWidth, sidebarCollapsed);
    },
    [sidebarCollapsed]
  );
  useEffect(() => {
    if (!draggingRail) {
      return;
    }
    const onMove = (event: MouseEvent) => {
      const rect = shellRef.current?.getBoundingClientRect();
      if (!rect) {
        return;
      }
      setRightWidth(clampRightWidth(rect.right - event.clientX));
    };
    const onUp = () => {
      setDraggingRail(false);
      setRightWidth((width) => {
        if (width != null) {
          try {
            localStorage.setItem("codesk.right.width", String(width));
          } catch {
            // ignore storage failures.
          }
        }
        return width;
      });
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
    return () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
  }, [draggingRail, clampRightWidth]);
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [selectedDaemonId, setSelectedDaemonId] = useState<string | null>(null);
  const [selectedThreadId, setSelectedThreadId] = useState("");
  const [focusThreadId, setFocusThreadId] = useState("");
  const [collapsedFolders, setCollapsedFolders] = useState<Set<string>>(() => new Set());
  const [creatingDocument, setCreatingDocument] = useState(false);
  const [createError, setCreateError] = useState("");
  const [renamingDocumentId, setRenamingDocumentId] = useState("");
  const [freshDocumentId, setFreshDocumentId] = useState("");
  const [titleDraft, setTitleDraft] = useState("");
  const [inboxOpen, setInboxOpen] = useState(false);
  const inboxRef = useRef<HTMLDivElement>(null);
  const inboxDialogRef = useRef<HTMLDivElement>(null);
  const inboxTriggerRef = useRef<HTMLButtonElement>(null);
  const lastAccessUpdateKeyRef = useRef("");
  const seenWorkspaceRef = useRef(false);
  const deletionHandledRef = useRef(false);
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);

  const requestedDocumentId = view.kind === "document" ? view.documentId : "";
  const activeDocument = requestedDocumentId ? rootDocuments.find((document) => document.id === requestedDocumentId) ?? null : null;
  const documentMissing = view.kind === "document" && rootNamespace.ready && !activeDocument;
  const documentThreads = useMemo(() => activeDocument ? workspace.threads.filter((thread) => thread.documentId === activeDocument.id) : [], [workspace.threads, activeDocument?.id]);
  const [threadAnchorInfo, setThreadAnchorInfo] = useState<Record<string, { orphaned: boolean; line: number }>>({});
  const groupedAgents = agentsByDaemon(workspace.agents, workspace.daemons);
  const documentTree = useMemo(() => buildDocumentTree(rootDocuments), [rootDocuments]);
  const threadCountByDocument = useMemo(() => {
    const counts = new Map<string, number>();
    for (const thread of workspace.threads) {
      counts.set(thread.documentId, (counts.get(thread.documentId) ?? 0) + 1);
    }
    return counts;
  }, [workspace.threads]);
  const activeFolder = folderBreadcrumb(activeDocument?.path);
  const activeWorkspace = workspaces.find((item) => item.id === workspaceId);
  const currentWorkspaceUser = workspace.users.find((user) => user.id === workspace.currentUserId) ?? null;
  const currentWorkspaceUserHandle = currentWorkspaceUser?.handle ? `@${currentWorkspaceUser.handle}` : "Workspace user";
  const currentWorkspaceUserIdentity = currentWorkspaceUser?.handle || currentWorkspaceUser?.name || "Workspace user";
  const canInviteMembers = workspace.currentMembershipRole === "owner" || workspace.currentMembershipRole === "admin";
  const inboxSummary = useMemo(() => workspaceInboxSummary({ ...workspace, documents: rootNamespace.ready ? rootDocuments : undefined }, { nowMs: now }), [workspace, rootDocuments, rootNamespace.ready, now]);
  const inboxCount = inboxSummary.counts.total;
  const inboxAriaLabel = inboxBadgeLabel(inboxSummary);

  useEffect(() => {
    if (workspace.workspaceId === workspaceId) {
      seenWorkspaceRef.current = true;
      deletionHandledRef.current = false;
    }
  }, [workspace.workspaceId, workspaceId]);

  useEffect(() => {
    if (!seenWorkspaceRef.current || deletionHandledRef.current || workspace.workspaceId || loading) {
      return;
    }
    deletionHandledRef.current = true;
    onWorkspaceDeleted(workspaceId);
    const remaining = workspaces.filter((item) => item.id !== workspaceId);
    navigate(resolveRoot({ authenticated: true, account, workspaces: remaining }), { replace: true });
  }, [account, loading, onWorkspaceDeleted, workspace.workspaceId, workspaceId, workspaces]);

  const closeInbox = useCallback((restoreFocus = true) => {
    setInboxOpen(false);
    if (restoreFocus) {
      window.setTimeout(() => inboxTriggerRef.current?.focus(), 0);
    }
  }, []);

  const openInboxItem = useCallback(
    (item: WorkspaceInboxItem) => {
      if (item.agentId) {
        setSelectedAgentId(item.agentId);
        setModal("agent-detail");
        closeInbox();
      }
    },
    [closeInbox],
  );

  useEffect(() => {
    if (!inboxOpen) {
      return;
    }
    const focusTimer = window.setTimeout(() => {
      const dialog = inboxDialogRef.current;
      if (!dialog) {
        return;
      }
      const first = focusableElements(dialog)[0];
      (first ?? dialog).focus();
    }, 0);

    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && inboxRef.current?.contains(target)) {
        return;
      }
      closeInbox();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeInbox();
        return;
      }
      if (event.key !== "Tab") {
        return;
      }
      const dialog = inboxDialogRef.current;
      if (!dialog) {
        return;
      }
      const focusable = focusableElements(dialog);
      if (!focusable.length) {
        event.preventDefault();
        dialog.focus();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      window.clearTimeout(focusTimer);
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [closeInbox, inboxOpen]);

  useEffect(() => {
    if (view.kind !== "home" || !workspace.workspaceId || !rootNamespace.ready || !activeWorkspace) {
      return;
    }
    const resolved = resolveWorkspace(activeWorkspace, rootDocuments);
    if (resolved.kind === "workspace" && resolved.view.kind === "document") {
      navigate(resolved, { replace: true });
    }
  }, [activeWorkspace, rootDocuments, rootNamespace.ready, view.kind, workspace.workspaceId]);

  useEffect(() => {
    const documentId = view.kind === "document" ? activeDocument?.id ?? "" : "";
    if (view.kind === "document" && !documentId) {
      return;
    }
    const accessKey = `${workspaceId}\0${documentId}`;
    if (lastAccessUpdateKeyRef.current === accessKey) {
      return;
    }
    lastAccessUpdateKeyRef.current = accessKey;
    onAccess(workspaceId, documentId);
    void api.updateLastAccessed(workspaceId, documentId ? { documentId } : {}).catch(() => {});
  }, [activeDocument, api, onAccess, view.kind, workspaceId]);

  const startRenamingDocument = useCallback((document: DocumentItem) => {
    setRenamingDocumentId(document.id);
    setTitleDraft(fileName(document.path));
  }, []);

  const cancelRenamingDocument = useCallback(() => {
    setRenamingDocumentId("");
    setFreshDocumentId("");
    setTitleDraft("");
  }, []);

  const commitDocumentTitle = useCallback(
    (document: DocumentItem, draft: string) => {
      const nextPath = documentPathFromFileName(document, rootDocuments, draft);
      if (nextPath !== document.path) {
        rootNamespace.moveFile(document.id, nextPath);
      }
      setRenamingDocumentId("");
      setFreshDocumentId("");
      setTitleDraft("");
    },
    [rootDocuments, rootNamespace],
  );

  const createDocument = useCallback(async () => {
    if (creatingDocument || !workspace.rootDocumentId) {
      return;
    }
    setCreatingDocument(true);
    setCreateError("");
    try {
      const doc = await api.createDocument(workspaceId);
      const path = untitledDocumentPath(rootDocuments, activeDocument?.path);
      rootNamespace.upsertFile(doc.id, path);
      navigate({ kind: "workspace", slug: workspaceSlug, view: { kind: "document", documentId: doc.id } });
      setRightTab("threads");
      setSelectedThreadId("");
      setRenamingDocumentId(doc.id);
      setFreshDocumentId(doc.id);
      setTitleDraft(fileName(path));
      void reload();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreatingDocument(false);
    }
  }, [
    activeDocument?.path,
    api,
    creatingDocument,
    reload,
    rootDocuments,
    rootNamespace,
    workspace.rootDocumentId,
    workspaceId,
    workspaceSlug,
  ]);

  const toggleFolder = (path: string) => {
    setCollapsedFolders((current) => {
      const next = new Set(current);
      if (next.has(path)) {
        next.delete(path);
      } else {
        next.add(path);
      }
      return next;
    });
  };

  // Reveal the active document by expanding any collapsed ancestor folders.
  const activeDocumentPath = activeDocument?.path;
  useEffect(() => {
    const ancestors = folderAncestors(activeDocumentPath);
    if (!ancestors.length) {
      return;
    }
    setCollapsedFolders((current) => {
      if (!ancestors.some((path) => current.has(path))) {
        return current;
      }
      const next = new Set(current);
      for (const path of ancestors) {
        next.delete(path);
      }
      return next;
    });
  }, [activeDocumentPath]);
  const workspacePeopleList = workspacePeople(workspace);
  // The Participants tab badge shows who is present on THIS document right now (here-now); subscriber counts
  // live inside the panel, which owns the fetch. hereNow needs only presence, so it is cheap to derive here.
  const documentPresentCount = useMemo(
    () => (activeDocument ? documentParticipants(workspace, activeDocument.id, [], now).hereNow.length : 0),
    [workspace, activeDocument?.id, now],
  );
  const documentActivityList = documentActivity(workspace, activeDocument?.id);
  // Unread = new since you last opened the tab (snapshot delta — honest for
  // snapshot-fresh activity; not a live stream). Marked seen when the tab opens.
  const [activitySeenAt, setActivitySeenAt] = useState("");
  const newestActivityAt = documentActivityList[0]?.occurredAt ?? "";
  const activityUnread = rightTab !== "activity" && newestActivityAt !== "" && newestActivityAt > activitySeenAt;
  useEffect(() => {
    if (rightTab === "activity" && newestActivityAt) {
      setActivitySeenAt(newestActivityAt);
    }
  }, [rightTab, newestActivityAt]);
  const activityActorLabel: Record<string, string> = {};
  for (const user of workspace.users) {
    activityActorLabel[user.id] = user.handle;
  }
  for (const agent of workspace.agents) {
    activityActorLabel[agent.id] = agent.handle;
  }

  useEffect(() => {
    if (selectedThreadId && !documentThreads.some((thread) => thread.id === selectedThreadId)) {
      setSelectedThreadId("");
    }
  }, [documentThreads, selectedThreadId]);

  useEffect(() => {
    if (renamingDocumentId && !rootDocuments.some((document) => document.id === renamingDocumentId)) {
      cancelRenamingDocument();
    }
  }, [cancelRenamingDocument, renamingDocumentId, rootDocuments]);

  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "n") {
        event.preventDefault();
        void createDocument();
      }
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => {
      window.removeEventListener("keydown", handleKeyDown);
    };
  }, [createDocument]);

  return (
    <main
      ref={shellRef}
      className={`shell${sidebarCollapsed ? " sidebar-collapsed" : ""}${draggingRail ? " dragging" : ""}`}
      style={rightWidth != null ? ({ "--right": `${rightWidth}px` } as CSSProperties) : undefined}
    >
      <button
        className="sb-rail-handle"
        type="button"
        onClick={toggleSidebar}
        aria-label={sidebarCollapsed ? "Expand sidebar" : "Collapse sidebar"}
        data-tip={sidebarCollapsed ? "Expand sidebar" : "Collapse sidebar"}
      >
        {sidebarCollapsed ? "›" : "‹"}
      </button>
      <aside className={`sb ${inboxOpen ? "inbox-open" : ""}`}>
        <div className="workspace-switcher">
          <div className="row gap-8 min-0">
            <div className="avi workspace-avi">{initials(activeWorkspace?.name ?? workspace.name)}</div>
            <div className="col gap-0 min-0">
              <b className="small truncate">{workspace.name || activeWorkspace?.name || "Workspace"}</b>
              <span className="tiny muted truncate">{currentWorkspaceUserHandle}</span>
            </div>
          </div>
          <select aria-label="Workspace" value={workspaceSlug} onChange={(event) => onWorkspaceChange(event.target.value)}>
            {workspaces.map((workspace) => (
              <option value={workspace.slug} key={workspace.id}>{workspace.name}</option>
            ))}
          </select>
        </div>

        <div className="sb-search">
          <div className="input search-box coming-soon" aria-disabled="true" title="Coming soon">
            <div className="row gap-6">
              <Icon name="search" />
              <span>Search — coming soon</span>
            </div>
          </div>
        </div>

        <nav className="sb-section inbox-nav" ref={inboxRef}>
          <button
            ref={inboxTriggerRef}
            className={`nav-item inbox-trigger ${inboxOpen ? "on" : ""}`}
            type="button"
            aria-label={inboxAriaLabel}
            aria-haspopup="dialog"
            aria-expanded={inboxOpen}
            aria-controls="workspace-inbox-flyout"
            title={inboxAriaLabel}
            onClick={() => {
              setInboxOpen((open) => !open);
            }}
          >
            <Icon name="inbox" />
            <span>Inbox</span>
            {inboxCount ? (
              <>
                <span className={`ct has inbox-ct ${inboxSummary.badgeTone}`} title={inboxAriaLabel}>
                  {inboxBadgeText(inboxCount)}
                </span>
                <em className={`inbox-corner-badge ${inboxSummary.badgeTone}`} aria-hidden="true">
                  {inboxBadgeText(inboxCount)}
                </em>
              </>
            ) : null}
          </button>
          {inboxOpen ? (
            <WorkspaceInboxFlyout
              dialogRef={inboxDialogRef}
              summary={inboxSummary}
              documents={rootDocuments}
              onItem={openInboxItem}
            />
          ) : null}
        </nav>

        <div className="divider sb-divider" />

        <section className="sb-section flex-1 doc-tree">
          <div className="lab">
            <span className="label">Documents</span>
            <button
              className="btn ghost icon sm"
              title="New doc"
              aria-label="New document"
              type="button"
              onClick={() => void createDocument()}
              disabled={creatingDocument || !workspace.rootDocumentId}
            >
              <Icon name="plus" />
            </button>
          </div>
          <div className="doc-tree-body">
            <DocumentTree
              nodes={documentTree}
              activeDocumentId={activeDocument?.id ?? ""}
              collapsedFolders={collapsedFolders}
              renamingDocumentId={renamingDocumentId}
              freshDocumentId={freshDocumentId}
              renamingDraft={titleDraft}
              onToggleFolder={toggleFolder}
              threadCountFor={(id) => threadCountByDocument.get(id) ?? 0}
              onSelectDocument={(id) => {
                navigate({ kind: "workspace", slug: workspaceSlug, view: { kind: "document", documentId: id } });
              }}
            />
          </div>
          {!rootNamespace.ready ? <p className="tiny muted empty-note">Syncing documents...</p> : null}
          {rootNamespace.ready && !rootDocuments.length ? <p className="tiny muted empty-note">No documents yet.</p> : null}
        </section>

        <div className="divider" />
        <section className="agent-summary">
          <div className="row between">
            <span className="label">Agents</span>
            <button className="btn ghost sm" type="button" onClick={() => setModal("agent")}>
              <Icon name="plus" />
              New
            </button>
          </div>
          <div className="col gap-6 agent-summary-list">
            {workspace.agents.map((agent) => {
              const status = visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons, workspace.agentEvents, now);
              return (
                <button
                  key={agent.id}
                  className="agent-mini row gap-8"
                  type="button"
                  title={`@${agent.handle}: ${status.title}`}
                  aria-label={`Open @${agent.handle}. Status: ${status.label}`}
                  onClick={() => {
                    setSelectedAgentId(agent.id);
                    setModal("agent-detail");
                  }}
                >
                  <div className="avi sm agent">{initials(agent.handle)}</div>
                  <span className="agent-mini-copy">
                    <span className="small truncate">@{agent.handle}</span>
                    <span className="agent-mini-status" title={status.title} aria-label={`Status: ${status.label}`}>
                      <StatusDot tone={status.tone} />
                      <span className={`agent-mini-status-label ${status.tone}`}>{status.label}</span>
                    </span>
                  </span>
                </button>
              );
            })}
            {!workspace.agents.length ? <span className="tiny muted">Create an agent after deploying a local environment.</span> : null}
          </div>
        </section>

        <button
          className="manage-entry"
          type="button"
          onClick={() => {
            setManageTab("members");
            setModal("manage");
          }}
        >
          <Icon name="settings" />
          <span>Manage / Settings</span>
        </button>

        <footer className="account-footer">
          <div className="row between">
            <div className="row gap-8 min-0">
              <div className="avi sm you">{initials(currentWorkspaceUserIdentity)}</div>
              <span className="small truncate">{currentWorkspaceUserHandle}</span>
            </div>
            <button className="btn ghost sm" onClick={onSignOut}>Sign out</button>
          </div>
        </footer>
      </aside>

      <section className="doc-area">
        <header className="doc-toolbar">
          <div className="breadcrumb document-breadcrumb">
            {activeDocument ? (
              <DocumentPathBar
                document={activeDocument}
                workspaceName={workspace.name || activeWorkspace?.name || "Workspace"}
                draft={renamingDocumentId === activeDocument.id ? titleDraft : fileName(activeDocument.path)}
                editing={renamingDocumentId === activeDocument.id}
              />
            ) : (
              <>
                <span>{activeFolder}</span>
                <Icon name="chevron" />
                <b>No document selected</b>
              </>
            )}
            {!connected ? <span className="chip sm warn">workspace offline</span> : null}
          </div>
          <div className="row gap-6">
            <CollaboratorAvatars people={workspacePeopleList} onClick={() => setRightTab("coworkers")} />
            <button className="btn sm ghost icon" type="button" onClick={() => setModal("rename")} disabled={!activeDocument} aria-label="Document options">
              <Icon name="more" />
            </button>
          </div>
        </header>
        {loading ? <div className="notice compact">Loading workspace...</div> : null}
        {error ? <div className="notice error compact">{error}</div> : null}
        {createError ? <div className="notice error compact">{createError}</div> : null}
        {documentMissing ? (
          <DocumentNotFound
            onBackToWorkspace={() => {
              navigate({ kind: "workspace", slug: workspaceSlug, view: { kind: "home" } }, { replace: true });
            }}
          />
        ) : activeDocument ? (
          <DocumentEditor
            api={api}
            token={token}
            workspaceId={workspaceId}
            actorName={currentWorkspaceUser?.name || currentWorkspaceUserHandle}
            actorLabel={currentWorkspaceUserHandle}
            document={activeDocument}
            threads={documentThreads}
            focusThreadId={focusThreadId}
            onFocusThreadHandled={() => setFocusThreadId("")}
            onThreadCreated={(threadId) => {
              setRightTab("threads");
              setSelectedThreadId(threadId);
              void reload();
            }}
            onThreadsChanged={() => void reload()}
            onJumpToThread={(threadId) => setFocusThreadId(threadId)}
            titleEditing={renamingDocumentId === activeDocument.id}
            titleDraft={renamingDocumentId === activeDocument.id ? titleDraft : fileName(activeDocument.path)}
            onTitleEditStart={() => startRenamingDocument(activeDocument)}
            onTitleDraftChange={setTitleDraft}
            onTitleEditCancel={cancelRenamingDocument}
            onTitleCommit={(draft) => commitDocumentTitle(activeDocument, draft)}
            onThreadAnchorInfo={setThreadAnchorInfo}
          />
        ) : rootNamespace.ready && rootDocuments.length ? (
          <div className="notice compact">Opening document...</div>
        ) : (
          <EmptyWorkspace
            onCreateDocument={() => void createDocument()}
            onCreateDaemon={() => setModal("daemon")}
            creatingDocument={creatingDocument}
            canCreateDocument={Boolean(workspace.rootDocumentId)}
          />
        )}
      </section>

      <aside className="ctx">
        <div
          className="rz"
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize context panel"
          onMouseDown={(event) => {
            setDraggingRail(true);
            event.preventDefault();
          }}
        />
        <div className="ctx-tabs">
          {(["threads", "activity", "coworkers"] as const).map((tab) => (
            <button key={tab} className={`btn sm ${rightTab === tab ? "selected" : "ghost"}`} onClick={() => setRightTab(tab)}>
              <Icon name={tab === "threads" ? "thread" : tab === "activity" ? "activity" : "people"} />
              {rightTabLabels[tab]}
              {tab === "threads" ? <span className="muted">{documentThreads.length}</span> : null}
              {tab === "coworkers" ? <span className="muted">{documentPresentCount}</span> : null}
              {tab === "activity" && activityUnread ? <span className="unread-dot" aria-label="New activity" /> : null}
            </button>
          ))}
        </div>
        {rightTab === "threads" ? (
          <ThreadsPanel
            api={api}
            workspaceId={workspaceId}
            threads={documentThreads}
            threadAnchorInfo={threadAnchorInfo}
            selectedThreadId={selectedThreadId}
            onSelectThread={setSelectedThreadId}
            onJumpToThread={(threadId) => {
              if (activeDocument) {
                navigate({ kind: "workspace", slug: workspaceSlug, view: { kind: "document", documentId: activeDocument.id } });
              }
              setFocusThreadId(threadId);
            }}
            onReply={() => void reload()}
          />
        ) : null}
        {rightTab === "activity" ? <ActivityPanel activities={documentActivityList} hasDocument={!!activeDocument} actorLabel={activityActorLabel} /> : null}
        {rightTab === "coworkers" ? (
          <ParticipantsPanel
            // Key on scope so a workspace/document switch REMOUNTS the panel: the old scope's state is
            // discarded and unrenderable, closing the async-race window (a stale read lands on an unmounted
            // instance). This is the make-invalid-states-unrepresentable fix for the QA-caught switch race.
            key={workspaceId + "/" + (activeDocument?.id ?? "none")}
            documentId={activeDocument?.id}
            workspace={workspace}
            agents={workspace.agents}
            api={api}
            workspaceId={workspaceId}
            onAgent={(agent) => {
              setSelectedAgentId(agent.id);
              setModal("agent-detail");
            }}
          />
        ) : null}
      </aside>

      {modal === "rename" && activeDocument ? (
        <RenameDocumentModal
          document={activeDocument}
          onClose={() => setModal(null)}
          onRename={async (path) => {
            rootNamespace.moveFile(activeDocument.id, path);
            setModal(null);
          }}
          onDelete={async () => {
            rootNamespace.tombstoneFile(activeDocument.id);
            navigate({ kind: "workspace", slug: workspaceSlug, view: { kind: "home" } }, { replace: true });
            setModal(null);
          }}
        />
      ) : null}
      {modal === "daemon" ? <CreateDaemonModal api={api} workspaceId={workspaceId} daemons={workspace.daemons} onClose={() => setModal(null)} onDone={() => void reload()} /> : null}
      {modal === "agent" ? <CreateAgentModal api={api} workspaceId={workspaceId} daemons={workspace.daemons} onClose={() => setModal(null)} onDone={() => { setModal(null); void reload(); }} /> : null}
      {modal === "agent-detail" && selectedAgentId ? <AgentDetailModal api={api} workspaceId={workspaceId} agentId={selectedAgentId} agents={workspace.agents} daemons={workspace.daemons} runs={workspace.agentRuns} agentEvents={workspace.agentEvents} onClose={() => setModal(null)} onChanged={() => void reload()} /> : null}
      {modal === "daemon-detail" && selectedDaemonId ? <DaemonDetailModal api={api} workspaceId={workspaceId} daemonId={selectedDaemonId} daemons={workspace.daemons} agents={workspace.agents} runs={workspace.agentRuns} agentEvents={workspace.agentEvents} onClose={() => setModal(null)} onChanged={() => { setModal(null); void reload(); }} /> : null}
      {modal === "manage" ? (
        <ManageModal
          api={api}
          workspaceId={workspaceId}
          workspace={workspace}
          workspaceSlug={workspaceSlug}
          activeTab={manageTab}
          canInvite={canInviteMembers}
          groupedAgents={groupedAgents}
          onTabChange={setManageTab}
          onClose={() => setModal(null)}
          onRefresh={() => void reload()}
          onNewDaemon={() => setModal("daemon")}
          onDaemon={(daemon) => {
            setSelectedDaemonId(daemon.id);
            setModal("daemon-detail");
          }}
          onNewAgent={() => setModal("agent")}
          onAgent={(agent) => {
            setSelectedAgentId(agent.id);
            setModal("agent-detail");
          }}
          onWorkspaceSaved={(updatedWorkspace) => {
            onWorkspaceUpdated(updatedWorkspace);
            if (updatedWorkspace.slug !== workspaceSlug) {
              navigate({ kind: "workspace", slug: updatedWorkspace.slug, view }, { replace: true });
            }
          }}
        />
      ) : null}
    </main>
  );
}

export function DocumentPathBar({
  document,
  workspaceName,
  editing,
  draft,
}: {
  document: DocumentItem;
  workspaceName: string;
  editing: boolean;
  draft: string;
}) {
  const folder = parentPath(document.path);
  const folderSegments = folder.split("/").filter(Boolean);
  const file = editing ? displayFileName(document, document.id, draft) : fileName(document.path);
  const extensionIndex = file.lastIndexOf(".");
  const fileStem = extensionIndex > 0 ? file.slice(0, extensionIndex) : file;
  const fileExtension = extensionIndex > 0 ? file.slice(extensionIndex) : "";

  return (
    <div className="docpath-title">
      <span className="docpath-seg">{workspaceName}</span>
      {folderSegments.map((segment, index) => (
        <span className="docpath-part" key={`${segment}:${index}`}>
          <span className="docpath-sep">/</span>
          <span className="docpath-seg">{segment}</span>
        </span>
      ))}
      <span className="docpath-sep">/</span>
      <span className="docpath-file">
        <span className="docpath-file-stem">{fileStem}</span>
        {fileExtension ? <span className="docpath-file-ext">{fileExtension}</span> : null}
      </span>
    </div>
  );
}

function DocumentTitleEditor({
  document,
  editing,
  draft,
  onStartEdit,
  onDraftChange,
  onCancel,
  onCommit,
}: {
  document: DocumentItem;
  editing: boolean;
  draft: string;
  onStartEdit: () => void;
  onDraftChange: (value: string) => void;
  onCancel: () => void;
  onCommit: (value: string) => void;
}) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const skipBlurCommitRef = useRef(false);
  const value = editing ? draft : fileName(document.path);
  const visibleName = splitVisibleFileName(value);

  const focusStem = () => {
    window.setTimeout(() => {
      const input = inputRef.current;
      if (!input) {
        return;
      }
      input.focus();
      input.setSelectionRange(0, editableFileStem(input.value).length);
    }, 0);
  };

  useEffect(() => {
    if (!editing) {
      return;
    }
    focusStem();
  }, [document.id, editing]);

  return (
    <div
      className={`document-title-field ${editing ? "editing" : ""}`}
      onMouseDown={(event) => {
        if (window.document.activeElement === inputRef.current) {
          return;
        }
        event.preventDefault();
        onStartEdit();
        focusStem();
      }}
    >
      <span className="document-title-mirror" aria-hidden="true">
        <span className={`document-title-mirror-stem ${visibleName.stem ? "" : "placeholder"}`}>
          {visibleName.stem || "Untitled"}
        </span>
        {visibleName.extension ? <span className="document-title-mirror-ext">{visibleName.extension}</span> : null}
      </span>
      <input
        ref={inputRef}
        className="document-title-input"
        aria-label="Document title"
        value={value}
        placeholder="Untitled"
        onFocus={() => {
          onStartEdit();
          const input = inputRef.current;
          input?.setSelectionRange(0, editableFileStem(input.value).length);
        }}
        onChange={(event) => onDraftChange(filterDocumentFileNameInput(event.target.value))}
        onKeyDown={(event) => {
          if (event.key === "Enter") {
            event.preventDefault();
            skipBlurCommitRef.current = true;
            onCommit(event.currentTarget.value || `Untitled${documentExtension(document.path)}`);
            event.currentTarget.blur();
          }
          if (event.key === "Escape") {
            event.preventDefault();
            skipBlurCommitRef.current = true;
            onDraftChange(fileName(document.path));
            onCancel();
            event.currentTarget.blur();
          }
        }}
        onBlur={(event) => {
          if (skipBlurCommitRef.current) {
            skipBlurCommitRef.current = false;
          return;
        }
        if (editing) {
          onCommit(event.currentTarget.value || `Untitled${documentExtension(document.path)}`);
        }
      }}
      />
    </div>
  );
}

export function DocumentTree(props: {
  nodes: DocTreeNode[];
  activeDocumentId: string;
  collapsedFolders: Set<string>;
  renamingDocumentId: string;
  freshDocumentId: string;
  renamingDraft: string;
  onToggleFolder: (path: string) => void;
  threadCountFor: (documentId: string) => number;
  onSelectDocument: (documentId: string) => void;
}) {
  const {
    nodes,
    activeDocumentId,
    collapsedFolders,
    renamingDocumentId,
    freshDocumentId,
    renamingDraft,
    onToggleFolder,
    threadCountFor,
    onSelectDocument,
  } = props;
  return (
    <>
      {nodes.map((node) => {
        if (node.kind === "folder") {
          const expanded = !collapsedFolders.has(node.path);
          return (
            <div className="tree-group" key={`folder:${node.path}`}>
              <button
                className="nav-item folder-row"
                type="button"
                onClick={() => onToggleFolder(node.path)}
                aria-expanded={expanded}
              >
                <span className={`car ${expanded ? "open" : ""}`}>
                  <Icon name="caret" />
                </span>
                <span className="truncate">{node.name}</span>
                <span className="ct">{node.fileCount}</span>
              </button>
              {expanded && node.children.length ? (
                <div className="tree-children">
                  <DocumentTree {...props} nodes={node.children} />
                </div>
              ) : null}
            </div>
          );
        }
        const threadCount = threadCountFor(node.document.id);
        const active = node.document.id === activeDocumentId;
        const renaming = node.document.id === renamingDocumentId;
        return (
          <button
            className={`nav-item file-row ${active ? "on" : ""} ${renaming ? "renaming-row" : ""}`}
            key={`file:${node.document.id}`}
            type="button"
            onClick={() => onSelectDocument(node.document.id)}
          >
            <span className="car leaf" />
            <span className="truncate">{renaming ? displayFileName(node.document, renamingDocumentId, renamingDraft) : node.name}</span>
            {node.document.id === freshDocumentId ? <span className="chip sm accent">new</span> : null}
            {threadCount ? <span className="ct has">{threadCount}</span> : null}
          </button>
        );
      })}
    </>
  );
}

// Coarse re-render cadence so a daemon that stops checking in decays online -> stale ->
// disconnected on its own. 12s is ample against the 30s online window; it only recomputes from
// state — no polling, no network.
const DAEMON_LIVENESS_TICK_MS = 12_000;

// Returns a wall-clock timestamp that advances every intervalMs, so time-derived UI (liveness
// decay, "last check-in … ago") re-renders without any new data.
function useNowTicker(intervalMs: number) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}

const inboxDotColor: Record<WorkspaceInboxItem["kind"], string> = {
  "needs-review": "var(--needs-you)",
  failed: "var(--danger)",
};

const inboxGlyph: Record<WorkspaceInboxItem["kind"], string> = {
  "needs-review": "review",
  failed: "alert",
};

const inboxKindLabel: Record<WorkspaceInboxItem["kind"], string> = {
  "needs-review": "Needs review",
  failed: "Failed",
};

function WorkspaceInboxFlyout({
  dialogRef,
  summary,
  documents,
  onItem,
}: {
  dialogRef: RefObject<HTMLDivElement>;
  summary: WorkspaceInboxSummary;
  documents: DocumentItem[];
  onItem: (item: WorkspaceInboxItem) => void;
}) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  const [expandedReasonId, setExpandedReasonId] = useState("");

  const renderRowBody = (item: WorkspaceInboxItem) => {
    const documentLabel = inboxDocumentLabel(documents, item.documentId);
    const timeLabel = relativeTime(item.occurredAt, now);
    return (
      <>
        <span className="inbox-kind" title={inboxKindLabel[item.kind]}>
          <span className={`inbox-dot ${item.unread ? "solid" : "hollow"}`} style={{ "--dot": inboxDotColor[item.kind] } as CSSProperties} />
          <Icon name={inboxGlyph[item.kind]} />
        </span>
        <span className="inbox-copy min-0">
          <span className="inbox-main">
            <strong>{item.actorLabel}</strong>
            <span>{item.action}</span>
            {documentLabel ? <span className="inbox-doc-link">{documentLabel}</span> : null}
          </span>
          <span className="inbox-detail">
            <span className="truncate">{item.summary}</span>
            {timeLabel ? <span className="inbox-time">{timeLabel}</span> : null}
          </span>
          {item.kind === "failed" ? (
            <span className="inbox-failed-detail">
              <button
                className="inbox-reason-link"
                type="button"
                onClick={() => setExpandedReasonId((current) => (current === item.id ? "" : item.id))}
                aria-expanded={expandedReasonId === item.id}
              >
                View reason
              </button>
              {expandedReasonId === item.id ? <span className="inbox-reason">{item.reason || item.summary}</span> : null}
            </span>
          ) : null}
        </span>
      </>
    );
  };

  return (
    <div
      id="workspace-inbox-flyout"
      className="inbox-flyout"
      role="dialog"
      aria-label="Inbox"
      tabIndex={-1}
      ref={dialogRef}
    >
      <div className="inbox-flyout-head">
        <div className="col gap-2 min-0">
          <b>Inbox</b>
          <span className="tiny muted">
            {summary.counts.total ? `${summary.counts.total} need attention` : "Inbox zero"}
          </span>
        </div>
      </div>
      <div className="inbox-list">
        {summary.items.map((item) => {
          const canOpen = item.kind === "needs-review" && !!item.agentId;
          const className = `inbox-row ${item.kind} ${item.unread ? "unread" : "read"}`;
          return canOpen ? (
            <button className={className} type="button" key={item.id} onClick={() => onItem(item)}>
              {renderRowBody(item)}
            </button>
          ) : (
            <article className={className} key={item.id}>
              {renderRowBody(item)}
            </article>
          );
        })}
        {!summary.items.length ? <p className="inbox-empty">Inbox zero · No attention needed.</p> : null}
      </div>
    </div>
  );
}

export function DaemonsManagement({
  workspace,
  onRefresh,
  onNew,
  onDaemon,
}: {
  workspace: ReturnType<typeof useWorkspace>["workspace"];
  onRefresh: () => void;
  onNew: () => void;
  onDaemon: (daemon: Daemon) => void;
}) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  const visibleDaemons = workspace.daemons.filter((daemon) => daemon.status !== "deleted");
  const countByStatus = (status: string) => visibleDaemons.filter((daemon) => daemonLiveStatus(daemon, now) === status).length;
  const agentsForDaemon = (daemonId: string) => workspace.agents.filter((agent) => agent.daemonId === daemonId);

  return (
    <div className="management-canvas">
      <div className="management-inner">
        <div className="management-head">
          <div className="col gap-2">
            <div className="row gap-8">
              <span className="display management-title">Local environments</span>
              <span className="chip">{visibleDaemons.length} total</span>
            </div>
            <div className="small muted">Local environments that sync this workspace and run agents.</div>
          </div>
          <div className="row gap-6">
            <button className="btn" type="button" onClick={onRefresh}>
              <Icon name="refresh" />
              Check liveness
            </button>
            <button className="btn accent" type="button" onClick={onNew}>
              <Icon name="plus" />
              New local environment
            </button>
          </div>
        </div>

        <div className="metric-grid">
          <MetricCard label="Online" value={countByStatus("online")} tone="ok" />
          <MetricCard label="Stale" value={countByStatus("stale")} tone="warn" />
          <MetricCard label="Offline" value={countByStatus("disconnected")} tone="err" />
          <MetricCard label="Agents hosted" value={workspace.agents.length} />
        </div>

        <div className="card flat table-card">
          <table className="ds-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Status</th>
                <th>Agents</th>
                <th>Fingerprint</th>
                <th>Last check-in</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {visibleDaemons.map((daemon) => {
                const status = daemonLiveStatus(daemon, now);
                return (
                  <tr key={daemon.id} onClick={() => onDaemon(daemon)}>
                    <td>
                      <div className="row gap-8">
                        <div className="avi sm daemon">{initials(daemon.name)}</div>
                        <div className="col gap-0 min-0">
                          <b className="small truncate">{daemon.name}</b>
                          <div className="tiny muted mono truncate">{daemon.id}</div>
                        </div>
                      </div>
                    </td>
                    <td><span className={`chip sm ${status}`}><StatusDot tone={status} />{status}</span></td>
                    <td>{agentsForDaemon(daemon.id).length}</td>
                    <td className="mono small muted">{daemon.id.slice(0, 10)}…{daemon.id.slice(-4)}</td>
                    <td className="small muted">
                      {daemon.lastSeenAt && new Date(daemon.lastSeenAt).getUTCFullYear() >= 2020 ? `${shortTime(daemon.lastSeenAt)} ago` : "never"}
                    </td>
                    <td><button className="btn ghost icon sm" type="button" onClick={(event) => { event.stopPropagation(); onDaemon(daemon); }}><Icon name="more" /></button></td>
                  </tr>
                );
              })}
              {!visibleDaemons.length ? (
                <tr>
                  <td colSpan={6} className="small muted">No local environments yet. Create one to sync docs locally and host agents.</td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}

function AgentsManagement({
  workspace,
  groupedAgents,
  onNew,
  onAgent,
}: {
  workspace: ReturnType<typeof useWorkspace>["workspace"];
  groupedAgents: Array<{ daemonId: string; daemonName: string; agents: Agent[] }>;
  onNew: () => void;
  onAgent: (agent: Agent) => void;
}) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  const running = workspace.agents.filter((agent) => visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons, workspace.agentEvents, now).key === "running").length;
  const daemonById = new Map(workspace.daemons.map((daemon) => [daemon.id, daemon]));

  return (
    <div className="management-canvas">
      <div className="management-inner">
        <div className="management-head">
          <div className="col gap-2">
            <div className="row gap-8">
              <span className="display management-title">Agents</span>
              <span className="chip">{workspace.agents.length} total · {running} running</span>
            </div>
            <div className="small muted">Codex collaborators in this workspace. Each is owned by a local environment.</div>
          </div>
          <div className="row gap-6">
            <button className="btn accent" type="button" onClick={onNew}>
              <Icon name="plus" />
              New agent
            </button>
          </div>
        </div>

        {groupedAgents.map((group) => {
          const daemon = daemonById.get(group.daemonId);
          const daemonTone = daemon ? daemonLiveStatus(daemon, now) : "disconnected";
          return (
            <section key={group.daemonId}>
              <div className="roster-group-head">
                <div className="avi sm daemon">{initials(group.daemonName)}</div>
                <span><b>{group.daemonName}</b></span>
                <span className={`chip sm ${daemonTone}`}><StatusDot tone={daemonTone} />{daemonTone}</span>
                <span className="stripe" />
                <span>{group.agents.length} agents</span>
              </div>
              <div className="roster-grid">
                {group.agents.map((agent) => {
                  const status = visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons, workspace.agentEvents, now);
                  const statusLabel = detailStatusLabel(status);
                  return (
                    <button className="agent-roster-card" key={agent.id} onClick={() => onAgent(agent)} aria-label={`Open @${agent.handle}. Status: ${statusLabel}`}>
                      <div className="agent-roster-top">
                        <div className="agent-roster-identity">
                          <div className="avi agent agent-roster-avatar">{initials(agent.handle)}</div>
                          <div className="col gap-0 min-0">
                            <b className="truncate">@{agent.handle}</b>
                            <span className="tiny muted truncate">{agent.role}</span>
                          </div>
                        </div>
                        <span className={`chip sm agent-roster-status ${status.tone}`} title={status.title}>
                          <StatusDot tone={status.tone} />
                          <span className="agent-chip-text">{statusLabel}</span>
                        </span>
                      </div>
                    </button>
                  );
                })}
              </div>
            </section>
          );
        })}
        {!workspace.agents.length ? (
          <p className="empty-note">
            {workspace.daemons.some((daemon) => daemon.status !== "deleted")
              ? "No agents yet. Add one to a local environment; it will start working when that local environment is online."
              : "No agents yet. Create a local environment first, then add an agent to it."}
          </p>
        ) : null}
      </div>
    </div>
  );
}

export function MetricCard({ label, value, tone }: { label: string; value: number; tone?: "ok" | "warn" | "err" }) {
  return (
    <div className="card p-12 metric-card">
      <div className="label">{label}</div>
      <div className={`display metric-value ${tone ?? ""}`}>{value}</div>
    </div>
  );
}

function orderPopoverThreads(threads: LiveThread[]) {
  return [...threads].sort((left, right) => Number(left.status === "resolved") - Number(right.status === "resolved"));
}

function mergePopoverThread(current: LiveThread, updated: ThreadItem): LiveThread {
  return {
    ...updated,
    anchor: {
      ...updated.anchor,
      start: current.anchor.start,
      end: current.anchor.end,
      line: current.anchor.line,
      excerpt: updated.anchor.excerpt || current.anchor.excerpt,
      resolved: current.anchor.resolved,
    },
  };
}

export function ThreadPopover({
  api,
  workspaceId,
  group,
  point,
  onClose,
  onJumpToThread,
  onThreadsChanged,
}: {
  api: ApiClient;
  workspaceId: string;
  group: LineThreadGroup<LiveThread>;
  point: { x: number; y: number };
  onClose: () => void;
  onJumpToThread: (threadId: string) => void;
  onThreadsChanged: () => void;
}) {
  const popoverRef = useRef<HTMLDivElement | null>(null);
  const backButtonRef = useRef<HTMLButtonElement | null>(null);
  const [threadItems, setThreadItems] = useState(() => orderPopoverThreads(group.threads));
  const [selectedThreadId, setSelectedThreadId] = useState("");
  const [reply, setReply] = useState("");
  const [replyBusy, setReplyBusy] = useState(false);
  const [statusBusy, setStatusBusy] = useState(false);
  const [error, setError] = useState("");
  const [viewportHeight, setViewportHeight] = useState(() => window.visualViewport?.height ?? window.innerHeight);
  const groupKey = `${group.line}:${group.threads.map((thread) => thread.id).sort().join("|")}`;
  const selected = selectedThreadId ? threadItems.find((thread) => thread.id === selectedThreadId) ?? null : null;
  const popoverStyle = {
    left: point.x,
    top: point.y,
    "--thread-popover-viewport-height": `${viewportHeight}px`,
  } as CSSProperties;

  useEffect(() => {
    setThreadItems((current) => {
      const currentById = new Map(current.map((thread) => [thread.id, thread]));
      return orderPopoverThreads(
        group.threads.map((thread) => {
          const local = currentById.get(thread.id);
          return local && local.updatedAt > thread.updatedAt ? local : thread;
        }),
      );
    });
  }, [group.threads]);

  useEffect(() => {
    setSelectedThreadId("");
    setReply("");
    setError("");
  }, [groupKey]);

  useEffect(() => {
    const onPointerDown = (event: PointerEvent) => {
      if (event.target instanceof Node && !popoverRef.current?.contains(event.target)) {
        onClose();
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onClose();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [onClose]);

  useEffect(() => {
    const viewport = window.visualViewport;
    const updateHeight = () => setViewportHeight(viewport?.height ?? window.innerHeight);
    viewport?.addEventListener("resize", updateHeight);
    window.addEventListener("resize", updateHeight);
    return () => {
      viewport?.removeEventListener("resize", updateHeight);
      window.removeEventListener("resize", updateHeight);
    };
  }, []);

  useEffect(() => {
    if (selectedThreadId) {
      window.setTimeout(() => backButtonRef.current?.focus(), 0);
    }
  }, [selectedThreadId]);

  const replaceThread = (updated: ThreadItem) => {
    setThreadItems((current) => orderPopoverThreads(current.map((thread) => (
      thread.id === updated.id ? mergePopoverThread(thread, updated) : thread
    ))));
  };

  const openThread = (threadId: string) => {
    setSelectedThreadId(threadId);
    setReply("");
    setError("");
  };

  const backToList = () => {
    const previousThreadId = selectedThreadId;
    setSelectedThreadId("");
    setReply("");
    setError("");
    window.setTimeout(() => {
      const rows = popoverRef.current?.querySelectorAll<HTMLButtonElement>("[data-thread-id]");
      Array.from(rows ?? []).find((row) => row.dataset.threadId === previousThreadId)?.focus();
    }, 0);
  };

  const jumpToThread = (threadId: string) => {
    onJumpToThread(threadId);
  };

  const submitReply = async (event: FormEvent) => {
    event.preventDefault();
    if (!selected || !reply.trim() || replyBusy) return;
    setReplyBusy(true);
    setError("");
    try {
      const result = await api.replyThread(workspaceId, selected.id, reply.trim());
      replaceThread(result.thread);
      setReply("");
      onThreadsChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to send reply");
    } finally {
      setReplyBusy(false);
    }
  };

  const toggleStatus = async () => {
    if (!selected || statusBusy) return;
    const nextStatus = selected.status === "resolved" ? "open" : "resolved";
    setStatusBusy(true);
    setError("");
    try {
      const result = await api.updateThreadStatus(workspaceId, selected.id, nextStatus);
      replaceThread(result.thread);
      onThreadsChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update thread status");
    } finally {
      setStatusBusy(false);
    }
  };

  if (selected) {
    const resolved = selected.status === "resolved";
    const author = selected.createdByHandle ? `@${selected.createdByHandle}` : selected.createdByName || "Someone";
    return (
      <>
        <div className="thread-popover-scrim" aria-hidden="true" />
        <div
          ref={popoverRef}
          className="thread-popover thread-popover-detail card lifted"
          style={popoverStyle}
          role="dialog"
          aria-modal="false"
          aria-label={`Thread by ${author}`}
        >
          <div className="thread-popover-head thread-popover-detail-head">
            <button ref={backButtonRef} className="btn ghost icon sm" type="button" onClick={backToList} aria-label="Back to threads on this line">
              <Icon name="back" />
            </button>
            <span className={`thread-popover-status-dot ${resolved ? "resolved" : "open"}`} />
            <b className="small truncate">{author}</b>
            <span className="tiny muted">· {resolved ? "resolved" : "open"}</span>
            <span className="thread-popover-head-spacer" />
            <button className="btn ghost icon sm" onClick={onClose} type="button" aria-label="Close">×</button>
          </div>
          <div className="thread-popover-anchor">
            <span className="thread-popover-anchor-quote" aria-hidden="true">
              <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
                <path
                  d="M6.5 4.2C4.9 4.9 3.9 6.3 3.9 8.1v3.2h3.3V8.1H5.5c0-1 .5-1.7 1.6-2.1l-.6-1.8Zm5.6 0c-1.6.7-2.6 2.1-2.6 3.9v3.2h3.3V8.1h-1.7c0-1 .5-1.7 1.6-2.1l-.6-1.8Z"
                  fill="currentColor"
                />
              </svg>
            </span>
            <span className="thread-popover-anchor-excerpt">{selected.anchor.excerpt || selected.title}</span>
            <span className="thread-popover-anchor-line">line {group.line}</span>
          </div>
          <div className="thread-popover-messages">
            {selected.messages.map((message) => (
              <article className="thread-popover-message" key={message.id}>
                <div className={`avi sm ${message.authorType === "agent" ? "agent" : "you"}`}>
                  {initials(message.authorHandle || message.authorName || message.authorId)}
                </div>
                <div className="thread-popover-message-body">
                  <div className="row gap-6">
                    <strong className="small">@{message.authorHandle || message.authorName || message.authorId}</strong>
                    <span className="tiny muted">{shortTime(message.createdAt)}</span>
                  </div>
                  <p>{message.body}</p>
                </div>
              </article>
            ))}
          </div>
          <div className="thread-popover-actions">
            <button
              className="thread-popover-status-action"
              type="button"
              onClick={toggleStatus}
              disabled={statusBusy}
              aria-label={resolved ? "Reopen thread" : "Mark as resolved"}
            >
              <span className="thread-popover-check">✓</span>
              {statusBusy ? "Updating…" : resolved ? "Reopen" : "Mark as resolved"}
            </button>
            <button className="thread-popover-jump" type="button" onClick={() => jumpToThread(selected.id)} aria-label={`Jump to line ${group.line}`}>
              Jump to line {group.line} →
            </button>
          </div>
          {error ? <div className="thread-popover-error" role="alert">{error}</div> : null}
          <form className="thread-popover-reply" onSubmit={submitReply}>
            <textarea
              rows={1}
              value={reply}
              onChange={(event) => setReply(event.target.value)}
              onKeyDown={(event) => {
                if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
                  event.preventDefault();
                  event.currentTarget.form?.requestSubmit();
                }
              }}
              placeholder="Reply to this thread…"
              aria-label="Reply to this thread"
            />
            <button className="btn accent" disabled={!reply.trim() || replyBusy} aria-label="Send reply">
              {replyBusy ? "Sending…" : "Send"}
            </button>
          </form>
        </div>
      </>
    );
  }

  return (
    <>
      <div className="thread-popover-scrim" aria-hidden="true" />
      <div
        ref={popoverRef}
        className="thread-popover card lifted"
        style={popoverStyle}
        role="dialog"
        aria-modal="false"
        aria-label={`Threads on line ${group.line}`}
      >
        <div className="thread-popover-head">
          <Icon name="message" />
          <b className="small">Line {group.line}</b>
          <span className="small muted">· {threadItems.length} thread{threadItems.length === 1 ? "" : "s"}</span>
          <span className="thread-popover-head-spacer" />
          <button className="btn ghost icon sm" onClick={onClose} type="button" aria-label="Close">×</button>
        </div>
        <div className="thread-popover-list">
          {threadItems.map((thread) => {
            const resolved = thread.status === "resolved";
            const author = thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone";
            return (
              <button
                className={`thread-popover-row${resolved ? " resolved" : ""}`}
                key={thread.id}
                data-thread-id={thread.id}
                type="button"
                onClick={() => openThread(thread.id)}
                aria-label={`Open thread by ${author}`}
              >
                <div className={`avi sm ${thread.createdByType === "agent" ? "agent" : "you"}`}>
                  {initials(thread.createdByHandle || thread.createdByName || "T")}
                </div>
                <div className="col gap-2 min-0 thread-popover-row-copy">
                  <div className="small truncate">
                    <b>{author}</b> {thread.messages[0]?.body || thread.title}
                  </div>
                  <div className="tiny muted">{shortTime(thread.updatedAt)}</div>
                </div>
                <Icon name="chevron" />
              </button>
            );
          })}
        </div>
        <div className="thread-popover-foot">
          <button type="button" onClick={() => jumpToThread(threadItems[0]?.id || "")} aria-label={`Jump to line ${group.line}`}>
            Jump to line {group.line} →
          </button>
        </div>
      </div>
    </>
  );
}

export function DocumentEditor({
  api,
  token,
  workspaceId,
  actorName,
  actorLabel,
  document,
  threads,
  focusThreadId,
  onFocusThreadHandled,
  onThreadCreated,
  onThreadsChanged,
  onJumpToThread,
  titleEditing,
  titleDraft,
  onTitleEditStart,
  onTitleDraftChange,
  onTitleEditCancel,
  onTitleCommit,
  onThreadAnchorInfo,
}: {
  api: ApiClient;
  token: string;
  workspaceId: string;
  actorName: string;
  actorLabel: string;
  document: DocumentItem;
  threads: ThreadItem[];
  focusThreadId: string;
  onFocusThreadHandled: () => void;
  onThreadCreated: (threadId: string) => void;
  onThreadsChanged: () => void;
  onJumpToThread: (threadId: string) => void;
  titleEditing: boolean;
  titleDraft: string;
  onTitleEditStart: () => void;
  onTitleDraftChange: (value: string) => void;
  onTitleEditCancel: () => void;
  onTitleCommit: (value: string) => void;
  onThreadAnchorInfo: (info: Record<string, { orphaned: boolean; line: number }>) => void;
}) {
  const draftRef = useRef<HTMLTextAreaElement | null>(null);
  const [selection, setSelection] = useState<SurfaceSelection | null>(null);
  const [threadDraftOpen, setThreadDraftOpen] = useState(false);
  const [threadBody, setThreadBody] = useState("");
  const [activeThreadGroup, setActiveThreadGroup] = useState<LineThreadGroup<LiveThread> | null>(null);
  const [threadPopoverPoint, setThreadPopoverPoint] = useState({ x: 0, y: 0 });
  const [formatRequest, setFormatRequest] = useState<{ id: number; command: MarkdownPreviewCommandName } | null>(null);
  const isMarkdownDocument = isMarkdownDocumentPath(document.path);
  const { ydoc, ytext, ready, connected } = useDocumentSync({
    workspaceId,
    token,
    document,
    actorName,
  });
  const hasRangeSelection = Boolean(selection);
  const toolbarPoint = {
    x: clamp(selection?.point.x ?? 24, 12, Math.max(12, window.innerWidth - 680)),
    y: clamp((selection?.point.y ?? 120) - 48, 12, Math.max(12, window.innerHeight - 64)),
  };
  const drafterPoint = {
    x: clamp((selection?.point.x ?? 24) + 20, 12, Math.max(12, window.innerWidth - 340)),
    y: clamp((selection?.point.y ?? 120) + 28, 12, Math.max(12, window.innerHeight - 320)),
  };

  const lastAnchorInfoRef = useRef("");
  useEffect(() => {
    if (!ydoc || !ytext || !ready) return;
    // Y.js mutates ytext in place, so a live content edit does not change the
    // effect's dependencies. Recompute on every ytext change so a deletion that
    // orphans an anchor (or an edit that revives one) re-classifies live, not
    // only on reload. #40.
    const recompute = () => {
      const content = ytext.toString();
      const info: Record<string, { orphaned: boolean; line: number }> = {};
      for (const thread of threads) {
        const resolved = resolveThreadAnchorLive(thread.anchor, ydoc, content);
        info[thread.id] = { orphaned: !resolved.resolved && thread.anchor.kind !== "document", line: resolved.line };
      }
      const serialized = JSON.stringify(info);
      if (serialized !== lastAnchorInfoRef.current) {
        lastAnchorInfoRef.current = serialized;
        onThreadAnchorInfo(info);
      }
    };
    recompute();
    ytext.observe(recompute);
    return () => ytext.unobserve(recompute);
  }, [ydoc, ytext, ready, threads, onThreadAnchorInfo]);

  useEffect(() => {
    setActiveThreadGroup(null);
    setSelection(null);
    setThreadDraftOpen(false);
    setThreadBody("");
    setFormatRequest(null);
  }, [document.id, document.path]);

  useEffect(() => {
    if (!activeThreadGroup) return;
    const currentIds = new Set(activeThreadGroup.threads.map((thread) => thread.id));
    const content = ytext.toString();
    const nextThreads = threads
      .filter((thread) => currentIds.has(thread.id))
      .map((thread): LiveThread => ({ ...thread, anchor: resolveThreadAnchorLive(thread.anchor, ydoc, content) }));
    if (nextThreads.length) {
      setActiveThreadGroup({ line: activeThreadGroup.line, threads: nextThreads });
    }
  }, [threads, ydoc, ytext]);

  useEffect(() => {
    if (threadDraftOpen) {
      window.setTimeout(() => draftRef.current?.focus(), 0);
    }
  }, [threadDraftOpen]);

  const createThread = async (event: FormEvent) => {
    event.preventDefault();
    if (!threadBody.trim() || !selection) {
      return;
    }
    const result = await api.createThread(workspaceId, {
      documentId: document.id,
      title: selection.title,
      body: threadBody,
      relativeStart: selection.relativeStart,
      relativeEnd: selection.relativeEnd,
      kind: "text-range",
      excerpt: selection.excerpt.slice(0, 140),
      stateAtAnchor: selection.stateAtAnchor,
    });
    setThreadBody("");
    setSelection(null);
    setThreadDraftOpen(false);
    onThreadCreated(result.thread.id);
  };

  const openLineThreads = (group: LineThreadGroup<LiveThread>, point: { x: number; y: number }) => {
    setThreadPopoverPoint(point);
    setActiveThreadGroup(group);
  };

  const openThreadDraft = () => {
    if (!hasRangeSelection) {
      return;
    }
    setThreadDraftOpen(true);
  };

  const requestFormat = (command: MarkdownPreviewCommandName) => {
    setFormatRequest((current) => ({ id: (current?.id ?? 0) + 1, command }));
  };

  return (
    <div className="doc-canvas">
      <div className="doc-inner editor-body">
        <div className="document-title-row">
          <DocumentTitleEditor
            document={document}
            editing={titleEditing}
            draft={titleDraft}
            onStartEdit={onTitleEditStart}
            onDraftChange={onTitleDraftChange}
            onCancel={onTitleEditCancel}
            onCommit={onTitleCommit}
          />
        </div>

        <div className="doc-meta-row">
          <span className="chip">Document</span>
          <span className="chip outline">You · {actorLabel}</span>
          <span className={`chip outline ${connected ? "ok" : "warn"}`}>{connected ? "Live" : "Reconnecting"}</span>
        </div>

        <div className="editor-frame">
          <DocumentSurface
            documentId={document.id}
            ydoc={ydoc}
            ytext={ytext}
            ready={ready}
            threads={threads}
            focusThreadId={focusThreadId}
            onFocusThreadHandled={onFocusThreadHandled}
            onSelectionChange={setSelection}
            onLineThreadsOpen={openLineThreads}
            formatRequest={formatRequest}
            enableMarkdownLivePreview={isMarkdownDocument}
          />
        </div>

        {hasRangeSelection && !threadDraftOpen ? (
          <div
            className="selection-toolbar"
            style={{ left: toolbarPoint.x, top: toolbarPoint.y }}
            onMouseDown={(event) => event.preventDefault()}
          >
            <button className="primary" type="button" onClick={openThreadDraft}>
              <Icon name="thread" />
              Open thread
            </button>
            {isMarkdownDocument ? (
              <>
                <div className="sep" />
                <button type="button" title="Heading 1" onClick={() => requestFormat("heading1")}>H1</button>
                <button type="button" title="Heading 2" onClick={() => requestFormat("heading2")}>H2</button>
                <button type="button" title="Bold" onClick={() => requestFormat("bold")}><b>B</b></button>
                <button type="button" title="Italic" onClick={() => requestFormat("italic")}><i>I</i></button>
                <button type="button" title="Code" onClick={() => requestFormat("code")}><span className="mono">{"{ }"}</span></button>
                <button type="button" title="Link" onClick={() => requestFormat("link")}>Link</button>
                <button type="button" title="Quote" onClick={() => requestFormat("quote")}>Quote</button>
                <button type="button" title="Bulleted list" onClick={() => requestFormat("bulletList")}>List</button>
              </>
            ) : null}
          </div>
        ) : null}
        {threadDraftOpen ? (
          <form
            className="thread-drafter card lifted"
            style={{ left: drafterPoint.x, top: drafterPoint.y }}
            onSubmit={createThread}
          >
            <div className="thread-drafter-head">
              <div className="row gap-6">
                <Icon name="thread" />
                <b className="small">New thread</b>
              </div>
              <button className="btn ghost icon sm" onClick={() => setThreadDraftOpen(false)} type="button" aria-label="Close">×</button>
            </div>
            <div className="thread-drafter-body">
              <div className="quoted-range">
                <div className="line">{selection?.title ?? "Selection"}</div>
                <span>{selection?.excerpt || selection?.title || "Selection"}</span>
              </div>
              <textarea
                ref={draftRef}
                value={threadBody}
                onChange={(event) => setThreadBody(event.target.value)}
                placeholder="@codex-agent can you review this section?"
              />
              <div className="row between">
                <span className="tiny muted">{ready ? "Synced" : "Opening document..."} · <span className="kbd">⌘↵</span> to post</span>
                <button className="btn accent sm" disabled={!threadBody.trim() || !selection}>Open thread</button>
              </div>
            </div>
          </form>
        ) : null}

        {activeThreadGroup ? (
          <ThreadPopover
            api={api}
            workspaceId={workspaceId}
            group={activeThreadGroup}
            point={threadPopoverPoint}
            onClose={() => setActiveThreadGroup(null)}
            onJumpToThread={onJumpToThread}
            onThreadsChanged={onThreadsChanged}
          />
        ) : null}
      </div>
    </div>
  );
}

export function ThreadsPanel({
  api,
  workspaceId,
  threads,
  threadAnchorInfo,
  selectedThreadId,
  onSelectThread,
  onJumpToThread,
  onReply,
}: {
  api: ApiClient;
  workspaceId: string;
  threads: ThreadItem[];
  threadAnchorInfo?: Record<string, { orphaned: boolean; line: number }>;
  selectedThreadId: string;
  onSelectThread: (threadId: string) => void;
  onJumpToThread: (threadId: string) => void;
  onReply: () => void;
}) {
  const [reply, setReply] = useState("");
  const [statusBusy, setStatusBusy] = useState(false);
  const [statusError, setStatusError] = useState("");
  const [cardError, setCardError] = useState<Record<string, string>>({});
  const [resolvedFolded, setResolvedFolded] = useState(() => {
    try { return localStorage.getItem("codesk.threads.resolvedFolded") !== "false"; } catch { return true; }
  });
  const selected = selectedThreadId ? threads.find((thread) => thread.id === selectedThreadId) ?? null : null;
  const selectedResolved = selected?.status === "resolved";
  const openThreads = threads.filter((t) => t.status !== "resolved");
  const resolvedThreads = threads.filter((t) => t.status === "resolved");

  const toggleResolvedFold = () => {
    const next = !resolvedFolded;
    setResolvedFolded(next);
    try { localStorage.setItem("codesk.threads.resolvedFolded", String(next)); } catch {}
  };

  const toggleStatus = async () => {
    if (!selected || statusBusy) return;
    setStatusBusy(true);
    setStatusError("");
    try {
      await api.updateThreadStatus(workspaceId, selected.id, selectedResolved ? "open" : "resolved");
      onReply();
    } catch (err) {
      setStatusError(err instanceof Error ? err.message : "Failed to update status");
    } finally {
      setStatusBusy(false);
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!selected || !reply.trim()) {
      return;
    }
    await api.replyThread(workspaceId, selected.id, reply);
    setReply("");
    onReply();
  };

  if (!threads.length) {
    return (
      <div className="ctx-body">
        <div className="row between">
          <span className="label">Threads on this doc</span>
        </div>
        <p className="empty-note">No threads on this document yet. Select text in the editor and open a thread.</p>
      </div>
    );
  }

  if (selected) {
    return (
      <div className="tdetail full">
        <div className="tdetail-head">
          <div className="col gap-6 min-0">
            <div className="row gap-6">
              <button className="btn ghost icon sm" type="button" onClick={() => onSelectThread("")} aria-label="Back to thread list">
                <Icon name="back" />
              </button>
              <b>Thread</b>
              <span className="chip sm">{threadReplyLabel(selected)}</span>
              <span className={`thread-badge ${selectedResolved ? "resolved" : "open"}`}>
                <span className="thread-badge-dot" />
                {selectedResolved ? "Resolved" : "Open"}
              </span>
              <span style={{ flex: 1 }} />
              <button className="btn ghost sm" type="button" onClick={toggleStatus} disabled={statusBusy} aria-label={selectedResolved ? "Reopen thread" : "Resolve thread"}>
                {statusBusy ? "…" : selectedResolved ? "Reopen" : "Resolve"}
              </button>
            </div>
            {statusError ? <div className="tiny" style={{ color: "var(--err)" }}>{statusError}</div> : null}
            <div className="quoted-range">
              <div className="tiny mono muted">
                {threadAnchorInfo?.[selected.id]?.orphaned
                  ? "⚠ anchor lost"
                  : selected.anchor.kind === "document" ? "document thread" : "anchored range"}
              </div>
              <span>{selected.anchor.excerpt || selected.title}</span>
              {selected.anchor.kind !== "document" && !threadAnchorInfo?.[selected.id]?.orphaned ? (
                <button className="jump-range-link" type="button" onClick={() => onJumpToThread(selected.id)}>
                  Jump to line {threadAnchorInfo?.[selected.id]?.line ?? ""}
                </button>
              ) : null}
            </div>
            {threadAnchorInfo?.[selected.id]?.orphaned ? (
              <>
                <div className="thread-orphan-warning">Anchor lost · original text deleted</div>
                <div className="thread-orphan-actions">
                  <button className="btn accent sm" type="button" onClick={toggleStatus} disabled={statusBusy}>{statusBusy ? "…" : "Resolve"}</button>
                </div>
              </>
            ) : null}
          </div>
        </div>
        <div className="tdetail-body">
          {selected.messages.map((message) => (
            <article className={`tmsg ${message.authorType === "agent" ? "agent" : ""}`} key={message.id}>
              <div className={`avi sm ${message.authorType === "agent" ? "agent" : "you"}`}>{initials(message.authorHandle || message.authorName || message.authorId)}</div>
              <div className="bubble">
                <div className="row between gap-8">
                  <strong className="small">@{message.authorHandle || message.authorName || message.authorId}</strong>
                  <span className="tiny muted">{shortTime(message.createdAt)}</span>
                </div>
                <p>{message.body}</p>
              </div>
            </article>
          ))}
        </div>
        <form onSubmit={submit} className="tdetail-foot reply-form">
          <textarea value={reply} onChange={(event) => setReply(event.target.value)} placeholder="Reply... use @ to mention humans or agents" />
          <div className="row between">
            <span className="tiny muted"><span className="kbd">⌘↵</span> to post</span>
            <button className="btn accent sm">Reply</button>
          </div>
        </form>
      </div>
    );
  }

  const resolveCard = async (threadId: string, e: React.MouseEvent) => {
    e.stopPropagation();
    try {
      await api.updateThreadStatus(workspaceId, threadId, "resolved");
      onReply();
    } catch (err) {
      setCardError((prev) => ({ ...prev, [threadId]: err instanceof Error ? err.message : "Failed to resolve" }));
    }
  };

  const renderThreadCard = (thread: ThreadItem) => {
    const info = threadAnchorInfo?.[thread.id];
    const isOrphaned = info?.orphaned ?? false;
    const anchorLine = info?.line;
    const isAnchored = thread.anchor.kind !== "document" && !isOrphaned;

    return (
      <button
        key={thread.id}
        className={`titem${thread.id === selectedThreadId ? " selected" : ""}${thread.status === "resolved" ? " resolved" : ""}${isOrphaned ? " orphaned" : ""}`}
        onClick={() => onSelectThread(thread.id)}
      >
        <Icon name="thread" />
        <div className="col gap-4 min-0">
          <div className="between gap-8">
            <span className="chip code sm truncate">{thread.anchor.excerpt || thread.title}</span>
            <span className="tiny muted">{shortTime(thread.updatedAt)}</span>
          </div>
          <div className="row gap-6 min-0">
            <div className={`avi sm ${thread.createdByType === "agent" ? "agent" : "you"}`}>{initials(thread.createdByHandle || thread.createdByName || "You")}</div>
            <span className="small truncate">
              <b>{thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone"}</b>{" "}
              {thread.messages[0]?.body || thread.title}
            </span>
          </div>
          {isOrphaned ? (
            <>
              <div className="thread-orphan-warning">⚠ Anchor lost · original text deleted</div>
              <div className="thread-orphan-actions">
                <span className="thread-resolve-link" role="button" onClick={(e) => resolveCard(thread.id, e)}>Resolve</span>
              </div>
              {cardError[thread.id] ? <div className="tiny" style={{ color: "var(--err)" }}>{cardError[thread.id]}</div> : null}
            </>
          ) : null}
          <div className="row gap-4 tiny muted">
            <span className={`thread-badge ${thread.status === "resolved" ? "resolved" : "open"}`}>
              <span className="thread-badge-dot" />
              {thread.status === "resolved" ? "Resolved" : "Open"}
            </span>
            <span>·</span>
            <span>{threadReplyLabel(thread)}</span>
            {!isOrphaned ? <><span>·</span><span>{thread.anchor.kind === "document" ? "document" : "anchored"}</span></> : null}
            {isAnchored && anchorLine ? (
              <>
                <span>·</span>
                <span
                  className="thread-jump-link"
                  role="link"
                  onClick={(e) => { e.stopPropagation(); onJumpToThread(thread.id); }}
                >
                  Jump to line {anchorLine} →
                </span>
              </>
            ) : null}
          </div>
        </div>
      </button>
    );
  };

  return (
    <div className="ctx-body">
      <div className="row between ctx-head">
        <span className="label">Threads on this doc</span>
      </div>
      <div className="tlist">
        {openThreads.map(renderThreadCard)}
      </div>
      {resolvedThreads.length > 0 ? (
        <div className="thread-fold">
          <button className="thread-fold-header" type="button" onClick={toggleResolvedFold} aria-expanded={!resolvedFolded}>
            <span className="thread-badge resolved"><span className="thread-badge-dot" /></span>
            <span>Resolved · {resolvedThreads.length}</span>
            <span className={`thread-fold-chevron${resolvedFolded ? "" : " expanded"}`}>▾</span>
          </button>
          {!resolvedFolded ? (
            <div className="tlist">
              {resolvedThreads.map(renderThreadCard)}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// Color lands only on the dot; the glyph stays monochrome (currentColor).
const activityDotColor: Record<ActivityCategory, string> = {
  "human-edit": "var(--accent)",
  comment: "var(--iris)",
  "agent-change": "var(--agent)",
  done: "var(--ok)",
  neutral: "var(--ink-3)",
};
const activityGlyph: Record<ActivityCategory, string> = {
  "human-edit": "doc",
  comment: "thread",
  "agent-change": "agent",
  done: "activity",
  neutral: "activity",
};

// Current-document activity, snapshot-fresh (NOT a live stream). Each row shows
// a semantic color dot for its category + a monochrome glyph; relative time is
// derived from the real occurredAt. Membership/summary IDs are resolved to
// handles where we can.
function ActivityPanel({
  activities,
  hasDocument,
  actorLabel,
}: {
  activities: ActivityEvent[];
  hasDocument: boolean;
  actorLabel: Record<string, string>;
}) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  return (
    <div className="ctx-body">
      <div className="row between ctx-head">
        <span className="label">Document Activity</span>
        <span className="chip sm">{activities.length}</span>
      </div>
      {activities.map((activity, index) => {
        const category = activityCategory(activity.type, activity.actorType);
        const label = actorLabel[activity.actorId];
        const text = label ? activity.summary.split(activity.actorId).join(`@${label}`) : activity.summary;
        return (
          <article className="activity-row" key={`${activity.occurredAt}-${activity.actorId}-${index}`}>
            <span className="activity-mark">
              <span className="activity-dot" style={{ background: activityDotColor[category] }} />
              <Icon name={activityGlyph[category]} />
            </span>
            <div className="col gap-2 min-0">
              <div className="small truncate">{text || activity.type}</div>
              <p className="tiny muted">{relativeTime(activity.occurredAt, now)}</p>
            </div>
          </article>
        );
      })}
      {!activities.length ? (
        <p className="empty-note">{hasDocument ? "No activity on this document yet." : "Open a document to see its activity."}</p>
      ) : null}
    </div>
  );
}

function CollaboratorAvatars({ people, onClick }: { people: WorkspacePerson[]; onClick: () => void }) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  // Same honest-decay ruling as the People panel (Eva): the ring reflects personOnline, which
  // decays on the presence freshness window, so a closed laptop drops the ring — never a stale
  // "online". The toolbar only renders confirmed-online collaborators; +N below is neutral.
  const online = people.filter((p) => personOnline(p, now));
  if (!online.length) return null;
  return (
    <button
      className="collaborator-avatars"
      type="button"
      onClick={onClick}
      title={`${online.length} online`}
      aria-label={`${online.length} collaborator${online.length === 1 ? "" : "s"} online — open People`}
    >
      {online.slice(0, 5).map((p) => (
        <div key={p.id} className={`avi sm ${p.kind === "agent" ? "agent" : "you"} online`}>
          {initials(p.handle || p.name)}
        </div>
      ))}
      {/* +N overflow is a neutral count, NOT ringed — a ring would imply all N are online (Eva). */}
      {online.length > 5 ? <span className="avi sm you">+{online.length - 5}</span> : null}
    </button>
  );
}

const personRoleTag = { you: "You", agent: "Agent", member: "Member" } as const;

// ParticipantsPanel (task #4): the document-scoped right tab. It replaces the workspace-wide People panel
// (AlphaToad's full-conversion ruling). Three groups over the open document:
//   Here now — humans + agents with fresh presence on THIS doc (honest decay; presence = real activity only).
//   Watching — agents subscribed to this doc (the durable notification relationship), each removable.
//   Add watcher — the picker of NOT-yet-subscribed agents; the only place an unsubscribed agent appears, so
//                 the panel never redraws the ambient every-agent-watches model.
// Subscribers are fetched from GET /documents/{id}/subscribers on doc change and after each mutation;
// subscribe/unsubscribe reuse the existing agent endpoints (one write path). Mutations are optimistic and
// reconcile against the server on completion.
export function ParticipantsPanel({
  documentId,
  workspace,
  agents,
  api,
  workspaceId,
  onAgent,
}: {
  documentId: string | undefined;
  workspace: Pick<WorkspaceState, "currentUserId" | "agents" | "users" | "presences">;
  agents: Agent[];
  api: ApiClient;
  workspaceId: string;
  onAgent: (agent: Agent) => void;
}) {
  // This panel is KEY'd on workspace+document by its parent, so a scope switch REMOUNTS it: the entire state
  // tree is discarded and old state is unrenderable by construction — no cross-document stale write can reach
  // a different scope's panel (its setState lands on an unmounted instance and is a no-op). That makes a
  // per-switch generation guard unnecessary; what remains is the SAME-scope race within one mount.
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  // null = UNKNOWN (this mount's read has not resolved yet) — distinct from [] (loaded, genuinely none). A
  // fresh mount starts unknown, so a just-switched-to document shows loading, never a false empty panel, and
  // actions stay disabled until its own read establishes server truth.
  const [subscriberIds, setSubscriberIds] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  const [busyIds, setBusyIds] = useState<Set<string>>(() => new Set());
  const [picking, setPicking] = useState(false);
  // readSeqRef bumps at the start of EVERY read, so among reads within this one mount (two concurrent
  // mutations' reconcile fetches) only the latest-STARTED may apply — an older read returning late can't
  // clobber a newer one and drop a change (same-scope out-of-order reconcile).
  const readSeqRef = useRef(0);

  const loadSubscribers = useCallback(async () => {
    if (!documentId) return;
    const seq = ++readSeqRef.current;
    try {
      const response = await api.listDocumentSubscribers(workspaceId, documentId);
      if (readSeqRef.current !== seq) return;
      setSubscriberIds(response.agents.map((agent) => agent.id));
      setError("");
    } catch (err) {
      if (readSeqRef.current !== seq) return;
      setError(err instanceof Error ? err.message : "Failed to load participants");
    }
  }, [api, workspaceId, documentId]);

  useEffect(() => {
    void loadSubscribers();
  }, [loadSubscribers]);

  const loaded = subscriberIds !== null;
  const { hereNow, watching, addable } = useMemo(
    () => documentParticipants(workspace, documentId, subscriberIds ?? [], now),
    [workspace, documentId, subscriberIds, now],
  );
  const agentsById = useMemo(() => new Map(agents.map((agent) => [agent.id, agent])), [agents]);

  const mutate = async (agentId: string, subscribe: boolean) => {
    if (!documentId || !loaded || busyIds.has(agentId)) {
      return;
    }
    setBusyIds((current) => new Set(current).add(agentId));
    // Optimistic: reflect the intent immediately (only meaningful once loaded), reconcile on completion.
    setSubscriberIds((ids) => (ids === null ? ids : subscribe ? Array.from(new Set([...ids, agentId])) : ids.filter((id) => id !== agentId)));
    try {
      if (subscribe) {
        await api.subscribeAgentToDocument(workspaceId, agentId, documentId);
      } else {
        await api.unsubscribeAgentFromDocument(workspaceId, agentId, documentId);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Update failed");
    } finally {
      // Same-scope reconcile; the readSeqRef guard keeps the latest-started read. If the user switched scope
      // mid-mutation this instance is unmounted, so these setState calls are harmless no-ops.
      await loadSubscribers();
      setBusyIds((current) => {
        const next = new Set(current);
        next.delete(agentId);
        return next;
      });
    }
  };

  const personAvatar = (person: WorkspacePerson, online: boolean) => (
    // Honest decay: the online ring is only ever present activity within the freshness window, never a
    // durable/subscribed state — an offline agent that is watching shows no ring.
    <div className={`avi ${person.kind === "agent" ? "agent" : "you"}${online ? " online" : ""}`} title={online ? "Online" : undefined}>
      {initials(person.handle || person.name)}
    </div>
  );

  const hereNowRow = (person: WorkspacePerson) => {
    const agent = person.kind === "agent" ? agentsById.get(person.id) : undefined;
    const body = (
      <div className="col gap-2 min-0">
        <strong className="small truncate">@{person.handle}</strong>
        <span className="tiny muted truncate">{personRoleTag[person.kind]}</span>
        <span className="sr-only">Online</span>
      </div>
    );
    return agent ? (
      <button key={person.id} className="agent-card" onClick={() => onAgent(agent)}>{personAvatar(person, true)}{body}</button>
    ) : (
      <article key={person.id} className="agent-card">{personAvatar(person, true)}{body}</article>
    );
  };

  const watchingRow = (person: WorkspacePerson) => {
    const agent = agentsById.get(person.id);
    const online = personOnline({ presentAt: workspace.presences[person.id]?.documentId === documentId ? workspace.presences[person.id]?.updatedAt : undefined }, now);
    return (
      <div key={person.id} className="agent-card row between">
        <button className="row gap-2 min-0 ghost flat" onClick={() => agent && onAgent(agent)} disabled={!agent}>
          {personAvatar(person, online)}
          <div className="col gap-2 min-0">
            <strong className="small truncate">@{person.handle}</strong>
            <span className="tiny muted truncate">Watching</span>
          </div>
        </button>
        <button className="btn sm ghost" onClick={() => void mutate(person.id, false)} disabled={busyIds.has(person.id)} title="Stop watching this document">
          Remove
        </button>
      </div>
    );
  };

  if (!documentId) {
    return (
      <div className="ctx-body people-pane">
        <p className="empty-note">Open a document to see its participants.</p>
      </div>
    );
  }

  return (
    <div className="ctx-body people-pane">
      <div className="row between ctx-head">
        <span className="label">Participants</span>
        <span className="chip sm">{loaded ? watching.length : "…"}</span>
      </div>
      {error ? <p className="empty-note error">{error}</p> : null}

      {/* Here now is presence-only (known synchronously from workspace state), so it renders even while the
          subscriber read is outstanding. Watching / Add-watcher wait for `loaded` so B never shows A's rows
          or a false empty/all-addable panel mid-switch, and their actions stay disabled while unknown. */}
      <div className="people-section-head"><span className="tiny muted">Here now</span><span className="chip sm">{hereNow.length}</span></div>
      {hereNow.length ? hereNow.map(hereNowRow) : <p className="empty-note">No one is on this document right now.</p>}

      <div className="people-section-head"><span className="tiny muted">Watching</span><span className="chip sm">{loaded ? watching.length : "…"}</span></div>
      {!loaded ? (
        <p className="empty-note">Loading participants…</p>
      ) : watching.length ? (
        watching.map(watchingRow)
      ) : (
        <p className="empty-note">No agents are watching this document yet.</p>
      )}

      <div className="people-section-head">
        <span className="tiny muted">Add watcher</span>
        <button className="btn sm ghost" onClick={() => setPicking((open) => !open)} disabled={!loaded || !addable.length} aria-expanded={picking}>
          {picking ? "Close" : "Add"}
        </button>
      </div>
      {picking && loaded ? (
        addable.length ? (
          <div className="col gap-2 add-watcher-picker">
            {addable.map((person) => (
              <div key={person.id} className="agent-card row between">
                <div className="row gap-2 min-0">
                  {personAvatar(person, false)}
                  <strong className="small truncate">@{person.handle}</strong>
                </div>
                <button className="btn sm" onClick={() => void mutate(person.id, true)} disabled={busyIds.has(person.id)}>
                  Subscribe
                </button>
              </div>
            ))}
          </div>
        ) : (
          <p className="empty-note">Every agent is already watching.</p>
        )
      ) : null}
    </div>
  );
}

function RenameDocumentModal({ document, onClose, onRename, onDelete }: { document: DocumentItem; onClose: () => void; onRename: (path: string) => Promise<void>; onDelete: () => Promise<void> }) {
  const [path, setPath] = useState(document.path);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Modal title="Document options" onClose={onClose}>
      <form
        className="form-stack"
        onSubmit={async (event) => {
          event.preventDefault();
          setBusy(true);
          setError("");
          try {
            await onRename(path);
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <label className="field"><span className="lab">Path</span><input value={path} onChange={(event) => setPath(event.target.value)} required /></label>
        {error ? <p className="error-text">{error}</p> : null}
        <button className="btn accent full" disabled={busy}>Save path</button>
        <button
          className="btn danger full"
          disabled={busy}
          type="button"
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              await onDelete();
            } catch (err) {
              setError(err instanceof Error ? err.message : String(err));
            } finally {
              setBusy(false);
            }
          }}
        >
          Delete document
        </button>
      </form>
    </Modal>
  );
}

export function ShellScriptBlock({ title, badge, command, children }: { title: string; badge?: string; command: string; children?: ReactNode }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    if (!command || typeof navigator === "undefined" || !navigator.clipboard) {
      return;
    }
    await navigator.clipboard.writeText(command);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className="deploy-block">
      <div className="row between">
        <b className="small">{title}</b>
        <div className="row gap-6">
          {badge ? <span className="chip sm">{badge}</span> : null}
          <button type="button" className="btn sm" onClick={copy} disabled={!command}>{copied ? "Copied" : "Copy script"}</button>
        </div>
      </div>
      <pre className="code">{command}</pre>
      {children}
    </div>
  );
}

export function CreateDaemonModal({ api, workspaceId, daemons, onClose, onDone }: { api: ApiClient; workspaceId: string; daemons: Daemon[]; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState("Local environment");
  const [token, setToken] = useState("");
  const [daemonId, setDaemonId] = useState("");
  const command = buildDaemonInstallCommand({
    backendUrl: apiBase,
    workspaceId,
    daemonToken: token || "nottyd_...",
    staticBaseUrl: daemonStaticBase,
  });
  // Track the created daemon in live workspace state so the chip reflects its real
  // check-in instead of a hard-coded "waiting". `daemon.updated` events flow through
  // the workspace socket the moment the daemon calls home, flipping this to online.
  const createdDaemon = daemons.find((daemon) => daemon.id === daemonId);
  const connected = createdDaemon ? daemonStatus(createdDaemon) === "online" : false;
  return (
    <Modal title={token ? `${name} created` : "New local environment"} onClose={onClose}>
      {token ? (
        <div className="token-reveal">
          <ShellScriptBlock title="Install local environment" badge="Host native" command={command}>
            <p className="small muted">This downloads the release bundle, installs the local environment and agent helper, writes local environment config, and starts a local service. Docker Compose is only for local development.</p>
          </ShellScriptBlock>
          <div className="row between">
            {connected ? (
              <span className="chip online"><StatusDot tone="online" />Local environment connected</span>
            ) : (
              <span className="chip"><StatusDot tone="stale" />Waiting for local environment to check in…</span>
            )}
            <button className="btn accent" onClick={onClose}>Done</button>
          </div>
        </div>
      ) : (
        <form
          className="form-stack"
          onSubmit={async (event) => {
            event.preventDefault();
            const response = await api.createDaemon(workspaceId, name);
            setDaemonId(response.daemon.id);
            setToken(response.token);
            onDone();
          }}
        >
          <label className="field"><span className="lab">Name</span><input value={name} onChange={(event) => setName(event.target.value)} required /></label>
          <button className="btn accent full">Create local environment</button>
        </form>
      )}
    </Modal>
  );
}

function isDaemonOffline(daemon: Daemon) {
  const status = daemonStatus(daemon);
  return status === "disconnected" || status === "deleted";
}

// Example agent personas. One is picked at random per modal open to seed the
// name/handle/role placeholders, so users see what each field is for.
const EXAMPLE_PERSONAS: Array<{ name: string; role: string }> = [
  {
    name: "Mira",
    role: "Mira is a sharp editor who tightens loose prose, cuts filler, and keeps your voice intact. She flags sentences that don't quite land and suggests cleaner phrasing instead of rewriting everything herself.",
  },
  {
    name: "Leo",
    role: "Leo is a brainstorming partner for essays, stories, and posts. He asks the questions that get you unstuck, offers angles you hadn't considered, and never lets a blank page win.",
  },
  {
    name: "Priya",
    role: "Priya is a careful researcher who gathers sources, pulls out the key points, and lays them out so you can write with confidence. She keeps a running note of what's solid and what still needs checking.",
  },
  {
    name: "Professor Adler",
    role: "Professor Adler reads for facts and sound logic. He checks claims against what's actually supported, points out leaps in reasoning, and tells you where an argument needs evidence before a careful reader will believe it.",
  },
  {
    name: "Devi",
    role: "Devi is a product manager with a deep feel for user experience. She reads your draft as the person it's meant for, points out where it confuses, drags, or assumes too much, and always asks what the reader is supposed to do next.",
  },
];

function randomPersona() {
  return EXAMPLE_PERSONAS[Math.floor(Math.random() * EXAMPLE_PERSONAS.length)];
}

function CreateAgentModal({ api, workspaceId, daemons, onClose, onDone }: { api: ApiClient; workspaceId: string; daemons: Daemon[]; onClose: () => void; onDone: () => void }) {
  const activeDaemons = daemons.filter((daemon) => daemon.status !== "deleted");
  const [daemonId, setDaemonId] = useState(() => {
    const online = activeDaemons.find((daemon) => daemonStatus(daemon) === "online");
    if (online) {
      return online.id;
    }
    // Offline daemons aren't selectable, so never default onto one — leave it empty.
    return activeDaemons.find((daemon) => !isDaemonOffline(daemon))?.id ?? "";
  });
  const [handle, setHandle] = useState("");
  const [handleEdited, setHandleEdited] = useState(false);
  const [name, setName] = useState("");
  const [role, setRole] = useState("");
  const [example] = useState(randomPersona);
  const namePlaceholder = example.name;
  const handlePlaceholder = identifierFromName(example.name, handleMaxLength);
  const rolePlaceholder = example.role;
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const selectedDaemon = activeDaemons.find((daemon) => daemon.id === daemonId);

  const runtimeTiles = useMemo(() => resolveRuntimeTiles(selectedDaemon), [selectedDaemon]);
  const selectableKinds = useMemo(() => selectableRuntimeKinds(runtimeTiles), [runtimeTiles]);
  const [runtimeKind, setRuntimeKind] = useState("");
  useEffect(() => {
    if (!selectableKinds.includes(runtimeKind)) {
      setRuntimeKind(selectableKinds[0] ?? "");
    }
  }, [selectableKinds, runtimeKind]);

  const selectedRuntime = runtimeTiles.find((tile) => tile.entry.kind === runtimeKind);
  const noReachableDaemon = activeDaemons.length > 0 && activeDaemons.every(isDaemonOffline);
  const [explainKind, setExplainKind] = useState<string | null>(null);
  // Only show the install/help panel while its runtime is still unavailable here.
  const explainTile = runtimeTiles.find(
    (tile) =>
      selectedDaemon &&
      tile.entry.kind === explainKind &&
      (tile.availability === "not_installed" || tile.availability === "update_required"),
  );
  const canCreate = Boolean(
    name.trim() &&
      handle.trim() &&
      role.trim() &&
      selectedDaemon &&
      !isDaemonOffline(selectedDaemon) &&
      runtimeKind &&
      selectableKinds.includes(runtimeKind) &&
      !submitting,
  );

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!canCreate || !selectedDaemon) {
      return;
    }
    setSubmitting(true);
    setError("");
    try {
      await api.createAgent(workspaceId, daemonId, { handle, name, role, kind: runtimeKind });
      onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create agent");
      setSubmitting(false);
    }
  };

  return (
    <Modal title="New agent" onClose={onClose} wide>
      <form className="new-agent-form" onSubmit={submit}>
        <div className="new-agent-grid">
          <div className="col gap-14 new-agent-col">
            <label className="field">
              <span className="lab">Display name</span>
              <input
                value={name}
                placeholder={namePlaceholder}
                onChange={(event) => {
                  const next = event.target.value;
                  setName(next);
                  if (!handleEdited) {
                    setHandle(identifierFromName(next, handleMaxLength));
                  }
                }}
                required
              />
            </label>
            <label className="field">
              <span className="lab">Handle</span>
              <input
                aria-label="Handle"
                value={handle}
                placeholder={handlePlaceholder}
                pattern={identifierPattern}
                minLength={handleMinLength}
                maxLength={handleMaxLength}
                title={identifierHelpText}
                onChange={(event) => {
                  const next = event.target.value;
                  setHandle(next);
                  // Re-enable auto-fill from the display name once the field is cleared.
                  setHandleEdited(next.length > 0);
                }}
                required
              />
              <span className="hint">{identifierHelpText} Auto-filled from the display name; edit to override. Unique in this workspace.</span>
            </label>
            <label className="field"><span className="lab">Role</span><textarea value={role} placeholder={rolePlaceholder} onChange={(event) => setRole(event.target.value)} required /></label>
            <div className="divider" />
            <div className="field">
              <span className="lab">Owning local environment</span>
              <div className="daemon-choice-list">
                {activeDaemons.map((daemon) => {
                  const status = daemonStatus(daemon);
                  const offline = isDaemonOffline(daemon);
                  const runtimeCount = (daemon.runtimes ?? []).filter((runtime) => runtime.available).length;
                  return (
                    <label className={`daemon-choice ${daemonId === daemon.id ? "selected" : ""} ${offline ? "disabled" : ""}`} key={daemon.id}>
                      <input
                        type="radio"
                        name="owning-daemon"
                        checked={daemonId === daemon.id}
                        disabled={offline}
                        onChange={() => setDaemonId(daemon.id)}
                      />
                      <div className="avi sm daemon">{initials(daemon.name)}</div>
                      <div className="col gap-0 min-0">
                        <b className="small truncate">{daemon.name}</b>
                        <span className="tiny muted truncate">{runtimeCount} runtime{runtimeCount === 1 ? "" : "s"}</span>
                      </div>
                      <span className={`chip sm ${status}`}><StatusDot tone={status} />{status}</span>
                    </label>
                  );
                })}
                {!activeDaemons.length ? <p className="small muted">Create a local environment before adding agents.</p> : null}
              </div>
              {noReachableDaemon ? (
                <span className="hint err-text">Every local environment is offline. Bring one online to host a new agent.</span>
              ) : null}
            </div>
          </div>

          <div className="col gap-14 new-agent-col">
            <div className="field">
              <div className="between">
                <span className="lab">Runtime</span>
                {selectedDaemon ? <span className="tiny muted truncate">detected on {selectedDaemon.name}</span> : null}
              </div>
              <div className="runtime-grid">
                {runtimeTiles.map((tile) => (
                  <RuntimeOption
                    key={tile.entry.kind}
                    tile={tile}
                    selected={tile.entry.kind === runtimeKind}
                    daemonSelected={Boolean(selectedDaemon)}
                    onSelect={() => setRuntimeKind(tile.entry.kind)}
                    onExplain={() => setExplainKind((current) => (current === tile.entry.kind ? null : tile.entry.kind))}
                  />
                ))}
              </div>
              {explainTile ? (
                <div className="rt-help">
                  <div className="between">
                    <b className="small">{explainTile.entry.label} isn’t available here</b>
                    <button type="button" className="btn ghost icon sm" onClick={() => setExplainKind(null)} aria-label="Close">×</button>
                  </div>
                  <p className="tiny muted">
                    {explainTile.availability === "update_required"
                      ? `The Codex CLI on ${selectedDaemon?.name ?? "this local environment"}’s host is below the version Codesk supports (${explainTile.meta}).`
                      : `Codex isn’t installed on ${selectedDaemon?.name ?? "this local environment"}’s host.`}{" "}
                    Install it on that machine, then reconnect the local environment so Codesk can re-scan its runtimes.
                  </p>
                  <div className="rt-help-cmd"><code>curl -fsSL https://chatgpt.com/codex/install.sh | sh</code></div>
                  <p className="tiny muted">Codex needs an active ChatGPT subscription or API (usage-based) billing to run.</p>
                </div>
              ) : selectedDaemon && selectableKinds.length === 0 ? (
                <span className="hint err-text">No supported runtime is available on this local environment. Codesk currently supports Codex — install the Codex CLI on this host, then re-scan.</span>
              ) : null}
            </div>
            <p className="small muted">Model and reasoning effort are taken from the runtime. System prompts are generated by the backend shared prompt and are not user-editable.</p>
          </div>
        </div>

        {error ? <p className="form-error">{error}</p> : null}

        <div className="new-agent-foot">
          <div className="small muted row gap-6 wrap binding-summary">
            <StatusDot tone="daemon" />
            Runs on <b>{selectedDaemon?.name ?? "—"}</b>
            <span className="faint">·</span>
            <span>{selectedRuntime ? selectedRuntime.entry.label : "No runtime selected"}</span>
          </div>
          <div className="row gap-6">
            <button type="button" className="btn" onClick={onClose}>Cancel</button>
            <button type="submit" className="btn accent" disabled={!canCreate}>Create agent</button>
          </div>
        </div>
      </form>
    </Modal>
  );
}

export function RuntimeOption({ tile, selected, daemonSelected, onSelect, onExplain }: { tile: RuntimeTile; selected: boolean; daemonSelected: boolean; onSelect: () => void; onExplain: () => void }) {
  const { entry, availability, meta } = tile;
  const tileClass = `rt-tile${entry.tile ? ` ${entry.tile}` : ""}`;

  if (availability === "coming_soon") {
    return (
      <div className="rt soon" aria-disabled="true">
        <div className="rt-top">
          <div className={tileClass}>{entry.monogram}</div>
          <span className="rt-soon"><ClockIcon />Soon</span>
        </div>
        <div className="rt-name">{entry.label}</div>
        <div className="rt-meta">{meta}</div>
      </div>
    );
  }

  if (availability !== "available") {
    return (
      <div className="rt off" aria-disabled="true">
        <div className="rt-top">
          <div className={tileClass}>{entry.monogram}</div>
          {daemonSelected ? (
            <button
              type="button"
              className="rt-help-btn"
              onClick={onExplain}
              aria-label={`Why is ${entry.label} unavailable, and how to install it`}
              title="Why isn't this available?"
            >
              <HelpIcon />
            </button>
          ) : null}
        </div>
        <div className="rt-name">{entry.label}</div>
        <div className={`rt-meta${availability === "update_required" ? " warn-text" : ""}`}>
          {daemonSelected ? meta : "Select a local environment to check"}
        </div>
      </div>
    );
  }

  return (
    <label className={`rt ${selected ? "sel" : ""}`}>
      <input type="radio" name="runtime" checked={selected} onChange={onSelect} hidden />
      <div className="rt-top">
        <div className={tileClass}>{entry.monogram}</div>
        <span className="rt-check"><CheckIcon /></span>
      </div>
      <div className="rt-name">{entry.label}</div>
      <div className="rt-meta">{meta}</div>
    </label>
  );
}

function CheckIcon() {
  return <svg className="i" viewBox="0 0 24 24"><path d="M5 12l5 5L20 7" /></svg>;
}

function HelpIcon() {
  return (
    <svg className="i" viewBox="0 0 24 24">
      <circle cx="12" cy="12" r="9" />
      <path d="M9.6 9.2a2.5 2.5 0 1 1 3.6 2.4c-.8.5-1.2 1-1.2 1.9" />
      <path d="M12 17h.01" />
    </svg>
  );
}

function ClockIcon() {
  return <svg className="i" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 8v4l3 2" /></svg>;
}

export function AgentDetailModal({ api, workspaceId, agentId, agents, daemons, runs, agentEvents, onClose, onChanged }: { api: ApiClient; workspaceId: string; agentId: string; agents: Agent[]; daemons: Daemon[]; runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"]; agentEvents: AgentEvent[]; onClose: () => void; onChanged: () => void }) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  const [prompt, setPrompt] = useState("Review the current workspace and respond if you have useful feedback.");
  // Derive the live agent from workspace state every render, so agent.updated / agent.run.updated
  // events reach the open modal instead of a frozen snapshot captured at click time. A deleted agent
  // is removed from the array by the reducer, so it drops out here and the effect closes the modal.
  const agent = agents.find((item) => item.id === agentId);
  useEffect(() => {
    if (!agent) {
      onClose();
    }
  }, [agent, onClose]);
  if (!agent) {
    return null;
  }
  const daemon = daemons.find((item) => item.id === agent.daemonId);
  const status = visibleAgentStatus(agent, runs, daemons, agentEvents, now);
  const statusLabel = detailStatusLabel(status);
  return (
    <Modal title={`@${agent.handle}`} onClose={onClose}>
      <div className="form-stack">
        <div className="modal-identity">
          <div className="avi agent">{initials(agent.handle)}</div>
          <div className="col gap-2">
            <span className={`chip sm ${status.tone}`} title={status.title}><StatusDot tone={status.tone} /> {statusLabel}</span>
            <span className="small muted">Local environment: {daemon?.name ?? agent.daemonId}</span>
          </div>
        </div>
        {status.key === "failed" && status.reason ? <p className="error-text">Failure reason: {status.reason}</p> : null}
        <p className="small"><strong>Role:</strong> {agent.role}</p>
        <label className="field"><span className="lab">One-off instruction</span><textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} /></label>
        <button className="btn accent full" onClick={async () => { await api.startAgent(workspaceId, agent.id, prompt); onChanged(); }}>Start run</button>
        <button className="btn danger full" onClick={async () => { await api.deleteAgent(workspaceId, agent.id); onChanged(); onClose(); }}>Delete agent</button>
      </div>
    </Modal>
  );
}

export function DaemonDetailModal({ api, workspaceId, daemonId, daemons, agents, runs, agentEvents, onClose, onChanged }: { api: ApiClient; workspaceId: string; daemonId: string; daemons: Daemon[]; agents: Agent[]; runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"]; agentEvents: AgentEvent[]; onClose: () => void; onChanged: () => void }) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  const [reinstallOpen, setReinstallOpen] = useState(false);
  const [reinstallToken, setReinstallToken] = useState("");
  const [reinstallError, setReinstallError] = useState("");
  const [reinstallLoading, setReinstallLoading] = useState(false);
  // Derive the live daemon from workspace state every render, so daemon.updated events and the
  // liveness tick reach the open modal instead of a frozen snapshot captured at click time. Exclude
  // soft-deleted rows: a daemon.deleted event upserts the daemon with status "deleted" (it stays in
  // the array), so matching it would render a dead modal instead of closing.
  const daemon = daemons.find((item) => item.id === daemonId && item.status !== "deleted");
  useEffect(() => {
    // Daemon deleted while the modal is open — nothing to show, close it.
    if (!daemon) {
      onClose();
    }
  }, [daemon, onClose]);
  if (!daemon) {
    return null;
  }
  const status = daemonLiveStatus(daemon, now);
  const daemonAgents = agents.filter((agent) => agent.daemonId === daemon.id);
  const prepareReinstall = async () => {
    setReinstallOpen(true);
    setReinstallToken("");
    setReinstallError("");
    setReinstallLoading(true);
    try {
      const response = await api.createDaemonReinstallToken(workspaceId, daemon.id);
      setReinstallToken(response.token);
    } catch (error) {
      setReinstallError(error instanceof Error ? error.message : "Could not prepare reinstall script");
    } finally {
      setReinstallLoading(false);
    }
  };
  const reinstallCommand = reinstallToken ? buildDaemonReinstallCommand({
    backendUrl: apiBase,
    workspaceId,
    daemonToken: reinstallToken,
    staticBaseUrl: daemonStaticBase,
  }) : "";
  const uninstallCommand = buildDaemonUninstallCommand({
    staticBaseUrl: daemonStaticBase,
  });
  return (
    <>
      <Modal title={daemon.name} onClose={onClose}>
        <div className="form-stack">
          <div className="deploy-block">
            <div className="row between">
              <b className="small">Status</b>
              <span className={`chip sm ${status}`}><StatusDot tone={status} />{status}</span>
            </div>
            <p className="tiny muted mono">ID: {daemon.id}</p>
            <p className="small muted">Last seen: {daemon.lastSeenAt ? new Date(daemon.lastSeenAt).toLocaleString() : "Never"}</p>
            <p className="small muted">Agents: {daemonAgents.length}</p>
            {daemonAgents.map((agent) => {
              const agentStatus = visibleAgentStatus(agent, runs, [daemon], agentEvents, now);
              return <p className="small" key={agent.id}>@{agent.handle} · {detailStatusLabel(agentStatus)}</p>;
            })}
          </div>
          <button className="btn accent full" onClick={() => void prepareReinstall()} disabled={reinstallLoading}>Reinstall local environment</button>
          <ShellScriptBlock title="Uninstall local environment" badge="Global" command={uninstallCommand}>
            <p className="small muted">Run this on the local environment host to remove the local Codesk installation. This uses the global uninstall script because workspace-specific uninstall is not supported yet.</p>
          </ShellScriptBlock>
          <button className="btn danger full" onClick={async () => { await api.deleteDaemon(workspaceId, daemon.id); onChanged(); onClose(); }}>Delete local environment record</button>
        </div>
      </Modal>
      {reinstallOpen ? (
        <Modal title="Reinstall local environment" onClose={() => setReinstallOpen(false)}>
          <div className="form-stack">
            {reinstallLoading ? (
              <div className="deploy-block">
                <div className="row between">
                  <b className="small">Reinstall script</b>
                  <span className="chip sm">Preparing</span>
                </div>
                <p className="small muted">Preparing a fresh local environment token and reinstall script.</p>
              </div>
            ) : reinstallError ? (
              <div className="deploy-block">
                <div className="row between">
                  <b className="small">Reinstall script</b>
                  <span className="chip sm danger">Unavailable</span>
                </div>
                <p className="small muted">{reinstallError}</p>
              </div>
            ) : (
              <ShellScriptBlock title="Reinstall local environment" badge="Host native" command={reinstallCommand}>
                <p className="small muted">Run this on the local environment host to remove the current local install and install the latest local environment again using the fresh token below.</p>
              </ShellScriptBlock>
            )}
          </div>
        </Modal>
      ) : null}
    </>
  );
}

export function EmptyWorkspace({
  onCreateDocument,
  onCreateDaemon,
  creatingDocument,
  canCreateDocument,
}: {
  onCreateDocument: () => void;
  onCreateDaemon: () => void;
  creatingDocument: boolean;
  canCreateDocument: boolean;
}) {
  return (
    <section className="doc-canvas">
      <div className="doc-inner empty-state">
        <h2 className="display">Let's get this workspace working.</h2>
        <p className="small muted">Codesk is best with at least one local environment: it syncs docs to disk and hosts your agents. You can also start by writing something.</p>
        <div className="empty-grid">
          <button className="card p-20 empty-choice" onClick={onCreateDaemon}>
            <div className="row gap-8"><div className="avi sm daemon">D</div><b>Deploy a local environment</b></div>
            <span className="small muted">Bring docs to local disk and enable agents.</span>
          </button>
          <button className="card p-20 empty-choice" onClick={onCreateDocument} disabled={!canCreateDocument || creatingDocument}>
            <div className="row gap-8"><Icon name="doc" /><b>Create your first doc</b></div>
            <span className="small muted">{creatingDocument ? "Creating..." : "Markdown or plaintext. Threads and agents come along."}</span>
          </button>
        </div>
      </div>
    </section>
  );
}

export function DocumentNotFound({ onBackToWorkspace }: { onBackToWorkspace: () => void }) {
  return (
    <section className="doc-canvas">
      <div className="doc-inner empty-state">
        <h2 className="display">Document not found</h2>
        <p className="small muted">This document is not available in the current workspace.</p>
        <button className="btn accent lg" type="button" onClick={onBackToWorkspace}>
          Back to workspace
        </button>
      </div>
    </section>
  );
}

export type ManageTab = "members" | "agents" | "local-env" | "workspace" | "danger";

export const MANAGE_TABS: { id: ManageTab; label: string; danger?: boolean }[] = [
  { id: "members", label: "Members & Invite" },
  { id: "agents", label: "Agents" },
  { id: "local-env", label: "Local environment" },
  { id: "workspace", label: "Workspace settings" },
  { id: "danger", label: "Danger zone", danger: true },
];

// Members & Invite tab (plan §4.2): the workspace-wide member list plus invite-link
// generation, migrated out of the toolbar's Share modal into Manage. The right-rail
// People panel is unchanged (#19 governs its display scope, not where management lives).
export function MembersAndInvite({
  api,
  workspaceId,
  users,
  canInvite,
}: {
  api: ApiClient;
  workspaceId: string;
  users: UserItem[];
  canInvite: boolean;
}) {
  const [link, setLink] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [inviteError, setInviteError] = useState("");
  const [creating, setCreating] = useState(false);
  const [copied, setCopied] = useState(false);

  const generate = async () => {
    setCreating(true);
    setInviteError("");
    setCopied(false);
    try {
      const response = await api.createWorkspaceInvite(workspaceId);
      setLink(new URL(response.url, publicOrigin).toString());
      setExpiresAt(response.invite.expiresAt);
    } catch (err) {
      setInviteError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  };

  const copy = async () => {
    if (!link) {
      return;
    }
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
    } catch (err) {
      setInviteError(err instanceof Error ? err.message : String(err));
    }
  };

  const members = users.filter((user) => user.kind !== "agent");

  return (
    <div className="manage-panel">
      <div className="manage-panel-head">
        <span className="display management-title">Members &amp; Invite</span>
        <span className="chip">{members.length} {members.length === 1 ? "member" : "members"}</span>
      </div>

      <div className="member-list">
        {members.map((user) => (
          <div className="member-row" key={user.id}>
            <div className="avi sm you">{initials(user.handle || user.name)}</div>
            <div className="col gap-0 min-0">
              <b className="small truncate">{user.name || `@${user.handle}`}</b>
              <span className="tiny muted truncate">@{user.handle}</span>
            </div>
            <span className="chip sm member-role">{user.role || "Member"}</span>
          </div>
        ))}
        {!members.length ? <p className="tiny muted">No members yet.</p> : null}
      </div>

      <div className="invite-block">
        <div className="lab"><span className="label">Invite by link</span></div>
        {canInvite ? (
          <>
            {inviteError ? <p className="error-text">{inviteError}</p> : null}
            {link ? (
              <>
                <input className="input" readOnly value={link} onFocus={(event) => event.currentTarget.select()} />
                {expiresAt ? <p className="tiny muted">Expires {formatInviteDate(expiresAt)}</p> : null}
                <div className="row gap-8">
                  <button className="btn accent" type="button" onClick={() => void copy()}>{copied ? "Copied" : "Copy link"}</button>
                  <button className="btn ghost" type="button" onClick={() => void generate()} disabled={creating}>Regenerate</button>
                </div>
              </>
            ) : (
              <button className="btn accent" type="button" onClick={() => void generate()} disabled={creating}>
                {creating ? "Generating…" : "Generate invite link"}
              </button>
            )}
          </>
        ) : (
          <p className="tiny muted">Only workspace owners and admins can invite new members.</p>
        )}
      </div>
    </div>
  );
}

const workspaceRuntimeOptions = [
  { value: "", label: "No default" },
  { value: "codex", label: "Codex" },
  { value: "claude", label: "Claude" },
];

// Workspace settings (plan §4.2). The backend supports partial PATCH: owners/admins can edit
// name/default runtime, while slug is owner-only because changing it invalidates old URLs.
function WorkspaceSettings({
  api,
  workspaceId,
  workspace,
  workspaceSlug,
  onSaved,
}: {
  api: ApiClient;
  workspaceId: string;
  workspace: ReturnType<typeof useWorkspace>["workspace"];
  workspaceSlug: string;
  onSaved: (workspace: WorkspaceSummary) => void;
}) {
  const currentSlug = workspace.slug || workspaceSlug;
  const currentDefaultRuntime = workspace.defaultRuntime ?? "";
  const role = workspace.currentMembershipRole || "";
  const canManage = role === "owner" || role === "admin";
  const canEditSlug = role === "owner";
  const [nameDraft, setNameDraft] = useState(workspace.name);
  const [slugDraft, setSlugDraft] = useState(currentSlug);
  const [runtimeDraft, setRuntimeDraft] = useState(currentDefaultRuntime);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    setNameDraft(workspace.name);
    setSlugDraft(currentSlug);
    setRuntimeDraft(currentDefaultRuntime);
    setMessage("");
    setError("");
  }, [currentDefaultRuntime, currentSlug, workspace.name]);

  const trimmedName = nameDraft.trim();
  const trimmedSlug = slugDraft.trim();
  const slugPattern = new RegExp(`^${identifierPattern}$`);
  const nameChanged = trimmedName !== workspace.name.trim();
  const slugChanged = trimmedSlug !== currentSlug;
  const runtimeChanged = runtimeDraft !== currentDefaultRuntime;
  const hasEditableChange =
    (canManage && nameChanged) ||
    (canEditSlug && slugChanged) ||
    (canManage && runtimeChanged);
  const slugValid =
    trimmedSlug.length >= workspaceSlugMinLength &&
    trimmedSlug.length <= workspaceSlugMaxLength &&
    slugPattern.test(trimmedSlug);
  const saveDisabled =
    saving ||
    !canManage ||
    !hasEditableChange ||
    !trimmedName ||
    (canEditSlug && slugChanged && !slugValid);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (saveDisabled) {
      return;
    }
    const input: UpdateWorkspaceSettingsInput = {};
    if (canManage && nameChanged) {
      input.name = trimmedName;
    }
    if (canEditSlug && slugChanged) {
      input.slug = trimmedSlug;
    }
    if (canManage && runtimeChanged) {
      input.defaultRuntime = runtimeDraft;
    }
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const response = await api.updateWorkspaceSettings(workspaceId, input);
      onSaved(response.workspace);
      setNameDraft(response.workspace.name);
      setSlugDraft(response.workspace.slug);
      setRuntimeDraft(response.workspace.defaultRuntime ?? "");
      setMessage("Workspace settings saved.");
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setError("Slug taken. Choose another workspace URL.");
      } else {
        setError(err instanceof Error ? err.message : String(err));
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="manage-panel">
      <div className="manage-panel-head">
        <span className="display management-title">Workspace settings</span>
      </div>
      <form className="settings-list" onSubmit={(event) => void save(event)}>
        <label className="field">
          <span className="lab">Workspace name</span>
          <input
            value={nameDraft}
            onChange={(event) => {
              setNameDraft(event.target.value);
              setMessage("");
              setError("");
            }}
            disabled={!canManage || saving}
          />
        </label>
        <label className="field">
          <span className="lab">Workspace URL slug</span>
          <input
            value={slugDraft}
            onChange={(event) => {
              setSlugDraft(event.target.value.trim().toLowerCase());
              setMessage("");
              setError("");
            }}
            disabled={!canEditSlug || saving}
            aria-describedby="workspace-slug-help"
          />
        </label>
        <p id="workspace-slug-help" className="tiny muted">
          {canEditSlug
            ? `${identifierHelpText} The public URL is ${publicOrigin}/w/${trimmedSlug || currentSlug}.`
            : "Only the workspace owner can change the URL slug."}
        </p>
        <label className="field">
          <span className="lab">Default agent runtime</span>
          <select
            value={runtimeDraft}
            onChange={(event) => {
              setRuntimeDraft(event.target.value);
              setMessage("");
              setError("");
            }}
            disabled={!canManage || saving}
          >
            {workspaceRuntimeOptions.map((option) => (
              <option value={option.value} key={option.value}>{option.label}</option>
            ))}
          </select>
        </label>
        <p className="tiny muted">
          {canManage
            ? "Owners and admins can update the name and default runtime. Slug changes are owner-only."
            : "Only workspace owners and admins can edit workspace settings."}
        </p>
        {canEditSlug && slugChanged && !slugValid ? <p className="error-text">{identifierHelpText}</p> : null}
        {error ? <p className="error-text">{error}</p> : null}
        {message ? <p className="success-text">{message}</p> : null}
        <button className="btn accent" type="submit" disabled={saveDisabled}>
          {saving ? "Saving..." : "Save settings"}
        </button>
      </form>
    </div>
  );
}

// Danger zone (plan §4.2). Destructive deletion is owner-only and server-enforced by an exact
// confirmName match. After DELETE succeeds, the UI waits for workspace.deleted before navigating.
function DangerZone({
  api,
  workspaceId,
  workspace,
}: {
  api: ApiClient;
  workspaceId: string;
  workspace: ReturnType<typeof useWorkspace>["workspace"];
}) {
  const [confirmName, setConfirmName] = useState("");
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const canDelete = workspace.currentMembershipRole === "owner";
  const matchesName = confirmName === workspace.name;

  const deleteWorkspace = async (event: FormEvent) => {
    event.preventDefault();
    if (!canDelete || !matchesName || deleting) {
      return;
    }
    setDeleting(true);
    setError("");
    setMessage("");
    try {
      await api.deleteWorkspace(workspaceId, confirmName);
      setMessage("Deletion requested. Waiting for workspace removal...");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  };

  return (
    <div className="manage-panel">
      <div className="manage-panel-head">
        <span className="display management-title">Danger zone</span>
      </div>
      <form className="danger-note" onSubmit={(event) => void deleteWorkspace(event)}>
        <p className="small">Delete this workspace permanently.</p>
        <p className="tiny muted">
          Deleting a workspace permanently removes its documents, agents and members. Type the exact
          workspace name to confirm.
        </p>
        <label className="field">
          <span className="lab">Type {workspace.name}</span>
          <input
            value={confirmName}
            onChange={(event) => {
              setConfirmName(event.target.value);
              setError("");
              setMessage("");
            }}
            disabled={!canDelete || deleting}
            autoComplete="off"
          />
        </label>
        {!canDelete ? <p className="tiny muted">Only the workspace owner can delete this workspace.</p> : null}
        {canDelete && confirmName && !matchesName ? <p className="error-text">Workspace name must match exactly.</p> : null}
        {error ? <p className="error-text">{error}</p> : null}
        {message ? <p className="success-text">{message}</p> : null}
        <button className="btn danger" type="submit" disabled={!canDelete || !matchesName || deleting}>
          {deleting ? "Deleting..." : "Delete workspace"}
        </button>
      </form>
    </div>
  );
}

// Low-frequency workspace management, pulled out of the toolbar and right rail into a single
// container (plan §4.2). Local environment holds what used to be the "Daemons" center view.
export function ManageModal({
  api,
  workspaceId,
  workspace,
  workspaceSlug,
  activeTab,
  canInvite,
  groupedAgents,
  onTabChange,
  onClose,
  onRefresh,
  onNewDaemon,
  onDaemon,
  onNewAgent,
  onAgent,
  onWorkspaceSaved = () => {},
}: {
  api: ApiClient;
  workspaceId: string;
  workspace: ReturnType<typeof useWorkspace>["workspace"];
  workspaceSlug: string;
  activeTab: ManageTab;
  canInvite: boolean;
  groupedAgents: Array<{ daemonId: string; daemonName: string; agents: Agent[] }>;
  onTabChange: (tab: ManageTab) => void;
  onClose: () => void;
  onRefresh: () => void;
  onNewDaemon: () => void;
  onDaemon: (daemon: Daemon) => void;
  onNewAgent: () => void;
  onAgent: (agent: Agent) => void;
  onWorkspaceSaved?: (workspace: WorkspaceSummary) => void;
}) {
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <section className="modal-card card lifted wide manage-modal" onClick={(event) => event.stopPropagation()} role="dialog" aria-modal="true" aria-label="Manage workspace">
        <header className="modal-header">
          <h2 className="modal-title">Manage workspace</h2>
          <button className="btn ghost icon sm" onClick={onClose} aria-label="Close">×</button>
        </header>
        <div className="manage-body">
          <nav className="manage-tabs" aria-label="Manage sections">
            {MANAGE_TABS.filter((tab) => !tab.danger).map((tab) => (
              <button
                key={tab.id}
                type="button"
                className={`manage-tab ${activeTab === tab.id ? "active" : ""}`}
                aria-current={activeTab === tab.id}
                onClick={() => onTabChange(tab.id)}
              >
                {tab.label}
              </button>
            ))}
            <span className="manage-tabs-spacer" />
            {MANAGE_TABS.filter((tab) => tab.danger).map((tab) => (
              <button
                key={tab.id}
                type="button"
                className={`manage-tab danger ${activeTab === tab.id ? "active" : ""}`}
                aria-current={activeTab === tab.id}
                onClick={() => onTabChange(tab.id)}
              >
                {tab.label}
              </button>
            ))}
          </nav>
          <div className="manage-content">
            {activeTab === "local-env" ? (
              <DaemonsManagement workspace={workspace} onRefresh={onRefresh} onNew={onNewDaemon} onDaemon={onDaemon} />
            ) : activeTab === "members" ? (
              <MembersAndInvite api={api} workspaceId={workspaceId} users={workspace.users} canInvite={canInvite} />
            ) : activeTab === "agents" ? (
              <AgentsManagement workspace={workspace} groupedAgents={groupedAgents} onNew={onNewAgent} onAgent={onAgent} />
            ) : activeTab === "workspace" ? (
              <WorkspaceSettings
                api={api}
                workspaceId={workspaceId}
                workspace={workspace}
                workspaceSlug={workspaceSlug}
                onSaved={onWorkspaceSaved}
              />
            ) : (
              <DangerZone api={api} workspaceId={workspaceId} workspace={workspace} />
            )}
          </div>
        </div>
      </section>
    </div>
  );
}

export function Modal({ title, children, onClose, wide }: { title: string; children: ReactNode; onClose: () => void; wide?: boolean }) {
  return (
    <div className="modal-backdrop">
      <section className={`modal-card card lifted${wide ? " wide" : ""}`}>
        <header className="modal-header">
          <h2 className="modal-title">{title}</h2>
          <button className="btn ghost icon sm" onClick={onClose} aria-label="Close">×</button>
        </header>
        {children}
      </section>
    </div>
  );
}

export function StatusDot({ tone }: { tone: string }) {
  return <span className={`status-dot ${tone}`} />;
}

export function Logo() {
  return (
    <div className="row gap-8 logo-row">
      <svg className="logo-mark" viewBox="14 31 72 38" aria-hidden="true">
        <circle cx="24" cy="50" r="9" fill="var(--accent)" />
        <circle cx="76" cy="50" r="9" fill="var(--agent)" />
        <line x1="39.6" y1="39.6" x2="60.4" y2="60.4" stroke="currentColor" strokeWidth="7.8" strokeLinecap="round" />
        <line x1="60.4" y1="39.6" x2="39.6" y2="60.4" stroke="currentColor" strokeWidth="7.8" strokeLinecap="round" />
      </svg>
      <span className="display logo-type">codesk</span>
    </div>
  );
}

export function Icon({ name }: { name: string }) {
  switch (name) {
    case "home":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M3 12l9-9 9 9M5 10v10h14V10" /></svg>;
    case "back":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M15 18l-6-6 6-6" /></svg>;
    case "activity":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></svg>;
    case "inbox":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M4 4h16l-2 10h-3l-1.5 3h-3L9 14H6L4 4z" /><path d="M4 20h16" /></svg>;
    case "mention":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="4" /><path d="M16 8v5a3 3 0 0 0 6 0v-1a10 10 0 1 0-4 8" /></svg>;
    case "review":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 3a9 9 0 0 1 0 18z" /></svg>;
    case "alert":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M12 3l10 18H2L12 3z" /><path d="M12 9v5M12 18h.01" /></svg>;
    case "thread":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5A8.5 8.5 0 1 1 21 11.5z" /></svg>;
    case "message":
      return <svg className="i sm" viewBox="0 0 16 16"><path d="M3 3h10a1.6 1.6 0 0 1 1.6 1.6v5A1.6 1.6 0 0 1 13 11.2H6.6L4 13.4v-2.2H3a1.6 1.6 0 0 1-1.6-1.6v-5A1.6 1.6 0 0 1 3 3Z" /></svg>;
    case "people":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" /><circle cx="9" cy="7" r="4" /></svg>;
    case "search":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4-4" /></svg>;
    case "plus":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M12 5v14M5 12h14" /></svg>;
    case "refresh":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-3-6.7L21 3" /><path d="M21 3v6h-6" /></svg>;
    case "stack":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M3 7l9-4 9 4-9 4-9-4zm0 6l9 4 9-4M3 17l9 4 9-4" /></svg>;
    case "doc":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" /><path d="M14 3v5h5" /></svg>;
    case "chevron":
      return <svg className="i sm muted" viewBox="0 0 24 24"><path d="M9 6l6 6-6 6" /></svg>;
    case "caret":
      return <svg className="i sm caret-icon" viewBox="0 0 24 24"><path d="M9 6l6 6-6 6" /></svg>;
    case "daemon":
      return <svg className="i sm" viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="6" rx="1.5" /><rect x="3" y="14" width="18" height="6" rx="1.5" /><circle cx="7" cy="7" r="0.6" /><circle cx="7" cy="17" r="0.6" /></svg>;
    case "agent":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M12 3l7 4v6c0 4-3 7-7 8-4-1-7-4-7-8V7l7-4z" /><path d="M9 12h6M9 16h6" /></svg>;
    case "share":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="18" cy="5" r="3" /><circle cx="6" cy="12" r="3" /><circle cx="18" cy="19" r="3" /><path d="M8.6 10.6l6.8-4.2M8.6 13.4l6.8 4.2" /></svg>;
    case "more":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="1.5" /><circle cx="19" cy="12" r="1.5" /><circle cx="5" cy="12" r="1.5" /></svg>;
    case "settings":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M4.2 4.2l2.1 2.1M17.7 17.7l2.1 2.1M2 12h3M19 12h3M4.2 19.8l2.1-2.1M17.7 6.3l2.1-2.1" /></svg>;
    default:
      return null;
  }
}
