# Websocket-Only Root CRDT Namespace

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document follows `.agent/PLANS.md` in this repository. It is self-contained so that a contributor can restart from this file alone and understand the intended behavior, the files to edit, and the validation commands to run.

## Purpose / Big Picture

Notty currently treats backend document metadata such as file paths as the namespace authority, while file bytes sync through document CRDT updates. After this change, the workspace namespace is represented by a hidden root CRDT document that syncs over the same document websocket as normal file contents. The backend stores and relays CRDT documents by ID, but does not interpret the root document or project file paths from it.

The user-visible result is that daemon-to-daemon file creates, renames, deletes, and byte edits can converge through websocket CRDT sync only. A person can verify the work by running tests, starting the full stack, creating or editing files in the daemon workspace, and observing the browser/backend/daemon converge without using the removed HTTP CRDT update endpoint.

## Progress

- [x] (2026-06-01 07:29Z) Created this ExecPlan after reviewing `.agent/PLANS.md`, the backend document websocket/update handlers, the daemon mux socket, the local SQLite cache, and the current content reconcile loop.
- [x] (2026-06-01 07:38Z) Added minimal Y.Map support to `internal/ycrdt` and tests for nested map round-trip plus atomic concurrent `loc`; `go test -timeout 20s ./internal/ycrdt` passes with existing macOS linker warnings.
- [x] (2026-06-01 07:41Z) Added backend hidden root document support, exposed `rootDocumentId`, removed HTTP CRDT update route registration/handler, and updated backend tests. `go test ./backend/internal/notty` passes with existing linker warnings.
- [x] (2026-06-01 08:02Z) Changed daemon outbox flushing to send content/root CRDT updates through `/ws/documents-sync`; daemon `SyncStep1` handling now replies with local diffs and clears confirmed outboxes when the server state vector is current.
- [x] (2026-06-01 08:20Z) Added daemon root document decoding, root projection entries in SQLite, root-first dirty reconciliation, local create/move/delete root intents, deterministic same-path projection allocation, and root tombstone projection.
- [x] (2026-06-01 08:24Z) Removed production daemon namespace use of legacy metadata move/delete APIs and removed the unused HTTP-accepted outbox finalizer.
- [x] (2026-06-01 08:28Z) Added/updated focused tests for websocket-only daemon sends, root projection conflict paths, root outgoing-before-incoming ordering, root tombstones instead of backend deletes, hidden backend root behavior, removed HTTP update routes, and root websocket sync.
- [x] (2026-06-01 08:35Z) `go test ./...` passes with existing macOS linker warnings from vendored Yrs.
- [x] (2026-06-01 08:37Z) `npm test` in `frontend` passes: TypeScript check plus 5 Vitest files / 31 tests. Vite emits existing deprecation warnings for esbuild/oxc options.
- [x] (2026-06-01 08:42Z) Rebuilt/restarted Docker backend, daemon, and frontend; verified authenticated backend workspace response exposes `rootDocumentId`, hides `.notty/root`, and returns 404 for workspace-scoped POST `/documents/{id}/updates`.
- [x] (2026-06-01 08:45Z) Browser-verified the frontend at `http://localhost:5173`: registered a new account, created a workspace, opened the workspace shell, created `docs/untitled.md`, and confirmed the visible document list/editor show the normal document rather than the hidden root.
- [x] (2026-06-01 08:50Z) Created a temporary daemon token for an authenticated workspace and verified the rebuilt Docker daemon starts and stays up against the workspace-scoped backend API.

## Surprises & Discoveries

- Observation: The existing mux websocket already uses `(documentID, y-protocol payload)` frames and the backend canonical handler persists inbound websocket `SyncStep2` and `SyncUpdate` messages.
  Evidence: `backend/internal/notty/server_documents.go` decodes document frames in `handleWorkspaceDocumentSyncWebsocket` and persists updates in `handleDocumentProtocolMessageWithStore`.
