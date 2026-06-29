export type Account = {
  id: string;
  email: string;
  displayName: string;
  lastAccessedWorkspaceId?: string;
};

export type WorkspaceSummary = {
  id: string;
  slug: string;
  name: string;
  lastAccessedDocumentId?: string;
};

export type DocumentItem = {
  id: string;
  path: string;
  title: string;
};

export type DocumentMetadata = {
  id: string;
  stateVector?: string;
  updateId?: number;
  updatedAt: string;
  clientIdSeed?: number;
};

export type UserItem = {
  id: string;
  handle: string;
  name: string;
  role: string;
  kind: string;
  status: string;
  updatedAt: string;
};

export type Daemon = {
  id: string;
  workspaceId: string;
  name: string;
  status: string;
  connectionStatus?: "online" | "stale" | "disconnected" | string;
  version?: string;
  os?: string;
  arch?: string;
  runtimes?: RuntimeDetection[];
  lastSeenAt?: string;
  lastSeenAgeSeconds?: number;
  createdAt: string;
  deletedAt?: string;
};

export type RuntimeDetection = {
  kind: string;
  available: boolean;
  version?: string;
  path?: string;
  reason?: string;
};

export type Agent = {
  id: string;
  daemonId: string;
  handle: string;
  name: string;
  role: string;
  kind: string;
  systemPrompt?: string;
  workspaceRoot: string;
  currentTurnId?: string;
  sessionId?: string;
  status: string;
  currentTask: string;
  currentActivity: string;
  currentRunId: string;
  lastHeartbeatAt?: string;
  lastRunCompleted?: string;
  updatedAt: string;
};

export type AgentRun = {
  id: string;
  agentId: string;
  agentHandle: string;
  agentName: string;
  agentKind: string;
  workspaceRoot: string;
  workingDirectory: string;
  prompt: string;
  status: string;
  desiredStatus: string;
  processId?: number;
  lastHeartbeatAt?: string;
  completedAt?: string;
  lastMessage?: string;
  logTail?: string[];
  error?: string;
  assignedTaskRef?: string;
  updatedAt: string;
};

export type ThreadAnchor = {
  kind: string;
  relativeStart?: string;
  relativeEnd?: string;
  excerpt?: string;
};

export type ThreadMessage = {
  id: string;
  threadId: string;
  authorId: string;
  authorType: string;
  authorHandle: string;
  authorName: string;
  body: string;
  kind: string;
  createdAt: string;
};

export type ThreadItem = {
  id: string;
  documentId: string;
  title: string;
  status: string;
  anchor: ThreadAnchor;
  createdById?: string;
  createdByType?: string;
  createdByHandle?: string;
  createdByName?: string;
  participantIds: string[];
  participantHandles: string[];
  messages: ThreadMessage[];
  createdAt: string;
  updatedAt: string;
};

export type AgentEvent = {
  id: string;
  agentId: string;
  agentHandle: string;
  type: string;
  box?: string;
  status: string;
  documentId?: string;
  threadId?: string;
  threadMessageId?: string;
  summary: string;
  prompt?: string;
  createdAt: string;
  updatedAt: string;
};

export type PresenceItem = {
  actorId: string;
  actorType: string;
  documentId?: string;
  filePath?: string;
  mode?: string;
  selection?: number[];
  activity: string;
  updatedAt: string;
};

export type WorkspaceState = {
  workspaceId: string;
  rootDocumentId: string;
  currentUserId?: string;
  currentDaemonId?: string;
  name: string;
  users: UserItem[];
  daemons: Daemon[];
  agents: Agent[];
  agentRuns: AgentRun[];
  threads: ThreadItem[];
  agentEvents: AgentEvent[];
  presences: Record<string, PresenceItem>;
  activities?: unknown[];
  updatedAt?: string;
};

export type AuthResponse = {
  token: string;
  account: Account;
  workspaces: WorkspaceSummary[];
};

export type WorkspaceEvent = {
  type: string;
  data: unknown;
};
