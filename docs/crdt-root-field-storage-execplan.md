# Close Root and Stream CRDT Correctness Gaps

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. If the plan changes during implementation, revise this file so a future contributor can restart from this file alone.

## Purpose / Big Picture

The root manifest is the shared directory tree for a Notty workspace. It records entries such as files and directories, their parent/name location, their content stream IDs, and tombstones for deleted entries. This root manifest is edited by multiple clients, so the storage unit inside the CRDT must match the facts that can change independently.

Today the native root map stores one JSON object per entry in `entriesById`. That makes the entire entry the conflict unit. If one replica renames `doc_a` while another replica tombstones `doc_a` from the same base state, the stale whole-entry object from either side can overwrite the other independent fact. Backend validation can reject some invalid final states, but it cannot recover information that was overwritten before validation sees it.

After this change, root manifest writes store mutable entry facts in separate CRDT maps. A concurrent rename and tombstone of the same entry should merge into one entry with the new location and the tombstone. The behavior is demonstrated by focused tests in `internal/rootmanifest` that fail against whole-entry storage and pass with field-level storage, plus backend tests proving root validation and content stream authorization still read the same logical manifest.

This plan also closes the remaining review items that protect the same first-principle model: stream reads and websocket sync must use the same root authority as writes, local queue state must only be advanced by the crash-atomic apply path, schema initialization must never destroy legacy document data silently, resolver invariants must be exercised beyond a few examples, and notification suppression must be explicit metadata rather than a file-extension special case. These are included because each one is about preserving state-machine truth rather than cosmetic cleanup.

## Progress

- [x] (2026-05-24T01:16:58Z) Read `.agent/PLANS.md` and confirmed this file must be a self-contained living ExecPlan.
- [x] (2026-05-24T01:16:58Z) Inspected `internal/rootmanifest/root_manifest.go`, `internal/ycrdt/map.go`, `backend/internal/notty/root_manifest.go`, and root manifest tests to anchor the plan in current APIs.
- [x] (2026-05-24T01:16:58Z) Chose a field-map overlay design instead of a destructive one-time migration because concurrent first writes from a legacy manifest must not bootstrap stale fields.
- [x] (2026-05-24T01:23:34Z) Expanded the plan to include stream read authorization, unsafe queue API removal, fail-closed legacy table handling, resolver invariant tests, and notification policy metadata as a separable milestone.
- [x] (2026-05-24T01:38:03Z) Reviewed `docs/crdt-root-field-storage-execplan-review.md` and incorporated the valid gaps: reject new legacy root writes, add validated read/shape validation, specify RawMessage field-map parsing, clarify entry existence, and make metadata/policy updates explicit intents.
- [x] (2026-05-24T01:52:56Z) Added low-level tests for same-entry concurrent `loc` and `tombstone` changes on a fresh field-map document.
- [x] (2026-05-24T01:52:56Z) Added low-level tests for same-entry concurrent `loc` and `tombstone` changes when the base document still uses legacy `entriesById` storage.
- [x] (2026-05-24T01:52:56Z) Added tests proving legacy base plus field overlay reads, partial field-only entries are rejected by validated reads, and `ApplyIntents` writes field maps without writing legacy `entriesById`.
- [x] (2026-05-24T01:52:56Z) Implemented field-map constants, legacy-base/field-overlay reading, `json.RawMessage` field-map parsing, `ReadValidated`, `ValidateShape`, explicit metadata intent fields, notification-policy root metadata storage, and field-map writes in `internal/rootmanifest/root_manifest.go`.
- [x] (2026-05-24T01:52:56Z) Verified the low-level root manifest package with `go test ./internal/rootmanifest`.
- [x] (2026-05-24T02:14:30Z) Added backend tests that reject root updates mutating legacy `entriesById` or `rootManifestJSON`.
- [x] (2026-05-24T02:14:30Z) Updated backend root manifest aliases and backend reads to use validated root manifests for authority/listing/persistence decisions.
- [x] (2026-05-24T02:14:30Z) Updated backend root manifest tests to expect field maps, not whole-entry root writes.
- [x] (2026-05-24T02:14:30Z) Replaced non-atomic local queue helper APIs with the atomic queue API and moved direct stream-state persistence setup into a `_test.go` fixture helper.
- [x] (2026-05-24T02:14:30Z) Enforced the same stream authority check for HTTP writes, websocket sync reads, websocket updates, websocket handshake, and awareness room membership.
- [x] (2026-05-24T02:14:30Z) Made Postgres schema initialization fail closed when legacy document tables contain rows instead of dropping them.
- [x] (2026-05-24T02:14:30Z) Added resolver invariant/property tests and a fuzz target for root manifest projection behavior.
- [x] (2026-05-24T02:14:30Z) Replaced runtime path-based notification suppression with explicit root entry notification policy metadata.
- [x] (2026-05-24T02:14:30Z) Ran targeted validation for `internal/rootmanifest`, `backend/internal/notty`, and `daemon/internal/syncer`.
- [x] (2026-05-24T02:14:30Z) Ran full Go validation, frontend tests/build, Postgres-backed backend validation, live backend/Postgres smoke validation, and Docker daemon/backend/filesystem regression validation.

## Surprises & Discoveries

- Observation: `ApplyIntents` currently reads a whole manifest, mutates `Entry` structs, and writes changed entries back to `entriesById` as whole JSON values.
  Evidence: In `internal/rootmanifest/root_manifest.go`, `ApplyIntents` calls `json.Marshal(next.EntriesByID[id])` and then `entries.InsertJSON(txn, id, string(payload))`.

- Observation: The existing backend validation protects against hard deletes, kind changes, content stream ID changes, and tombstone removal, but it cannot preserve independent facts after a CRDT merge has overwritten them.
  Evidence: `Validate` compares `previous` and `next` manifests after `Read`. If a stale whole-entry write wins before `Read`, the losing field is no longer visible to validation.

- Observation: The `internal/ycrdt.YMap` wrapper already has enough operations for field-level storage.
  Evidence: `internal/ycrdt/map.go` exposes `InsertJSON`, `InsertString`, `Remove`, `JSON`, and `GetJSON`. This plan can use `InsertJSON` for all field values and `JSON` for map scans.

- Observation: A one-time bootstrap that writes every field from a legacy whole-entry manifest is unsafe under concurrent first writers.
  Evidence: If replica A writes `loc=NewName` and replica B writes `tombstone=T` from the same legacy base, a bootstrap from replica B would also write stale `loc=OldName` into the new location map. That stale location would then conflict with A's real rename. The safe design is to keep legacy whole-entry data as a read base and write only the fields changed by each intent.

- Observation: The daemon has a crash-atomic queue apply path, but the old non-atomic helpers remain callable.
  Evidence: `daemon/internal/syncer/state_stream_state.go` exposes `ApplyReadyLocalOutbox`, `ApplyUnappliedInbox`, and `PersistLatestStreamDoc` in addition to `ApplyStreamQueueAtomically`. The first two mark outbox or inbox rows applied outside the transaction that persists the resulting stream state.

