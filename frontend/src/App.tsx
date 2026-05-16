import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ApiClient, apiBase, daemonStaticBase } from "./api";
import { DocumentSurface, type LiveThread, type SurfaceSelection } from "./DocumentSurface";
import {
  agentStatus,
  agentsByDaemon,
  buildDaemonInstallCommand,
  buildDaemonUninstallCommand,
  daemonStatus,
  type LineThreadGroup,
} from "./logic";
import { useDocumentSync } from "./useDocument";
import { useWorkspace } from "./useWorkspace";
import type { Account, Agent, Daemon, DocumentItem, ThreadItem, WorkspaceSummary } from "./types";
import "./styles.css";

const tokenStorageKey = "notty.auth.token";
const workspaceStorageKey = "notty.workspace.id";

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

function folderName(path?: string) {
  if (!path || !path.includes("/")) {
    return "Workspace";
  }
  return path.split("/").filter(Boolean)[0] || "Workspace";
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

function byFolder(documents: DocumentItem[]) {
  const groups = new Map<string, DocumentItem[]>();
  for (const document of documents) {
    const group = folderName(document.path);
    groups.set(group, [...(groups.get(group) ?? []), document]);
  }
  return Array.from(groups.entries()).sort(([left], [right]) => left.localeCompare(right));
}

function visibleAgentStatus(agent: Agent, runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"], daemons: Daemon[]) {
  const daemon = daemons.find((item) => item.id === agent.daemonId);
  const owningDaemonStatus = daemon ? daemonStatus(daemon) : "disconnected";
  if (owningDaemonStatus === "disconnected" || owningDaemonStatus === "deleted") {
    return "disconnected";
  }
  return agentStatus(agent, runs);
}

function clamp(value: number, min: number, max: number) {
  return Math.min(Math.max(value, min), max);
}

export function App() {
  const [token, setToken] = useState(() => localStorage.getItem(tokenStorageKey) ?? "");
  const [account, setAccount] = useState<Account | null>(null);
  const [workspaces, setWorkspaces] = useState<WorkspaceSummary[]>([]);
  const [workspaceId, setWorkspaceId] = useState(() => localStorage.getItem(workspaceStorageKey) ?? "");
  const [restoringSession, setRestoringSession] = useState(false);
  const api = useMemo(() => new ApiClient(token), [token]);

  const saveAuth = (nextToken: string, nextAccount: Account, nextWorkspaces: WorkspaceSummary[] = []) => {
    const safeWorkspaces = Array.isArray(nextWorkspaces) ? nextWorkspaces : [];
    localStorage.setItem(tokenStorageKey, nextToken);
    setToken(nextToken);
    setAccount(nextAccount);
    setWorkspaces(safeWorkspaces);
    const preferred = localStorage.getItem(workspaceStorageKey);
    setWorkspaceId(preferred && safeWorkspaces.some((workspace) => workspace.id === preferred) ? preferred : safeWorkspaces[0]?.id ?? "");
  };

  const signOut = () => {
    localStorage.removeItem(tokenStorageKey);
    localStorage.removeItem(workspaceStorageKey);
    setToken("");
    setAccount(null);
    setWorkspaces([]);
    setWorkspaceId("");
  };

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
        setAccount(response.account);
        setWorkspaces(safeWorkspaces);
        const preferred = localStorage.getItem(workspaceStorageKey);
        if (preferred && safeWorkspaces.some((workspace) => workspace.id === preferred)) {
          setWorkspaceId(preferred);
        } else {
          localStorage.removeItem(workspaceStorageKey);
          setWorkspaceId(safeWorkspaces[0]?.id ?? "");
        }
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

  if (!token) {
    return <AuthScreen api={api} onAuth={saveAuth} />;
  }

  if (restoringSession && !account) {
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

  if (!workspaceId) {
    return (
      <WorkspacePicker
        api={api}
        account={account}
        workspaces={workspaces}
        onWorkspaces={setWorkspaces}
        onSelect={(id) => {
          localStorage.setItem(workspaceStorageKey, id);
          setWorkspaceId(id);
        }}
        onSignOut={signOut}
      />
    );
  }

  return (
    <WorkspaceApp
      api={api}
      token={token}
      workspaceId={workspaceId}
      account={account}
      workspaces={workspaces}
      onWorkspaceChange={(id) => {
        localStorage.setItem(workspaceStorageKey, id);
        setWorkspaceId(id);
      }}
      onSignOut={signOut}
    />
  );
}

function AuthScreen({ api, onAuth }: { api: ApiClient; onAuth: (token: string, account: Account, workspaces: WorkspaceSummary[]) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [displayName, setDisplayName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const response =
        mode === "register"
          ? await api.register({ email, password, displayName })
          : await api.login({ email, password });
      onAuth(response.token, response.account, response.workspaces ?? []);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="auth-screen">
      <section className="card p-24 auth-panel">
        <Logo />
        <h1 className="auth-title">{mode === "login" ? "Welcome back" : "Create your account"}</h1>
        <p className="small muted auth-copy">
          {mode === "login" ? "Log in to your workspaces." : "You'll set up your workspace next."}
        </p>
        <form onSubmit={submit} className="form-stack">
          {mode === "register" ? (
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
          <button className="btn accent full lg" disabled={busy}>{busy ? "Working..." : mode === "login" ? "Log in" : "Create account"}</button>
        </form>
        <div className="auth-switch">
          {mode === "login" ? "No account?" : "Already have an account?"}
          <button className="btn-link" onClick={() => setMode(mode === "login" ? "register" : "login")}>
            {mode === "login" ? "Create one" : "Log in"}
          </button>
        </div>
      </section>
    </main>
  );
}

function WorkspacePicker({
  api,
  account,
  workspaces,
  onWorkspaces,
  onSelect,
  onSignOut,
}: {
  api: ApiClient;
  account: Account | null;
  workspaces: WorkspaceSummary[];
  onWorkspaces: (workspaces: WorkspaceSummary[]) => void;
  onSelect: (id: string) => void;
  onSignOut: () => void;
}) {
  const [name, setName] = useState("Product Workspace");
  const [handle, setHandle] = useState(account?.email.split("@")[0] ?? "owner");
  const [error, setError] = useState("");

  const createWorkspace = async (event: FormEvent) => {
    event.preventDefault();
    setError("");
    try {
      const response = await api.createWorkspace({ name, handle });
      const next = [...workspaces, response.workspace];
      onWorkspaces(next);
      onSelect(response.workspace.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <main className="auth-screen picker-screen">
      <section className="card p-24 picker-panel">
        <div className="row between gap-12">
          <Logo />
          <button className="btn ghost sm" onClick={onSignOut}>Sign out</button>
        </div>
        <div className="picker-head">
          <h1 className="auth-title">Choose a workspace</h1>
          <p className="small muted">Pick a tenant or create a new workspace-scoped home for documents, daemons, and agents.</p>
        </div>
        {workspaces.length ? (
          <div className="workspace-grid">
            {workspaces.map((workspace) => (
              <button className="workspace-tile card" key={workspace.id} onClick={() => onSelect(workspace.id)}>
                <div className="avi">{initials(workspace.name)}</div>
                <div className="col gap-2">
                  <strong className="small">{workspace.name}</strong>
                  <span className="tiny muted">{workspace.slug}</span>
                </div>
              </button>
            ))}
          </div>
        ) : null}
        <form onSubmit={createWorkspace} className="form-stack create-workspace-card">
          <div>
            <h2 className="modal-title">Create workspace</h2>
            <p className="small muted">You will become the workspace owner.</p>
          </div>
          <label className="field">
            <span className="lab">Workspace name</span>
            <input value={name} onChange={(event) => setName(event.target.value)} required />
          </label>
          <label className="field">
            <span className="lab">Your workspace handle</span>
            <input value={handle} onChange={(event) => setHandle(event.target.value)} required />
          </label>
          {error ? <p className="error-text">{error}</p> : null}
          <button className="btn accent full lg">Create and enter</button>
        </form>
      </section>
    </main>
  );
}

function WorkspaceApp({
  api,
  token,
  workspaceId,
  account,
  workspaces,
  onWorkspaceChange,
  onSignOut,
}: {
  api: ApiClient;
  token: string;
  workspaceId: string;
  account: Account | null;
  workspaces: WorkspaceSummary[];
  onWorkspaceChange: (id: string) => void;
  onSignOut: () => void;
}) {
  const { workspace, connected, loading, error, reload } = useWorkspace(workspaceId, token);
  const [activeDocumentId, setActiveDocumentId] = useState("");
  const [centerView, setCenterView] = useState<"document" | "daemons" | "agents">("document");
  const [rightTab, setRightTab] = useState<"threads" | "activity" | "people">("threads");
  const [modal, setModal] = useState<"daemon" | "agent" | "document" | "rename" | "agent-detail" | "daemon-detail" | null>(null);
  const [selectedAgent, setSelectedAgent] = useState<Agent | null>(null);
  const [selectedDaemon, setSelectedDaemon] = useState<Daemon | null>(null);
  const [selectedThreadId, setSelectedThreadId] = useState("");
  const [focusThreadId, setFocusThreadId] = useState("");

  const activeDocument = workspace.documents.find((document) => document.id === activeDocumentId) ?? workspace.documents[0] ?? null;
  const documentId = activeDocument?.id ?? "";
  useEffect(() => {
    if (documentId && documentId !== activeDocumentId) {
      setActiveDocumentId(documentId);
    }
  }, [activeDocumentId, documentId]);

  const documentThreads = workspace.threads.filter((thread) => thread.documentId === activeDocument?.id);
  const groupedAgents = agentsByDaemon(workspace.agents, workspace.daemons);
  const documentGroups = byFolder(workspace.documents);
  const activeFolder = folderName(activeDocument?.path);
  const activeWorkspace = workspaces.find((item) => item.id === workspaceId);
  const onlineAgents = workspace.agents.filter((agent) => visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons) !== "disconnected").length;

  useEffect(() => {
    if (selectedThreadId && !documentThreads.some((thread) => thread.id === selectedThreadId)) {
      setSelectedThreadId("");
    }
  }, [documentThreads, selectedThreadId]);

  return (
    <main className={`shell ${centerView === "document" ? "" : "management-shell"}`}>
      <aside className="sb">
        <div className="workspace-switcher">
          <div className="row gap-8 min-0">
            <div className="avi workspace-avi">{initials(activeWorkspace?.name ?? workspace.name)}</div>
            <div className="col gap-0 min-0">
              <b className="small truncate">{workspace.name || activeWorkspace?.name || "Workspace"}</b>
              <span className="tiny muted truncate">@{account?.email.split("@")[0] ?? "you"}</span>
            </div>
          </div>
          <select aria-label="Workspace" value={workspaceId} onChange={(event) => onWorkspaceChange(event.target.value)}>
            {workspaces.map((workspace) => (
              <option value={workspace.id} key={workspace.id}>{workspace.name}</option>
            ))}
          </select>
        </div>

        <div className="sb-search">
          <div className="input search-box">
            <div className="row gap-6">
              <Icon name="search" />
              <span>Search or jump…</span>
            </div>
            <span className="kbd">⌘K</span>
          </div>
        </div>

        <nav className="sb-section">
          <button className="nav-item on" type="button">
            <Icon name="home" />
            <span>Home</span>
          </button>
          <button className="nav-item" type="button" onClick={() => setRightTab("activity")}>
            <Icon name="activity" />
            <span>Activity</span>
            <span className={`ct ${workspace.agentEvents.length ? "has" : ""}`}>{workspace.agentEvents.length}</span>
          </button>
          <button className="nav-item" type="button" onClick={() => setRightTab("threads")}>
            <Icon name="thread" />
            <span>Threads</span>
            <span className={`ct ${workspace.threads.length ? "has" : ""}`}>{workspace.threads.length}</span>
          </button>
        </nav>

        <div className="divider sb-divider" />

        <section className="sb-section flex-1 doc-tree">
          <div className="lab">
            <span className="label">Documents</span>
            <button className="btn ghost icon sm" title="New doc" type="button" onClick={() => setModal("document")}>
              <Icon name="plus" />
            </button>
          </div>
          {documentGroups.map(([folder, documents]) => (
            <div key={folder}>
              <div className="nav-item group-label">
                <span className="car">{folder === activeFolder ? "▾" : "▸"}</span>
                <Icon name="stack" />
                <span>{folder}</span>
              </div>
              <div className="tree-children">
                {documents.map((document) => {
                  const threadCount = workspace.threads.filter((thread) => thread.documentId === document.id).length;
                  return (
                    <button
                      className={`nav-item ${document.id === activeDocument?.id ? "on" : ""}`}
                      key={document.id}
                      type="button"
                      onClick={() => {
                        setActiveDocumentId(document.id);
                        setCenterView("document");
                      }}
                    >
                      <span className="car">·</span>
                      <Icon name="doc" />
                      <span className="truncate">{fileName(document.path)}</span>
                      {threadCount ? <span className="ct has">{threadCount}</span> : null}
                    </button>
                  );
                })}
              </div>
            </div>
          ))}
          {!workspace.documents.length ? <p className="tiny muted empty-note">No documents yet.</p> : null}
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
            {workspace.agents.slice(0, 5).map((agent) => {
              const status = visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons);
              return (
                <button
                  key={agent.id}
                  className="agent-mini row gap-8"
                  type="button"
                  onClick={() => {
                    setSelectedAgent(agent);
                    setModal("agent-detail");
                  }}
                >
                  <div className="avi sm agent">{initials(agent.handle)}</div>
                  <span className="small truncate">@{agent.handle}</span>
                  <StatusDot tone={status} />
                </button>
              );
            })}
            {!workspace.agents.length ? <span className="tiny muted">Create an agent after deploying a daemon.</span> : null}
          </div>
        </section>

        <footer className="account-footer">
          <div className="row between">
            <div className="row gap-8 min-0">
              <div className="avi sm you">{initials(account?.displayName ?? account?.email)}</div>
              <span className="small truncate">{account?.displayName ?? account?.email ?? "Signed in"}</span>
            </div>
            <button className="btn ghost sm" onClick={onSignOut}>Sign out</button>
          </div>
        </footer>
      </aside>

      <section className="doc-area">
        <header className="doc-toolbar">
          <div className="breadcrumb">
            <span>{centerView === "document" ? activeFolder : "Operations"}</span>
            <Icon name="chevron" />
            <b>{centerView === "daemons" ? "Daemons" : centerView === "agents" ? "Agents" : activeDocument ? fileName(activeDocument.path) : "No document selected"}</b>
            {centerView === "document" && activeDocument?.updateId ? <span className="chip sm">v {activeDocument.updateId}</span> : null}
            <span className={`chip sm ${connected ? "ok" : "warn"}`}>{connected ? "workspace live" : "workspace offline"}</span>
          </div>
          <div className="row gap-6">
            <div className="avi-stack" aria-label="Workspace presence">
              <div className="avi sm you" title="You">{initials(account?.displayName ?? account?.email)}</div>
              {workspace.agents.slice(0, 2).map((agent) => (
                <div className="avi sm agent" title={`@${agent.handle}`} key={agent.id}>{initials(agent.handle)}</div>
              ))}
            </div>
            <span className="divider-v" />
            <button className={`btn sm ${centerView === "daemons" ? "selected" : ""}`} type="button" onClick={() => setCenterView("daemons")}>
              <Icon name="daemon" />
              Daemons
            </button>
            <button className={`btn sm ${centerView === "agents" ? "selected" : ""}`} type="button" onClick={() => setCenterView("agents")}>
              <Icon name="agent" />
              Agents
            </button>
            <button className="btn sm ghost icon" type="button" onClick={() => setModal("rename")} disabled={!activeDocument || centerView !== "document"}>
              <Icon name="more" />
            </button>
          </div>
        </header>
        {loading ? <div className="notice compact">Loading workspace...</div> : null}
        {error ? <div className="notice error compact">{error}</div> : null}
        {centerView === "daemons" ? (
          <DaemonsManagement
            workspace={workspace}
            onRefresh={() => void reload()}
            onNew={() => setModal("daemon")}
            onDaemon={(daemon) => {
              setSelectedDaemon(daemon);
              setModal("daemon-detail");
            }}
          />
        ) : centerView === "agents" ? (
          <AgentsManagement
            workspace={workspace}
            groupedAgents={groupedAgents}
            onNew={() => setModal("agent")}
            onAgent={(agent) => {
              setSelectedAgent(agent);
              setModal("agent-detail");
            }}
          />
        ) : activeDocument ? (
          <DocumentEditor
            api={api}
            token={token}
            workspaceId={workspaceId}
            account={account}
            document={activeDocument}
            threads={documentThreads}
            focusThreadId={focusThreadId}
            onFocusThreadHandled={() => setFocusThreadId("")}
            onThreadSelected={(threadId) => {
              setRightTab("threads");
              setSelectedThreadId(threadId);
            }}
            onThreadCreated={(threadId) => {
              setRightTab("threads");
              setSelectedThreadId(threadId);
              void reload();
            }}
          />
        ) : (
          <EmptyWorkspace onCreateDocument={() => setModal("document")} onCreateDaemon={() => setModal("daemon")} />
        )}
      </section>

      <aside className={`ctx ${centerView === "document" ? "" : "hidden"}`}>
        <div className="ctx-tabs">
          {(["threads", "activity", "people"] as const).map((tab) => (
            <button key={tab} className={`btn sm ${rightTab === tab ? "selected" : "ghost"}`} onClick={() => setRightTab(tab)}>
              <Icon name={tab === "threads" ? "thread" : tab === "activity" ? "activity" : "people"} />
              {tab}
              {tab === "threads" ? <span className="muted">{documentThreads.length}</span> : null}
              {tab === "people" ? <span className="muted">{onlineAgents}</span> : null}
            </button>
          ))}
        </div>
        {rightTab === "threads" ? (
          <ThreadsPanel
            api={api}
            workspaceId={workspaceId}
            threads={documentThreads}
            selectedThreadId={selectedThreadId}
            onSelectThread={setSelectedThreadId}
            onJumpToThread={(threadId) => {
              setCenterView("document");
              setFocusThreadId(threadId);
            }}
            onReply={() => void reload()}
          />
        ) : null}
        {rightTab === "activity" ? <ActivityPanel workspace={workspace} /> : null}
        {rightTab === "people" ? (
          <PeoplePanel
            workspace={workspace}
            groupedAgents={groupedAgents}
            onAgent={(agent) => {
              setSelectedAgent(agent);
              setModal("agent-detail");
            }}
            onDaemon={(daemon) => {
              setSelectedDaemon(daemon);
              setModal("daemon-detail");
            }}
          />
        ) : null}
      </aside>

      {modal === "document" ? <CreateDocumentModal api={api} workspaceId={workspaceId} onClose={() => setModal(null)} onCreated={(doc) => { setActiveDocumentId(doc.id); setModal(null); void reload(); }} /> : null}
      {modal === "rename" && activeDocument ? <RenameDocumentModal api={api} workspaceId={workspaceId} document={activeDocument} onClose={() => setModal(null)} onDone={() => { setModal(null); void reload(); }} /> : null}
      {modal === "daemon" ? <CreateDaemonModal api={api} workspaceId={workspaceId} onClose={() => setModal(null)} onDone={() => void reload()} /> : null}
      {modal === "agent" ? <CreateAgentModal api={api} workspaceId={workspaceId} daemons={workspace.daemons} onClose={() => setModal(null)} onDone={() => { setModal(null); void reload(); }} /> : null}
      {modal === "agent-detail" && selectedAgent ? <AgentDetailModal api={api} workspaceId={workspaceId} agent={selectedAgent} daemons={workspace.daemons} runs={workspace.agentRuns} onClose={() => setModal(null)} onChanged={() => void reload()} /> : null}
      {modal === "daemon-detail" && selectedDaemon ? <DaemonDetailModal api={api} workspaceId={workspaceId} daemon={selectedDaemon} agents={workspace.agents.filter((agent) => agent.daemonId === selectedDaemon.id)} runs={workspace.agentRuns} onClose={() => setModal(null)} onChanged={() => { setModal(null); void reload(); }} /> : null}
    </main>
  );
}

function DaemonsManagement({
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
  const visibleDaemons = workspace.daemons.filter((daemon) => daemon.status !== "deleted");
  const countByStatus = (status: string) => visibleDaemons.filter((daemon) => daemonStatus(daemon) === status).length;
  const agentsForDaemon = (daemonId: string) => workspace.agents.filter((agent) => agent.daemonId === daemonId);

  return (
    <div className="management-canvas">
      <div className="management-inner">
        <div className="management-head">
          <div className="col gap-2">
            <div className="row gap-8">
              <span className="display management-title">Daemons</span>
              <span className="chip">{visibleDaemons.length} total</span>
            </div>
            <div className="small muted">Local processes that sync this workspace and run agents.</div>
          </div>
          <div className="row gap-6">
            <button className="btn" type="button" onClick={onRefresh}>
              <Icon name="refresh" />
              Check liveness
            </button>
            <button className="btn accent" type="button" onClick={onNew}>
              <Icon name="plus" />
              New daemon
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
                const status = daemonStatus(daemon);
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
                  <td colSpan={6} className="small muted">No daemons yet. Create one to sync docs locally and host agents.</td>
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
  const running = workspace.agents.filter((agent) => visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons) === "working").length;
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
            <div className="small muted">Codex collaborators in this workspace. Each is owned by a daemon.</div>
          </div>
          <div className="row gap-6">
            <div className="input roster-filter">
              <Icon name="search" />
              <span>Filter agents…</span>
            </div>
            <button className="btn accent" type="button" onClick={onNew}>
              <Icon name="plus" />
              New agent
            </button>
          </div>
        </div>

        {groupedAgents.map((group) => {
          const daemon = daemonById.get(group.daemonId);
          const daemonTone = daemon ? daemonStatus(daemon) : "disconnected";
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
                  const status = visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons);
                  const inboxCount = workspace.agentEvents.filter((event) => event.agentId === agent.id && event.box === "for_me").length;
                  return (
                    <button className="agent-roster-card" key={agent.id} onClick={() => onAgent(agent)}>
                      <div className="between gap-8">
                        <div className="row gap-8 min-0">
                          <div className="avi agent">{initials(agent.handle)}</div>
                          <div className="col gap-0 min-0">
                            <b className="truncate">@{agent.handle}</b>
                            <span className="tiny muted truncate">{agent.role}</span>
                          </div>
                        </div>
                        <span className={`chip sm ${status}`}><StatusDot tone={status} />{status}</span>
                      </div>
                      <div className="small muted roster-activity">{agent.currentActivity || agent.currentTask || "Waiting for workspace notifications."}</div>
                      <div className="row between gap-8">
                        <div className="row gap-4 tiny muted">
                          <Icon name="thread" />
                          <span>{workspace.threads.filter((thread) => thread.participantIds.includes(agent.id)).length} threads</span>
                          <span>·</span>
                          <span>{workspace.agentEvents.filter((event) => event.agentId === agent.id).length} inbox</span>
                        </div>
                        {inboxCount ? <span className="chip accent sm">{inboxCount} for-me</span> : null}
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
              ? "No agents yet. Add one to a daemon; it will start working when that daemon is online."
              : "No agents yet. Create a daemon first, then add an agent to it."}
          </p>
        ) : null}
      </div>
    </div>
  );
}

function MetricCard({ label, value, tone }: { label: string; value: number; tone?: "ok" | "warn" | "err" }) {
  return (
    <div className="card p-12 metric-card">
      <div className="label">{label}</div>
      <div className={`display metric-value ${tone ?? ""}`}>{value}</div>
    </div>
  );
}

function DocumentEditor({
  api,
  token,
  workspaceId,
  account,
  document,
  threads,
  focusThreadId,
  onFocusThreadHandled,
  onThreadSelected,
  onThreadCreated,
}: {
  api: ApiClient;
  token: string;
  workspaceId: string;
  account: Account | null;
  document: DocumentItem;
  threads: ThreadItem[];
  focusThreadId: string;
  onFocusThreadHandled: () => void;
  onThreadSelected: (threadId: string) => void;
  onThreadCreated: (threadId: string) => void;
}) {
  const draftRef = useRef<HTMLTextAreaElement | null>(null);
  const [selection, setSelection] = useState<SurfaceSelection | null>(null);
  const [threadDraftOpen, setThreadDraftOpen] = useState(false);
  const [threadBody, setThreadBody] = useState("");
  const [activeThreadGroup, setActiveThreadGroup] = useState<LineThreadGroup<LiveThread> | null>(null);
  const [threadPopoverPoint, setThreadPopoverPoint] = useState({ x: 0, y: 0 });
  const { ydoc, ytext, ready, connected } = useDocumentSync({
    workspaceId,
    token,
    document,
    actorName: account?.displayName ?? account?.email ?? "Human",
  });
  const hasRangeSelection = Boolean(selection);
  const toolbarPoint = {
    x: clamp(selection?.point.x ?? 24, 12, Math.max(12, window.innerWidth - 420)),
    y: clamp((selection?.point.y ?? 120) - 48, 12, Math.max(12, window.innerHeight - 64)),
  };
  const drafterPoint = {
    x: clamp((selection?.point.x ?? 24) + 20, 12, Math.max(12, window.innerWidth - 340)),
    y: clamp((selection?.point.y ?? 120) + 28, 12, Math.max(12, window.innerHeight - 320)),
  };

  useEffect(() => {
    setActiveThreadGroup(null);
    setSelection(null);
    setThreadDraftOpen(false);
    setThreadBody("");
  }, [document.id]);

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

  const openThread = (threadId: string) => {
    onThreadSelected(threadId);
    setActiveThreadGroup(null);
  };

  const openThreadDraft = () => {
    if (!hasRangeSelection) {
      return;
    }
    setThreadDraftOpen(true);
  };

  return (
    <div className="doc-canvas">
      <div className="doc-inner editor-body">
        <div className="doc-meta-row">
          <span className="chip">Document</span>
          <span className="chip outline">Owner · {account?.displayName ?? account?.email ?? "You"}</span>
          <span className="chip outline">Updated {shortTime(document.updatedAt)} ago</span>
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
            <div className="sep" />
            <button type="button" title="Bold"><b>B</b></button>
            <button type="button" title="Italic"><i>I</i></button>
            <button type="button" title="Code"><span className="mono">{"{ }"}</span></button>
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
              <button className="btn ghost icon sm" onClick={() => setThreadDraftOpen(false)} type="button">×</button>
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
          <div className="thread-popover card lifted" style={{ left: threadPopoverPoint.x, top: threadPopoverPoint.y }}>
            <div className="thread-popover-head">
              <div>
                <b className="small">{activeThreadGroup.threads.length} thread{activeThreadGroup.threads.length === 1 ? "" : "s"} on this line</b>
                <div className="tiny muted">line {activeThreadGroup.line}</div>
              </div>
              <button className="btn ghost icon sm" onClick={() => setActiveThreadGroup(null)} type="button">×</button>
            </div>
            <div className="thread-popover-list">
              {activeThreadGroup.threads.map((thread) => (
                <button className="thread-popover-row" key={thread.id} type="button" onClick={() => openThread(thread.id)}>
                  <div className={`avi sm ${thread.createdByType === "agent" ? "agent" : "you"}`}>{initials(thread.createdByHandle || thread.createdByName || "T")}</div>
                  <div className="col gap-2 min-0">
                    <div className="small truncate">
                      <b>{thread.createdByHandle ? `@${thread.createdByHandle}` : thread.createdByName || "Someone"}</b>{" "}
                      {thread.messages[0]?.body || thread.title}
                    </div>
                    <div className="tiny muted">{shortTime(thread.updatedAt)} · {thread.messages.length} replies</div>
                  </div>
                </button>
              ))}
            </div>
            <div className="thread-popover-foot">
              <span className="tiny muted">Open a thread to reply or jump to range.</span>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}

function ThreadsPanel({
  api,
  workspaceId,
  threads,
  selectedThreadId,
  onSelectThread,
  onJumpToThread,
  onReply,
}: {
  api: ApiClient;
  workspaceId: string;
  threads: ThreadItem[];
  selectedThreadId: string;
  onSelectThread: (threadId: string) => void;
  onJumpToThread: (threadId: string) => void;
  onReply: () => void;
}) {
  const [reply, setReply] = useState("");
  const selected = selectedThreadId ? threads.find((thread) => thread.id === selectedThreadId) ?? null : null;

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
          <button className="btn ghost sm icon" type="button"><Icon name="plus" /></button>
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
              <button className="btn ghost icon sm" type="button" onClick={() => onSelectThread("")}>
                <Icon name="back" />
              </button>
              <b>Thread</b>
              <span className="chip sm">{selected.messages.length} replies</span>
            </div>
            <div className="quoted-range">
              <div className="tiny mono muted">{selected.anchor.kind === "document" ? "document thread" : "anchored range"}</div>
              <span>{selected.anchor.excerpt || selected.title}</span>
              {selected.anchor.kind !== "document" ? (
                <button className="jump-range-link" type="button" onClick={() => onJumpToThread(selected.id)}>
                  Jump to range
                </button>
              ) : null}
            </div>
          </div>
          <button className="btn ghost icon sm" type="button"><Icon name="more" /></button>
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

  return (
    <div className="ctx-body">
      <div className="row between ctx-head">
        <span className="label">Threads on this doc</span>
        <button className="btn ghost sm icon" type="button"><Icon name="plus" /></button>
      </div>
      <div className="tlist">
        {threads.map((thread) => (
          <button
            key={thread.id}
            className={thread.id === selectedThreadId ? "titem selected" : "titem"}
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
              <div className="row gap-4 tiny muted">
                <span>{thread.messages.length} replies</span>
                <span>·</span>
                <span>{thread.anchor.kind === "document" ? "document" : "anchored"}</span>
              </div>
            </div>
          </button>
        ))}
      </div>
    </div>
  );
}

function ActivityPanel({ workspace }: { workspace: ReturnType<typeof useWorkspace>["workspace"] }) {
  return (
    <div className="ctx-body">
      <div className="row between ctx-head">
        <span className="label">Recent activity</span>
        <span className="chip sm">{workspace.agentEvents.length}</span>
      </div>
      {workspace.agentEvents.slice(0, 12).map((event) => (
        <article className="activity-row" key={event.id}>
          <div className="avi sm agent">{initials(event.agentHandle || event.agentId)}</div>
          <div className="col gap-2 min-0">
            <div className="small truncate"><b>@{event.agentHandle || event.agentId}</b> {event.type}</div>
            <p className="tiny muted">{event.summary}</p>
          </div>
        </article>
      ))}
      {!workspace.agentEvents.length ? <p className="empty-note">Workspace activity will appear here.</p> : null}
    </div>
  );
}

function PeoplePanel({
  workspace,
  groupedAgents,
  onAgent,
  onDaemon,
}: {
  workspace: ReturnType<typeof useWorkspace>["workspace"];
  groupedAgents: Array<{ daemonId: string; daemonName: string; agents: Agent[] }>;
  onAgent: (agent: Agent) => void;
  onDaemon: (daemon: Daemon) => void;
}) {
  return (
    <div className="ctx-body people-pane">
      <div className="row between ctx-head">
        <span className="label">Daemons</span>
        <span className="chip sm">{workspace.daemons.length}</span>
      </div>
      {workspace.daemons.map((daemon) => (
        <button className="daemon-card card" key={daemon.id} onClick={() => onDaemon(daemon)}>
          <div className="avi sm daemon">{initials(daemon.name)}</div>
          <div className="col gap-2 min-0">
            <strong className="small truncate">{daemon.name}</strong>
            <span className="tiny muted truncate">{daemon.id}</span>
          </div>
          <span className={`chip sm ${daemonStatus(daemon)}`}>{daemonStatus(daemon)}</span>
        </button>
      ))}
      <div className="row between ctx-head with-space">
        <span className="label">Agents</span>
        <span className="chip sm">{workspace.agents.length}</span>
      </div>
      {groupedAgents.map((group) => (
        <section className="agent-group" key={group.daemonId}>
          <div className="grp-head">
            <span>{group.daemonName}</span>
            <span>{group.agents.length}</span>
          </div>
          {group.agents.map((agent) => (
            <button key={agent.id} className="agent-card" onClick={() => onAgent(agent)}>
              <div className="avi agent">{initials(agent.handle)}</div>
              <div className="col gap-2 min-0">
                <strong className="small truncate">@{agent.handle}</strong>
                <span className="tiny muted truncate">{agent.currentActivity || agent.role}</span>
              </div>
              <StatusDot tone={visibleAgentStatus(agent, workspace.agentRuns, workspace.daemons)} />
            </button>
          ))}
        </section>
      ))}
    </div>
  );
}

function CreateDocumentModal({ api, workspaceId, onClose, onCreated }: { api: ApiClient; workspaceId: string; onClose: () => void; onCreated: (document: DocumentItem) => void }) {
  const [path, setPath] = useState("docs/untitled.md");
  const [content, setContent] = useState("# Untitled\n");
  return (
    <Modal title="New document" onClose={onClose}>
      <form
        className="form-stack"
        onSubmit={async (event) => {
          event.preventDefault();
          onCreated(await api.createDocument(workspaceId, { path, content }));
        }}
      >
        <label className="field"><span className="lab">Path</span><input value={path} onChange={(event) => setPath(event.target.value)} required /></label>
        <label className="field"><span className="lab">Initial content</span><textarea value={content} onChange={(event) => setContent(event.target.value)} /></label>
        <button className="btn accent full">Create document</button>
      </form>
    </Modal>
  );
}

function RenameDocumentModal({ api, workspaceId, document, onClose, onDone }: { api: ApiClient; workspaceId: string; document: DocumentItem; onClose: () => void; onDone: () => void }) {
  const [path, setPath] = useState(document.path);
  return (
    <Modal title="Rename document" onClose={onClose}>
      <form
        className="form-stack"
        onSubmit={async (event) => {
          event.preventDefault();
          await api.renameDocument(workspaceId, document.id, path);
          onDone();
        }}
      >
        <label className="field"><span className="lab">Path</span><input value={path} onChange={(event) => setPath(event.target.value)} required /></label>
        <button className="btn accent full">Save path</button>
      </form>
    </Modal>
  );
}

function CreateDaemonModal({ api, workspaceId, onClose, onDone }: { api: ApiClient; workspaceId: string; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState("Local daemon");
  const [token, setToken] = useState("");
  const command = buildDaemonInstallCommand({
    backendUrl: apiBase,
    workspaceId,
    daemonToken: token || "nottyd_...",
    staticBaseUrl: daemonStaticBase,
  });
  return (
    <Modal title={token ? `${name} created` : "New daemon"} onClose={onClose}>
      {token ? (
        <div className="token-reveal">
          <div className="token-warning">
            <div className="row gap-8">
              <div className="avi sm daemon">D</div>
              <b>Save this token now. You won't see it again.</b>
            </div>
            <p className="small muted">The backend stores only the token hash. If you lose it, rotate the daemon token later.</p>
            <div className="row gap-6">
              <code className="token-block">{token}</code>
            </div>
          </div>
          <div className="deploy-block">
            <div className="row between">
              <b className="small">Install daemon</b>
              <span className="chip sm">Host native</span>
            </div>
            <pre className="code">{command}</pre>
            <p className="small muted">This downloads the release bundle, installs the daemon and agent helper, writes daemon config, and starts a local service. Docker Compose is only for local development.</p>
          </div>
          <div className="row between">
            <span className="chip"><StatusDot tone="stale" />Waiting for daemon to check in…</span>
            <button className="btn accent" onClick={onClose}>Done</button>
          </div>
        </div>
      ) : (
        <form
          className="form-stack"
          onSubmit={async (event) => {
            event.preventDefault();
            const response = await api.createDaemon(workspaceId, name);
            setToken(response.token);
            onDone();
          }}
        >
          <label className="field"><span className="lab">Name</span><input value={name} onChange={(event) => setName(event.target.value)} required /></label>
          <button className="btn accent full">Create daemon</button>
        </form>
      )}
    </Modal>
  );
}

function CreateAgentModal({ api, workspaceId, daemons, onClose, onDone }: { api: ApiClient; workspaceId: string; daemons: Daemon[]; onClose: () => void; onDone: () => void }) {
  const activeDaemons = daemons.filter((daemon) => daemon.status !== "deleted");
  const [daemonId, setDaemonId] = useState(activeDaemons[0]?.id ?? "");
  const [handle, setHandle] = useState("codex-agent");
  const [name, setName] = useState("Codex Agent");
  const [role, setRole] = useState("Review workspace changes and provide constructive engineering feedback.");
  const selectedDaemon = activeDaemons.find((daemon) => daemon.id === daemonId);
  const canCreate = Boolean(selectedDaemon);
  return (
    <Modal title="New agent" onClose={onClose}>
      <form
        className="form-stack"
        onSubmit={async (event) => {
          event.preventDefault();
          if (!canCreate) {
            return;
          }
          await api.createAgent(workspaceId, daemonId, { handle, name, role, kind: "codex" });
          onDone();
        }}
      >
        <label className="field"><span className="lab">Display name</span><input value={name} onChange={(event) => setName(event.target.value)} required /></label>
        <label className="field"><span className="lab">Handle</span><input value={handle} onChange={(event) => setHandle(event.target.value)} required /></label>
        <label className="field"><span className="lab">Role</span><textarea value={role} onChange={(event) => setRole(event.target.value)} required /></label>
        <div className="field">
          <span className="lab">Owning daemon</span>
          <div className="daemon-choice-list">
            {activeDaemons.map((daemon) => {
              const status = daemonStatus(daemon);
              return (
                <label className={`daemon-choice ${daemonId === daemon.id ? "selected" : ""}`} key={daemon.id}>
                  <input
                    type="radio"
                    checked={daemonId === daemon.id}
                    onChange={() => setDaemonId(daemon.id)}
                  />
                  <div className="avi sm daemon">{initials(daemon.name)}</div>
                  <div className="col gap-0 min-0">
                    <b className="small truncate">{daemon.name}</b>
                    <span className="tiny muted truncate">{daemon.id}</span>
                  </div>
                  <span className={`chip sm ${status}`}><StatusDot tone={status} />{status}</span>
                </label>
              );
            })}
            {!activeDaemons.length ? <p className="small muted">Create a daemon before adding agents.</p> : null}
          </div>
        </div>
        <p className="small muted">System prompts are generated by the backend shared prompt and are not user-editable.</p>
        <button className="btn accent full" disabled={!canCreate}>Create agent</button>
      </form>
    </Modal>
  );
}

function AgentDetailModal({ api, workspaceId, agent, daemons, runs, onClose, onChanged }: { api: ApiClient; workspaceId: string; agent: Agent; daemons: Daemon[]; runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"]; onClose: () => void; onChanged: () => void }) {
  const [prompt, setPrompt] = useState("Review the current workspace and respond if you have useful feedback.");
  const daemon = daemons.find((item) => item.id === agent.daemonId);
  const status = visibleAgentStatus(agent, runs, daemons);
  return (
    <Modal title={`@${agent.handle}`} onClose={onClose}>
      <div className="form-stack">
        <div className="modal-identity">
          <div className="avi agent">{initials(agent.handle)}</div>
          <div className="col gap-2">
            <span><StatusDot tone={status} /> {status}</span>
            <span className="small muted">Daemon: {daemon?.name ?? agent.daemonId}</span>
          </div>
        </div>
        <p className="small"><strong>Role:</strong> {agent.role}</p>
        <label className="field"><span className="lab">One-off instruction</span><textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} /></label>
        <button className="btn accent full" onClick={async () => { await api.startAgent(workspaceId, agent.id, prompt); onChanged(); }}>Start run</button>
        <button className="btn danger full" onClick={async () => { await api.deleteAgent(workspaceId, agent.id); onChanged(); onClose(); }}>Delete agent</button>
      </div>
    </Modal>
  );
}

function DaemonDetailModal({ api, workspaceId, daemon, agents, runs, onClose, onChanged }: { api: ApiClient; workspaceId: string; daemon: Daemon; agents: Agent[]; runs: ReturnType<typeof useWorkspace>["workspace"]["agentRuns"]; onClose: () => void; onChanged: () => void }) {
  const uninstallCommand = buildDaemonUninstallCommand({
    workspaceId,
    staticBaseUrl: daemonStaticBase,
  });
  const uninstallAllCommand = buildDaemonUninstallCommand({
    staticBaseUrl: daemonStaticBase,
    all: true,
  });
  return (
    <Modal title={daemon.name} onClose={onClose}>
      <div className="form-stack">
        <p><StatusDot tone={daemonStatus(daemon)} /> {daemonStatus(daemon)}</p>
        <p className="tiny muted mono">ID: {daemon.id}</p>
        <h3 className="modal-title">Agents on this daemon</h3>
        {agents.map((agent) => <p className="small" key={agent.id}>@{agent.handle} · {visibleAgentStatus(agent, runs, [daemon])}</p>)}
        <div className="deploy-block">
          <div className="row between">
            <b className="small">Uninstall local daemon</b>
            <span className="chip sm">Run on host</span>
          </div>
          <pre className="code">{uninstallCommand}</pre>
          <p className="small muted">Run this on the machine where the daemon was installed. It stops the local service and removes daemon config, runtime, workspace, agent files, and unused Notty binaries. Delete the daemon record here after local uninstall.</p>
        </div>
        <div className="deploy-block">
          <div className="row between">
            <b className="small">Uninstall all local daemons</b>
            <span className="chip sm danger">Destructive</span>
          </div>
          <pre className="code">{uninstallAllCommand}</pre>
          <p className="small muted">Use this when you want zero local residue on that machine. It removes every Notty daemon service, config, runtime, synced workspace, agent workspace, and local Notty binary.</p>
        </div>
        <button className="btn danger full" onClick={async () => { await api.deleteDaemon(workspaceId, daemon.id); onChanged(); }}>Delete daemon</button>
      </div>
    </Modal>
  );
}

function EmptyWorkspace({ onCreateDocument, onCreateDaemon }: { onCreateDocument: () => void; onCreateDaemon: () => void }) {
  return (
    <section className="doc-canvas">
      <div className="doc-inner empty-state">
        <h2 className="display">Let's get this workspace working.</h2>
        <p className="small muted">notty is best with at least one daemon: it syncs docs to disk and hosts your agents. You can also start by writing something.</p>
        <div className="empty-grid">
          <button className="card p-20 empty-choice" onClick={onCreateDaemon}>
            <div className="row gap-8"><div className="avi sm daemon">D</div><b>Deploy a daemon</b></div>
            <span className="small muted">Bring docs to local disk and enable agents.</span>
          </button>
          <button className="card p-20 empty-choice" onClick={onCreateDocument}>
            <div className="row gap-8"><Icon name="doc" /><b>Create your first doc</b></div>
            <span className="small muted">Markdown or plaintext. Threads and agents come along.</span>
          </button>
        </div>
      </div>
    </section>
  );
}

function Modal({ title, children, onClose }: { title: string; children: ReactNode; onClose: () => void }) {
  return (
    <div className="modal-backdrop">
      <section className="modal-card card lifted">
        <header className="modal-header">
          <h2 className="modal-title">{title}</h2>
          <button className="btn ghost icon sm" onClick={onClose}>×</button>
        </header>
        {children}
      </section>
    </div>
  );
}

function StatusDot({ tone }: { tone: string }) {
  return <span className={`status-dot ${tone}`} />;
}

function Logo() {
  return (
    <div className="row gap-8 logo-row">
      <div className="logo-mark">n</div>
      <span className="display logo-type">notty</span>
    </div>
  );
}

function Icon({ name }: { name: string }) {
  switch (name) {
    case "home":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M3 12l9-9 9 9M5 10v10h14V10" /></svg>;
    case "back":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M15 18l-6-6 6-6" /></svg>;
    case "activity":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></svg>;
    case "thread":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5A8.5 8.5 0 1 1 21 11.5z" /></svg>;
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
    case "daemon":
      return <svg className="i sm" viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="6" rx="1.5" /><rect x="3" y="14" width="18" height="6" rx="1.5" /><circle cx="7" cy="7" r="0.6" /><circle cx="7" cy="17" r="0.6" /></svg>;
    case "agent":
      return <svg className="i sm" viewBox="0 0 24 24"><path d="M12 3l7 4v6c0 4-3 7-7 8-4-1-7-4-7-8V7l7-4z" /><path d="M9 12h6M9 16h6" /></svg>;
    case "more":
      return <svg className="i sm" viewBox="0 0 24 24"><circle cx="12" cy="12" r="1.5" /><circle cx="19" cy="12" r="1.5" /><circle cx="5" cy="12" r="1.5" /></svg>;
    default:
      return null;
  }
}
