# AI-Agent-Native Workspace
## Product Vision and Technical Design Specification

**Version:** Draft 1  
**Audience:** Founder, product, engineering, infrastructure, design  
**Purpose:** Summarize the product vision, strategic positioning, and proposed technical implementation for a shared, AI-agent-native workspace.

---

## 1. Executive Summary

The product is a **shared, live workspace for both humans and AI agents**. It is designed around a simple belief:

> The future unit of work is not a standalone app, branch, or personal computer. It is a shared working copy of a workspace that humans and agents co-edit in real time.

Today, people collaborate across many disconnected tools:
- docs for writing
- tickets for planning
- code repos for software
- spreadsheets for models
- folders for assets
- chat for coordination

AI agents currently operate poorly in this environment. They are usually:
- invoked ad hoc,
- confined to a single surface,
- stateless or weakly stateful,
- detached from shared live context,
- unable to collaborate naturally with other agents and humans.

This product aims to change that.

The proposed system is a **multiplayer workspace** where:
- humans and agents are both first-class users,
- all work artifacts are exposed as files or file-like objects,
- everyone can see changes stream live,
- comments and review happen in context,
- large or risky changes can fork into proposal workspaces,
- Git provides durable lineage and forking,
- a real-time collaboration engine provides Google-Docs-style synchronous editing.

The result is a system that combines:
- the **agent ergonomics of a local filesystem**,
- the **liveness of Google Docs**,
- the **safety and version lineage of Git**,
- an **agent execution layer comparable to Codex or Claude Code**,
- and the **structure of a shared workspace operating system**.

---

## 2. Product Vision

### 2.1 Core Thesis

Humans collaborate slowly, which is why modern software is fragmented into apps, branches, queues, and handoffs. AI agents can collaborate much faster, but only if they share the same live state.

If every agent must:
- fetch context from scratch,
- work in a private copy,
- dump results back as a final artifact,
- and rely on humans to manually merge everything,

then most of the potential advantage of agentic work is lost.

The product vision is therefore:

> Build an AI-agent-native workspace where humans and persistent AI agents work together in the same shared working copy, across many artifact types, with live visibility and structured safety.

### 2.2 Product Metaphor

This should not be conceived as:
- a chatbot with files,
- a doc editor with AI features,
- a Git repo for non-developers,
- or a task manager with agents.

It should be conceived as:

> A shared working environment where files, comments, tasks, code, spreadsheets, and agents all exist in one multiplayer space.

### 2.3 End State

In the ideal end state:
- a product manager, researcher, engineer, finance analyst, and several AI agents all operate in the same workspace,
- everyone can see live edits and comments,
- agents behave like coworkers with identities, responsibilities, and memory,
- large tasks fork into side workspaces when necessary,
- the canonical workspace continuously evolves without chaotic merge overhead.

---

## 3. Product Principles

### 3.1 Shared Working Copy by Default

There should be one visible shared working copy of the workspace. Collaboration should feel live and synchronous, not batch-based.

### 3.2 Everything Is File or File-Like

Agents work efficiently with filesystems. The system should expose all objects through a filesystem-like interface so agents can:
- traverse directories,
- read files,
- diff content,
- patch files,
- and use standard tooling patterns.

### 3.3 Live Stream First

Changes should stream live by default, like Google Docs. Humans and agents should be able to see work unfold as it happens.

### 3.4 Fork When Risk or Scale Demands It

Large, risky, or high-conflict changes should fork into proposal workspaces rather than directly mutate the shared live state.

### 3.5 Agents Are First-Class Users

Agents are not background functions. They should have:
- identities,
- permissions,
- memory boundaries,
- inboxes,
- presence,
- responsibilities,
- and visible activity in the workspace.

Crucially, the workspace should distinguish between the **agent as a principal in the shared system** and the **agent execution layer** that actually reads files, edits code, runs commands, and carries out work. In practice, the execution layer should feel much closer to tools like Codex and Claude Code than to a simple chat assistant.

### 3.6 Git for Lineage, Not for Live Editing