- Observation: Backend stream writes are root-authorized, but websocket sync reads can currently restore a stream before applying the same authority check.
  Evidence: `backend/internal/notty/store_streams.go` uses `streamUpdateAllowedTx` in `ApplyStreamUpdate`, while `backend/internal/notty/server_streams.go` calls `EncodeStreamSyncUpdates` for websocket sync step 1. `EncodeStreamSyncUpdates` restores the requested stream ID directly.

- Observation: Postgres schema initialization currently drops old document tables.
  Evidence: `backend/internal/notty/store_postgres.go` includes `DROP TABLE IF EXISTS document_checkpoints`, `document_updates`, `document_heads`, `documents`, and `document_mentions`. Dropping empty compatibility tables is harmless; dropping nonempty legacy tables is silent data loss.

- Observation: Notification suppression used to be encoded as a path extension rule, and is now encoded as root entry metadata.
  Evidence: Before this plan, `backend/internal/notty/store.go` used `.log` extension detection in document inbox/update notification paths. The implementation now carries `notificationPolicy` from the root manifest into `Document` and checks that metadata instead.

- Observation: Field-map writes alone do not stop old clients or malformed root updates from mutating the old whole-entry root storage.
  Evidence: Backend root updates arrive as generic Yjs updates through `ApplyStreamUpdate`. If an update writes `entriesById[doc_a]` or `rootManifestJSON`, the current transition validator only sees the resulting logical manifest. Without a legacy-storage fingerprint check, old whole-entry writes can still affect fields that have not yet been overlaid into field maps.

- Observation: `YMap.JSON()` produces JSON objects whose values can be strings, objects, or other JSON values.
  Evidence: A field map such as `entryKindById` encodes `{"doc_a":"file"}`, while `entryLocById` encodes `{"doc_a":{"parentId":"root","name":"new.md","normName":"new.md"}}`. The reader must parse map values as `json.RawMessage`, not `string`, before decoding each field.

- Observation: `crdt.Doc.Update` must not call `doc.GetMap` inside the update callback because the document lock is already held.
  Evidence: An early `go test ./internal/rootmanifest` run hung while field write helpers fetched maps inside `doc.Update`. The implementation now pre-acquires `rootFieldMaps` before `doc.Update`, and tests that manually write maps pre-acquire those maps before their update callbacks.

- Observation: The first low-level field-map milestone is implemented and passing.
  Evidence: `go test ./internal/rootmanifest` returned `ok  	notty/internal/rootmanifest	(cached)` after adding same-entry rename/tombstone tests, legacy overlay tests, validated-read tests, and field-map storage tests.

- Observation: The full implementation is validated across unit, integration, frontend, and Docker whole-stack paths.
  Evidence: `go test ./...` passed; `go test ./internal/rootmanifest -run '^$' -fuzz=FuzzRootResolverInvariants -fuzztime=10s` passed; `sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" scripts/test-postgres.sh` passed; `sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" scripts/test-live.sh` passed with `live smoke passed`; `cd frontend && npm test && npm run build` passed; `sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" NOTTY_REGRESSION_STRESS_LINES=100 go test -tags=regression ./test/regression -count=1` passed.

## Decision Log

- Decision: Treat field-level root storage as a correctness requirement for distributed editing, not as a cleanup task.
  Rationale: The root manifest is necessarily replicated and edited by multiple clients. Entry location, tombstone, and immutable identity fields are separate facts. Storing them as one entry blob creates artificial conflicts and can lose valid concurrent work.
  Date/Author: 2026-05-24 / Codex

- Decision: Use top-level per-field Y.Maps instead of nested maps for the first implementation.
  Rationale: `internal/ycrdt.YMap` already supports top-level named maps and JSON scalar/object values. Top-level maps avoid adding nested-map wrapper code while still making the CRDT conflict unit one field per entry ID.
  Date/Author: 2026-05-24 / Codex

- Decision: Use legacy `entriesById` or `rootManifestJSON` as a read base, then overlay field maps on top.
  Rationale: This avoids a dangerous concurrent migration bootstrap. Legacy entries continue to supply fields that have not been rewritten yet. Field maps override only the facts they explicitly contain.
  Date/Author: 2026-05-24 / Codex

- Decision: New root manifest writes must not update `entriesById` or rewrite `rootManifestJSON`.
  Rationale: Writing whole entries after field maps exist recreates the original conflict unit. Clearing legacy text can also delete the only base for entries whose fields have not yet been overlaid. Legacy storage should remain read-only compatibility data until a later, coordinated compaction.
  Date/Author: 2026-05-24 / Codex

- Decision: Do not implement legacy compaction or removal in this plan.
  Rationale: Removing legacy storage safely requires knowing that all active writers understand field maps, or using a separate migration protocol. This plan's goal is to make new writes field-level and preserve existing documents.
  Date/Author: 2026-05-24 / Codex

- Decision: Use one stream authority rule for reads, writes, websocket room membership, and awareness.
  Rationale: The model has only two authorized stream categories: the workspace root stream and live content streams referenced by the root manifest. Endpoint-specific exceptions let unauthorized clients infer or read state even if writes are blocked.
  Date/Author: 2026-05-24 / Codex

- Decision: Remove normal access to non-atomic queue apply helpers rather than documenting them as dangerous.
  Rationale: Crash atomicity is a state-machine invariant. Keeping public helpers that can mark rows applied before the merged state is persisted leaves a regression path in the codebase.
  Date/Author: 2026-05-24 / Codex

- Decision: Make legacy SQL document handling fail closed before adding any destructive cleanup.
  Rationale: Schema initialization should be idempotent and non-destructive. If old document tables contain rows, the server should return a clear migration error instead of dropping user data. Automatic backfill can be added later once the legacy schema contract is fully specified.
  Date/Author: 2026-05-24 / Codex

- Decision: Replace ongoing path-based notification suppression with explicit metadata, not another path rule.
  Rationale: A path such as `*.log` is not authority or policy. If some documents are quiet, that should be represented as a document/root-entry fact like `notificationPolicy=quiet` and validated like other metadata.
  Date/Author: 2026-05-24 / Codex

- Decision: Treat legacy root storage as read-only compatibility data after this migration starts.
  Rationale: Keeping `entriesById` and `rootManifestJSON` as a read base preserves existing workspaces, but accepting new writes to those structures preserves the old whole-entry conflict unit. Backend root update validation must reject any logical update that mutates legacy root storage.
  Date/Author: 2026-05-24 / Codex

- Decision: Separate root manifest parsing from root manifest shape validation.
  Rationale: Low-level reads need to parse mixed legacy and field-map state for tests and transition checks, but backend authorization and listing paths need a semantically valid manifest. `Read` should parse and report corrupt JSON; `ReadValidated` should call `ValidateShape` before callers use the manifest for authority decisions.
  Date/Author: 2026-05-24 / Codex

- Decision: Use `entryKindById` as the existence marker for field-only entries.
  Rationale: Legacy entries can exist because they appear in legacy storage. Field-only entries need one authoritative existence fact. A location-only or tombstone-only field key may be parsed for diagnostics, but `ValidateShape` must reject a non-root entry with missing or invalid kind.
  Date/Author: 2026-05-24 / Codex

