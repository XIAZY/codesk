# Reversible Root Tombstone With a Per-Document Reverse Window

Status: proposed canonical design for the follow-up to PR #239

This document defines the backend and daemon contract for automatic same-ID
recovery after an ambiguous local delete. API payloads, persistence, state
machines, failure behavior, compatibility, and the red-first verification gate
are normative unless a later reviewed amendment says otherwise.

## 1. Problem

A local filesystem absence is ambiguous. A real remove and the gap inside an
unlink-plus-rewrite editor save look identical at one instant. The daemon can
debounce and re-observe, but it cannot make a filesystem observation atomic
with a remote namespace commit.

The current root CRDT already has the right reversible representation:

- `deleted=true` tombstones one root entry.
- Upserting that same entry sets `deleted=false`.
- Neither operation deletes the referenced content document or its history.
- The root document itself is the permanent hidden workspace namespace and is
  never the object being tombstoned.

The remaining failure is local identity loss. If a replacement crosses the
daemon's final absence cutoff, the daemon can publish a tombstone, forget the
materialized-path correlation, and later create a new document for the bytes
that reappear. PR #239 head `229e7d0a` prevents byte loss by archiving a late
regular occupant under the exact document ID. This design upgrades that floor
to identity-preserving recovery when the originating local runtime sees its
own path reappear inside a short backend-authoritative window.

## 2. Principle

There is no new delete, soft-delete, purge, finalization, or root-entry
deadline.

The existing tombstone remains the delete. A reverse window is temporary
backend authorization for one daemon runtime to self-correct its own mistaken
local-absence classification. The tombstone still commits and streams
immediately to every root subscriber. Other runtimes project it normally and
never veto it.

V1 deliberately allows at most one current reverse window per content
document. A later accepted tombstone command for that document replaces the
previous window. This keeps backend storage one-to-one with the content
document. The displaced origin falls back to PR #239's byte-safe archive path;
it may lose automatic identity restoration, but it does not lose bytes.

## 3. Scope

### 3.1 Existing byte-safe floor

PR #239 head `229e7d0a` provides the strict-improvement floor:

- confirmed local absence publishes the existing root tombstone;
- root outbox state and exact local-delete intent survive retries and restart;
- a late regular occupant is archived under the exact document ID;
- directory, dangling symlink, FIFO, socket, and other non-content occupants
  are not read or archived as file content.

That frozen head remains the byte-safe implementation base, but the owner chose
on 2026-08-02 to hold PR #239 until this backend-plus-daemon feature is complete.
It must not be retargeted or merged separately. No seal on `229e7d0a` transfers
to the combined implementation head.

### 3.2 This feature

This feature adds:

- one backend reverse-window row per content document;
- backend-time window creation and restore admission;
- auth-derived origin binding and operation idempotency;
- replica-local durable materialized-path correlation;
- namespace-hidden content synchronization into the same content document;
- one backend transaction that consumes the window and performs the ordinary
  same-ID root upsert;
- crash-safe recovery at every boundary.

This is a backend plus daemon feature, not a daemon-only change.

### 3.3 Non-goals

- Making local filesystem observation atomic with remote state.
- Deleting or expiring the root document.
- Preventing a human-authored raw root CRDT operation from manually reversing
  a tombstone.
- Adding a global delete vote or allowing a stale runtime to veto a delete.
- Changing Yjs conflict resolution.
- Compacting or garbage-collecting tombstoned root entries.
- Deleting content documents or their history.
- Retaining multiple simultaneous reverse windows for one content document.
- Adding a user-configurable reverse-window duration in V1.

## 4. Terms

Root document
: The permanent hidden workspace CRDT containing all root namespace entries.

Root entry
: One file entry inside the root document. Current daemon mutations key file
  entries by content-document ID.

Content document
: The CRDT document referenced by a root entry. The backend reverse-window row
  is keyed by this document's ID, never by the root document's ID.

Local runtime
: One filesystem projection owned by a daemon: the primary workspace or one
  agent worktree. Each runtime has its own root directory and its own
  `<runtime-root>/.notty/sync.db`.

Origin
: The authenticated daemon runtime that independently confirmed local absence
  and issued the tombstone command that owns the current reverse window.

Materialized path
: The unique runtime-local path actually projected on disk. It may differ from
  the root entry's desired path because collision planning is local.

Reverse window
: The one current backend row for a content document. It records the
  server-time deadline, owning origin, tombstone operation, and optional
  consumed restore result.

Admission
: The backend transaction that validates the current reverse window and
  content frontier, applies the ordinary root upsert, and marks the window
  consumed.

## 5. Normative invariants

1. A tombstone is globally visible immediately. A reverse window never delays,
   suppresses, or conditions `deleted=true`.
2. The root document is never tombstoned and never receives `reverse_until`.
3. A reverse window is keyed by the referenced content-document ID. At most
   one row exists for that document.
4. A new accepted tombstone operation replaces the prior row for the same
   content document. The latest accepted command owns the only current window.
5. An exact retry of the current tombstone operation does not extend its
   deadline.
6. Origin identity is derived from authenticated daemon context, never trusted
   from JSON.
7. A non-origin runtime projects the streamed tombstone exactly as today.
8. Projection-induced filesystem changes never create a local-delete intent
   or a reverse window.
9. Each local runtime's SQLite database is already isolated. Local rows do not
   need a daemon, agent, or runtime-scope column.
10. The origin retains entry ID, content-document ID, desired path, and exact
    materialized path across projection and restart.
11. Reappeared bytes synchronize into the same content document while the root
    entry remains tombstoned.
12. Backend proof that it incorporated the origin's required content state
    vector precedes restore admission.
13. Window validation/consumption and the accepted root upsert commit in one
    backend transaction.
14. A consumed row remains available until the next tombstone so a lost
    success response can be retried idempotently. A later tombstone supersedes
    that replay result together with the prior window.
15. Backend time is authoritative. A local clock only schedules work.
16. Expiry changes no root or content state. The tombstone remains.
17. A post-expiry file enters the normal local-create pipeline and receives a
    new document ID.
18. A changed entry identity/path or conflicting active path claim defeats an
    automatic restore.
19. A displaced origin or rejected restore preserves a regular local occupant
    through the existing exact-document recovery archive before forgetting the
    correlation.
20. Concurrent ordinary `deleted=false` and `deleted=true` updates converge by
    existing CRDT rules. The reverse-window row adds no merge priority.
21. Restore projection never overwrites a post-proof local change. Changed
    content merges into the same document; a now-absent or non-content path is
    persisted as a fresh local tombstone workflow before any materialization.

## 6. Runtime and origin identity

The source storage grain matters:

- `newWorkspaceRuntime` opens `workspaceSyncDBPath(rootDir)`.
- The primary runtime passes the configured workspace root.
- Each agent runtime passes its agent-worktree root.
- `TestWorkspaceRuntimeSyncDBsArePerLocalWorkspace` proves distinct primary
  and agent `.notty/sync.db` paths and isolated state vectors.

Therefore local origin isolation comes from the database instance itself.

The backend derives the origin from `AuthContext`:

| Authenticated request | Derived origin scope |
| --- | --- |
| daemon token, no acting-agent header | `primary` |
| daemon token plus validated acting-agent header | `agent:<agent UUID>` |

