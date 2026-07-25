import { FormEvent, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, ReactNode, RefObject } from "react";
import { ApiClient, ApiError, apiBase, daemonStaticBase, desktopStaticBase, publicOrigin, type UpdateWorkspaceSettingsInput } from "./api";
import { DocumentSurface, type LiveThread, type SurfaceSelection } from "./DocumentSurface";
import type { MarkdownPreviewCommandName } from "./markdownLivePreview";
import {
  agentDisplayStatus,
  agentsByDaemon,
  buildDaemonInstallCommand,
  buildDaemonReinstallCommand,
  buildDaemonUninstallCommand,
  defaultDaemonInstallPlatform,
  workspacePeople,
  documentParticipants,
  clampPopoverPosition,
  personOnline,
  documentActivity,
  activityCategory,
  relativeTime,
  daemonStatus,
  hasGenuineCheckIn,
  daemonLiveStatus,
  handleMaxLength,
  handleMinLength,
  identifierFromName,
  identifierHelpText,
  identifierPattern,
  isMarkdownDocumentPath,
  detectDesktopPlatform,
  desktopPlatformHasApp,
  daemonDesktopPlatform,
  daemonDownloadTarget,
  resolveDesktopManifest,
  desktopPlatformInstallTarget,
  desktopDownloadTargets,
  defaultDesktopDownloadTarget,
  randomWorkspaceName,
  threadReplyLabel,
  workspaceSlugMaxLength,
  workspaceSlugMinLength,
  type ActivityCategory,
  type DaemonInstallPlatform,
  type DesktopPlatform,
  type DesktopDownloadTarget,
  type WorkspacePerson,
  type LineThreadGroup,
  resolveThreadAnchorLive,
} from "./logic";
import { resolveRoot, resolveWorkspace, type WorkspaceView } from "./routes";
import { navigate, useRoute } from "./useRoute";
import { useRootNamespace } from "./useRootNamespace";
import { useDocumentSync } from "./useDocument";
import { useWorkspace } from "./useWorkspace";
import { Onboarding, type OnboardingActionEvent, type OnboardingPresentation } from "./Onboarding";
import { OnboardingChecklist } from "./OnboardingChecklist";
import { useOnboardingController } from "./onboardingController";
import type { OnboardingRole } from "./onboardingEngine";
import type { Account, ActivityEvent, Agent, Daemon, DocumentItem, ThreadItem, UserItem, WorkspaceInvitePreview, WorkspaceState, WorkspaceSummary } from "./types";
import { resolveRuntimeTiles, selectableRuntimeKinds, type RuntimeTile } from "./runtimes";
import "./styles.css";

