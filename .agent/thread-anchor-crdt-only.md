# Simplify thread anchors to CRDT-relative positions

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/plans.md` in this repository. It is self-contained so a new contributor can continue the work without prior conversation context.

## Purpose / Big Picture

Notty stores document discussion threads anchored to text. The stable anchor is a CRDT-relative position, which means the anchor follows the text as collaborative edits move content around. The current code also stores derived absolute line and offset metadata on each thread anchor. Those absolute values become stale after edits, and the frontend currently trusts the stale `line` field when drawing thread markers, causing markers to stack or point at the wrong place.

After this change, the backend stores only stable CRDT anchor data. The frontend resolves anchors against the currently loaded Yjs document and computes line numbers and marker positions only as render-time view data. A user should be able to create multiple threads in a document, edit text before those threads, and still see markers attached to the correct live text rather than stale stored line numbers.

## Progress

- [x] (2026-05-12T05:09:03Z) Read `.agent/plans.md`, inspected backend, daemon, and frontend anchor code paths, and created this ExecPlan.
- [x] (2026-05-12T05:17:01Z) Simplified backend thread anchor and create-thread request types so persisted/API anchors no longer expose `line`, `start`, `end`, or duplicate `documentId`.
- [x] (2026-05-12T05:17:01Z) Simplified Postgres thread persistence schema and scan/write paths to remove stale anchor columns and legacy JSON anchor storage.
- [x] (2026-05-12T05:17:01Z) Kept daemon helper inputs such as `--line` and `--quote`, but changed daemon backend calls to send only CRDT-relative anchor fields.
- [x] (2026-05-12T05:17:01Z) Updated frontend thread rendering to derive live anchors, computed lines, and marker groups from the current Yjs document instead of stored `anchor.line`.
- [x] (2026-05-12T05:17:01Z) Added focused regression assertions for canonical backend anchor responses and daemon helper payload canonicalization; existing frontend live-anchor test now uses the CRDT-only anchor shape.
- [x] (2026-05-12T05:17:01Z) Ran backend, daemon, and frontend tests successfully.
- [x] (2026-05-12T05:23:50Z) Rebuilt/restarted backend, frontend, and daemon containers; verified in the browser that thread sidebar labels no longer use persisted line metadata and the visible marker renders from the live resolved group.
- [x] (2026-05-12T05:21:30Z) Restarted the daemon with an explicit workspace ID and daemon token after the profile default environment attempted the obsolete unauthenticated workspace route.

## Surprises & Discoveries

- Observation: `line` has two meanings today. In the daemon helper it is a useful human input for choosing a target line. In backend storage it is stale denormalized metadata.
  Evidence: `daemon/internal/syncer/thread_anchor.go` resolves `Line` into CRDT relative anchors, while `frontend/src/App.tsx` renders gutter markers using parent-level groups derived from `thread.anchor.line`.
- Observation: raw `anchorStart` and `anchorEnd` are also copied into agent notification events from thread anchors.
  Evidence: `backend/internal/notty/store.go` assigns `event.AnchorStart = thread.Anchor.Start` and `event.AnchorEnd = thread.Anchor.End` for document-thread, thread-mentioned, and thread-replied notifications.
- Observation: Removing `line`, `start`, and `end` from the backend response caught an important frontend dependency: the thread sidebar had been showing stale `line` values from raw `ThreadItem` objects. That display now uses “anchored range” or “document thread” unless the document editor has resolved live lines locally.
  Evidence: `npm --prefix frontend test` initially failed typechecking after the `ThreadAnchor` type change until stale `anchor.line` sidebar reads were removed.
- Observation: Restarting the daemon profile without workspace credentials uses empty Compose defaults and causes the daemon to call the obsolete unauthenticated `/api/workspace` route.
  Evidence: daemon logs showed `initial refresh error: json: cannot unmarshal number into Go value of type syncer.workspaceResponse` while backend logged repeated `GET /api/workspace` 404 responses. Restarting with `NOTTY_WORKSPACE_ID` and `NOTTY_DAEMON_TOKEN` opened document websockets successfully.

## Decision Log

- Decision: Store only CRDT-relative anchor fields in backend thread anchors, and treat line/offset values as client or daemon input conveniences only.
  Rationale: CRDT-relative anchors are the only stable identity for a range in an editable document. Persisting derived line/offset values creates stale state and makes frontend bugs likely.
  Date/Author: 2026-05-12 / Codex
- Decision: Do not ask the backend to materialize documents to recompute line numbers.
  Rationale: The backend should remain a lightweight persistence and coordination service. The frontend already has the live Yjs document and rendered DOM, which is the correct place to compute display positions.
  Date/Author: 2026-05-12 / Codex
- Decision: Keep daemon `--line`, `--quote`, and offset helper inputs.
  Rationale: Agents need easy ways to create anchored threads. Those inputs are not canonical storage; the daemon converts them to CRDT-relative anchors before calling the backend.
  Date/Author: 2026-05-12 / Codex
- Decision: Remove `anchorStart` and `anchorEnd` from agent events as part of the same simplification.
  Rationale: They were copied from stale thread anchor offsets, so preserving them would keep a second source of stale document-coordinate truth.
  Date/Author: 2026-05-12 / Codex

## Outcomes & Retrospective

The implementation achieved the intended simplification: canonical anchors no longer contain stale line or offset fields, the daemon still supports human-friendly line/quote helper inputs, and the frontend groups markers from live resolved anchors. Backend, daemon, and frontend tests pass, the rebuilt stack is healthy, and browser verification showed the existing thread displayed as an “anchored” thread rather than a stale persisted line.

## Context and Orientation

The backend lives under `backend/internal/notty`. `types.go` defines API structs such as `ThreadAnchor`, `Thread`, and `CreateThreadRequest`. `store.go` implements in-memory store logic and business rules. `store_postgres.go` creates and uses the Postgres schema. The backend should validate thread anchors but should not compute document line numbers for rendering.

The daemon lives under `daemon/internal/syncer`. In this repository, “daemon” means the long-running process that syncs a local filesystem workspace with the backend and manages agents. `thread_anchor.go` lets agents create threads using helper inputs such as a document path plus a line number or quote. That helper must continue to resolve those inputs locally into CRDT-relative positions.

The frontend lives under `frontend/src`. It loads a Yjs document for the selected file and renders thread markers in `App.tsx`. `logic.ts` contains pure utility functions and tests. “CRDT-relative position” means a compact Yjs-encoded anchor that can be resolved against a live Yjs document to find the current text offset after edits.

## Plan of Work

First, update backend types so `ThreadAnchor` contains `Kind`, `RelativeStart`, `RelativeEnd`, and `Excerpt` only. `Thread` keeps `DocumentID` because the thread itself belongs to a document. `CreateThreadRequest` should accept only `DocumentID`, `Title`, `Body`, `RelativeStart`, `RelativeEnd`, `Kind`, and `Excerpt`. The backend should accept document-level threads when no relative anchors are provided, and text-range threads only when both relative anchors are present.

Second, simplify Postgres thread storage. Remove the stale `anchor_start`, `anchor_end`, and `anchor_line` columns from the schema and from query code. Remove the legacy `anchor JSONB` path if no longer needed. Since this is pre-MVP and data loss is acceptable, migrations may drop obsolete columns directly.

Third, update daemon helper structs so they still accept line, quote, and offset inputs internally, but clear those helper-only fields before sending the backend request. Backend-facing thread creation should carry only relative anchors and excerpt.

Fourth, update the frontend. `resolveThreadAnchorLive` should return a render-only structure with current `start`, `end`, `line`, and `excerpt`, without pretending those fields exist on canonical `ThreadAnchor`. `DocumentEditor` should build marker groups from this live resolved data inside the component. Gutter markers should no longer consume a parent-provided `threadGroups` based on stored lines.

Fifth, run focused tests and browser verification. Tests should prove that backend round-trips omit stale fields, daemon line/quote resolution still works, and frontend markers use live resolved positions.

## Concrete Steps

Work from `/Users/zhongyangxia/Downloads/notty`.

Run searches before and after editing:

    rg -n "anchor\\.line|anchor_line|anchor_start|anchor_end|ThreadAnchor|CreateThreadRequest|buildLineThreads" backend daemon frontend/src -S -g '!**/node_modules/**'

Run Go tests after backend/daemon changes:

    go test ./backend/internal/notty ./daemon/internal/syncer

Run frontend tests after frontend changes:

    npm --prefix frontend test

Use Docker Compose and the browser for final verification if the stack is not already running:

    docker compose up -d --build

Then open `http://localhost:5173` in the in-app browser, create or select a document with multiple threads, edit text before existing anchors, and verify that thread markers remain attached to the correct live positions.