Git is excellent for:
- checkpoints,
- lineage,
- forks,
- proposals,
- rollback,
- and merge workflows.

Git is not the right primitive for keystroke-level live collaboration.

### 3.7 Safety Without Killing Flow

The system should feel fluid like a collaborative document, while still supporting:
- approvals,
- provenance,
- reversibility,
- semantic validation,
- and conflict containment.

---

## 4. Product Definition

### 4.1 What the Product Is

A shared, AI-native workspace where humans and persistent agents collaborate synchronously on:
- markdown and documents,
- code,
- spreadsheets,
- images and media,
- comments and review threads,
- tasks and execution records,
- and other file-like objects.

### 4.2 What Makes It Different

The product is not defined by “having AI.” It is defined by a different model of work:

- **shared state instead of isolated outputs**,
- **persistent coworkers instead of one-shot assistants**,
- **live collaboration instead of job submission**, and
- **workspace-native coordination instead of cross-tool handoff**.

### 4.3 Ideal Initial Use Cases

The initial product should not try to replace all enterprise software at once. It should focus on high-value collaborative workflows with rich mixed media and clear agent leverage.

Strong starting wedges include:
- product + engineering planning,
- software delivery coordination,
- research + strategy + documentation,
- financial planning and operating models,
- marketing / campaign production.

---

## 5. User and Agent Model

### 5.1 Human Users

Human users participate through:
- file navigation,
- rich editors,
- comments,
- live co-editing,
- review,
- approvals,
- and task assignment.

### 5.2 Agent Users

Each agent is modeled as a principal with:
- `agent_id`
- name
- role
- permissions
- memory scope
- subscriptions
- tools
- execution policies
- write budgets
- draft/proposal permissions
- activity status

Example:

```yaml
agent:
  id: growth_analyst
  name: Growth Analyst
  role: Analyze campaigns and forecasts
  can_read:
    - /marketing
    - /sales
    - /finance/inputs
  can_write:
    - /marketing/drafts
    - /analysis
  can_comment:
    - /marketing
    - /finance
  publish_policy:
    finance_changes: approval_required
    marketing_docs: auto_publish_small_edits
  subscriptions:
    - campaign_briefs
    - weekly_kpis
```

### 5.3 Agent Behavior Philosophy

Agents should behave like real collaborators:
- pick up tasks,
- read relevant context,
- make live changes,
- leave comments,
- respond to review,
- and escalate to proposals for large changes.

### 5.4 Agent Identity vs Execution Layer

The product should make a clear distinction between:
- the **agent identity layer** inside the workspace, and
- the **agent execution layer** that performs the work.

The identity layer is what users see:
- name
- role
- permissions
- presence
- comment history
- task ownership
- proposal participation

The execution layer is the runtime that actually operates on the mounted workspace:
- reads files and directory trees
- edits files or structured objects
- runs commands/tests/tools
- observes diffs
- produces bounded writes
- works inside a local or cloud sandbox

A good mental model is that the workspace product provides the shared world state, while **Codex- or Claude-Code-like runtimes are the hands of the agent inside that world**.

---

## 6. Workspace and Object Model

### 6.1 The Workspace

A workspace is a replicated graph of typed objects projected as a filesystem-like tree.

Conceptually:
- users see folders and files,
- the system stores a typed object graph,
- and the current state is materialized into a live working copy for every participant.

### 6.2 Object Types

The system should support at least the following first-class object types:
- text file
- markdown document
- code file
- spreadsheet workbook
- image/media asset
- comment thread
- task
- proposal
- execution record
- agent definition
- folder/tree node

### 6.3 Filesystem Projection

Everything should be addressable through a filesystem-like interface.

Example:

```text
/workspace
  /docs
    spec.md
    launch-plan.md
  /code
    api/
    web/
  /finance
    forecast.sheet/
  /design
    hero.image/
  /.comments
  /.tasks
  /.agents
  /.history
  /.proposals
```

### 6.4 Simple Files vs Package Objects

Simple objects should be plain files:
- `.md`
- `.py`
- `.ts`
- `.json`
- `.yaml`

