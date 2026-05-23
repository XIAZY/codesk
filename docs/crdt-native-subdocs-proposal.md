# Notty CRDT-Native Root Manifest, Two-Way Filesystem Sync, and Scan-Accelerated Daemon Proposal

**Date:** 2026-05-22
**Repository context:** `XIAZY/notty`
**Scope:** backend generic CRDT streams, root manifest schema, daemon two-way filesystem projection, SQLite local sync store, SQLite filesystem operation mutex, and low-risk Git-style working-tree scan acceleration.

---

## Executive summary

The design is to make workspace metadata a CRDT stream, make every file's content a separate CRDT stream, and make the daemon a deterministic projector between those CRDT streams and a real local filesystem.

```text
Backend
  Postgres stores generic CRDT streams.
  workspaces.root_stream_id identifies the workspace root manifest Y.Doc.
  File content streams are Y.Docs keyed by documentID/contentStreamID.
  Backend validates and stores CRDT streams, but does not own path uniqueness.

Daemon
  .notty/state.sqlite stores durable stream state, inbox, outbox, projection state, scan metadata, and fs jobs.
  .notty/fslock.sqlite serializes Notty-owned filesystem operations.
  RootManifestProjector syncs filesystem namespace <-> root manifest stream.
  ContentProjector syncs local file bytes <-> content streams.
  WorkspaceFS performs safe read/write/move/delete/archive operations.
  WorkspaceFS.Scan is stat-first, byte-lazy, hint-aware, capability-aware, and bounded.
```

The key split is:

```text
Root manifest answers:
  Which stable entries exist, and where do they want to live?

Content streams answer:
  What bytes does each file entry contain?

Filesystem projection answers:
  Given those facts and local dirty state, what safe path operations can be performed now?
```

The scan acceleration in this version borrows Git's cheapest working-tree ideas only: cached stat metadata, path/dir/full hints, directory mtime caches, strong file identity for moves, and bounded clean-hash fallback. It does **not** borrow Git's object database, staging model, or fuzzy rename similarity.

---

## Review-driven corrections integrated in this version

This version explicitly incorporates the following implementation safety fixes:

1. `FileKey` is part of stat equality. If `FileKey` changes, is missing, or is not reliable, `ContentProjector` must not short-circuit on stat equality.
2. The only normative `RootManifestProjector.CaptureLocal` and `ContentProjector.CaptureLocal` pseudocode is stat-first and byte-lazy. There is no stale no-arg whole-tree `p.FS.Scan()` path or byte-eager root scan.
3. `scan_state.file_key_reliable` has a required startup probe. On unreliable filesystems, FileKey move detection and content stat short-circuiting are disabled.
4. `scan_hints` has a hard pending-row cap. If the cap is exceeded, pending hints collapse into one `full` hint. `directory_scan_cache` has child-count and JSON-size caps.
5. Composite stat indexes are intentionally omitted. Stat fields are row data compared after lookup.
6. Stat-cache columns and scan tables are part of the initial `state.sqlite` schema from day one, even if scan acceleration is wired later.
7. `WorkspaceFS.Scan` has explicit fallback behavior when directory mtimes and FileKeys are unreliable.
8. Local create is two-phase: root scan discovers path/stat; a later bounded targeted step reads bytes only for pending creates.
9. Pending content creates have a complete lifecycle and reaper. Rows are deleted only after the dependent content-init outbox is acknowledged, the content stream has a projected state, and the manifest projection is no longer pending.
10. Cross-stream outbox dependencies are defined explicitly. Root-create and content-init rows live in one outbox table, and dependency resolution is cross-stream-aware.
11. Directory operations are specified for local create, local rename, local `rm -rf`, remote rename, remote delete, and orphan recovery.
12. Periodic full-scan scheduling is explicit and uses `scan_state.last_full_scan_at`.
13. `scan_state` singleton initialization and capability probes run on first daemon boot.
14. `content_projection.materialized_path` is unique. Pending-create stat rows and content-outbox references have schema constraints.
15. Bulk imports and bulk renames on filesystems without reliable FileKey are deliberately conservative v1 limitations.

---

# Part A: Core invariants

These invariants drive backend, daemon, frontend, and tests.

```text
1. entryID/documentID is identity.
2. Path is mutable desired metadata, never identity.
3. A file's contentStreamID is stable across rename/move.
4. For current Notty, contentStreamID normally equals documentID.
5. The root manifest may validly contain duplicate desired paths.
6. Duplicate desired paths are resolved during projection, not by mutating root.
7. Deletes are tombstones until explicit garbage collection.
8. Filesystem events are wakeups; reconciliation uses snapshots.
9. Local mutations are state assignments, not imperative events.
10. Outgoing CRDT updates are durably recorded before network I/O.
11. Filesystem projection is done through WorkspaceFS preconditions.
12. Dirty or unknown local bytes are never overwritten or silently deleted.
13. No behavior depends on file purpose or path name, such as log, markdown, generated, or agent file.
14. Scan acceleration is an optimization only. If it is uncertain, fall back to safe conservative behavior.
```

The most important rule is:

```text
Path is not identity.
```

A path is a projection constraint. A document/entry ID is identity.

---

# Part B: Root manifest model

## B1. Root manifest stream

Each workspace has exactly one root manifest stream:

```text
workspace.root_stream_id == root Y.Doc guid
stream_id == ydoc.guid
```

The root stream contains metadata only, not file content bytes.

Conceptual Yjs shape:

```ts
const entriesById = root.getMap<Y.Map<any>>("entriesById")
const documentRefs = root.getMap<Y.Doc>("documentRefs") // optional convenience
```

The first implementation can call this `documentsById` if it only supports files, but the long-term schema should be `entriesById`, because real filesystem sync eventually needs first-class directories.

## B2. Entry schema

### Directory entry

```ts
type RootDirEntry = {
  id: string
  kind: "dir"

  // null only for the root directory
  loc: null | {
    parentId: string
    name: string
    normName: string
  }

  tombstone?: Tombstone

  createdBy?: string
  updatedBy?: string
  createdAt?: string
  updatedAt?: string
}
```

### File entry

```ts
type RootFileEntry = {
  id: string
  kind: "file"

  loc: {
    parentId: string
    name: string
    normName: string
  }

  contentStreamId: string

  tombstone?: Tombstone

  createdBy?: string
  updatedBy?: string
  createdAt?: string
  updatedAt?: string
}
```

### Tombstone

```ts
type Tombstone = {
  actorId: string
  actorType: "human" | "daemon" | "agent"
  at: string
}
```

## B3. Identity mapping

For Notty v1:

```text
entryID == documentID
contentStreamID == documentID
content Y.Doc guid == documentID
```

Example:

```ts
entriesById.set("doc_123", {
  id: "doc_123",
  kind: "file",
  loc: {
    parentId: "dir_docs",
    name: "spec.md",
    normName: "spec.md",
  },
  contentStreamId: "doc_123",
})
```

`documentRefs` may optionally contain:

```ts
documentRefs.set("doc_123", new Y.Doc({ guid: "doc_123" }))
```

Authority is still `entriesById[docID].contentStreamId`. The subdoc reference is convenience for clients that want native Yjs subdoc events.

## B4. Flat ID table, not path map

Do not make path the authoritative key:

```ts
// Bad as authority
filesByPath.set("docs/spec.md", doc)
```

Use ID authority:

```text
entriesById
  doc_a -> wants README.md
  doc_b -> wants README.md
```

Then project to unique local paths:

```text
doc_a -> README.md
doc_b -> README (conflict doc_b).md
```

`filesByPath` and `pathClaims` can be derived indexes, but they should not be CRDT truth.

## B5. Atomic location

Represent location as one assignable value:

```ts
entry.set("loc", { parentId, name, normName })
```

Do not represent a move as independent keys:

```ts
entry.set("parentId", nextParent)
entry.set("name", nextName)
```

Concurrent independent updates to parent/name can create mixed semantic states. `loc` should be replaced atomically. Until `internal/ycrdt` has ergonomic nested support, encode `loc` as canonical JSON.

---

# Part C: Deterministic materialized paths

Root desired paths are not required to be unique. Projection derives unique local filesystem paths.

Example input:

```text
doc_a desired path = README.md
doc_b desired path = README.md
doc_c desired path = docs/spec.md
```

Output:

```text
doc_a -> README.md
doc_b -> README (conflict doc_b).md
doc_c -> docs/spec.md
```

Algorithm:

```go
func ResolveMaterializedPaths(manifest RootManifest) Projection {
    live := manifest.LiveEntries()
    reachable := ResolveReachableEntries(live)

    groups := map[SiblingKey][]Entry{}
    for _, entry := range reachable {
        key := SiblingKey{ParentID: entry.Loc.ParentID, NormName: entry.Loc.NormName}
        groups[key] = append(groups[key], entry)
    }

    for _, group := range groups {
        sort.Slice(group, func(i, j int) bool {
            return group[i].ID < group[j].ID
        })
    }

    result := Projection{EntryPath: map[string]string{}, Taken: map[string]string{}}
    ordered := TopologicalEntryOrder(reachable)

    for _, entry := range ordered {
        group := groups[SiblingKey{entry.Loc.ParentID, entry.Loc.NormName}]
        index := IndexInGroup(group, entry.ID)

        desired := manifest.DesiredPath(entry.ID)
        materialized := desired
        if index > 0 {
            materialized = ConflictPath(desired, entry.ID)
        }

        materialized = FirstFreePath(materialized, entry.ID, result.Taken)
        result.EntryPath[entry.ID] = materialized
        result.Taken[materialized] = entry.ID
    }

    return result
}
```

Rules:

```text
- Conflict winner is arbitrary but stable: lexical entryID order.
- Conflict suffix is projection-only.
- Do not write conflict suffix back into root.
- Do not use arrival order.
- Do not use wall-clock time.
- Do not use local filesystem event order.
```

Conflict path:

```go
func ConflictPath(path, entryID string) string {
    dir, base := SplitDirBase(path)
    stem, ext := SplitStemExt(base)
    return JoinPath(dir, stem+" (conflict "+ShortID(entryID)+")"+ext)
}
```

## C1. Orphans and cycles

Concurrent operations can produce odd but survivable states:

```text
parent directory deleted while child moved into it
directory cycle from concurrent moves
child references missing parent
```

Projection policy:

```text
1. Live entries with a live acyclic parent chain are reachable.
2. Entries whose parent is missing, tombstoned, or cyclic are orphaned.
3. Orphaned entries materialize under a deterministic recovered namespace.
```

Example:

```text
Recovered/orphans/<entryID>/<original-name>
```

This path is projection-only. It does not mutate root. The user can later rename/move the file normally.

---

# Part D: Backend design

## D1. Backend responsibility

The backend owns:

```text
- auth and workspace scoping
- generic CRDT stream persistence
- stream sync transport
- update deduplication
- checkpointing
- root stream validation
- derived workspace metadata
- compatibility shims during migration
```

The backend does not own:

```text
- path uniqueness
- filesystem conflict repair
- local materialized paths
- plaintext document authority
- daemon projection policy
```

## D2. Postgres schema

### Workspaces

```sql
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS root_stream_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_root_stream_id
  ON workspaces(root_stream_id)
  WHERE root_stream_id IS NOT NULL;
```

### Stream heads

```sql
CREATE TABLE IF NOT EXISTS crdt_stream_heads (
  workspace_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'unknown',

  state_vector BYTEA NOT NULL DEFAULT '',
  update_id BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL,

  PRIMARY KEY (workspace_id, stream_id)
);
```

### Stream updates

```sql
CREATE TABLE IF NOT EXISTS crdt_stream_updates (
  id BIGSERIAL PRIMARY KEY,

  workspace_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,

  update BYTEA NOT NULL,
  update_sha256 TEXT NOT NULL,

  actor_id TEXT NOT NULL,
  actor_type TEXT NOT NULL,

  created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_crdt_stream_updates_stream_id
  ON crdt_stream_updates(workspace_id, stream_id, id ASC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_crdt_stream_updates_dedupe
  ON crdt_stream_updates(workspace_id, stream_id, update_sha256);
```

### Stream checkpoints

```sql
CREATE TABLE IF NOT EXISTS crdt_stream_checkpoints (
  id BIGSERIAL PRIMARY KEY,

  workspace_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,

  update_id BIGINT NOT NULL,
  crdt_state BYTEA NOT NULL,
  state_vector BYTEA NOT NULL,

  created_at TIMESTAMPTZ NOT NULL,

  UNIQUE(workspace_id, stream_id, update_id)
);

CREATE INDEX IF NOT EXISTS idx_crdt_stream_checkpoints_stream_update
  ON crdt_stream_checkpoints(workspace_id, stream_id, update_id DESC);
```

## D3. Generic stream API

### Bootstrap

```http
GET /api/workspaces/{workspaceID}/bootstrap
```

Response:

```json
{
  "workspaceId": "ws_...",
  "rootStreamId": "stream_..."
}
```

### Generic stream websocket

```http
GET /ws/workspaces/{workspaceID}/streams/{streamID}
```

Use existing y-protocol framing:

```go
MessageSync      = 0
MessageAwareness = 1

SyncStep1  = 0
SyncStep2  = 1
SyncUpdate = 2
```

### Generic stream HTTP update

```http
POST /api/workspaces/{workspaceID}/streams/{streamID}/updates
Content-Type: application/octet-stream
```

Response:

```json
{
  "accepted": true,
  "applied": true,
  "updateId": 123,
  "stateVector": "<base64>"
}
```

The daemon uses this for durable outbox retry.

## D4. Stream apply algorithm

```go
func ApplyStreamUpdate(workspaceID, streamID string, update []byte, meta OperationMeta) (ApplyResult, error) {
    hash := SHA256(update)
    tx := db.Begin()

    if StreamUpdateHashExists(tx, workspaceID, streamID, hash) {
        head := GetStreamHead(tx, workspaceID, streamID)
        tx.Commit()
        return ApplyResult{Accepted: true, Applied: false, UpdateID: head.UpdateID}, nil
    }

    doc := RestoreStreamDocForUpdate(tx, workspaceID, streamID)
    beforeSV := doc.StateVectorV1()

    if err := doc.ApplyUpdate(update); err != nil {
        tx.Rollback()
        return ApplyResult{}, err
    }

    if IsRootStream(tx, workspaceID, streamID) {
        manifest := ReadRootManifest(doc)
        if err := ValidateRootManifest(tx, workspaceID, manifest); err != nil {
            tx.Rollback()
            return ApplyResult{}, err
        }
    }

    afterSV := doc.StateVectorV1()
    if bytes.Equal(beforeSV, afterSV) {
        tx.Commit()
        return ApplyResult{Accepted: true, Applied: false, UpdateID: CurrentUpdateID(tx, workspaceID, streamID)}, nil
    }

    updateID := InsertStreamUpdate(tx, workspaceID, streamID, update, hash, meta)
    stateUpdate := doc.EncodeStateAsUpdate(nil)

    UpsertStreamHead(tx, workspaceID, streamID, afterSV, updateID)
    if ShouldCheckpoint(updateID) {
        InsertCheckpoint(tx, workspaceID, streamID, updateID, stateUpdate, afterSV)
    }

    tx.Commit()

    BroadcastStreamUpdate(workspaceID, streamID, update)
    PublishWorkspaceEvent(StreamUpdated)

    return ApplyResult{Accepted: true, Applied: true, UpdateID: updateID}, nil
}
```

## D5. Root stream validator

Validation after applying candidate update:

```text
Required:
  - entriesById exists and is a map.
  - root entry exists.
  - root entry is kind=dir.
  - root.loc is null.
  - every entry key matches entry.id.
  - kind is file or dir.
  - file entries have contentStreamId.
  - non-root entries have valid loc.
  - loc.name is non-empty and path-safe.
  - loc.normName == normalize(loc.name).
  - no path segment is . or ...
  - no synced path segment is .notty.

Immutable:
  - entry.kind cannot change.
  - file.contentStreamId cannot change.
  - tombstone cannot be removed once set.

Allowed:
  - duplicate sibling normName values.
  - orphaned entries.
  - tombstoned parents with live children.
  - entries whose materialized paths would conflict.
```

The validator checks schema and security. It does not enforce path uniqueness.

## D6. Derived workspace metadata

`GET /api/workspace` derives document metadata from root:

```go
func GetWorkspaceMetadata(workspaceID string) WorkspaceResponse {
    rootStreamID := GetRootStreamID(workspaceID)
    rootDoc := RestoreStreamDoc(workspaceID, rootStreamID)

    manifest := ReadRootManifest(rootDoc)
    projection := ResolveMaterializedPaths(manifest)

    docs := []DocumentMetadata{}
    for _, file := range manifest.LiveFiles() {
        head := GetStreamHead(workspaceID, file.ContentStreamID)
        docs = append(docs, DocumentMetadata{
            ID:              file.ID,
            ContentStreamID: file.ContentStreamID,
            Path:            projection.EntryPath[file.ID],
            DesiredPath:     manifest.DesiredPath(file.ID),
            Title:           file.Loc.Name,
            UpdateID:        head.UpdateID,
            StateVector:     Base64(head.StateVector),
        })
    }

    return WorkspaceResponse{Documents: docs, ...}
}
```

For compatibility, `path` is the unique materialized path. Add `desiredPath` for diagnostics and UI clarity.

## D7. Compatibility shims

Keep old endpoints temporarily, but implement them as stream mutations:

```text
POST /api/documents
  -> generate docID
  -> append root:create entry update
  -> append content:init update

PATCH /api/documents/{documentID}
  -> append root loc update

DELETE /api/documents/{documentID}
  -> append root tombstone update

GET /api/documents/by-path?path=...
  -> restore root
  -> resolve materialized paths
  -> return matching entry
```

---

# Part E: Daemon local SQLite state

Each workspace replica owns:

```text
<workspace>/.notty/state.sqlite
<workspace>/.notty/fslock.sqlite
<workspace>/.notty/recovered/...
```

`state.sqlite` is durable sync/projector state. `fslock.sqlite` is an operational filesystem mutex. Actual workspace documents remain normal files.

## E1. SQLite pragmas