The durable backend identity is `(workspace_id, daemon_id, origin_scope)`. The
existing middleware verifies that an acting agent belongs to the authenticated
daemon. Tombstone-window endpoints require daemon authentication; a human-only
session cannot create or automatically consume a window.

Requests contain no `daemonId`, `agentId`, or `originScope` fields.

## 7. Backend API

The endpoints are semantic commands beside the existing document routes. They
do not accept arbitrary root Yjs update bytes, because a command for one entry
must not smuggle mutations to unrelated entries.

### 7.1 Tombstone and open or replace the window

    POST /api/workspaces/{workspaceID}/root/tombstones

Request:

    {
      "operationId": "UUID",
      "entryId": "root entry id",
      "contentDocumentId": "UUID",
      "expectedDesiredPath": "docs/spec.md"
    }

The daemon persists `operationId` before its first network attempt.

Under the root-document mutation lock and one PostgreSQL transaction, the
backend:

1. derives the origin from authentication;
2. resolves the workspace's permanent root document;
3. locks the root `document_heads` row and loads the latest canonical root;
4. locks the current reverse-window row for `contentDocumentId`, if present;
5. if the current row has the same operation ID and request fingerprint,
   returns that stored result without changing `reverse_until`;
6. rejects same-operation/different-input reuse;
7. validates that the entry is a file, names the requested content document,
   and still has the expected desired path;
8. generates the ordinary `deleted=true` root update when needed;
9. UPSERTs the one reverse-window row with the auth-derived origin and
   `reverse_until = serverNow + 5 minutes`, clearing any prior consumption;
10. persists the root update/head and window UPSERT atomically;
11. commits before broadcasting or responding.

The command may replace a still-open row from another origin. It may also open
a window when the entry is already tombstoned, because another runtime can
independently confirm absence before receiving the streamed tombstone. V1's
single-row rule means the later accepted command supersedes the earlier
window.

Success response:

    {
      "operationId": "UUID",
      "rootDocumentId": "UUID",
      "entryId": "root entry id",
      "contentDocumentId": "UUID",
      "desiredPath": "docs/spec.md",
      "origin": {
        "daemonId": "UUID",
        "scope": "primary or agent:<UUID>"
      },
      "openedAt": "RFC3339Nano",
      "reverseUntil": "RFC3339Nano",
      "serverNow": "RFC3339Nano",
      "rootUpdateId": 123,
      "rootUpdate": "optional base64 Yjs update",
      "applied": true
    }

`rootUpdateId` and `rootUpdate` are omitted and `applied=false` when the entry
was already tombstoned. First acceptance returns 201. An exact retry of the
still-current operation returns 200 with the original deadline. Reuse with a
different request fingerprint returns 409 `operation_mismatch`.

If an older operation is retried after a different operation has replaced its
row, it is no longer the current idempotency record. The backend treats the
newly accepted arrival as the latest command. Daemons must serialize one local
tombstone workflow per content document; cross-runtime ordering remains the
defined latest-accepted-command-wins policy.

### 7.2 Read current window status

    GET /api/workspaces/{workspaceID}/documents/{contentDocumentID}/reverse-window?operationId={tombstoneOperationId}

The request is authenticated as a daemon runtime and names its persisted
tombstone operation. A matching current row returns:

    {
      "status": "open | expired | consumed",
      "serverNow": "RFC3339Nano"
    }

Status is derived as follows:

- `consumed` when `consumed_at` is non-null;
- `open` when unconsumed and `serverNow < reverse_until`;
- `expired` otherwise.

An operation/origin mismatch returns `409 window_superseded` without disclosing
the current origin. Status reads do not mutate root, content, or window state.
Expired and consumed rows remain until another tombstone replaces them; no
cleanup process is required for correctness.

### 7.3 Consume the current window and restore

    POST /api/workspaces/{workspaceID}/documents/{contentDocumentID}/restore-tombstone

Request:

    {
      "tombstoneOperationId": "UUID",
      "restoreOperationId": "UUID",
      "contentStateVector": "base64 Yjs state vector"
    }

`contentStateVector` is captured after the origin merged the current local
regular file into the same content document and received a backend state-vector
acknowledgement. V1 caps its decoded form at 64 KiB.

Under the root-document mutation lock and one PostgreSQL transaction, the
backend:

1. derives the requester origin;
2. locks the root `document_heads` row and loads the latest canonical root;
3. locks the content document's reverse-window row;
4. returns the stored result for an exact consumed restore-operation replay;
5. requires the row's tombstone operation and origin to match;
6. requires `serverNow < reverse_until`;
7. proves that the backend content state vector dominates the required vector;
8. loads the current root entry and requires the same entry identity, file
   kind, content document, and desired path;
9. requires the entry to remain tombstoned and no other active file entry to
   claim the normalized desired path;
10. generates the ordinary same-entry upsert with `deleted=false`;
11. persists the root update and marks the row consumed with
    `restoreOperationId` and the resulting update ID;
12. commits before broadcasting or responding.

Success response:

    {
      "tombstoneOperationId": "UUID",
      "restoreOperationId": "UUID",
      "consumedAt": "RFC3339Nano",
      "rootUpdateId": 124,
      "rootUpdate": "base64 Yjs update",
      "serverNow": "RFC3339Nano"
    }

An exact retry after response loss returns the same success even after the
window boundary, provided no later tombstone replaced the document's row. A
later tombstone instead returns `window_superseded`; a different restore
operation against the same consumed row returns `window_consumed`.

| Status | Code | Meaning |
| --- | --- | --- |
| 404 | `window_not_found` | No row for the content document |
| 404 | `origin_mismatch` | Requester is not the row's auth-derived origin |
| 409 | `window_superseded` | Tombstone operation is no longer current |
| 409 | `window_consumed` | A different restore already consumed the row |
| 409 | `content_not_synced` | Backend state does not dominate the required frontier |
| 409 | `entry_not_tombstoned` | Another operation already reactivated the entry |
| 409 | `entry_changed` | Entry identity, kind, document, or path changed |
| 409 | `path_claim_conflict` | Another active entry claims the desired path |
| 410 | `window_expired` | Backend time reached or passed `reverse_until` |
| 422 | `invalid_state_vector` | Malformed or oversized content frontier |

Transient storage failures return 5xx and leave the window and root at their
pre-request state.

### 7.4 Existing websocket compatibility

Normal Yjs synchronization remains unchanged:

- semantic commands generate ordinary root updates and publish through the
  existing `DocumentRoom`;
- existing UI upsert, move, tombstone, and manual reversal operations remain
  raw CRDT operations;
- the reverse window gates only the daemon's automatic recovery workflow, not
  every possible `deleted=false` CRDT value;
- after rejection or ambiguous response, the daemon never submits an
  unvalidated generic same-ID root upsert.

The hidden-content prerequisite already holds in exact source:
`ensureDocumentSubscribed` gates a websocket document only on `HasDocument`,
and `ApplyCRDTUpdateWithResult` has no root-visibility check. A root entry's
`deleted=true` does not set the referenced content document's `hidden` metadata
or delete it. The daemon can therefore subscribe and synchronize that content
document while its root entry is tombstoned.

## 8. Backend persistence

