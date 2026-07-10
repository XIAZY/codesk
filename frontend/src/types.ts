export type Account = {
  id: string;
  email: string;
  displayName: string;
  emailVerified?: boolean;
  lastAccessedWorkspaceId?: string;
};

export type WorkspaceSummary = {
  id: string;
  slug: string;
  name: string;
  defaultRuntime?: string;
  lastAccessedDocumentId?: string;
};

export type WorkspaceInvite = {
  id: string;
  workspaceId: string;
  expiresAt: string;
  createdAt: string;
};

export type WorkspaceInvitePreview = {
  workspace: {
    name: string;
    slug: string;
  };
  expiresAt: string;
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
  // Client-only: wall-clock ms when this payload was received, stamped at the socket/snapshot
  // boundary. Combined with lastSeenAgeSeconds to decay liveness on a timer without trusting the
  // client clock against the server's lastSeenAt. Never sent by the backend.
  receivedAtMs?: number;
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

// The lean subscriber projection served by GET /documents/{id}/subscribers (task #4) — identity only.
export type DocumentSubscriberAgent = {
  id: string;
  handle: string;
  name: string;
  kind: string;
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
  // Opaque base64 Y.js state vector captured when the anchor was created (or last
  // re-anchored). Used by orphan detection to count only the ORIGINAL characters
  // and ignore text inserted after anchor time. Absent on legacy anchors.
  stateAtAnchor?: string;
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
  runId?: string;
  lastError?: string;
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

// The workspace-wide Document Activity feed (backend ActivityEvent). Delivered
// in the workspace snapshot; the store carries it through but does not update it
// incrementally, so it is snapshot-fresh rather than per-event live.
export type ActivityEvent = {
  type: string;
  documentId?: string;
  actorId: string;
  actorType: string;
  summary: string;
  occurredAt: string;
  presenceRef?: string;
};

export type WorkspaceState = {
  workspaceId: string;
  rootDocumentId: string;
  currentUserId?: string;
  currentDaemonId?: string;
  currentMembershipRole?: "owner" | "admin" | "member" | string;
  name: string;
  slug?: string;
  defaultRuntime?: string;
  users: UserItem[];
  daemons: Daemon[];
  agents: Agent[];
  agentRuns: AgentRun[];
  threads: ThreadItem[];
  agentEvents: AgentEvent[];
  presences: Record<string, PresenceItem>;
  activities?: ActivityEvent[];
  updatedAt?: string;
};

export type AuthResponse = {
  token?: string;
  account: Account;
  workspaces?: WorkspaceSummary[];
};

export type WorkspaceEvent = {
  type: string;
  data: unknown;
};
