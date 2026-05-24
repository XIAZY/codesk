# CRDT-Native Subdocs Implementation Summary

Branch: `codex/crdt-native-subdocs`  
Latest pushed implementation commit: `0cf6d50 Document CRDT stream implementation summary`  
Review hardening changes after that commit are tracked in `docs/crdt-review-fixes-execplan.md`.

## What Was Implemented

- Replaced document-path authority with a CRDT-native stream model:
  - workspace namespace metadata lives in a root manifest CRDT stream;
  - each file's bytes live in a separate generic content stream;
  - stable document identity is based on entry/content stream IDs, not paths.
- Added backend generic stream storage and APIs:
  - `workspaces.root_stream_id`;
  - `crdt_stream_heads`, `crdt_stream_updates`, `crdt_stream_checkpoints`;
  - generic stream update POST and websocket sync routes.
- Added root manifest architecture:
  - native `Y.Map("entriesById")` root manifest storage;
  - deterministic materialized path resolver;
  - duplicate desired-path handling via projection-only conflict paths;
  - tombstone, orphan, cycle, and directory-descendant projection behavior.
- Converted document-facing backend APIs into stream-backed aliases:
  - create, move, delete, update, by-path lookup, workspace snapshots, and document websocket behavior now derive from root/content streams.
- Removed old backend SQL document authority:
  - old document tables are dropped during schema initialization;
  - workspace documents are projected from the root stream instead.
- Replaced the daemon document sync/cache architecture with stream projection:
  - `streamProjection`;
  - `WorkspaceSyncLoop`;
  - `RootManifestProjector`;
  - `ContentProjector`;
  - `StreamSender`;
  - generic stream websocket receiver.
- Added daemon local state and recovery:
  - `state.sqlite` for streams, projections, outbox/inbox, scan hints, pending creates, and filesystem jobs;
  - `fslock.sqlite` for serialized filesystem mutations;
  - stale running job/reset behavior on startup.
- Added stat-first filesystem projection:
  - capability probes for FileKey, directory mtime, and ctime;
  - stat-only scans with path hints and scan cursors;
  - byte-lazy pending content create finalization;
  - dirty-byte preservation for remote writes/deletes;
  - move detection using FileKey first, clean-hash fallback second.
- Added primary and agent workspace stream projection:
  - agent workspaces sync through the same stream projection architecture;
  - primary/agent edits to the same content stream are merged through CRDT updates.
- Removed obsolete daemon legacy files:
  - `document_cache.go`;
  - `document_sync.go`;
  - `reconcile_queue.go`;
  - `replica.go`.
- Hardened the stream sync state machine after review:
  - root manifest validation rejects hard-deleted entries;
  - root mutation keys include intent payloads;
  - backend stream writes are authorized by root/content stream authority;
  - compatibility creates apply root and content updates transactionally;
  - daemon stream queue application persists state and applied markers atomically;
  - projection paths advance only after filesystem jobs complete;
  - retryable move collisions can be revived, and move swaps use temp moves;
  - materialized directory deletes tombstone directory trees;
  - stale backend-rejected content outbox rows are dropped without blocking later sends;
  - content streams not live in the root manifest cannot apply local outbox or project bytes to disk.

## Namespace And Compatibility Policies

- Root namespace names are normalized with `rootmanifest.NormalizeName`: names are trimmed and compared case-insensitively within a parent directory.
- `Location.Name` preserves the trimmed display name, while `Location.NormName` is the canonical sibling key.
- Duplicate desired paths are resolved by projection-only conflict paths. The root manifest location remains the desired namespace location; conflict paths are local materialization details.
- Generic stream APIs are now the authority for CRDT writes. A stream update is accepted only for the workspace root stream or a live content stream referenced by the current root manifest.
- Legacy document HTTP routes and document websocket routes remain as stream-backed compatibility aliases. They do not restore SQL document authority.
- Document responses no longer expose `clientIdSeed`; backend-authored initial content updates let Yrs assign the document client ID, matching daemon-created CRDT docs.
- `DocumentRooms`, `DocumentRoom`, and `DocumentConn` names remain for websocket room plumbing even when the room carries a generic stream. Renaming them was deferred because it is a broad mechanical diff with little correctness value.
- Log-path notification suppression remains an existing product notification policy outside the CRDT projection model. Agent logs are not treated as normal notification-bearing documents.

## Different From Or Not Implemented From The Design

