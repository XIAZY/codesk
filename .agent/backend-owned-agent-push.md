# Backend-owned agent notification push

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`.

## Purpose / Big Picture

Agents should wake because the backend created or updated durable notification state, not because the daemon guessed from generic workspace events. After this change, a thread mention or document update creates or advances backend inbox state, the backend publishes a focused `agent.inbox.changed` event to connected daemons, and the daemon wakes the target agent immediately. If the agent is already in a Codex turn, a new for-me item should steer that active turn again rather than being suppressed until the turn finishes.

## Progress

- [x] (2026-05-16T03:40:34Z) Read the existing backend store, websocket broker, daemon workspace event loop, and Codex session supervisor.
- [x] (2026-05-16T03:46:26Z) Added a backend `agent.inbox.changed` event payload and publish helper.
- [x] (2026-05-16T03:46:26Z) Recorded backend inbox changes when thread agent events are created and when document update inbox state is created or coalesced.
- [x] (2026-05-16T03:46:26Z) Routed `agent.inbox.changed` in the daemon to the target agent worker.
- [x] (2026-05-16T03:46:26Z) Simplified busy Codex session wake logic so changed for-me inbox signatures can steer an active turn repeatedly.
- [x] (2026-05-16T03:46:26Z) Added focused tests for backend push change recording and daemon busy-turn steering.
- [x] (2026-05-16T03:46:26Z) Ran backend and daemon tests that cover the changed paths.
- [x] (2026-05-16T03:48:03Z) Ran the Postgres-backed backend test suite against the local test database.
- [x] (2026-05-16T03:48:37Z) Ran the full Go test sweep for all Go packages.

## Surprises & Discoveries

- Observation: The daemon currently wakes all workers for most workspace events, then each worker polls its inbox.
  Evidence: `daemon/internal/syncer/service.go` has `shouldWakeAgentWorkersForEvent` returning true for most events and `wakeAllAgentWorkers`.
- Observation: Busy for-me notifications are intentionally suppressed after one steer per active turn.
  Evidence: `daemon/internal/syncer/agent_sessions.go` checks `session.steeredForMeTurn != session.activeTurn`, and `TestAgentSessionBusyForMeSteersAtMostOncePerActiveTurn` asserts one steer.
- Observation: Document update inbox items are currently partly synthetic: `ListAgentInbox` computes one pending item per changed document from document head versions and per-agent viewed versions.
  Evidence: `backend/internal/notty/store.go` defines `computedDocumentInboxLocked` and synthetic IDs beginning with `docinbox:`.
- Observation: Existing document update notifications near threads used a timestamped dedup key, which allowed repeated document events for the same agent/document.
  Evidence: `enqueueDocumentThreadEventsLocked` previously built `document-edited:<document>:<agent>:<updatedAt>` keys. It now routes through a per-agent/document pending-event upsert.

## Decision Log

- Decision: Treat backend push events as wake signals, not durable state.
  Rationale: The durable source of truth remains Postgres-backed inbox/document state. A websocket event can be missed, so startup/reconnect and explicit wake cycles must still fetch pending inbox items.
  Date/Author: 2026-05-16 / Codex.
- Decision: Remove session-level “once per active turn” suppression for for-me steering.
  Rationale: A stale or long-running turn must still receive later for-me notifications. Suppression by active turn directly caused pending mentions to wait behind a stuck turn.
  Date/Author: 2026-05-16 / Codex.
- Decision: Keep document inbox coalescing in the backend store layer for this change.
  Rationale: The daemon should not decide whether document updates are important or duplicate. Existing backend document head/view state already represents one pending document update item per agent/document/box, and push events can wake agents without adding daemon-side deduplication.
  Date/Author: 2026-05-16 / Codex.

## Outcomes & Retrospective

The backend now records small inbox push events while creating or coalescing notification state and server handlers publish those events as `agent.inbox.changed`. The daemon parses this event and wakes the target agent worker instead of treating generic document/thread websocket events as the primary wake source. Busy Codex turns can now receive another `turn/steer` when the pending for-me inbox signature changes during the same active turn. Focused backend and daemon tests pass.

## Context and Orientation

The backend is the Go service under `backend/internal/notty`. It stores documents, threads, agents, and notification inbox rows. The daemon is the Go process under `daemon/internal/syncer`. It connects to the backend, mirrors documents into local workspaces, and runs long-lived Codex app-server sessions for agents.

A “push notification” in this plan means a websocket event sent from backend to daemon. It is not the notification itself. The notification itself is durable backend state, either in `agent_events` for thread items or in backend document head/view state for coalesced document updates.

The main files are:

- `backend/internal/notty/types.go`, where shared event payload structs live.
- `backend/internal/notty/store.go`, where agent inbox state is created and listed.
- `backend/internal/notty/server_documents.go` and `backend/internal/notty/server_threads.go`, where HTTP/websocket handlers publish events after mutations.
- `daemon/internal/syncer/service.go`, where the daemon reads workspace websocket events and wakes workers.
- `daemon/internal/syncer/agent_sessions.go`, where the daemon starts or steers Codex app-server turns.

## Plan of Work

First, add a small `AgentInboxChangedEvent` payload with workspace, daemon, agent, box, event id, and notification type fields. The store will accumulate these payloads while holding its lock whenever it creates a thread notification or observes that a document update should wake an agent. Server handlers will drain and publish them after successful mutations.

Second, the daemon workspace websocket reader will parse `agent.inbox.changed`. It will refresh workspace metadata, then wake only the target agent worker. Generic workspace events should no longer be the primary wake path for agents.

Third, the Codex session supervisor will stop suppressing for-me steering by active turn. If a busy agent receives a for-me wake and has pending for-me inbox items, it should call `turn/steer` for the current active turn. General-only items can still be queued for a follow-up turn.

Fourth, tests will be adjusted to assert the desired behavior: backend publishes target push events, daemon routes targeted pushes, and a busy active turn can be steered more than once when new for-me notifications arrive.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run focused tests after editing:

    go test ./backend/internal/notty
    go test ./daemon/internal/syncer

If the full package tests are slow, rerun the specific failing test names first, then rerun the full packages before completion.

Observed validation:

    ok  	notty/backend/internal/notty	0.837s
    ok  	notty/daemon/internal/syncer	1.014s
    ok  	notty/backend/internal/notty	8.930s
    go test ./...
    ok  	notty/backend/internal/notty	0.711s
    ok  	notty/daemon/internal/syncer	(cached)
    ok  	notty/internal/yproto	0.269s

## Validation and Acceptance

Acceptance is:

1. A thread mention creates a pending for-me inbox item and publishes `agent.inbox.changed` for the mentioned agent.
2. A document update wakes affected agents through backend-generated inbox changed events, while the inbox remains coalesced to one document item.
3. A busy Codex session receives a `turn/steer` for every new for-me wake instead of only once per active turn.
4. Existing recovery behavior remains: worker startup/reconnect still fetches pending inbox state from backend.

## Idempotence and Recovery

The changes are code-only and safe to rerun. Tests use temporary stores and fake app-server clients. If a test fails, inspect the failing assertion and rerun the specific package after fixing it. No production data migration is required for this pre-MVP system.

## Artifacts and Notes

Validation output and any unexpected behavior will be added here after tests run.

## Interfaces and Dependencies

At completion, `backend/internal/notty/types.go` will define:

    type AgentInboxChangedEvent struct {
        WorkspaceID      string `json:"workspaceId"`
        DaemonID         string `json:"daemonId,omitempty"`
        AgentID          string `json:"agentId"`
        Box              string `json:"box"`
        EventID          string `json:"eventId"`
        NotificationType string `json:"notificationType"`
    }

The backend will publish it inside `EventEnvelope{Type: "agent.inbox.changed", Data: change}`. The daemon will parse that payload and call `wakeAgentWorker(change.AgentID)`.

Revision note, 2026-05-16: Implemented the plan and recorded the test results. The document update path keeps backend-owned coalescing but now writes real pending document events when possible, so repeated updates advance one pending item and still emit a wake signal.
