# notty

notty is a multi-tenant collaborative workspace where humans and long-running AI agents work in the same file tree, discuss document ranges in anchored threads, and coordinate through daemon-managed local workspaces.

This README is the project encyclopedia. It is intended for product managers, designers, frontend agents, backend agents, and new engineers who have no prior context. It describes the product, its concepts, its components, the current user flows, and the API contracts those flows depend on.

## One Sentence

notty is Google Docs plus a shared repo-like filesystem plus resident Codex agents that can edit files and discuss work in anchored threads.

## Product Goals

- Make humans and AI agents feel like collaborators in one shared workspace.
- Keep documents as real files that can be projected to disk and edited by tools.
- Keep discussion out of document text by using threads anchored to document ranges.
- Let agents preserve identity and context over time instead of treating them as one-shot prompts.
- Keep agent attention manageable through inboxes instead of waking agents for every keystroke.
- Keep tenancy explicit: workspaces are the boundary for users, documents, daemons, agents, and threads.

## Product Non-Goals

- notty is not a chat app with a document attached.
- notty is not a generic task queue for one-off agents.
- notty is not trying to support document-text mentions yet.
- notty no longer has separate comments, proposals, or merge-request concepts.
- notty should not require the frontend to load every document's CRDT history to render the workspace shell.

## Key Features

- Email/password registration and login for human users.
- JWT-backed human frontend sessions.
- Workspace creation and workspace-scoped membership.
- Workspace-scoped daemon token creation.
- One-time daemon token display for local daemon deployment.
- Daemon liveness display: online, stale, offline.
- Daemon-owned agents.
- Single global `New agent` flow where the user selects the owning daemon.
- Agent detail views with associated daemon information.
- Markdown/text/code document creation, move, delete, and live editing.
- Yjs/y-protocol document sync over per-document websockets.
- Lightweight workspace metadata endpoint.
- Document range threads using Yjs CRDT relative positions.
- Thread replies and thread `@handle` mentions.
- Agent notification center with `for-me` and `general` inboxes.
- Agent document diffing from `last-viewed` to `head` or explicit update IDs.
- Daemon filesystem projection for canonical workspace documents.
- Dedicated synced workspace copy for each agent.
- Resident Codex sessions supervised by the daemon.
- Agent helper CLI for reading documents, reading threads, creating threads, replying, viewing inbox items, and diffing documents.

## Core Concepts

### Workspace

A workspace is the tenant boundary. Documents, users, members, daemons, agents, threads, presences, activities, CRDT updates, checkpoints, and agent inboxes all belong to a workspace.

Workspace ownership is created through the normal workspace creation flow. There is no global bootstrap owner.

### Account

An account is a login identity. Accounts are global, identified by email, and authenticated with a password. A single account can be a member of multiple workspaces.

### Workspace User

A workspace user is the account's in-workspace collaborator identity. It has a handle, display name, role, and status. Handles are unique inside a workspace, not globally.

### Document

A content document is a CRDT text stream with an ID, update ID, state vector, and Yjs CRDT history. File-like namespace data such as path, move, and delete state lives in the workspace root CRDT document. The frontend should treat document text as live CRDT state, not as a REST string payload.

### Thread

A thread is the only discussion primitive. It can be document-level or anchored to a document text range. Range threads use CRDT relative anchors. Display fields such as line, start, end, and excerpt are not authoritative.

### Daemon

A daemon is a workspace-scoped process that syncs notty documents to local disk and runs agents. It authenticates with a daemon token minted by the backend. A daemon owns zero or more agents.

### Agent

An agent is a daemon-managed Codex collaborator. It has a stable UUID, handle, name, role, status, owning daemon, workspace root, Codex session metadata, log file, and notification inboxes.

The agent UUID is the backend identity. The handle is user-facing and should not be used as daemon acting identity.

### Agent Inbox

Agents have two inbox classes:

- `for-me`: direct thread mentions, relevant thread replies, and document activity on documents where the agent is already participating.
- `general`: broader workspace document activity that the agent can inspect when useful.

Document-update inbox items are deduplicated. Agents should use document diff tools rather than receiving a prompt for every keystroke.

## Component Overview

### Backend

The backend is a Go HTTP/websocket server on port `8080`.

Responsibilities:

- Authenticate human users with JWT.
- Authenticate daemons with workspace-scoped opaque tokens.
- Resolve daemon acting agent identity through `X-Notty-Acting-Agent-ID`.
- Enforce workspace scoping.
- Store canonical data in Postgres.
- Serve lightweight workspace metadata.
- Accept Yjs/y-protocol document websocket connections.
- Persist CRDT document updates and checkpoints.
- Broadcast workspace events and document updates.
- Store threads, thread messages, participants, activities, presence, agents, agent runs, and agent inbox events.
- Provide bounded document diffs.

The backend should generally avoid materializing full document text. Bounded diffing is the main intentional server-side materialization path.

### Frontend

The frontend is currently React/Vite on port `5173`.

Responsibilities:

- Register and log in human users.
- Create/select workspaces.
- Manage workspace members.
- Create daemons and show one-time daemon deploy tokens.
- Display daemon liveness and daemon-owned agents.
- Create agents through one global `New agent` flow with daemon selection.
- Render workspace metadata.
- Open only the active document's Yjs websocket.
- Render and edit the active document.
- Create document-level and range-anchored threads.
- Show all threads for a selected document or line.
- Show agent roster, agent details, associated daemon, current run state, and collaboration state.

### Daemon

The daemon is a Go process normally run with the Docker Compose `daemon` profile.

Responsibilities:

- Authenticate to a specific workspace with `NOTTY_WORKSPACE_ID` and `NOTTY_DAEMON_TOKEN`.
- Check in to update daemon liveness.
- Sync canonical documents into a local workspace projection.
- Maintain daemon-local CRDT/cache data.
- Maintain per-agent workspaces.
- Watch local filesystem changes and convert local edits into CRDT updates.
- Apply remote CRDT updates back to local files.
- Start, resume, and supervise Codex sessions for agents.
- Expose `notty-agent-tool` to running agents.
- Write generated agent runtime logs under daemon-local data storage.

### Postgres

Postgres is the only datastore in the current product model.

Important tables:

- `accounts`: human login accounts.
- `workspaces`: tenant records.
- `workspace_members`: account-to-workspace membership.
- `users`: in-workspace human collaborator identities.
- `daemons`: workspace daemon records and token hashes.
- `agents`: daemon-owned agent records.
- `agent_runs`: agent process/run status.
- `documents`: content document identity and CRDT stream metadata. Visible paths live in the root CRDT document, not in this table as workspace namespace state.
- `document_heads`: current document `state_vector` and `update_id`, not full content.
- `document_updates`: CRDT binary update log.
- `document_checkpoints`: periodic compacted CRDT checkpoints.
- `threads`: thread metadata and anchors.
- `thread_messages`: thread message bodies.
- `thread_participants`: users/agents involved in a thread.
- `presences`: current actor presence.
- `activities`: meaningful workspace activity.
- `agent_events`: agent notifications/inbox records.
- `agent_document_views`: per-agent last viewed document versions.

## Storage And Sync Model

### Source Of Truth

Postgres is the source of truth. For documents, the source of truth is the CRDT update stream plus checkpoints, not a plaintext file snapshot.

### Document Heads

`document_heads` stores lightweight head metadata:

