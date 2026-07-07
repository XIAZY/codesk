import type {
  Agent,
  AuthResponse,
  Daemon,
  DocumentMetadata,
  WorkspaceInvite,
  WorkspaceInvitePreview,
  ThreadItem,
  WorkspaceState,
  WorkspaceSummary,
} from "./types";

export type UpdateWorkspaceSettingsInput = {
  name?: string;
  slug?: string;
  defaultRuntime?: string;
};

function cleanOrigin(value: string) {
  return value.trim().replace(/\/+$/, "");
}

const browserOrigin = typeof window !== "undefined" ? window.location.origin : "http://localhost:5173";
const configuredPublicOrigin = cleanOrigin(import.meta.env.VITE_PUBLIC_ORIGIN || "");
const configuredApiBase = cleanOrigin(import.meta.env.VITE_API_BASE || "");
const configuredDaemonStaticBase = cleanOrigin(import.meta.env.VITE_DAEMON_STATIC_BASE || "");

export const publicOrigin = configuredPublicOrigin || cleanOrigin(browserOrigin);
export const apiBase = configuredApiBase || publicOrigin;
export const daemonStaticBase = configuredDaemonStaticBase || `${publicOrigin}/daemons`;

export class ApiError extends Error {
  status: number;
  details: string;

  constructor(status: number, details: string) {
    super(details || `Request failed with ${status}`);
    this.status = status;
    this.details = details;
  }
}

export class ApiClient {
  private token: string;

  constructor(token: string) {
    this.token = token;
  }