- Decision: Keep notification policy as a later milestone if the core root storage migration becomes too broad.
  Rationale: Explicit notification metadata is the right design, but it is less coupled to CRDT root convergence than field maps, legacy-write rejection, stream authorization, queue atomicity, migration safety, and resolver invariants. It should not delay the core correctness fixes if scope grows.
  Date/Author: 2026-05-24 / Codex

## Outcomes & Retrospective

This plan is implemented and validated. Root writes now store mutable facts in field maps, legacy root storage is a read-only compatibility base, backend root updates reject legacy storage mutations, backend authority paths use validated root reads, stream access is root-authorized across writes, sync reads, websocket handshake, updates, and awareness room entry, queue state advances only through the crash-atomic apply path, schema initialization fails closed for nonempty legacy document tables, resolver invariants are covered by deterministic and fuzz tests, and notification quieting is explicit root metadata rather than a filename rule.

Validation passed at unit, integration, frontend, and whole-stack levels. The whole-stack validation included a Docker live smoke that started backend plus Postgres and exercised user registration, workspace creation, document creation, and workspace snapshot retrieval over HTTP, plus the Docker regression package that drives backend, daemon, Postgres, websockets, and filesystem projection together.

## Context and Orientation

The repository root is `/home/ubuntu/notty`.

A CRDT is a data type that can be edited independently on multiple replicas and later merged. In this repository, CRDT documents are wrapped by `internal/ycrdt`. A `Doc` is a CRDT document, a `YMap` is a key/value map inside that document, and a CRDT update is a byte slice that can be applied to another replica with `crdt.ApplyUpdateV1`.

The root manifest is the logical file tree for a workspace. The root manifest code lives in `internal/rootmanifest/root_manifest.go`. Its public model is:

- `Manifest`, which has `EntriesByID map[string]Entry`.
- `Entry`, which contains `ID`, `Kind`, `Loc`, `ContentStreamID`, `Tombstone`, `CreatedBy`, `UpdatedBy`, `CreatedAt`, and `UpdatedAt`.
- `Location`, which contains `ParentID`, `Name`, and `NormName`.
- `Tombstone`, which records who deleted an entry and when.
- `Intent`, which describes a root change such as `create-file`, `create-dir`, `loc`, or `tombstone`.

The current legacy storage names in `internal/rootmanifest/root_manifest.go` are:

    TextName = "rootManifestJSON"
    MapName = "entriesById"
    RootEntryID = "root"

`Read(doc)` currently prefers the legacy native map `entriesById` and falls back to the older text value `rootManifestJSON`. `ApplyIntents(doc, intents)` reads the manifest, mutates entry structs, validates the full manifest transition with `Validate(previous, next)`, writes changed entries into `entriesById`, and clears `rootManifestJSON`.

The backend aliases these root manifest functions in `backend/internal/notty/root_manifest.go`. Backend stream storage validates root updates in `backend/internal/notty/store_streams.go`: it reads the previous root manifest, applies the incoming CRDT update, reads the next root manifest, and calls `ValidateRootManifest(previousRoot, nextRoot)`. Backend content stream authorization also uses `ReadRootManifest`, so the logical output of `Read` must remain stable after storage changes.

The daemon root projector in `daemon/internal/syncer/root_projector.go` reads the root manifest with `rootmanifest.Read`, compares it to the local filesystem, and emits `rootmanifest.Intent` values through `rootmanifest.ApplyIntents`.

The daemon local queue state is managed in `daemon/internal/syncer/state_stream_state.go`. `ApplyStreamQueueAtomically` is the safe path because it applies ready local outbox rows and unapplied inbox rows to the CRDT document, inserts a new `stream_states` row, updates `streams.latest_state_id`, and only then marks the exact inbox/outbox rows applied inside one SQL transaction. `ApplyReadyLocalOutbox` and `ApplyUnappliedInbox` perform the same logical work without that transaction boundary and must not remain normal APIs.

Backend websocket stream sync is implemented in `backend/internal/notty/server_streams.go`. The websocket endpoint currently upgrades, joins a `DocumentRoom`, and then handles sync messages. Sync step 1 calls `Store.EncodeStreamSyncUpdates` in `backend/internal/notty/store_streams.go`, which restores the requested stream state. Stream write authorization exists in `ApplyStreamUpdate` through `streamUpdateAllowedTx`, but read/sync authorization must use the same root-derived rule before state is sent or awareness is shared.

Postgres schema initialization is in `backend/internal/notty/store_postgres.go`. The legacy document tables named `documents`, `document_heads`, `document_updates`, `document_checkpoints`, and `document_mentions` are no longer the authority after the stream rewrite, but nonempty versions of those tables still represent user data. A safe initializer may drop empty obsolete tables, but it must fail closed when they contain rows.

Backend document notifications are computed in `backend/internal/notty/store.go`. Before this plan, notification suppression was based on `.log` filename detection. The implementation now uses explicit root-entry metadata named `notificationPolicy`, with the empty value and `normal` meaning ordinary behavior and `quiet` meaning document-update inbox items should not be created.

## Plan of Work

First, add tests that describe the correctness property before changing production code. In `internal/rootmanifest/root_manifest_test.go`, add a test that starts from a root document with `doc_a`, forks two replicas, applies a `loc` intent on one replica and a `tombstone` intent on the other, merges the updates in both orders, and expects the final manifest to contain both `Loc.Name == "new.md"` and a non-nil tombstone. Run this against a fresh document created through `ApplyIntents` so it exercises the new field-map-only path after implementation.

Add a second low-level test for the migration critical path. Build a legacy base document by manually writing a whole `Entry` JSON object to `doc.GetMap(MapName)` or by writing a `Manifest` JSON string to `doc.GetText(TextName)`, then fork two replicas from that base. Apply a `loc` intent on one replica and a `tombstone` intent on the other. After merging, expect the same final manifest with both facts preserved. This test guards against the unsafe bootstrap design where each first writer copies stale whole-entry fields into field maps.

Add a read overlay test in `internal/rootmanifest/root_manifest_test.go`. Seed a legacy `entriesById` entry with `Loc.Name == "old.md"` and `ContentStreamID == "doc_a"`. Then write only an `entryLocById` field-map value with `Loc.Name == "new.md"`. `Read` must return `new.md` for the location and must still return `ContentStreamID == "doc_a"` from the legacy base. This proves field maps are overlays, not partial replacement manifests.

Then update `internal/rootmanifest/root_manifest.go`. Keep `TextName`, `MapName`, and `RootEntryID` for compatibility. Add constants for the new field maps:

    KindMapName = "entryKindById"
    LocMapName = "entryLocById"
    ContentStreamMapName = "entryContentStreamIdById"
    TombstoneMapName = "entryTombstoneById"
    CreatedByMapName = "entryCreatedById"
    UpdatedByMapName = "entryUpdatedById"
    CreatedAtMapName = "entryCreatedAtById"
    UpdatedAtMapName = "entryUpdatedAtById"
    NotificationPolicyMapName = "entryNotificationPolicyById"