For `state.sqlite`:

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = FULL;
```

For `fslock.sqlite`:

```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = NORMAL;
```

## E2. `state.sqlite` schema

### Streams

```sql
CREATE TABLE streams (
  stream_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL, -- root, content, unknown

  latest_state_id INTEGER,
  projected_state_id INTEGER,

  latest_update_id INTEGER NOT NULL DEFAULT 0,
  latest_state_vector BLOB,

  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

Meaning:

```text
latest_state_id:
  newest merged CRDT state after local + remote updates.

projected_state_id:
  state that has been successfully projected to local filesystem.
```

The distinction prevents unprojected remote changes from being misread as local edits after a crash.

### Stream state generations

```sql
CREATE TABLE stream_states (
  id INTEGER PRIMARY KEY AUTOINCREMENT,

  stream_id TEXT NOT NULL,
  state_update BLOB NOT NULL,
  state_vector BLOB NOT NULL,

  materialized_text_sha256 TEXT,

  created_at TEXT NOT NULL,

  FOREIGN KEY(stream_id) REFERENCES streams(stream_id)
);

CREATE INDEX idx_stream_states_stream_id
  ON stream_states(stream_id, id);
```

### Inbox

```sql
CREATE TABLE stream_inbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,

  stream_id TEXT NOT NULL,
  update_sha256 TEXT NOT NULL,
  update_bytes BLOB NOT NULL,

  remote_update_id INTEGER,
  received_at TEXT NOT NULL,
  applied_at TEXT,

  UNIQUE(stream_id, update_sha256)
);

CREATE INDEX idx_stream_inbox_unapplied
  ON stream_inbox(stream_id, applied_at, id);
```

### Outbox

```sql
CREATE TABLE stream_outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,

  stream_id TEXT NOT NULL,
  mutation_key TEXT NOT NULL,
  update_sha256 TEXT NOT NULL,
  update_bytes BLOB NOT NULL,

  actor_id TEXT NOT NULL,
  actor_type TEXT NOT NULL,
  reason TEXT NOT NULL,

  -- Optional hint used when this row creates or wakes another stream.
  -- Examples: root, content, unknown.
  kind_hint TEXT,

  -- Cross-stream dependency. May point to a row with a different stream_id.
  depends_on_id INTEGER,

  -- Set once this update has been folded into the local state for stream_id.
  -- This is separate from acked_at because local application and remote ack are
  -- different recovery milestones.
  local_applied_at TEXT,

  created_at TEXT NOT NULL,
  sent_at TEXT,
  acked_at TEXT,
  ack_update_id INTEGER,

  UNIQUE(stream_id, mutation_key),

  FOREIGN KEY(depends_on_id) REFERENCES stream_outbox(id)
);

CREATE INDEX idx_stream_outbox_pending
  ON stream_outbox(acked_at, depends_on_id, id);

CREATE INDEX idx_stream_outbox_local_ready
  ON stream_outbox(stream_id, local_applied_at, depends_on_id, id);
```

For local create:

```text
root:create:doc_abc     -> no dependency
content:init:doc_abc    -> depends_on_id = root create row
```

`local_applied_at` is independent from `acked_at`. A row can be folded into
local CRDT state before it is sent or acked. The sender still requires
`local_applied_at IS NOT NULL` and an acked dependency before network send.

### Manifest projection

```sql
CREATE TABLE manifest_projection (
  entry_id TEXT PRIMARY KEY,

  kind TEXT NOT NULL,
  content_stream_id TEXT,

  desired_path TEXT NOT NULL,
  materialized_path TEXT NOT NULL,

  -- Strong local identity when reliable.
  -- Unix-like: dev+ino. Windows: volume serial + file index.
  file_key TEXT,

  -- Cached stat metadata. Present from schema day one; nullable until populated.
  size_bytes INTEGER,
  mode INTEGER,
  mtime_ns INTEGER,
  ctime_ns INTEGER,
  stat_valid INTEGER NOT NULL DEFAULT 0,

  -- Hash of last known clean projected bytes, when known.
  last_clean_hash TEXT,

  root_projected_state_id INTEGER NOT NULL,

  tombstoned INTEGER NOT NULL DEFAULT 0,
  pending_create INTEGER NOT NULL DEFAULT 0,

  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_manifest_projection_materialized_path
  ON manifest_projection(materialized_path);

CREATE INDEX idx_manifest_projection_file_key
  ON manifest_projection(file_key);
```

Do **not** add a composite stat index over `(materialized_path, file_key, size_bytes, mode, mtime_ns, ctime_ns)`. Lookups are by `materialized_path` or `file_key`; stat fields are compared after the row is loaded.

### Content projection

```sql
CREATE TABLE content_projection (
  stream_id TEXT PRIMARY KEY,

  entry_id TEXT NOT NULL,
  materialized_path TEXT NOT NULL,

  projected_state_id INTEGER,
  projected_hash TEXT,

  -- Stat tuple observed when projected_hash/projected_state_id was last written.
  -- FileKey is part of identity. If FileKey is unreliable or changed, do not
  -- use stat equality to skip reading bytes.
  file_key TEXT,
  size_bytes INTEGER,
  mode INTEGER,
  mtime_ns INTEGER,
  ctime_ns INTEGER,
  stat_valid INTEGER NOT NULL DEFAULT 0,

  dirty INTEGER NOT NULL DEFAULT 0,

  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_content_projection_materialized_path
  ON content_projection(materialized_path);
```

`content_projection.materialized_path` is unique. Two content streams pointing at the same local materialized path is a projection inconsistency and should fail loudly rather than silently corrupting local ownership.

Do **not** add a composite stat index here. `content_projection` is normally loaded by `stream_id`, and occasional path lookups use the unique `materialized_path` index. Stat fields are compared in memory after loading the row.

### Scan hints

```sql
CREATE TABLE scan_hints (
  id INTEGER PRIMARY KEY AUTOINCREMENT,

  kind TEXT NOT NULL, -- path, dir, full
  path TEXT,
  reason TEXT NOT NULL,

  created_at TEXT NOT NULL
);

CREATE INDEX idx_scan_hints_kind_path
  ON scan_hints(kind, path);
```

Insertion cap:

```go
const MaxPendingScanHints = 10_000
const ScanHintDrainLimit = 1_000

func InsertScanHint(tx Tx, kind ScanHintKind, path, reason string) error {
    path = NormalizeRel(path)
    if IsIgnoredPath(path) {
        return nil
    }

    if tx.QueryInt(`SELECT COUNT(*) FROM scan_hints`) >= MaxPendingScanHints {
        tx.Exec(`DELETE FROM scan_hints`)
        tx.Exec(`INSERT INTO scan_hints(kind, path, reason, created_at) VALUES ('full', '', 'hint-overflow', ?)`, now())
        return nil
    }

    tx.Exec(`INSERT INTO scan_hints(kind, path, reason, created_at) VALUES (?, ?, ?, ?)`, kind, path, reason, now())
    return nil
}
```

Hints are performance hints only. If the hint table overflows, replacing everything with one full hint does not lose correctness.

### Directory scan cache

```sql
CREATE TABLE directory_scan_cache (
  path TEXT PRIMARY KEY,

  mtime_ns INTEGER NOT NULL,
  ctime_ns INTEGER,
  entry_count INTEGER NOT NULL DEFAULT 0,

  children_json TEXT NOT NULL,

  updated_at TEXT NOT NULL
);
```

Caps:

```go
const MaxCachedDirChildren = 1_000
const MaxCachedDirJSONBytes = 64 * 1024
```

If a directory has more than `MaxCachedDirChildren` children, or encoded `children_json` would exceed `MaxCachedDirJSONBytes`, do not cache it. Re-reading that directory is cheaper and safer than maintaining a large JSON cache row.

### Scan state

```sql
CREATE TABLE scan_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),

  cursor_path TEXT,
  incomplete INTEGER NOT NULL DEFAULT 0,
  last_full_scan_at TEXT,

  directory_mtime_reliable INTEGER NOT NULL DEFAULT 0,
  file_key_reliable INTEGER NOT NULL DEFAULT 0,
  ctime_reliable INTEGER NOT NULL DEFAULT 0,

  capabilities_initialized INTEGER NOT NULL DEFAULT 0,
  last_capability_probe_at TEXT,

  updated_at TEXT NOT NULL
);
```

On first daemon boot for a workspace replica, initialize the singleton row before starting watchers or reconciliation:

```sql
INSERT INTO scan_state (
  id, cursor_path, incomplete, last_full_scan_at,
  directory_mtime_reliable, file_key_reliable, ctime_reliable,
  capabilities_initialized, last_capability_probe_at, updated_at
) VALUES (
  1, '', 0, NULL,
  0, 0, 0,
  0, NULL, datetime('now')
) ON CONFLICT(id) DO NOTHING;
```

Then run the FileKey, directory-mtime, and ctime probes exactly once during startup if `capabilities_initialized = 0`, persist the results, set `capabilities_initialized = 1`, and insert a `full` scan hint. If the user moves the workspace to a different filesystem, the daemon may expose a manual `--reprobe-filesystem-capabilities` maintenance command that clears `capabilities_initialized` and reruns the probes.


### Pending content creates

Local create is two-phase, so keep a durable pending table:

```sql
CREATE TABLE pending_content_creates (
  entry_id TEXT PRIMARY KEY,
  content_stream_id TEXT NOT NULL,
  materialized_path TEXT NOT NULL,

  observed_file_key TEXT,
  observed_size_bytes INTEGER,
  observed_mode INTEGER,
  observed_mtime_ns INTEGER,
  observed_ctime_ns INTEGER,
  observed_stat_valid INTEGER NOT NULL DEFAULT 0,

  root_mutation_key TEXT NOT NULL,

  -- needs_bytes: root entry was discovered, but bytes have not been read yet.
  -- reading: row is claimed by the bounded byte-read finalizer; reset to needs_bytes on startup.
  -- outbox_created: content:init outbox row exists.
  -- completed: content:init was acked and local content_projection is initialized.
  -- cancelled: discovery became invalid before content:init was created.
  status TEXT NOT NULL CHECK(status IN ('needs_bytes', 'reading', 'outbox_created', 'completed', 'cancelled')),

  content_outbox_id INTEGER,

  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,

  FOREIGN KEY(content_outbox_id) REFERENCES stream_outbox(id),

  CHECK (
    observed_stat_valid = 0 OR (
      observed_file_key IS NOT NULL AND
      observed_size_bytes IS NOT NULL AND
      observed_mode IS NOT NULL AND
      observed_mtime_ns IS NOT NULL AND
      observed_ctime_ns IS NOT NULL
    )
  )
);

CREATE INDEX idx_pending_content_creates_status
  ON pending_content_creates(status, created_at);
```

`observed_stat_valid = 1` means the discovery stat contains a reliable FileKey and all tuple fields needed to verify that the later byte read is still reading the same candidate file. If `file_key_reliable = 0`, leave `observed_stat_valid = 0`; the byte-read phase uses before/after stat stability for the current path, but the content known-clean fast path and FileKey move detection remain disabled.

Lifecycle:

```text
needs_bytes
  -> reading         when bounded finalizer claims the row
  -> cancelled       if observed path no longer matches or the file vanished

reading
  -> outbox_created  after bounded finalizer reads stable bytes and creates content:init outbox
  -> needs_bytes     on daemon startup or finalizer retryable failure
  -> cancelled       if observed path no longer matches or the file vanished

outbox_created
  -> completed       after content_outbox_id is acked and content_projection.projected_state_id is set

completed/cancelled
  -> deleted by reaper after PendingCreateRetention, default 24h
```


On daemon startup, reset stale `reading` rows because no byte-read worker survives process death:

```sql
UPDATE pending_content_creates
   SET status = 'needs_bytes', updated_at = datetime('now')
 WHERE status = 'reading';
```

The row is not deleted merely because the outbox exists. It completes only after the content init is acked and the local content projection has a projected state. This prevents crash recovery from losing track of a file whose metadata was created but whose content stream has not yet been locally projected.

### Filesystem jobs

```sql
CREATE TABLE fs_jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,

  job_key TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,

  entry_id TEXT,
  stream_id TEXT,

  source_path TEXT,
  target_path TEXT,

  expected_hash TEXT,
  target_hash TEXT,
  target_state_id INTEGER,

  status TEXT NOT NULL, -- pending, running, done, failed
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,

  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX idx_fs_jobs_pending
  ON fs_jobs(status, id);
```

## E3. `fslock.sqlite` schema

Use a single SQLite write transaction as the filesystem operation mutex:

```sql
CREATE TABLE IF NOT EXISTS lock_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  holder TEXT,
  operation TEXT,
  path_a TEXT,
  path_b TEXT,
  acquired_at TEXT
);
```

Acquire and release:

```go
func (l *FSLockDB) WithFilesystemLock(ctx context.Context, operation, pathA, pathB string, fn func() error) error {
    conn := l.Conn(ctx)
    defer conn.Close()

    if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
        return err
    }

    committed := false
    defer func() {
        if !committed {
            _, _ = conn.ExecContext(context.Background(), "ROLLBACK")
        }
    }()

    _, err := conn.ExecContext(ctx, `
        INSERT INTO lock_meta (id, holder, operation, path_a, path_b, acquired_at)
        VALUES (1, ?, ?, ?, ?, datetime('now'))
        ON CONFLICT(id) DO UPDATE SET
          holder = excluded.holder,
          operation = excluded.operation,
          path_a = excluded.path_a,
          path_b = excluded.path_b,
          acquired_at = excluded.acquired_at
    `, HolderID(), operation, pathA, pathB)
    if err != nil {
        return err
    }

    if err := fn(); err != nil {
        return err
    }

    _, _ = conn.ExecContext(ctx, `DELETE FROM lock_meta WHERE id = 1`)

    if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
        return err
    }

    committed = true
    return nil
}
```

Use a separate DB from `state.sqlite` so filesystem I/O does not hold the durable sync-state writer lock.

---


# Part F: WorkspaceSyncLoop

## F1. Event sources

Each local workspace replica has one `WorkspaceSyncLoop`.

Events:

```text
- root/content websocket received update -> insert stream_inbox row, queue streamID
- filesystem watcher event -> insert scan hint, queue root/content stream
- periodic full-scan ticker -> insert full scan hint, queue root stream
- outbox sender ack -> queue streamID
- fs job failure/retry -> queue root/content stream
```

Filesystem events are wakeups only. Projectors use snapshots.

Periodic scan defaults:

```go
const PeriodicFullScanInterval = 5 * time.Minute
const StartupFullScanRequired = true
```

At startup, always insert one `full` scan hint after scan capability initialization. During normal operation, the periodic ticker checks `scan_state.last_full_scan_at`; if it is null or older than `PeriodicFullScanInterval`, insert one `full` scan hint and queue the root stream. When a full stat scan completes without `scan.Incomplete`, update `last_full_scan_at = now`.

Watcher overflow, watcher restart, or scan-cache corruption must also insert a `full` hint regardless of `last_full_scan_at`.

## F2. ReconcileOne

`ReconcileOne` must apply ready local outbox rows for the current stream before incoming updates. This makes cross-stream local creates work: the root reconcile creates a `content:init` outbox row for the content stream, and the later content-stream reconcile locally applies that row only after the root-create dependency is acked.

```go
const MaxPendingContentCreatesPerCycle = 32
const MaxPendingContentCreateBytesPerCycle = 64 << 20 // 64 MiB

type PendingCreateLimits struct {
    MaxRows  int
    MaxBytes int64
}

func (l *WorkspaceSyncLoop) ReconcileOne(ctx context.Context, streamID string) error {
    projector := l.projectors.ProjectorFor(streamID)

    tx := l.state.BeginImmediate(ctx)
    stream := tx.GetOrCreateStream(streamID)
    latestDoc := tx.LoadDoc(stream.LatestStateID)

    // 1. Capture fresh local intent for this stream.
    mutations, err := projector.CaptureLocal(ctx, tx.Env(), latestDoc)
    if err != nil {
        tx.Rollback()
        return err
    }

    for _, mutation := range mutations {
        tx.EnsureStream(mutation.StreamID, mutation.KindHint)
        outboxID := tx.UpsertOutbox(mutation)

        if mutation.StreamID == streamID && tx.OutboxDependencyAcked(outboxID) {
            latestDoc.ApplyUpdate(mutation.Update)
            tx.MarkOutboxLocallyApplied(outboxID, now())
        } else {
            tx.QueueStream(mutation.StreamID)
        }
    }

    // 2. Apply previously-created local outbox rows for this stream whose
    // dependencies are now acked and which have not yet been folded into local
    // stream state. This is the normative cross-stream path.
    readyLocal := tx.ReadyLocalOutbox(streamID)
    for _, row := range readyLocal {
        latestDoc.ApplyUpdate(row.UpdateBytes)
        tx.MarkOutboxLocallyApplied(row.ID, now())
    }

    // 3. Apply remote inbox after local intent.
    inbox := tx.UnappliedInbox(streamID)
    for _, row := range inbox {
        latestDoc.ApplyUpdate(row.UpdateBytes)
        tx.MarkInboxApplied(row.ID)
    }

    newState := latestDoc.EncodeStateAsUpdate(nil)
    newSV := latestDoc.StateVectorV1()
    newStateID := tx.InsertStreamState(streamID, newState, newSV)
    tx.UpdateLatestState(streamID, newStateID, newSV)

    if err := projector.PlanApplyMerged(ctx, tx.Env(), latestDoc, newStateID); err != nil {
        tx.Rollback()
        return err
    }

    // If this content stream just folded a pending content:init row, connect the
    // already-existing local file to the new state without scheduling a rewrite.
    tx.FinalizeCompletedPendingContentCreatesForStream(streamID, newStateID)

    tx.Commit()

    if streamID == l.RootStreamID {
        more, err := l.ProcessPendingContentCreates(ctx, PendingCreateLimits{
            MaxRows:  MaxPendingContentCreatesPerCycle,
            MaxBytes: MaxPendingContentCreateBytesPerCycle,
        })
        if err != nil {
            l.Queue(streamID)
            return err
        }
        if more {
            l.Queue(streamID)
        }
    }

    if err := l.RunPendingFSJobs(ctx); err != nil {
        l.Queue(streamID)
        return err
    }

    if l.HasPendingOutbox(streamID) {
        l.sendQueue.Mark(streamID)
    }

    return nil
}
```

Order is essential:

```text
1. capture local outgoing changes
2. append/upsert local updates to outbox
3. apply ready local outbox rows for this stream
4. apply incoming updates
5. persist merged state
6. plan projection
7. finalize pending create projection state if applicable
8. commit state.sqlite
9. process a bounded batch of pending content creates, if root
10. run filesystem jobs outside state transaction
```

Bulk import behavior is intentionally conservative in v1. If a user runs `cp -r template/ workspace/` and introduces 1,000 new files, the daemon processes at most `MaxPendingContentCreatesPerCycle` rows and at most `MaxPendingContentCreateBytesPerCycle` bytes per root reconciliation cycle, then requeues the root stream. This avoids a single root cycle monopolizing the daemon. Bulk imports are not optimized beyond bounded progress and crash-safe retry in v1.

## F2a. Cross-stream outbox contract

There is one `stream_outbox` table for all streams. `InsertOutbox`, `InsertCrossStreamOutbox`, `InsertMutationOutbox`, and `UpsertOutbox` all mean the same durable operation: insert or reuse a row keyed by `(stream_id, mutation_key)`.

A mutation is cross-stream when it is produced while reconciling one stream but targets another stream. The common case is local create:

```text
root reconcile produces:
  root:create:doc_abc       stream_id = root_stream
  content:init:doc_abc      stream_id = doc_abc, depends_on_id = root:create row
```

Rules:

```text
- `depends_on_id` may point to an outbox row for a different stream.
- A row is sendable only after its dependency is acked.
- A row is locally applicable only after its dependency is acked.
- `local_applied_at` records that the row has been folded into the local target stream state.
- Target stream reconciliation applies ready, locally-unapplied outbox rows before inbox rows.
- Cross-stream insertion must queue the target stream so the row is eventually locally applied and projected.
```

`mutation.DependsOn` may be expressed as a mutation key during insertion. The transaction resolves it to `depends_on_id` by looking up the already-inserted outbox row with that mutation key. If the dependency is missing, insertion fails and the root stream is requeued rather than creating an unsendable orphan outbox row.

This keeps local state, remote send order, and crash recovery aligned. The content stream does not project bytes for a document whose root create was rejected or not accepted yet.

## F3. Sender

```go
func (s *StreamSender) SendPending(ctx context.Context) error {
    for {
        tx := s.state.BeginImmediate(ctx)
        row := tx.NextSendableOutboxRow()
        if row == nil {
            tx.Commit()
            return nil
        }
        tx.MarkOutboxSent(row.ID, time.Now())
        tx.Commit()

        ack, err := s.transport.PostStreamUpdate(ctx, row.StreamID, row.UpdateBytes, row.Actor)
        if err != nil {
            return err
        }

        tx = s.state.BeginImmediate(ctx)
        tx.MarkOutboxAcked(row.ID, ack.UpdateID, time.Now())
        tx.Commit()

        s.reconcileQueue.Mark(row.StreamID)
    }
}
```

`NextSendableOutboxRow` requires dependencies to be acked across the whole outbox table, not just within one stream:

```sql
acked_at IS NULL
AND local_applied_at IS NOT NULL
AND (
  depends_on_id IS NULL
  OR EXISTS (
    SELECT 1 FROM stream_outbox dep
    WHERE dep.id = stream_outbox.depends_on_id
      AND dep.acked_at IS NOT NULL
  )
)
```

This guarantees root create before content init and prevents a dependent content update from being sent before the local target stream has folded it into `state.sqlite`. When a content-init row is acked, the sender queues the content stream. The content-stream reconciliation folds the init update into local state, initializes `content_projection.projected_state_id`, clears `manifest_projection.pending_create`, and marks the pending row `completed`. The 24-hour reaper deletes completed/cancelled pending rows.

# Part G: WorkspaceFS and scan acceleration

## G1. WorkspaceFS API

```go
type WorkspaceFS struct {
    Root  string
    Locks *FSLockDB
}

func (fs *WorkspaceFS) Scan(ctx context.Context, opts ScanOptions) (WorkspaceScan, error)
func (fs *WorkspaceFS) Stat(ctx context.Context, path string) (FileStat, error)

// Opens, reads, and revalidates a file without holding fslock.sqlite during
// the potentially long byte read. See G10.
func (fs *WorkspaceFS) ReadBytesStable(ctx context.Context, path string, opts StableReadOptions) (ReadBytesResult, bool, error)

func (fs *WorkspaceFS) WriteIfUnchanged(ctx context.Context, path string, expected Hash, content []byte) error
func (fs *WorkspaceFS) DeleteIfUnchanged(ctx context.Context, path string, expected Hash) error
func (fs *WorkspaceFS) MoveIfNoTarget(ctx context.Context, from, to string) error
func (fs *WorkspaceFS) Archive(ctx context.Context, path, reason string) (string, error)
func (fs *WorkspaceFS) EnsureParent(path string) error
```

Every mutating filesystem operation goes through `fslock.sqlite`. Stable reads use `fslock.sqlite` only for short open/stat and final re-stat phases; they do not hold the filesystem writer lock while reading large files.

## G2. FileStat and FileSnapshot

```go
type FileKind string

const (
    FileKindFile        FileKind = "file"
    FileKindDir         FileKind = "dir"
    FileKindSymlink     FileKind = "symlink"
    FileKindUnsupported FileKind = "unsupported"
)

type FileStat struct {
    Path      string
    Kind      FileKind
    Exists    bool

    // Strong local file identity when reliable.
    // Unix-like: dev+ino. Windows: volume serial + file index.
    FileKey   string

    SizeBytes int64
    Mode      uint32
    MTimeNS   int64
    CTimeNS   int64

    StatValid bool
}

type FileSnapshot struct {
    Stat FileStat

    // Optional. Empty unless explicitly requested by bounded hash matching.
    Hash string
}
```

## G3. SameStatTuple must include FileKey

The known-clean content fast path is race-sensitive. Atomic replacement can preserve size and timestamps while changing the underlying file identity. Therefore, `FileKey` is part of stat identity.

```go
type ScanCapabilities struct {
    DirectoryMTimeReliable bool
    FileKeyReliable        bool
    CTimeReliable          bool
}

func SameStatTuple(cached FileStat, current FileStat, caps ScanCapabilities) bool {
    if !cached.StatValid || !current.StatValid {
        return false
    }

    if cached.Kind != current.Kind || cached.SizeBytes != current.SizeBytes || cached.Mode != current.Mode || cached.MTimeNS != current.MTimeNS {
        return false
    }

    if caps.CTimeReliable && cached.CTimeNS != current.CTimeNS {
        return false
    }

    if !caps.FileKeyReliable {
        return false
    }
    if cached.FileKey == "" || current.FileKey == "" || cached.FileKey != current.FileKey {
        return false
    }

    return true
}
```

If `file_key_reliable = 0`, `SameStatTuple` returns false for content fast paths. The content projector may still use stats for scheduling, but it must read bytes before returning “no mutation.”

## G4. FileKey reliability probe

Do not assume FileKey is reliable. Test it at startup under `.notty/tmp/filekey-test`.

```go
func (fs *WorkspaceFS) TestFileKeyReliability(ctx context.Context) bool {
    dir := fs.Abs(".notty/tmp/filekey-test")
    _ = os.RemoveAll(dir)
    if err := os.MkdirAll(dir, 0o700); err != nil { return false }
    defer os.RemoveAll(dir)

    a := filepath.Join(dir, "a")
    b := filepath.Join(dir, "b")
    a2 := filepath.Join(dir, "a-renamed")

    if err := os.WriteFile(a, []byte("a"), 0o600); err != nil { return false }
    if err := os.WriteFile(b, []byte("b"), 0o600); err != nil { return false }

    ka1, oka1 := fs.FileKeyAbs(a)
    kb1, okb1 := fs.FileKeyAbs(b)
    if !oka1 || !okb1 || ka1 == "" || kb1 == "" || ka1 == kb1 { return false }

    time.Sleep(10 * time.Millisecond)
    ka2, oka2 := fs.FileKeyAbs(a)
    kb2, okb2 := fs.FileKeyAbs(b)
    if !oka2 || !okb2 || ka1 != ka2 || kb1 != kb2 { return false }

    if err := os.Rename(a, a2); err != nil { return false }
    ka3, oka3 := fs.FileKeyAbs(a2)
    if !oka3 || ka3 != ka1 { return false }

    c := filepath.Join(dir, "c")
    if err := os.WriteFile(c, []byte("a"), 0o600); err != nil { return false }
    kc, okc := fs.FileKeyAbs(c)
    if !okc || kc == "" || kc == ka1 || kc == kb1 { return false }

    return true
}
```

If this fails, set `scan_state.file_key_reliable = 0`. On unreliable filesystems, move detection skips FileKey and content stat short-circuiting is disabled.

## G5. Directory mtime reliability probe

```go
func (fs *WorkspaceFS) TestDirectoryMTimeReliability(ctx context.Context) bool {
    dir := fs.Abs(".notty/tmp/mtime-test")
    _ = os.RemoveAll(dir)
    if err := os.MkdirAll(dir, 0o700); err != nil { return false }
    defer os.RemoveAll(dir)

    before := StatDirMTimeNS(dir)
    WriteTempFileInside(dir)
    afterCreate := StatDirMTimeNS(dir)
    RemoveTempFileInside(dir)
    afterRemove := StatDirMTimeNS(dir)

    return afterCreate != before && afterRemove != afterCreate
}
```

If this fails, set `directory_mtime_reliable = 0` and never use `directory_scan_cache`.

## G6. ScanOptions and fallback behavior

```go
type ScanHintKind string

const (
    ScanHintPath ScanHintKind = "path"
    ScanHintDir  ScanHintKind = "dir"
    ScanHintFull ScanHintKind = "full"
)

type ScanHint struct {
    Kind   ScanHintKind
    Path   string
    Reason string
}

type ScanBudget struct {
    MaxPaths     int
    MaxDirs      int
    MaxDuration  time.Duration
    MaxHashBytes int64
}

type ScanOptions struct {
    Hints        []ScanHint
    StatOnly     bool
    UseDirCache  bool
    Capabilities ScanCapabilities
    Budget       ScanBudget
    CursorPath   string
}

type WorkspaceScan struct {
    Files      map[string]FileSnapshot
    Dirs       map[string]FileStat
    Missing    map[string]struct{}
    Incomplete bool
    CursorPath string
}
```

Fallback behavior:

```text
If directory_mtime_reliable = 0:
  Ignore directory_scan_cache. Dir hints do fresh readdir/stat of that dir.
  Full scans do budgeted readdir/stat traversal.

If file_key_reliable = 0:
  FileStat.FileKey is empty or diagnostic only. Move detection skips FileKey.
  Content known-clean stat short-circuit is disabled.

If both are unreliable:
  WorkspaceFS.Scan still returns stat-only snapshots. Full/root recovery scans do
  budgeted readdir-everything traversal, skipping .notty. Hot-path hinted scans
  stat hinted paths and fresh readdir/stat hinted dirs. If the budget is exhausted,
  save cursor_path, set scan_state.incomplete, and requeue root.
```

## G7. WorkspaceFS.Scan

```go
func (fs *WorkspaceFS) Scan(ctx context.Context, opts ScanOptions) (WorkspaceScan, error) {
    if !opts.Capabilities.DirectoryMTimeReliable {
        opts.UseDirCache = false
    }

    if HasFullHint(opts.Hints) || opts.CursorPath != "" || len(opts.Hints) == 0 {
        return fs.budgetedFullStatScan(ctx, opts)
    }

    scan := WorkspaceScan{Files: map[string]FileSnapshot{}, Dirs: map[string]FileStat{}, Missing: map[string]struct{}{}}

    for _, hint := range CoalesceHints(opts.Hints) {
        switch hint.Kind {
        case ScanHintPath:
            snap, err := fs.StatPath(ctx, hint.Path, opts.Capabilities)
            if err != nil { return scan, err }
            if snap.Stat.Exists {
                if snap.Stat.Kind == FileKindDir { scan.Dirs[hint.Path] = snap.Stat } else { scan.Files[hint.Path] = snap }
            } else {
                scan.Missing[hint.Path] = struct{}{}
            }

        case ScanHintDir:
            partial, err := fs.scanDirectoryWithCache(ctx, hint.Path, opts)
            if err != nil { return scan, err }
            scan.Merge(partial)
        }

        if opts.Budget.Exceeded() {
            scan.Incomplete = true
            scan.CursorPath = hint.Path
            return scan, nil
        }
    }

    return scan, nil
}
```

## G8. Directory cache

```go
const MaxCachedDirChildren = 1_000
const MaxCachedDirJSONBytes = 64 * 1024

func (fs *WorkspaceFS) scanDirectoryWithCache(ctx context.Context, dir string, opts ScanOptions) (WorkspaceScan, error) {
    current := fs.StatDir(dir)
    cached := fs.LoadDirectoryCache(dir)

    if opts.UseDirCache && cached.ValidFor(current) {
        return cached.AsWorkspaceScan(), nil
    }

    children := fs.ReadDir(dir)
    scan := WorkspaceScan{Files: map[string]FileSnapshot{}, Dirs: map[string]FileStat{}}

    for _, child := range children {
        rel := JoinRel(dir, child.Name())
        if IsIgnoredPath(rel) { continue }

        snap := fs.StatPath(ctx, rel, opts.Capabilities)
        if snap.Stat.Kind == FileKindDir { scan.Dirs[rel] = snap.Stat } else { scan.Files[rel] = snap }
    }

    if len(children) <= MaxCachedDirChildren {
        encoded := EncodeChildrenJSON(children)
        if len(encoded) <= MaxCachedDirJSONBytes { fs.StoreDirectoryCache(dir, current, children) } else { fs.DeleteDirectoryCache(dir) }
    } else {
        fs.DeleteDirectoryCache(dir)
    }

    return scan, nil
}
```

## G9. Matching and move detection

Matching order:

```text
1. Exact materialized path match.
2. Same FileKey match, only if file_key_reliable = 1.
3. Bounded clean-hash match.
4. Otherwise unmatched.
```

```go
type MatchResult struct {
    ByEntryID      map[string]MatchedPath
    UnmatchedFiles map[string]FileSnapshot
    MissingEntries []ProjectedEntry
}

type MatchedPath struct {
    EntryID string
    Path    string
    Reason  string // exact-path, file-key, clean-hash
    Stat    FileStat
}
```

FileKey move detection:

```go
func MatchByFileKey(scanState ScanState, missing []ProjectedEntry, scan WorkspaceScan, matches MatchResult) {
    if !scanState.FileKeyReliable { return }

    fileKeyIndex := map[string]string{}
    for path, snap := range scan.Files {
        if snap.Stat.FileKey != "" { fileKeyIndex[snap.Stat.FileKey] = path }
    }

    for _, entry := range missing {
        if entry.FileKey == "" { continue }
        path := fileKeyIndex[entry.FileKey]
        if path == "" || AlreadyMatched(path, matches) { continue }
        matches.ByEntryID[entry.EntryID] = MatchedPath{EntryID: entry.EntryID, Path: path, Reason: "file-key", Stat: scan.Files[path].Stat}
    }
}
```

Bounded hash fallback:

```go
type MoveDetectionLimits struct {
    MaxHashCandidates int
    MaxHashBytes      int64
}

func DetectHashMoves(missing []ProjectedEntry, unmatched map[string]FileSnapshot, limits MoveDetectionLimits) []MatchedPath {
    if len(unmatched) > limits.MaxHashCandidates { return nil }

    var bytesHashed int64
    sizeIndex := groupUnmatchedBySize(unmatched)
    results := []MatchedPath{}

    for _, entry := range missing {
        if entry.LastCleanHash == "" || entry.SizeBytes <= 0 { continue }
        candidates := sizeIndex[entry.SizeBytes]
        if len(candidates) != 1 { continue }

        candidate := candidates[0]
        if bytesHashed+candidate.Stat.SizeBytes > limits.MaxHashBytes { break }

        hash := HashFile(candidate.Path)
        bytesHashed += candidate.Stat.SizeBytes

        if hash == entry.LastCleanHash {
            results = append(results, MatchedPath{EntryID: entry.EntryID, Path: candidate.Path, Reason: "clean-hash", Stat: candidate.Stat})
        }
    }

    return onlyUniqueMatches(results)
}
```

If matching is not proven:

```text
missing tracked entry -> tombstone intent
unmatched file -> create intent
```

---

## G10. Stable byte reads without long filesystem locks

`ReadBytesStable` must not hold the `fslock.sqlite` writer transaction during `os.ReadFile` for large files. Holding the lock during a 100 MiB read can block unrelated Notty writes, moves, deletes, and archives for seconds.

Use an open/read/revalidate pattern:

```go
type StableReadOptions struct {
    ExpectedStat *FileStat
    Capabilities ScanCapabilities
    MaxBytes int64 // 0 means no per-file limit
}

type ReadBytesResult struct {
    Bytes []byte
    OpenStat FileStat
    FinalStat FileStat
}

func (fs *WorkspaceFS) ReadBytesStable(ctx context.Context, path string, opts StableReadOptions) (ReadBytesResult, bool, error) {
    var f *os.File
    var openStat FileStat

    // Short critical section: verify discovery stat and open the file handle.
    err := fs.Locks.WithFilesystemLock(ctx, "open-stable-read", path, "", func() error {
        current, err := fs.Stat(ctx, path)
        if err != nil || !current.Exists {
            return err
        }
        if opts.ExpectedStat != nil && !SameCreateDiscoveryStat(*opts.ExpectedStat, current, opts.Capabilities) {
            return nil
        }

        opened, err := os.Open(fs.Abs(path))
        if err != nil {
            return err
        }

        fdStat, err := fs.FStat(opened, path)
        if err != nil {
            _ = opened.Close()
            return err
        }
        if !SameOpenFileStat(current, fdStat, opts.Capabilities) {
            _ = opened.Close()
            return nil
        }

        f = opened
        openStat = fdStat
        return nil
    })
    if err != nil || f == nil {
        return ReadBytesResult{}, false, err
    }
    defer f.Close()

    if opts.MaxBytes > 0 {
        limited := io.LimitReader(f, opts.MaxBytes+1)
        data, err := io.ReadAll(limited)
        if err != nil {
            return ReadBytesResult{}, false, err
        }
        if int64(len(data)) > opts.MaxBytes {
            return ReadBytesResult{}, false, ErrFileTooLargeForSingleRead
        }
        return fs.finishStableRead(ctx, path, f, data, openStat, opts)
    }

    data, err := io.ReadAll(f)
    if err != nil {
        return ReadBytesResult{}, false, err
    }
    return fs.finishStableRead(ctx, path, f, data, openStat, opts)
}
```

Final validation reacquires the filesystem lock and verifies that the path still points to the same file identity observed at open time:

```go
func (fs *WorkspaceFS) finishStableRead(ctx context.Context, path string, f *os.File, data []byte, openStat FileStat, opts StableReadOptions) (ReadBytesResult, bool, error) {
    var finalStat FileStat
    ok := false

    err := fs.Locks.WithFilesystemLock(ctx, "finish-stable-read", path, "", func() error {
        current, err := fs.Stat(ctx, path)
        if err != nil || !current.Exists {
            return err
        }
        fdStat, err := fs.FStat(f, path)
        if err != nil {
            return err
        }
        if !SameOpenFileStat(openStat, fdStat, opts.Capabilities) {
            return nil
        }
        if !SameOpenFileStat(openStat, current, opts.Capabilities) {
            return nil
        }
        finalStat = current
        ok = true
        return nil
    })
    if err != nil || !ok {
        return ReadBytesResult{}, false, err
    }

    return ReadBytesResult{Bytes: data, OpenStat: openStat, FinalStat: finalStat}, true, nil
}
```

When `FileKeyReliable = true`, `SameOpenFileStat` must compare FileKey. When `FileKeyReliable = false`, the check falls back to size/mode/mtime/ctime and is explicitly weaker. That weaker mode is acceptable for creating content from the current file at a path, but it must not be used to preserve document identity across bulk renames.

# Part H: RootManifestProjector

## H1. Responsibility

`RootManifestProjector` owns namespace sync:

```text
- local create -> root entry + pending content init
- local rename/move -> root loc update
- local delete -> root tombstone
- root create -> track entry and queue content stream
- root rename/move -> move local path preserving bytes
- root tombstone -> safe delete, detach dirty bytes, or untrack
- duplicate desired path projection
- orphan/cycle projection
- durable entryID <-> materialized path mapping
```

It does not own content edits after initial local create. That is `ContentProjector`.

## H2. CaptureLocal

`CaptureLocal` is stat-first and byte-lazy. It must not call an unbounded no-args `p.FS.Scan()`, and it must not read all file bytes during namespace scan.

```go
func (p *RootManifestProjector) CaptureLocal(ctx context.Context, env ProjectorEnv, rootDoc *crdt.Doc) ([]StreamMutation, error) {
    manifest := ReadRootManifest(rootDoc)
    projection := ResolveMaterializedPaths(manifest)

    hints := env.DrainScanHints(ScanHintDrainLimit)
    scanState := env.LoadScanState()
    tracker := env.LoadManifestProjection()

    scan, err := p.FS.Scan(ctx, ScanOptions{
        Hints:    hints,
        StatOnly: true,
        Capabilities: ScanCapabilities{
            DirectoryMTimeReliable: scanState.DirectoryMTimeReliable,
            FileKeyReliable:        scanState.FileKeyReliable,
            CTimeReliable:          scanState.CTimeReliable,
        },
        UseDirCache: scanState.DirectoryMTimeReliable,
        CursorPath:  scanState.CursorPath,
        Budget:      DefaultRootScanBudget(),
    })
    if err != nil { return nil, err }

    matches := MatchFilesystemToProjection(tracker, scan, scanState)
    moves := DetectMoves(MoveDetectionInput{
        Tracker:       tracker,
        Scan:          scan,
        Matches:       matches,
        PreferFileKey: scanState.FileKeyReliable,
        HashLimits:    DefaultMoveDetectionLimits(),
    })

    intents := DeriveRootIntents(p.IDGenerator, manifest, tracker, scan, matches, moves, p.ActorID, p.ActorType)

    if scan.Incomplete {
        env.SaveScanCursor(scan.CursorPath)
        env.QueueStream(p.RootStreamID)
    } else {
        env.ClearScanCursor()
    }

    mutations := ApplyRootIntentsAndBuildMutations(rootDoc, intents)

    // Local creates discovered by stat-only scan are recorded for later targeted
    // byte reads. Do not read their bytes here.
    env.InsertPendingContentCreates(intents.PendingContentCreates)
    env.UpdateManifestProjectionStats(matches, moves, intents)

    return mutations, nil
}
```


## H3. Two-phase local create

A stat-only root scan discovers an untracked path; it does not read bytes during the scan.

```text
Phase 1: stat-only root scan
  - discover unmatched path docs/new.md
  - allocate/persist entryID doc_abc in manifest_projection
  - emit root:create:doc_abc
  - record pending_content_creates row with observed FileStat if FileKey is reliable

Phase 2: bounded targeted content reads
  - claim at most MaxPendingContentCreatesPerCycle pending rows
  - read at most MaxPendingContentCreateBytesPerCycle total bytes
  - verify the file is stable using ReadBytesStable
  - emit content:init:doc_abc, depending on root:create:doc_abc
  - requeue root if pending rows remain
```

```go
type PendingCreateLimits struct {
    MaxRows  int
    MaxBytes int64
}

func (l *WorkspaceSyncLoop) ProcessPendingContentCreates(ctx context.Context, limits PendingCreateLimits) (more bool, err error) {
    batch := l.state.ClaimPendingContentCreates("needs_bytes", "reading", limits.MaxRows)
    if len(batch) == 0 {
        return false, nil
    }

    var bytesRead int64

    for _, create := range batch {
        if limits.MaxBytes > 0 && bytesRead >= limits.MaxBytes {
            l.state.ResetPendingContentCreate(create.EntryID, "needs_bytes")
            more = true
            continue
        }
        if limits.MaxBytes > 0 && bytesRead > 0 && create.ObservedSizeBytes > 0 && bytesRead+create.ObservedSizeBytes > limits.MaxBytes {
            // Do not start another large read in this cycle. A single large file
            // may exceed the byte budget, but it should be the only file read in
            // that cycle.
            l.state.ResetPendingContentCreate(create.EntryID, "needs_bytes")
            more = true
            continue
        }

        expected := create.ObservedStatPtr() // nil when observed_stat_valid = 0
        read, ok, err := l.fs.ReadBytesStable(ctx, create.MaterializedPath, StableReadOptions{
            ExpectedStat: expected,
            Capabilities: l.state.ScanCapabilities(),
            MaxBytes: l.cfg.MaxSinglePendingCreateBytes,
        })
        if err != nil {
            l.state.ResetPendingContentCreate(create.EntryID, "needs_bytes")
            return true, err
        }
        if !ok {
            l.state.CancelPendingContentCreate(create.EntryID, "create-path-changed")
            l.state.InsertScanHint(ScanHintPath, create.MaterializedPath, "create-stat-changed")
            l.Queue(l.RootStreamID)
            continue
        }

        bytesRead += int64(len(read.Bytes))
        if limits.MaxBytes > 0 && bytesRead > limits.MaxBytes && len(read.Bytes) > 0 {
            // The file was already read. Create the outbox row, but stop after it.
            more = true
        }

        update := BuildInitialContentUpdate(read.Bytes)
        outboxID := l.state.InsertOutbox(StreamMutation{
            StreamID:    create.ContentStreamID,
            MutationKey: "content:init:" + create.ContentStreamID,
            Update:      update,
            ActorID:     l.ActorID,
            ActorType:   l.ActorType,
            Reason:      "content-create-local",
            DependsOn:   create.RootMutationKey,
        })

        l.state.MarkPendingContentCreateOutboxCreated(
            create.EntryID,
            outboxID,
            read.FinalStat,
            HashBytes(read.Bytes),
        )
        l.Queue(create.ContentStreamID)
    }

    if l.state.HasPendingContentCreates("needs_bytes") {
        more = true
    }
    return more, nil
}
```

This deliberately does not optimize bulk imports in v1. A large `cp -r` progresses in bounded batches across multiple root reconciliation cycles. The byte budget is a scheduling budget, not a hard maximum for a single file: if the first pending file in a cycle is larger than `MaxPendingContentCreateBytesPerCycle` but below `MaxSinglePendingCreateBytes`, the daemon may read that one file and then stop. That keeps the daemon responsive and makes crash recovery straightforward.

Pending-create cleanup rules:

```text
1. Sender acks content_outbox_id.
2. The target content stream folds the init update locally.
3. content_projection.projected_state_id is set for the new content stream.
4. TryCompletePendingContentCreate clears manifest_projection.pending_create.
5. TryCompletePendingContentCreate marks pending_content_creates.status = 'completed'.
6. Reaper deletes old completed/cancelled rows after PendingCreateRetention.
```

## H4. PlanApplyMerged

Root projection planning is also stat-first.

```go
func (p *RootManifestProjector) PlanApplyMerged(ctx context.Context, env ProjectorEnv, rootDoc *crdt.Doc, rootStateID int64) error {
    manifest := ReadRootManifest(rootDoc)
    projection := ResolveMaterializedPaths(manifest)

    tracker := env.LoadManifestProjection()
    scanState := env.LoadScanState()

    scan, err := p.FS.Scan(ctx, ScanOptions{
        Hints: []ScanHint{{Kind: ScanHintFull, Reason: "root-apply-merged"}},
        StatOnly: true,
        Capabilities: ScanCapabilities{
            DirectoryMTimeReliable: scanState.DirectoryMTimeReliable,
            FileKeyReliable:        scanState.FileKeyReliable,
            CTimeReliable:          scanState.CTimeReliable,
        },
        UseDirCache: scanState.DirectoryMTimeReliable,
        CursorPath:  scanState.CursorPath,
        Budget:      DefaultRootScanBudget(),
    })
    if err != nil { return err }

    plan := DiffProjection(tracker, projection, scan, rootStateID)

    for _, job := range plan.FSJobs { env.InsertFSJob(job) }
    for _, streamID := range plan.ContentStreamsToQueue {
        env.EnsureStream(streamID, "content")
        env.QueueStream(streamID)
    }

    if scan.Incomplete {
        env.SaveScanCursor(scan.CursorPath)
        env.QueueStream(p.RootStreamID)
    } else {
        env.ClearScanCursor()
    }

    return nil
}
```

## H5. Root projection jobs

### Remote create

```text
insert manifest_projection row
ensure content stream exists
queue content stream
```

Root projector does not write bytes.

### Remote rename/move

```text
fs_job:
  kind = move-entry
  source_path = old materialized path
  target_path = new materialized path
```

Execution:

```go
WorkspaceFS.MoveIfNoTarget(source, target)
```

This preserves dirty local bytes. It does not rewrite content.

### Remote tombstone

If local file is clean:

```text
fs_job: delete-clean-entry
```

If local file is dirty or unknown:

```text
fs_job: detach-dirty-entry
```

Detach means:

```text
1. Preserve bytes at a safe path.
2. Allocate new documentID.
3. Insert pending manifest_projection row for new ID.
4. Queue root stream.
5. Next CaptureLocal creates root entry + content init for the dirty bytes.
6. Old entry remains tombstoned.
```

### Path conflict

Root contains duplicate desired paths. No root mutation. Projection jobs move/track entries into deterministic materialized paths.

---

# Part I: ContentProjector

## I1. Responsibility

`ContentProjector` owns file bytes for one content stream.

```text
local file edit -> Y.Text("content") CRDT update
content CRDT update -> filesystem write
dirty divergence -> requeue and preserve local bytes
```

It does not move, rename, delete, or create namespace entries.

## I2. CaptureLocal

Content capture uses the known-clean stat fast path. `FileKey` is part of equality. If FileKey is missing, changed, or unreliable, read bytes.

```go
func (p *ContentProjector) CaptureLocal(ctx context.Context, env ProjectorEnv, doc *crdt.Doc) ([]StreamMutation, error) {
    projection := env.GetContentProjection(p.StreamID)
    if projection == nil || env.HasBlockingFSJob(p.StreamID) {
        return nil, nil
    }

    stat, err := p.FS.Stat(ctx, projection.MaterializedPath)
    if err != nil { return nil, err }
    if !stat.Exists { return nil, nil } // root handles namespace delete

    caps := env.ScanCapabilities()
    if projection.StatValid && SameStatTuple(projection.Stat, stat, caps) {
        return nil, nil
    }

    local, ok, err := p.FS.ReadBytesStable(ctx, projection.MaterializedPath, StableReadOptions{
        Capabilities: caps,
        MaxBytes: env.MaxContentReadBytes(),
    })
    if err != nil { return nil, err }
    if !ok {
        env.QueueStream(p.StreamID)
        return nil, nil
    }

    localHash := HashBytes(local.Bytes)
    if localHash == projection.ProjectedHash {
        env.UpdateContentProjectionStat(p.StreamID, local.FinalStat)
        return nil, nil
    }

    projectedState := env.LoadState(projection.ProjectedStateID)
    if projectedState == nil { return nil, ErrUnknownProjectedBase }

    baseText := MaterializeContentText(projectedState)
    update := BuildTextUpdateFromBase(projectedState, baseText, string(local.Bytes))

    return []StreamMutation{{
        StreamID:    p.StreamID,
        MutationKey: "content:edit:" + p.StreamID + ":" + localHash,
        Update:      update,
        ActorID:     p.ActorID,
        ActorType:   p.ActorType,
        Reason:      "content-local-edit",
    }}, nil
}
```

This fast path is only a skip-read optimization. It never authorizes overwriting. Remote writes still go through `WorkspaceFS.WriteIfUnchanged`.

## I3. PlanApplyMerged

```go
func (p *ContentProjector) PlanApplyMerged(ctx context.Context, env ProjectorEnv, doc *crdt.Doc, stateID int64) error {
    projection := env.GetContentProjection(p.StreamID)
    if projection == nil { return nil }

    content := doc.GetText("content").ToString()
    targetHash := HashString(content)

    env.InsertFSJob(FSJob{
        JobKey:        "content:write:" + p.StreamID + ":" + strconv.FormatInt(stateID, 10),
        Kind:          "write-content",
        StreamID:      p.StreamID,
        EntryID:       projection.EntryID,
        TargetPath:    projection.MaterializedPath,
        ExpectedHash:  projection.ProjectedHash,
        TargetHash:    targetHash,
        TargetStateID: stateID,
    })

    return nil
}
```

When the write job succeeds:

```text
content_projection.projected_state_id = targetStateID
content_projection.projected_hash = targetHash
content_projection stat tuple = final stat including file_key
streams.projected_state_id = targetStateID
```

If `WriteIfUnchanged` fails with divergence, mark the content projection dirty and requeue the content stream. Do not overwrite.

---

# Part J: Common operations

## J1. Local create

```text
User creates docs/a.md.
Watcher inserts dir/path hints and queues root.
Root scan discovers unmatched path/stat only.
Root projector allocates doc_a and emits root:create:doc_a.
Root projector inserts pending_content_creates(doc_a, observed stat).
After commit, ProcessPendingContentCreates reads only docs/a.md bytes in a bounded batch.
It emits content:init:doc_a dependent on root:create:doc_a.
Sender sends root update first, then content update.
```

## J2. Remote create

```text
Root stream receives doc_a.
Root projector inserts manifest_projection row and queues content stream.
Content stream syncs doc_a.
ContentProjector writes docs/a.md when content state is available.
```

## J3. Local edit

```text
User edits docs/a.md.
Root projector sees same namespace identity and emits no root mutation.
ContentProjector stats docs/a.md.
If SameStatTuple matches, no read/no update.
If stat changed, ContentProjector reads bytes and emits content update if bytes differ from projected hash.
```

## J4. Remote edit

```text
Content stream receives update.
ContentProjector plans write-content fs_job.
WorkspaceFS.WriteIfUnchanged writes merged content only if local hash matches projected hash.
If local diverged, preserve bytes, mark dirty, requeue content stream.
```

## J5. Local rename/move

```text
docs/a.md -> docs/b.md
```

Root projector detects same entry via FileKey if reliable, or bounded clean-hash if unique. It emits:

```text
doc_a.loc = locFor("docs/b.md")
```

Content stream unchanged.

## J6. Remote rename while local file dirty

Root projector moves path with `MoveIfNoTarget`, preserving bytes. Content projector later sees dirty bytes at the new path and emits the content update.

## J7. Local delete

Tracked file missing. Root projector emits tombstone for that entry.

## J8. Remote delete while local file clean

`DeleteIfUnchanged(path, projectedHash)` succeeds and projection untracks the entry.

## J9. Remote delete while local file dirty

Root projector does not delete. It detaches the dirty bytes into a new document create. Old doc remains tombstoned.

## J10. Concurrent same-path create

```text
Client A offline creates README.md -> doc_a.
Client B offline creates README.md -> doc_b.
```

After root sync:

```text
doc_a desired README.md
doc_b desired README.md
```

Projection:

```text
min(doc_a, doc_b) -> README.md
other             -> README (conflict <id>).md
```

Both identities survive. No root repair write.

---

## J11. Directory operations

The schema supports first-class directories. Directory handling should be implemented as normal root manifest entries, not as path-special cases. Files still own their content streams; directories own no content stream.

### Directory create

Local empty directory create:

```text
mkdir docs
```

Root scan sees an unmatched directory path and emits:

```text
root:create-dir:dir_abc
  entriesById[dir_abc] = { kind: 'dir', loc: locFor('docs') }
```

Remote directory create materializes with `WorkspaceFS.EnsureParent` / `MkdirIfMissing`. Empty directories are therefore preserved.

### Directory rename

Preferred local detection uses strong directory identity when available:

```text
old directory path missing
new directory path has same reliable FileKey
  -> update dir entry loc only
  -> descendants keep the same parentId and need no root mutations
```

If directory FileKey is unreliable, the projector may detect a prefix move only when the evidence is strong: all or nearly all tracked descendants under the old prefix appear under one new prefix and each descendant matches by reliable FileKey or clean hash. If evidence is not strong, fall back to per-entry behavior: missing tracked entries become tombstones and unmatched paths become creates.

Remote directory rename projects as:

```text
1. derive old materialized prefix and new materialized prefix
2. if old dir exists and new dir does not exist, schedule one move-dir job
3. otherwise schedule per-entry move jobs in deterministic order
4. update manifest_projection.materialized_path for the directory and descendants after successful jobs
```

A directory move must preserve bytes. It must not rewrite content files. Content dirty state remains attached to the same content streams after the path move.

### Local `rm -rf`

A local recursive delete is observed as a directory entry and descendant entries missing from the filesystem snapshot. The root projector emits tombstones for every missing live entry in that subtree:

```text
rm -rf docs/
  -> tombstone child files and child directories
  -> tombstone docs directory
```

Use deterministic deepest-first ordering when building intents so tests are stable, but CRDT correctness must not depend on that order. The content streams are not immediately garbage-collected; file entries are tombstoned and content stream garbage collection is a later retention policy.

### Remote directory delete

A normal user/UI directory delete should be represented as an explicit subtree tombstone transaction, not only a tombstone on the parent directory. Remote projection then safe-deletes each clean local file and directory in the subtree. Dirty or unknown local files are detached into new documents before the old tombstoned entries are untracked.

If the root state contains only a parent directory tombstone while children remain live, treat those children as orphans. This can happen through concurrency or malformed older clients. Do not silently delete surviving child identities. Materialize them under the deterministic orphan namespace described in Part C, for example:

```text
Recovered/orphans/<entryID>/<original-name>
```

This orphan recovery is surprising for an intentional `rm -rf`, so intentional directory delete operations must tombstone the subtree. Orphan recovery is for concurrent edge states, not the normal directory-delete UX.

### Directory collision policy

If a remote directory rename collides with a local directory or file:

```text
- never overwrite the local target
- if target is tracked, resolve through deterministic materialized conflict paths
- if target is untracked/dirty, detach it into a new pending document or recovered path
- requeue root projection
```

This follows the same rule as file path collisions: preserve all identities and bytes; resolve the local path view deterministically.

# Part K: Crash recovery

## K1. Crash after local create ID allocation

SQLite has:

```text
manifest_projection pending_create doc_a
pending_content_creates doc_a
outbox root:create:doc_a
```

On restart, daemon reuses doc_a. No duplicate document.

## K2. Crash after root create sent but content init not sent

Root outbox row is acked. `pending_content_creates` may still be `needs_bytes`, or the `content:init` outbox may exist but be unacked or not yet locally applied/projected. On restart, `ProcessPendingContentCreates`, target-stream `ReconcileOne`, and the sender continue from SQLite rows. The pending row reaches `completed` only after the content init is acked and `content_projection.projected_state_id` is set.

## K3. Crash after remote update merged but before file write

`streams.latest_state_id` advanced, but `streams.projected_state_id` did not. `fs_jobs` has pending write. On restart, retry job. Do not treat old file as local edit against new state.

## K4. Crash after file write but before marking job done

On restart, inspect job. If target path already has `target_hash`, mark job done and advance projected state. Otherwise retry or classify divergence.

## K5. Crash during filesystem operation

`fslock.sqlite` write transaction is released by SQLite when process exits. `fs_jobs` row remains pending/running. On startup, reset stale `running` jobs to `pending`.

---

# Part L: Migration milestones

## Milestone 1: Generic backend stream storage

Add `root_stream_id`, `crdt_stream_heads`, `crdt_stream_updates`, and `crdt_stream_checkpoints`.

## Milestone 2: Generic stream websocket/API

Add `/ws/workspaces/{workspaceID}/streams/{streamID}` and generic update POST.

## Milestone 3: Extend `internal/ycrdt`

Add explicit GUID creation/read, YMap support, nested map/object support, and optional subdoc reference support.

## Milestone 4: Root manifest helpers

Add:

```go
func ReadRootManifest(doc *crdt.Doc) (RootManifest, error)
func ApplyRootIntents(doc *crdt.Doc, intents []RootIntent) ([]byte, error)
func ValidateRootManifest(previous, next RootManifest) error
func ResolveMaterializedPaths(manifest RootManifest) Projection
```

## Milestone 5: Root-derived workspace API

Make `GET /api/workspace` derive documents from root stream.

## Milestone 6: Compatibility shims

Change old document create/move/delete endpoints to mutate root/content streams.

## Milestone 7: Daemon `state.sqlite`

Create the full v1 schema from day one, including nullable stat-cache columns, `scan_hints`, `directory_scan_cache`, `scan_state`, `pending_content_creates`, and `fs_jobs`. This includes `content_projection.materialized_path UNIQUE`, `pending_content_creates.content_outbox_id REFERENCES stream_outbox(id)`, and the pending-create observed-stat CHECK. The scan-acceleration code can remain disabled until later, but the schema should not require a future projection-table migration.

## Milestone 8: Daemon `fslock.sqlite`

Add global filesystem operation mutex and route all `WorkspaceFS` mutations through it.

## Milestone 9: RootManifestProjector baseline

Stop daemon local create/move/delete from calling semantic REST APIs. Emit root/content stream mutations instead.

## Milestone 10: ContentProjector generic streams

Move content sync from document-specific endpoints to generic streams. Existing document websocket can remain an alias.

## Milestone 11: Remove SQL document authority

After migration, SQL `documents` is no longer source of truth.

## Milestone 12: Wire scan acceleration

1. Initialize `scan_state` singleton on first boot and run reliability probes.
2. Stat-only `WorkspaceFS.Scan`.
3. Bounded scan hints with overflow-to-full behavior.
4. Directory cache with child-count and JSON-size caps.
5. Periodic full-scan scheduling using `PeriodicFullScanInterval`.
6. Directory mtime reliability probe.
7. FileKey reliability probe.
8. Bounded hash move fallback.
9. Bounded pending content create finalizer and lifecycle reaper.
10. ContentProjector known-clean fast path where `SameStatTuple` includes FileKey.
11. Directory operation planning for first-class directory entries.

# Part M: Tests

## M1. Backend tests

```text
- generic stream update stores update and advances head.
- duplicate update does not advance head twice.
- checkpoint + tail restore equals full replay.
- stream websocket SyncStep1/Step2 works for root and content streams.
- root validator rejects malformed entries.
- root validator rejects contentStreamId mutation.
- root validator allows duplicate desired paths.
- workspace metadata derives deterministic materialized paths.
- old document create shim creates root entry + content stream.
- old document move shim updates root loc.
- old document delete shim tombstones root entry.
```

## M2. Root resolver property tests

```text
- same manifest always produces same projection.
- duplicate sibling names produce stable conflict paths.
- entryID order determines canonical path.
- conflict suffix is not written back into root.
- tombstoned entries do not materialize.
- orphaned entries materialize under recovered namespace.
- parent cycles do not crash projection.
- directory rename updates descendant derived paths.
```

## M3. SQLite/projector tests

```text
- local create persists generated docID before send.
- restart after local create reuses same docID.
- content init waits for root create ack.
- duplicate inbox update ignored.
- outbox retry safe after crash.
- fs_job pending survives restart.
- fs_job done inferred if target hash already present.
- running fs_job resets to pending after restart.
```

## M4. Scan acceleration tests

```text
- SameStatTuple returns false when FileKey changes
- ContentProjector does not short-circuit when FileKey is unreliable
- FileKey reliability probe passes on stable local filesystem and can be forced to fail in tests
- directory mtime reliability probe stores result in scan_state
- first daemon boot inserts scan_state singleton and full scan hint
- periodic full scan inserts full hint after PeriodicFullScanInterval
- scan_hints overflow collapses to one full hint
- directory_scan_cache refuses directories above child/JSON caps
- WorkspaceFS.Scan falls back to budgeted readdir when directory_mtime_reliable = 0
- move detection uses FileKey first
- bounded hash fallback refuses large candidate sets
- bulk rename on unreliable FileKey filesystem tombstones old docs and creates new docs rather than guessing identity
- ReadBytesStable does not hold fslock during byte read and rejects replacement during read
- pending content creates prepare only MaxPendingContentCreatesPerCycle rows per cycle
- pending content creates requeue remaining rows and clean up after content outbox ack
```

## M5. Integration tests

```text
1. Two daemons create README.md offline.
   After sync, both docs exist and both daemons project same conflict paths.

2. Daemon A edits file while daemon B renames it.
   After sync, path converges and content is preserved.

3. Daemon A deletes file while daemon B edits it.
   After sync, original doc is tombstoned and dirty edit becomes new doc.

4. Primary workspace and agent workspace both edit same content stream.
   CRDT merge preserves both edits.

5. Remote rename collides with local untracked file.
   Local file is detached or conflict-projected; nothing overwritten.
```

---

# Part N: Known v1 limitations

## N1. Bulk imports are bounded, not optimized

A large `cp -r` that introduces hundreds or thousands of new files is processed in batches. The daemon deliberately reads only `MaxPendingContentCreatesPerCycle` pending creates and only `MaxPendingContentCreateBytesPerCycle` bytes per root reconciliation cycle. This preserves responsiveness and crash safety but means large imports may take multiple cycles to fully sync.

## N2. Bulk renames on unreliable FileKey filesystems may lose document identity

On filesystems where FileKey is unreliable, such as some NFS, SMB, or FUSE configurations, the daemon skips FileKey-based move detection and uses only bounded clean-hash matching. If a bulk rename produces more candidates than `MaxHashMoveCandidates`, v1 falls back to the safe interpretation:

```text
missing tracked entries -> tombstone old entries
unmatched new paths -> create new entries
```

This preserves bytes and convergence, but it does not preserve document identity for every renamed file. Thread anchors, last-viewed state, and other document-ID-scoped metadata remain attached to the tombstoned old documents. This is a known v1 limitation.

## N3. No fuzzy rename similarity

The daemon does not implement Git-style fuzzy rename similarity. Notty preserves document identity only when the evidence is strong: reliable FileKey or bounded exact clean-hash match.

# Final recommendation

Use the CRDT-native root manifest as the metadata authority, generic streams as the backend persistence model, and SQLite as the daemon's local transactional sync store. Add scan acceleration as a conservative optimization:

```text
- stat-first root scans
- byte-lazy file content reads
- FileKey only after a reliability probe
- FileKey included in content stat equality
- bounded hint and directory-cache growth
- bounded hash fallback only for tiny move candidate sets
- capability-aware fallback to safe full stat scans
- two-phase local create content reads
```

The final behavior is deterministic, crash-recoverable, and safe under two-way sync. It preserves stable document identity while still making common filesystem operations — create, edit, rename, delete, concurrent same-path create, and dirty-vs-remote conflicts — project reliably between CRDT streams and local files.
