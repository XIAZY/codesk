# Root CRDT document lifecycle and frontend projection

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This repository contains `.agent/PLANS.md`, and this file follows that guide. It is self-contained so a new contributor can resume the work using only this file and the current tree.

## Purpose / Big Picture

Notty stores collaborative document bytes as CRDT streams, meaning clients exchange mergeable updates rather than overwriting a server-owned copy of a file. The workspace root is now also a CRDT stream. After this change, the visible document tree in the browser and daemon will come from the root stream, not from legacy backend path metadata. The backend will keep one lifecycle command, `POST /documents`, whose only job is to allocate and initialize an empty content document stream. The caller then attaches that stream to the workspace by writing a root CRDT entry, and writes any initial content through the normal document websocket.

Users should be able to create, rename, and delete documents in the browser without calling backend metadata rename/delete APIs. A document creation should allocate an empty backend stream, then the browser should update root and content over websocket. The daemon should keep doing the same for local file creates. The legacy HTTP CRDT update endpoint is already gone, and this plan removes the remaining unused namespace mutation endpoints.

## Progress

- [x] (2026-06-03 03:33Z) Read `.agent/PLANS.md`, inspected backend, daemon, and frontend document lifecycle code, and confirmed current daemon creation already calls `POST /api/documents` with empty content then writes the root entry itself.
- [x] (2026-06-03 03:41Z) Updated backend creation so `POST /documents` ignores request `content`, allocates an empty content stream, allows duplicate path hints, and rolls back in-memory state if persistence fails.
- [x] (2026-06-03 03:42Z) Removed legacy backend namespace mutation endpoints for `PATCH /documents/{id}` and `DELETE /documents/{id}`, including workspace-scoped aliases, handlers, store move/delete methods, and hard-delete bookkeeping.
- [x] (2026-06-03 03:44Z) Added backend regression tests proving create ignores content, duplicate path hints allocate separate streams, removed rename/delete routes are non-OK, and duplicate CRDT updates do not persist.
- [x] (2026-06-03 03:45Z) Added frontend root namespace helpers and tests for root projection, root entry creation, rename, tombstone behavior, malformed entry filtering, and Yjs convergence.
- [x] (2026-06-03 03:45Z) Refactored frontend document sync into `useYDocSync`, subscribed to `workspace.rootDocumentId` through `useRootNamespace`, and rendered/projected visible documents from root entries merged with stream metadata.
- [x] (2026-06-03 03:46Z) Updated frontend create, rename, and delete flows so create uses `POST /documents` only for allocation, then writes root/content CRDT updates over websocket; rename/delete mutate root only.
- [x] (2026-06-03 03:46Z) Removed unused frontend API methods for metadata rename/delete.
- [x] (2026-06-03 03:47Z) Preserved daemon behavior of calling `POST /api/documents` with empty content and writing root entries after stream allocation; existing daemon regression tests passed without requiring a production code change for this pass.
- [x] (2026-06-03 04:04Z) Ran final validation: `go test ./...` passed, and `(cd frontend && npm test)` passed with 6 test files and 35 tests.
- [x] (2026-06-03 04:03Z) Verified with Docker daemon and browser: browser create/rename/delete updated the root-projected UI, and a one-off rebuilt daemon created `docs/daemon-final.md`; reading the root CRDT websocket showed an `entriesById` entry pointing at the daemon-created content stream.

## Surprises & Discoveries

- Observation: `daemon/internal/syncer/service.go` already implements the corrected lifecycle for daemon local creates. It calls `POST /api/documents` with `Content: ""`, then calls `upsertRootFileEntry` for the returned document ID and marks the root/content streams dirty.
  Evidence: `processLocalCreates` calls `createDocumentFromLocalCandidate`, then `upsertRootFileEntry`; `createDocumentFromLocalCandidate` sends an empty `Content` field.
- Observation: The backend still has a unique index on `(workspace_id, path)` and `Store.CreateDocument` rejects duplicate visible paths. That treats create-time path metadata as namespace authority, which conflicts with root projection where conflicts are resolved by clients.
  Evidence: `backend/internal/notty/store_postgres.go` creates `idx_documents_workspace_path`, and `backend/internal/notty/store.go` calls `documentExistsAtPathLocked` inside `CreateDocument`.
