# CRDT-Native Subdocument Architecture Proposal

## Goals

- Make workspace document metadata CRDT-native.
- Use generated root stream IDs, not reserved names.
- Avoid double writes: no SQL document table as document-manifest authority.
- Keep current content transport and reconciliation temporarily where useful.
- Build a new generic per-workspace stream loop as the future synchronization foundation.
- Defer detailed root filesystem projection policy until requirements are settled.

## Source Of Truth

Postgres stores CRDT streams. It should not own document metadata semantics.

```text
workspaces
  id
  root_stream_id

crdt_stream_heads
  workspace_id
  stream_id
  state_vector
  update_id
  updated_at

crdt_stream_updates
  id
  workspace_id
  stream_id
  update
  actor_id
  actor_type
  created_at

crdt_stream_checkpoints
  workspace_id
  stream_id
  update_id
  crdt_state
  state_vector
  created_at
```

Invariant:

```text
stream_id == ydoc.guid
```

Root doc stream:

```text
stream_id = workspace.root_stream_id
```

Document content stream:

```text
stream_id = document guid / document id
```

SQL remains appropriate for auth, users, daemons, agents, threads, inbox, and tokens.

## Root Doc Model

The root doc contains workspace document metadata and subdocument references.

Exact path-conflict and filesystem projection policy is intentionally not finalized yet, but the root doc must support:

- Document identity.
- Document path/name/title metadata.
- Deleted or tombstoned state.
- Subdoc GUID / content stream ID.
- Created and updated metadata.
- Enough structure to derive a document tree.

Initial conservative shape:

```ts
root.getMap("documentsById")
// docId -> metadata object/map

root.getMap("documentRefs")
// docId -> Y.Doc({ guid: docId })
```

Open requirement: decide whether path uniqueness is represented as:

```text
documentsById only
pathClaims path -> claims
filesByPath path -> docId
```

Do not finalize this until root projection semantics are designed.

## Backend

Backend becomes generic stream storage and transport for CRDT docs.

It should provide:

```text
GET/WS stream sync for stream_id
POST/WS stream update for stream_id
checkpoint + tail sync
ack persisted update
broadcast stream updates
```

Backend should no longer own document metadata writes.

Remove semantic document mutation authority:

```text
POST /documents
PATCH /documents/{id}
DELETE /documents/{id}
GET /documents/by-path
```

Derived read APIs can remain:

```text
GET /api/workspace
```

But they must materialize root CRDT temporarily and return derived metadata. They are not source of truth.

## Frontend

Frontend syncs the root doc and uses it for metadata.

- Load workspace bootstrap: `workspaceId`, `rootStreamId`.
- Sync root `Y.Doc`.
- Render document list/tree from root doc.
- Mutate root doc for create/rename/delete once root schema is finalized.
- Continue active document content sync by document stream ID.
- Destroy inactive content docs to free memory.

Frontend should not call semantic document mutation APIs.

## Daemon

Daemon moves toward one `WorkspaceSyncLoop` per local workspace replica:

```text
primary workspace:
  ~/Notty/workspaces/<workspace_id>

agent workspace:
  ~/Notty/agents/<workspace_id>/<agent_id>
```

Each workspace loop owns:

```text
.notty/streams/<stream_id>/state.bin
.notty/streams/<stream_id>/inbox.log
.notty/streams/<stream_id>/outbox.log
.notty/streams/<stream_id>/metadata.json
```

There is no daemon-wide shared document cache in the final design.

## Generic Stream Loop

The new loop is generic and stream-oriented.

Channels carry signals only. Disk carries durable truth.

```text
reconcileCh <- streamId
sendCh      <- streamId
```

Inbound websocket:

```text
receive update
append to inbox.log
reconcileCh <- streamId
```

Reconciler:

```text
lock stream
load state.bin into Y.Doc
capture local outgoing changes before applying inbox
append local updates to outbox.log
apply local updates to in-memory doc
apply inbox updates
persist merged state.bin
call stream-specific projector
sendCh <- streamId if outbox exists
```

Websocket sender:

```text
read outbox.log
send unacked updates
record ack
reconcileCh <- streamId
```

Only the reconciler clears or finalizes acked outbox state.

## Projectors

The loop uses projectors, but detailed root projector policy is deferred.

Required interface direction:

```go
type StreamProjector interface {
    CaptureLocal(ctx context.Context, doc *crdt.Doc) ([]StreamUpdate, error)
    ApplyMerged(ctx context.Context, doc *crdt.Doc) error
}
```

### Content Projector Requirements

- Use `state.bin` as the outgoing diff base.
- Do not store separate projected-base CRDT or text files.
- Capture local disk diff before applying incoming updates.
- If `state.bin` is missing or corrupt and a local file exists, archive and resync.
- Project merged `Y.Text("content")` to disk safely through `WorkspaceFS`.

### Root Projector Requirements

Root projection policy still needs design. Requirements:

- Translate local create/rename/delete observations into root CRDT updates.
- Apply merged root metadata to local tracking and filesystem structure.
- Handle path conflicts deterministically.
- Never silently lose document identity.
- Use `WorkspaceFS` for actual file operations.
- Preserve content streams by document ID across renames.
- Treat deletion as tombstone unless explicitly garbage-collected.

## Transport

Phase 1:

```text
generic stream websocket per stream
```

This supports the root stream immediately and can coexist with current content document websocket while migrating.

Phase 2:

```text
one workspace websocket
frames include streamId
```

The reconciler should not change when transport changes.

## Internal YCRDT Work

Minimal support needed for root docs:

- Create `Y.Doc` with explicit GUID.
- Read GUID.
- `YMap` support.
- Nested map/object support.
- Subdoc reference support.
- Maybe `YArray` later for ordering/tree.

## Implementation Milestones

1. Add `root_stream_id` to workspaces.
2. Add generic CRDT stream storage.
3. Add generic stream websocket/API.
4. Extend `internal/ycrdt` for root manifest structures.
5. Add `WorkspaceSyncLoop`, `StreamStore`, queue, and transport interfaces.
6. Use the new loop for the root stream only.
7. Update frontend to sync/read root doc metadata.
8. Update daemon to maintain local root stream state.
9. Remove semantic document metadata mutation APIs once frontend/daemon mutate root directly.
10. Migrate content docs into the new per-workspace stream loop later.
11. Finalize root filesystem projection policy before moving daemon create/rename/delete fully onto root mutations.

## Open Requirements For Root Metadata

Before implementing root metadata mutations, decide:

- How to represent path uniqueness.
- How to preserve concurrent same-path creates.
- How to project path conflicts to disk.
- How to represent folders/order.
- How tombstones are garbage-collected.
- How thread anchors behave after tombstone.
- How root doc validation prevents malformed metadata.

This keeps the architecture future-proof without prematurely baking in weak filesystem manifest semantics.
