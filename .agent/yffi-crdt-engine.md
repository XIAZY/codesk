# Replace ygo with a yffi-backed CRDT engine

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document follows `.agent/PLANS.md`. A future contributor should be able to read only this file and the repository, then continue the work without prior conversation.

## Purpose / Big Picture

Notty stores and syncs document contents as Yjs-compatible CRDT updates. The current Go CRDT engine is `github.com/reearth/ygo`, vendored under `third_party/ygo`. We found a concrete incompatibility where ygo applies a valid Yjs update sequence to the wrong text order. This work replaces ygo with a Notty-owned internal CRDT adapter backed by `y-crdt/y-crdt`'s `yffi` library, while preserving the existing websocket/y-protocol wire format. After this change, frontend Yjs clients, the backend, and daemon workspaces should exchange the same update-v1 and state-vector-v1 bytes as before, but Go-side CRDT operations should be delegated to the Rust Yrs implementation through FFI.

The observable outcome is that focused compatibility tests pass for browser/Yjs-generated updates, Go-generated updates, delete/backspace updates, UTF-16 offsets, and the known ygo right-origin failure case. Existing websocket sync tests should continue to pass without changing the frontend protocol.

## Progress

- [x] (2026-05-17) Confirmed current websocket/y-protocol framing is implemented in `internal/yproto/yproto.go`, not by ygo.
- [x] (2026-05-17) Confirmed `yffi` exposes the primitives Notty needs: document creation with client IDs, UTF-16 offset mode, update apply, state vector, state diff, YText operations, update observers, and sticky indexes.
- [x] (2026-05-17) Created this ExecPlan.
- [x] (2026-05-17) Added `third_party/y-crdt` as a git submodule pinned at `639db2038fa44d09f628a2650fd900e3b109ad1e`.
- [x] (2026-05-17) Added `scripts/build-yffi.sh` and confirmed host yffi builds with `cargo build -p yffi --release --locked`.
- [x] (2026-05-17) Implemented initial `internal/ycrdt` adapter with document creation, update apply, state vectors, state diffs, YText insert/delete/string, and sticky-index anchors.
- [x] (2026-05-17) Added adapter tests for roundtrip updates, state-vector diffs, delete updates, the known right-origin case, and UTF-16 offsets; `GOCACHE=/tmp/notty-gocache go test ./internal/ycrdt -count=1` passes.
- [x] (2026-05-17) Added a hardcoded Yjs-generated right-origin regression test using the bytes from `ygo-yjs-incompatibility.md`.
- [x] (2026-05-17) Refactored `internal/yproto` to be CRDT-engine agnostic and byte-oriented.
- [x] (2026-05-17) Replaced backend ygo call sites with `internal/ycrdt`.
- [x] (2026-05-17) Replaced daemon ygo call sites with `internal/ycrdt`.
- [x] (2026-05-17) Replaced thread-anchor encoding with yffi sticky-index binary anchors and verified those bytes decode as Yjs relative positions in Node/Yjs.
- [x] (2026-05-17) Removed `third_party/ygo` and the `go.mod` replacement for ygo.
- [x] (2026-05-17) Ran focused correctness tests, the full Go suite, frontend tests, regression compile, local Go builds, focused race tests, and Docker backend/daemon builds.

## Surprises & Discoveries

- Observation: `internal/yproto` already encodes the exact sync message tags used by Yjs and Yrs: sync step 1, sync step 2, and update. The CRDT engine is only responsible for the inner bytes.
  Evidence: `internal/yproto/yproto.go` encodes top-level `MessageSync = 0`, `MessageAwareness = 1`, and sync subtypes `0`, `1`, `2`; frontend `frontend/src/yProtocol.ts` uses `y-protocols/sync.js` with the same framing.
- Observation: `yffi` releases currently build only a small set of targets upstream, so Notty cannot depend on prebuilt upstream binaries for all daemon targets.
  Evidence: `/tmp/y-crdt/.github/workflows/release.yml` lists C-FFI targets for x86_64 Linux, x86_64 macOS, and x86_64 Windows, while Notty distributes daemon binaries for additional arm64 targets.
- Observation: `yffi` has `Y_OFFSET_UTF16`, which is important because the frontend's Yjs/editor offsets and thread anchors are based on UTF-16 positions, not byte positions.
  Evidence: `tests-ffi/include/libyrs.h` documents `Y_OFFSET_BYTES` and `Y_OFFSET_UTF16` in `YOptions`.