Complex objects should behave like package directories:

```text
forecast.sheet/
  workbook.json
  cells.parquet
  formulas.json
  formatting.json
  comments.jsonl
```

```text
roadmap.doc/
  content.md
  blocks.json
  comments.jsonl
  metadata.json
```

```text
hero.image/
  blob.png
  metadata.json
  regions.json
  comments.jsonl
```

This preserves filesystem ergonomics while allowing richer structured storage internally.

---

## 7. Collaboration Model

### 7.1 Shared Live Working Copy

The core product experience is a shared live working copy. Everyone sees the workspace evolving together.

For the MVP, this is intentionally closer to Google Docs for text than to a traditional Git workflow.

### 7.2 Live Streaming of Changes

Changes should stream live by default, but the MVP must sharply limit what is truly co-editable in live mode.

For the MVP:
- users should be able to observe all relevant work live,
- but only a narrow set of object types should support true low-latency concurrent editing.

Users should be able to observe:
- text edits,
- code changes,
- comments,
- proposal activity,
- and agent activity.

The MVP should support true live concurrent editing only for:
- markdown/text
- code files at the text level
- comments
- lightweight task metadata

The MVP should not promise true Google-Docs-style live editing for:
- spreadsheet packages
- filesystem tree operations
- media/package objects
- large generated artifacts

### 7.3 Visibility vs Trust

The system should separate:
- **live visibility** of changes,
- from **trusted / stable state**.

Recommended visible states:
1. **Stable** – trusted baseline
2. **Live** – low-latency stream of in-progress operations layered over stable
3. **Proposed** – visible changes not yet accepted into stable state

This allows high liveness without sacrificing safety.

### 7.4 Conflict Prevention Over Resolution

Most conflicts should be prevented through:
- live presence,
- collaborator intent awareness,
- soft leases on active regions,
- write budgets for agents,
- and policy-based escalation to proposal workspaces.

The most important MVP rule is:

> everyone can see work live, but not everything is safe to mutate live.

---

## 8. Comments and Review

### 8.1 Universal Comments

Comments should be first-class objects, not just a document feature.

A comment can attach to:
- a text range,
- a code symbol or line range,
- a spreadsheet cell/range,
- an image region,
- a folder,
- a file,
- a proposal,
- or an execution result.

### 8.2 Comment Storage

Comments should be stored as anchored structured objects and projected as sidecar files where useful.

Example:

```json
{
  "id": "cmt_123",
  "author": "review_agent",
  "anchor": {
    "type": "code_symbol",
    "path": "/code/api/auth.ts",
    "symbol": "authenticateUser"
  },
  "body": "This branch bypasses token expiry validation.",
  "status": "open",
  "created_at": "2026-04-18T10:30:00Z"
}
```

### 8.3 Review Threads as Coordination Primitives

Comment threads should support:
- mentions,
- agent replies,
- human replies,
- resolution,
- task creation,
- and proposal linkage.

---

## 9. Presence and Awareness

### 9.1 Why Presence Matters

Live collaboration works because users can see each other.

Presence is not cosmetic. It is a core anti-conflict mechanism.

### 9.2 Human Presence

Expose:
- current file
- cursor/selection
- active edit region
- mode: reading / editing / reviewing

### 9.3 Agent Presence

Expose:
- active file or region
- current task
- current activity
- intended scope
- whether working in live or proposal mode

Examples:
- “Research Agent is expanding section 4”
- “Forecast Agent is updating Q3 assumptions”
- “Reviewer Agent left 2 comments on `auth.ts`”

---

## 10. Forking and Proposal Model

### 10.1 Why Forking Exists

The live workspace is ideal for small and medium collaboration. It is not always ideal for:
- large refactors,
- bulk edits,
- schema migrations,
- destructive tree operations,
- or work likely to conflict with many active users.

The MVP should default to proposal workspaces earlier rather than later. If a change spans many files, touches active regions, or looks like a broad rewrite, proposal mode is the preferred path.

### 10.2 Git-Backed Proposals