- Observation: The daemon websocket currently ignores server `SyncStep1`, so it is only a good receiver and initial-sync requester, not a complete bidirectional peer.
  Evidence: `daemon/internal/syncer/document_socket.go` returns nil for `yproto.SyncStep1` in `handleDocumentSyncMessage`.
- Observation: The content reconciler already has the important safety order: durable old outgoing work first, local edit capture next, pending incoming remote updates after local outgoing generation, and disk projection last.
  Evidence: `daemon/internal/syncer/service.go` loads `content_outbox`, then builds new local records, then applies `incoming_updates`, then calls `applyProjectedContent`.
- Observation: The first Y.Map test run hung because tests called `Doc.GetMap` from inside `Doc.Read` or `Doc.Update`, which tries to take the same document mutex already held by the transaction.
  Evidence: `go test -timeout 5s ./internal/ycrdt -run TestYMapStringAndNestedMapRoundTrip -v` timed out with the blocked stack at `Doc.GetMap`; tests now capture root map handles before entering the transaction.
- Observation: Several daemon regression tests were still proving the old HTTP update transport and backend metadata delete behavior rather than the desired CRDT-native behavior.
  Evidence: The tests waited for `/api/documents/{id}/updates`, `PATCH /api/documents/{id}`, or backend deletes; they now use websocket send hooks and assert root tombstones/root entries instead.

## Decision Log

- Decision: Keep the backend name `document` for every syncable CRDT and do not introduce a new `stream` abstraction.
  Rationale: Renaming would add churn without changing behavior. The existing document tables, rooms, routes, and mux frame already describe a CRDT document by ID.
  Date/Author: 2026-06-01 / Codex
- Decision: Remove the HTTP CRDT update endpoint instead of keeping it as a second write path.
  Rationale: The daemon and clients should have one CRDT transport. HTTP remains for bootstrap and compatibility metadata only; CRDT bytes flow over websocket.
  Date/Author: 2026-06-01 / Codex
- Decision: Generate outgoing daemon CRDT updates before applying pending incoming remote updates.
  Rationale: This preserves local filesystem edits as first-class CRDT operations before remote state is allowed to drive projection over disk.
  Date/Author: 2026-06-01 / Codex
- Decision: Keep backend root handling limited to hidden document creation, visibility filtering, and normal websocket sync.
  Rationale: Root projection belongs in clients and daemons. Backend interpretation would duplicate client logic and violate the CRDT-native design.
  Date/Author: 2026-06-01 / Codex

## Outcomes & Retrospective

Focused validation currently passes:

    go test -timeout 20s ./internal/ycrdt
    go test ./backend/internal/notty
    go test ./daemon/internal/syncer

Full validation currently passes:

    go test ./...
    (cd frontend && npm test)

Docker/browser verification also passed after rebuilding the backend, daemon, and frontend images. The API check created an authenticated workspace and observed `rootDocumentId: "doc_root_<workspaceID>"`, zero visible documents, `rootVisible: false`, and HTTP 404 for the removed workspace-scoped CRDT update endpoint. The browser check created a new account/workspace and a visible `docs/untitled.md` document in the UI. A rebuilt daemon container was also started with a temporary workspace daemon token and remained `Up`.

## Context and Orientation

The backend lives in `backend/internal/notty`. A backend `Document` is the persisted identity for a CRDT document. The tables `documents`, `document_heads`, `document_updates`, and `document_checkpoints` store document identity and CRDT update history. `backend/internal/notty/server_documents.go` contains the document websocket handlers, including the muxed websocket endpoint `/ws/documents-sync`. `backend/internal/notty/server_routes.go` registers HTTP and websocket routes.