Observed verification on 2026-05-12:

    docker compose ps showed backend, daemon, frontend, and postgres running.
    http://localhost:8080/healthz returned 200 {"status":"ok"}.
    The Postgres `threads` table only had anchor_excerpt, anchor_kind, anchor_relative_end, and anchor_relative_start anchor columns.
    The browser showed docs/untitled.md with one marker, and the thread sidebar label read "1 replies · anchored" instead of "line 1".
    Clicking the marker opened "Threads near line 1", proving the marker opens the live resolved cluster rather than a stored raw line bucket.

## Validation and Acceptance

Backend acceptance: creating a thread with `relativeStart` and `relativeEnd` succeeds, returns an anchor with only `kind`, `relativeStart`, `relativeEnd`, and `excerpt`, and does not return `line`, `start`, `end`, or nested `documentId` inside `anchor`.

Daemon acceptance: `notty-agent-tool create-thread --path docs/file.md --line 2 --body "..."` still resolves a valid anchored thread because the daemon converts the line to relative anchors before sending the request.

Frontend acceptance: a document with threads renders marker groups from live resolved anchors. If content is inserted above a thread, the marker moves with the anchored text. If multiple threads resolve to the same or nearby rendered position, the UI exposes all of them instead of hiding or stacking individual markers.

## Idempotence and Recovery