- Observation: The generated upstream yffi header currently has duplicate `YDoc` typedefs that cgo rejects.
  Evidence: `go test ./internal/ycrdt` initially failed with `type conversion loop at YDoc`. The adapter now uses `internal/ycrdt/yrs_min.h`, a minimal local header declaring only the yffi symbols Notty calls.
- Observation: Calling `ytext(doc, name)` while a write transaction is open can hang.
  Evidence: `TestYCRDTRoundTripUpdate` timed out while blocked in `_Cfunc_ytext`. The adapter now creates the root `YText` branch in `Doc.GetText` before write transactions are opened, and tests get the text handle before opening a write transaction.
- Observation: The yffi sticky-index binary format is accepted by JavaScript Yjs as a relative position.
  Evidence: `TestYCRDTStickyIndexIsYjsRelativePositionCompatible` creates a yffi anchor, decodes it with `Y.decodeRelativePosition`, and verifies it moves after a JS-side insert.
- Observation: A reentrant `YText.Len()` implementation would make the adapter harder to reason about.
  Evidence: The first adapter version used an active transaction shortcut so `Len()` could be called while `Doc.Update` held the document mutex. It was replaced with explicit `LenInTxn(txn)` call sites, and `go test -race ./internal/ycrdt ./internal/yproto` passes.
- Observation: yffi/cgo changes the daemon release build constraints.
  Evidence: `scripts/build-daemon-release.sh` now only builds the host platform and exits clearly for other platforms until a real Rust/cgo cross-compilation pipeline is added.
- Observation: Linking with `-lyrs` produces runtime dependencies on `libyrs.so` / `libyrs.dylib`.
  Evidence: Docker `ldd` initially failed with `libyrs.so: No such file or directory`. The adapter now links the yffi static archive directly. Docker runtime images still need `libgcc_s.so.1`, so backend and daemon runtime images explicitly install `libgcc`.

## Decision Log

- Decision: Preserve the current websocket/y-protocol wire format and replace only the CRDT engine.
  Rationale: The protocol framing is already standard Yjs-compatible framing and is not the source of the incompatibility. Changing it would add risk without solving the ygo bug.
  Date/Author: 2026-05-17 / Codex
- Decision: Build a Notty-specific `internal/ycrdt` API instead of a full ygo-compatible fork.
  Rationale: A narrow adapter is simpler, easier to test, and avoids preserving accidental ygo API surface. It also makes ownership clear: Notty owns only the operations it actually uses.
  Date/Author: 2026-05-17 / Codex
- Decision: Use update-v1 and state-vector-v1 compatibility as the first acceptance target.
  Rationale: The frontend and existing websocket sync already use Yjs update-v1/state-vector-v1 bytes. V2 can be considered later, but adding it now is unnecessary scope.
  Date/Author: 2026-05-17 / Codex
- Decision: Use a minimal local C header instead of including yffi's generated `libyrs.h` directly.
  Rationale: cgo rejects the generated duplicate typedefs, and a small header makes the binding surface explicit. This does not fork yffi behavior; it only declares the stable C symbols used by Notty.
  Date/Author: 2026-05-17 / Codex
- Decision: Do not use yffi update observers for the first adapter.
  Rationale: Local update bytes can be computed by taking a state vector before a transaction and encoding the state diff after it commits. That avoids cgo callback lifetime complexity and is enough for Notty's current local edit generation.
  Date/Author: 2026-05-17 / Codex
- Decision: Keep `internal/yproto` byte-only.
  Rationale: Y-protocol framing should not know which CRDT engine produced the inner update bytes. This keeps frontend, backend, daemon, and future CRDT-engine swaps from depending on protocol helpers with hidden materialization.
  Date/Author: 2026-05-17 / Codex
- Decision: Use explicit transaction-aware text length (`LenInTxn`) instead of implicit reentrant locking.
  Rationale: Go mutexes are not reentrant, and trying to fake that in a cgo wrapper creates race-prone behavior. The explicit helper makes in-transaction operations clear at call sites.
  Date/Author: 2026-05-17 / Codex
- Decision: Link `libyrs.a` directly instead of shipping `libyrs` as a separate shared library.
  Rationale: Notty's daemon distribution should stay a simple binary package. A static yffi archive avoids requiring install scripts and Docker images to manage `LD_LIBRARY_PATH` or platform-specific dylib locations.
  Date/Author: 2026-05-17 / Codex

## Outcomes & Retrospective