  async register(input: { email: string; password: string; displayName: string }) {
    return this.request<AuthResponse>("/api/auth/register", {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async login(input: { email: string; password: string }) {
    return this.request<AuthResponse>("/api/auth/login", {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async verifyEmail(token: string) {
    return this.request<{ account: AuthResponse["account"] }>("/api/auth/verify-email", {
      method: "POST",
      body: JSON.stringify({ token }),
    });
  }

  async resendVerification(email: string) {
    return this.request<{ status: string }>("/api/auth/resend-verification", {
      method: "POST",
      body: JSON.stringify({ email }),
    });
  }

  async forgotPassword(email: string) {
    return this.request<{ status: string }>("/api/auth/forgot-password", {
      method: "POST",
      body: JSON.stringify({ email }),
    });
  }

  async resetPassword(token: string, password: string) {
    return this.request<{ status: string }>("/api/auth/reset-password", {
      method: "POST",
      body: JSON.stringify({ token, password }),
    });
  }

  async me() {
    return this.request<{ account: AuthResponse["account"]; workspaces: WorkspaceSummary[] }>("/api/auth/me");
  }

  async listWorkspaces() {
    return this.request<{ workspaces: WorkspaceSummary[] }>("/api/workspaces");
  }

  async createWorkspace(input: { name: string; slug: string; handle: string }) {
    return this.request<{ workspace: WorkspaceSummary }>("/api/workspaces", {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async createWorkspaceInvite(workspaceId: string) {
    return this.request<{ invite: WorkspaceInvite; url: string }>(workspacePath(workspaceId, "/invites"), {
      method: "POST",
    });
  }

  async previewWorkspaceInvite(token: string) {
    return this.request<WorkspaceInvitePreview>(`/api/invites/${encodeURIComponent(token)}`);
  }

  async acceptWorkspaceInvite(token: string, input: { handle: string }) {
    return this.request<{ workspace: WorkspaceSummary }>(`/api/invites/${encodeURIComponent(token)}/accept`, {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async loadWorkspace(workspaceId: string) {
    return this.request<WorkspaceState>(workspacePath(workspaceId, "/workspace"));
  }

  async updateWorkspaceSettings(workspaceId: string, input: UpdateWorkspaceSettingsInput) {
    return this.request<{ workspace: WorkspaceSummary }>(workspacePath(workspaceId, "/workspace"), {
      method: "PATCH",
      body: JSON.stringify(input),
    });
  }

  async deleteWorkspace(workspaceId: string, confirmName: string) {
    return this.request<void>(workspacePath(workspaceId, "/"), {
      method: "DELETE",
      body: JSON.stringify({ confirmName }),
    });
  }

  async createDocument(workspaceId: string) {
    return this.request<DocumentMetadata>(workspacePath(workspaceId, "/documents"), {
      method: "POST",
      body: JSON.stringify({}),
    });
  }

  async updateLastAccessed(workspaceId: string, input: { documentId?: string } = {}) {
    return this.request<{ status: string }>(workspacePath(workspaceId, "/last-accessed"), {
      method: "PATCH",
      body: JSON.stringify(input),
    });
  }

  async createThread(workspaceId: string, input: Record<string, unknown>) {
    return this.request<{ thread: ThreadItem }>(workspacePath(workspaceId, "/threads"), {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async replyThread(workspaceId: string, threadId: string, body: string) {
    return this.request<{ thread: ThreadItem }>(workspacePath(workspaceId, `/threads/${encodeURIComponent(threadId)}/messages`), {
      method: "POST",
      body: JSON.stringify({ body, kind: "comment" }),
    });
  }

  async createDaemon(workspaceId: string, name: string) {
    return this.request<{ daemon: Daemon; token: string }>(workspacePath(workspaceId, "/daemons"), {
      method: "POST",
      body: JSON.stringify({ name }),
    });
  }

  async createDaemonReinstallToken(workspaceId: string, daemonId: string) {
    return this.request<{ daemon: Daemon; token: string }>(workspacePath(workspaceId, `/daemons/${encodeURIComponent(daemonId)}/reinstall-token`), {
      method: "POST",
    });
  }

  async deleteDaemon(workspaceId: string, daemonId: string) {
    return this.request<{ daemon: Daemon }>(workspacePath(workspaceId, `/daemons/${encodeURIComponent(daemonId)}`), {
      method: "DELETE",
    });
  }

  async createAgent(workspaceId: string, daemonId: string, input: { handle: string; name: string; role: string; kind: string }) {
    return this.request<Agent>(workspacePath(workspaceId, `/daemons/${encodeURIComponent(daemonId)}/agents`), {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async updateAgent(workspaceId: string, agentId: string, input: { handle: string; name: string; role: string }) {
    return this.request<Agent>(workspacePath(workspaceId, `/agents/${encodeURIComponent(agentId)}`), {
      method: "PATCH",
      body: JSON.stringify(input),
    });
  }

  async deleteAgent(workspaceId: string, agentId: string) {
    return this.request<{ status: string }>(workspacePath(workspaceId, `/agents/${encodeURIComponent(agentId)}`), {
      method: "DELETE",
    });
  }

  async startAgent(workspaceId: string, agentId: string, prompt: string) {
    return this.request<Record<string, unknown>>(workspacePath(workspaceId, `/agents/${encodeURIComponent(agentId)}/runs`), {
      method: "POST",
      body: JSON.stringify({ prompt }),
    });
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers);
    if (!headers.has("Content-Type") && init.body) {
      headers.set("Content-Type", "application/json");
    }
    if (this.token) {
      headers.set("Authorization", `Bearer ${this.token}`);
    }
    const response = await fetch(`${apiBase}${path}`, { ...init, headers });
    if (!response.ok) {
      throw new ApiError(response.status, await responseErrorText(response));
    }
    if (response.status === 204) {
      return undefined as T;
    }
    return response.json() as Promise<T>;
  }
}

async function responseErrorText(response: Response) {
  const text = await response.text();
  try {
    const payload = JSON.parse(text) as { error?: unknown };
    if (typeof payload.error === "string" && payload.error.trim()) {
      return payload.error;
    }
  } catch {
    // Fall back to raw text below.
  }
  return text;
}

export function workspacePath(workspaceId: string, path: string) {
  return `/api/workspaces/${encodeURIComponent(workspaceId)}${path}`;
}

export function workspaceWsUrl(workspaceId: string, token: string) {
  const url = new URL(`${apiBase.replace(/^http/, "ws")}/ws/workspaces/${encodeURIComponent(workspaceId)}`);
  url.searchParams.set("token", token);
  return url.toString();
}

export function documentWsUrl(workspaceId: string, documentId: string, token: string, clientId: number) {
  const url = new URL(`${apiBase.replace(/^http/, "ws")}/ws/workspaces/${encodeURIComponent(workspaceId)}/documents/${encodeURIComponent(documentId)}`);
  url.searchParams.set("token", token);
  url.searchParams.set("client_id", String(clientId));
  url.searchParams.set("actor_type", "human");
  return url.toString();
}