- Native Yjs subdoc references were not implemented as first-class references. The authority remains `contentStreamId`, which the proposal allows; subdoc references were optional convenience.
- The `internal/ycrdt` wrapper was extended only as far as needed for GUIDs and root manifest `Y.Map` JSON/string operations. A broad ergonomic nested-object/subdoc API was not added.
- A legacy root manifest text fallback, `rootManifestJSON`, remains as a read fallback for previously-written updates. New writes use `Y.Map("entriesById")`.
- Compatibility surfaces remain intentionally:
  - old document HTTP routes are stream-backed aliases;
  - document websocket routes alias generic content streams;
  - `workspace.snapshot` events still exist for clients;
  - legacy actor query behavior remains tested.
- No production backfill from pre-existing SQL `documents` rows into root/content streams was added. Schema initialization drops the old document tables instead of migrating their data.
- Bulk import handling is bounded, not optimized. Large imports are processed across multiple reconciliation cycles by byte and row budgets.
- Property-style exhaustive tests were not added for every resolver/projector invariant. Focused unit, integration, Docker regression, Postgres, frontend, and live smoke coverage were added and run.
- Field-level CRDT storage for each root entry property was not implemented. Root entries are stored as whole-entry JSON values inside `Y.Map("entriesById")`; validation and projection hardening protect the current representation.
- The branch is pushed but not merged.

## Errors Hit And Fixes Applied

- Host Go tests initially could not link `libyrs.a`.
  - Fixed by using the Rust toolchain path and building/running with `PATH="$HOME/.cargo/bin:$PATH"`.
- Yjs compatibility tests needed frontend Node dependencies.
  - Fixed by installing/running through the frontend test setup.
- Docker was not available to the user without sudo.
  - Fixed by running Docker-backed tests with `sudo -n`.
- Postgres schema still had document-path uniqueness assumptions.
  - Fixed by removing SQL document authority and allowing duplicate desired paths in root projection.
- Yjs delete-set updates could change content without changing only the state-vector check.
  - Fixed stream update no-op detection to compare encoded document state as well.
- Append-heavy Docker regression could capture an early partial snapshot from an actively-written file.
  - Fixed pending content creates with a 5 second stat-stability window before content init.
- Unprojected remote creates were initially misread as local deletes.
  - Fixed root projection to requeue/project missing remote content instead of tombstoning it prematurely.
- Offline same-path create regression produced only one surviving document in earlier iterations.
  - Fixed root projection so unprojected remote entries do not claim existing untracked local files at the same path.
- Outbox retries could see the same logical root mutation encoded as different Yjs update bytes.
  - Fixed outbox handling so unapplied rows are replaceable and already-applied logical mutations are idempotent.
- Projection swaps hit `UNIQUE constraint failed: manifest_projection.materialized_path`.
  - Fixed projection upserts to tolerate temporary empty paths and clear prior owners before assigning final paths.
- Primary/agent Docker regression failed because agent projection could tombstone tracked files while content projection was still catching up.
  - Fixed agent/root projection to requeue content streams, preserve tracked entries with pending projection/outbox work, and restore missing content projection paths.
- Offline local same-path Docker regression failed with:
  - `daemon_a file README.md did not converge: got "offline-a\n" want "offline-b\n"`.
  - Root cause: generated conflict paths could be mistaken for user-authored moves when file keys were reused during projection.
  - Fixed by adding path ownership guards before file-key and clean-hash move detection accepts a move.
- Dirty-delete Docker regression later exposed a stale old-content stream race:
  - the daemon locally applied an old content outbox row after the root manifest had tombstoned that stream;
  - the backend correctly rejected the stale content update as unreferenced;
  - local projection had already advanced the old stream's clean hash to dirty bytes, allowing a clean-delete job to remove the file.
  - Fixed by dropping non-live content outbox before local apply and by keeping dropped outbox rows as local dirty-byte evidence for root delete planning.

## Verification Status

Final review-hardening verification on 2026-05-24 passed:

- `PATH="$HOME/.cargo/bin:$PATH" go test ./...`
- `cd frontend && npm test && npm run build`
- daemon installer and uninstaller script tests
- `sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" scripts/test-postgres.sh`
- `sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" scripts/test-live.sh`
- `sudo -n env HOME="$HOME" PATH="$HOME/.cargo/bin:$PATH" GOCACHE=/tmp/notty-gocache GOPATH="$HOME/go" go test -tags=regression ./test/regression -count=1 -timeout 30m -v`

Known non-failing warnings: frontend Vite esbuild/oxc deprecation warnings and the existing production chunk-size warning.