For large diffs, the system should use Git-backed forks or proposal workspaces.

These proposals should:
- branch from a checkpointed workspace state,
- create a lightweight side workspace,
- allow agents and humans to collaborate within that side workspace,
- and later produce a structured merge proposal back into the main workspace.

### 10.3 Proposal Workflow

1. Agent or user identifies a large change.
2. System recommends proposal mode or automatically escalates.
3. Proposal workspace is forked from a Git checkpoint.
4. Agent performs work in the proposal workspace.
5. Other agents and humans review, comment, and validate.
6. Proposal is merged into stable/live workspace if accepted.

### 10.4 Product Metaphor

Non-technical users should see this as:
- “Create side workspace”
- “Discuss proposal”
- “Merge into main workspace”

Developers may understand the Git lineage underneath, but the product should not require raw Git fluency.

---

## 11. High-Level Technical Architecture

The MVP architecture should be intentionally simple:
- a product-native editor for text and code
- a server-authoritative live collaboration service
- a local filesystem projection for agents
- Git-backed proposal workspaces for larger changes

For the MVP, the server-side system should be implemented in **Go**. Go is the preferred backend language here because it provides:
- strong efficiency for long-lived concurrent connections and coordination workloads,
- straightforward deployment as a portable single binary,
- predictable operational behavior for daemon-style services,
- and a good fit for backend components that need to run reliably across developer machines, cloud environments, and enterprise infrastructure.

### 11.1 Major Layers

1. **Client / Edge Workspace Layer**
2. **Realtime Collaboration Layer**
3. **Workspace Storage Layer**
4. **Filesystem Projection Layer**
5. **Git Proposal Layer**
6. **Agent Runtime Layer**
7. **Permissions / Provenance Layer**

---

## 12. Storage and State Model

### 12.1 Core Principle

The shared working copy should not be implemented as a single mutable folder where all edits directly overwrite canonical files.

For the MVP, the canonical collaborative state should live in the application collaboration layer, not in the local filesystem mount.

Instead, the system should maintain:
- current document state for live-editable files
- durable storage for comments, tasks, and metadata
- a projected local file tree for agents
- Git checkpoints for stable history and proposals

### 12.2 Object IDs and Paths

Paths should not be the true identity of objects.

Each object needs:
- stable `object_id`
- type
- metadata
- current path projection
- backlinks/references

This allows renames and moves without breaking anchors or references.

---

## 13. Local Replica and Filesystem Layer

### 13.1 Local Replica Philosophy

Every agent runtime should get a local materialized workspace.

For the MVP, the human editing experience should happen in product-native editors backed directly by the collaboration layer. The local filesystem mount exists primarily to support agent runtimes and toolchains.

This preserves the ergonomics of:
- local file traversal,
- open/read/write flows,
- standard tool behavior,
- and agent-native filesystem reasoning.

### 13.2 Mounted Workspace

Example:

```text
/mnt/workspaces/acme
```

Agents should be able to:
- `ls`
- `grep`
- read files
- write patches
- watch changes
- use normal local toolchains

### 13.3 Intercepted Writes

The mounted workspace should not simply sync file bytes on close.

Instead, a local workspace daemon should:
1. detect writes,
2. diff against local base state,
3. turn writes into bounded text or file operations,
4. send ops to the collaboration layer,
5. apply remote ops back into the local working tree.

For the MVP, this daemon should be implemented in **Go**. Go is a pragmatic fit because it can handle:
- long-lived network connections to the server efficiently,
- filesystem watching and diff orchestration in a portable native binary,
- structured process management for local agent runtimes,
- and cross-platform distribution without requiring a separate language runtime on the host machine.

For the MVP, this bridge should only be required to work well for:
- text files
- code files
- comment sidecars or metadata files if needed

The MVP should explicitly avoid promising arbitrary safe interception for every editor, save pattern, or complex package object.

### 13.3A Reconciliation Between Collaboration Layer and Local Filesystem

Under the MVP model, the collaboration layer is the source of truth and the local filesystem is a projection.