- Observation: The browser still renders from `workspace.documents` and still calls `renameDocument`/`deleteDocument`.
  Evidence: `frontend/src/App.tsx` passes `workspace.documents` into the tree and modals, and `frontend/src/api.ts` defines `renameDocument` and `deleteDocument`.
- Observation: The running Docker database still had a legacy partial unique index named `idx_documents_workspace_visible_path`, in addition to the index name found in source.
  Evidence: The first duplicate-path API smoke test returned HTTP 400 with `duplicate key value violates unique constraint "idx_documents_workspace_visible_path"` and showed that memory had already been mutated. The schema now drops both legacy index names, and `Store.CreateDocument` rolls back state on persistence failure.
- Observation: Browser root mutations exposed a CRDT ack loop where the backend kept persisting idempotent 12-byte sync-step replies.
  Evidence: Backend logs showed repeated `document ws inbound update` lines for the same root document and actor at 03:55Z. `ApplyCRDTUpdateWithResult` now restores the current doc, applies the inbound update in memory, compares state vectors, and returns `Applied=false` for idempotent updates.
- Observation: The Browser plugin's Playwright click helpers timed out on this local page, but the DOM-aware CUA helper worked reliably.
  Evidence: `tab.playwright.domSnapshot()` showed the create/options controls, `tab.dom_cua.get_visible_dom()` returned node IDs, and `tab.dom_cua.click` successfully drove create, rename, and delete.

## Decision Log

- Decision: Keep `POST /documents` and make it an allocator/bootstrap command, not a namespace write.
  Rationale: New content document IDs must be registered before websocket sync accepts them. Root projection should remain a normal CRDT stream update performed by the client or daemon, not a backend side effect.
  Date/Author: 2026-06-03 / Codex.
- Decision: Remove backend rename/delete metadata endpoints once callers no longer use them.
  Rationale: Rename and delete are namespace mutations. In the CRDT-native model, namespace mutations belong in the root stream and must converge through websocket sync, not through server-owned metadata RPCs.
  Date/Author: 2026-06-03 / Codex.
- Decision: Keep `GET /workspace` during this work.
  Rationale: Clients still need `rootDocumentId`, daemon/agent/thread state, and content stream heads. The visible document array becomes supplemental compatibility data rather than namespace authority.
  Date/Author: 2026-06-03 / Codex.
- Decision: Do not remove `GET /documents/by-path` in this pass.
  Rationale: Daemon agent tools still call it. Removing it would require a separate agent-tool root projection lookup migration. The user's requirement says unused legacy endpoints should be removed; this endpoint is legacy but still used.
  Date/Author: 2026-06-03 / Codex.
- Decision: Keep `CreateDocumentRequest.Path` required as a compatibility hint instead of introducing a placeholder path fallback in this pass.
  Rationale: The current frontend and daemon both have a path at allocation time, the backend still has a non-null `documents.path` column, and changing request shape is unnecessary for the user-visible CRDT-native namespace behavior. Duplicate path hints are allowed so backend metadata no longer owns namespace uniqueness.
  Date/Author: 2026-06-03 / Codex.
- Decision: Make backend CRDT update application idempotent by comparing state vectors before persisting.
  Rationale: Websocket sync can legally resend updates or send state-vector diff replies that the backend already has. Persisting those no-op updates creates update churn and can loop with state-vector acknowledgements.
  Date/Author: 2026-06-03 / Codex.
- Decision: Use a reusable frontend `useYDocSync` hook plus a one-shot websocket sender for initial content.
  Rationale: Root and content are both CRDT streams, but the editor needs awareness while root does not. A shared hook keeps stream sync behavior consistent, and the one-shot sender keeps `POST /documents` as allocation only while still supporting initial document content.
  Date/Author: 2026-06-03 / Codex.

## Outcomes & Retrospective

Completed on 2026-06-03. The backend now treats `POST /documents` as empty stream allocation, removes HTTP metadata rename/delete routes, and ignores idempotent CRDT updates. The frontend now projects visible documents from the root CRDT stream and performs create/rename/delete namespace changes by mutating root. The daemon path remains aligned with the corrected lifecycle: local creates allocate an empty stream, then write root and content over websocket.

Validation passed with unit/regression tests, Docker API smoke tests, browser interaction checks, and a final daemon root-stream verification. The main remaining technical debt is `GET /documents/by-path`, which remains because agent tooling still uses it.