- `document_id`
- `state_vector`
- `update_id`
- `updated_at`

It intentionally does not store full plaintext content or full CRDT snapshots.

### Document Updates

`document_updates` stores binary Yjs CRDT updates. This is the canonical append-only update history.

### Checkpoints

`document_checkpoints` stores periodic compacted CRDT state. The backend currently creates checkpoints every 100 updates. Checkpoints are an optimization so clients and diff code do not need to replay an unbounded number of updates.

### Frontend Document Loading

The frontend should:

1. Load workspace metadata from `GET /api/workspaces/{workspaceID}/workspace`.
2. Keep document metadata in memory.
3. Open a document websocket only for the active document.
4. Use Yjs/y-protocol to receive the checkpoint/update-derived document state.
5. Dispose inactive document CRDT state when switching documents.

The frontend should not use workspace metadata as a full document content API.

### Daemon Document Projection

The daemon receives document updates, reconciles them with local filesystem changes, and projects canonical document state to files. Agent workspaces are separate local copies managed by the daemon.

Daemon internal caches and generated agent runtime logs belong in daemon-controlled paths, not as product documents. User-authored workspace files, including ordinary `.log` files, continue to sync unless they match the ignored dot-path policy.

## Authentication And Identity

### Human Authentication

Humans authenticate with email/password:

- Register: `POST /api/auth/register`
- Login: `POST /api/auth/login`
- Backend returns a JWT.
- Frontend sends `Authorization: Bearer <jwt>` on HTTP requests.
- Browser websockets may pass the JWT as `?token=<jwt>` because browser WebSocket APIs cannot set arbitrary headers.

JWTs are signed with `NOTTY_JWT_SECRET`. The backend requires both `NOTTY_JWT_SECRET` and Postgres.

### Daemon Authentication

Daemons authenticate with an opaque token:

- Human creates a daemon record.
- Backend returns a plaintext token once.
- Backend stores only `token_hash`.
- Daemon sends `Authorization: Bearer <nottyd_token>`.
- Daemon tokens are workspace-scoped and must match the route workspace ID.
- Successful daemon authentication updates `daemons.last_seen_at`.

### Agent Acting Identity

Agents are managed by daemons. When a daemon performs an action on behalf of an agent, it sends:

```http
X-Notty-Acting-Agent-ID: agent_<uuid>
```

Rules:

- The value must be the canonical agent UUID.
- Handles are not accepted for daemon acting identity.
- The backend verifies that the acting agent belongs to the authenticated daemon.
- If the header is absent, the principal is the daemon itself.

### Error Format

Backend errors use:

```json
{"error":"message"}
```

Common statuses:

- `400`: invalid request or validation error.
- `401`: missing/invalid token.
- `403`: authenticated but not allowed for this workspace/principal.
- `404`: referenced object not found.
- `413`: document diff request too large.
- `503`: auth/database unavailable.

## User Flows And Endpoint Mapping

### 1. Human Registers

Endpoint:

```http
POST /api/auth/register
```

Request:

```json
{
  "email": "person@example.com",
  "password": "secret123",
  "displayName": "Person Name"
}
```

Response `201`:

```json
{
  "token": "<jwt>",
  "account": {
    "id": "account_...",
    "email": "person@example.com",
    "displayName": "Person Name",
    "createdAt": "...",
    "updatedAt": "..."
  },
  "workspaces": []
}
```

Notes:

- Registration creates the account only.
- Workspace user handle is created later when the user creates or joins a workspace.

### 2. Human Logs In

Endpoint:

```http
POST /api/auth/login
```

Request:

```json
{
  "email": "person@example.com",
  "password": "secret123"
}
```

Response `200`:

```json
{
  "token": "<jwt>",
  "account": {"id": "account_...", "email": "person@example.com", "displayName": "Person Name"},
  "workspaces": [{"id": "ws_...", "slug": "product-workspace", "name": "Product Workspace"}]
}
```

### 3. Human Creates A Workspace

Endpoint:

```http
POST /api/workspaces
Authorization: Bearer <jwt>
```

Request:

```json
{
  "name": "Product Workspace",
  "slug": "product-workspace",
  "handle": "alice"
}
```

Response `201`:

```json
{
  "workspace": {
    "id": "ws_...",
    "slug": "product-workspace",
    "name": "Product Workspace",
    "createdAt": "...",
    "updatedAt": "..."
  },
  "member": {
    "workspaceId": "ws_...",
    "accountId": "account_...",
    "userId": "user_...",
    "userHandle": "alice",
    "userName": "Person Name",
    "membershipRole": "owner",
    "status": "active"
  }
}
```

Notes:

- The `handle` field creates the account owner's workspace user handle.
- Handles are normalized and must be unique inside the workspace.

### 4. Human Selects A Workspace

Endpoint:

```http
GET /api/workspaces
Authorization: Bearer <jwt>
```

Response:

```json
{"workspaces":[{"id":"ws_...","slug":"product-workspace","name":"Product Workspace"}]}
```

Then load the selected workspace:

```http
GET /api/workspaces/{workspaceID}/workspace
Authorization: Bearer <jwt>
```

Response:

```json
{
  "workspaceId": "ws_...",
  "currentUserId": "user_...",
  "currentDaemonId": "",
  "name": "Product Workspace",
  "users": [],
  "daemons": [],
  "agents": [],
  "agentRuns": [],
  "threads": [],
  "agentEvents": [],
  "presences": {},
  "activities": [],
  "updatedAt": "..."
}
```

Notes:

- The workspace response does not expose a visible document list. Clients read the workspace root CRDT document to derive files, moves, and deletes.
- Human callers see all agents in the workspace.
- Daemon callers only see agents owned by that daemon.

### 5. Human Adds A Workspace Member

List members:

```http
GET /api/workspaces/{workspaceID}/members
Authorization: Bearer <jwt>
```

Create/add member:

```http
POST /api/workspaces/{workspaceID}/members
Authorization: Bearer <jwt>
```

Request:

```json
{
  "email": "teammate@example.com",
  "displayName": "Teammate",
  "handle": "teammate",
  "role": "Designer"
}
```

Response `201`:

```json
{"member": {"workspaceId":"ws_...","accountId":"account_...","userId":"user_...","membershipRole":"member","status":"active"}}
```

Notes:

- The invited account must already exist.
- This creates a workspace user identity for the account.

### 6. Human Creates And Deploys A Daemon

List daemons:

```http
GET /api/workspaces/{workspaceID}/daemons
Authorization: Bearer <jwt>
```

Create daemon:

```http
POST /api/workspaces/{workspaceID}/daemons
Authorization: Bearer <jwt>
```

Request:

```json
{"name":"Local daemon"}
```

Response `201`:

```json
{
  "daemon": {
    "id": "daemon_...",
    "workspaceId": "ws_...",
    "name": "Local daemon",
    "status": "active",
    "connectionStatus": "disconnected",
    "lastSeenAgeSeconds": 0,
    "createdAt": "..."
  },
  "token": "nottyd_..."
}
```

Frontend should display the token once and provide a hosted installer command. The frontend, backend, and static origins are environment-driven so local development can use localhost while production uses the public domains.

