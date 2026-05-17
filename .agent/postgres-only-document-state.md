# Make backend document state Postgres-only and disposable

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`. It is self-contained so a future contributor can restart from this file without relying on chat history.

## Purpose / Big Picture

Notty stores collaborative documents as CRDT updates. A CRDT update is a binary operation that can be applied by Yjs-compatible clients in any safe order and still converge. The backend currently has two possible authorities for document content: Postgres tables and a retained in-memory `*crdt.Doc` stored on the shared `Document` struct. This can make a fresh browser sync receive stale in-memory state even when Postgres has the correct update history.

After this change, the backend will treat Postgres as the only document authority. The backend may temporarily materialize a CRDT document inside a single function for features that mathematically require plaintext or compact state, such as diffs, server-side replace-text, and checkpoint creation. That temporary document must be discarded before the function returns. A human can see the change working by running the Postgres tests and by verifying that document sync, diffs, and replace-text still work while no shared backend document stores full content or a retained CRDT doc.

## Progress

- [x] (2026-05-17T03:29:11Z) Read `.agent/PLANS.md` and identified the required living-plan sections.
- [x] (2026-05-17T03:29:11Z) Inspected document sync, persistence, checkpoint, and test code paths.
- [x] (2026-05-17T03:29:11Z) Created this ExecPlan with the target architecture and validation approach.
- [x] (2026-05-17T03:43:00Z) Remove the file/JSON store mode so `NewStore` accepts only a Postgres DSN.
- [x] (2026-05-17T03:43:00Z) Remove retained document materialization fields from the shared backend `Document` type.
- [x] (2026-05-17T03:53:00Z) Rewrite document creation, sync, update, replace-text, diff, and checkpoint paths to use Postgres metadata plus scoped CRDT materialization.
- [x] (2026-05-17T03:53:00Z) Remove obsolete file-store tests and rewrite critical sync tests around Postgres-backed temporary CRDT docs.
- [x] (2026-05-17T03:54:00Z) Run formatting, the critical backend/Postgres tests, and broader Go tests.

## Surprises & Discoveries

- Observation: Existing Postgres tests already assert that reloaded documents should not keep `Content`, `CRDTState`, or `Doc` in shared state, but production code still keeps branches that use `document.Doc` when non-nil.
  Evidence: `backend/internal/notty/store_postgres_test.go` checks metadata-only state after reload/sync/update, while `backend/internal/notty/store.go` has `if document.Doc != nil` inside `encodeDocumentCheckpointSyncUpdates`.

- Observation: `ReplaceDocumentText` is the most direct leak path for Postgres mode.
  Evidence: `backend/internal/notty/store.go` restores Postgres history into a local doc, assigns it to `document.Doc`, and later sync can serve from that retained doc.

- Observation: Streaming checkpoint plus every tail update directly through a websocket sync response is correct but fragile under repeated `SyncStep1` calls because it can enqueue many messages before the test/live writer drains.
  Evidence: `TestHandleDocumentProtocolMessageConcurrentSyncAndUpdates` timed out with goroutines blocked in `DocumentConn.Enqueue` while handling `SyncStep1`.

- Observation: A reconnecting client with local-only edits needs the server to send a server state vector after its missing update, and tests must process the full handshake.
  Evidence: A reconnect test initially diverged because the old test assumed exactly two queued messages while the checkpoint-plus-tail path could produce more than two.

- Observation: The websocket path was persisting canonical empty Yjs updates (`[0,0]`) as real document updates.
  Evidence: Postgres test logs showed `canonical_empty_yjs_update=true` followed by `applied=true update_id=...`.

## Decision Log

- Decision: Remove non-Postgres backend store mode instead of maintaining a parallel memory/file implementation.
  Rationale: The user explicitly stated that Postgres is the only datastore. Keeping a second store mode forces the shared `Document` model to carry content and `*crdt.Doc`, which is the root abstraction that allowed stale memory to become authoritative.
  Date/Author: 2026-05-17 / Codex

- Decision: Keep checkpoints, but define them as portable CRDT update artifacts stored in Postgres, never as process memory.
  Rationale: A checkpoint is useful because it lets clients apply one compact update plus a short tail instead of replaying the full update log. It is still just binary CRDT data that frontend and daemon clients can apply through the same protocol.
  Date/Author: 2026-05-17 / Codex

- Decision: Allow materialization only in scoped helper functions for replace-text, diff, and checkpoint creation.
  Rationale: These features need either plaintext or a compact encoded state. The correctness problem is not temporary materialization; it is retaining the materialized object in shared backend state.
  Date/Author: 2026-05-17 / Codex

- Decision: Answer websocket `SyncStep1` by materializing a disposable CRDT doc from the latest persisted checkpoint plus tail, then returning a single Yjs diff update and a server state-vector challenge.
  Rationale: This keeps Postgres as the authority, avoids retained backend state, preserves native y-protocol semantics for clients with local-only edits, and avoids flooding the connection with checkpoint plus every historical tail update.
  Date/Author: 2026-05-17 / Codex

- Decision: Remove `stateVector` from the lightweight `document.updated` event.
  Rationale: The state vector is not cheaply known on every hot-path write without materializing. The event is metadata/notification only; actual document authority is the CRDT websocket and Postgres update history.
  Date/Author: 2026-05-17 / Codex

- Decision: Drop canonical empty Yjs updates at the websocket boundary.
  Rationale: `[0,0]` is a protocol no-op, not a document change. Persisting or broadcasting it creates useless versions and inbox churn without improving convergence.
  Date/Author: 2026-05-17 / Codex

## Outcomes & Retrospective

The backend is now Postgres-only for document authority. Shared `Document` values contain metadata only; plaintext and `*crdt.Doc` values are local temporaries in diff, replace-text, checkpoint, test helper, and websocket sync functions. The websocket sync path reconstructs from persisted checkpoint/tail, computes a single Yjs diff, sends that diff as `SyncStep2`, and sends server `SyncStep1` so reconnecting clients can return local-only edits.

Validation completed:

- `go test ./backend/internal/notty`
- `scripts/test-postgres.sh -timeout 60s`
- `go test ./...`
- `docker compose up -d --build backend`
- `GET http://localhost:8080/healthz` returned `200 {"status":"ok"}`