The one-to-one PostgreSQL table is:

    CREATE TABLE IF NOT EXISTS document_reverse_windows (
        document_id UUID PRIMARY KEY,
        workspace_id UUID NOT NULL,
        root_document_id UUID NOT NULL,
        entry_id TEXT NOT NULL,
        desired_path TEXT NOT NULL,
        origin_daemon_id UUID NOT NULL,
        origin_scope TEXT NOT NULL,
        tombstone_operation_id UUID NOT NULL,
        tombstone_request_fingerprint TEXT NOT NULL,
        tombstone_update_id BIGINT,
        opened_at TIMESTAMPTZ NOT NULL,
        reverse_until TIMESTAMPTZ NOT NULL,
        restore_operation_id UUID,
        restore_update_id BIGINT,
        consumed_at TIMESTAMPTZ,
        created_at TIMESTAMPTZ NOT NULL,
        updated_at TIMESTAMPTZ NOT NULL,
        CONSTRAINT chk_document_reverse_window_time
            CHECK (reverse_until > opened_at),
        CONSTRAINT chk_document_reverse_window_scope
            CHECK (
                origin_scope = 'primary'
                OR origin_scope LIKE 'agent:%'
            ),
        CONSTRAINT chk_document_reverse_window_consumption
            CHECK (
                (consumed_at IS NULL
                    AND restore_operation_id IS NULL
                    AND restore_update_id IS NULL)
                OR
                (consumed_at IS NOT NULL
                    AND restore_operation_id IS NOT NULL
                    AND restore_update_id IS NOT NULL)
            ),
        CONSTRAINT uq_document_reverse_window_tombstone_operation
            UNIQUE (
                workspace_id,
                origin_daemon_id,
                origin_scope,
                tombstone_operation_id
            ),
        CONSTRAINT uq_document_reverse_window_restore_operation
            UNIQUE (
                workspace_id,
                origin_daemon_id,
                origin_scope,
                restore_operation_id
            ),
        CONSTRAINT fk_document_reverse_window_workspace
            FOREIGN KEY (workspace_id)
            REFERENCES workspaces(id)
            ON DELETE CASCADE,
        CONSTRAINT fk_document_reverse_window_document
            FOREIGN KEY (workspace_id, document_id)
            REFERENCES documents(workspace_id, id)
            ON DELETE CASCADE,
        CONSTRAINT fk_document_reverse_window_root
            FOREIGN KEY (workspace_id, root_document_id)
            REFERENCES documents(workspace_id, id)
            ON DELETE CASCADE,
        CONSTRAINT fk_document_reverse_window_daemon
            FOREIGN KEY (workspace_id, origin_daemon_id)
            REFERENCES daemons(workspace_id, id)
            ON DELETE CASCADE
    );

    CREATE INDEX IF NOT EXISTS idx_document_reverse_windows_workspace_until
        ON document_reverse_windows (workspace_id, reverse_until)
        WHERE consumed_at IS NULL;

`document_id` is the exact primary key required by the one-current-window
rule. It is the content document, not the root document. The composite document
foreign key also proves that the content document belongs to the same
workspace. `root_document_id` records the namespace binding used for final
validation.

`origin_scope` is server-derived `primary` or `agent:<UUID>`.
`tombstone_request_fingerprint` is SHA-256 over canonical semantic request
fields. No content bytes or local materialized paths are stored server-side.

`tombstone_update_id` is nullable when the entry was already tombstoned.
Consumed fields remain until the next tombstone UPSERT replaces the row, which
is what makes response-loss replay deterministic without a second history
table.

### 8.1 Atomic store methods

Add dedicated store methods rather than composing a generic CRDT apply with a
later window write:

- `OpenDocumentReverseWindow`
- `DocumentReverseWindowStatus`
- `ConsumeDocumentReverseWindow`

The two mutation methods begin one PostgreSQL transaction, lock the root
`document_heads` row, load and decode the latest canonical root state under
that lock, generate the semantic root update, persist the update/head plus the
window mutation, commit, then publish and update in-memory state. Generating
the semantic update from a pre-lock in-memory snapshot would let a second
backend process validate stale paths or entry state. A root update followed by
a separate window UPSERT would create the forbidden
tombstone-without-correlation state. A standalone status check followed by a
generic websocket upsert would create a restore TOCTOU.

Root and window mutations serialize under the same in-process root-document
lock used by ordinary document updates. The transaction also locks the root
`document_heads` row (`SELECT ... FOR UPDATE`) before the reverse-window row so
multiple backend processes use the same ordering. Every root mutation path,
including generic websocket root updates, must honor that head-row
serialization contract and refresh/merge the locked canonical head before
persisting its result. Broadcast happens only after commit.

### 8.2 Shared root semantics

The backend needs a small pure root helper that can:

- decode `entriesById`;
- normalize a visible root path;
- locate and validate a file entry by entry ID and content-document ID;
- generate the ordinary tombstone and same-ID upsert;
- detect an active normalized path claim.

The semantic endpoints generate their own target-only root update. They never
accept arbitrary Yjs bytes in the JSON body.

## 9. Replica-local persistence

The SQLite database is per local workspace runtime. Extend the existing
`root_local_delete_intents` table; do not add a parallel restore-intent table
and do not add `runtime_scope`.

Because SQLite cannot safely add all required `NOT NULL`, `CHECK`, and index
constraints in place, migrate by creating a replacement table, copying legacy
rows, dropping the old table, and renaming inside one transaction:

    CREATE TABLE root_local_delete_intents_v2 (
        root_document_id TEXT NOT NULL,
        content_document_id TEXT NOT NULL,
        entry_id TEXT NOT NULL DEFAULT '',
        desired_path TEXT NOT NULL DEFAULT '',
        materialized_path TEXT NOT NULL DEFAULT '',
        tombstone_operation_id TEXT,
        opened_at INTEGER,
        reverse_until INTEGER,
        restore_operation_id TEXT,
        required_content_state_vector BLOB,
        observed_file_identity TEXT,
        observed_content_sha256 TEXT,
        phase TEXT NOT NULL DEFAULT 'legacy_floor' CHECK (
            phase IN (
                'legacy_floor',
                'tombstone_pending',
                'window_open',
                'content_syncing',
                'restore_pending',
                'projection_pending'
            )
        ),
        attempts INTEGER NOT NULL DEFAULT 0,
        next_attempt_at INTEGER,
        last_error TEXT,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (root_document_id, content_document_id)
    );

    INSERT INTO root_local_delete_intents_v2 (
        root_document_id,
        content_document_id,
        created_at,
        updated_at,
        phase
    )
    SELECT
        root_document_id,
        content_document_id,
        created_at,
        created_at,
        'legacy_floor'
    FROM root_local_delete_intents;

    DROP TABLE root_local_delete_intents;
    ALTER TABLE root_local_delete_intents_v2
        RENAME TO root_local_delete_intents;

    CREATE UNIQUE INDEX root_local_delete_intents_tombstone_operation
        ON root_local_delete_intents (tombstone_operation_id)
        WHERE tombstone_operation_id IS NOT NULL
          AND tombstone_operation_id <> '';

    CREATE UNIQUE INDEX root_local_delete_intents_restore_operation
        ON root_local_delete_intents (restore_operation_id)
        WHERE restore_operation_id IS NOT NULL
          AND restore_operation_id <> '';

    CREATE UNIQUE INDEX root_local_delete_intents_materialized_path
        ON root_local_delete_intents (
            root_document_id,
            materialized_path
        )
        WHERE materialized_path <> '';

