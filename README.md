# notty

`notty` is a collaborative workspace for humans and resident AI agents.

The product goal is not "chat with an agent beside a document." The goal is a shared working environment where humans and agents can read the same files, edit the same workspace, discuss specific document ranges in anchored threads, and decide when to act through a notification center.

This README describes the current product shape and the frontend contract. It is intentionally written as a product/design brief so another agent or designer can rebuild the frontend without rediscovering the backend and daemon assumptions.

## Product Summary

notty has three first-class objects:

- Documents: Markdown/text/code files synced through Yjs CRDT state.
- Threads: anchored discussions attached to a document or a specific document range.
- Agents: long-running Codex collaborators with roles, handles, workspaces, logs, and notification inboxes.

The workspace should feel like a collaborative editor where agents are present as teammates, not like a command console. Agents can make filesystem edits, but their communication should happen in threads.

## Current User Experience

The current product supports:

- A document library backed by canonical server state.
- A live Yjs-powered editor for the active document.
- File create, move, delete, and text editing.
- Presence updates for humans and agents.
- Document threads anchored with Yjs relative positions, so they survive normal edits better than raw line numbers.
- Thread replies and `@handle` mentions inside threads.
- Persistent agents with handle, name, role, status, current activity, and dedicated workspace.
- Agent notification center with two inboxes: `for-me` and `general`.
- Agent document diffs from an agent's last viewed document version to another version.
- A daemon that syncs the canonical workspace to the local filesystem and gives every agent its own synced working copy.

Removed or intentionally absent:

- Proposal/merge UI and APIs are removed.
- "Comments" are not a separate concept. Threads are the comment/discussion primitive.
- Plain `@handle` text inside Markdown documents is not a notification. Mentions are notification-bearing only inside threads.
- `/api/workspace` should not be used to fetch full document contents or CRDT history.

## Product Principles

### Threads Are The Collaboration Layer

Threads are how humans and agents discuss work. A thread can be document-level or anchored to a document range.

Design implications:

- Thread affordances should be visible near the text they refer to.
- If multiple threads sit on the same line/range, the UI must let the user inspect all of them, not just show a count.
- Threads should show participants, latest activity, mention state, and whether the current user/agent is expected to respond.
- Creating a thread should feel lightweight: select text, write a note, optionally mention an agent.

Implementation implications:

- Browser-created text-range threads must send `relativeStart` and `relativeEnd` generated from the live Yjs document.
- Backend rejects text-range threads that only provide raw offsets.
- Agent-created threads use simple helper arguments such as `--path`, `--line`, and `--quote`; the daemon generates CRDT-relative anchors.

### Agents Are Resident Collaborators

Agents are intended to be long-running collaborators with memory, context, and stable identity. They are not one-shot tasks by default.

Each agent has:

- A handle such as `@codex-agent`.
- A role describing how it should participate.
- A dedicated synced workspace under `/workspace/agents/<agent-id>`.
- A resident Codex app-server session when running.
- A log file in its workspace, e.g. `codex-agent.log`.
- A notification center.

Design implications:

- Agents should appear as teammates with understandable state: `idle`, `working`, or `disconnected`.
- Show current task/activity when available, but avoid making process internals the center of the UI.
- Agent logs are useful for debugging, but the primary product interaction should be through threads and inbox items.
- The UI should distinguish "agent needs my attention" from "agent is merely present."

### The Notification Center Controls Agent Attention

Agents should not be woken up for every keystroke. Document updates are deduplicated into inbox items.

There are two agent inbox classes:

- `for-me`: direct thread mentions, thread replies/mentions relevant to the agent, and document updates for documents the agent has participated in.
- `general`: broader workspace document activity that the agent may ignore unless it has useful feedback or wants to act.

Design implications:

- Surface `for-me` items as higher priority.
- Show `general` items as ambient activity, not urgent tasks.
- Provide clear actions: inspect item, view thread, diff document, mark viewed, complete/dismiss.
- Do not force an agent response unless it was directly mentioned in a thread.

### Documents Are CRDT Documents, Not REST Payloads

The canonical text state is synchronized through Yjs/y-protocol over document websockets.

Design implications:

- The frontend should keep only the active document's Yjs document in memory.
- Document switching should open/sync the selected document, not preload every document's CRDT history.
- `/api/workspace` is for lightweight workspace metadata: document IDs/paths/update IDs, users, agents, threads, presences, and activities.
- The editor should be robust under rapid typing, deletion, and remote updates.

## Frontend Design Brief

The frontend should be designed around three work zones:

1. Workspace navigation
2. Active document editor
3. Collaboration/agent context

### Workspace Navigation

The navigation area should help users answer:

- What files exist?
- Which file am I editing?
- Which files changed recently?
- Which files have unresolved or active threads?
- Which agents are active or need attention?

Recommended elements:

- File tree or grouped document list.
- Recent activity indicators.
- Thread badges per document.
- Agent roster with concise status.
- Create/move/delete file actions kept secondary and safe.

### Active Document Editor

The editor is the core working surface.

Required behavior:

- Open one active document at a time.
- Sync through `/ws/documents/{id}` using Yjs/y-protocol.
- Preserve local keystrokes during remote updates.
- Support text selection and anchored thread creation.
- Show presence/selection when available.
- Show line-level thread affordances with a way to open every thread on that line.

Important UX details:

- Avoid hiding thread content behind a small count-only label.
- Thread markers should be precise but not noisy.
- If a document has many threads, provide filtering by open/recent/mentioned.
- Backspace/delete must feel immediate and must sync in real time.

### Thread Panel

Threads are central enough to deserve a dedicated panel or drawer.

Recommended thread panel capabilities:

- Show all threads for the active document.
- Jump from thread to anchor.
- Show thread title, excerpt, participants, and messages.
- Reply in thread.
- Mention agents/users with `@handle`.
- Create document-level thread when no text is selected.
- Create range thread from selected text.

Thread anchor behavior:

- Browser should derive Yjs relative anchors directly from the active `Y.Doc`.
- Display metadata such as line, start/end, and excerpt is useful but not authoritative.
- If an anchor cannot resolve in the current document state, the UI should degrade gracefully and still show the thread in the document thread list.

### Agent Area

Agents should feel like collaborators, not background daemons.

Recommended agent views:

- Agent roster: handle, name, role, status, current activity.
- Agent detail: role, workspace root, session state, latest log tail, inbox summary.
- Notification center: `for-me` and `general` inboxes.
- Thread mentions: show when an agent was directly mentioned.

Avoid:

- Exposing raw CRDT anchors to users or agents.
- Treating every document update as urgent.
- Making logs the main collaboration interface.

### Activity Feed

The workspace activity feed should summarize meaningful events:

- Document created/moved/deleted.
- Thread created/replied.
- Agent status/session changes.
- Presence changes if useful, but avoid spam.

Document keystrokes should not flood the visible activity feed.

## Core Workflows

### Human Edits A Document

1. Frontend loads `/api/workspace` for metadata.
2. User selects a document.
3. Frontend opens `/ws/documents/{id}`.
4. Yjs sync initializes the active document.
5. User edits locally.
6. Frontend sends Yjs updates over the websocket.
7. Backend persists updates and broadcasts them to peers.
8. Daemon receives updates and projects the document into filesystem workspaces.
9. Agent inbox document-update items are deduplicated.

### Human Starts A Thread

1. User selects a range or chooses document-level comment.
2. Browser creates Yjs relative positions from the active document.
3. Browser posts `POST /api/threads`.
4. Backend stores the thread and broadcasts `thread.created`.
5. Mentions inside the thread body create `for-me` inbox items for mentioned agents.

### Agent Reviews Notifications

1. Backend records inbox items for agents.
2. Daemon periodically wakes/resumes each long-running agent session.
3. Agent sees a short notification summary.
4. Agent can call helper tools:
   - `list-inbox --box for-me`
   - `list-inbox --box general`
   - `get-inbox-item`
   - `diff-document`
   - `mark-document-viewed`
   - `create-thread`
   - `reply-thread`
5. Agent replies in an existing thread or creates a new anchored thread if it has useful feedback.

### Agent Creates A Thread

Agents do not create CRDT anchors manually.

Example:

```sh
notty-agent-tool create-thread \
  --path docs/spec.md \
  --line 42 \
  --quote "exact text to discuss" \
  --title "Potential ambiguity" \
  --body "I think this sentence needs clarification."
```

The daemon resolves the file, reconciles pending local/remote state, materializes text temporarily, computes Yjs relative anchors, and then calls the backend.

## Architecture

### Backend

The backend is a Go HTTP/websocket server on `:8080`.

Responsibilities:

- Maintain canonical workspace metadata.
- Store document CRDT updates and checkpoints in Postgres.
- Serve lightweight workspace metadata.
- Accept Yjs/y-protocol document websocket connections.
- Persist and broadcast document updates.
- Store users, agents, threads, presences, activities, and agent inbox items.
- Compute bounded document diffs for agent review.

The backend should generally avoid materializing all documents. Full text materialization should be reserved for bounded operations such as diffing.

### Daemon

The daemon is a Go process that bridges the server and local filesystem.

Responsibilities:

- Sync backend documents into `/workspace/notty`.
- Maintain per-agent workspaces under `/workspace/agents/<agent-id>`.
- Watch filesystem changes and convert local edits into CRDT updates.
- Apply remote CRDT updates back to files.
- Maintain local CRDT cache/state needed for correct projection and agent thread anchoring.
- Respect file locks for notty's own file reads/writes.
- Run and supervise resident Codex app-server sessions.
- Expose `notty-agent-tool` to agents through an authenticated local tool gateway.

Dotfiles and `.notty` directories are internal workspace files and should not become documents. Agent log files such as `codex-agent.log` are normal synced workspace files.

### Frontend

The current frontend is React/Vite on `:5173`, but the product contract is more important than the implementation.

Frontend responsibilities:

- Render workspace metadata from `/api/workspace`.
- Maintain a Yjs document only for the active document.
- Connect to `/ws` for workspace events.
- Connect to `/ws/documents/{id}` for active document sync.
- Create threads with browser-generated relative anchors.
- Show agents, presence, thread context, and notification state clearly.

## API Surface For Frontend Rebuilds

Important HTTP endpoints:

- `GET /api/workspace`: lightweight workspace metadata.
- `GET /api/documents/by-path?path=...`: resolve document metadata by path.
- `POST /api/documents`: create document.
- `PATCH /api/documents/{id}`: move/rename document.
- `DELETE /api/documents/{id}`: delete document.
- `GET /api/documents/{id}/threads`: list document threads.
- `POST /api/threads`: create thread.
- `GET /api/threads/{id}`: fetch thread.
- `POST /api/threads/{id}/messages`: reply to thread.
- `POST /api/presence`: publish presence/selection.
- `POST /api/users`, `PATCH /api/users/{id}`, `DELETE /api/users/{id}`.
- `POST /api/agents`, `PATCH /api/agents/{id}`, `DELETE /api/agents/{id}`.
- `PATCH /api/agents/{id}/session`: update agent session/status.
- `GET /api/agents/{id}/inbox`: list agent inbox items.
- `GET /api/agent-inbox/{id}`: fetch inbox item.
- `PATCH /api/agent-inbox/{id}`: complete/dismiss/update inbox item.
- `GET /api/agents/{id}/documents/{documentID}/diff`: bounded document diff.
- `POST /api/agents/{id}/documents/{documentID}/viewed`: mark document viewed.

Websockets:

- `GET /ws`: workspace event stream.
- `GET /ws/documents/{id}?client_id=...&actor_id=...&actor_type=...`: Yjs/y-protocol document sync.

Frontend thread creation must provide:

- `documentId`
- `body`
- `relativeStart` and `relativeEnd` for text-range threads
- display metadata: `start`, `end`, `line`, `excerpt`

Document-level threads can omit relative anchors.

## Agent Helper Tools

Agents interact with notty through `notty-agent-tool`.

Common commands:

```sh
notty-agent-tool list-documents
notty-agent-tool get-document-by-path --path docs/spec.md
notty-agent-tool list-threads-for-document --document-id <document-id>
notty-agent-tool get-thread --thread-id <thread-id>
notty-agent-tool list-inbox --box for-me
notty-agent-tool list-inbox --box general
notty-agent-tool get-inbox-item --item-id <item-id>
notty-agent-tool diff-document --document-id <document-id> --from last-viewed --to head
notty-agent-tool mark-document-viewed --document-id <document-id>
notty-agent-tool create-thread --path docs/spec.md --line 42 --quote "exact text" --body "..."
notty-agent-tool reply-thread --thread-id <thread-id> --body "..."
```

Agents should not need to know about raw CRDT relative-position encoding.

## Design Constraints And Gotchas

- Do not rebuild proposal/merge flows; they are intentionally removed.
- Do not implement document plaintext mentions as notifications.
- Do not treat raw line numbers as stable thread anchors.
- Do not fetch or hold all document CRDT states in the frontend.
- Do not make `/api/workspace` a document content loading path.
- Do not hide multiple same-line threads behind an inaccessible badge.
- Do not assume agent logs are the source of truth for agent state; use agent/session fields and inbox data.
- Do not wake agents for every keystroke; document inbox items are deduplicated.

## Running Locally

```sh
docker compose up --build
```

Open:

```text
http://localhost:5173
```

Services:

- `backend`: Go API and websocket server on `:8080`.
- `frontend`: React/Vite client on `:5173`.
- `daemon`: local projection daemon, agent workspace manager, and Codex app-server supervisor.
- `postgres`: canonical datastore.

The daemon container installs `@openai/codex` and mounts `${HOME}/.codex` so agent sessions can use local Codex credentials.

## Testing

Normal test suite:

```sh
go test ./...
cd frontend && npm test
```

Regression tests:

```sh
go test -tags=regression ./test/regression
```

Stress and restart diagnostics are documented in `test/regression/README.md`.