The reconciliation loop should work like this:

1. A human edits a file in the product UI.
2. The editor sends small text operations to the collaboration service.
3. The collaboration service applies those operations to canonical document state.
4. The local projection daemon observes the new canonical state and materializes it into the local file tree.
5. An agent edits the projected local file.
6. The daemon detects the file change, diffs it against the last projected version, and converts the delta into bounded text operations.
7. Those operations are submitted back to the collaboration service with a base version.
8. If accepted, the canonical state advances and the new state is projected back to local files.
9. If rejected or deemed too risky, the system refreshes from canonical state or escalates the work to proposal mode.

This means the local filesystem is not doing peer-style merge. It is a synchronized working copy with a writeback bridge.

In practice, the daemon plays four roles:
- `server_client`: maintain authenticated sessions with the backend, stream workspace events, submit local operations, and receive canonical updates
- `materializer`: collaboration state to local files
- `detector`: local file changes to diffs
- `agent_supervisor`: launch, monitor, and stop local AI agent processes such as Codex and Claude Code, while wiring them to the correct workspace projection and policy context

The daemon is therefore not just a sync adapter. It is the local control plane for the workspace. Concretely, it is responsible for:
- talking to the server over the workspace protocol
- detecting local file changes and computing diffs against projected base state
- translating accepted local edits into collaboration operations
- applying canonical remote updates back into the projected filesystem
- managing local AI agent processes, their lifecycle, health, and workspace attachment

For each projected file, the daemon should track lightweight reconciliation state such as:
- `object_id`
- `path`
- `canonical_version`
- `projected_version`
- `projected_text_hash`
- `last_projection_time`

For each managed agent process, the daemon should also track lightweight runtime state such as:
- `agent_instance_id`
- `agent_kind` (`codex`, `claude_code`, etc.)
- `workspace_id`
- `working_directory`
- `process_id`
- `launch_time`
- `last_heartbeat_time`
- `status`
- `assigned_task_ref` if applicable

The simplest writeback algorithm is:
1. detect that a projected file changed on disk
2. load current disk content
3. diff it against the last projected content
4. convert the diff into insert/delete/replace-range operations
5. submit those operations with the last known base version
6. if accepted, update local reconciliation metadata
7. if rejected, refresh from canonical state or escalate to proposal mode

This design intentionally avoids making the raw filesystem itself the collaboration protocol.

At the process layer, the daemon should provide a similarly simple supervision model:
1. receive an instruction or policy to start a local agent process
2. create or attach the correct projected workspace directory
3. launch the agent runtime with the appropriate identity, permissions, and environment
4. observe process health, exit status, and heartbeats
5. stream relevant status back to the server
6. restart, stop, or quarantine the process based on policy

The initial supported local runtimes should explicitly include Codex and Claude Code. The abstraction should remain generic so additional agent runtimes can be added later without changing the core daemon contract.

### 13.4 Why This Matters

This preserves the local filesystem experience for agents without making naïve file overwrite the canonical sync protocol.

---

## 14. Realtime Collaboration Layer

### 14.1 Collaboration Semantics

This layer should behave more like Google Docs than like file sync for the specific object types that support live collaboration.

Its job is to:
- accept small edits,
- stream them live,
- merge concurrent changes,
- and broadcast results to all connected replicas.

For the MVP, this layer is primarily a text collaboration system with presence and comments, not a universal collaboration engine for every artifact type.

### 14.2 Recommended Model

Use:
- optimistic local application,
- server-side sequencing,
- simple admission rules,
- local pending-op queues,
- and realtime subscriptions.

The MVP should use a central authority model for live text/code collaboration. A client applies local edits optimistically, the server assigns authoritative order, and clients rebase pending local edits when remote edits arrive.

That central collaboration server, along with adjacent control-plane services for permissions, admission rules, agent orchestration, and canonical state management, should be implemented in Go for efficiency and portability.

### 14.3 Why Not Pure Peer-to-Peer

Pure peer-to-peer collaboration makes policy, permissions, and enterprise control harder.