Times use the existing Unix-nanosecond SQLite convention. Cached
`opened_at`/`reverse_until` values schedule checks but never authorize or expire
a restore locally.

The row is both the path/document correlation and durable workflow record.
In-memory watch state is rebuilt from it at startup.

Legacy rows cannot be assigned a trustworthy server window. They migrate as
`legacy_floor` and finish under PR #239's archive behavior. New semantic
tombstones populate the extended fields and persist after root projection.

`storeRootProjectionEntries` currently deletes every local-delete intent for a
root after projection. Change that transaction so it clears only
`phase='legacy_floor'` rows under the existing floor behavior. Feature rows
must survive inactive projection. Their exact materialized path lives in the
intent even when the inactive `root_projection_entries` row has no materialized
path.

## 10. Complete state-machine inventory

The feature is not one state machine. Correctness requires the following
machines to compose without an unowned boundary.

### 10.1 Root-entry state

| State | Event | Result |
| --- | --- | --- |
| Active `deleted=false` | Accepted semantic tombstone | Tombstoned `deleted=true`; window opens atomically |
| Tombstoned | Accepted semantic tombstone from a new operation | Remains tombstoned; current window is replaced |
| Tombstoned | Admitted semantic restore | Active `deleted=false`; window becomes consumed atomically |
| Either | Generic/raw CRDT update | Existing Yjs convergence applies; no window priority is added |

The root document itself has no corresponding delete state. It is permanent.

### 10.2 Filesystem observation and debounce

| State | Observation/event | Transition |
| --- | --- | --- |
| Tracked/present | Missing signal | Record an in-memory missing candidate with path, identity, generation, and debounce deadline |
| Missing candidate | Same-path content observation | Cancel candidate; mark the same document dirty when bytes differ |
| Missing candidate | Create with the missing file identity at another path | Classify as local move; cancel missing candidate |
| Missing candidate | Newer missing generation | Older drain result becomes stale and cannot consume the new candidate |
| Missing candidate | Projection-owned event | Suppress/cancel; never create a local tombstone workflow |
| Ready candidate | Final content-providing observation | Cancel delete and reconcile bytes |
| Ready candidate | Final absent/directory/dangling/FIFO/socket/other observation | Confirm the tracked document is absent without reading non-content objects |
| Ready candidate | Observation error or unstable classification | Retain/retry; do not publish a tombstone |
| Confirmed absence | Durable local workflow insert | Enter `tombstone_pending` |

The classifier has physical states `absent`, `regular`, `directory`, and
`other`; only physical regular and symlink-to-regular observations provide
document content. No stage performs content I/O before this classification.

### 10.3 Backend reverse-window state

| Current row state | Command | Result |
| --- | --- | --- |
| None | New tombstone operation | `open(origin, tombstoneOp, openedAt, reverseUntil)` |
| Open/expired/consumed | Same current tombstone operation and fingerprint | Return stored tombstone result; no deadline change |
| Any row | Same operation with different fingerprint | Reject `operation_mismatch`; no change |
| Open/expired/consumed | Different accepted tombstone operation | Replace row with a new open window; latest accepted arrival wins |
| Open | Matching origin/op restore before deadline with dominated content vector and valid namespace | `consumed(restoreOp, rootUpdate, consumedAt)` plus active root entry in one transaction |
| Open | Failed validation, expiry, or transaction error | Row and root remain unchanged |
| Consumed | Exact restore replay | Return the stored success, even after the old deadline |
| Consumed | Different restore operation | Reject `window_consumed`; no change |
| Consumed | Later accepted tombstone | Replace consumed result with the new open window |

`expired` is derived from backend time, not persisted as a separate mutation.
The row stays consumed/expired until a later tombstone replaces it.

### 10.4 Durable local workflow

| Phase | Durable fact | Next action |
| --- | --- | --- |
| `legacy_floor` | Row predates backend-window support | Complete PR #239 archive/untrack behavior |
| `tombstone_pending` | Stable absence and exact paths were persisted before network | Retry semantic tombstone with the same operation ID |
| `window_open` | Backend accepted this operation as the current per-document window | Retain correlation and watch for content-providing reappearance |
| `content_syncing` | A content occupant reappeared and restore operation ID is durable | Merge current bytes into the same content document and wait for backend frontier acknowledgement |
| `restore_pending` | Backend acknowledged the required content frontier | Retry atomic restore with the same operation IDs |
| `projection_pending` | Backend consumed the window and committed `deleted=false` | Reconcile root and bind the same document/path, then delete the row |

There is no persistent expired state. Own-window expiry removes the correlation
and requeues a still-present content occupant without first moving it, so the
normal create path can assign a new document ID. Supersession, origin mismatch,
or a changed remote namespace instead archives unsafe regular bytes under the
old exact document ID before clearing the stale workflow. An already-active
same-ID/same-path root state finishes binding as success-equivalent.

### 10.5 Hidden-content synchronization and acknowledgement

| State | Event | Transition |
| --- | --- | --- |
| Waiting at correlated path | Non-content/absent occupant | Stay waiting; never open/read it |
| Waiting | Content-providing occupant | Persist restore op, identity, and hash; enter merge pending |
| Merge pending | Current bytes merged into same content document | Durable outbox/send, capture required local vector, enter ack wait |
| Ack wait | Socket write succeeds | Stay in ack wait; transport success is not backend proof |
| Ack wait | Explicit backend `SyncStep1` does not dominate | Resend missing diff/probe; stay in ack wait |
| Ack wait | Backend vector dominates and final file identity/hash is unchanged | Persist required vector; enter `restore_pending` |
| Ack wait | File identity/hash changed | Merge latest bytes and advance the required frontier |
| Ack wait | Disconnect/restart | Reconnect, explicitly probe, and resume from durable local CRDT/outbox state |
| Any pre-restore state | Authoritative expiry/supersession/namespace rejection | Take the outcome-specific terminal path; never locally un-tombstone |

### 10.6 Root projection and local binding

| State | Event | Transition |
| --- | --- | --- |
| Active/tracked | Root tombstone arrives | Enter projection scope, clean pending watcher state, remove/archive as allowed, untrack |
| Origin projecting tombstone | Correlated content already reappeared | Leave bytes in place, retain/rekey correlation, schedule hidden sync |
| Non-origin projecting tombstone | Watcher observes projection removal | Suppress it; no local row/window |
| Tombstoned/untracked origin | Admitted restore commits | Persist `projection_pending`, merge root update, reconcile |
| Projection pending | Latest root is matching active entry and path still provides content | Reserve path, merge extant bytes, bind same document/path |
| Projection pending | Latest root is active but path became absent/non-content after proof | Do not materialize; persist a fresh tombstone operation and compensate through the semantic delete path |
| Projection pending | Later tombstone/move/rebind/path claim won | Do not bind stale restore; archive/yield according to current namespace |
| Active/bound | Projection row and in-memory binding are durable | Delete local workflow row |

Projection scope and durable workflow ownership overlap: there is no point at
which a watcher event can become an independent delete/create while neither
owns the path.

### 10.7 Local-create arbitration

Every content-providing create observation is classified in this order:

1. projection-owned path: suppress and let projection finish;
2. active same-document binding: ordinary dirty/move reconciliation;
3. retained reverse correlation at the materialized path: hidden same-ID
   restore workflow;