Implement small helpers in the same file. `readLegacyManifest(doc)` should contain the current `Read` fallback logic: prefer `entriesById` when nonempty, otherwise use `rootManifestJSON`, otherwise return `New()`. `readFieldOverlays(doc, base)` should scan the JSON of each new field map, union all entry IDs it sees, initialize any missing entry as `Entry{ID: entryID}`, and overlay only the fields present in those maps. It should start with a cloned base to avoid mutating caller-owned structs. Field-only entries are considered well-formed only when `entryKindById` provides a valid kind; overlay parsing may expose partial entries, but `ValidateShape` must reject them before backend authorization or listing uses the manifest.

Use `KindMapName` as the intended existence index for field-only entries, but still union keys from every field map while reading. If a corrupt or partial document has a location or tombstone for an entry without a kind, the reconstructed manifest should expose that partial entry rather than silently dropping it; `Validate` can then reject it when a validated update path is used.

Field map values should be encoded with `json.Marshal` and written with `YMap.InsertJSON`. This works for both scalars and objects: `KindMapName` stores JSON strings like `"file"`, `LocMapName` stores JSON objects like `{"parentId":"root","name":"new.md","normName":"new.md"}`, `TombstoneMapName` stores JSON objects, and `NotificationPolicyMapName` stores JSON strings like `"quiet"` when the notification policy milestone is implemented. When reading a map, first unmarshal the map JSON as `map[string]json.RawMessage`, then decode each value into the matching Go type. Error messages must include the map name and entry ID, for example `entryLocById[doc_a]: invalid location JSON: ...`. Treat absent map keys as "no override". Do not write JSON `null` as a tombstone removal marker because tombstone removal is not allowed by the data model.

Change `Read(doc)` to call `readLegacyManifest(doc)` first and then overlay field maps. An empty document still returns `New()`. A legacy-only document still reads exactly as it does today. A field-only document reads from field maps plus the synthetic root entry from `New()`. A mixed document reads legacy as the base and field maps as per-field overrides.

Add `ValidateShape(manifest Manifest) error` and `ReadValidated(doc *crdt.Doc) (Manifest, error)` in `internal/rootmanifest/root_manifest.go`. `ValidateShape` should contain the single-manifest requirements currently embedded in `Validate`: entries map exists, root exists, root ID/kind/location/tombstone are valid, every entry key matches entry ID, non-root entries have valid kind, files have nonempty content stream IDs, directories do not have content stream IDs, non-root entries have valid locations, notification policy values are valid if that milestone is implemented, and tombstoned entries remain structurally valid. `Validate(previous, next)` should call `ValidateShape(next)` before transition checks such as no hard delete, no kind change, no content stream ID change, and no tombstone removal. Backend authorization and listing paths should call `ReadValidated`, not bare `Read`.

Change `ApplyIntents(doc, intents)` so it still constructs `previous` and `next` logical manifests and still calls `Validate(previous, next)`. The change is only the CRDT write phase. For a create intent, write every field needed for that new entry to the field maps: kind, location for non-root entries, content stream ID for file entries, tombstone if present, and metadata fields if nonempty. For a `loc` intent, write only the location field and explicit metadata fields carried by the intent. For a `tombstone` intent, write only the tombstone field and explicit metadata fields carried by the intent. Add `UpdatedBy` and `UpdatedAt` fields to `Intent` if move/delete metadata should be updated by loc or tombstone intents; otherwise record that metadata changes are create-only for this milestone. Do not write unchanged fields from `next` for existing entries.

Make tombstones monotonic at the storage and validation levels. `entryTombstoneById` is set-only at the application level: no normal intent removes a tombstone key, and `ApplyIntents` must never call `YMap.Remove` for tombstone removal. Backend validation must reject any root update whose logical result removes a tombstone that existed in the previous validated manifest, including malicious field-map key removals.

Do not clear `rootManifestJSON` and do not write `entriesById` from `ApplyIntents`. This is intentional. Legacy values are compatibility base data. A later compaction can remove them only after all clients are known to write field maps and after compaction itself is designed to avoid stale concurrent field writes.

Reject new writes to legacy root storage at the backend root-update boundary. In `backend/internal/notty/store_streams.go`, when `applyStreamUpdateTx` identifies the stream as the workspace root stream, compute a legacy root storage fingerprint before applying the incoming CRDT update and again after applying it. The fingerprint should include the trimmed `rootManifestJSON` text and a canonical JSON representation of `entriesById`. If the fingerprint changes, reject the update with an error such as `legacy root storage is read-only; use field maps`. This check belongs in backend validation because root updates can arrive through websocket or generic stream paths, bypassing `rootmanifest.ApplyIntents`. Existing legacy storage remains readable as the base; it is simply read-only for new root updates.

Update `backend/internal/notty/root_manifest.go` only if new constants need aliases for tests. Keep `ReadRootManifest`, `ApplyRootIntents`, and `ValidateRootManifest` as thin wrappers around `internal/rootmanifest` so the backend and daemon continue to share one implementation.

Update `backend/internal/notty/root_manifest_test.go`. Replace `TestApplyRootIntentsStoresNativeEntriesMap` with a test that expects field maps to contain the new entry fields and expects the legacy text to remain untouched when it was already present. For a fresh doc, it is acceptable for legacy text to be empty because there was no legacy base. The test should still call `ReadRootManifest` and verify that the logical manifest contains the created entry.

Review backend root validation tests in `backend/internal/notty/store_streams_test.go` and server tests in `backend/internal/notty/server_test.go`. The logical manifest behavior should not change, so most tests should keep passing without large edits. If a test directly inspects `entriesById`, update it to inspect the appropriate field maps or, preferably, to assert behavior through `ReadRootManifest`.

Run daemon syncer tests after the rootmanifest tests pass. The daemon should be unaffected at the API level because it reads and writes through `rootmanifest.Read` and `rootmanifest.ApplyIntents`, but it is a critical path because it is the main local writer of root intents.

Next, remove the non-atomic queue apply surface from `daemon/internal/syncer/state_stream_state.go`. Delete `ApplyReadyLocalOutbox` and `ApplyUnappliedInbox` or make them unexported helpers that are called only inside the transaction owned by `ApplyStreamQueueAtomically`; do not leave methods that mark applied rows independently. `PersistLatestStreamDoc` is used as a test fixture in many syncer tests, not as the normal queue apply path. Prefer renaming it to an unexported fixture helper and updating same-package tests, or add a comment and a compile-time search test that prevents production code from calling it. The goal is that production queue reconciliation has one durable state transition: `ApplyStreamQueueAtomically`.

Then centralize stream access authorization in `backend/internal/notty/store_streams.go`. Rename or generalize `streamUpdateAllowedTx` into a shared helper such as `streamAccessAllowedTx`. The rule should not depend on endpoint type: the root stream is allowed, and a non-root stream is allowed only when the current root manifest contains a live file entry whose `contentStreamId` equals that stream ID. Use that helper from `ApplyStreamUpdate` and from sync-read paths. `EncodeStreamSyncUpdates` should either call the shared authorization before restoring the requested stream, or be split into a public authorized method and a private restore method used only by internal root validation. Do not add an endpoint-specific allowlist.