The ygo replacement is implemented. Backend and daemon code now import `notty/internal/ycrdt`, while `internal/yproto` remains a raw y-protocol framing package. The old vendored `third_party/ygo` dependency has been removed.

Validation run on 2026-05-17:

    GOCACHE=/tmp/notty-gocache go test ./... -count=1 -timeout 180s
    GOCACHE=/tmp/notty-gocache go test -race ./internal/ycrdt ./internal/yproto -count=1 -timeout 120s
    GOCACHE=/tmp/notty-gocache make build-go
    GOCACHE=/tmp/notty-gocache make build-daemon
    npm test -- --run
    GOCACHE=/tmp/notty-gocache go test -tags regression ./test/regression -run TestDoesNotExist -count=0
    docker compose build backend daemon
    docker compose run --rm --entrypoint ldd backend /usr/local/bin/notty-backend
    docker compose run --rm --entrypoint ldd daemon /usr/local/bin/notty-daemon

The main remaining operational caveat is release packaging: yffi requires Rust/cgo, so the current daemon release script intentionally supports only the host platform. Multi-platform daemon release builds need a separate cross-compilation solution instead of pretending the old pure-Go cross build still applies.

## Context and Orientation

Yjs is a JavaScript CRDT library. A CRDT is a data structure that lets multiple peers edit independently and later converge to the same state. Notty's frontend uses the real Yjs library and `y-protocols/sync.js`. The backend and daemon currently use a Go library named ygo. The term "wire compatible" means that the bytes sent over websockets remain valid Yjs protocol messages and valid Yjs CRDT updates, regardless of which implementation produced them.

The current layers are:

- `frontend/src/yProtocol.ts` wraps Yjs sync and awareness messages for the browser.
- `internal/yproto/yproto.go` wraps and unwraps the websocket binary message framing on the Go side.
- `backend/internal/notty/server_documents.go` handles document websocket messages and persists incoming CRDT updates.
- `backend/internal/notty/store_postgres.go` reconstructs document state for checkpoints, diffs, and sync replies.
- `daemon/internal/syncer/service.go` applies remote document updates and generates outgoing updates from local file changes.
- `daemon/internal/syncer/document_cache.go` stores per-document CRDT cache files for daemon reconciliation.
- `daemon/internal/syncer/thread_anchor.go` creates CRDT-relative thread anchors.
- `third_party/ygo` is the current vendored Go CRDT engine and should be removed at the end.

The replacement library is `y-crdt/y-crdt`. Its Rust crate `yrs` is the core implementation. Its `yffi` crate exposes a C ABI, which Go can call through cgo. The relevant yffi functions include `ydoc_new_with_options`, `ytransaction_apply`, `ytransaction_state_vector_v1`, `ytransaction_state_diff_v1`, `ytext_insert`, `ytext_remove_range`, `ytext_string`, and `ysticky_index_from_index`.

## Plan of Work

First, add `y-crdt` in a reproducible location. Prefer a git submodule under `third_party/y-crdt` if network access is available; otherwise copy or vendor the exact checked-out source from `/tmp/y-crdt` and record the commit. Add a build script that compiles `yffi` into a static library for the current host target. Keep the first iteration local and test-focused.

Second, create `internal/ycrdt`. This package should be the only Go package that imports C symbols from yffi. Its API should be small and Notty-specific. It should expose document creation, cleanup, state-vector-v1 encoding, update-v1 diff encoding, update-v1 application, YText string/length/insert/delete operations, and relative anchor operations. It should set `YOptions.encoding = Y_OFFSET_UTF16` so text indexes match Yjs.

Third, refactor `internal/yproto/yproto.go` so it no longer imports ygo. Keep the existing raw-byte functions such as `BuildSyncStep1FromStateVector`, `BuildSyncStep2FromUpdate`, `BuildSyncUpdate`, `DecodeProtocolMessage`, and `DecodeSyncMessage`. Move any doc-aware helpers to call `internal/ycrdt` or remove them from production code. Tests should assert the wire bytes still match `y-protocols/sync.js` behavior.

Fourth, replace backend call sites. Backend code should use `internal/ycrdt` only in places where materialization is unavoidable: creating initial document updates, creating checkpoints, computing sync replies from checkpoints and tails, and diff support. The backend should remain Postgres-first and should not reintroduce long-lived in-memory document state.