4. retained correlation defeated by a newer active namespace claim: archive
   unsafe old-document bytes, release the reservation, then project the winner;
5. authoritative own-window expiry: release correlation, leave bytes in
   place, then normal creation with a new document ID;
6. no binding/correlation: normal create-document pipeline.

The ordering is mandatory. Running normal creation before correlation lookup
recreates the original identity-loss bug.

### 10.8 Persist before network

After debounce and final re-observation confirm absence:

1. load the active root projection row;
2. capture entry ID, content-document ID, desired path, and exact materialized
   path;
3. generate `tombstone_operation_id` once;
4. insert `tombstone_pending` before the first network attempt;
5. transactionally increment `attempts` before each network write, so
   `attempts > 0` durably means the command may have escaped;
6. retry only the semantic tombstone command.

While `attempts = 0`, a confirmed present observation may cancel the row. Once
`attempts > 0`, resolve it by idempotent retry/status even if the process died
between incrementing the counter and writing the socket; never guess and never
fall back to a generic update. Each accepted phase transition resets
`attempts`, `next_attempt_at`, and `last_error`; the pre-send increment rule
applies independently to tombstone and restore commands.

### 10.9 Project the tombstone without forgetting

On success:

1. persist server timestamps and transition to `window_open`;
2. verify the returned content document, operation, and origin match the local
   runtime;
3. apply the returned root update to the incoming cache if present;
4. let ordinary root reconciliation project `deleted=true`;
5. remove normal active tracking after projection;
6. retain the SQLite correlation at its exact materialized path.

A materialized-path lookup intercepts future local-create observations before
they mint a new document ID.

### 10.10 Namespace-hidden content synchronization

When the correlated path provides content:

1. generate and persist `restore_operation_id` plus file identity and SHA-256;
2. classify with the shared occupant classifier;
3. never open a directory, dangling symlink, FIFO, socket, or other
   non-content occupant;
4. temporarily include the same content document in the socket's desired set
   while its root entry remains inactive;
5. rebuild tracked content state from the cached projected base;
6. merge current bytes through the existing CRDT text-diff path into the same
   content document;
7. send an explicit `SyncStep1` probe carrying the local post-merge vector and
   wait until a returned backend `SyncStep1` state vector dominates it;
8. re-read and reclassify the path immediately before restore; repeat merge/ack
   if identity or bytes changed, and stop without opening a non-content object;
9. persist the final required vector and transition to `restore_pending`.

A websocket write is not an acknowledgement. The current backend emits a
`SyncStep1` after a newly applied update, but a duplicate update can be a no-op
and produce no passive acknowledgement; the explicit probe is therefore
required after every send/reconnect. Reconnect may reset transport state but
not the durable phase or required content frontier. A filesystem mutation
after the final pre-request observation is unavoidably concurrent with the
remote commit; after activation it is handled as an ordinary same-document
edit. Projection must bind the extant file through the normal merge path rather
than overwrite it with an older projected snapshot.

### 10.11 Restore and projection

The daemon calls restore only from `restore_pending`.

On success it:

1. stores `projection_pending` before clearing retry state;
2. applies the returned root update;
3. refreshes/reconciles the root CRDT state without materializing this entry;
4. validates that the latest reconciled root is active at the same entry,
   document, and desired path; a later tombstone or namespace change defeats
   this stale projection even if an earlier restore response was successful;
5. reclassifies the correlated path: content continues to same-ID binding,
   changed content merges, while absent/non-content state is never overwritten
   and instead durably becomes a fresh `tombstone_pending` operation;
6. for the content-providing branch, reserves the correlated materialized path
   for the same document so local collision allocation cannot silently change
   identity;
7. on that same content-providing branch, runs root projection in merge/bind
   mode, reuses or reconstitutes same-document tracking, and merges extant
   bytes rather than overwriting them;
8. deletes the intent only after `root_projection_entries` durably contains an
   active same-entry/same-document row at the correlated path and in-memory
   tracking binds the same document/path.

If the process dies after backend success, startup sees `projection_pending`,
the durable correlation, and the latest root state. It finishes projection
only if that state is still the matching active entry. If a later tombstone or
remote namespace change won before local binding, it takes the corresponding
archive/yield path instead.

## 11. Reconciliation-loop integration

The feature uses existing reconciliation stages rather than a parallel loop:

1. `reconcileLocalMetadataOperations` keeps its candidate/debounce/final
   re-observation logic.
2. Its confirmed-absence branch persists `tombstone_pending` and calls the
   semantic tombstone endpoint instead of forgetting correlation after a
   generic root update.
3. `reconcileRootNamespace` still consumes the streamed tombstone everywhere.
   The origin's feature row survives projection; non-origin runtimes have no
   row in their separate SQLite databases.
4. Watcher and periodic-scan create classification first consult the retained
   materialized-path correlation. A match enters `content_syncing` before the
   normal create-document path.
5. The document socket temporarily syncs the inactive same-ID content document
   and exposes a state-vector acknowledgement waiter.
6. A due-work pass advances `window_open`, `content_syncing`,
   `restore_pending`, and `projection_pending` rows. Existing attempts,
   `next_attempt_at`, and `last_error` patterns provide retry scheduling.
7. Own-window expiry exits to normal new-file creation; superseded or
   namespace-conflicted work exits to the byte-safe archive path. Neither case
   writes `deleted=false` locally without backend admission.

The root collision planner treats every retained `materialized_path` as a
reservation. If a genuinely newer active root entry must own that path,
reconciliation first terminates the stale reverse workflow, archives any
unsafe content-providing occupant under the old exact document ID, releases the
reservation, and only then projects the newer entry. It never lets the same
bytes be interpreted simultaneously as a reverse candidate and as a new or
different document.

## 12. Projection guard

Remote tombstone projection must not look like a new local delete. The
projection scope spans filesystem removal/archive, pending-missing cleanup, and
untracking. A watcher event queued during that scope is ignored; after
untracking it cannot resolve to a tracked document. Periodic scan skips a
projecting item and cannot resolve it after untrack.

For the origin, the same projection removes active tracking but preserves the
SQLite correlation. If the correlated path is still absent, projection simply
untracks it. If a regular file or symlink-to-regular reappeared between backend
commit and local projection, origin projection must not archive or delete it;
it leaves the occupant in place and schedules `content_syncing`. A non-content
occupant is left unopened and the correlation remains eligible for a later
content observation. Reappearance is routed through the correlation, not
through a synthetic local-delete event.

The pre-projection race may also have rekeyed the live tracked path. Before
untracking, recovery classifies the deduplicated union of the persisted
materialized path and the current tracked path. With exactly one
content-providing occupant it transactionally rekeys the retained correlation
to that path before untracking. With distinct content-providing occupants at
both paths, automatic identity restore is ambiguous: the existing byte-safe
recovery flow archives both under the exact document ID and the local reverse
workflow yields. A crash at any point retries from the still-durable row; no
regular-file path associated with the document is dropped before correlation
or archive ownership is durable.

## 13. Multi-runtime semantics

### 13.1 Normal delete

Runtime A confirms absence. The backend commits `deleted=true` plus A's current
window and broadcasts the root update. B and C project it normally. Their
self-induced filesystem removals create no local intents or windows.

### 13.2 Origin replacement inside the window