Update `backend/internal/notty/server_streams.go` so websocket authorization happens before `s.upgrader.Upgrade`, before a room is created or joined, and before awareness snapshots are sent. For unauthorized streams, the handler should return an HTTP error during the handshake instead of opening a websocket and later closing it. Once authorized, per-message updates still go through `ApplyStreamUpdate`, so write authorization remains enforced even if root state changes during a long session. If live root state changes make a previously authorized content stream tombstoned during an open session, the next write should fail through `ApplyStreamUpdate`; a separate session-revocation mechanism is out of scope unless tests show awareness data leaks after tombstone.

Add backend tests for stream access. In `backend/internal/notty/store_streams_test.go`, assert that `EncodeStreamSyncUpdates` or its replacement rejects an unreferenced stream ID, rejects a stream referenced only by a tombstoned file entry, allows the root stream, and allows a live referenced content stream. In `backend/internal/notty/server_test.go`, add websocket handshake tests that verify an unauthorized stream does not join a room and does not receive awareness or sync data. These tests must exercise reads, not just writes.

Make Postgres schema initialization fail closed for legacy document tables. Before executing any `DROP TABLE IF EXISTS document...` statement in `backend/internal/notty/store_postgres.go`, add a helper that checks whether each legacy table exists and contains rows. If any legacy table contains rows, return a typed error such as `ErrLegacyDocumentsNeedMigration` with a message that names the table and row count. It is acceptable in this plan to drop empty legacy tables after the guard passes. Do not add an environment-variable bypass in normal initialization; an operator who wants destructive cleanup can drop the old tables explicitly outside the server. This keeps the runtime rule simple and non-special.

Add a Postgres integration test for the fail-closed migration guard. In `backend/internal/notty/store_postgres_test.go`, create a minimal legacy `documents` table with one row in the disposable `notty_test` database, call the schema initializer, and expect `ErrLegacyDocumentsNeedMigration`. Then delete the row and expect initialization to succeed. The existing test harness already skips without `NOTTY_DATABASE_TEST_URL`, so the full suite remains runnable in environments without Postgres while the integration path is verifiable with `scripts/test-postgres.sh` or the documented environment variables.

Add resolver invariant tests in `internal/rootmanifest`. Keep the existing example tests, then add a deterministic randomized test or Go fuzz target that generates small manifests with directories, files, duplicate desired names, tombstones, orphan parents, and simple cycles. The invariants are: `Resolve` must never panic; tombstoned entries must not appear in `Projection.EntryPath`; every live non-root entry must either have a materialized path or be classified as an orphan when disconnected; materialized paths must be unique; root must never appear as a projected child; duplicate sibling names must produce stable paths independent of map iteration order; and every reachable non-root child path must have its projected parent path as a prefix. Seed the fuzzer with known edge cases from existing tests so `go test` exercises them even without a fuzz run.

Finally, replace runtime path-based notification suppression with explicit metadata if quiet document updates remain a product requirement and the core correctness milestones above are not at risk. Treat this as a separate milestone that can be deferred without weakening CRDT convergence. Add `NotificationPolicy string` to `rootmanifest.Entry` with allowed values `""`, `"normal"`, and `"quiet"`; the empty value should read as `"normal"` for compatibility. Store it in its own field map, for example `entryNotificationPolicyById`, so changing notification policy does not conflict with location or tombstone changes. Add an explicit intent shape such as `Type: "notification-policy"`, `EntryID`, and `NotificationPolicy`; do not rely on create-only support if existing documents need policy changes. Validate allowed values in `ValidateShape`. Update backend stream-derived document state to carry the root entry's notification policy, and replace `isLogDocumentPath` checks in `backend/internal/notty/store.go` with a policy check. If existing `.log` documents must stay quiet, write an explicit one-time data migration or daemon intent that marks those entries `quiet`; do not leave extension matching in the runtime notification path. Add tests that prove a `quiet` document does not create document-update inbox items and a `.log` document without explicit quiet policy behaves like any other normal document.

## Concrete Steps

Work from the repository root:

    cd /home/ubuntu/notty

Confirm the working tree before implementation. Existing untracked review files are not part of this plan and should not be edited unless explicitly needed:

    git status --short --branch

Add tests first:

    go test ./internal/rootmanifest

Before implementation, the new same-entry concurrent tests should fail or be impossible to satisfy with whole-entry storage. A useful failure message should identify the lost field, for example:

    concurrent tombstone overwrote rename: doc_a path "old.md"

Implement `readLegacyManifest`, `readFieldOverlays`, field-map constants, and field-map write helpers in `internal/rootmanifest/root_manifest.go`. Keep changes focused in this package until low-level tests pass. When writing field maps, pre-acquire the `YMap` handles before entering `doc.Update`; do not call `doc.GetMap` from inside the update callback because the CRDT document lock is already held.

Run the low-level package tests:

    go test ./internal/rootmanifest

Expected result after the low-level implementation:

    ok  	notty/internal/rootmanifest	...

Current evidence from 2026-05-24T01:52:56Z:

    ok  	notty/internal/rootmanifest	(cached)

Add shape validation and backend legacy-write rejection, then run focused root storage tests:

    go test ./internal/rootmanifest -run 'Test.*ValidateShape|Test.*ReadValidated|Test.*Field'
    go test ./backend/internal/notty -run 'TestRootUpdateRejectsLegacyEntriesByIdWrite|TestRootUpdateRejectsRootManifestJSONWrite|Test.*Root.*Legacy'

Expected result is that malformed partial field-map manifests fail validated reads, valid mixed legacy/field manifests pass validated reads, and incoming root updates that mutate `entriesById` or `rootManifestJSON` are rejected while existing legacy data remains readable.

Update backend tests and aliases as needed, then run:

    go test ./backend/internal/notty

Expected result:

    ok  	notty/backend/internal/notty	...

Remove or quarantine unsafe queue helpers, then verify that production code cannot call the old non-atomic path:

    rg -n "ApplyReadyLocalOutbox|ApplyUnappliedInbox" daemon/internal/syncer --glob '!**/*_test.go'

Expected result after the change is no matches. If `PersistLatestStreamDoc` remains for fixtures, verify it is not called outside tests:

    rg -n "PersistLatestStreamDoc" daemon/internal/syncer --glob '!**/*_test.go'

Expected result is no production callers, or only a clearly documented unexported helper if the implementation renames it. Then run the queue tests:

    go test ./daemon/internal/syncer -run 'TestApplyStreamQueueAtomically|Test.*StreamQueue|Test.*Outbox|Test.*Inbox'

Expected result:

    ok  	notty/daemon/internal/syncer	...

Implement centralized stream access authorization and websocket pre-upgrade authorization, then run focused backend tests:

    go test ./backend/internal/notty -run 'Test.*Stream.*Authorization|Test.*Unauthorized.*Stream|Test.*Websocket.*Stream'