## Context and Orientation

The backend lives in `backend/internal/notty`. `server_routes.go` registers HTTP and websocket routes. `server_documents.go` contains document lifecycle handlers and websocket sync handlers. `store.go` owns in-memory workspace state and document creation; `store_postgres.go` persists documents, document heads, and updates in Postgres. A "CRDT stream" means a Yjs/Yrs document whose update history is stored in `document_updates` and synchronized over `/ws/documents-sync` or `/ws/documents/{id}`.

The daemon lives in `daemon/internal/syncer`. It mirrors a local filesystem folder to backend CRDT streams. `service.go` detects local file creates and calls backend APIs. `root_namespace.go` decodes and writes the root CRDT map. `document_socket.go` and `document_cache.go` manage websocket sync and local outboxes.

The frontend lives in `frontend/src`. `useDocument.ts` currently syncs one content document using Yjs and the backend websocket. `api.ts` contains HTTP API methods. `App.tsx` renders the workspace and document tree. `logic.ts` reduces workspace metadata websocket events. The package already depends on `yjs` and `y-protocols`, so root projection should reuse those libraries.

The root stream schema used by the daemon is intentionally simple. The Yjs document has a top-level map named `root`; that map contains a nested map named `entriesById`; each file entry is a nested map keyed by entry ID. The entry map stores string keys `kind`, `contentDocumentId`, `loc`, and `deleted`. `kind` is currently `file`. `loc` is a JSON string shaped like `{ "parentId": "", "name": "docs/file.md" }`. `deleted` is the string `"true"` or `"false"`. The content document ID is the backend stream ID for the document bytes.

## Plan of Work

First, adjust backend creation to match the lifecycle contract. In `Store.CreateDocument`, preserve ID allocation, client ID seed creation, initial empty CRDT update creation, activity logging, and persistence. Stop applying `req.Content` to the initial CRDT document. Treat the submitted path as a required compatibility hint only; it is normalized, but duplicate paths do not block allocation. Remove the Postgres unique path indexes so duplicate compatibility hints do not fail persistence.

Second, remove unused backend metadata namespace routes. Delete route registrations for `PATCH /documents/{id}` and `DELETE /documents/{id}` in both workspace-scoped and legacy `/api` route groups. Remove `handleMoveDocument`, `handleDeleteDocument`, and any store methods that become unreferenced. Keep tests covering these paths as 404 or non-OK.

Third, add frontend root namespace helpers. Create a small module that can decode root entries from a `Y.Doc`, project visible `DocumentItem` values by merging root path/title with workspace stream metadata, and mutate root entries for create, rename, and delete. Keep the schema identical to daemon `root_namespace.go`. The helper should be deterministic, ignore invalid entries, and treat tombstoned entries as hidden. It should not call backend metadata mutation APIs.

Fourth, refactor frontend websocket sync into a generic CRDT stream hook. The existing `useDocumentSync` should continue to expose `ydoc`, `ytext`, `ready`, and `connected` for content documents, but root sync should be able to reuse the same websocket protocol code without awareness state. Root sync should subscribe to `workspace.rootDocumentId` and expose decoded projected documents.

Fifth, wire the browser UI to root projection. `App.tsx` should render the projected root documents when the root stream is ready. `workspace.documents` remains supplemental stream metadata and a bootstrap fallback while root is loading. Create should call `POST /documents` with empty content, mutate root with the returned ID/path, mutate the returned content doc with initial text if provided, and select the new document. Rename should mutate root `loc`. Delete should tombstone root. Remove the `renameDocument` and `deleteDocument` methods from `api.ts` once no caller remains.

Sixth, run tests and perform end-to-end verification. Unit tests should prove backend lifecycle behavior and frontend helper behavior. Regression tests should prove legacy endpoints are gone and the frontend no longer calls metadata rename/delete. Docker/browser verification should create, rename, and delete a document in the browser and confirm the daemon-backed workspace converges.

## Concrete Steps

All commands run from `/Users/zhongyangxia/Documents/dev/notty` unless stated otherwise.

Run focused backend tests:

    go test ./backend/internal/notty

Run daemon tests:

    go test ./daemon/internal/syncer

Run CRDT tests:

    go test ./internal/ycrdt

Run all Go tests:

    go test ./...

Run frontend tests:

    cd frontend
    npm test