The daemon lives in `daemon/internal/syncer`. A daemon is a local process that watches a workspace directory, syncs CRDT documents with the backend, and writes projected state to disk. `daemon/internal/syncer/document_socket.go` owns the mux websocket connection. `daemon/internal/syncer/document_cache.go` owns the local SQLite cache. `daemon/internal/syncer/service.go` owns content reconciliation. `daemon/internal/syncer/replica.go` watches filesystem changes and tracks paths to document IDs.

The root document is a hidden backend document whose CRDT state describes the workspace namespace. Hidden means syncable by ID but omitted from visible document lists and path lookup. The backend must not decode this document. Daemons decode it locally and project it to the filesystem before projecting file bytes.

The existing local SQLite `documents.applied_seq` and `documents.projected_seq` columns are reused. For content documents, `projected_seq` means file bytes were written to disk. For the root document, `projected_seq` means namespace entries were projected to disk.

## Plan of Work

First, update the backend with a hidden root document. Add a `Hidden` field to `Document`, add `rootDocumentId` to workspace responses, add a `hidden` column to the `documents` table, create the root document idempotently during store load, and filter hidden documents from visible metadata responses. Remove `handlePostDocumentUpdate` and unregister `POST /documents/{id}/updates` routes. Keep websocket document sync unchanged except for tests that prove root sync works.

Second, add minimal Y.Map support in `internal/ycrdt`. The daemon needs top-level maps and nested maps, string values, deletion, and iteration. Add `Doc.GetMap`, `YMap.GetString`, `YMap.SetString`, `YMap.GetMap`, `YMap.SetMap`, `YMap.Delete`, and `YMap.Entries`, plus tests proving encode/apply convergence and atomic `loc` behavior.

Third, make the daemon a full websocket sync peer. Incoming websocket updates remain passive and are appended to `incoming_updates`. On server `SyncStep1`, the daemon must compute and send `SyncStep2` from the local cached document. Local outgoing root/content updates must be sent through the mux socket as `SyncUpdate`, not posted over HTTP.

Fourth, add a daemon root namespace layer. Define root entry and projection types in a new syncer file. Decode the root Y.Map into desired entries. Detect local filesystem namespace drift against the last successful root projection. Convert drift into root intents. Apply local intents to the root CRDT before pending incoming root updates are applied. Project the merged root state to disk, update tracker maps, and mark affected content document IDs dirty.

Fifth, remove daemon namespace use of legacy metadata move/delete APIs. Content reconciliation should only handle bytes. Local creates may continue to use `POST /api/documents` as a compatibility document ID allocator, but path authority comes from the daemon-created root entry.

Sixth, update tests and run the full suite. Unit tests should cover backend hidden root behavior, removal of HTTP update routes, websocket-only outbound daemon updates, root projection correctness, conflict paths, crash recovery cursors, and the outgoing-before-incoming ordering. Regression tests should cover local create/edit/rename/delete and daemon restart convergence using websocket-only CRDT updates.

## Concrete Steps

Run all commands from `/Users/zhongyangxia/Documents/dev/notty`.

1. Run focused backend tests while changing routes and hidden root behavior:

    go test ./backend/internal/notty

2. Run Y.Map tests:

    go test ./internal/ycrdt

3. Run daemon syncer tests:

    go test ./daemon/internal/syncer

4. Run all Go tests:

    go test ./...

5. Run regression tests:

    go test -tags=regression ./test/regression

6. Start the stack and verify browser behavior:

    docker compose up --build

The observed outputs must be recorded here as work proceeds.

## Validation and Acceptance

The implementation is accepted when these behaviors are demonstrably true:

The HTTP CRDT update endpoint is gone. A POST to `/api/documents/{id}/updates` or `/api/workspaces/{workspaceID}/documents/{id}/updates` does not route to a CRDT write handler.

`GET /api/workspace` includes `rootDocumentId`, and the root document is omitted from the visible `documents` array.

The hidden root document syncs over `/ws/documents-sync` using the same mux document frame as content documents.