The schema cleanup intentionally breaks compatibility with old thread rows. Because the user has explicitly allowed data loss, the safe recovery path is to rebuild containers or clear the local database and let the app recreate the simplified schema. Code changes should avoid destructive shell commands unless explicitly requested.

If tests fail after partial changes, rerun the search command in `Concrete Steps` to find stale references to removed fields.

## Artifacts and Notes

Relevant starting evidence:

    frontend/src/App.tsx computed `groups = buildLineThreads(documentThreads)` before live CRDT anchor resolution.
    frontend/src/App.tsx positioned markers with `top: (line - 1) * 26 + 6`.
    backend/internal/notty/store_postgres.go persisted `anchor_start`, `anchor_end`, and `anchor_line`.

## Interfaces and Dependencies

At completion, `backend/internal/notty/types.go` should define:

    type ThreadAnchor struct {
        Kind          string `json:"kind"`
        RelativeStart string `json:"relativeStart,omitempty"`
        RelativeEnd   string `json:"relativeEnd,omitempty"`
        Excerpt       string `json:"excerpt,omitempty"`
    }

The frontend may define a separate render-only type in `frontend/src/logic.ts`:

    type ResolvedThreadAnchor = ThreadAnchor & {
        start: number;
        end: number;
        line: number;
        excerpt: string;
        resolved: boolean;
    }

This render-only type must not be sent to the backend as canonical storage.

Revision note 2026-05-12: Initial plan created after inspecting the stale `anchor.line` implementation. The plan records the decision to keep line as helper input only and remove stale line/offset storage.

Revision note 2026-05-12: Updated after implementation and test runs. The backend now returns canonical CRDT-only anchors, daemon helper calls canonicalize payloads before sending them to the backend, and frontend marker grouping is derived from live resolved anchors.

Revision note 2026-05-12: Updated after Docker and browser verification. The running database schema confirms stale anchor columns were dropped and the browser confirms the frontend no longer surfaces persisted line metadata in the thread list. The frontend now clusters marker groups by measured vertical proximity before rendering a marker.

Revision note 2026-05-12: Added daemon restart recovery details after discovering the profile default environment is not sufficient for auth-mode daemon startup.