For Docker/browser verification, rebuild and start the services with the repository's existing compose file. The exact command may change depending on the local environment, so record the final command and observed output in `Artifacts and Notes`.

## Validation and Acceptance

Backend acceptance:

`POST /documents` returns a created document ID whose content stream is empty even when the request includes non-empty `content`. A websocket sync of that returned ID should produce the empty document state until the client sends content over websocket. Two `POST /documents` requests with the same path hint should both allocate IDs; root projection, not backend metadata, resolves namespace conflicts. `PATCH /documents/{id}` and `DELETE /documents/{id}` should return 404 or another non-OK routing response.

Frontend acceptance:

The browser loads `workspace.rootDocumentId`, subscribes to that stream, and renders document rows from root entries. Creating a document should issue one `POST /documents` allocation and then write root/content CRDT updates over websocket. Renaming and deleting a document should not call `PATCH /documents/{id}` or `DELETE /documents/{id}`. The document tree should update from root CRDT state after create, rename, and tombstone.

Daemon acceptance:

A local file create should call `POST /api/documents` with empty content, write a root entry over websocket, and send content bytes over the document websocket. A local rename should update root `loc`; a local delete should tombstone root. The daemon should not call metadata rename/delete APIs.

End-to-end acceptance:

After starting backend, frontend, and daemon, use the browser to create a document with initial content. The created document appears in the UI from root projection, can be opened and edited, and appears in the daemon workspace. Rename it in the UI; the daemon filesystem path changes. Delete it in the UI; the daemon removes or tombstones the local projection and the UI hides it.

## Idempotence and Recovery

The code edits are additive followed by removal of now-unused route handlers and methods. Running tests is safe and repeatable. If Docker services are already running, rebuild/restart commands can be repeated; if a port is already occupied, use the existing running service rather than starting a conflicting one. Do not reset the repository because the tree contains prior unstaged work that this plan builds on.

## Artifacts and Notes

Initial inspection:

    git status --short
     M backend/internal/notty/server_auth_test.go
     M backend/internal/notty/server_documents.go
     M backend/internal/notty/server_routes.go
     ...
     ?? daemon/internal/syncer/root_namespace.go
     ?? internal/ycrdt/map.go

    rg showed `PATCH /documents/{id}` and `DELETE /documents/{id}` are registered only in `backend/internal/notty/server_routes.go` and called from frontend `api.ts`/`App.tsx`.

This section will be updated with test and browser verification transcripts as implementation proceeds.

Final test evidence:

    go test ./...
    ok   notty/backend/internal/notty (cached)
    ok   notty/daemon/internal/syncer (cached)
    ok   notty/internal/ycrdt (cached)
    ok   notty/internal/yproto (cached)

    cd frontend && npm test
    Test Files  6 passed (6)
    Tests  35 passed (35)

Final API smoke evidence:

    {
      "rootVisible": false,
      "created": [201, 201],
      "samePathCount": 2,
      "patch": 404,
      "delete": 404
    }

Browser verification evidence:

    The browser loaded the rebuilt app at http://localhost:5173. DOM CUA created a document through the New document modal; the UI showed `docs/untitled.md` and the editor showed `# Untitled`. The options modal renamed it to `docs/root-renamed.md`; the tree and breadcrumb updated. The same modal then deleted it; the UI returned to the empty workspace state, proving the root tombstone hid it.

Daemon verification evidence:

    A one-off rebuilt daemon ran with `NOTTY_WORKSPACE_ID=ws_a939c4df-9312-480b-bfe0-617be58cbce0` and a pre-existing local file `docs/daemon-final.md`. The authenticated workspace metadata showed one document:

        {"id":"doc_b0e998d3-b952-459a-ab5b-f633f1e96aba","path":"docs/daemon-final.md","updateId":559304}

    Reading the hidden root document over websocket decoded this root entry:

        {
          "key": "doc_b0e998d3-b952-459a-ab5b-f633f1e96aba",
          "contentDocumentId": "doc_b0e998d3-b952-459a-ab5b-f633f1e96aba",
          "path": "docs/daemon-final.md",
          "deleted": "false"
        }