A central sequencer is preferable for:
- audit,
- policy enforcement,
- simpler debugging,
- permission validation,
- and deterministic ordering.

---

## 15. Merge Strategy by Object Type

There should not be one universal merge algorithm.

### 15.1 Text and Markdown

For the MVP, use sequence-based collaboration semantics for text:
- insert/delete/replace operations
- insert
- delete
- replace range

This provides live co-editing with deterministic convergence.

### 15.2 Code

For the MVP, code should be treated as text for live collaboration, plus asynchronous validation.

#### Layer 1: Text Merge
- range-based text edits
- sequence semantics for live collaboration

#### Layer 2: Async Validation
- syntax checks
- lint hooks
- test hooks where practical

This avoids putting expensive semantic systems on the critical path of every keystroke.

### 15.3 Spreadsheets

Spreadsheets are out of scope for the MVP.

### 15.4 Filesystem Tree

For the MVP, tree operations should be conservative and mostly serialized:
- create file
- move file
- rename file
- delete file
- create folder
- relink object path

Most non-trivial tree changes should escalate to proposal mode rather than occur in contested live sessions.

Use stable object IDs so moves and renames do not destroy identity.

### 15.5 Comments

Comment operations:
- add thread
- reply
- resolve
- reopen
- mention
- re-anchor if required

---

## 16. Conflict Prevention and Resolution

### 16.1 Most Important Principle

Prevent conflicts whenever possible instead of relying on brute-force resolution after the fact.

For the MVP, the system should prefer admission control over clever reconciliation.

### 16.2 Techniques

#### Presence
Everyone can see who is active where.

#### Intent Signals
Agents can advertise planned scope.

#### Soft Leases
Advisory ownership of a region, symbol, range, or object.

#### Write Budgets
Agents cannot rewrite massive areas without escalation.

#### Proposal Escalation
Large risky edits fork automatically.

#### Admission Rules
The server should reject or escalate live changes that are:
- too large
- overlapping with an active contested region
- cross-file and high-churn
- structural tree operations
- outside the allowed live-capable object types

### 16.3 Types of Conflicts

- text conflict
- tree conflict
- policy conflict

Each must be handled differently.

---

## 17. Git Layer

### 17.1 Role of Git

Git should power:
- durable workspace checkpoints,
- proposal forks,
- merge proposals,
- rollback,
- lineage,
- and historical diffs.

Git should **not** power keystroke-level collaboration.

### 17.2 Checkpointing

The live workspace should periodically serialize into coherent Git commits.

Checkpoint triggers may include:
- manual checkpoints
- task completion
- agent run completion
- scheduled intervals
- before proposal fork
- after proposal merge

### 17.3 Proposal Branches

A proposal workspace maps to a Git branch or equivalent ref.

Example:

```text
refs/workspaces/acme/stable
refs/workspaces/acme/proposals/refactor-auth
refs/workspaces/acme/proposals/q4-forecast-rebuild
```

### 17.4 Merge Behavior

For the MVP, merge behavior only needs to cover:
- text/code diffs
- comments and metadata
- conservative handling of tree changes

### 17.5 Product Value of Git

Git gives the system trusted, time-tested mechanics for:
- forking,
- diffing,
- merging,
- and rollback.

This is particularly valuable for agent-generated large diffs.

---

## 18. Sync Protocol Model

### 18.1 Two Planes

The best design separates:

#### Replica Plane
- local replica durability
- bootstrap / offline catch-up

#### Collaboration Plane
- fine-grained live operations
- presence
- comments
- active-region awareness
- conflict prevention

### 18.2 Why Not Pure Syncthing Semantics

A Syncthing-like model is excellent for file replication and local filesystem preservation, but its file/version conflict behavior is too coarse for realtime collaboration.

### 18.3 Recommended Hybrid

Use a replica mindset for:
- local replicas,
- device identity,
- offline catch-up,

and a Google-Docs-like system for:
- live operations,
- concurrent editing,
- human/agent interaction,
- and shared unfolding work.

For the MVP, the collaboration plane should be scoped to text/code/comments, while the replica plane materializes those states into a normal file tree for agents.

