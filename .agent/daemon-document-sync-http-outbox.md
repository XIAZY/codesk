# Refactor daemon document sync to one incoming websocket and HTTP outgoing writes

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` from the repository root. It is intentionally self-contained so a contributor with no prior conversation context can understand and complete the work.

## Purpose / Big Picture

Notty currently opens a document websocket for every workspace replica and every document. A workspace replica is the local filesystem copy for either the daemon's primary workspace or one agent's workspace. With many agents and documents, the daemon creates `(agents + 1) * documents` document websockets. Worse, outgoing edit attribution is inferred indirectly from the local source path and whichever websocket sends the update. That is not a clean model: path and actor identity are different facts.

After this change, each daemon process will maintain one long-lived incoming document websocket per backend document, and local outgoing edits will be sent through a stateless HTTP endpoint with explicit actor identity. Users should see the same collaborative editing behavior, but daemon connection count and attribution logic become simpler and safer. The implementation can be observed through tests and by inspecting that agent workspace replicas no longer own document websocket connections.

## Progress

- [x] (2026-05-18 00:00Z) Wrote this ExecPlan after inspecting the current daemon and backend sync paths.
- [x] (2026-05-18 00:20Z) Added backend HTTP CRDT update endpoint using existing apply/broadcast semantics.
- [x] (2026-05-18 00:20Z) Added backend tests for HTTP ack, canonical empty no-op behavior, actor attribution, and websocket broadcast.
- [x] (2026-05-18 00:35Z) Added daemon `documentSync` incoming websocket manager.
- [x] (2026-05-18 00:40Z) Removed per-replica document websocket ownership and websocket-based outbox sending.
- [x] (2026-05-18 00:40Z) Changed outbox records to store explicit actor identity.
- [x] (2026-05-18 00:45Z) Changed daemon outbox retry to POST raw CRDT bytes over HTTP and clear only on accepted ack.
- [x] (2026-05-18 01:05Z) Updated daemon tests for retry, restart-safe outbox behavior, incoming-before-local reconciliation, and incoming sync append behavior.
- [x] (2026-05-18 01:10Z) Ran focused and broad backend/daemon tests.
- [x] (2026-05-18 02:35Z) Narrowed the remaining correctness work: keep HTTP locking and backend duplicate behavior unchanged for this pass, make `DocumentSync` subscriber-only, and remove daemon metadata state-vector cache truth.
- [x] (2026-05-18 02:55Z) Implemented subscriber-only `DocumentSync` and removed metadata state-vector cache truth.
- [x] (2026-05-18 03:05Z) Verified the subscriber-only change with focused tests and a disposable daemon container.
- [x] (2026-05-18 15:35Z) Replaced self-initializing `state.bin` as known content with a projected-base model that never diffs workspace files without a workspace-local projected base.
- [x] (2026-05-18 15:35Z) Removed obsolete websocket-convergence fields, metadata state-vector residue, and dead cache checkpoint code.
- [x] (2026-05-18 15:50Z) Ran focused unit tests, broad backend/daemon tests, image build, and disposable daemon container verification after the projected-base cleanup.
- [x] (2026-05-18 16:30Z) Simplified the cache bootstrap again: missing or corrupt daemon `state.bin` means backend content is not materialized yet; missing or corrupt workspace projected base means local file state is unknown and must be archived before projection.

## Surprises & Discoveries

- Observation: The existing outbox item stores `SourcePath` but not `ActorID` or `ActorType`.
  Evidence: `daemon/internal/syncer/document_cache.go` defines `outboxUpdateRecord` with `SourcePath` only, and `sendOutboxUpdateProbe` chooses a websocket by matching `tracked.Path`.

- Observation: Backend websocket update handling already contains the core apply-and-broadcast logic needed by an HTTP endpoint.
  Evidence: `backend/internal/notty/server_documents.go` calls `store.ApplyCRDTUpdateWithResult`, broadcasts `yproto.BuildSyncUpdate(update)`, publishes `document.updated`, and publishes inbox changes.

- Observation: Tests that regenerated a projected base CRDT state from plaintext were invalid under the durable outbox model.
  Evidence: A retry test lost the local line until it stored the projected base using the exact shared CRDT state (`baseDoc.EncodeStateAsUpdate()`), which is what production materialization already does. Identical plaintext is not equivalent to identical CRDT causal state.

- Observation: A daemon document websocket can still send cached state back to the backend when it receives a server `SyncStep1`.
  Evidence: `daemon/internal/syncer/document_sync.go` handles `yproto.SyncStep1` by calling `encodeCachedReply`, and the backend applies inbound `SyncStep2`/`SyncUpdate` as document mutations. This violates the intended single outgoing HTTP path.

- Observation: Initializing `state.bin` as an empty CRDT state is not a first-principle bootstrap.
  Evidence: a missing daemon cache is not proof that the backend document is empty. Treating it as empty caused local workspace bytes to be diffed against invented content.

- Observation: The workspace projected base is the only safe authority for local diff generation.
  Evidence: when `base.txt` or `base.state.bin` is missing/corrupt, the daemon cannot prove the active file is a local edit against a known base. The safe generic behavior is to archive that active file and rebuild it from materialized backend content when available.

## Decision Log

- Decision: Keep one daemon reconciliation loop as the only owner of cache, projected bases, outbox creation, outbox retry, and projection writes.
  Rationale: Outgoing edits are a consequence of reconciling a dirty local file against its projected base and the latest shared CRDT base. Splitting outgoing sends into a separate loop would duplicate state and coordination.
  Date/Author: 2026-05-18 / Codex

- Decision: Make `DocumentSync` incoming-only.
  Rationale: Incoming websocket sync and outgoing local edit generation are different concerns. `DocumentSync` should append remote updates to the pending log and mark the document dirty, not mutate local projections or send local edits.
  Date/Author: 2026-05-18 / Codex

- Decision: Use HTTP for outgoing CRDT updates.
  Rationale: The daemon already has raw CRDT update bytes. Incremental CRDT updates do not need a y-protocol handshake. HTTP provides a clear durable acceptance response, which simplifies outbox clearing.
  Date/Author: 2026-05-18 / Codex

- Decision: Store actor identity on the outbox item.
  Rationale: Actor attribution must be explicit. A local file path is not an actor identity, and fallback to another websocket must not change attribution.
  Date/Author: 2026-05-18 / Codex

- Decision: Do not change cache-lock HTTP behavior or backend duplicate-update behavior in this pass.
  Rationale: The user explicitly scoped these out because they are operational/performance concerns rather than the immediate correctness violation, and changing them would add unnecessary risk to this PR.
  Date/Author: 2026-05-18 / Codex

- Decision: Make daemon `state.bin` the only daemon cache state truth, but not a local-edit authority.
  Rationale: Metadata state vectors are derived data and have been removed. If `state.bin` is missing or invalid, backend content is not materialized locally yet. The daemon waits for normal websocket updates instead of inventing empty content.
  Date/Author: 2026-05-18 / Codex

- Decision: Make the workspace projected base the only local-diff authority.
  Rationale: Local edits are computed by comparing the active workspace file to `workspace/.notty/projections/<document>/base.txt` plus its matching CRDT state. Missing/corrupt projected base means the active file has unknown ancestry, so it is archived and the file is rebuilt from backend materialized content when available.
  Date/Author: 2026-05-18 / Codex

- Decision: Archive unknown existing workspace bytes before rebuilding projection.
  Rationale: If projected base is missing or corrupt, the daemon cannot safely generate a CRDT diff from the active file. Preserving those bytes under workspace `.notty/recovered` avoids silent loss while keeping the active document path aligned with backend truth.
  Date/Author: 2026-05-18 / Codex

## Outcomes & Retrospective

Implemented the refactor with less network ownership in workspace replicas. The backend now has `POST /documents/{id}/updates` and legacy `POST /api/documents/{id}/updates` routes that accept raw CRDT update bytes and reuse the same apply/broadcast/event helper as websocket `SyncUpdate`.

The daemon now uses `documentSync` for incoming websocket traffic only. It appends remote CRDT updates to the shared pending log and marks the document dirty. `workspaceReplica` no longer opens or owns document websockets; it watches local files, tracks actor identity on each `trackedFile`, and queues dirty document IDs.

Outgoing local edits now flow through the reconciliation loop: retry existing outbox first, apply pending incoming updates to the cache, inspect dirty local projections, generate one actor-attributed CRDT update, store it durably, POST it over HTTP, and clear it only after an accepted response. Backend failures leave the outbox on disk and requeue the document.

Validation passed:

    go test ./daemon/internal/syncer -count=1
    go test ./backend/internal/notty -run 'TestHTTPDocumentUpdate|TestDocumentProtocolUpdatePublishes|TestWorkspaceEndpointsOmit' -count=1
    go test ./backend/internal/notty ./daemon/internal/syncer -count=1

The Go linker still prints existing y-crdt macOS deployment-target warnings during tests; the test binaries pass.

The subscriber-only `DocumentSync` follow-up is complete. The daemon cache no longer treats missing `state.bin` as known empty content, and workspace reconciliation no longer generates local updates unless a valid projected base exists. Existing active files with missing/corrupt projected bases are archived under `.notty/recovered/<document>` and the document path is rebuilt from materialized backend content once available. The stale metadata state-vector and websocket-convergence fields are gone. This intentionally still does not move HTTP sends outside the cache lock and does not add backend duplicate-update dedupe.

Additional validation passed:

    go test ./daemon/internal/syncer -run 'TestDocumentCache|TestDocumentSync|TestMaterializeTrackedFile|TestReconcileDoesNotSendLocalUpdateWithoutTrustedBase|TestOutgoingOutbox|TestReconcile' -count=1
    go test ./backend/internal/notty ./daemon/internal/syncer -count=1
    docker compose build backend daemon
    docker compose --profile daemon --env-file /private/tmp/notty-daemon-verify.env up -d --force-recreate daemon

The disposable daemon projected a non-empty document correctly, projected an empty document as a zero-byte file, created projected bases for both files, and did not send websocket document updates during initial sync.

## Context and Orientation

The backend lives under `backend/internal/notty`. It exposes HTTP routes in `server_routes.go`, document HTTP and websocket handlers in `server_documents.go`, and persistence/apply logic in `store.go` plus `store_postgres.go`. A CRDT update is the binary Yjs/Yrs update payload representing a document change. The backend stores these updates in Postgres and broadcasts accepted updates to websocket subscribers.

The daemon syncer lives under `daemon/internal/syncer`. The `Service` type in `service.go` starts the daemon, owns the shared `documentCache`, owns the daemon-wide reconciliation queue, and manages agent workspace replicas. A `workspaceReplica` in `replica.go` watches a local filesystem workspace. A `trackedFile` represents one local projection of one backend document. The document cache persists CRDT state, pending remote update logs, and outbox records on disk.

The current code opens document websockets inside `workspaceReplica.connectDocument`, so every replica owns a websocket per document. The daemon-wide reconciliation loop in `Service.Run` drains dirty documents every 2 seconds and calls `reconcileTrackedDocument`. That function applies pending remote updates, computes local outgoing CRDT updates, stores a durable outbox, and currently sends the outbox over an existing tracked websocket.

The desired model keeps the daemon-wide reconciliation loop but removes network ownership from workspace replicas. A new incoming-only `DocumentSync` manager owns one websocket per backend document per daemon. It receives websocket CRDT updates and appends raw update bytes to the pending remote log. The reconciliation loop then applies those pending updates and handles local dirty files.

## Plan of Work

First, add a backend HTTP endpoint for raw CRDT update submission. Add a route under both authenticated workspace routes and legacy unauthenticated routes. Implement a handler that reads a bounded raw request body, derives `OperationMeta` from auth and `X-Notty-Acting-Agent-ID`, calls the same store apply path as websocket `SyncUpdate`, broadcasts accepted updates to document websocket subscribers, publishes workspace and inbox events, and returns `{accepted, applied, updateId}`. Keep the websocket code using the same helper so apply/broadcast behavior cannot diverge.

Second, change daemon outbox records to carry `ActorID` and `ActorType`. Store those fields when creating an outbox from a dirty `trackedFile`. To do that cleanly, put `ActorID` and `ActorType` on `trackedFile` at materialization time from its owning `workspaceReplica`, rather than deriving from source paths later.

Third, add a daemon HTTP send helper that posts raw update bytes to `/documents/{documentID}/updates` through `cfg.workspaceAPIPath`, applying daemon auth and the stored actor identity. The helper should return accepted/applied/update ID. The daemon should clear an outbox only when the response says `accepted: true`. Timeout or non-2xx response leaves the outbox on disk and marks the document dirty for retry.

Fourth, add incoming-only `DocumentSync` management. `Service.refresh` should ensure one `DocumentSync` exists for each active non-ignored document and stop stale syncs. A `DocumentSync` should materialize or load the daemon cache state, open `/ws/documents/{id}` as the daemon actor, send `SyncStep1`, read `SyncStep2` and `SyncUpdate`, append the raw update bytes to the pending remote log, and mark the document dirty. It should reconnect until context cancellation. It must not write workspace files or send outgoing local updates.

Fifth, simplify `workspaceReplica` and `trackedFile` by removing websocket fields, connection methods, `connectDocument`, `readLoop`, and websocket close logic. `ensureTracked` should materialize a tracked file and register it in maps without opening a document websocket. `Run` should still watch filesystem changes, reconcile local create/delete/rename, and send presence through the existing HTTP presence API.

Sixth, delete websocket outbox sender logic. Remove `sendOutboxUpdateProbe`, remove `outboxConverged`, and replace the old convergence-based clearing path with HTTP ack-based clearing. Keep the 2-second ticker and the 60-second full scan. Preserve the ordering that existing outbox is retried before generating a new outgoing update for the same document.

Seventh, update tests. Backend tests should validate HTTP update ack, actor attribution, duplicate/no-op acceptance, and websocket broadcast. Daemon tests should validate outbox actor storage, HTTP ack clearing, HTTP failure retaining outbox, incoming updates applying before local diff generation, and that replicas no longer create document websockets. Existing tests that assume websocket outbox probing should be rewritten or removed if they test obsolete behavior.

## Concrete Steps

From repository root `/Users/zhongyangxia/Downloads/notty`, inspect the current worktree:

    git status --short

Run backend tests after the backend endpoint change:

    go test ./backend/internal/notty -count=1

Run daemon syncer tests after daemon refactor:

    go test ./daemon/internal/syncer -count=1

Run broader Go tests after both halves compile:

    go test ./...

If frontend is untouched, no frontend test is required for this refactor.

## Validation and Acceptance

Acceptance is met when:

1. The backend exposes `POST /api/workspaces/{workspaceID}/documents/{documentID}/updates` and accepts raw CRDT update bytes with daemon authentication.
2. The backend persists accepted updates and broadcasts them to document websocket subscribers.
3. Daemon workspace replicas no longer open `/ws/documents/{id}`.
4. The daemon opens at most one incoming document websocket per backend document.
5. Outbox records contain explicit `ActorID` and `ActorType`.
6. Outbox retry uses HTTP and clears only after `accepted: true`.
7. Tests prove failure keeps the outbox and retrying the same update is safe.
8. `go test ./backend/internal/notty ./daemon/internal/syncer -count=1` passes.

## Idempotence and Recovery

All edits are source changes and test changes. If a test fails halfway through implementation, rerun the specific failing package after fixing it. The daemon outbox format is pre-MVP and may break compatibility; old outbox records without actor identity may be treated as invalid and left for regeneration, or sent as daemon only if that is explicitly simpler and safe. Since the user accepts breaking compatibility and data loss, prefer deleting obsolete compatibility branches over carrying complex migration code.

## Artifacts and Notes

Important code changes:

    daemon/internal/syncer/service.go now owns HTTP outbox retry and finalization.
    daemon/internal/syncer/replica.go no longer opens document websockets.
    daemon/internal/syncer/document_sync.go owns incoming document websocket sync.
    backend/internal/notty/server_documents.go shares apply/broadcast behavior between websocket SyncUpdate and HTTP raw updates.

Expected final shape:

    workspaceReplica: filesystem watcher only.
    DocumentSync: incoming websocket only.
    Service reconciliation loop: cache/outbox/projection owner.
    Backend HTTP endpoint: durable actor-attributed CRDT update acceptance.

## Interfaces and Dependencies

Backend handler to add in `backend/internal/notty/server_documents.go`:

    func (s *Server) handlePostDocumentUpdate(w http.ResponseWriter, r *http.Request)

Backend helper to share websocket and HTTP behavior:

    func (s *Server) applyAndPublishDocumentUpdate(r *http.Request, documentID string, update []byte, meta OperationMeta, exclude *DocumentConn) (*ApplyCRDTUpdateResult, error)

Daemon HTTP helper to add in `daemon/internal/syncer/backend_api.go` or `service.go`:

    func (s *Service) postDocumentUpdate(ctx context.Context, documentID string, record *outboxUpdateRecord) (*postDocumentUpdateResponse, error)

Outbox record shape in `daemon/internal/syncer/document_cache.go`:

    type outboxUpdateRecord struct {
        Update []byte
        UpdateSHA256 string
        TargetStateVector []byte
        ObservedContent string
        ObservedState []byte
        SourcePath string
        ActorID string
        ActorType string
        CreatedAt time.Time
    }

New daemon incoming sync type:

    type documentSync struct {
        cfg Config
        cache *documentCache
        document *document
        markDirty func(string)
    }

The final implementation may adjust names if a simpler shape emerges, but the ownership boundaries must remain the same.