The daemon sends local content and root updates over `/ws/documents-sync`, and no production daemon code posts CRDT update bytes to `/api/documents/{id}/updates`.

For root and content reconciliation, outgoing local updates are generated and persisted before incoming remote updates are applied.

Local file create, rename, delete, and edit converge between daemon workspaces. Rename keeps the same content document ID, and delete tombstones the root entry instead of deleting backend content history.

Concurrent same-path creates keep both entries and materialize deterministic conflict paths.

Dirty or unknown local bytes are archived before authoritative root projection deletes or overwrites a path.

Daemon startup recovers when the root document has `applied_seq > projected_seq` or unconfirmed outgoing updates.

The full stack opens in a browser, authenticates, and shows the expected visible workspace without exposing the hidden root document as a normal file.

## Idempotence and Recovery

Postgres migrations must be additive and safe to rerun. SQLite migrations must use `CREATE TABLE IF NOT EXISTS` and additive columns or new tables. Root document creation must be idempotent. Websocket outgoing updates must be idempotent by update hash; reconnects can resend unconfirmed updates without creating divergent CRDT state.

Filesystem projection must treat already-created, already-moved, and already-deleted paths as successful retry states when the daemon can prove the disk state matches either the old projection or desired projection. If projection cannot prove that disk bytes are safe to overwrite or delete, it must archive them before applying authoritative root state.

Projection cursors advance only after filesystem mutation and tracker updates succeed.

## Artifacts and Notes

Important current files:

`backend/internal/notty/server_documents.go` contains the websocket document sync implementation and the HTTP update handler that will be removed.

`backend/internal/notty/server_routes.go` registers routes and must stop registering HTTP CRDT update POST routes.

`backend/internal/notty/store.go` and `backend/internal/notty/store_postgres.go` contain document identity, metadata filtering, root creation, and CRDT update persistence.

`daemon/internal/syncer/document_socket.go` contains the mux websocket client and must become a full sync peer.

`daemon/internal/syncer/document_cache.go` stores local CRDT updates, pending incoming updates, outgoing updates, and projection cursors.

`daemon/internal/syncer/service.go` contains the content reconciler and currently posts outgoing updates over HTTP.

`daemon/internal/syncer/replica.go` watches filesystem namespace changes and currently marks local moves/deletes for metadata API handling.

`internal/ycrdt` contains the Go wrapper over the vendored Yrs FFI and must grow minimal Y.Map support.

## Interfaces and Dependencies

In `internal/ycrdt`, define:

    type YMap struct { ... }
    type YMapEntry struct {
        Key string
        ValueKind string
        StringValue string
        MapValue *YMap
    }
    func (d *Doc) GetMap(name string) *YMap
    func (m *YMap) GetString(txn *Transaction, key string) (string, bool, error)
    func (m *YMap) SetString(txn *Transaction, key string, value string) error
    func (m *YMap) GetMap(txn *Transaction, key string) (*YMap, bool, error)
    func (m *YMap) SetMap(txn *Transaction, key string) (*YMap, error)
    func (m *YMap) Delete(txn *Transaction, key string) error
    func (m *YMap) Entries(txn *Transaction) ([]YMapEntry, error)

In `daemon/internal/syncer`, define root projection types:

    type rootEntry struct {
        EntryID string
        Kind string
        ContentDocumentID string
        ParentID string
        Name string
        Deleted bool
    }

    type rootProjectionEntry struct {
        EntryID string
        Kind string
        ContentDocumentID string
        DesiredPath string
        MaterializedPath string
        Active bool
        ProjectedSeq int64
    }

    type rootIntent struct {
        Kind string
        EntryID string
        ContentDocumentID string
        SourcePath string
        TargetPath string
    }

Change log:

2026-06-01: Initial ExecPlan created from the agreed design: backend documents remain generic CRDT sync units; root is a hidden document; CRDT updates are websocket-only; daemon reconciliation generates outgoing before applying incoming; backend does not project root state.