A sees content reappear at its retained materialized path, synchronizes it into
the same content document while hidden, proves backend acknowledgement, and
atomically consumes its current window with `deleted=false`. B and C receive an
ordinary root update and rematerialize the same document.

### 13.3 Two independent origins

A and B can independently confirm absence before receiving each other's root
event. Backend command serialization gives one `document_reverse_windows` row.
The later accepted command replaces the earlier row and becomes the only
origin eligible for automatic same-ID restoration. The displaced runtime
receives `window_superseded` or `origin_mismatch`; it preserves any regular
occupant in the exact-document recovery archive and reconciles current root
state. This is V1's explicit last-accepted-tombstone-wins tradeoff.

### 13.4 Restore versus another delete

Restore admission validates current state under the root lock. A later or
genuinely concurrent ordinary tombstone remains an ordinary CRDT operation.
Existing Yjs convergence determines the namespace result; the window supplies
no timestamp or merge priority.

### 13.5 Remote move, reactivation, or path claim

If identity/path changed or another active entry claims the path, automatic
restore is rejected. Same-document content already synchronized remains valid
document history. Unsafe regular local bytes are archived before correlation
is removed. If the entry is already active with the same document and path,
the daemon reconciles it as a success-equivalent state, finishes same-ID local
binding, and clears the stale workflow; it does not archive or mint a new
document.

### 13.6 Terminal rejection handling

Terminal outcomes do not share one fallback:

| Outcome | Local action |
| --- | --- |
| Own window expired | Remove correlation, leave a content occupant in place, enqueue normal local create so it receives a new document ID |
| Window superseded or origin mismatch | Archive a content occupant under the old exact document ID, remove correlation, and do not let stale presence veto the newer tombstone |
| Entry active with same ID/path | Finish ordinary projection/binding and clear the workflow |
| Entry moved, rebound, or path-conflicted | Preserve unsafe regular bytes in exact-document recovery archive, reconcile remote namespace, then clear workflow |
| Transient backend/storage failure | Keep phase and bytes; retry with bounded backoff |

## 14. Failure and retry semantics

| Failure point | Required recovery |
| --- | --- |
| Crash before local row commit | No command issued; normal scan may classify again |
| Crash after local row, before request | Startup retries same tombstone operation |
| Backend commits tombstone/window, response lost | Exact current-operation retry returns original deadline/result |
| Window UPSERT fails | Root update/head/broadcast remain absent |
| Root persistence fails | Window mutation remains absent |
| Another tombstone replaces the row | Earlier origin archives an unsafe content occupant and yields to the newer tombstone |
| Socket misses root broadcast | Response update or normal state-vector resync repairs cache |
| Crash in `window_open` | Startup rebuilds path correlation and reads status |
| Reappearance during outage | Keep correlation and bytes; do not mint a new document until status resolves |
| Crash before content outbox commit | `content_syncing` rereads and merges |
| Content update commits, ack is lost | Reconnect state-vector handshake proves it; resend is idempotent |
| File changes during content sync | Reread, merge another delta, and advance required frontier |
| Crash after content ack | `restore_pending` retries same restore operation |
| Window expires during preparation | Final admission rejects; root stayed hidden |
| Backend commits restore, response lost | Same restore operation returns stored success |
| Window consume update fails | Root remains tombstoned and window open |
| Root restore persistence fails | Window remains open |
| Crash after restore commit | `projection_pending` revalidates and either finishes same-ID binding or takes the newer-state outcome |
| Crash after projection, before clear | Startup verifies active same-ID/path binding and deletes row |
| Path disappears or becomes non-content after final proof but before bind | Do not rematerialize; persist and send a fresh semantic tombstone operation |
| Local clock jumps | Scheduling may shift; backend status/admission remains authoritative |

## 15. Window

V1 is fixed:

    reverse_until = backend_now + 5 minutes

The backend store/server receives an injected clock in tests. No request field,
daemon setting, workspace setting, or environment override controls V1.

The strict boundary is `backend_now < reverse_until`; equality is expired. A
local timer may prompt status work, but only an authoritative response permits
dropping the correlation as expired.

## 16. Compatibility and rollout

1. Deploy the backend table, semantic API, and capability advertisement first.
2. Advertise `documentTombstoneReverseWindowV1` in workspace capabilities.
3. New daemons use the semantic path only when capability is present.
4. Old daemons continue PR #239's generic tombstone plus byte-safe archive.
5. A new daemon talking to an old backend chooses the floor before persisting a
   semantic command.
6. After a semantic command may have been sent, ambiguous failure is resolved
   only through retry/status, never generic fallback.
7. Migrated `legacy_floor` rows finish under floor behavior and receive no
   synthetic window.
8. Existing UI and generic root CRDT operations remain interoperable.

The owner chose the combined release on 2026-08-02: hold PR #239, implement the
backend and daemon feature on top of `229e7d0a`, then replace every prior seal
with a complete exact-head review of the combined result. Retarget, readiness,
and merge remain blocked until that combined gate is green.

## 17. Observability

Structured logs include tombstone operation, restore operation, root entry,
content document, derived origin, phase, outcome, and duration. They never log
content bytes.

Recommended metrics:

- `document_reverse_windows_opened_total`
- `document_reverse_windows_replaced_total`
- `document_reverse_windows_idempotent_total`
- `document_tombstone_restore_total{outcome}`
- `document_tombstone_restore_rejected_total{code}`
- `document_tombstone_content_ack_wait_seconds`
- `root_local_delete_intent_age_seconds`
- `root_local_delete_intents{phase}`

An offline local row older than its cached deadline is not itself a correctness
alert. Repeated status, content-ack, or projection failures while connected are.

## 18. Implementation map

Backend:

- `backend/internal/notty/server_routes.go`: workspace-scoped semantic routes.
- New `server_root_tombstones.go`: payloads, auth-derived origin, error mapping,
  post-commit broadcast.
- `backend/internal/notty/store.go`: staged root commands, window operations,
  and state-vector dominance helper.
- `backend/internal/notty/store_postgres.go`: one-to-one table, row locking,
  UPSERT/consume, and atomic root persistence.
- Shared/internal root helper: decode, validate, and generate target-only root
  mutations.

Daemon:

- `daemon/internal/syncer/document_cache.go`: migrate/extend
  `root_local_delete_intents`; phase helpers; retain feature rows through
  projection.
- `daemon/internal/syncer/root_namespace.go`: semantic client, origin
  projection behavior, materialized-path claim.
- `daemon/internal/syncer/workspace_runtime.go` and `service.go`: startup phase
  recovery, create interception, and due-work orchestration.
- `daemon/internal/syncer/document_socket.go`: temporary hidden-document
  subscription and state-vector acknowledgement waiter.
- `daemon/internal/syncer/replica.go` and change-index code: projection scope
  and self-induced missing-event suppression.

The frozen PR #239 worktree remains unchanged. This design lives on its own
branch and can be cherry-picked with the eventual implementation.

## 19. Red-first verification gate

Every row starts red against pre-feature code or against the listed causal
mutation. Concurrency-sensitive focused tests run under the race detector.

### 19.1 Test architecture

The gate has four layers. A higher layer never substitutes for a lower one.