---

## 19. Agent Runtime Architecture

### 19.1 Agents as Principals

Each agent should run as a first-class principal with:
- identity
- policy
- local mounted workspace view
- event subscriptions
- memory and context scope
- write restrictions
- execution budget

### 19.1A Codex / Claude Code as the Execution Layer

The clearest way to think about the runtime is:

- the **workspace** is the shared collaboration and state layer
- the **agent principal** is the coworker identity inside that workspace
- the **execution layer** is a Codex- or Claude-Code-like runtime that carries out work against the mounted working copy

This matters because products like Codex and Claude Code are effective precisely because they can:
- inspect a local codebase or file tree
- edit files directly
- run commands and tests
- iterate based on tool outputs
- operate inside a sandboxed execution environment

The proposed product should not replace that execution pattern. It should **standardize and orchestrate it** inside a shared multiplayer workspace. In other words, Codex / Claude Code are best understood as the agent execution layer, while this product is the shared coordination, state, and review layer above them.

### 19.1B Deployment Model

An agent execution layer may run:
- locally on a user or agent machine
- in a dedicated cloud sandbox
- in an ephemeral proposal workspace sandbox
- in a persistent background worker attached to the workspace

Regardless of deployment model, the runtime should mount the same workspace abstraction and report its actions back through the canonical collaboration and provenance systems.

### 19.2 Runtime Capabilities

Agents should be able to:
- inspect the filesystem tree
- subscribe to file and task changes
- read/write through bounded text and file edit APIs
- create comments
- fork proposals
- respond to reviews
- trigger validations
- run commands, tests, and toolchains through the execution runtime

### 19.3 Bounded Writes

Even if agents see a normal file tree, their writes should be captured as bounded operations such as:
- replace range
- patch block
- create file
- rename file
- add comment

This prevents large blind rewrites from becoming the default mutation pattern.

### 19.4 Agent Storm Protection

Controls should include:
- write budgets
- active region avoidance
- human-priority rules in contested regions
- automatic escalation to proposal mode
- backoff when conflict rate rises

---

## 20. Provenance and Auditability

### 20.1 Non-Negotiable Requirement

Every agent action must be inspectable.

Each operation should carry provenance including:
- author
- author type
- execution ID
- model/tool used
- triggering instruction
- read set / inputs if available
- whether change was autonomous or requested
- confidence and policy tags

### 20.2 User Experience

A user should be able to click any change and understand:
- who made it,
- why it was made,
- what context it used,
- and whether it is trusted, proposed, or awaiting review.

---

## 21. Permissions and Policy

### 21.1 Permissions Beyond Read/Write

Permissions should include:
- read
- comment
- suggest
- edit live
- create proposal
- publish
- approve
- run tool
- assign task

### 21.2 Human vs Agent Policies

Agents should usually have narrower, more capability-based permissions than humans.

Example:
- may edit marketing drafts,
- may comment on finance,
- may not publish finance,
- may run workspace toolchains.

### 21.3 Policy-Aware Collaboration

The collaboration engine should enforce policy before accepting operations into the canonical stream.

---

## 22. Search and Indexing

For the MVP, search and indexing should stay simple.

Indexes should include:
- lexical search
- code symbol search
- change history search
- comment/thread search
- proposal search

Agents should be able to assemble context from this index rather than scanning the entire tree every time.

---

## 23. Product UX Flows

### 23.1 Live Collaborative Drafting

1. Human opens `spec.md`
2. Planner Agent expands outline live
3. Research Agent fills evidence sections
4. Reviewer Agent comments on contradictions
5. Human resolves or rewrites
6. Stable checkpoint created

### 23.2 Large Code Refactor

1. Agent scopes impact: 84 files, active human editors detected
2. System escalates to proposal workspace
3. Proposal fork created from checkpoint
4. Agent performs refactor in side workspace
5. Reviewer agents inspect imports/tests/style
6. Human reviews summarized proposal
7. Merge accepted, then checkpointed

### 23.3 Spreadsheet Rebuild