Expected result is that unknown or tombstoned content streams are rejected for both writes and sync reads, while the root stream and live referenced content streams remain accessible.

Implement the Postgres legacy table guard, then run the integration test when a disposable Postgres database is available:

    NOTTY_DATABASE_TEST_URL="$NOTTY_DATABASE_TEST_URL" NOTTY_DATABASE_TEST_ISOLATED=1 go test ./backend/internal/notty -run 'TestPostgres.*Legacy.*Document'

If `NOTTY_DATABASE_TEST_URL` is unset, the test should skip rather than fail. When it runs, it must prove that a nonempty legacy table causes a typed migration error and an empty legacy table can be dropped.

Add resolver invariant tests, then run both normal tests and a short fuzz session:

    go test ./internal/rootmanifest
    go test ./internal/rootmanifest -run '^$' -fuzz=FuzzRootResolverInvariants -fuzztime=10s

Expected result is no panics and no invariant failures. If fuzzing is unavailable in the local Go toolchain, record that in `Surprises & Discoveries` and keep the deterministic seeded test active under ordinary `go test`.

If explicit notification policy metadata is implemented in this pass, remove runtime path-extension suppression and run notification-focused backend tests:

    go test ./backend/internal/notty -run 'Test.*Notification|Test.*Inbox|Test.*Log'

Expected result is that only documents with explicit `notificationPolicy=quiet` suppress document-update inbox items. A `.log` filename alone must not suppress notifications.

Run daemon syncer tests:

    go test ./daemon/internal/syncer

Expected result:

    ok  	notty/daemon/internal/syncer	...

Run the full Go test suite:

    go test ./...

Expected result is that every package passes. If unrelated packages fail due to missing external services or environment assumptions, record the exact failure in `Surprises & Discoveries` and run the closest deterministic subset that covers the root manifest, backend validation, and daemon projection critical paths.

If frontend tests are part of the branch's normal merge gate, run them after Go tests:

    cd /home/ubuntu/notty/frontend
    npm test
    npm run build

Only record these frontend results if the frontend directory and scripts exist in the current checkout. This root storage change is backend/daemon/internal logic, so frontend failures must be evaluated for relevance before changing frontend code.

## Validation and Acceptance

The primary acceptance behavior is CRDT merge correctness. A test named like `TestApplyIntentsConcurrentRenameAndTombstonePreservesBothFacts` should prove the fresh field-map path. It should create a base root with one file, fork two CRDT docs, apply a rename on one and a tombstone on the other, merge both updates, and assert that the final manifest has the new name and the tombstone. Run the merge in both update orders. This is the critical distributed behavior.

A second test named like `TestApplyIntentsLegacyBaseConcurrentRenameAndTombstonePreservesBothFacts` should prove the migration path. It should seed the base document using only legacy whole-entry storage, apply the same concurrent rename and tombstone from two replicas, merge both updates, and assert that both facts survive. This test protects against stale first-write bootstrapping.

A read overlay test should prove that field maps do not replace the whole manifest. When a legacy entry supplies `ContentStreamID` and a field map supplies only `Loc`, `Read` must return both. This is critical for existing workspaces because not every field of every entry will be rewritten immediately.

Legacy root storage acceptance is that old root storage is read-only after the migration. A root update that changes `entriesById` or `rootManifestJSON` must be rejected by backend validation, regardless of whether the resulting logical manifest would otherwise pass `Validate`. Existing legacy values must continue to serve as read base data. Tests named like `TestRootUpdateRejectsLegacyEntriesByIdWrite` and `TestRootUpdateRejectsRootManifestJSONWrite` must cover generic CRDT updates that bypass `ApplyRootIntents`.

Shape validation acceptance is that backend authority paths never use malformed root manifests. `Read` may parse a partial manifest for low-level tests and diagnostics, but `ReadValidated` must reject a non-root entry without a kind, a file without content stream ID, a directory with content stream ID, invalid locations, invalid notification policy values, and malformed root entry shape. Backend stream authorization and document listing must use validated reads.

Backend acceptance is that root update validation still observes a complete logical manifest before and after each update. Run `go test ./backend/internal/notty` and ensure tests around root updates, content stream authorization, and invalid root transitions still pass. A content stream write should still be allowed only when the reconstructed root manifest links the content stream ID to a live file entry.

Daemon acceptance is that filesystem projection still uses the same logical manifest API. Run `go test ./daemon/internal/syncer`. Tests that capture local changes should continue to emit root intents and apply them without knowing whether storage is legacy or field maps.

Queue atomicity acceptance is that there is no production-callable path that marks inbox or outbox rows applied outside the transaction that persists the merged stream state. The focused queue tests must still include rollback tests where an injected failure before commit leaves `stream_states`, `stream_inbox.applied_at`, and `stream_outbox.local_applied_at` unchanged together. A source search must show no production callers of old non-atomic helpers.

Stream authority acceptance is that the same root-derived rule applies to every stream access path. An unreferenced stream ID must fail HTTP update, websocket sync step 1, and websocket update. A tombstoned file's content stream must fail the same paths. The root stream and a live referenced content stream must still sync and update successfully. Unauthorized websocket attempts must not create or join a room and must not receive awareness snapshots.

Migration safety acceptance is fail-closed behavior. In a disposable Postgres database with a nonempty legacy `documents` table, `initPostgresSchema` must return `ErrLegacyDocumentsNeedMigration` or an error wrapping that value, and the row must still exist after the error. With empty legacy tables, initialization may drop them and continue. The plan does not accept silent drops of nonempty legacy document tables.

Resolver invariant acceptance is broader than example snapshots. Deterministic seeded tests and a short fuzz run must show that `Resolve` does not panic, does not materialize tombstones, assigns unique materialized paths, handles cycles and orphans deterministically, produces stable conflict paths independent of map iteration order, and projects reachable children under their projected parent path. A failing fuzz seed must be committed as a deterministic regression test before implementation is considered complete.

Notification policy acceptance, if that milestone is implemented in this plan, is data-driven behavior. A document whose root entry has `notificationPolicy=quiet` must not create document-update inbox items. A document whose root entry has no policy or `notificationPolicy=normal` must follow the ordinary notification rules even if the path ends in `.log`. Runtime code should not call `isLogDocumentPath` to decide notification behavior after the migration is complete.

Full acceptance is:

    cd /home/ubuntu/notty
    go test ./...

The implementation is not complete until the full suite passes or every failure is documented as unrelated with a narrower passing command that covers the root manifest critical paths.

## Idempotence and Recovery

The implementation should be additive and safe to retry. Adding constants and helper functions in `internal/rootmanifest/root_manifest.go` can be repeated by re-running tests after each edit. The field-map reader should tolerate absent maps by treating them as empty overlays.

Do not remove or clear `entriesById` or `rootManifestJSON` during this plan. Keeping them makes the migration reversible at the data level: if a partially implemented field-map writer has a bug, existing legacy data is still present for inspection and for older code paths. New writes should simply stop adding more whole-entry state.