The installer does not require Codex. It keeps `NOTTY_CODEX_COMMAND` as configured, defaulting to `codex`, and writes an explicit daemon `PATH` based on the install shell plus common Codex locations such as Homebrew, `/usr/local/bin`, system bin directories, `~/.local/bin`, `~/.npm-global/bin`, and the npm prefix bin. If Codex is missing, broken, or too old for `app-server`, the installer prints a warning and still installs the daemon; the daemon reports Codex as an unavailable runtime until Codex is installed or fixed.

```sh
curl -fsSL https://static.nottyai.co/daemons/install.sh | sh -s -- \
  --backend-url https://api.nottyai.co \
  --workspace-id ws_... \
  --daemon-token nottyd_... \
  --static-base https://static.nottyai.co/daemons
```

Delete daemon:

```http
DELETE /api/workspaces/{workspaceID}/daemons/{daemonID}
Authorization: Bearer <jwt>
```

Response:

```json
{"daemon": {"id":"daemon_...","status":"deleted","connectionStatus":"disconnected"}}
```

Notes:

- Delete is a soft delete.
- Deleting a daemon marks its agents disconnected.

### 7. Human Creates An Agent Under A Daemon

Preferred endpoint:

```http
POST /api/workspaces/{workspaceID}/daemons/{daemonID}/agents
Authorization: Bearer <jwt>
```

Request:

```json
{
  "handle": "codex-agent",
  "name": "Codex Agent",
  "role": "Implement changes in the shared workspace",
  "kind": "codex"
}
```

Response `201`:

```json
{
  "id": "agent_...",
  "daemonId": "daemon_...",
  "handle": "codex-agent",
  "name": "Codex Agent",
  "role": "Implement changes in the shared workspace",
  "kind": "codex",
  "systemPrompt": "...derived shared prompt...",
  "workspaceRoot": "agents/agent_...",
  "status": "idle",
  "updatedAt": "..."
}
```

Alternative endpoint:

```http
POST /api/workspaces/{workspaceID}/agents
Authorization: Bearer <jwt>
```

This accepts the same request plus `"daemonId": "daemon_..."`. The frontend product should prefer the daemon-scoped endpoint.

Notes:

- Agent kind is a runtime kind reported by the selected daemon in its daemon status `runtimes` payload.
- Agent creation rejects malformed kinds, daemons that have not reported runtime availability, and kinds that are not reported as available by the selected daemon.
- Agent system prompts are derived by the backend from the shared prompt template plus name, handle, kind, and role.
- End users should not customize the system prompt directly.
- The frontend should expose one `New agent` action and ask the user to select a daemon and one of that daemon's available runtimes.

### 8. Human Edits An Agent

Update agent metadata:

```http
PATCH /api/workspaces/{workspaceID}/agents/{agentID}
Authorization: Bearer <jwt>
```

Request:

```json
{
  "handle": "reviewer",
  "name": "Reviewer",
  "role": "Review product changes"
}
```

Response:

```json
{"id":"agent_...","handle":"reviewer","name":"Reviewer","role":"Review product changes"}
```

Delete agent:

```http
DELETE /api/workspaces/{workspaceID}/agents/{agentID}
Authorization: Bearer <jwt>
```

Response:

```json
{"status":"deleted"}
```

### 9. Human Creates Content Streams And Updates The Root

Create an empty content document stream:

```http
POST /api/workspaces/{workspaceID}/documents
Authorization: Bearer <jwt>
```

Request:

```json
{}
```

Response `201`:

```json
{
  "id": "doc_...",
  "stateVector": "...",
  "updateId": 1,
  "updatedAt": "...",
  "clientIdSeed": 123
}
```

The backend creates only the content stream. Current visible paths, moves,
deletes, and path conflicts are represented in the workspace root CRDT document
and projected by clients/daemons. The backend does not expose a current-path
lookup or move/delete document API.

### 10. Frontend Syncs Active Document Text

Open a document websocket:

```http
GET /ws/workspaces/{workspaceID}/documents/{documentID}?client_id=<uint64>&actor_id=<id>&actor_type=human&token=<jwt>
```

Protocol:

- Websocket binary messages use Yjs/y-protocol.
- `MessageSync` handles `SyncStep1`, `SyncStep2`, and incremental `SyncUpdate`.
- `MessageAwareness` carries awareness/presence state for document peers.
- Backend persists applied CRDT updates.
- Backend broadcasts applied updates to other sessions in the document room.
- Backend publishes `agent.inbox.changed` when an applied content update creates or changes agent inbox work.

Notes:

- Browser clients normally pass JWT as `?token=...`.
- Daemon clients can authenticate with `Authorization: Bearer <daemon-token>`.
- Daemon agent document websocket operations may include `X-Notty-Acting-Agent-ID`.

### 11. Human Creates A Thread

Create thread:

```http
POST /api/workspaces/{workspaceID}/threads
Authorization: Bearer <jwt-or-daemon-token>
```

Document-level request:

```json
{
  "documentId": "doc_...",
  "title": "General question",
  "body": "Should this section be split?"
}
```

Range-thread request:

```json
{
  "documentId": "doc_...",
  "title": "Clarify requirement",
  "body": "@codex-agent can you review this?",
  "relativeStart": "<base64 yjs relative position>",
  "relativeEnd": "<base64 yjs relative position>",
  "start": 12,
  "end": 34,
  "line": 5,
  "excerpt": "text being discussed"
}
```

Response `201`:

```json
{
  "thread": {"id":"thread_...","documentId":"doc_...","status":"open","anchor":{},"messages":[]},
  "message": {"id":"msg_...","threadId":"thread_...","body":"...","kind":"comment"}
}
```

Rules:

- `relativeStart` and `relativeEnd` must be supplied together.
- If a thread has line/range metadata but no relative anchors, the backend rejects it.
- If no range metadata and no relative anchors are supplied, the thread is document-level.
- Thread message mentions create agent inbox events.

### 12. Human Replies To A Thread

Fetch thread:

```http
GET /api/workspaces/{workspaceID}/threads/{threadID}
Authorization: Bearer <jwt-or-daemon-token>
```

List document threads:

```http
GET /api/workspaces/{workspaceID}/documents/{documentID}/threads
Authorization: Bearer <jwt-or-daemon-token>
```

Reply:

```http
POST /api/workspaces/{workspaceID}/threads/{threadID}/messages
Authorization: Bearer <jwt-or-daemon-token>
```

Request:

```json
{
  "body": "I agree with this direction.",
  "kind": "comment"
}
```

Response `201`:

```json
{"thread": {"id":"thread_..."},"message": {"id":"msg_...","threadId":"thread_..."}}
```

Notes:

- `@handle` mentions in `body` are resolved against workspace users and agents.
- Mentions of agents create `for-me` inbox items.
- Replies to threads can notify participating agents.

### 13. Frontend Publishes Presence

Endpoint:

```http
POST /api/workspaces/{workspaceID}/presence
Authorization: Bearer <jwt-or-daemon-token>
```

Request:

```json
{
  "actorId": "user_...",
  "actorType": "human",
  "documentId": "doc_...",
  "filePath": "docs/spec.md",
  "mode": "editing",
  "selection": [10, 20],
  "activity": "Editing docs/spec.md"
}
```

Response:

```json
{"actorId":"user_...","actorType":"human","documentId":"doc_...","selection":[10,20],"activity":"Editing docs/spec.md"}
```

Notes:

- In auth mode, actor identity is derived from auth context and can override request actor fields.
- Workspace websocket broadcasts `presence.updated`.

### 14. Human Starts Or Stops Agent Work

Start run for one agent:

```http
POST /api/workspaces/{workspaceID}/agents/{agentID}/runs
Authorization: Bearer <jwt>
```

Request:

```json
{
  "prompt": "Review the current document and suggest improvements.",
  "assignedTaskRef": "thread_..."
}
```

Response `201`:

```json
{"agent": {"id":"agent_...","status":"working"},"run": {"id":"run_...","agentId":"agent_...","status":"queued"}}
```

Stop run:

```http
POST /api/workspaces/{workspaceID}/agent-runs/{runID}/stop
Authorization: Bearer <jwt>
```

Response:

```json
{"id":"run_...","desiredStatus":"stopped"}
```

### 15. Daemon Updates Agent Runtime State

Update daemon status and runtime availability:

```http
PATCH /api/workspaces/{workspaceID}/daemon/status
Authorization: Bearer <daemon-token>
```

Request:

```json
{
  "version": "0.62.0",
  "os": "linux",
  "arch": "arm64",
  "runtimes": [{"kind":"codex","available":true,"version":"codex 0.134.0","path":"/usr/local/bin/codex"}]
}
```

Update agent session:

```http
PATCH /api/workspaces/{workspaceID}/agents/{agentID}/session
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Request:

```json
{
  "status": "working",
  "sessionId": "session_...",
  "currentTurnId": "turn_...",
  "currentActivity": "Reviewing notifications",
  "lastHeartbeatAt": "2026-05-10T12:00:00Z"
}
```

Update run:

```http
PATCH /api/workspaces/{workspaceID}/agent-runs/{runID}
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Request:

```json
{
  "status": "running",
  "desiredStatus": "running",
  "sessionId": "session_...",
  "processId": 123,
  "lastHeartbeatAt": "2026-05-10T12:00:00Z",
  "lastMessage": "Started",
  "logTail": ["line 1", "line 2"],
  "error": "",
  "exitCode": null
}
```

Notes:

- Daemon/agent access is restricted to agents owned by the authenticated daemon.
- Workspace websocket broadcasts `agent.updated` and `agent.run.updated`.

### 16. Agent Reviews Inbox And Diffs

List inbox:

```http
GET /api/workspaces/{workspaceID}/agents/{agentID}/inbox?box=for-me&status=pending
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Response:

```json
{"items": [{"id":"aevt_...","agentId":"agent_...","type":"thread.mentioned","box":"for_me","status":"pending","summary":"..."}]}
```

Fetch inbox item:

```http
GET /api/workspaces/{workspaceID}/agent-inbox/{itemID}
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Update inbox item:

```http
PATCH /api/workspaces/{workspaceID}/agent-inbox/{itemID}
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Request:

```json
{"status":"completed"}
```

Allowed status values used by the product include `pending`, `processing`, `completed`, and `dismissed`.

Diff document:

```http
GET /api/workspaces/{workspaceID}/agents/{agentID}/documents/{documentID}/diff?from=last-viewed&to=head
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Response:

```json
{
  "diff": {
    "documentId": "doc_...",
    "fromUpdateId": 10,
    "toUpdateId": 20,
    "fromContent": "...",
    "toContent": "...",
    "unified": "...",
    "hunks": [{"op":"equal","text":"..."},{"op":"insert","text":"..."}]
  }
}
```

Supported version specs:

- `last-viewed`
- `head`
- `current`
- `latest`
- `update:<id>`
- raw numeric update ID, such as `42`

Diff limits:

- Max input bytes per side: 2 MiB.
- Max lines per side: 20,000.
- Max line product: 2,000,000.
- Max response bytes: 1 MiB.
- Oversized diffs return `413`.

Mark document viewed:

```http
POST /api/workspaces/{workspaceID}/agents/{agentID}/documents/{documentID}/viewed
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Request:

```json
{"updateId": 20}
```

If `updateId` is omitted, the backend marks the document viewed at the current head.

### 17. Daemon Claims Agent Events

Claim event:

```http
POST /api/workspaces/{workspaceID}/agent-events/claim
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Request:

```json
{
  "agentId": "agent_...",
  "claimedBy": "daemon_..."
}
```

Response:

```json
{"event": {"id":"aevt_...","status":"processing","agentId":"agent_...","claimedBy":"daemon_..."}}
```

Update event:

```http
PATCH /api/workspaces/{workspaceID}/agent-events/{eventID}
Authorization: Bearer <daemon-token>
X-Notty-Acting-Agent-ID: agent_...
```

Request:

```json
{
  "status": "completed",
  "threadId": "thread_...",
  "runId": "run_...",
  "lastError": ""
}
```

Notes:

- Inbox endpoints are the preferred agent-facing interface.
- Agent event endpoints exist for daemon automation state transitions.

## API Contracts

### Shared Headers

Human HTTP:

```http
Authorization: Bearer <jwt>
Content-Type: application/json
```

Daemon HTTP:

```http
Authorization: Bearer <nottyd_token>
Content-Type: application/json
```

Daemon acting as agent:

```http
Authorization: Bearer <nottyd_token>
X-Notty-Acting-Agent-ID: agent_...
Content-Type: application/json
```

Browser websocket auth:

```text
?token=<jwt>
```

Daemon websocket auth:

```http
Authorization: Bearer <nottyd_token>
X-Notty-Acting-Agent-ID: agent_...
```

### Models

Account:

```json
{"id":"account_...","email":"person@example.com","displayName":"Person","createdAt":"...","updatedAt":"..."}
```

Workspace:

```json
{"id":"ws_...","slug":"product-workspace","name":"Product Workspace","createdAt":"...","updatedAt":"..."}
```

WorkspaceMember:

```json
{"workspaceId":"ws_...","accountId":"account_...","userId":"user_...","userHandle":"alice","userName":"Alice","membershipRole":"owner","status":"active","invitedBy":"","createdAt":"...","acceptedAt":"..."}
```

DocumentMetadata:

```json
{"id":"doc_...","stateVector":"...","updateId":12,"updatedAt":"...","clientIdSeed":123}
```

Daemon:

```json
{"id":"daemon_...","workspaceId":"ws_...","name":"Local daemon","status":"active","connectionStatus":"online","version":"0.62.0","os":"linux","arch":"arm64","runtimes":[{"kind":"codex","available":true,"version":"codex 0.134.0","path":"/usr/local/bin/codex"}],"lastSeenAt":"...","lastSeenAgeSeconds":4,"createdAt":"...","deletedAt":"..."}
```

Agent:

```json
{"id":"agent_...","daemonId":"daemon_...","handle":"codex-agent","name":"Codex Agent","role":"...","kind":"codex","systemPrompt":"...","workspaceRoot":"agents/agent_...","currentTurnId":"...","sessionId":"...","status":"idle","currentTask":"","currentActivity":"","currentRunId":"","lastHeartbeatAt":"...","lastRunCompleted":"...","updatedAt":"..."}
```

Thread:

```json
{"id":"thread_...","documentId":"doc_...","title":"Clarify requirement","status":"open","anchor":{"documentId":"doc_...","kind":"text-range","relativeStart":"...","relativeEnd":"...","start":12,"end":34,"line":5,"excerpt":"..."},"createdById":"user_...","createdByType":"human","createdByHandle":"alice","createdByName":"Alice","participantIds":["agent_..."],"participantHandles":["codex-agent"],"messages":[],"createdAt":"...","updatedAt":"..."}
```

ThreadMessage:

```json
{"id":"msg_...","threadId":"thread_...","authorId":"user_...","authorType":"human","authorHandle":"alice","authorName":"Alice","body":"@codex-agent please review","kind":"comment","createdAt":"..."}
```

AgentEvent:

```json
{"id":"aevt_...","agentId":"agent_...","agentHandle":"codex-agent","type":"thread.mentioned","box":"for_me","status":"pending","documentId":"doc_...","threadId":"thread_...","threadMessageId":"msg_...","anchorStart":12,"anchorEnd":34,"fromUpdateId":0,"toUpdateId":0,"summary":"...","prompt":"...","dedupKey":"...","claimedBy":"","runId":"","lastError":"","attemptCount":0,"availableAt":"...","createdAt":"...","updatedAt":"..."}
```

### HTTP Route Inventory

| Method | Route | Auth | Request | Response | Used By |
| --- | --- | --- | --- | --- | --- |
| `GET` | `/healthz` | none | none | `{"status":"ok"}` | health checks |
| `POST` | `/api/auth/register` | none | `RegisterRequest` | `AuthResponse` | registration |
| `POST` | `/api/auth/login` | none | `LoginRequest` | `AuthResponse` | login |
| `GET` | `/api/auth/me` | human | none | `{account, workspaces}` | session restore |
| `GET` | `/api/workspaces` | human | none | `{workspaces}` | workspace picker |
| `POST` | `/api/workspaces` | human | `CreateWorkspaceRequest` | `{workspace, member}` | workspace creation |
| `GET` | `/api/workspaces/{workspaceID}/workspace` | human or daemon | none | workspace metadata | workspace shell and daemon refresh |
| `GET` | `/api/workspaces/{workspaceID}/members` | human | none | `{members}` | member management |
| `POST` | `/api/workspaces/{workspaceID}/members` | human | `AddWorkspaceMemberRequest` | `{member}` | member management |
| `GET` | `/api/workspaces/{workspaceID}/daemons` | human | none | `{daemons}` | daemon management |
| `POST` | `/api/workspaces/{workspaceID}/daemons` | human | `CreateDaemonRequest` | `{daemon, token}` | daemon setup |
| `DELETE` | `/api/workspaces/{workspaceID}/daemons/{daemonID}` | human | none | `{daemon}` | daemon deletion |
| `POST` | `/api/workspaces/{workspaceID}/daemons/{daemonID}/agents` | human | `CreateAgentRequest` without `daemonId` | `Agent` | preferred agent creation |
| `POST` | `/api/workspaces/{workspaceID}/documents` | human or daemon | `CreateDocumentRequest` | `DocumentMetadata` | create empty document stream |
| `GET` | `/api/workspaces/{workspaceID}/documents/{id}/threads` | human or daemon | none | `{threads}` | document thread list |
| `POST` | `/api/workspaces/{workspaceID}/agents` | human | `CreateAgentRequest` with `daemonId` | `Agent` | alternate agent creation |
| `PATCH` | `/api/workspaces/{workspaceID}/agents/{id}` | human | `UpdateAgentRequest` | `Agent` | edit agent |
| `PATCH` | `/api/workspaces/{workspaceID}/agents/{id}/session` | agent/daemon scoped | `UpdateAgentSessionRequest` | `Agent` | daemon runtime status |
| `DELETE` | `/api/workspaces/{workspaceID}/agents/{id}` | human | none | `{"status":"deleted"}` | delete agent |
| `POST` | `/api/workspaces/{workspaceID}/agents/{id}/runs` | human | `StartAgentRunRequest` | `{agent, run}` | start agent |
| `POST` | `/api/workspaces/{workspaceID}/threads` | human or daemon/agent | `CreateThreadRequest` | `{thread, message}` | create thread |
| `GET` | `/api/workspaces/{workspaceID}/threads/{id}` | human or daemon | none | `{thread}` | read thread |
| `POST` | `/api/workspaces/{workspaceID}/threads/{id}/messages` | human or daemon/agent | `ReplyThreadRequest` | `{thread, message}` | reply thread |
| `POST` | `/api/workspaces/{workspaceID}/presence` | human or daemon | `UpsertPresenceRequest` | `Presence` | presence |
| `POST` | `/api/workspaces/{workspaceID}/agent-runs` | human | `StartAgentRunRequest` | `{agent, run}` | alternate run creation |
| `PATCH` | `/api/workspaces/{workspaceID}/agent-runs/{id}` | agent/daemon scoped | `UpdateAgentRunRequest` | `AgentRun` | daemon runtime status |
| `POST` | `/api/workspaces/{workspaceID}/agent-runs/{id}/stop` | human | none | `AgentRun` | stop agent |
| `POST` | `/api/workspaces/{workspaceID}/agent-events/claim` | agent/daemon scoped | `ClaimAgentEventRequest` | `{event}` | daemon event loop |
| `PATCH` | `/api/workspaces/{workspaceID}/agent-events/{id}` | agent/daemon scoped | `UpdateAgentEventRequest` | `AgentEvent` | daemon event loop |
| `GET` | `/api/workspaces/{workspaceID}/agents/{id}/notifications` | agent/daemon scoped | `?status=pending` | `{notifications}` | legacy alias |
| `GET` | `/api/workspaces/{workspaceID}/agent-notifications/{id}` | agent/daemon scoped | none | `{notification}` | legacy alias |
| `PATCH` | `/api/workspaces/{workspaceID}/agent-notifications/{id}` | agent/daemon scoped | `UpdateAgentNotificationRequest` | `{notification}` | legacy alias |
| `GET` | `/api/workspaces/{workspaceID}/agents/{id}/inbox` | agent/daemon scoped | `?box=for-me&status=pending` | `{items}` | notification center |
| `GET` | `/api/workspaces/{workspaceID}/agent-inbox/{id}` | agent/daemon scoped | none | `{item}` | notification center |
| `PATCH` | `/api/workspaces/{workspaceID}/agent-inbox/{id}` | agent/daemon scoped | `UpdateAgentNotificationRequest` | `{item}` | notification center |
| `GET` | `/api/workspaces/{workspaceID}/agents/{id}/documents/{documentID}/diff` | agent/daemon scoped | `?from=last-viewed&to=head` | `{diff}` | agent review |
| `POST` | `/api/workspaces/{workspaceID}/agents/{id}/documents/{documentID}/viewed` | agent/daemon scoped | `MarkDocumentViewedRequest` | `{view}` | agent review |

### Request Types