1. **Pure unit/model tests:** injected clock, deterministic state transitions,
   decoded state-vector dominance, root helper validation, outcome mapping,
   and create-arbitration ordering. Enumerate every legal transition and assert
   forbidden transitions leave state unchanged. Run bounded generated action
   sequences against a small reference model and assert the invariants in
   section 5 after every action.
2. **Durability and transaction tests:** reopen the real SQLite file after
   every local transition; use real PostgreSQL transactions and two independent
   store/server instances for row-lock serialization and failure injection.
   Assert database rows, canonical root state, update IDs, and broadcast counts,
   not just HTTP codes.
3. **Protocol and filesystem integration tests:** real multiplexed websocket,
   real state vectors, watcher plus periodic scan, real regular/symlink/
   directory/FIFO/socket occupants, and process restart with the same runtime
   root. Test hooks are one-shot barriers at named boundaries; sleeps are only
   bounded liveness deadlines, never transition coordination.
4. **Product regression:** real backend, daemon, primary workspace, and at
   least two agent worktrees. Drive actual unlink/rewrite/rename operations and
   kill/restart processes at every durable boundary. Assert final root CRDT,
   content document ID/text/frontier, each runtime's filesystem projection,
   SQLite/PostgreSQL rows, recovery archives, and absence of duplicate docs.

Every concurrency row runs both legal orders. Every crash row resumes from the
same SQLite/PostgreSQL state rather than rebuilding fixtures. Load-bearing rows
must have a causal mutation that produces the specific forbidden outcome. The
focused suites run at least 100x plain and 20x under `-race`; the final tree
then runs full package/repository plain, race, vet, diff-check, Docker
regression, and hosted exact-head gates.

### 19.2 Backend contract and persistence

| ID | Required proof | Causal mutation that must turn it red |
| --- | --- | --- |
| B1 | Window row is keyed by content-document ID, never root-document ID | Write the workspace root ID as `document_id` |
| B2 | Two files in one root receive independent rows/deadlines | Store one deadline on the root document |
| B3 | One content document has at most one row | Drop the document primary key |
| B4 | New tombstone operation replaces the prior row and origin | Ignore `ON CONFLICT (document_id)` update |
| B5 | Exact current tombstone retry preserves original deadline | Recompute deadline on retry |
| B6 | Same operation with changed binding returns conflict | Remove request fingerprint check |
| B7 | Origin is derived for primary and bound agent auth | Trust origin fields from JSON |
| B8 | Human-only auth cannot create/consume a window | Allow empty daemon identity |
| B9 | Cross-daemon/agent restore is rejected | Remove origin comparison |
| B10 | Tombstone root update and window UPSERT commit together | Persist root first |
| B11 | Injected window-write failure leaves root/head/broadcast unchanged | Ignore UPSERT failure |
| B12 | Injected root-write failure leaves window unchanged | Commit window first |
| B13 | Already-tombstoned command can replace current window without a root delta | Require false-to-true transition |
| B14 | Tombstone broadcasts immediately to all subscribers | Delay broadcast until expiry |
| B15 | Window metadata never appears in root CRDT frames | Write deadline into root entry |
| B16 | Status and admission use injected backend clock; equality is expired | Use client time or accept equality |
| B17 | Expiry changes no root/content state | Add expiry mutation |
| B18 | Malformed/oversized content vector is rejected | Skip decode/size limit |
| B18a | Content updates remain accepted while the root entry is tombstoned | Gate document websocket writes on root visibility |
| B19 | Restore waits for backend vector dominance | Treat socket write as acknowledgement |
| B20 | Restore consume and root upsert commit together | Validate then generic websocket upsert |
| B21 | Consume failure leaves root tombstoned/window open | Mark consumed first |
| B22 | Root persistence failure leaves window open | Ignore root commit failure |
| B23 | Exact restore replay returns prior success after expiry | Check clock before replay identity |
| B24 | Different restore cannot reuse consumed row | Drop restore-operation comparison |
| B25 | Simultaneous consumes produce one committed restore | Remove row lock/conditional update |
| B26 | Entry identity/kind/path/document change rejects restore | Validate only document ID |
| B27 | Active normalized path conflict rejects restore | Remove path-claim scan |
| B28 | Semantic command changes only the target root entry | Accept arbitrary Yjs payload |
| B29 | Restart preserves current window and consumed replay result | Keep window only in memory |
| B30 | Content/workspace/daemon deletion follows FK policy | Remove one declared FK |
| B31 | Two backend processes validate and mutate from the latest locked root head | Generate semantic update from a pre-lock in-memory root snapshot |
| B32 | Composite daemon FK rejects a cross-workspace origin binding | Reference daemon ID without workspace ID |
| B33 | A delayed old operation arriving after replacement follows the explicit latest-accepted-arrival policy | Silently return the displaced result as though it were still current |
| B34 | Duplicate content update followed by explicit SyncStep1 still proves the frontier | Depend only on the passive ack emitted for newly applied updates |

### 19.3 Daemon local state and classifier

| ID | Required proof | Causal mutation that must turn it red |
| --- | --- | --- |
| D1 | Primary and agent runtimes open distinct `.notty/sync.db` files | Reuse primary DB path for agents |
| D2 | Migration preserves legacy rows as `legacy_floor` | Drop rows during rebuild |
| D3 | Stable absence persists exact entry/desired/materialized paths and one op ID before network | Generate operation ID in request code |
| D4 | Restart from `tombstone_pending` retries the same operation | Drop startup phase scan |
| D5 | Ambiguous response never falls back to generic tombstone | Add generic fallback on timeout |
| D6 | Response binding mismatch fails closed | Trust mismatched document/operation |
| D7 | Inactive origin projection retains feature row | Delete all intents in projection transaction |
| D7a | Content reappearing before origin projection is left in place and enters `content_syncing` | Reuse PR #239's archive-on-origin-projection branch |
| D8 | Legacy floor projection still clears legacy row | Retain `legacy_floor` forever |
| D9 | Retained materialized path intercepts create before new document ID | Run normal create queue first |
| D10 | Materialized path is unique across retained correlations | Drop partial unique index/claim check |
| D11 | Regular and symlink-to-regular occupants enter restore | Reject every symlink |
| D12 | Directory, dangling symlink, FIFO, socket never enter content read | Call `ReadFile` before classification |
| D13 | FIFO scenario completes within bounded timeout | Treat any non-directory as content |
| D14 | Hidden same-ID content document is subscribed | Derive desired socket docs only from active root |
| D15 | Current bytes merge into existing document ID | Call create-document endpoint |
| D16 | Socket write without backend vector ack cannot advance phase | Resolve waiter on write success |
| D17 | Dominating backend vector completes waiter | Compare raw vectors for equality |
| D18 | File change during sync forces another merge/frontier | Keep first snapshot |
| D19 | Root restore cannot run before content acknowledgement | Reorder restore before waiter |
| D20 | Restore operation survives restart/response loss | Generate it per retry |
| D21 | `projection_pending` clears only after durable active same-ID/path binding | Clear row on HTTP success |
| D22 | Local clock never independently expires a row | Delete by cached deadline alone |
| D23 | Own-window expiry leaves content bytes in place and requeues normal create with a new ID | Archive/move the file before create classification |
| D24 | Supersession or remote path/identity conflict preserves exact-document bytes without stale recreation | Clear/requeue as a new file before archive |
| D25 | Already-active same-ID/same-path state finishes binding as success-equivalent | Treat every `entry_not_tombstoned` as conflict |
| D26 | No local runtime-scope column or widened PK is required | Open multiple runtimes on one DB in the test fixture |
| D27 | `attempts` is persisted before a request may escape; only `attempts=0` is cancelable | Increment attempts after the socket write |
| D28 | Explicit frontier probe survives duplicate send, reconnect, and daemon restart | Advance on successful websocket write |
| D29 | A later tombstone before `projection_pending` binding defeats the stale local bind | Trust an earlier restore response without rereading root state |
| D30 | Retained materialized paths are resolved before normal create/collision allocation | Mint a new document before correlation lookup |
| D31 | Pre-projection rekey with one content occupant durably rekeys correlation before untrack | Persist only the old path and drop current tracking |
| D32 | Distinct content occupants at persisted and current paths both archive and do not auto-restore | Pick one occupant silently |
| D33 | Active remote path claimant terminates/archives stale correlation before taking the reservation | Project the claimant over the correlated occupant |
| D34 | Post-proof local absence/non-content state compensates with a fresh durable tombstone instead of rematerializing | Project cached content before reclassifying the path |