const tokenStorageKey = "codesk.auth.token";
const portableFileNameIllegalChars = /[\u0000-\u001F<>:"\/\\|?*]/g;
const windowsReservedBaseName = /^(con|prn|aux|nul|com[1-9]|lpt[1-9])$/i;

export function shouldRenderOnboardingChecklist(
  namespaceReady: boolean,
  activePresentation: OnboardingPresentation | null | undefined,
): boolean {
  return namespaceReady && activePresentation !== "spotlight";
}

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

type ThreadAnchorInfo = Record<string, { orphaned: boolean; line: number }>;

function threadIsObsolete(thread: ThreadItem, anchorInfo?: ThreadAnchorInfo) {
  return thread.status !== "resolved"
    && thread.anchor.kind !== "document"
    && anchorInfo?.[thread.id]?.orphaned === true;
}

function visibleAgentStatus(
  agent: Agent,
  runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"],
  daemons: Daemon[],
  nowMs: number,
) {
  return agentDisplayStatus(agent, runs, daemons, nowMs);
}

function detailStatusLabel(status: ReturnType<typeof visibleAgentStatus>) {
  return status.detailLabel ?? status.label;
}

function clamp(value: number, min: number, max: number) {
  return Math.min(Math.max(value, min), max);
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
  if (route.kind === "desktopConnectComplete") {
    return <RouteMessageScreen title="Credential handoff complete" body="Check your desktop app for connection status. You can close this tab." />;
  }

  if (!token) {
    if (route.kind === "invite") {
      return <InvitePage api={api} inviteToken={route.token} account={null} workspaces={[]} onAuth={saveAuth} onAccepted={acceptInviteWorkspace} />;
    }
    if (route.kind === "desktopConnect") {
      return (
        <AuthScreen api={api} mode="login" onAuth={saveAuth} title="Connect desktop app" copy="Log in to connect your Codesk desktop app." preserveRoute>
          <DesktopConnectCallbackInfo callback={route.callback} />
        </AuthScreen>
      );
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

  if (route.kind === "desktopConnect") {
    return <DesktopConnectPage api={api} callback={route.callback} workspaces={workspaces} />;
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
        title={`Join ${preview.workspace.name}`}
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
        <h1 className="auth-title">Join {preview.workspace.name}</h1>
        <p className="small muted">You'll join this project and share its documents and discussions with the team and its agents.</p>
        <p className="tiny muted">This invitation expires in {relativeInviteExpiry(preview.expiresAt)}.</p>
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
            <span className="hint">This is how teammates and agents mention you here — unique to this workspace. {identifierHelpText}</span>
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

const callbackNoncePattern = /^\/desktop\/connect\/[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/;

export function isLoopbackCallback(raw: string): boolean {
  try {
    const parsed = new URL(raw);
    if (parsed.protocol !== "http:" || parsed.hostname !== "127.0.0.1") return false;
    if (parsed.port === "" || parsed.port === "0") return false;
    if (!callbackNoncePattern.test(parsed.pathname)) return false;
    return `http://127.0.0.1:${parsed.port}${parsed.pathname}` === raw;
  } catch {
    return false;
  }
}

function DesktopConnectCallbackInfo({ callback }: { callback: string }) {
  if (!callback || !isLoopbackCallback(callback)) {
    return <p className="muted tiny">Desktop app callback not detected.</p>;
  }
  return <p className="muted tiny">Your desktop app is waiting to connect.</p>;
}

function DesktopConnectPage({
  api,
  callback,
  workspaces,
}: {
  api: ApiClient;
  callback: string;
  workspaces: WorkspaceSummary[];
}) {
  const [selected, setSelected] = useState<string | null>(workspaces.length === 1 ? workspaces[0].id : null);
  const [environmentName, setEnvironmentName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const formRef = useRef<HTMLFormElement>(null);
  const [formFields, setFormFields] = useState<Record<string, string> | null>(null);

  if (!callback || !isLoopbackCallback(callback)) {
    return <RouteMessageScreen title="Invalid callback" body="The desktop app callback URL is missing or not a loopback address." />;
  }

  const selectedWorkspace = workspaces.find((w) => w.id === selected) ?? null;

  const submit = async () => {
    const name = environmentName.trim();
    if (!selectedWorkspace || !name) return;
    setSubmitting(true);
    setError("");
    try {
      const response = await api.createDaemon(selectedWorkspace.id, name);
      setFormFields({
        daemon_id: response.daemon.id,
        token: response.token,
        workspace_id: selectedWorkspace.id,
        workspace_name: selectedWorkspace.name,
        workspace_slug: selectedWorkspace.slug,
        workspace_url: `${publicOrigin}/w/${encodeURIComponent(selectedWorkspace.slug)}`,
      });
    } catch (err) {
      setError(err instanceof ApiError ? err.details : err instanceof Error ? err.message : String(err));
      setSubmitting(false);
    }
  };

  useEffect(() => {
    if (formFields && formRef.current) {
      formRef.current.submit();
    }
  }, [formFields]);

  if (workspaces.length === 0) {
    return <RouteMessageScreen title="No workspaces" body="Create a workspace first, then reconnect your desktop app." />;
  }

  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">Connect desktop app</h1>
        <p className="muted">Choose where your desktop app will sync and name the local environment it creates.</p>
        <div className="form-stack">
          {workspaces.map((w) => (
            <label key={w.id} className="row gap-8 items-center" style={{ cursor: "pointer", padding: "8px 0" }}>
              <input type="radio" name="workspace" checked={selected === w.id} onChange={() => setSelected(w.id)} />
              <div className="avi workspace-avi sm">{initials(w.name)}</div>
              <div className="col gap-2 min-0">
                <b className="truncate">{w.name}</b>
                <span className="tiny muted truncate">/{w.slug}</span>
              </div>
            </label>
          ))}
          <label className="field">
            <span className="lab">Local environment name</span>
            <input
              aria-label="Local environment name"
              value={environmentName}
              placeholder="Build server"
              onChange={(event) => setEnvironmentName(event.target.value)}
              disabled={submitting}
              required
            />
            <span className="hint">This is what you'll see in Local environments.</span>
          </label>
          {error ? <p className="error-text">{error}</p> : null}
          <button className="btn accent full lg" disabled={!selected || !environmentName.trim() || submitting} onClick={submit}>
            {submitting ? "Connecting..." : "Connect"}
          </button>
        </div>
      </section>
      {formFields ? (
        <form ref={formRef} method="POST" action={callback} style={{ display: "none" }}>
          {Object.entries(formFields).map(([k, v]) => (
            <input key={k} type="hidden" name={k} value={v} />
          ))}
        </form>
      ) : null}
    </main>
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

function relativeInviteExpiry(value: string) {
  const date = new Date(value);
  const ms = date.valueOf() - Date.now();
  if (Number.isNaN(date.valueOf()) || ms <= 0) {
    return "soon";
  }
  const minutes = Math.round(ms / 60000);
  if (minutes < 60) {
    return `${minutes} minute${minutes === 1 ? "" : "s"}`;
  }
  const hours = Math.round(minutes / 60);
  if (hours < 48) {
    return `${hours} hour${hours === 1 ? "" : "s"}`;
  }
  const days = Math.round(hours / 24);
  return `${days} day${days === 1 ? "" : "s"}`;
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
          <h1 className="auth-title">Create your first workspace</h1>
          <p className="small muted">A workspace is the shared home for one project or team. Documents, members, local files, and agents all live here.</p>
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
      <label className="field">
        <span className="lab">Workspace name</span>
        <div className="name-row">
          <input
            aria-label="Workspace name"
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
        <span className="hint">Use a project, team, or client name. You can change it later.</span>
      </label>
      <label className="field">
        <span className="lab">Workspace address</span>
        <div className="address-row">
          <span className="address-prefix">codesk.co/</span>
          <input
            aria-label="Workspace address"
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
        </div>
        <span className="hint">This becomes the link people use to reach the workspace.</span>
      </label>
      <label className="field">
        <span className="lab">Your handle</span>
        <input
          aria-label="Your handle"
          value={handle}
          onChange={(event) => setHandle(event.target.value)}
          pattern={identifierPattern}
          minLength={handleMinLength}
          maxLength={handleMaxLength}
          title={identifierHelpText}
          required
        />
        <span className="hint">Teammates and agents mention you by this. {identifierHelpText}</span>
      </label>
      {error ? <p className="error-text">{error}</p> : null}
      <button className="btn accent full lg">Create workspace</button>
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

// Document subscribers (watchers), lifted to the parent so the top-bar badge count is live while the popover
// is unmounted. Scope-carrying: the stored result is tagged with the workspace+document key it was read for,
// and `subscriberIds` reads as null (unknown/loading) unless that key matches the CURRENT scope — so a stale
// A-count can never paint on B (the make-invalid-states-unrepresentable fix, without an effect reset). A
// readSeq guard keeps only the latest-started read among same-scope reads, and optimistic mutations are
// applied only while the stored result is still this scope. This is the task-#4 isolation, hoisted.
const NO_BUSY_IDS: Set<string> = new Set();

export function useDocumentSubscribers(api: ApiClient, workspaceId: string, documentId: string | undefined) {
  const scopeKey = documentId ? `${workspaceId}/${documentId}` : "";
  // result, errorState, and busyState are all SCOPE-CARRYING: each is tagged with the scope key it belongs to,
  // and reads as null/empty unless that key matches the CURRENT scope. So a stale scope's async settle can
  // never surface its ids, error, or busy marker on a different document — and a mutation on A that settles
  // after a switch to B cannot delete B's same-agent busy marker (its cleanup no-ops on a foreign scope).
  const [result, setResult] = useState<{ key: string; ids: string[] } | null>(null);
  const [errorState, setErrorState] = useState<{ key: string; message: string }>({ key: scopeKey, message: "" });
  const [busyState, setBusyState] = useState<{ key: string; ids: Set<string> }>({ key: scopeKey, ids: NO_BUSY_IDS });
  const readSeqRef = useRef(0);
  // The CURRENT scope, readable synchronously inside async callbacks captured under an OLD scope. A reconcile
  // from a dead scope (a mutation on doc A settling after a switch to B) checks this and becomes a full no-op —
  // critically it must not bump readSeq or fetch, or it would starve B's in-flight initial read.
  const scopeKeyRef = useRef(scopeKey);
  scopeKeyRef.current = scopeKey;

  const subscriberIds = result && result.key === scopeKey ? result.ids : null;
  const loaded = subscriberIds !== null;
  const error = errorState.key === scopeKey ? errorState.message : "";
  const busyIds = busyState.key === scopeKey ? busyState.ids : NO_BUSY_IDS;

  const reload = useCallback(async () => {
    if (!documentId) return;
    const key = scopeKey;
    // A reconcile from a scope that is no longer current does nothing — no seq bump, no fetch — so it cannot
    // drop the current scope's read.
    if (key !== scopeKeyRef.current) return;
    const seq = ++readSeqRef.current;
    try {
      const response = await api.listDocumentSubscribers(workspaceId, documentId);
      // Re-check AFTER the await: neither a newer same-scope read (seq) nor a scope switch (key) may have
      // happened, or this response is stale and must not land.
      if (readSeqRef.current !== seq || key !== scopeKeyRef.current) return;
      setResult({ key, ids: response.agents.map((agent) => agent.id) });
      setErrorState((current) => (current.key === key && current.message ? { key, message: "" } : current));
    } catch (err) {
      if (readSeqRef.current !== seq || key !== scopeKeyRef.current) return;
      setErrorState({ key, message: err instanceof Error ? err.message : "Failed to load watchers" });
    }
  }, [api, workspaceId, documentId, scopeKey]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const mutate = useCallback(
    async (agentId: string, subscribe: boolean) => {
      const key = scopeKey;
      if (!documentId || (busyState.key === key && busyState.ids.has(agentId))) return;
      // Scope-carry the busy marker: if the stored busy state is a foreign scope, start fresh under this scope.
      setBusyState((current) => {
        const base = current.key === key ? current.ids : NO_BUSY_IDS;
        return { key, ids: new Set(base).add(agentId) };
      });
      // Optimistic, scope-carrying: only touch ids while the stored result is still this scope.
      setResult((current) =>
        current && current.key === key
          ? { key, ids: subscribe ? Array.from(new Set([...current.ids, agentId])) : current.ids.filter((id) => id !== agentId) }
          : current,
      );
      try {
        if (subscribe) {
          await api.subscribeAgentToDocument(workspaceId, agentId, documentId);
        } else {
          await api.unsubscribeAgentFromDocument(workspaceId, agentId, documentId);
        }
      } catch (err) {
        // Only surface the error if this mutation's scope is still current — a rejection arriving after a
        // switch must not paint A's error onto B (also guarded by the scope-carrying derivation).
        if (scopeKeyRef.current === key) {
          setErrorState({ key, message: err instanceof Error ? err.message : "Update failed" });
        }
      } finally {
        await reload();
        // Clear this mutation's busy marker only while the busy state is still THIS scope — never delete a
        // different scope's marker for the same agent id.
        setBusyState((current) => {
          if (current.key !== key || !current.ids.has(agentId)) return current;
          const next = new Set(current.ids);
          next.delete(agentId);
          return { key, ids: next };
        });
      }
    },
    [api, workspaceId, documentId, scopeKey, busyState, reload],
  );

  return { subscriberIds, loaded, busyIds, error, reload, mutate };
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
  const [modal, setModal] = useState<"daemon" | "agent" | "move" | "delete-doc" | "agent-detail" | "daemon-detail" | "manage" | null>(null);
  // Default tab is Members & Invite (plan tab order). Integration protocol (Juan's
  // single-flip rule): A2 fills Members before integration, so nothing ships showing
  // a placeholder. If A2 slips out of the batch, flipping this default to "local-env"
  // (the tab with real content) becomes a REQUIRED pre-integration change.
  const [manageTab, setManageTab] = useState<ManageTab>("members");
  // Phase E: collapsible left rail + resizable right rail, both remembered in localStorage.
  const [sidebarCollapsed, setSidebarCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem("codesk.sb.collapsed") === "1";
    } catch {
      return false;
    }
  });
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
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [selectedDaemonId, setSelectedDaemonId] = useState<string | null>(null);
  const [selectedThreadId, setSelectedThreadId] = useState("");
  const [focusThreadId, setFocusThreadId] = useState("");
  const [documentThreadsOpen, setDocumentThreadsOpen] = useState(false);
  const documentThreadsRef = useRef<HTMLDivElement>(null);
  const documentThreadsDialogRef = useRef<HTMLDivElement>(null);
  const documentThreadsTriggerRef = useRef<HTMLButtonElement>(null);
  const [documentWatchersOpen, setDocumentWatchersOpen] = useState(false);
  const documentWatchersRef = useRef<HTMLDivElement>(null);
  const documentWatchersDialogRef = useRef<HTMLDivElement>(null);
  const documentWatchersTriggerRef = useRef<HTMLButtonElement>(null);
  const [documentActivityOpen, setDocumentActivityOpen] = useState(false);
  const [moreMenuOpen, setMoreMenuOpen] = useState(false);
  const documentMoreRef = useRef<HTMLDivElement>(null);
  const documentActivityDialogRef = useRef<HTMLDivElement>(null);
  const moreMenuRef = useRef<HTMLDivElement>(null);
  const documentMoreTriggerRef = useRef<HTMLButtonElement>(null);
  const [collapsedFolders, setCollapsedFolders] = useState<Set<string>>(() => new Set());
  const [creatingDocument, setCreatingDocument] = useState(false);
  const [createError, setCreateError] = useState("");
  const [renamingDocumentId, setRenamingDocumentId] = useState("");
  const [freshDocumentId, setFreshDocumentId] = useState("");
  const [titleDraft, setTitleDraft] = useState("");
  const lastAccessUpdateKeyRef = useRef("");
  const seenWorkspaceRef = useRef(false);
  const deletionHandledRef = useRef(false);
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);

  const requestedDocumentId = view.kind === "document" ? view.documentId : "";
  const activeDocument = requestedDocumentId ? rootDocuments.find((document) => document.id === requestedDocumentId) ?? null : null;
  const documentMissing = view.kind === "document" && rootNamespace.ready && !activeDocument;
  const documentThreads = useMemo(() => activeDocument ? workspace.threads.filter((thread) => thread.documentId === activeDocument.id) : [], [workspace.threads, activeDocument?.id]);
  const [threadAnchorInfo, setThreadAnchorInfo] = useState<ThreadAnchorInfo>({});
  // Document subscribers (watchers) are fetched at the parent so the top-bar badge count is live even while
  // the popover is unmounted (mirrors documentOpenThreadCount). The hook is scope-carrying — its ids only
  // read as loaded when their key matches the current workspace+document — so a stale A-count can never paint
  // on B. See useDocumentSubscribers.
  const {
    subscriberIds: documentSubscriberIds,
    loaded: subscribersLoaded,
    busyIds: subscriberBusyIds,
    error: subscribersError,
    reload: reloadSubscribers,
    mutate: mutateSubscriber,
  } = useDocumentSubscribers(api, workspaceId, activeDocument?.id);
  const documentWatcherCount = useMemo(() => {
    if (documentSubscriberIds === null) return null;
    const subscribed = new Set(documentSubscriberIds);
    return workspace.agents.reduce((count, agent) => (subscribed.has(agent.id) ? count + 1 : count), 0);
  }, [workspace.agents, documentSubscriberIds]);
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

  // --- Onboarding (Batch 1 wiring) --------------------------------------------
  // selectionActive is lifted from DocumentEditor (below) so the account-scoped
  // first-selection tip can trigger; openThreadDraftRequest is the parent->editor
  // signal the tip's "Start thread" action fires (mirrors the formatRequest idiom).
  const [selectionActive, setSelectionActive] = useState(false);
  const [openThreadDraftRequest, setOpenThreadDraftRequest] = useState(0);
  const onboardingRole: OnboardingRole =
    workspace.currentMembershipRole === "owner" || workspace.currentMembershipRole === "admin"
      ? workspace.currentMembershipRole
      : "member";
  const onboarding = useOnboardingController({
    enabled: rootNamespace.ready,
    accountId: account?.id ?? "",
    workspaceId,
    roles: [onboardingRole],
    route: view.kind,
    selectionActive,
    workspaceState: workspace,
    documentCount: rootDocuments.length,
    // No workspace-wide agent-watcher count exists in Batch 1 (subscribers are fetched
    // per open document via useDocumentSubscribers), so "put an agent to work" derives
    // from the agent-run / agent-thread signals; the watcher leg lands with Batch 2's
    // Watchers/Activity work.
    watchedDocumentCount: 0,
    nowMs: now,
  });
  const recordOnboardingEvent = onboarding.record;
  const handleOnboardingAction = useCallback((event: OnboardingActionEvent) => {
    if (event === "open-thread-draft") {
      setOpenThreadDraftRequest((request) => request + 1);
    }
  }, []);

  const documentOpenThreadCount = useMemo(
    () => documentThreads.filter((thread) => (
      thread.status !== "resolved" && !threadIsObsolete(thread, threadAnchorInfo)
    )).length,
    [documentThreads, threadAnchorInfo],
  );

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

  const closeDocumentThreads = useCallback((restoreFocus = true) => {
    setDocumentThreadsOpen(false);
    setSelectedThreadId("");
    if (restoreFocus) {
      window.setTimeout(() => documentThreadsTriggerRef.current?.focus(), 0);
    }
  }, []);

  const closeDocumentWatchers = useCallback((restoreFocus = true) => {
    setDocumentWatchersOpen(false);
    if (restoreFocus) {
      window.setTimeout(() => documentWatchersTriggerRef.current?.focus(), 0);
    }
  }, []);

  const closeDocumentActivity = useCallback((restoreFocus = true) => {
    setDocumentActivityOpen(false);
    if (restoreFocus) {
      window.setTimeout(() => documentMoreTriggerRef.current?.focus(), 0);
    }
  }, []);

  const closeMoreMenu = useCallback((restoreFocus = true) => {
    setMoreMenuOpen(false);
    if (restoreFocus) {
      window.setTimeout(() => documentMoreTriggerRef.current?.focus(), 0);
    }
  }, []);

  useEffect(() => {
    if (!documentThreadsOpen) {
      return;
    }
    const focusTimer = window.setTimeout(() => {
      const dialog = documentThreadsDialogRef.current;
      if (!dialog) {
        return;
      }
      const first = focusableElements(dialog)[0];
      (first ?? dialog).focus();
    }, 0);
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && documentThreadsRef.current?.contains(target)) {
        return;
      }
      closeDocumentThreads();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeDocumentThreads();
        return;
      }
      if (event.key !== "Tab") {
        return;
      }
      const dialog = documentThreadsDialogRef.current;
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
  }, [closeDocumentThreads, documentThreadsOpen]);

  useEffect(() => {
    if (!documentWatchersOpen) {
      return;
    }
    const focusTimer = window.setTimeout(() => {
      const dialog = documentWatchersDialogRef.current;
      if (!dialog) {
        return;
      }
      const first = focusableElements(dialog)[0];
      (first ?? dialog).focus();
    }, 0);
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && documentWatchersRef.current?.contains(target)) {
        return;
      }
      closeDocumentWatchers();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeDocumentWatchers();
        return;
      }
      if (event.key !== "Tab") {
        return;
      }
      const dialog = documentWatchersDialogRef.current;
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
  }, [closeDocumentWatchers, documentWatchersOpen]);

  useEffect(() => {
    if (!documentActivityOpen) {
      return;
    }
    const focusTimer = window.setTimeout(() => {
      const dialog = documentActivityDialogRef.current;
      if (!dialog) {
        return;
      }
      const first = focusableElements(dialog)[0];
      (first ?? dialog).focus();
    }, 0);
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && documentMoreRef.current?.contains(target)) {
        return;
      }
      closeDocumentActivity();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeDocumentActivity();
        return;
      }
      if (event.key !== "Tab") {
        return;
      }
      const dialog = documentActivityDialogRef.current;
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
  }, [closeDocumentActivity, documentActivityOpen]);

  useEffect(() => {
    if (!moreMenuOpen) {
      return;
    }
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && documentMoreRef.current?.contains(target)) {
        return;
      }
      closeMoreMenu(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeMoreMenu();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [closeMoreMenu, moreMenuOpen]);

  useEffect(() => {
    setDocumentThreadsOpen(false);
    setSelectedThreadId("");
    setThreadAnchorInfo({});
    setDocumentWatchersOpen(false);
    setDocumentActivityOpen(false);
    setMoreMenuOpen(false);
  }, [activeDocument?.id]);

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
      setSelectedThreadId("");
      setRenamingDocumentId(doc.id);
      setFreshDocumentId(doc.id);
      setTitleDraft(fileName(path));
      void reload();
      recordOnboardingEvent("first_document_created", "workspace");
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreatingDocument(false);
    }
  }, [
    activeDocument?.path,
    api,
    creatingDocument,
    recordOnboardingEvent,
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
  const documentActivityList = documentActivity(workspace, activeDocument?.id);
  const activityActorLabel: Record<string, string> = {};
  for (const user of workspace.users) {
    activityActorLabel[user.id] = user.handle;
  }
  for (const agent of workspace.agents) {
    activityActorLabel[agent.id] = agent.handle;
  }

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
      className={`shell${sidebarCollapsed ? " sidebar-collapsed" : ""}`}
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
      <aside className="sb">
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

        <section className="sb-section flex-1 doc-tree">
          <div className="lab">
            <span className="label">Documents</span>
            <button
              className="btn ghost icon sm"
              title="New doc"
              aria-label="New document"
              data-onboarding-id="new-document"
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
              const status = visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons, now);
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
          <div className="row doc-toolbar-actions">
            <CollaboratorAvatars people={workspacePeopleList} />
            {activeDocument ? (
              <div className="document-threads-entry" ref={documentThreadsRef}>
                <button
                  ref={documentThreadsTriggerRef}
                  className={`btn sm document-threads-trigger ${documentThreadsOpen ? "selected" : "ghost"}`}
                  type="button"
                  data-onboarding-id="document-threads"
                  aria-label={`Threads, ${documentOpenThreadCount} open`}
                  aria-haspopup="dialog"
                  aria-expanded={documentThreadsOpen}
                  aria-controls="document-threads-popover"
                  onClick={() => {
                    if (documentThreadsOpen) {
                      closeDocumentThreads(false);
                    } else {
                      setDocumentWatchersOpen(false);
                      setDocumentActivityOpen(false);
                      setMoreMenuOpen(false);
                      setSelectedThreadId("");
                      setDocumentThreadsOpen(true);
                    }
                  }}
                >
                  <Icon name="message" />
                  <span>{documentOpenThreadCount}</span>
                </button>
                {documentThreadsOpen ? (
                  <div
                    ref={documentThreadsDialogRef}
                    id="document-threads-popover"
                    className="document-threads-popover card lifted"
                    role="dialog"
                    aria-modal="false"
                    aria-label="Threads on this document"
                    tabIndex={-1}
                  >
                    {!selectedThreadId ? (
                      <div className="document-threads-popover-head">
                        <div className="row gap-8 min-0">
                          <Icon name="message" />
                          <b>Threads</b>
                          <span className="small muted">· {documentOpenThreadCount} open</span>
                        </div>
                        <button className="btn ghost icon sm" type="button" onClick={() => closeDocumentThreads()} aria-label="Close threads">×</button>
                      </div>
                    ) : null}
                    <ThreadsPanel
                      api={api}
                      workspaceId={workspaceId}
                      threads={documentThreads}
                      threadAnchorInfo={threadAnchorInfo}
                      selectedThreadId={selectedThreadId}
                      onSelectThread={setSelectedThreadId}
                      onJumpToThread={(threadId) => {
                        closeDocumentThreads(false);
                        setFocusThreadId(threadId);
                      }}
                      onReply={() => void reload()}
                      onClose={() => closeDocumentThreads()}
                      embedded
                    />
                  </div>
                ) : null}
              </div>
            ) : null}
            {activeDocument ? (
              <div className="document-watchers-entry" ref={documentWatchersRef}>
                <button
                  ref={documentWatchersTriggerRef}
                  className={`btn sm document-watchers-trigger ${documentWatchersOpen ? "selected" : "ghost"}`}
                  type="button"
                  data-onboarding-id="document-watchers"
                  aria-label={documentWatcherCount === null ? "Watchers" : `Watchers, ${documentWatcherCount} watching`}
                  aria-haspopup="dialog"
                  aria-expanded={documentWatchersOpen}
                  aria-controls="document-watchers-popover"
                  onClick={() => {
                    if (documentWatchersOpen) {
                      closeDocumentWatchers(false);
                    } else {
                      // Only one document popover open at a time — opening Watchers closes Threads and Activity.
                      setDocumentThreadsOpen(false);
                      setDocumentActivityOpen(false);
                      setMoreMenuOpen(false);
                      setSelectedThreadId("");
                      // Refetch on open so the always-visible badge self-heals cross-client subscription
                      // changes (no workspace WS event carries them); between opens the count can read a
                      // stale N — accepted parity with the old closed tab.
                      void reloadSubscribers();
                      setDocumentWatchersOpen(true);
                    }
                  }}
                >
                  <Icon name="users" />
                  <span>{documentWatcherCount === null ? "…" : documentWatcherCount}</span>
                </button>
                {documentWatchersOpen ? (
                  <div
                    ref={documentWatchersDialogRef}
                    id="document-watchers-popover"
                    className="document-watchers-popover card lifted"
                    role="dialog"
                    aria-modal="false"
                    aria-label="Watchers on this document"
                    tabIndex={-1}
                  >
                    <div className="document-threads-popover-head">
                      <div className="row gap-8 min-0">
                        <Icon name="users" />
                        <b>Watchers</b>
                        <span className="small muted">· {documentWatcherCount === null ? "…" : documentWatcherCount}</span>
                      </div>
                      <button className="btn ghost icon sm" type="button" onClick={() => closeDocumentWatchers()} aria-label="Close watchers">×</button>
                    </div>
                    <WatchersPanel
                      key={workspaceId + "/" + activeDocument.id}
                      documentId={activeDocument.id}
                      workspace={workspace}
                      subscriberIds={documentSubscriberIds}
                      loaded={subscribersLoaded}
                      busyIds={subscriberBusyIds}
                      error={subscribersError}
                      onMutate={mutateSubscriber}
                    />
                  </div>
                ) : null}
              </div>
            ) : null}
            <div className="document-more-entry" ref={documentMoreRef}>
              <button
                ref={documentMoreTriggerRef}
                className={`btn sm ghost icon document-more-trigger ${moreMenuOpen || documentActivityOpen ? "selected" : ""}`}
                type="button"
                data-onboarding-id="document-more"
                onClick={() => {
                  if (moreMenuOpen) {
                    closeMoreMenu(false);
                  } else {
                    // One document surface at a time — opening the menu closes the Activity popover.
                    setDocumentActivityOpen(false);
                    setMoreMenuOpen(true);
                  }
                }}
                disabled={!activeDocument}
                aria-label="Document options"
                aria-haspopup="menu"
                aria-expanded={moreMenuOpen}
              >
                <Icon name="more" />
              </button>
              {moreMenuOpen && activeDocument ? (
                <div className="document-more-menu card lifted" role="menu" aria-label="Document options">
                  <button
                    role="menuitem"
                    className="document-more-item"
                    type="button"
                    onClick={() => {
                      setMoreMenuOpen(false);
                      // Three-way exclusion: opening Activity closes Threads and Watchers.
                      setDocumentThreadsOpen(false);
                      setSelectedThreadId("");
                      setDocumentWatchersOpen(false);
                      setDocumentActivityOpen(true);
                    }}
                  >
                    <Icon name="activity" />
                    <span>Document activity</span>
                  </button>
                  <button role="menuitem" className="document-more-item" type="button" onClick={() => { setMoreMenuOpen(false); setModal("move"); }}>
                    <Icon name="move" />
                    <span>Move document</span>
                  </button>
                  <button role="menuitem" className="document-more-item destructive" type="button" onClick={() => { setMoreMenuOpen(false); setModal("delete-doc"); }}>
                    <Icon name="trash" />
                    <span>Delete document</span>
                  </button>
                </div>
              ) : null}
              {documentActivityOpen && activeDocument ? (
                <div
                  ref={documentActivityDialogRef}
                  id="document-activity-popover"
                  className="document-activity-popover card lifted"
                  role="dialog"
                  aria-modal="false"
                  aria-label="Activity on this document"
                  tabIndex={-1}
                >
                  <div className="document-threads-popover-head">
                    <div className="row gap-8 min-0">
                      <Icon name="activity" />
                      <b>Activity</b>
                    </div>
                    <button className="btn ghost icon sm" type="button" onClick={() => closeDocumentActivity()} aria-label="Close activity">×</button>
                  </div>
                  <ActivityPanel activities={documentActivityList} hasDocument={!!activeDocument} actorLabel={activityActorLabel} />
                </div>
              ) : null}
            </div>
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
            onThreadCreated={() => {
              void reload();
              // Account-scoped so "has used threads" survives a workspace switch — the
              // account-durable completion leg of the first-selection tip (plan §4.3).
              onboarding.record("first_thread_created", "account");
            }}
            onThreadsChanged={() => void reload()}
            titleEditing={renamingDocumentId === activeDocument.id}
            titleDraft={renamingDocumentId === activeDocument.id ? titleDraft : fileName(activeDocument.path)}
            onTitleEditStart={() => startRenamingDocument(activeDocument)}
            onTitleDraftChange={setTitleDraft}
            onTitleEditCancel={cancelRenamingDocument}
            onTitleCommit={(draft) => commitDocumentTitle(activeDocument, draft)}
            onThreadAnchorInfo={setThreadAnchorInfo}
            onSelectionActiveChange={setSelectionActive}
            openThreadDraftRequest={openThreadDraftRequest}
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

      {modal === "move" && activeDocument ? (
        <MoveDocumentModal
          document={activeDocument}
          documents={rootDocuments}
          onClose={() => setModal(null)}
          onMove={async (path) => {
            rootNamespace.moveFile(activeDocument.id, path);
            setModal(null);
          }}
        />
      ) : null}
      {modal === "delete-doc" && activeDocument ? (
        <DeleteDocumentModal
          document={activeDocument}
          onClose={() => setModal(null)}
          onDelete={async () => {
            rootNamespace.tombstoneFile(activeDocument.id);
            navigate({ kind: "workspace", slug: workspaceSlug, view: { kind: "home" } }, { replace: true });
            setModal(null);
          }}
        />
      ) : null}
      {modal === "daemon" ? <CreateDaemonModal api={api} workspaceId={workspaceId} daemons={workspace.daemons} onClose={() => setModal(null)} onDone={() => void reload()} /> : null}
      {modal === "agent" ? <CreateAgentModal api={api} workspaceId={workspaceId} daemons={workspace.daemons} onClose={() => setModal(null)} onDone={() => { setModal(null); void reload(); }} /> : null}
      {modal === "agent-detail" && selectedAgentId ? <AgentDetailModal api={api} workspaceId={workspaceId} agentId={selectedAgentId} agents={workspace.agents} daemons={workspace.daemons} runs={workspace.agentRuns} onClose={() => setModal(null)} onChanged={() => void reload()} /> : null}
      {modal === "daemon-detail" && selectedDaemonId ? <DaemonDetailModal api={api} workspaceId={workspaceId} daemonId={selectedDaemonId} daemons={workspace.daemons} agents={workspace.agents} runs={workspace.agentRuns} onClose={() => setModal(null)} onChanged={() => { setModal(null); void reload(); }} /> : null}
      {modal === "manage" ? (
        <ManageModal
          api={api}
          workspaceId={workspaceId}
          workspace={workspace}
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
          }}
          onMemberInvited={() => onboarding.record("member_invited", "workspace")}
        />
      ) : null}
      {onboarding.active ? (
        <Onboarding
          step={onboarding.active}
          stepIndex={onboarding.stepIndex}
          total={onboarding.total}
          onNext={onboarding.next}
          onBack={onboarding.back}
          onSkip={onboarding.skip}
          onAction={handleOnboardingAction}
        />
      ) : null}
      {shouldRenderOnboardingChecklist(rootNamespace.ready, onboarding.active?.presentation) ? (
        <OnboardingChecklist
          progress={onboarding.checklist}
          dismissed={onboarding.checklistDismissed}
          onDismiss={onboarding.dismissChecklist}
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
  const running = workspace.agents.filter((agent) => visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons, now).key === "running").length;
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
            <button className="btn accent" type="button" data-onboarding-id="new-agent" onClick={onNew}>
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
                  const status = visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons, now);
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