Fifth, replace daemon call sites. The daemon should use `internal/ycrdt` for temporary documents during reconciliation, cache load/store, remote update application, local plaintext edit update generation, and state-vector tracking. Keep the existing websocket and cache model unless tests expose a correctness issue.

Sixth, replace thread anchor encoding. The safest first version is to store yffi sticky indexes using a Yjs-compatible RelativePosition JSON representation if frontend/backend compatibility requires it. If binary sticky-index encoding is proven byte-compatible with the frontend's Yjs relative-position encoding, binary anchors can remain. This decision must be proven by tests before finalizing.

Finally, delete `third_party/ygo`, remove the `go.mod` replace directive, and run the full focused and broad test suites.

## Concrete Steps

Work from repository root `/Users/zhongyangxia/Downloads/notty`.

Run:

    git status --short

Expect no unrelated working-tree changes before beginning. If unrelated user changes appear, do not overwrite them.

Add or vendor y-crdt, then build yffi for the host. The exact command will be updated after the build path is created. The intended shape is:

    ./scripts/build-yffi.sh

Add adapter tests and run:

    go test ./internal/ycrdt

After the backend and daemon migrations, run:

    go test ./internal/yproto ./backend/internal/notty ./daemon/internal/syncer

At the end, run:

    go test ./...

If frontend protocol fixtures are added, also run:

    cd frontend && npm test

## Validation and Acceptance

The change is accepted only if the following behaviors are demonstrated by tests:

1. A Yjs-generated update that currently triggers the ygo right-origin bug converges to the same text under `internal/ycrdt` as it does under Yjs.
2. A Go/yffi-generated insert update applies correctly in a browser/Yjs document.
3. A browser/Yjs-generated delete or backspace update applies correctly in Go/yffi.
4. State-vector sync replies generated by Go/yffi are accepted by the existing frontend Yjs protocol code.
5. UTF-16 positions behave correctly for a string containing non-BMP characters, such as emoji.
6. Thread anchors created from a selected range move correctly after text is inserted before the range.
7. Existing backend websocket sync tests and daemon reconciliation tests pass without changing the websocket protocol.

## Idempotence and Recovery

The adapter work is additive until the final deletion of ygo. If the yffi build fails or compatibility tests fail, keep ygo in place and stop before migrating production paths. The initial adapter package can be removed without affecting existing behavior. Do not delete `third_party/ygo` until all production code imports have moved.

If a cgo build leaves generated artifacts, keep them in a clearly named path such as `.vendor/yffi` or `third_party/y-crdt/target`; do not place generated binaries in source directories unless the repository already tracks that pattern.

## Artifacts and Notes

The known ygo incompatibility is documented in `ygo-yjs-incompatibility.md`. The replacement must include a regression test equivalent to that minimal reproduction.

Current wire framing in Go:

    MessageSync = 0
    MessageAwareness = 1
    SyncStep1 = 0
    SyncStep2 = 1
    SyncUpdate = 2

These values must not change.

## Interfaces and Dependencies

The final internal adapter should be approximately:

    package ycrdt

    type Doc struct
    type Text struct
    type Transaction struct

    func New(clientID uint64) (*Doc, error)
    func (d *Doc) Close()
    func (d *Doc) ClientID() uint64
    func (d *Doc) Text(name string) *Text
    func (d *Doc) ApplyUpdateV1(update []byte) error
    func (d *Doc) StateVectorV1() ([]byte, error)
    func (d *Doc) EncodeStateAsUpdateV1(remoteStateVector []byte) ([]byte, error)
    func (d *Doc) Read(fn func(*Transaction) error) error
    func (d *Doc) Update(fn func(*Transaction) error) ([]byte, error)
    func (t *Text) String(txn *Transaction) (string, error)
    func (t *Text) Len(txn *Transaction) (int, error)
    func (t *Text) Insert(txn *Transaction, index int, value string) error
    func (t *Text) Delete(txn *Transaction, index int, length int) error
    func (t *Text) RelativeAnchorFromIndex(txn *Transaction, index int, assoc int) ([]byte, error)
    func (t *Text) IndexFromRelativeAnchor(txn *Transaction, anchor []byte) (int, bool, error)

The package may adjust names during implementation, but the surface should remain this small. The adapter owns all cgo memory management. No backend, daemon, or protocol package should call yffi symbols directly.

Revision note, 2026-05-17: Created the plan after confirming the current wire protocol is already owned by Notty and yffi has sufficient API coverage. The plan intentionally preserves websocket behavior and narrows the replacement to the CRDT engine.