### 19.4 Projection ownership

| ID | Required proof | Causal mutation that must turn it red |
| --- | --- | --- |
| P1 | Remote tombstone removal stays in projection scope through untrack | End projection before remove |
| P2 | Pending-missing state clears before projection scope ends | Remove change-index cleanup |
| P3 | Late REMOVE after untrack cannot resolve to a document | Keep path binding alive |
| P4 | Periodic scan cannot classify projection removal as local absence | Stop skipping projecting items |
| P5 | Non-origin projection creates no local row or backend window | Route every deleted entry through origin flow |
| P6 | Origin reappearance is correlation-driven create, not repeated delete | Keep inactive file in normal missing loop |
| P7 | Projection scope, retained correlation, and active binding have no unowned watcher interval | End projection before either correlation or binding owns the path |
| P8 | Root update stream-before-response and response-before-stream orders converge identically | Clear correlation on the first arrival |

### 19.5 Original unlink/rewrite race timeline

The original bug is tested by placing the replacement at each ownership
boundary, not by relying on probabilistic timing:

| ID | Replacement timing | Required result |
| --- | --- | --- |
| R1 | Before debounce expires | Missing candidate cancels; no tombstone/window |
| R2 | While the final re-observation is blocked | Present-first order cancels; absent-first order may commit, but retained correlation catches the later replacement |
| R3 | After local row commit with `attempts=0` | Present may cancel only before the durable pre-send attempt marker |
| R4 | After `attempts` increments but before socket write | Idempotent resolution runs; no generic fallback or new document |
| R5 | After backend tombstone/window commit but before HTTP response | Stream-first and response-first orders both retain the path and converge |
| R6 | Before origin tombstone projection/untrack | Occupant remains in place and enters hidden same-ID sync |
| R7 | After origin projection but before the backend deadline | Correlation intercepts create before document creation; same ID restores |
| R8 | While content update is written but before backend frontier proof | Root stays tombstoned; explicit probe/reconnect eventually proves or retries |
| R9 | After frontier proof but before restore request | Restart resumes `restore_pending` with the same operation IDs |
| R10 | After restore commit but before response/local projection | Replay or root sync reaches `projection_pending`; no duplicate root update |
| R11 | After restore response but before durable same-ID binding | Content merges/binds; absent or non-content state becomes a fresh tombstone without rematerialization |
| R12 | Exactly at `reverse_until` | Backend rejects at equality; bytes stay and normal creation gets a new ID |
| R13 | After authoritative expiry | Original stays tombstoned; present bytes become a new document |
| R14 | No replacement | Tombstone remains globally visible; no resurrection |

Each timing row is driven once by watcher events, once with the relevant create
or remove event deliberately omitted so periodic scan owns discovery, and once
with daemon restart between the two actions. Atomic temp-file rename and
unlink-plus-create editor save patterns both use this matrix.

### 19.6 End-to-end scenarios

| ID | Scenario | Required result |
| --- | --- | --- |
| E1 | Real remove, no reappearance | Tombstone reaches all runtimes; window expires; no identity resurrection |
| E2 | Origin replacement inside window | Same content-document ID, bytes synced hidden-first, `deleted=false` streams |
| E2a | Replacement returns before the origin projects its committed tombstone | Occupant remains at its path and enters hidden content sync rather than archive/delete |
| E3 | Replacement exactly at backend boundary | Restore rejected; new-document path handles occupant |
| E4 | Replacement after window | Original remains tombstoned; later file gets new ID |
| E5 | B projects A's tombstone | B removes normally and creates no local intent/window |
| E6 | B has stale local file before stream arrives | B cannot veto A's tombstone |
| E7 | A and B independently tombstone same document | One row; later accepted origin replaces earlier |
| E8 | Displaced origin has a replacement file | Restore rejected; exact-document archive preserves bytes |
| E9 | Current origin restores | Window consumed once; same-ID root upsert commits |
| E10 | Restore races another ordinary delete | Existing CRDT rules converge all runtimes |
| E11 | Remote move while content prepares | Restore rejects; remote path wins; bytes preserved |
| E12 | Another document claims desired path | Restore rejects without overwrite |
| E13 | Backend restarts after tombstone/restore commit | Window and replay result survive |
| E14 | Daemon restarts in every local phase | Workflow resumes with same IDs and no half-visible root |
| E15 | Network loss after every request write | Retry/status resolves without generic fallback |
| E16 | Concurrent content edits during hidden sync | CRDT converges and required frontier is dominated |
| E17 | Primary plus two agent worktrees | Three distinct local DBs; only current backend origin may restore |
| E18 | File changes after final content ack but before root admission | Root activation never overwrites it; normal same-document reconciliation syncs the later edit |
| E18a | File disappears or becomes non-content after final content ack but before local bind | Restore may commit, but projection never recreates/overwrites the path and a fresh tombstone converges globally |
| E19 | Later tombstone lands after restore commit but before origin local binding | Latest root stays tombstoned; stale `projection_pending` never rematerializes it |
| E20 | A create event is lost at every R1-R13 race point | Periodic scan reaches the same identity/data result without blocking |
| E21 | Reappearance is renamed before origin projection | Unique current occupant is retained/rekeyed; two distinct occupants fall back to exact-document archives |
| E22 | Backend process A and B concurrently tombstone/restore the same root | Head-row lock order gives a serializable semantic result and one broadcast per committed update |

### 19.7 Aggregate gates

- Focused backend tests plain/race, repeated.
- Focused daemon lifecycle/classifier/projection tests plain/race, repeated.
- Full backend and daemon-syncer packages plain/race.
- Full repository Go test, vet, build, formatting, and diff-check.
- Real PostgreSQL transaction and failure-injection rows.
- Real filesystem rows for regular file, symlink-to-regular, directory,
  dangling symlink, FIFO, and socket.
- Real websocket multi-runtime convergence suite.
- Process-restart matrix reusing the same SQLite and PostgreSQL state.
- Hosted exact-head backend, full Go, regression, browser, diff-check, and
  Windows cross-build rows.

No prior `229e7d0a` seal transfers. An implementation requires fresh exact-head
source review, causal mutations, aggregate tests, hosted CI, and final
byte-identity verification.