Additional daemon filesystem regression evidence:

    Added `TestWorkspaceRuntimeProjectsRemoteClientLifecycleToFilesystem`, which acts as a non-frontend client by sending root/content Yjs updates through the daemon websocket handler. It verifies remote create, root rename, content edit after rename, and root tombstone all project correctly into the daemon workspace filesystem. The test intentionally keeps the backend document metadata path as `legacy/path-hint.md` while the root CRDT path is authoritative.

    This regression exposed and fixed a daemon bug: a clean root rename changed `DocumentPath`, but content reconciliation exited early when there was no content update, so the filesystem move was skipped. `reconcileTrackedDocument` now treats desired-path mismatch as reconcile work.

    go test ./daemon/internal/syncer -run TestWorkspaceRuntimeProjectsRemoteClientLifecycleToFilesystem -count=1
    ok   notty/daemon/internal/syncer

    go test ./daemon/internal/syncer
    ok   notty/daemon/internal/syncer

    go test ./...
    ok   notty/backend/internal/notty
    ok   notty/daemon/internal/syncer
    ok   notty/internal/ycrdt
    ok   notty/internal/yproto

Complex backend API/daemon filesystem regression evidence:

    Added `TestBackendAPIDocumentLifecycleSyncsDatabaseAndDaemonFilesystem` under `test/regression` with the `regression` build tag. The test starts the real Docker Compose backend, Postgres, and daemon; allocates two documents through `POST /documents`; writes root file entries over the root document websocket; applies repeated content websocket updates with inserts and deletes at the beginning, middle, and end; moves both documents through root CRDT updates; edits again after moves; and tombstones one document. After every operation it reconstructs document/root CRDT state from Postgres and checks the daemon-managed filesystem content or absence.

    This regression exposed and fixed a backend bug: delete-only Yjs updates can change the delete set without advancing the state vector, so the prior idempotence check dropped valid deletes. `ApplyCRDTUpdateWithResult` now compares full encoded document state before/after to detect true no-ops while still using the state vector for sync metadata.

    go test ./backend/internal/notty -run 'TestApplyCRDTUpdate(PersistsDeleteOnlyUpdate|DoesNotPersistIdempotentUpdate)' -count=1
    ok   notty/backend/internal/notty

    go test -tags=regression ./test/regression -run TestBackendAPIDocumentLifecycleSyncsDatabaseAndDaemonFilesystem -count=1
    ok   notty/test/regression 83.898s

    go test ./...
    ok   notty/backend/internal/notty
    ok   notty/daemon/internal/syncer
    ok   notty/internal/ycrdt
    ok   notty/internal/yproto

## Interfaces and Dependencies

Backend:

`backend/internal/notty/types.go` keeps:

    type CreateDocumentRequest struct {
        Path string `json:"path"`
        Content string `json:"content"`
    }

The `Content` field remains temporarily for request compatibility but is ignored by `Store.CreateDocument`. The response remains document metadata so existing clients can obtain `id`, `stateVector`, and `updateId`.

Frontend:

Create `frontend/src/rootNamespace.ts` with functions equivalent to:

    projectRootDocuments(rootDoc: Y.Doc, streamDocuments: DocumentItem[]): DocumentItem[]
    upsertRootFileEntry(rootDoc: Y.Doc, documentID: string, path: string): void
    moveRootFileEntry(rootDoc: Y.Doc, documentID: string, path: string): void
    tombstoneRootFileEntry(rootDoc: Y.Doc, documentID: string): void

Create or refactor a generic sync hook equivalent to:

    useYDocSync({ workspaceId, documentId, token, actorId, actorType, enabled })

`useDocumentSync` should call this hook for content documents and keep its existing return type. A root hook can call it with `workspace.rootDocumentId` and no awareness UI requirements.

Revision note 2026-06-03: Created this plan after the user corrected the lifecycle model: backend creates empty content streams only; clients update root as a regular CRDT stream.

Revision note 2026-06-03: Completed implementation and validation. Updated the plan to reflect the actual path-hint decision, the legacy partial index discovery, idempotent CRDT update handling, and Docker/browser/daemon evidence.

Revision note 2026-06-03: Added remote-client-to-daemon filesystem regression coverage and fixed clean root rename projection so metadata-only root path changes move clean daemon files even when content has no pending updates.

Revision note 2026-06-03: Added real backend/Postgres/daemon lifecycle regression coverage for API allocation, root moves, repeated text inserts/deletes, and tombstone projection. Fixed backend delete-only update persistence by comparing full CRDT state for idempotence instead of state vector only.