This workflow should be delayed until after the MVP.

---

## 24. Recommended MVP

### 24.1 Scope

The MVP should focus on the smallest artifact set that proves the thesis.

Recommended first-class support:
- markdown/text
- code files and code directories
- comments and review threads
- live presence
- agent identities
- Git-backed proposals
- provenance and history

Recommended collaboration stack for the MVP:
- product-native text/code editors
- a server-authoritative collaboration service
- a local filesystem projection for agents and toolchains
- proposal workspaces for large or risky changes

### 24.2 What to Delay

Delay until later:
- spreadsheets and other structured artifacts beyond text/code
- broad enterprise integrations
- highly complex permission inheritance models
- full arbitrary filesystem interception across all object types

### 24.3 MVP Success Criteria

The MVP is successful if teams can clearly feel that:
- agents are real collaborators,
- not just tools,
- work is visible as it unfolds,
- large risky work is safely contained,
- and the workspace is easier to operate than today’s fragmented stack.

Concretely, the MVP should let:
- a human edit a markdown or code file in the product UI
- one or more agents observe and update the same file through a filesystem projection
- comments and presence stream live
- large refactors move into proposal mode instead of staying in live mode

The MVP does not need to solve:
- spreadsheets
- media/design workflows
- rich automation ecosystems
- fully general live collaboration for every artifact type

---

## 25. Strategic Positioning

### 25.1 Category Framing

This product should not be framed merely as “AI-native Notion.”

A better framing is:

> A multiplayer operating system for humans and agents.

or

> A live shared workspace where AI coworkers and humans build together.

### 25.2 Why This Matters Strategically

Many existing products add agents onto legacy surfaces:
- docs,
- tickets,
- chat,
- whiteboards,
- databases,
- code editors.

This product’s opportunity is to make the workspace itself agent-native from the ground up.

---

## 26. Core Design Decisions

### 26.1 Decisions Recommended

- Make the filesystem the universal interface.
- Make the backend smarter than a filesystem.
- Stream changes live by default.
- Use type-specific merge logic.
- Use Git for checkpoints, forks, and proposals.
- Treat agents as first-class principals.
- Make Codex/Claude-Code-like runtimes the agent execution layer.
- Preserve provenance for every change.
- Use proposal workspaces to contain large/risky diffs.

### 26.2 Decisions to Avoid

- Do not make raw file overwrite the source of truth.
- Do not make Git the keystroke-level sync engine.
- Do not reduce agents to stateless prompt responders.
- Do not make every collaboration primitive live only inside chat.
- Do not expose conflict copies as the normal workflow.

---

## 27. Open Technical Questions

These questions should be resolved during architecture and prototyping:

1. What is the simplest server-authoritative text collaboration model that gives acceptable latency and reliability?
2. How aggressively should the system auto-escalate from live edits to proposal workspaces?
3. How should comments survive large refactors, moves, or generated rewrites?
4. How much of the local mounted filesystem should be virtual vs directly materialized on disk?
5. How often should live state checkpoint into Git commits?
6. Should proposal workspaces remain live-visible to non-participants by default?

---

## 28. Final Summary

The proposed product is a **shared, live, file-native workspace for humans and AI agents**.

Its defining characteristics are:
- one shared working copy,
- live streaming collaboration for text and code,
- filesystem projection for agent runtimes,
- comments and reviews in context,
- agents as real collaborators,
- Git-backed forks and proposals for large changes,
- and a simple server-authoritative collaboration model for the MVP.

The best implementation is a hybrid:
- **Google-Docs-like collaboration semantics** for text and code,
- **application-native collaborative state** as the source of truth,
- **Git** for lineage and proposal workflows,
- **Codex/Claude-Code-like execution runtimes** for agent action,
- and a **local mounted workspace projection** for agent efficiency.

This is not simply a document editor with AI, nor a repo with extra automation. It is a new model for collaborative work in which the workspace itself is designed for mixed human and agent participation.

If executed well, the product can become:

> the default operating environment for teams composed of both humans and AI coworkers.