function ThreadDetailContent({
  thread,
  statusLabel,
  anchorLabel,
  backButtonRef,
  backLabel,
  onBack,
  onClose,
  onToggleStatus,
  statusBusy,
  onJump,
  jumpLabel,
  error,
  reply,
  onReplyChange,
  onSubmitReply,
  replyBusy,
}: {
  thread: ThreadItem;
  statusLabel: "open" | "obsolete" | "resolved";
  anchorLabel?: string;
  backButtonRef?: RefObject<HTMLButtonElement>;
  backLabel: string;
  onBack: () => void;
  onClose?: () => void;
  onToggleStatus: () => void;
  statusBusy: boolean;
  onJump?: () => void;
  jumpLabel?: string;
  error: string;
  reply: string;
  onReplyChange: (value: string) => void;
  onSubmitReply: (event: FormEvent) => void;
  replyBusy: boolean;
}) {
  const resolved = thread.status === "resolved";
  const author = thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone";
  return (
    <>
      <div className="thread-popover-head thread-popover-detail-head">
        <button ref={backButtonRef} className="btn ghost icon sm" type="button" onClick={onBack} aria-label={backLabel}>
          <Icon name="back" />
        </button>
        <b className="small truncate">{author}</b>
        <span className="tiny muted">· {statusLabel}</span>
        <span className="thread-popover-head-spacer" />
        {onClose ? <button className="btn ghost icon sm" onClick={onClose} type="button" aria-label="Close">×</button> : null}
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
        <span className="thread-popover-anchor-excerpt">{thread.anchor.excerpt || thread.title}</span>
        {anchorLabel ? <span className="thread-popover-anchor-line">{anchorLabel}</span> : null}
      </div>
      <div className="thread-popover-messages">
        {thread.messages.map((message) => (
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
          onClick={onToggleStatus}
          disabled={statusBusy}
          aria-label={resolved ? "Reopen thread" : "Mark as resolved"}
        >
          {statusBusy ? "Updating…" : resolved ? "Reopen" : "Mark as resolved"}
        </button>
        {onJump && jumpLabel ? (
          <button className="thread-popover-jump" type="button" onClick={onJump} aria-label={jumpLabel}>
            {jumpLabel} →
          </button>
        ) : null}
      </div>
      {error ? <div className="thread-popover-error" role="alert">{error}</div> : null}
      <form className="thread-popover-reply" onSubmit={onSubmitReply}>
        <textarea
          rows={1}
          value={reply}
          onChange={(event) => onReplyChange(event.target.value)}
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
    </>
  );
}

export function ThreadPopover({
  api,
  workspaceId,
  group,
  point,
  onClose,
  onThreadsChanged,
}: {
  api: ApiClient;
  workspaceId: string;
  group: LineThreadGroup<LiveThread>;
  point: { x: number; y: number };
  onClose: () => void;
  onThreadsChanged: () => void;
}) {
  const popoverRef = useRef<HTMLDivElement | null>(null);
  const backButtonRef = useRef<HTMLButtonElement>(null);
  const [threadItems, setThreadItems] = useState(() => [...group.threads]);
  const [selectedThreadId, setSelectedThreadId] = useState("");
  const [reply, setReply] = useState("");
  const [replyBusy, setReplyBusy] = useState(false);
  const [statusBusy, setStatusBusy] = useState(false);
  const [error, setError] = useState("");
  const [viewportHeight, setViewportHeight] = useState(() => window.visualViewport?.height ?? window.innerHeight);
  const [placement, setPlacement] = useState<{ left: number; top: number } | null>(null);
  const groupKey = `${group.line}:${group.threads.map((thread) => thread.id).sort().join("|")}`;
  const selected = selectedThreadId ? threadItems.find((thread) => thread.id === selectedThreadId) ?? null : null;
  const openThreadItems = threadItems.filter((thread) => thread.status !== "resolved");
  const headerThread = threadItems.find((thread) => thread.status !== "resolved") ?? threadItems[0];
  const headerExcerpt = headerThread?.anchor.excerpt || headerThread?.title || "Thread";
  const popoverStyle = {
    left: placement?.left ?? point.x,
    top: placement?.top ?? point.y,
    "--thread-popover-viewport-height": `${viewportHeight}px`,
  } as CSSProperties;

  // Keep the card inside the viewport (thread-redesign containment fix): after render — when the card's real
  // size is known and CSS has capped its height — clamp the fixed position, and re-clamp on resize and when
  // the list↔detail switch changes the card's height. The clamp math is pure (clampPopoverPosition); this
  // only measures and feeds it.
  useLayoutEffect(() => {
    const reposition = () => {
      const node = popoverRef.current;
      if (!node) return;
      const rect = node.getBoundingClientRect();
      setPlacement(
        clampPopoverPosition(
          point,
          { width: rect.width, height: rect.height },
          { width: window.visualViewport?.width ?? window.innerWidth, height: window.visualViewport?.height ?? window.innerHeight },
        ),
      );
    };
    reposition();
    window.addEventListener("resize", reposition);
    window.visualViewport?.addEventListener("resize", reposition);
    return () => {
      window.removeEventListener("resize", reposition);
      window.visualViewport?.removeEventListener("resize", reposition);
    };
  }, [point.x, point.y, selectedThreadId, viewportHeight]);

  useEffect(() => {
    setThreadItems((current) => {
      const currentById = new Map(current.map((thread) => [thread.id, thread]));
      return group.threads.map((thread) => {
        const local = currentById.get(thread.id);
        return local && local.updatedAt > thread.updatedAt ? local : thread;
      });
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
    setThreadItems((current) => current.map((thread) => (
      thread.id === updated.id ? mergePopoverThread(thread, updated) : thread
    )));
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
          <ThreadDetailContent
            thread={selected}
            statusLabel={resolved ? "resolved" : "open"}
            anchorLabel={`line ${group.line}`}
            backButtonRef={backButtonRef}
            backLabel="Back to threads on this line"
            onBack={backToList}
            onClose={onClose}
            onToggleStatus={toggleStatus}
            statusBusy={statusBusy}
            error={error}
            reply={reply}
            onReplyChange={setReply}
            onSubmitReply={submitReply}
            replyBusy={replyBusy}
          />
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
        aria-label={`Threads anchored to ${headerExcerpt}`}
      >
        <div className="thread-popover-head thread-popover-list-head">
          <Icon name="message" />
          <div className="thread-popover-head-copy">
            <b className="small thread-popover-head-excerpt">{headerExcerpt}</b>
            <span className="small muted">
              · {openThreadItems.length ? `${openThreadItems.length} thread${openThreadItems.length === 1 ? "" : "s"}` : "0 open"}
            </span>
          </div>
          <button className="btn ghost icon sm" onClick={onClose} type="button" aria-label="Close">×</button>
        </div>
        <div className="thread-popover-list">
          {openThreadItems.map((thread) => {
            const author = thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone";
            return (
              <button
                className="thread-popover-row"
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
          {!openThreadItems.length ? <p className="empty-note">No open threads on this line</p> : null}
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
  titleEditing,
  titleDraft,
  onTitleEditStart,
  onTitleDraftChange,
  onTitleEditCancel,
  onTitleCommit,
  onThreadAnchorInfo,
  onSelectionActiveChange,
  openThreadDraftRequest,
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
  titleEditing: boolean;
  titleDraft: string;
  onTitleEditStart: () => void;
  onTitleDraftChange: (value: string) => void;
  onTitleEditCancel: () => void;
  onTitleCommit: (value: string) => void;
  onThreadAnchorInfo: (info: ThreadAnchorInfo) => void;
  onSelectionActiveChange?: (active: boolean) => void;
  openThreadDraftRequest?: number;
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
      const info: ThreadAnchorInfo = {};
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

  // Report live text-selection presence up to WorkspaceApp so the account-scoped
  // first-selection onboarding tip can trigger; reset on unmount so leaving the
  // document view clears the signal.
  useEffect(() => {
    onSelectionActiveChange?.(hasRangeSelection);
  }, [hasRangeSelection, onSelectionActiveChange]);
  useEffect(() => () => onSelectionActiveChange?.(false), [onSelectionActiveChange]);

  // The onboarding tip's "Start thread" action bumps openThreadDraftRequest; open the
  // draft for the current selection once per bump (ref-compare so a later selection
  // change never re-opens it), mirroring the formatRequest request-id idiom.
  const lastThreadDraftRequestRef = useRef(0);
  useEffect(() => {
    if (openThreadDraftRequest && openThreadDraftRequest !== lastThreadDraftRequestRef.current) {
      lastThreadDraftRequestRef.current = openThreadDraftRequest;
      if (hasRangeSelection) {
        setThreadDraftOpen(true);
      }
    }
  }, [openThreadDraftRequest, hasRangeSelection]);

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
            <button className="primary" type="button" data-onboarding-id="selection-thread" onClick={openThreadDraft}>
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
  onClose,
  embedded = false,
}: {
  api: ApiClient;
  workspaceId: string;
  threads: ThreadItem[];
  threadAnchorInfo?: ThreadAnchorInfo;
  selectedThreadId: string;
  onSelectThread: (threadId: string) => void;
  onJumpToThread: (threadId: string) => void;
  onReply: () => void;
  onClose?: () => void;
  embedded?: boolean;
}) {
  const [reply, setReply] = useState("");
  const [replyBusy, setReplyBusy] = useState(false);
  const [statusBusy, setStatusBusy] = useState(false);
  const [statusError, setStatusError] = useState("");
  const [obsoleteFolded, setObsoleteFolded] = useState(() => {
    try { return localStorage.getItem("codesk.threads.obsoleteFolded") !== "false"; } catch { return true; }
  });
  const [resolvedFolded, setResolvedFolded] = useState(() => {
    try { return localStorage.getItem("codesk.threads.resolvedFolded") !== "false"; } catch { return true; }
  });
  const selected = selectedThreadId ? threads.find((thread) => thread.id === selectedThreadId) ?? null : null;
  const selectedResolved = selected?.status === "resolved";
  const selectedObsolete = Boolean(selected && threadIsObsolete(selected, threadAnchorInfo));
  const openThreads = threads.filter((thread) => thread.status !== "resolved" && !threadIsObsolete(thread, threadAnchorInfo));
  const obsoleteThreads = threads.filter((thread) => threadIsObsolete(thread, threadAnchorInfo));
  const resolvedThreads = threads.filter((t) => t.status === "resolved");

  const toggleObsoleteFold = () => {
    const next = !obsoleteFolded;
    setObsoleteFolded(next);
    try { localStorage.setItem("codesk.threads.obsoleteFolded", String(next)); } catch {}
  };

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
    if (!selected || !reply.trim() || replyBusy) {
      return;
    }
    setReplyBusy(true);
    setStatusError("");
    try {
      await api.replyThread(workspaceId, selected.id, reply.trim());
      setReply("");
      onReply();
    } catch (err) {
      setStatusError(err instanceof Error ? err.message : "Failed to send reply");
    } finally {
      setReplyBusy(false);
    }
  };

  if (!threads.length) {
    return (
      <div className={`ctx-body${embedded ? " document-threads-popover-body" : ""}`}>
        {!embedded ? (
          <div className="row between">
            <span className="label">Threads on this doc</span>
          </div>
        ) : null}
        <p className="empty-note">No threads on this document yet. Select text in the editor and open a thread.</p>
      </div>
    );
  }

  if (selected) {
    const selectedInfo = threadAnchorInfo?.[selected.id];
    const selectedHasLiveAnchor = selected.anchor.kind !== "document" && !selectedInfo?.orphaned;
    const anchorLabel = selected.anchor.kind === "document"
      ? "document"
      : selectedHasLiveAnchor && selectedInfo?.line ? `line ${selectedInfo.line}` : undefined;
    return (
      <div className="tdetail thread-document-detail">
        <ThreadDetailContent
          thread={selected}
          statusLabel={selectedResolved ? "resolved" : selectedObsolete ? "obsolete" : "open"}
          anchorLabel={anchorLabel}
          backLabel="Back to thread list"
          onBack={() => onSelectThread("")}
          onClose={onClose}
          onToggleStatus={toggleStatus}
          statusBusy={statusBusy}
          onJump={selectedHasLiveAnchor ? () => onJumpToThread(selected.id) : undefined}
          jumpLabel={selectedHasLiveAnchor && selectedInfo?.line ? `Jump to line ${selectedInfo.line}` : undefined}
          error={statusError}
          reply={reply}
          onReplyChange={setReply}
          onSubmitReply={submit}
          replyBusy={replyBusy}
        />
      </div>
    );
  }

  const renderThreadCard = (thread: ThreadItem) => {
    const isObsolete = threadIsObsolete(thread, threadAnchorInfo);
    const statusClass = thread.status === "resolved" ? "resolved" : isObsolete ? "obsolete" : "open";

    return (
      <button
        key={thread.id}
        className={`titem${thread.id === selectedThreadId ? " selected" : ""}${thread.status === "resolved" ? " resolved" : ""}${isObsolete ? " obsolete" : ""}`}
        onClick={() => onSelectThread(thread.id)}
        aria-label={`Open ${statusClass} thread by ${thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone"}`}
      >
        <div className={`avi sm ${thread.createdByType === "agent" ? "agent" : "you"}`}>{initials(thread.createdByHandle || thread.createdByName || "You")}</div>
        <div className="col gap-3 min-0 titem-copy">
          <div className="row gap-6 min-0">
            <span className="small truncate titem-summary">
              <b>{thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone"}</b>{" "}
              {thread.messages[0]?.body || thread.anchor.excerpt || thread.title}
            </span>
            <span className="tiny muted">{shortTime(thread.updatedAt)}</span>
          </div>
          <div className="row gap-4 tiny muted">
            <span>{threadReplyLabel(thread)}</span>
          </div>
        </div>
        <Icon name="chevron" />
      </button>
    );
  };

  return (
    <div className={`ctx-body${embedded ? " document-threads-popover-body" : ""}`}>
      {!embedded ? (
        <div className="row between ctx-head">
          <span className="label">Threads on this doc</span>
        </div>
      ) : null}
      <div className="tlist">
        {openThreads.map(renderThreadCard)}
      </div>
      {obsoleteThreads.length > 0 ? (
        <div className="thread-fold obsolete">
          <button className="thread-fold-header" type="button" onClick={toggleObsoleteFold} aria-expanded={!obsoleteFolded}>
            <span>Obsolete · {obsoleteThreads.length}</span>
            <span className={`thread-fold-chevron${obsoleteFolded ? "" : " expanded"}`}>▾</span>
          </button>
          {!obsoleteFolded ? (
            <div className="tlist">
              {obsoleteThreads.map(renderThreadCard)}
            </div>
          ) : null}
        </div>
      ) : null}
      {resolvedThreads.length > 0 ? (
        <div className="thread-fold">
          <button className="thread-fold-header" type="button" onClick={toggleResolvedFold} aria-expanded={!resolvedFolded}>
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

function CollaboratorAvatars({ people }: { people: WorkspacePerson[] }) {
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  // Same honest-decay ruling as the People panel (Eva): the ring reflects personOnline, which
  // decays on the presence freshness window, so a closed laptop drops the ring — never a stale
  // "online". The toolbar only renders confirmed-online collaborators; +N below is neutral.
  const online = people.filter((p) => personOnline(p, now));
  if (!online.length) return null;
  // Presence display only — not interactive (AlphaToad: clicking the avatars shouldn't open the Watchers
  // popover). A non-button, non-focusable element so there's no dead click affordance.
  return (
    <div className="collaborator-avatars" title={`${online.length} online`}>
      {online.slice(0, 5).map((p) => (
        <div key={p.id} className={`avi sm ${p.kind === "agent" ? "agent" : "you"} online`}>
          {initials(p.handle || p.name)}
        </div>
      ))}
      {/* +N overflow is a neutral count, NOT ringed — a ring would imply all N are online (Eva). */}
      {online.length > 5 ? <span className="avi sm you">+{online.length - 5}</span> : null}
    </div>
  );
}

// WatchersPanel: the document subscribers surface, hosted in the top-bar Watchers popover (it replaces the
// old right-rail Participants tab). Subscriber-only — the "Here now" presence display is intentionally gone
// with the tab (top-bar avatars still carry workspace presence; doc-level presence is a future revival). Two
// groups over the open document:
//   Watching — agents subscribed to this doc (the durable notification relationship), each removable.
//   Add watcher — the picker of NOT-yet-subscribed agents; the only place an unsubscribed agent appears.
// The panel is PRESENTATIONAL: the subscriber list, its fetch, and the async-race guards live in
// useDocumentSubscribers at the parent (so the top-bar badge count is live while this popover is unmounted).
// This instance is KEY'd on workspace+document by its parent, so a scope switch remounts it and the local
// picker state can never leak across documents. subscribe/unsubscribe are optimistic (in the hook) and
// reconcile on completion.
export function WatchersPanel({
  documentId,
  workspace,
  subscriberIds,
  loaded,
  busyIds,
  error,
  onMutate,
}: {
  documentId: string | undefined;
  workspace: Pick<WorkspaceState, "currentUserId" | "agents" | "users" | "presences">;
  subscriberIds: string[] | null;
  loaded: boolean;
  busyIds: Set<string>;
  error: string;
  onMutate: (agentId: string, subscribe: boolean) => void;
}) {
  const [picking, setPicking] = useState(false);
  // Watching / addable are subscription-only groupings (nowMs is irrelevant here since this surface no longer
  // renders presence), so pass 0 and ignore hereNow.
  const { watching, addable } = useMemo(
    () => documentParticipants(workspace, documentId, subscriberIds ?? [], 0),
    [workspace, documentId, subscriberIds],
  );

  const personAvatar = (person: WorkspacePerson) => (
    <div className="avi agent">{initials(person.handle || person.name)}</div>
  );

  const watchingRow = (person: WorkspacePerson) => (
    <div key={person.id} className="agent-card row between">
      <div className="row gap-2 min-0">
        {personAvatar(person)}
        <div className="col gap-2 min-0">
          <strong className="small truncate">@{person.handle}</strong>
          <span className="tiny muted truncate">Watching</span>
        </div>
      </div>
      <button className="btn sm ghost" onClick={() => onMutate(person.id, false)} disabled={busyIds.has(person.id)} title="Stop watching this document">
        Remove
      </button>
    </div>
  );

  return (
    <div className="ctx-body people-pane watchers-pane">
      {error ? <p className="empty-note error">{error}</p> : null}

      {/* Watching / Add-watcher wait for `loaded` so a just-switched-to document never shows the prior doc's
          rows or a false empty/all-addable panel, and actions stay disabled while the count is unknown. */}
      <div className="people-section-head"><span className="tiny muted">Watching</span><span className="chip sm">{loaded ? watching.length : "…"}</span></div>
      {!loaded ? (
        <p className="empty-note">Loading watchers…</p>
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
                  {personAvatar(person)}
                  <strong className="small truncate">@{person.handle}</strong>
                </div>
                <button className="btn sm" onClick={() => onMutate(person.id, true)} disabled={busyIds.has(person.id)}>
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

// The distinct folders across all documents, root first — folders are not entities, they exist only as path
// prefixes derived from document paths (Tom's fact), so this synthesizes them the same way the sidebar tree is
// built. Each carries its depth for indentation. A "New folder" is just a name typed under a selected folder;
// it becomes a real prefix only when a Move commits, so nothing is written here.
export function documentFolders(documents: DocumentItem[]): { path: string; name: string; depth: number }[] {
  const seen = new Set<string>([""]);
  const folders: { path: string; name: string; depth: number }[] = [{ path: "", name: "root", depth: 0 }];
  for (const doc of documents) {
    const segments = (doc.path || "").split("/").filter(Boolean).slice(0, -1);
    let prefix = "";
    segments.forEach((segment, index) => {
      prefix = prefix ? `${prefix}/${segment}` : segment;
      if (!seen.has(prefix)) {
        seen.add(prefix);
        folders.push({ path: prefix, name: segment, depth: index + 1 });
      }
    });
  }
  // Pre-order the tree by comparing paths SEGMENT-WISE, not as flat strings: a plain string compare puts
  // `notes-old` before `notes/x` (because '-' < '/'), which would render a depth-2 child indented under the
  // wrong depth-1 parent — an indented tree that isn't pre-order visually mis-parents. Segment-wise, a parent
  // prefix always sorts immediately before its children and before any later sibling.
  return folders.sort((left, right) => {
    const a = left.path.split("/");
    const b = right.path.split("/");
    const shared = Math.min(a.length, b.length);
    for (let i = 0; i < shared; i += 1) {
      if (a[i] !== b[i]) {
        return a[i].localeCompare(b[i]);
      }
    }
    return a.length - b.length;
  });
}

// Move = change the document's location (distinct from the title-bar rename, which edits only the name; both
// go through rootNamespace.moveFile). Folder picker: pick any folder in the derived tree, or type a "New folder
// in ‹selected›" name that becomes a real prefix only on commit. Reuses the existing filename validation.
export function MoveDocumentModal({ document, documents, onClose, onMove }: { document: DocumentItem; documents: DocumentItem[]; onClose: () => void; onMove: (path: string) => Promise<void> }) {
  const folders = useMemo(() => documentFolders(documents), [documents]);
  const baseName = document.path.split("/").pop() || document.path;
  const currentFolder = document.path.split("/").filter(Boolean).slice(0, -1).join("/");
  const [selectedFolder, setSelectedFolder] = useState(currentFolder);
  const [newFolder, setNewFolder] = useState("");
  const [newFolderActive, setNewFolderActive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // The new folder only applies while its inline row is active — cancelling it drops back to the plain selected
  // folder as the destination.
  const newFolderName = newFolderActive ? newFolder.trim() : "";
  const startNewFolder = () => {
    setNewFolderActive(true);
    setNewFolder("New folder");
  };
  // The folder name that would ACTUALLY commit — normalized the same way moveFile is: illegal chars filtered,
  // trailing dots/spaces stripped ("." / ".." collapse to nothing). The preview, target, and conflict check all
  // use this, so what the user sees is exactly what commits — no preview/commit divergence.
  const normalizedNewFolder = filterDocumentFileNameInput(newFolderName).replace(/[. ]+$/g, "");
  // Refuse names whose normalized form differs (`.`/`..`, trailing dots/spaces, illegal chars) AND any
  // dot-PREFIXED name: the daemon's visible-root contract ignores any dot-prefixed path segment
  // (isIgnoredWorkspaceRelativePath), so `Docs/.secret/…` would write a path the daemon refuses to project.
  const invalidNewFolder = newFolderName !== "" && (normalizedNewFolder !== newFolderName || newFolderName.startsWith("."));
  // If the typed New Folder name matches an EXISTING folder (case-insensitive), resolve to that folder's
  // CANONICAL path and move into it — moving into an existing folder is legal, so don't error; just don't
  // pretend it's new (the preview shows the real, canonically-cased path, not the typed case). Eva's honesty
  // ruling over a hard reject.
  const prospectiveFolder = normalizedNewFolder ? (selectedFolder ? `${selectedFolder}/${normalizedNewFolder}` : normalizedNewFolder) : selectedFolder;
  const existingFolderMatch = normalizedNewFolder ? folders.find((folder) => folder.path.toLowerCase() === prospectiveFolder.toLowerCase()) : undefined;
  const targetFolder = existingFolderMatch ? existingFolderMatch.path : prospectiveFolder;
  const targetPath = targetFolder ? `${targetFolder}/${baseName}` : baseName;

  // An active New Folder row with a blank name must NOT silently fall back to moving into the parent — it's a
  // required-name error until the user types one or presses Escape/blur to cancel the row.
  const blankNewFolder = newFolderActive && newFolderName === "";
  const isReserved = newFolderName !== "" && windowsReservedBaseName.test(splitVisibleFileName(newFolderName).stem || newFolderName);
  // Conflicts are case-insensitive, matching the title-bar path contract — an occupied path is occupied
  // regardless of case (Other/PRODUCT.md blocks a move to Other/Product.md).
  const conflicts = documents.some((item) => item.id !== document.id && item.path.toLowerCase() === targetPath.toLowerCase());
  const validation = blankNewFolder
    ? "Enter a name for the new folder, or press Escape to cancel."
    : invalidNewFolder
      ? "That folder name isn't allowed — no leading “.”, no “.”/“..”, no trailing dots or spaces, and no illegal characters."
      : isReserved
        ? "That name is reserved by the operating system."
        : conflicts
          ? "A document already lives there — pick another folder, or rename it in the title bar."
          : "";
  const unchanged = targetPath === document.path;

  return (
    <Modal title="Move document" onClose={onClose}>
      <div className="form-stack move-doc">
        <div className="move-folder-tree" role="listbox" aria-label="Destination folder">
          {folders.flatMap((folder) => {
            const rows = [
              <button
                key={folder.path || "root"}
                role="option"
                aria-selected={folder.path === selectedFolder}
                className={`move-folder-row${folder.path === selectedFolder ? " selected" : ""}`}
                style={{ "--depth": String(folder.depth) } as CSSProperties}
                type="button"
                onClick={() => setSelectedFolder(folder.path)}
              >
                <Icon name="folder" />
                <span className="truncate">{folder.path === "" ? "root" : folder.name}</span>
                {folder.path === currentFolder ? <span className="tiny muted">current</span> : null}
              </button>,
            ];
            // The New Folder row is inserted directly under the selected folder — it's a name, not a written
            // folder, so it materializes only when the Move commits (Tom's fact: folders are path prefixes).
            if (newFolderActive && folder.path === selectedFolder) {
              rows.push(
                <div
                  key={(folder.path || "root") + "/__new"}
                  className="move-folder-row move-folder-new"
                  style={{ "--depth": String(folder.depth + 1) } as CSSProperties}
                >
                  <Icon name="folder" />
                  <input
                    autoFocus
                    aria-label="New folder name"
                    value={newFolder}
                    onChange={(event) => setNewFolder(event.target.value)}
                    onFocus={(event) => event.target.select()}
                    onBlur={() => {
                      // A blank row on blur collapses back to the plain selection (never left showing an empty
                      // creation row that silently means "move to parent").
                      if (newFolder.trim() === "") {
                        setNewFolderActive(false);
                        setNewFolder("");
                      }
                    }}
                    onKeyDown={(event) => {
                      if (event.key === "Escape") {
                        event.preventDefault();
                        setNewFolderActive(false);
                        setNewFolder("");
                      }
                    }}
                  />
                </div>,
              );
            }
            return rows;
          })}
        </div>
        <p className="hint">Moves to <b>{targetPath}</b>. Threads, activity, and subscriptions move with the document — they're keyed by ID, not path.</p>
        {validation ? <p className="error-text">{validation}</p> : null}
        {error ? <p className="error-text">{error}</p> : null}
        <div className="row between move-doc-actions">
          <button className="btn sm" type="button" onClick={startNewFolder} disabled={newFolderActive}>
            <Icon name="plus" />
            New Folder
          </button>
          <div className="row gap-8">
            <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
            <button
              className="btn accent"
              type="button"
              disabled={busy || !!validation || unchanged}
              onClick={async () => {
                setBusy(true);
                setError("");
                try {
                  await onMove(targetPath);
                } catch (err) {
                  setError(err instanceof Error ? err.message : String(err));
                } finally {
                  setBusy(false);
                }
              }}
            >
              Move
            </button>
          </div>
        </div>
      </div>
    </Modal>
  );
}

function DeleteDocumentModal({ document, onClose, onDelete }: { document: DocumentItem; onClose: () => void; onDelete: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const name = document.path.split("/").pop() || document.path;
  return (
    <Modal title="Delete document" onClose={onClose}>
      <div className="form-stack">
        <p>Delete <b>{name}</b> and all of its threads and activity? This can't be undone.</p>
        {error ? <p className="error-text">{error}</p> : null}
        <div className="row gap-8">
          <button className="btn full" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button
            className="btn danger full"
            type="button"
            disabled={busy}
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
        </div>
      </div>
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

export function DaemonPlatformControl({ value, onChange }: { value: DaemonInstallPlatform; onChange: (platform: DaemonInstallPlatform) => void }) {
  return (
    <div className="install-platform-control">
      <span className="lab">Operating system</span>
      <div className="platform-segments" role="group" aria-label="Local environment operating system">
        <button
          type="button"
          className={`platform-segment${value === "unix" ? " selected" : ""}`}
          aria-pressed={value === "unix"}
          onClick={() => onChange("unix")}
        >
          macOS / Linux
        </button>
        <button
          type="button"
          className={`platform-segment${value === "windows" ? " selected" : ""}`}
          aria-pressed={value === "windows"}
          onClick={() => onChange("windows")}
        >
          Windows
        </button>
      </div>
    </div>
  );
}

// The resolved state of the live desktop manifest for a platform. `idle` = no app on this
// platform (linux/unknown, no fetch); `loading` = fetch in flight; `ready` = at least one URL
// resolved; `unavailable` = fetch failed / CORS-blocked / manifest yielded no valid target.
type DesktopManifestState = {
  status: "idle" | "loading" | "ready" | "unavailable";
  urls: Partial<Record<DesktopDownloadTarget, string>>;
  macNotarized: boolean | null;
};

const DESKTOP_MANIFEST_TIMEOUT_MS = 8000;

// Fetches + validates the live R2 manifest for a platform — #61's consumer half. Bounded wait
// (AbortController + timeout), unmount-safe, and race-safe: a platform change or unmount cancels
// the in-flight request and the `active` guard drops any late/stale result. On ANY failure
// (network, CORS, non-2xx, invalid manifest) it settles to `unavailable` — never an infinite
// spinner, never a dead link. The fetch is currently expected to fail on CORS until AlphaToad
// opens GET/HEAD for app.getcodesk.com (or a same-origin proxy is chosen).
function useDesktopManifest(platform: DesktopPlatform): DesktopManifestState {
  const [state, setState] = useState<DesktopManifestState>({ status: "idle", urls: {}, macNotarized: null });
  useEffect(() => {
    if (!desktopPlatformHasApp(platform)) {
      setState({ status: "idle", urls: {}, macNotarized: null });
      return;
    }
    let active = true;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), DESKTOP_MANIFEST_TIMEOUT_MS);
    setState({ status: "loading", urls: {}, macNotarized: null });
    const manifestOS = platform === "mac" ? "macos" : "windows";
    fetch(`${desktopStaticBase}/${manifestOS}/latest/manifest.json`, { signal: controller.signal, headers: { Accept: "application/json" } })
      .then((response) => (response.ok ? response.json() : Promise.reject(new Error(`HTTP ${response.status}`))))
      .then((raw) => {
        if (!active) return;
        const resolved = resolveDesktopManifest(platform, raw, desktopStaticBase);
        const hasUrl = Object.keys(resolved.urls).length > 0;
        setState({ status: hasUrl ? "ready" : "unavailable", urls: resolved.urls, macNotarized: resolved.macNotarized });
      })
      .catch(() => {
        if (!active) return;
        setState({ status: "unavailable", urls: {}, macNotarized: null });
      })
      .finally(() => clearTimeout(timer));
    return () => {
      active = false;
      clearTimeout(timer);
      controller.abort();
    };
  }, [platform]);
  return state;
}

// The per-platform download button, resolved from the live manifest. macOS is one universal
// target; Windows publishes x64 AND ARM64 (UA can't authoritatively pick, so x64 is the default
// with an always-visible ARM64 link). A target with no resolved URL renders DISABLED — "Checking…"
// while the manifest loads, then "Desktop download temporarily unavailable" — never a dead link.
// Labels are keyed to the ACTUAL target, never assumed — the primary is UA-picked and on an ARM
// browser IS windows-arm64, so a hard-coded "x64" primary label would point ARM users at the wrong
// file. Both the button and the alternate link derive their text from whichever target they resolve.
const DESKTOP_TARGET_LABEL: Record<DesktopDownloadTarget, string> = {
  "macos-universal": "Download for Mac",
  "windows-amd64": "Download for Windows (x64)",
  "windows-arm64": "Download for Windows (ARM64)",
};
const DESKTOP_TARGET_ALT_PROMPT: Record<DesktopDownloadTarget, string> = {
  "macos-universal": "Get another build →",
  "windows-amd64": "Prefer the x64 build? Get it →",
  "windows-arm64": "On Windows on ARM? Get the ARM64 build →",
};

function DesktopDownloadButton({ platform, manifest }: { platform: DesktopPlatform; manifest: DesktopManifestState }) {
  const targets = desktopDownloadTargets(platform);
  const primary = defaultDesktopDownloadTarget(platform);
  const primaryUrl = primary ? manifest.urls[primary] ?? null : null;
  const primaryLabel = primary ? DESKTOP_TARGET_LABEL[primary] : "Download";
  const altTarget = targets.find((target) => target !== primary); // the other Windows arch
  const altUrl = altTarget ? manifest.urls[altTarget] ?? null : null;
  const loading = manifest.status === "loading";
  return (
    <div className="ds-download-btn">
      {primaryUrl ? (
        <a className="btn accent full" href={primaryUrl} download>↓ {primaryLabel}</a>
      ) : (
        <button type="button" className="btn accent full" disabled aria-disabled="true">
          {loading ? "Checking for the latest build…" : "Desktop download temporarily unavailable"}
        </button>
      )}
      {altTarget ? (
        altUrl ? (
          <a className="ds-alt-arch" href={altUrl} download>{DESKTOP_TARGET_ALT_PROMPT[altTarget]}</a>
        ) : (
          <span className="ds-alt-arch muted" aria-disabled="true">{loading ? "Checking for the other build…" : `${DESKTOP_TARGET_LABEL[altTarget]} is temporarily unavailable.`}</span>
        )
      ) : null}
    </div>
  );
}

export function CreateDaemonModal({ api, workspaceId, daemons, onClose, onDone }: { api: ApiClient; workspaceId: string; daemons: Daemon[]; onClose: () => void; onDone: () => void }) {
  // Desktop-app selection and command-line selection are separate dimensions. Linux has no GUI
  // download, so it must never appear as a peer in the desktop-app chooser; it remains available
  // in the explicitly entered command-line setup alongside macOS and Windows.
  const [detectedPlatform] = useState<DesktopPlatform>(() => detectDesktopPlatform());
  const [platform, setPlatform] = useState<DesktopPlatform>(() => (
    desktopPlatformHasApp(detectedPlatform) ? detectedPlatform : "unknown"
  ));
  // The command-line OS is a command-PRESENTATION choice, not a creation input (the token is
  // OS-independent), so it lives on the command page after Generate — never the setup page. A
  // detected platform is preselected; an unknown UA is ASKED (null) rather than guessed.
  const [commandPlatform, setCommandPlatform] = useState<Exclude<DesktopPlatform, "unknown"> | null>(() => (
    detectedPlatform === "unknown" ? null : detectedPlatform
  ));
  const [terminalMode, setTerminalMode] = useState(false);
  const [environmentName, setEnvironmentName] = useState("");
  const [nameError, setNameError] = useState("");
  const [token, setToken] = useState("");
  const [daemonId, setDaemonId] = useState("");
  const [createStatus, setCreateStatus] = useState<"idle" | "preparing" | "ready" | "unconfirmed">("idle");
  // Single-fire guard: the terminal daemon is created AT MOST ONCE per modal. POST /daemons is
  // non-idempotent with no request key, so this guard MUST survive BOTH success and an ambiguous
  // failure for the modal's lifetime — it is only ever set true, NEVER reset. Resetting it would let
  // re-entry after a failure (Back / platform switch → return) issue a second POST and duplicate the
  // record; the failure settles into the unconfirmed state with no retry instead.
  const createStartedRef = useRef(false);
  // Snapshot ALL daemon ids at open (not only the online ones) so detection recognizes a genuinely
  // NEW record — the desktop app's own daemon, or this modal's terminal daemon, both created after
  // open — and NEVER a pre-existing daemon that was merely OFFLINE at open and later reconnects.
  const [initialDaemonIds] = useState(() => new Set(daemons.map((d) => d.id)));

  const manifest = useDesktopManifest(platform); // live R2 URLs + macOS notarization state
  const isUnknown = platform === "unknown";
  const installTarget = commandPlatform ? desktopPlatformInstallTarget(commandPlatform) : null;
  // Built ONLY from a real token AND a chosen OS — never a placeholder, and nothing until the user
  // has picked the OS (an unknown UA is asked, never guessed).
  const command = token && installTarget ? buildDaemonInstallCommand({ backendUrl: apiBase, workspaceId, daemonToken: token, staticBaseUrl: daemonStaticBase, platform: installTarget }) : "";

  const selectPlatform = (next: DesktopPlatform) => {
    setPlatform(next);
  };

  // Provision the command-line daemon only after the named form is deliberately submitted.
  // Opening the panel and switching operating systems are reversible navigation and issue no POST.
  const runTerminalCreate = useCallback(async (name: string) => {
    if (createStartedRef.current) return;
    createStartedRef.current = true;
    setCreateStatus("preparing");
    try {
      const response = await api.createDaemon(workspaceId, name);
      setDaemonId(response.daemon.id);
      setToken(response.token);
      setCreateStatus("ready");
      onDone();
    } catch {
      // POST /daemons is non-idempotent and carries no request key (backend mints a fresh UUID +
      // one-time token per call), so on ANY failure we cannot know whether the record committed
      // before the response was lost. A blind retry could duplicate it, and the one-time token is
      // gone with the lost response — there is nothing recoverable to retry toward. So we refresh
      // the list ONCE (any committed record surfaces in Local environments) and settle into a single
      // honest "unconfirmed" state: no command, no copy, no retry. The guard stays set — no re-fire.
      // (A future idempotency key + recoverable token is the real fix; ledgered, not this round.)
      onDone();
      setCreateStatus("unconfirmed");
    }
  }, [api, workspaceId, onDone]);

  const generateInstallCommand = (event: FormEvent) => {
    event.preventDefault();
    const name = environmentName.trim();
    if (!name) {
      setNameError("Enter a name for this local environment.");
      return;
    }
    setNameError("");
    void runTerminalCreate(name);
  };

  // Connected is LIVE-DERIVED from a real check-in, never a Download click. It accepts this flow's
  // owned terminal record OR a genuinely NEW record (id unseen at open) coming online — so
  // terminal→Back→app and Linux→mac/win→app recognize the desktop app's own daemon, while a
  // pre-existing daemon that was offline at open and merely reconnects is NOT mistaken for it.
  const connectedDaemon =
    daemons.find((d) => d.id === daemonId && daemonStatus(d) === "online")
    ?? daemons.find((d) => daemonStatus(d) === "online" && !initialDaemonIds.has(d.id));
  const connected = Boolean(connectedDaemon);
  const terminalDaemon = daemonId ? daemons.find((d) => d.id === daemonId) : undefined;
  const terminalFailed = terminalDaemon ? isDaemonOffline(terminalDaemon) && hasGenuineCheckIn(terminalDaemon) : false;

  const desktopPlatformSelector = (
    <div className="ds-platform-select" role="group" aria-label="Which computer are you connecting?">
      {(["mac", "windows"] as const).map((p) => (
        <button key={p} type="button" className={`ds-platform-pill${platform === p ? " selected" : ""}`} aria-pressed={platform === p} onClick={() => selectPlatform(p)}>
          {p === "mac" ? "Mac" : "Win"}
        </button>
      ))}
    </div>
  );
  const commandPlatformSelector = (
    // aria-describedby links this group to the "Choose the OS…" required-choice prompt so SR users
    // hear the requirement on focus — but ONLY while that prompt exists (none selected / no command
    // yet). Once an OS is picked the prompt is gone, so the reference must drop or it would dangle.
    <div className="ds-platform-select" role="group" aria-label="Command-line operating system" aria-describedby={!command ? "command-os-hint" : undefined}>
      {(["mac", "linux", "windows"] as const).map((p) => (
        <button
          key={p}
          type="button"
          className={`ds-platform-pill${commandPlatform === p ? " selected" : ""}`}
          aria-pressed={commandPlatform === p}
          onClick={() => setCommandPlatform(p)}
        >
          {p === "mac" ? "macOS" : p === "windows" ? "Windows" : "Linux"}
        </button>
      ))}
    </div>
  );

  return (
    <Modal title={terminalMode ? "Set up from the command line" : "Connect this workspace to your computer"} onClose={onClose}>
      {connected ? (
        <div className="ds-connected">
          <div className="ds-connected-card">
            <span className="ds-check" aria-hidden="true">✓</span>
            <div><strong>Local environment connected</strong><span className="small muted">Checked in just now</span></div>
          </div>
          <p className="chip online"><StatusDot tone="online" />Connected. You can create an agent now.</p>
          <div className="row end"><button className="btn accent" onClick={onClose}>Done</button></div>
        </div>
      ) : terminalMode ? (
        <div className="ds-terminal">
          <p className="small muted">For servers and headless computers.</p>
          {/* No OS control on the setup page — the OS is a command-format choice that only matters
              AFTER a token exists, so it lives on the command view below. */}
          <form className="ds-command-form" onSubmit={generateInstallCommand}>
            <label className="field">
              <span className="lab">Local environment name</span>
              <input
                aria-label="Local environment name"
                value={environmentName}
                placeholder="Build server"
                onChange={(event) => {
                  setEnvironmentName(event.target.value);
                  if (nameError) setNameError("");
                }}
                aria-invalid={Boolean(nameError)}
                aria-describedby={nameError ? "daemon-name-error" : undefined}
                disabled={createStartedRef.current}
              />
              <span className="hint">Name it before we create it — this is what you'll see in Local environments.</span>
            </label>
            {/* role=alert (assertive) announces this submit-blocking validation error immediately —
                the user is waiting on the submit result, so it must not queue behind other speech. */}
            {nameError ? <p id="daemon-name-error" className="error-text" role="alert" aria-live="assertive">{nameError}</p> : null}
            {createStatus === "idle" ? (
              <>
                <p className="tiny muted">Nothing is created until you generate the install command — opening this panel makes no changes.</p>
                <button type="submit" className="btn accent full">Generate install command</button>
              </>
            ) : null}
          </form>
          {createStatus === "unconfirmed" ? (
            // Non-idempotent create failed ambiguously — one honest state, no command, no copy, no
            // blind Retry (which could duplicate). The user closes and verifies in Local environments.
            <div className="ds-terminal-unconfirmed">
              {/* This multi-sentence recovery copy must WRAP — the global `.chip` is a nowrap pill and
                  clips it at 390px, truncating the honest failure message exactly where honesty matters.
                  Fix lives on this element, not on global `.chip`. */}
              <p className="ds-unconfirmed-msg"><StatusDot tone="stale" />We lost contact while creating this environment. It may already exist. Close this dialog and check Local environments before trying again.</p>
            </div>
          ) : createStatus === "ready" ? (
            <div className="ds-command-result">
              {/* The OS is picked HERE, on the command view — it re-formats the SAME token's command
                  (shell vs Windows PowerShell) and issues NO POST. A detected UA is preselected; an
                  unknown UA has none preselected and must choose before any command appears. */}
              {commandPlatformSelector}
              {command ? (
                <>
                  <ShellScriptBlock title="Install command" badge={installTarget === "windows" ? "PowerShell" : "Shell"} command={command} />
                  {terminalFailed ? (
                    <p className="chip"><StatusDot tone="stale" />No connection yet. Make sure the command ran completely.</p>
                  ) : (
                    <p className="chip"><StatusDot tone="stale" />Install command ready — Codesk detects the connection automatically.</p>
                  )}
                </>
              ) : (
                <p className="small muted ds-pick-os" id="command-os-hint">Choose the OS you'll run this on to see the install command.</p>
              )}
            </div>
          ) : createStatus === "preparing" ? (
            // Preparing: a real token doesn't exist yet — show no command and nothing copyable.
            <p className="small muted ds-preparing">Preparing your install command…</p>
          ) : null}
          <button type="button" className="ds-back" onClick={() => setTerminalMode(false)}>← Back to desktop app downloads</button>
        </div>
      ) : isUnknown ? (
        <div className="ds-chooser">
          <p className="small muted">Which computer are you connecting?</p>
          <button type="button" className="ds-choice" onClick={() => selectPlatform("mac")}>
            <span className="ds-choice-icon" aria-hidden="true">↓</span>
            <span className="ds-choice-text"><strong>Mac</strong><span className="small muted">Download the app</span></span>
            <span className="ds-choice-chev" aria-hidden="true">›</span>
          </button>
          <button type="button" className="ds-choice" onClick={() => selectPlatform("windows")}>
            <span className="ds-choice-icon" aria-hidden="true">↓</span>
            <span className="ds-choice-text"><strong>Windows</strong><span className="small muted">Download the .msi</span></span>
            <span className="ds-choice-chev" aria-hidden="true">›</span>
          </button>
          <div className="ds-headless-entry">
            <span className="small muted">Running on a server or headless computer? Set up Codesk from the command line.</span>
            <button type="button" className="ds-terminal-link" onClick={() => setTerminalMode(true)}>Use command line setup</button>
          </div>
        </div>
      ) : (
        <div className="ds-download">
          <div className="ds-app-card">
            <span className="ds-app-icon" aria-hidden="true">↓</span>
            <div><strong>{platform === "mac" ? "Codesk for Mac" : "Codesk for Windows"}</strong><span className="small muted">{platform === "mac" ? "macOS · .dmg" : "Windows · .msi"}</span></div>
          </div>
          <DesktopDownloadButton platform={platform} manifest={manifest} />
          <ol className="ds-steps">
            <li>{platform === "mac" ? "Open the .dmg and drag Codesk to Applications, then open it." : "Run the .msi installer, then open Codesk."}</li>
            <li>Codesk opens a browser page — choose this workspace and select Connect.</li>
            <li>This page updates when the app connects.</li>
          </ol>
          {/* First-run guidance is behavior-only by default (macOS "may warn" / Windows SmartScreen).
              The macOS notarization CLAIM is added ONLY when the live manifest's signed_and_notarized
              is explicitly false — never invented. The Windows manifest carries no signing metadata,
              so we never assert its signing state. */}
          <p className="ds-firstrun small muted">
            {platform === "mac"
              ? `First open: macOS may warn about an unidentified developer — right-click Codesk and choose Open to confirm.${manifest.macNotarized === false ? " (This build isn't notarized yet.)" : ""}`
              : "First run: Windows may show a SmartScreen notice — choose More info, then Run anyway."}
          </p>
          <p className="small muted">No terminal commands or access tokens to copy.</p>
          <p className="chip"><StatusDot tone="stale" />Waiting for Codesk to connect…</p>
          <div className="ds-download-foot">
            <div className="ds-headless-entry">
              <span className="small muted">Running on a server or headless computer? Set up Codesk from the command line.</span>
              <button type="button" className="ds-terminal-link" onClick={() => setTerminalMode(true)}>Use command line setup</button>
            </div>
            {desktopPlatformSelector}
          </div>
        </div>
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
    <Modal title="Create an agent that stays with the project" onClose={onClose} wide>
      <form className="new-agent-form" onSubmit={submit}>
        <p className="small muted new-agent-lead">An agent isn't a one-off chat. It has its own name, role, and environment, and keeps working in this workspace over time.</p>
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
              <span className="hint">Give it a name that's easy to recognize.</span>
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
            <label className="field"><span className="lab">Role</span><textarea value={role} placeholder={rolePlaceholder} onChange={(event) => setRole(event.target.value)} required /><span className="hint">Describe what it owns, how it knows the work is done, and when it should check with a person.</span></label>
            <div className="divider" />
            <div className="field">
              <span className="lab">Owning local environment</span>
              <span className="hint">The agent runs in the local environment you choose.</span>
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

export function AgentDetailModal({ api, workspaceId, agentId, agents, daemons, runs, onClose, onChanged }: { api: ApiClient; workspaceId: string; agentId: string; agents: Agent[]; daemons: Daemon[]; runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"]; onClose: () => void; onChanged: () => void }) {
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
  const status = visibleAgentStatus(agent, runs, daemons, now);
  const statusLabel = detailStatusLabel(status);
  return (
    <Modal title={`@${agent.handle}`} onClose={onClose}>
      <div className="form-stack">
        <div className="modal-identity">
          <div className="avi agent">{initials(agent.handle)}</div>
          <div className="col gap-2">
            <span className={`chip sm ${status.tone}`} title={status.title}><StatusDot tone={status.tone} /> {status.label}</span>
            {statusLabel !== status.label ? <span className="small muted status-detail">{statusLabel}</span> : null}
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

export function DaemonDetailModal({ api, workspaceId, daemonId, daemons, agents, runs, onClose, onChanged }: { api: ApiClient; workspaceId: string; daemonId: string; daemons: Daemon[]; agents: Agent[]; runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"]; onClose: () => void; onChanged: () => void }) {
  const initialDaemonOS = daemons.find((item) => item.id === daemonId && item.status !== "deleted")?.os;
  const now = useNowTicker(DAEMON_LIVENESS_TICK_MS);
  const [reinstallOpen, setReinstallOpen] = useState(false);
  const [reinstallToken, setReinstallToken] = useState("");
  const [reinstallError, setReinstallError] = useState("");
  const [reinstallLoading, setReinstallLoading] = useState(false);
  const [deleteConfirmOpen, setDeleteConfirmOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState("");
  const [installPlatform, setInstallPlatform] = useState<DaemonInstallPlatform>(() => defaultDaemonInstallPlatform(initialDaemonOS));
  // #63 uninstall is honest by INSTALL METHOD, and the record never stores how it was installed —
  // the desktop app is new, so many existing mac/win environments were terminal-installed. So
  // mac/win open on a neutral question with NO pre-selection (Anton's no-guessed-default rule);
  // Linux/unknown have no app → terminal-only, question skipped.
  const [installMethod, setInstallMethod] = useState<"app" | "terminal" | null>(
    () => (desktopPlatformHasApp(daemonDesktopPlatform(initialDaemonOS)) ? null : "terminal")
  );
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
  useEffect(() => {
    if (daemon?.os) {
      setInstallPlatform(defaultDaemonInstallPlatform(daemon.os));
    }
  }, [daemon?.os]);
  // Derived from the stable initial OS so the manifest hook runs unconditionally BEFORE the early
  // return below (hooks can't sit after a conditional return); a connected daemon's OS is stable.
  const deskPlatform = daemonDesktopPlatform(initialDaemonOS);
  const manifest = useDesktopManifest(deskPlatform);
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
    platform: installPlatform,
  }) : "";
  const uninstallCommand = buildDaemonUninstallCommand({
    staticBaseUrl: daemonStaticBase,
    platform: installPlatform,
  });
  const hasApp = desktopPlatformHasApp(deskPlatform);
  // App-path reinstall = re-download the app, through the SAME live-manifest resolver as install →
  // an unresolved target stays disabled-honest until the R2 manifest is browser-readable (CORS),
  // never a dead link.
  // Use the DAEMON's real architecture, not the browser's — a Windows ARM64 daemon managed from an
  // x64/mac browser must get the ARM64 MSI, and an unrecognized arch fails closed (no wrong build).
  const appReinstallTarget = daemonDownloadTarget(deskPlatform, daemon.arch);
  const appReinstallUrl = appReinstallTarget ? manifest.urls[appReinstallTarget] ?? null : null;
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
            {/* One predicate for "did this daemon ever check in": hasGenuineCheckIn (year-2020 gate).
                A truthy check swallows Go's zero time (0001-01-01 is a non-empty string), rendering a
                fake "1/1/1" date for a never-checked-in orphan — dishonest exactly in the failure-
                recovery path. A genuine receipt still shows its real date. */}
            <p className="small muted">Last seen: {hasGenuineCheckIn(daemon) ? new Date(daemon.lastSeenAt!).toLocaleString() : "Never"}</p>
            <p className="small muted">Agents: {daemonAgents.length}</p>
            {daemonAgents.map((agent) => {
              const agentStatus = visibleAgentStatus(agent, runs, [daemon], now);
              return <p className="small" key={agent.id}>@{agent.handle} · {detailStatusLabel(agentStatus)}</p>;
            })}
          </div>

          {/* Honest by install method: the record never stores HOW it was installed and — unlike
              platform — there's no truthful signal, so mac/win ask with NO guessed default; the
              switcher stays for correction. Linux/unknown have no app → terminal-only, no question. */}
          {hasApp ? (
            <div className="ds-method">
              <p className="toglab">How did you install Codesk on this computer?</p>
              <div className="ds-method-toggle" role="group" aria-label="Install method">
                <button type="button" className={`ds-method-opt${installMethod === "app" ? " on" : ""}`} aria-pressed={installMethod === "app"} onClick={() => setInstallMethod("app")}>Desktop app</button>
                <button type="button" className={`ds-method-opt${installMethod === "terminal" ? " on" : ""}`} aria-pressed={installMethod === "terminal"} onClick={() => setInstallMethod("terminal")}>Terminal</button>
              </div>
            </div>
          ) : null}

          {installMethod === null ? (
            <p className="small muted">Pick one to see the matching Reinstall and Uninstall steps. We don't guess — the desktop app is new, so many Macs and PCs were set up from the terminal.</p>
          ) : installMethod === "app" ? (
            <>
              {appReinstallUrl ? (
                <a className="btn full" href={appReinstallUrl} download>Reinstall — re-download the app</a>
              ) : (
                <button type="button" className="btn full" disabled aria-disabled="true">Reinstall — re-download temporarily unavailable</button>
              )}
              <div className="ds-uninstall-app">
                <p className="uh">Uninstall the Codesk app</p>
                <ol className="ds-osteps">
                  {deskPlatform === "windows" ? (
                    <>
                      <li>Quit Codesk from the system tray (right-click → Quit).</li>
                      <li>Settings → Apps → Codesk → Uninstall.</li>
                    </>
                  ) : (
                    <>
                      <li>Quit Codesk — click the menu-bar icon and choose Quit.</li>
                      <li>Open Applications and move Codesk to the Trash.</li>
                    </>
                  )}
                </ol>
                <p className="small muted">
                  {deskPlatform === "windows"
                    ? "The installer registered a standard Windows uninstaller — no command needed."
                    : "This removes the app and its background sync. To also remove it from Codesk, use “Delete record” below."}
                </p>
              </div>
            </>
          ) : (
            <>
              <button className="btn accent full" onClick={() => void prepareReinstall()} disabled={reinstallLoading}>Reinstall — run the reinstall script</button>
              <DaemonPlatformControl value={installPlatform} onChange={setInstallPlatform} />
              <ShellScriptBlock title="Uninstall local environment" badge={installPlatform === "windows" ? "PowerShell" : "Shell"} command={uninstallCommand}>
                <p className="small muted">This script is computer-wide — it may stop every Codesk environment and agent on this machine. It's the global uninstall script; workspace-specific uninstall isn't supported yet.</p>
              </ShellScriptBlock>
            </>
          )}

          {/* Orthogonal to uninstall — removes the Codesk-side record only, never touches the machine.
              Disclaimer is method-agnostic: "Codesk software" holds for both the GUI app and the CLI. */}
          <div className="ds-delete-record">
            <button className="btn danger full" onClick={() => { setDeleteError(""); setDeleteConfirmOpen(true); }}>Delete local environment record</button>
            <p className="tiny muted">Removes this environment from Codesk — it does not remove the Codesk software from your computer.</p>
          </div>
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
              <>
                <DaemonPlatformControl value={installPlatform} onChange={setInstallPlatform} />
                <ShellScriptBlock title="Reinstall local environment" badge={installPlatform === "windows" ? "PowerShell" : "Shell"} command={reinstallCommand}>
                  <p className="small muted">Run this on the local environment host to remove the current local install and install the latest local environment again using the fresh token below.</p>
                </ShellScriptBlock>
              </>
            )}
          </div>
        </Modal>
      ) : null}
      {deleteConfirmOpen ? (
        <Modal title="Delete local environment record" onClose={() => { if (!deleting) setDeleteConfirmOpen(false); }}>
          <div className="form-stack">
            <p className="small">Delete this local environment record? Removes “{daemon.name}” from Codesk.</p>
            <p className="small muted">This will not remove the Codesk software on your computer — to uninstall the software, use the steps above.</p>
            {deleteError ? <p className="error-text">{deleteError}</p> : null}
            <div className="row end gap-8">
              <button type="button" className="btn" disabled={deleting} onClick={() => setDeleteConfirmOpen(false)}>Cancel</button>
              <button
                type="button"
                className="btn danger"
                disabled={deleting}
                onClick={async () => {
                  setDeleting(true);
                  setDeleteError("");
                  try {
                    await api.deleteDaemon(workspaceId, daemon.id);
                    onChanged();
                    onClose();
                  } catch (error) {
                    // Explicit failure, never silent — the record still exists, so let the user retry.
                    setDeleteError(error instanceof Error ? error.message : "Couldn't delete the record. Try again.");
                    setDeleting(false);
                  }
                }}
              >
                {deleting ? "Deleting…" : "Delete record"}
              </button>
            </div>
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
          <button className="card p-20 empty-choice" data-onboarding-id="connect-local-env" onClick={onCreateDaemon}>
            <div className="row gap-8"><div className="avi sm daemon">D</div><b>Deploy a local environment</b></div>
            <span className="small muted">Bring docs to local disk and enable agents.</span>
          </button>
          <button className="card p-20 empty-choice" data-onboarding-id="create-document" onClick={onCreateDocument} disabled={!canCreateDocument || creatingDocument}>
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
  onMemberInvited,
}: {
  api: ApiClient;
  workspaceId: string;
  users: UserItem[];
  canInvite: boolean;
  onMemberInvited?: () => void;
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
      onMemberInvited?.();
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

function WorkspaceSettings({
  api,
  workspaceId,
  workspace,
  onSaved,
}: {
  api: ApiClient;
  workspaceId: string;
  workspace: ReturnType<typeof useWorkspace>["workspace"];
  onSaved: (workspace: WorkspaceSummary) => void;
}) {
  const workspaceUrl = `${publicOrigin}/w/${workspace.slug}`;
  const role = workspace.currentMembershipRole || "";
  const canManage = role === "owner" || role === "admin";
  const [nameDraft, setNameDraft] = useState(workspace.name);
  const [saving, setSaving] = useState(false);
  const [copied, setCopied] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    setNameDraft(workspace.name);
    setCopied(false);
    setMessage("");
    setError("");
  }, [workspace.name, workspace.slug]);

  const trimmedName = nameDraft.trim();
  const nameChanged = trimmedName !== workspace.name.trim();
  const saveDisabled = saving || !canManage || !nameChanged || !trimmedName;

  const copyWorkspaceUrl = async () => {
    setError("");
    try {
      await navigator.clipboard.writeText(workspaceUrl);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (saveDisabled) {
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const response = await api.updateWorkspaceSettings(workspaceId, { name: trimmedName });
      onSaved(response.workspace);
      setNameDraft(response.workspace.name);
      setMessage("Workspace settings saved.");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
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
        <div className="setting-readonly workspace-url-setting">
          <div className="workspace-url-label">
            <span className="lab">Workspace URL</span>
            <span className="tiny muted">Permanent</span>
          </div>
          <div className="workspace-url-value-row">
            <span className="setting-value mono workspace-url-value">{workspaceUrl}</span>
            <button
              className="btn sm"
              type="button"
              aria-label={copied ? "Workspace URL copied" : "Copy workspace URL"}
              onClick={() => void copyWorkspaceUrl()}
            >
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
          <p className="tiny muted">This URL is permanent and cannot be changed.</p>
        </div>
        <p className="tiny muted">
          {canManage
            ? "Owners and admins can rename the workspace."
            : "Only workspace owners and admins can edit workspace settings."}
        </p>
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
  onMemberInvited,
}: {
  api: ApiClient;
  workspaceId: string;
  workspace: ReturnType<typeof useWorkspace>["workspace"];
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
  onMemberInvited?: () => void;
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
              <MembersAndInvite api={api} workspaceId={workspaceId} users={workspace.users} canInvite={canInvite} onMemberInvited={onMemberInvited} />
            ) : activeTab === "agents" ? (
              <AgentsManagement workspace={workspace} groupedAgents={groupedAgents} onNew={onNewAgent} onAgent={onAgent} />
            ) : activeTab === "workspace" ? (
              <WorkspaceSettings
                api={api}
                workspaceId={workspaceId}
                workspace={workspace}
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
    case "mention":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="4" /><path d="M16 8v5a3 3 0 0 0 6 0v-1a10 10 0 1 0-4 8" /></svg>;
    case "alert":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M12 3l10 18H2L12 3z" /><path d="M12 9v5M12 18h.01" /></svg>;
    case "thread":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5A8.5 8.5 0 1 1 21 11.5z" /></svg>;
    case "message":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M4 5h16a1.7 1.7 0 0 1 1.7 1.7v8.6A1.7 1.7 0 0 1 20 17H10l-4.3 3.4V17H4a1.7 1.7 0 0 1-1.7-1.7V6.7A1.7 1.7 0 0 1 4 5Z" /></svg>;
    case "people":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" /><circle cx="9" cy="7" r="4" /></svg>;
    case "users":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="9" cy="8" r="3.4" /><path d="M2.8 20c0-3.6 2.8-5.8 6.2-5.8s6.2 2.2 6.2 5.8" /><path d="M15.8 4.9a3.4 3.4 0 0 1 0 6.2" /><path d="M17 14.4c2.9.4 4.6 2.6 4.6 5.6" /></svg>;
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
    case "folder":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M3 8V6.5A1.5 1.5 0 0 1 4.5 5H9l2 2h8A1.5 1.5 0 0 1 20.5 8.5V18A1.5 1.5 0 0 1 19 19.5H4.5A1.5 1.5 0 0 1 3 18Z" /></svg>;
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
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="4.5" cy="12" r="1.9" fill="currentColor" stroke="none" /><circle cx="12" cy="12" r="1.9" fill="currentColor" stroke="none" /><circle cx="19.5" cy="12" r="1.9" fill="currentColor" stroke="none" /></svg>;
    case "move":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M3 8V6.5A1.5 1.5 0 0 1 4.5 5H9l2 2h8A1.5 1.5 0 0 1 20.5 8.5V18A1.5 1.5 0 0 1 19 19.5H4.5A1.5 1.5 0 0 1 3 18Z" /><path d="M7.5 12.25h6" /><path d="M11 9.75 13.5 12.25 11 14.75" /></svg>;
    case "trash":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M3.5 6.5h17" /><path d="M8.5 6.5V5a1.5 1.5 0 0 1 1.5-1.5h4A1.5 1.5 0 0 1 15.5 5v1.5" /><path d="M18.5 6.5l-.9 13a2 2 0 0 1-2 1.9H8.4a2 2 0 0 1-2-1.9l-.9-13" /><path d="M10 10.5v6.5M14 10.5v6.5" /></svg>;
    case "settings":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M4.2 4.2l2.1 2.1M17.7 17.7l2.1 2.1M2 12h3M19 12h3M4.2 19.8l2.1-2.1M17.7 6.3l2.1-2.1" /></svg>;
    default:
      return null;
  }
}