Remaining risk: arbitrary duplicate CRDT update suppression is no longer done by retained in-memory docs. Canonical empty Yjs updates are ignored at the websocket boundary, and CRDT application is idempotent, so the durable source of truth remains correct. A future performance hardening pass could add content-addressed update dedupe at the Postgres layer.

## Context and Orientation

The backend lives under `backend/internal/notty`. The central store type is `Store` in `backend/internal/notty/store.go`. The durable Postgres implementation is in `backend/internal/notty/store_postgres.go`. Browser and daemon CRDT websocket sync is handled by `backend/internal/notty/server_documents.go`.

The current `Document` type in `backend/internal/notty/types.go` mixes metadata fields, such as `ID`, `Path`, and `UpdateID`, with materialized state fields, such as `Content`, `CRDTState`, and `Doc`. `Content` is plaintext. `CRDTState` is a base64-encoded full CRDT update. `Doc` is a pointer to a live CRDT document from the Go CRDT library. In the target architecture, shared backend `Document` values must contain metadata only.

The source-of-truth tables are:

- `documents`: stable document identity and path metadata.
- `document_heads`: current document update id and latest known state vector.
- `document_updates`: ordered CRDT update bytes.
- `document_checkpoints`: compact CRDT update bytes representing the state through a specific update id.

A state vector is a compact CRDT summary that says which client clocks a document has seen. The server uses it to avoid sending updates the peer already has. A checkpoint is one CRDT update that reconstructs a document state through a known update id when applied to an empty client document.

## Plan of Work

First, make `NewStore` Postgres-only. `backend/cmd/server/main.go` should pass `NOTTY_DATABASE_URL` and fail fast if it is missing. `backend/internal/notty/config.go` can keep `DataFile` only if another component still needs it, but backend store creation should not fall back to JSON.

Second, change `backend/internal/notty/types.go` so `Document` no longer has `Content`, `CRDTState`, or `Doc`. Remove helper methods that derive those fields from a CRDT doc. Keep `DocumentCheckpoint.CRDTState`, because checkpoints are durable Postgres artifacts.

Third, rewrite document creation in `backend/internal/notty/store.go`. `CreateDocument` should create a temporary CRDT doc, capture its initial update/state vector, store metadata in `s.state.Documents`, append the initial update, persist to Postgres, and discard the doc.

Fourth, rewrite sync. `EncodeDocumentSyncUpdates` should restore a local disposable CRDT doc from the latest persisted checkpoint plus tail, compute one Yjs diff against the peer state vector, return that diff, and discard the doc. It must not call `EncodeDocumentUpdate` or inspect a retained document doc.

Fifth, rewrite `ApplyCRDTUpdateWithResult`. The hot path should append the incoming update to `document_updates`, update `document_heads`, publish events, and avoid materialization. It should return metadata. Duplicate/no-op detection can remain a future optimization, but correctness must not depend on materializing or retaining a doc. If a checkpoint is due, checkpoint creation may materialize inside `insertPostgresCheckpointAtHeadTxLocked` and discard immediately.