If a test fails after implementation, first inspect the reconstructed logical manifest from `Read`, not the raw CRDT maps. The contract for callers is the logical manifest. Raw map inspection belongs only in storage-specific tests.

If a field map receives corrupt JSON, `Read` should return an error with the map name and entry ID if possible. Do not silently ignore corrupt values. Silent ignore would hide data loss and make backend validation less useful.

If an incoming root update changes legacy storage, reject it without trying to repair or compact the document. The existing legacy data is compatibility base data, and changing it from a new update is exactly what this migration forbids. A rejected update should leave the persisted stream head and update table unchanged because `applyStreamUpdateTx` rolls back on error.

If `ReadValidated` starts rejecting data that existing code previously listed, treat that as either a real corrupt root document or a missing migration step. Do not make backend authorization silently fall back to unvalidated `Read`, because stream authority must fail closed.

The queue API change is safe to retry because it removes or narrows public methods without changing stored rows. If tests need fixture setup, add or rename fixture helpers in `_test.go` files or unexported package helpers; do not reintroduce production methods that mark rows applied outside `ApplyStreamQueueAtomically`.

The stream authorization change should fail closed. If the root manifest cannot be restored or parsed while authorizing a non-root stream, return an error and do not open a websocket. Avoid caching authorization results across requests in this plan; root manifest state is the source of truth, and stale authorization cache bugs would be harder to validate than direct checks.

The Postgres legacy-table guard must run before destructive statements and must not modify legacy tables when it returns an error. This makes the operation retryable: an operator can run a backfill or manually archive/drop the legacy tables, then start the server again. Do not add a broad "force" switch to normal startup because it would make destructive behavior easy to trigger accidentally.

Resolver fuzzing can create large failing inputs. When a fuzz failure appears, minimize it if the Go toolchain provides a minimized seed, then add a small deterministic test case to `internal/rootmanifest/root_manifest_test.go` or a committed seed under the package's fuzz testdata. Do not leave a behavior justified only by a fuzz transcript.

Notification policy migration should avoid ongoing path rules. If preserving current quiet behavior for existing `.log` files is required, write a bounded migration that sets explicit `notificationPolicy=quiet` once and record that migration in this plan. After migration, runtime notification code must consult metadata only.

Avoid broad refactors while implementing this plan. `DocumentRoom` naming and the fixed pending-create stability delay remain outside this plan unless a validation failure shows they are directly coupled to the correctness changes above. `ClientIDSeed` was removed in a follow-up cleanup after confirming daemon-created CRDT docs let Yrs assign document client IDs.

## Artifacts and Notes

The current whole-entry write shape to replace is in `internal/rootmanifest/root_manifest.go`:

    payload, err := json.Marshal(next.EntriesByID[id])
    if err := entries.InsertJSON(txn, id, string(payload)); err != nil {
        return err
    }

The existing Y.Map wrapper supports the required operations in `internal/ycrdt/map.go`:

    func (m *YMap) InsertJSON(txn *Transaction, key string, jsonValue string) error
    func (m *YMap) JSON() (string, error)
    func (m *YMap) GetJSON(key string) (string, bool, error)

The validation logic to preserve is in `internal/rootmanifest/root_manifest.go`:

    if before.Kind != "" && before.Kind != after.Kind {
        return fmt.Errorf("entry %q kind cannot change", id)
    }
    if before.Kind == EntryKindFile && before.ContentStreamID != "" && before.ContentStreamID != after.ContentStreamID {
        return fmt.Errorf("entry %q contentStreamId cannot change", id)
    }
    if before.Tombstone != nil && after.Tombstone == nil {
        return fmt.Errorf("entry %q tombstone cannot be removed", id)
    }

The test scenario that must pass after implementation is:

    base: doc_a has loc old.md and no tombstone
    replica A intent: loc doc_a new.md
    replica B intent: tombstone doc_a at 2026-05-23T00:00:00Z
    merged manifest: doc_a has loc new.md and non-nil tombstone

The legacy root storage fingerprint should cover the old compatibility structures only:

    type LegacyRootStorageFingerprint struct {
        TextJSON    string
        EntriesJSON string
    }

    func legacyRootStorageFingerprint(doc *crdt.Doc) (LegacyRootStorageFingerprint, error)

`EntriesJSON` should be canonical JSON for `doc.GetMap(MapName).JSON()` so insignificant key ordering does not trigger rejection. `TextJSON` should be trimmed and canonicalized when it contains valid JSON. Empty text and empty map JSON should compare consistently before and after no-op updates.

The non-atomic queue helpers to remove or quarantine are in `daemon/internal/syncer/state_stream_state.go`:

    func (s *WorkspaceStateDB) ApplyReadyLocalOutbox(ctx context.Context, streamID string, doc *crdt.Doc) ([]StreamOutboxRow, error)
    func (s *WorkspaceStateDB) ApplyUnappliedInbox(ctx context.Context, streamID string, doc *crdt.Doc) ([]StreamInboxRow, error)

The authorized queue path to keep is:

    func (s *WorkspaceStateDB) ApplyStreamQueueAtomically(ctx context.Context, streamID string, kind string, doc *crdt.Doc, materializedTextSHA256 string) (StreamQueueApplyResult, error)

The websocket read path to harden is in `backend/internal/notty/server_streams.go`:

    head, updates, err := store.EncodeStreamSyncUpdates(streamID, data)

That call must not send updates until the same root-derived stream authority rule used by `ApplyStreamUpdate` has accepted the stream.

The destructive schema statements to guard are in `backend/internal/notty/store_postgres.go`:

    DROP TABLE IF EXISTS document_checkpoints
    DROP TABLE IF EXISTS document_updates
    DROP TABLE IF EXISTS document_heads
    DROP TABLE IF EXISTS documents
    DROP TABLE IF EXISTS document_mentions

The path-policy logic retired from `backend/internal/notty/store.go` was:

    func isLogDocumentPath(path string) bool {
        return strings.EqualFold(filepath.Ext(strings.TrimSpace(path)), ".log")
    }

The replacement is `documentNotificationsQuiet(document)`, which checks `Document.NotificationPolicy == "quiet"`.

## Interfaces and Dependencies

The public root manifest API should remain source-compatible:

    func Read(doc *crdt.Doc) (Manifest, error)
    func ReadValidated(doc *crdt.Doc) (Manifest, error)
    func ValidateShape(manifest Manifest) error
    func ApplyIntents(doc *crdt.Doc, intents []Intent) ([]byte, error)
    func Validate(previous Manifest, next Manifest) error

Add storage constants in `internal/rootmanifest/root_manifest.go`:

    const (
        KindMapName = "entryKindById"
        LocMapName = "entryLocById"
        ContentStreamMapName = "entryContentStreamIdById"
        TombstoneMapName = "entryTombstoneById"
        CreatedByMapName = "entryCreatedById"
        UpdatedByMapName = "entryUpdatedById"
        CreatedAtMapName = "entryCreatedAtById"
        UpdatedAtMapName = "entryUpdatedAtById"
        NotificationPolicyMapName = "entryNotificationPolicyById"
    )