```ts
type RegisterRequest = {
  email: string;
  password: string;
  displayName: string;
};

type LoginRequest = {
  email: string;
  password: string;
};

type CreateWorkspaceRequest = {
  name: string;
  slug?: string;
  handle?: string;
};

type AddWorkspaceMemberRequest = {
  email: string;
  displayName?: string;
  handle?: string;
  role?: string;
};

type CreateDaemonRequest = {
  name: string;
};

type CreateDocumentRequest = {
  documentId?: string;
  clientOperationId?: string;
};

type CreateThreadRequest = {
  documentId: string;
  title?: string;
  body: string;
  relativeStart?: string;
  relativeEnd?: string;
  start?: number;
  end?: number;
  line?: number;
  excerpt?: string;
};

type ReplyThreadRequest = {
  body: string;
  kind?: string;
};

type CreateAgentRequest = {
  daemonId?: string;
  handle: string;
  name: string;
  role?: string;
  kind: string;
  systemPrompt?: string;
};

type UpdateAgentRequest = {
  handle?: string;
  name?: string;
  role?: string;
  systemPrompt?: string;
};

type UpdateAgentSessionRequest = {
  status?: string;
  sessionId?: string;
  currentTurnId?: string;
  currentActivity?: string;
  lastHeartbeatAt?: string;
};

type StartAgentRunRequest = {
  agentId?: string;
  agentName?: string;
  role?: string;
  agentKind?: string;
  prompt: string;
  assignedTaskRef?: string;
};

type UpdateAgentRunRequest = {
  status?: string;
  desiredStatus?: string;
  sessionId?: string;
  processId?: number;
  lastHeartbeatAt?: string;
  lastMessage?: string;
  logTail?: string[];
  error?: string;
  exitCode?: number | null;
};

type UpdateAgentNotificationRequest = {
  status: "pending" | "completed" | "dismissed" | string;
};

type MarkDocumentViewedRequest = {
  updateId?: number;
};
```

Notes:

- `systemPrompt` exists in legacy request structs, but the backend derives the actual prompt. Product UI should not expose system prompt customization.
- `CreateThreadRequest.start`, `end`, and line fields use UTF-16 offsets for browser/editor compatibility.

### Websocket Contracts

Workspace event websocket:

```http
GET /ws/workspaces/{workspaceID}?token=<jwt>
```

Initial message:

```json
{"type":"workspace.snapshot","data":{"users":[],"daemons":[],"agents":[],"agentRuns":[],"threads":[],"agentEvents":[],"presences":{},"activities":[]}}
```

Event envelope:

```json
{"type":"event.name","data":{}}
```

Known event types:

- `workspace.snapshot`
- `thread.created`
- `thread.updated`
- `thread.message.created`
- `presence.updated`
- `daemon.created`
- `daemon.deleted`
- `agent.created`
- `agent.updated`
- `agent.deleted`
- `agent.run.updated`
- `agent.event.updated`

Document websocket:

```http
GET /ws/workspaces/{workspaceID}/documents/{documentID}?client_id=<uint64>&actor_id=<id>&actor_type=<human|agent|daemon>&token=<jwt>
```

Protocol:

- Binary Yjs/y-protocol messages.
- Backend sends sync replies and awareness broadcasts.
- Client sends sync step/update messages and awareness messages.
- Backend persists applied CRDT updates and broadcasts to peers.

## Agent Helper CLI

Agents should use `notty-agent-tool` instead of calling backend APIs manually. The daemon injects:

- `NOTTY_AGENT_TOOL_BASE_URL`
- `NOTTY_AGENT_TOOL_TOKEN`

Commands:

```sh
notty-agent-tool list-documents
notty-agent-tool get-document-by-path --path docs/spec.md
notty-agent-tool list-threads-for-document --document-id doc_...
notty-agent-tool get-thread --thread-id thread_...
notty-agent-tool list-inbox --box for-me
notty-agent-tool list-inbox --box general
notty-agent-tool get-inbox-item --item-id aevt_...
notty-agent-tool complete-inbox-item --item-id aevt_...
notty-agent-tool dismiss-inbox-item --item-id aevt_...
notty-agent-tool diff-document --document-id doc_... --from last-viewed --to head
notty-agent-tool mark-document-viewed --document-id doc_...
notty-agent-tool create-thread --path docs/spec.md --line 42 --title "Question" --body "..."
notty-agent-tool create-thread --path docs/spec.md --line 42 --quote "exact text" --body "..."
notty-agent-tool create-thread --document-id doc_... --document --body "Document-level note"
notty-agent-tool reply-thread --thread-id thread_... --body "..."
```

Create-thread anchor options:

- `--document`: create document-level thread.
- `--path` or `--document-id`: choose target document.
- `--line`: anchor to a full line.
- `--quote`: anchor to exact text, optionally constrained by `--line`.
- `--start-line`, `--start-column`, `--end-line`, `--end-column`: precise range.
- `--start`, `--end`: UTF-16 offsets.

The daemon resolves these into backend `relativeStart` and `relativeEnd` anchors.

## Frontend Design Expectations

### Workspace Shell

The workspace shell should show:

- Current workspace name and switcher.
- Document list.
- Active document.
- Thread count/activity per document.
- Daemon cards with online/stale/offline state.
- Agents grouped or labeled by owning daemon.
- Agent detail modal showing associated daemon.
- Activity feed for meaningful events.

### Daemon Management

The UI should:

- Allow creating a daemon.
- Show the daemon token once.
- Show the full deployment command.
- Show daemon liveness as online, stale, or offline.
- Allow deleting a daemon.
- Show which agents belong to each daemon.

### Agent Management

The UI should:

- Have one `New agent` button.
- Ask the user to select a daemon in the new-agent form.
- Show agent handle, name, role, status, and current activity.
- Show associated daemon in agent detail.
- Avoid exposing system prompt customization.

### Document Editor

The editor should:

- Load only the active document CRDT.
- Support rapid typing and deletion.
- Keep local keystrokes stable while remote updates arrive.
- Show document threads near the relevant ranges.
- Let users open every thread on a line/range.
- Create CRDT-relative anchors for range threads.

### Thread Panel

The thread UI should:

- Show all threads for the active document.
- Show messages and participants.
- Support replies.
- Support `@handle` mentions.
- Jump to thread anchors when resolvable.
- Degrade gracefully if an anchor cannot resolve.

## Agent Behavior Contract

The backend generates a shared system prompt for agents. Product-level behavior:

- Agents work from their own dedicated workspace copy.
- File changes sync promptly to other peers.
- Agents should avoid broad filesystem churn unless intended.
- Plain `@handle` text in Markdown is regular text, not a notification.
- Thread mentions are notification-bearing.
- Agents should inspect `for-me` inbox items first.
- General inbox items are ambient and may not require action.
- Agents do not need to reply unless mentioned or they have useful feedback.
- If agents communicate, they must use thread tools.
- If uncertain, agents should ask for input in a thread before substantial edits.
- Agents should reuse relevant existing threads when possible.

## Removed Or Intentionally Absent

- No separate comments feature. Threads are the discussion/comment primitive.
- No proposal/merge feature.
- No document-text mention notifications.
- No global bootstrap owner.
- No daemon acting identity by agent handle.
- No frontend preload of all document CRDT histories.
- No full-content `/api/workspace` response.
- No user-facing raw CRDT anchor editing.

## Development Setup

Start the local development stack:

```sh
cat > secrets.env <<'EOF'
NOTTY_DATABASE_URL=postgres://notty:notty@postgres:5432/notty?sslmode=disable
NOTTY_JWT_SECRET=replace-with-a-long-random-secret
NOTTY_MAILGUN_API_KEY=replace-with-mailgun-api-key
EOF
# Edit secrets.env with real local Mailgun test credentials.
make dev
```

Equivalent direct command if static artifacts were already generated:

```sh
docker compose --env-file deploy/env/dev.server.env up --build
```

The local Compose backend service loads committed non-secret defaults from
`deploy/env/dev.server.env` and git-ignored secrets from `secrets.env`. The
server requires working email configuration by default so registration, account
activation, and password reset can be tested locally.

Open:

```text
http://localhost:5173
```

The backend health endpoint is available on:

```text
http://localhost:8080/healthz
```

Local development services:

- `backend`: Go API and websocket server on internal port `:8080`; bound to `127.0.0.1:${NOTTY_BACKEND_PORT:-8080}`.
- `frontend`: React/Vite dev server on internal port `:5173`; bound to `127.0.0.1:${NOTTY_FRONTEND_PORT:-5173}`.
- `postgres`: canonical datastore.
- `static`: local static file server on internal port `:8000`; bound to `127.0.0.1:${NOTTY_STATIC_PORT:-5174}`.

Production services:

- Static homepage is hosted at `https://nottyai.co`.
- Static frontend is hosted at `https://app.nottyai.co`.
- Static daemon installer and artifacts are hosted at `https://static.nottyai.co/daemons`.
- The production server runs Docker Compose with `nginx` and `backend`. Nginx is the only public container, enforces `api.nottyai.co` host matching, and proxies to the private backend service. Backend connects to external Postgres through `NOTTY_DATABASE_URL`.

Default production routes:

- Homepage: `https://nottyai.co`
- App: `https://app.nottyai.co`
- API and websockets: `https://api.nottyai.co`
- Daemon downloads: `https://static.nottyai.co/daemons`

For product development from a source checkout, `make dev` is the supported one-command way to start the local backend, frontend, static, and Postgres stack. It first builds host-platform daemon artifacts into `dist/static/daemons`, then starts Docker Compose. Local dev does not run nginx. Local dev defaults `NOTTY_FRONTEND_ORIGIN`, `NOTTY_BACKEND_ORIGIN`, and `NOTTY_STATIC_ORIGIN` to localhost values instead of production domains. The local `static` service serves `dist/static` at `http://localhost:${NOTTY_STATIC_PORT:-5174}`, mirroring production’s separate static origin instead of coupling daemon downloads to the frontend.

Start an external daemon after creating a daemon token in the frontend:

```sh
curl -fsSL https://static.nottyai.co/daemons/install.sh | sh -s -- \
  --backend-url https://api.nottyai.co \
  --workspace-id ws_... \
  --daemon-token nottyd_... \
  --static-base https://static.nottyai.co/daemons
```

Build all local artifacts without publishing:

```sh
make build
```

Focused build targets:

- `make build-go`: compile all Go packages.
- `make build-frontend`: compile the Vite frontend into `frontend/dist`.
- `make build-daemon`: compile local `notty-daemon` and `notty-agent-tool` binaries into `bin`.
- `make build-static-local`: build local host-platform daemon artifacts into `dist/static/daemons`.
- `make build-backend-image`: build the backend Docker image locally without pushing.
- `make build-static VERSION=v0.1.0`: build the full production static tree.
- `make daemon-release-all VERSION=v0.1.0`: build daemon release tarballs for every supported target.

Build daemon release artifacts for every supported target:

```sh
make daemon-release-all VERSION=v0.1.0
```

This creates `dist/static/daemons/install.sh`, `dist/static/daemons/latest/manifest.json`, `dist/static/daemons/latest/SHA256SUMS`, and versioned tarballs containing `notty-daemon` and `notty-agent-tool`. Use `make daemon-release VERSION=v0.1.0 PLATFORMS=linux/arm64` for a focused one-platform release.

Publish artifacts without changing the running backend:

- `make publish-backend VERSION=v0.1.0`: build and push `alphatoad/notty:backend-v0.1.0` plus `backend-latest`.
- `make publish-frontend VERSION=v0.1.0`: build and upload frontend/homepage assets to Cloudflare R2.
- `make publish-static VERSION=v0.1.0`: build and upload daemon installer and release tarballs to Cloudflare R2.
- `make publish VERSION=v0.1.0`: run all three publish jobs.

Build only local daemon artifacts for the current host platform:

```sh
make build-static-local
```

This creates `dist/static/daemons` with `VERSION=dev` and only the current host platform, which is enough for local installer testing at `http://localhost:${NOTTY_STATIC_PORT:-5174}/daemons/install.sh`.

Deploy frontend/homepage assets to Cloudflare R2:

```sh
source ~/.zshrc
make deploy-frontend VERSION=v0.1.0
```

Deploy daemon installer and release tarballs to Cloudflare R2:

```sh
source ~/.zshrc
make deploy-static VERSION=v0.1.0
```

Deploy only the backend to the production server:

```sh
make deploy-backend VERSION=v0.1.0
```

Deploy the full release in order, frontend, daemon static artifacts, then backend:

```sh
source ~/.zshrc
make deploy VERSION=v0.1.0
```

Non-secret deployment defaults are split by consumer:

- `deploy/env/prod.deploy.env`: local deploy-machine defaults for Docker image tags, R2 publishing, static build origins, SSH host, and remote directory.
- `deploy/env/prod.server.env`: production backend runtime defaults copied to `/opt/notty/notty.server.env`.
- `deploy/env/dev.server.env`: local Docker Compose defaults for the development server.
- `secrets.env`: git-ignored local Docker Compose secrets. On production, keep the same file name at `/opt/notty/secrets.env`.

Deployment scripts load `deploy/env/prod.deploy.env` automatically. Use
`NOTTY_DEPLOY_ENV_FILE=/path/to/env` to test another set of deploy-machine
defaults. Keep secrets outside git: set `CLOUDFLARE_API_TOKEN` or
`NOTTY_CLOUDFLARE_TOKEN` locally for R2 publishing, and store backend secrets
such as `NOTTY_DATABASE_URL`, `NOTTY_JWT_SECRET`, and
`NOTTY_MAILGUN_API_KEY` in `/opt/notty/secrets.env` on the server.

`R2_ENDPOINT_URL` is the account endpoint only. Do not include the bucket name in
that URL; bucket names are supplied through `R2_HOMEPAGE_BUCKET`,
`R2_APP_BUCKET`, and `R2_DAEMONS_BUCKET`.

Current static routing uses separate R2 buckets:

- `nottyai.co`: R2 custom domain connected to `notty-homepage-prod`.
- `app.nottyai.co`: R2 custom domain connected to `notty-app-prod`.
- `static.nottyai.co`: R2 custom domain connected to `notty-static-prod`.

R2 serves object keys directly. To make root requests work without a transform
rule, `scripts/publish-static-r2.sh` uploads each root `index.html` twice when
the bucket prefix is empty: once as `index.html`, and once as the empty object
key. Daemon artifacts stay under `daemons/` so installer URLs remain stable.

Production backend deployment uses `compose.prod.yml`. The remote server should keep `/opt/notty/secrets.env` outside git with only secrets such as `NOTTY_DATABASE_URL`, `NOTTY_JWT_SECRET`, and `NOTTY_MAILGUN_API_KEY`. `scripts/deploy-backend.sh` calls `scripts/publish-backend.sh` to build and push `alphatoad/notty:backend-<version>`, uploads `compose.prod.yml`, `deploy/env/prod.server.env`, and the Compose-mounted nginx config to SSH host `notty`, then restarts the production Compose stack:

```sh
make deploy-backend VERSION=v0.1.0
```

Production API traffic is routed by the Compose-managed nginx service. The nginx config lives at `deploy/nginx/notty-api.conf` and is mounted into the nginx container as `/etc/nginx/conf.d/default.conf`. It handles:

- Host matching for `api.nottyai.co`; unmatched hosts are closed.
- TLS termination on `443` using `NOTTY_TLS_CERT_FILE` and `NOTTY_TLS_KEY_FILE`; port `80` only redirects to HTTPS.
- CORS for `https://app.nottyai.co`, `https://nottyai.co`, and local development origins.
- `Authorization`, `Content-Type`, and `X-Notty-Acting-Agent-ID` request headers.
- Websocket upgrades for `/ws/...`.
- Hiding backend `Access-Control-*` headers so production responses do not contain duplicate CORS headers.

Do not store production database, JWT, or Mailgun API credentials in git. Keep them in `secrets.env`, which is intentionally ignored.

Production `/opt/notty/secrets.env` should contain:

```sh
NOTTY_DATABASE_URL=postgres://USER:PASSWORD@HOST:5432/DB?sslmode=require
NOTTY_JWT_SECRET=replace-with-a-long-random-secret
NOTTY_MAILGUN_API_KEY=replace-with-mailgun-api-key
```

Important backend environment variables:

- `NOTTY_PORT`: backend port, default `8080`.
- `NOTTY_DATABASE_URL`: Postgres DSN.
- `NOTTY_JWT_SECRET`: required JWT signing secret.
- `NOTTY_MAILGUN_DOMAIN`: Mailgun sending domain, default `mail.getcodesk.com`.
- `NOTTY_MAILGUN_API_KEY`: Mailgun API key.
- `NOTTY_MAILGUN_FROM`: sender address used for verification and password reset emails, default `noreply@mail.getcodesk.com`.
- `NOTTY_REQUIRE_EMAIL`: defaults to `1`; startup fails if Mailgun config is missing or invalid, and explicit `false` is rejected.
- `NOTTY_PPROF_ADDR`: optional pprof bind address.

Important deployment environment variables in `deploy/env/prod.deploy.env`:

- `NOTTY_DEPLOY_SSH_HOST`: SSH host used by `scripts/deploy-backend.sh`, default `notty`.
- `NOTTY_REMOTE_DIR`: remote deploy directory, default `/opt/notty`.
- `DOCKER_REPO` and `DOCKER_PLATFORMS`: backend image repository and build platforms.
- `DAEMON_PLATFORMS`: comma- or space-separated daemon release platforms, or `all` for every supported target. Production builds Darwin host artifacts locally and uses native Rust/Go cross-compilation for Linux release artifacts and non-host Darwin targets. Linux builds require installed Rust musl targets plus `zig`, or `CC_LINUX_AMD64`/`CC_LINUX_ARM64` pointing at target C compilers. Darwin cross builds on macOS use `xcrun clang -arch`; Darwin cross builds from Linux require installed Rust targets plus `CC_DARWIN_AMD64`/`CC_DARWIN_ARM64` pointing at Darwin-capable compilers such as osxcross clang with an Apple SDK.
- `CLOUDFLARE_ACCOUNT_ID`, `R2_ENDPOINT_URL`, `R2_*_BUCKET`, and `R2_*_PREFIX`: static publish targets.

Important production server defaults in `deploy/env/prod.server.env`:

- `NOTTY_BACKEND_IMAGE`: default backend image if a deploy does not override it.
- `NOTTY_HTTP_BIND` and `NOTTY_HTTP_PORT`: nginx bind address and host port.
- `NOTTY_HTTPS_BIND` and `NOTTY_HTTPS_PORT`: nginx TLS bind address and host port.
- `NOTTY_FRONTEND_ORIGIN`, `NOTTY_BACKEND_ORIGIN`, and `NOTTY_STATIC_ORIGIN`: public production origins for app, API, and static artifacts.
- `NOTTY_API_HOST`: nginx host match for the backend API.
- `NOTTY_TLS_CERT_FILE` and `NOTTY_TLS_KEY_FILE`: TLS certificate and private key paths on the production host. Defaults are `/opt/notty/cert.pem` and `/opt/notty/private.pem`.
- `NOTTY_PUBLIC_ORIGIN`: public frontend origin used by backend-generated links, default `https://app.getcodesk.com`.
- `NOTTY_PPROF_ADDR`: optional pprof bind address.
- `NOTTY_MAILGUN_DOMAIN`: Mailgun sending domain, default `mail.getcodesk.com`.
- `NOTTY_MAILGUN_FROM`: sender address used for verification and password reset emails, default `noreply@mail.getcodesk.com`.
- `NOTTY_REQUIRE_EMAIL`: production default `1`; explicit `false` is rejected. Keep Mailgun API keys in `/opt/notty/secrets.env`.

Important frontend environment variables:

- `NOTTY_FRONTEND_ORIGIN`: public frontend origin, local default `http://localhost:5173`, production default `https://app.getcodesk.com`.
- `NOTTY_BACKEND_ORIGIN`: frontend API/websocket origin, local default `http://localhost:8080`, production default `https://api.nottyai.co`.
- `NOTTY_STATIC_ORIGIN`: static artifact origin, local default `http://localhost:5173`, production default `https://static.nottyai.co`.
- `NOTTY_FRONTEND_PORT`: loopback-only frontend dev port, default `5173`.
- `NOTTY_BACKEND_PORT`: loopback-only backend dev port, default `8080`.
- `NOTTY_DAEMON_STATIC_BASE`: daemon artifact origin override. Defaults to `${NOTTY_STATIC_ORIGIN}/daemons`.
- `VITE_PUBLIC_ORIGIN`, `VITE_API_BASE`, and `VITE_DAEMON_STATIC_BASE`: build-time equivalents used by Vite. They are derived from the origin variables in the standard env files.

Important daemon environment variables:

- `NOTTY_BACKEND_URL`: backend URL.
- `NOTTY_WORKSPACE_ID`: workspace the daemon belongs to.
- `NOTTY_DAEMON_TOKEN`: one-time daemon token from backend.
- `NOTTY_DAEMON_VERSION`: installed daemon version reported to the backend.
- `NOTTY_WORKSPACE_DIR`: local canonical workspace projection.
- `NOTTY_AGENT_WORKSPACE_ROOT`: parent directory for per-agent workspaces.
- `NOTTY_CODEX_COMMAND`: optional Codex executable used for Codex runtime detection, default `codex`.
- `NOTTY_AGENT_TOOL_BASE_URL`: local agent helper gateway URL.
- `NOTTY_PPROF_ADDR`: optional daemon pprof bind address.

## Testing

One-command test suite:

```sh
make tests
```

Focused test tiers:

```sh
make test-unit       # Go, frontend, installer, and uninstall unit/script tests
make test-postgres   # Postgres-backed backend tests in a disposable Docker DB
make test-regression # CRDT/filesystem/websocket regression stack in Docker
make test-live       # API smoke test against an isolated Docker stack
```

Correctness-focused regression areas:

- CRDT sync under sustained append/write pressure.
- Backend restart safety.
- Daemon restart safety.
- Filesystem reconciliation.
- Workspace divergence across agent workspaces.
- Thread mention notification enqueueing.
- Agent inbox deduplication.
- Document diff bounds.
- Daemon token scoping.
- Agent UUID acting identity enforcement.

## Design Principles To Preserve

- Keep the workspace metadata endpoint lightweight.
- Keep the active document as the only frontend CRDT document in memory.
- Treat Postgres as the only datastore.
- Keep threads as the only discussion primitive.
- Keep daemon ownership of agents visible.
- Keep daemon token creation explicit and one-time.
- Keep user-facing states simple.
- Test fragile synchronization and identity code paths, not trivial getters/setters.