Sixth, rewrite `ReplaceDocumentText`. It should call a helper that materializes the current document from Postgres into a local CRDT doc, compute a replacement update, persist that update, update metadata, broadcast the update, and discard the doc. It must return metadata plus the update bytes, not a document with content.

Seventh, keep `DiffDocument` and checkpoint creation as scoped materialization. `DiffDocument` should already clone metadata and call `documentContentAtUpdatePostgres`; that pattern is correct. Checkpoint creation should continue to call a helper that materializes from checkpoint plus tail and stores a compact state update.

Eighth, clean tests. Remove obsolete file-store tests or convert critical correctness scenarios to Postgres tests. The important tests are not simple getters/setters; they are document update convergence, checkpoint/tail bootstrap, replace-text, diff reconstruction, restart/reload, and the invariant that no shared `Document` stores content or live CRDT state.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run searches while editing:

    rg -n "Doc \\*crdt|\\.Doc\\b|CRDTState|\\.Content|cloneDocumentWithCRDTState|SyncDerivedFields|SyncProjectionFields|dataFile|NOTTY_DATA_FILE" backend/internal/notty backend/cmd

After each major edit, format Go files:

    gofmt -w backend/internal/notty/*.go backend/cmd/server/main.go

Run targeted tests through the isolated Postgres wrapper:

    scripts/test-postgres.sh

Run broader Go tests if the targeted suite passes:

    go test ./...

If Docker or Postgres is unavailable, record that in this plan and run the compile-only subset that does not require Postgres.

## Validation and Acceptance

Acceptance requires all of the following:

1. `rg -n "Doc \\*crdt|\\.Doc\\b|CRDTState|cloneDocumentWithCRDTState|SyncDerivedFields|SyncProjectionFields" backend/internal/notty` returns no shared document state uses except `DocumentCheckpoint.CRDTState`, diff response content fields, or test helper names that intentionally deal with temporary CRDT docs.

2. `scripts/test-postgres.sh -timeout 60s` passes. The key tests must include merged checkpoint/tail sync, replace-text, diff reconstruction, and metadata-only document invariants.

3. Starting the local stack with `docker compose up -d --build` succeeds and `GET http://localhost:8080/healthz` returns HTTP 200 with `{"status":"ok"}`.

4. The code path in `backend/internal/notty/server_documents.go` for `SyncStep1` has one general path for frontend and daemon clients. It must not branch on human versus agent identity.

## Idempotence and Recovery

This refactor is safe to rerun because it changes code, not production data. The Postgres test wrapper creates and destroys a disposable database named `notty_test`. If a test fails mid-run, rerun `scripts/test-postgres.sh`; the wrapper cleans its own containers unless `NOTTY_POSTGRES_TEST_KEEP=1` is set.

Because this is pre-MVP and compatibility is not required, old JSON state files and old in-memory store behavior are intentionally unsupported after this change.

## Artifacts and Notes

Important pre-change evidence:

    backend/internal/notty/store.go:504
        if document.Doc != nil {
            update := crdt.EncodeStateAsUpdateV1(document.Doc, decoded)
            ...
        }

    backend/internal/notty/store.go:1995
        document.Doc = doc

These lines show how temporary materialization can leak into shared state and later become a sync source.

## Interfaces and Dependencies

At the end of the refactor, the public backend store interface should keep these signatures or close equivalents:

    func NewStore(databaseURL string) (*Store, error)
    func NewStoreForWorkspace(databaseURL string, workspaceID string, workspaceName string) (*Store, error)
    func (s *Store) CreateDocument(req CreateDocumentRequest, meta OperationMeta) (*Document, error)
    func (s *Store) EncodeDocumentSyncUpdates(documentID string, stateVector []byte) (*DocumentMetadata, [][]byte, error)
    func (s *Store) ApplyCRDTUpdateWithResult(documentID string, update []byte, meta OperationMeta) (*ApplyCRDTUpdateResult, error)
    func (s *Store) ReplaceDocumentText(documentID string, nextText string, meta OperationMeta) (*Document, []byte, error)
    func (s *Store) DiffDocument(agentID string, documentID string, fromSpec string, toSpec string) (*DocumentDiff, error)

The implementation of those functions may materialize `*crdt.Doc` values only as local variables. No `*crdt.Doc` pointer should be stored in `WorkspaceState` or `Document`.

Revision note 2026-05-17: Initial plan created before implementation. The plan intentionally removes the file/JSON store because keeping it would preserve the mixed metadata/content model that caused the bug class.