Recommended unexported helpers in `internal/rootmanifest/root_manifest.go`:

    func readLegacyManifest(doc *crdt.Doc) (Manifest, error)
    func readFieldOverlays(doc *crdt.Doc, base Manifest) (Manifest, error)
    func readJSONMap(doc *crdt.Doc, mapName string) (map[string]json.RawMessage, error)
    func overlayStringMap(doc *crdt.Doc, mapName string, apply func(entryID string, value string)) error
    func overlayLocationMap(doc *crdt.Doc, mapName string, apply func(entryID string, value *Location)) error
    func overlayTombstoneMap(doc *crdt.Doc, mapName string, apply func(entryID string, value *Tombstone)) error
    type rootFieldMaps struct {
        kind *crdt.YMap
        loc *crdt.YMap
        contentStream *crdt.YMap
        tombstone *crdt.YMap
        createdBy *crdt.YMap
        updatedBy *crdt.YMap
        createdAt *crdt.YMap
        updatedAt *crdt.YMap
        notificationPolicy *crdt.YMap
    }
    func rootFieldMapsForDoc(doc *crdt.Doc) rootFieldMaps
    func writeEntryCreateFields(txn *crdt.Transaction, fieldMaps rootFieldMaps, entry Entry) error
    func writeEntryLocField(txn *crdt.Transaction, fieldMaps rootFieldMaps, entryID string, loc *Location) error
    func writeEntryTombstoneField(txn *crdt.Transaction, fieldMaps rootFieldMaps, entryID string, tombstone *Tombstone) error

The exact helper names can change during implementation, but the behavior must not: reads are legacy base plus field overlays, creates write all fields for the new entry, loc writes only location, tombstone writes only tombstone, and no new code writes whole entries into `entriesById`.

If notification policy is implemented in this plan, extend `Entry` in `internal/rootmanifest/root_manifest.go`:

    type Entry struct {
        NotificationPolicy string `json:"notificationPolicy,omitempty"`
    }

The actual struct already has other fields; add only this field. Valid values are:

    const (
        NotificationPolicyNormal = "normal"
        NotificationPolicyQuiet = "quiet"
    )

The empty value must be treated as `normal` when reading older data. `ValidateShape` must reject values other than empty, `normal`, and `quiet`. `ApplyIntents` must write notification policy into `NotificationPolicyMapName` only when a create or explicit policy-change intent carries it; it must not rewrite unrelated fields.

Extend `Intent` only for fields that can be updated independently by non-create intents. If move/delete metadata is required, add:

    type Intent struct {
        UpdatedBy string
        UpdatedAt string
    }

If notification policy is implemented, add:

    type Intent struct {
        NotificationPolicy string
    }

and support a `Type: "notification-policy"` intent that writes only `NotificationPolicyMapName` for the target entry plus explicit metadata fields if supplied. Do not infer metadata changes from the current wall clock inside `ApplyIntents`; callers must pass the facts they want written so replay and idempotency remain deterministic.

Backend aliases in `backend/internal/notty/root_manifest.go` may expose the new constants if tests need them:

    const RootManifestKindMapName = rootmanifest.KindMapName

Only add aliases that are actually used. Prefer behavior-level tests through `ReadRootManifest` over raw storage assertions outside the root manifest package. Update `ReadRootManifest` to call `rootmanifest.ReadValidated` for backend authority paths, or add a separate `ReadValidatedRootManifest` wrapper and use it wherever the backend makes access-control, listing, or persistence decisions. Keep low-level tests able to call `rootmanifest.Read` directly when they need to inspect partial parse output.

The daemon queue API at the end of the plan should expose only:

    func (s *WorkspaceStateDB) ApplyStreamQueueAtomically(ctx context.Context, streamID string, kind string, doc *crdt.Doc, materializedTextSHA256 string) (StreamQueueApplyResult, error)

`ApplyReadyLocalOutbox` and `ApplyUnappliedInbox` must not remain exported production methods. If direct state persistence is still needed for tests, use an unexported helper or a `_test.go` helper whose name makes fixture intent clear.

The backend stream access API should provide one shared rule:

    func streamAccessAllowedTx(tx *sql.Tx, workspaceID string, streamID string) (string, error)

The helper returns `StreamKindRoot` for the workspace root stream and `StreamKindContent` for a live file entry referenced by the root manifest. It returns an error for empty, unknown, tombstoned, or directory-only streams. `ApplyStreamUpdate`, `EncodeStreamSyncUpdates`, and websocket handshake authorization should use this same rule.

Root update validation in `backend/internal/notty/store_streams.go` should also provide:

    func legacyRootStorageFingerprint(doc *crdt.Doc) (LegacyRootStorageFingerprint, error)

The backend should compute this fingerprint before and after applying a root stream update and reject the update when the fingerprint changes. The fingerprint check should run before `ValidateRootManifest(previousRoot, nextRoot)` returns success, so legacy writes cannot be accepted as valid root changes.

The Postgres migration guard should provide a typed error:

    var ErrLegacyDocumentsNeedMigration = errors.New("legacy document tables require migration")

and a helper with behavior equivalent to:

    func guardLegacyDocumentTablesEmpty(ctx context.Context, db *sql.DB) error

The helper must check table existence before counting rows, return nil when tables are absent or empty, and return an error wrapping `ErrLegacyDocumentsNeedMigration` when any legacy table contains rows. The schema initializer must call it before any destructive legacy table statement.

## Revision Note

Created on 2026-05-24 by Codex in response to the request for a first-principles ExecPlan. The main design choice captured here is the field-map overlay migration strategy, chosen because bootstrapping all fields from a legacy whole-entry manifest can itself create stale concurrent field writes.

Revised on 2026-05-24 by Codex to include the non-field-level review items worth fixing: one stream access rule for reads/writes/websockets, removal of unsafe queue apply APIs, fail-closed legacy SQL table handling, resolver invariant tests, and notification policy metadata as a separable milestone. The revision intentionally rejects ongoing path-based or endpoint-specific special cases.

Revised on 2026-05-24 by Codex after reviewing `docs/crdt-root-field-storage-execplan-review.md`. This revision adds backend rejection of new legacy root storage writes, `ReadValidated` and `ValidateShape`, `json.RawMessage` field-map decoding, explicit field-only entry existence semantics, monotonic tombstone storage rules, explicit metadata/policy intent fields, and the parent-prefix resolver invariant.

Revised on 2026-05-24 by Codex during implementation. This revision records that the `internal/rootmanifest` field-map read/write milestone and tests were complete at that point, documents the `doc.GetMap` inside `doc.Update` deadlock discovery, and updates helper signatures to pre-acquire field maps.

Revised on 2026-05-24 by Codex after implementation and validation. This revision records completed backend legacy-write rejection, stream access centralization, queue helper removal, fail-closed Postgres legacy table handling, resolver fuzz/invariant coverage, notification policy runtime migration, and full validation evidence including Docker whole-stack checks.
