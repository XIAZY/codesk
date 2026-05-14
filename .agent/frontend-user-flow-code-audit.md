# Audit Frontend User Flows for Minimal Correct Design

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. It is an analysis plan only. It does not authorize product code changes; the requested deliverable is a markdown findings report.

## Purpose / Big Picture

The frontend has been rewritten around authenticated workspaces, documents, threads, daemons, and agents. The user wants a careful fact-based audit before further code changes because previous work exposed correctness and performance problems in CRDT synchronization, daemon behavior, backend materialization, and high-churn file handling. After this audit, a maintainer should be able to read one markdown report and understand which user flows are currently clean, which are over-complex, which edge cases are tested, and which dead-code or risky paths should be removed or redesigned later.

## Progress

- [x] (2026-05-11T07:37:38Z) Read `AGENTS.md` and `.agent/PLANS.md`.
- [x] (2026-05-11T07:37:38Z) Created this analysis-only ExecPlan.
- [x] (2026-05-11T07:45:04Z) Inspected frontend source files for each user flow.
- [x] (2026-05-11T07:45:04Z) Inspected relevant backend contracts, daemon sync code, and existing tests for each flow.
- [x] (2026-05-11T07:48:42Z) Ran validation commands to ground findings in current behavior.
- [x] (2026-05-11T07:50:00Z) Wrote `frontend-user-flow-audit.md` with evidence, severity, and recommendations.
- [x] (2026-05-11T07:50:00Z) Recorded validation evidence and limitations.

## Surprises & Discoveries

- Observation: The default regression tier passes, but backend restart append safety is still documented as opt-in because it reproduces a lost-write/reconnect diagnostic.
  Evidence: `go test -tags=regression ./test/regression` passed in 122.043s, while `test/regression/README.md` documents `NOTTY_REGRESSION_BACKEND_RESTART=1` as opt-in.
- Observation: The Postgres CRDT apply path still returns `Applied: true` for every inbound update, while the in-memory path compares CRDT snapshots and suppresses no-op duplicates.
  Evidence: `backend/internal/notty/store.go` in `ApplyCRDTUpdateWithResult`; duplicate suppression tests exist for the in-memory/server path but not for Postgres.
- Observation: Workspace websocket snapshots and HTTP workspace snapshots do not have the same shape.
  Evidence: `handleWorkspace` includes identity fields such as `workspaceId`, `currentUserId`, and `name`, while `handleWebsocket` sends arrays/maps only.
- Observation: Several documented frontend flows are not implemented in the current UI.
  Evidence: member management, document deletion, document-level thread creation, REST presence publication, and workspace activity rendering have backend/API support but no complete React flow.

## Decision Log

- Decision: Do not change product code during this audit.
  Rationale: The user explicitly requested fact-based analysis and a markdown findings file, not implementation.
  Date/Author: 2026-05-11 / Codex.
- Decision: Audit by user flow rather than by file.
  Rationale: The product failures discussed earlier were user-visible correctness failures across frontend, backend, and daemon boundaries; a file-only review would hide cross-boundary risks.
  Date/Author: 2026-05-11 / Codex.

## Outcomes & Retrospective

The audit produced `frontend-user-flow-audit.md`. The current implementation is strongest in backend/daemon correctness tests and weakest in frontend component/hook lifecycle coverage. The most important correctness risk found is not frontend visual code: it is the Postgres CRDT update apply path treating every inbound update as applied. The report recommends fixing that before further performance work, then adding targeted browser-side sync lifecycle tests and simplifying incomplete/dead UI/API surfaces.

## Context and Orientation

The frontend lives in `frontend/src/`. The main user interface is in `frontend/src/App.tsx`. HTTP requests are wrapped by `frontend/src/api.ts`. Workspace metadata streaming is handled by `frontend/src/useWorkspace.ts`. Active document CRDT synchronization is handled by `frontend/src/useDocument.ts` and `frontend/src/yProtocol.ts`. Pure helpers and their tests live in `frontend/src/logic.ts`, `frontend/src/logic.test.ts`, and `frontend/src/yProtocol.test.ts`.

The backend contracts used by the frontend are in `backend/internal/notty/server_auth.go`, `backend/internal/notty/server_workspace.go`, `backend/internal/notty/server_documents.go`, `backend/internal/notty/server_threads.go`, `backend/internal/notty/server_agents.go`, and `backend/internal/notty/server_collaboration.go`. Backend persistence and correctness tests live in `backend/internal/notty/store*.go`, `backend/internal/notty/server*_test.go`, and `test/regression/`.

This audit uses “first-principle design” to mean that a flow keeps one clear source of truth, avoids duplicate state and hidden compatibility layers, does not special-case product entities unnecessarily, keeps heavy CRDT/materialization work off hot paths unless required for correctness, and tests fragile correctness boundaries rather than trivial getters and setters.

## Plan of Work

First inspect the React user flows in `frontend/src/App.tsx`: authentication and workspace onboarding, workspace shell navigation, document editor and CRDT sync, thread creation/reply/detail, daemon creation/liveness/deletion, agent creation/status/detail, and activity/people side panels. For each flow, identify its source of truth, what local state it duplicates, what backend contracts it relies on, and what can fail.

Then inspect `frontend/src/api.ts`, `frontend/src/useWorkspace.ts`, `frontend/src/useDocument.ts`, and `frontend/src/yProtocol.ts` to assess whether synchronization and API wiring are minimal and consistent with the earlier goal of avoiding full-workspace/document materialization. Inspect existing tests to determine whether edge cases are covered.

Finally, write `frontend-user-flow-audit.md` at the repository root. The report should separate facts from recommendations. It should name files and functions, describe dead-code candidates, and avoid implementation patches.

## Concrete Steps

Run these commands from `/Users/zhongyangxia/Downloads/notty`:

    sed -n '1,260p' frontend/src/App.tsx
    sed -n '1,240p' frontend/src/api.ts
    sed -n '1,260p' frontend/src/useWorkspace.ts
    sed -n '1,320p' frontend/src/useDocument.ts
    sed -n '1,320p' frontend/src/yProtocol.ts
    rg -n "TODO|FIXME|legacy|snapshot|content|crdtState|proposal|mention|app_logic|diff" frontend backend daemon test
    cd frontend && npm test
    cd frontend && npm run build
    go test ./backend/internal/notty

If a command is blocked by sandbox policy, record that as a limitation rather than changing code.

## Validation and Acceptance

The audit is complete when `frontend-user-flow-audit.md` exists and covers every major user flow. The file must include concrete evidence from source files and tests, not broad opinions. The ExecPlan must also be updated with the final result and any validation commands that were run.

## Idempotence and Recovery

This plan only reads source and writes markdown documents. It can be rerun safely. If a validation command fails because of the current worktree, record the failure with the exact command and do not repair source code unless the user separately asks for implementation.

## Artifacts and Notes

The primary artifact is `frontend-user-flow-audit.md`.

Validation transcript summary:

    cd frontend && npm test
    Result: passed, 2 test files, 9 tests.

    cd frontend && npm run build
    Result: passed, Vite production build completed.

    go test ./backend/internal/notty
    Result: passed.

    go test ./daemon/internal/syncer
    Result: passed.

    go test -tags=regression ./test/regression
    Result: passed in 122.043s.

## Interfaces and Dependencies

No new runtime interfaces or dependencies are introduced by this audit. The relevant existing interfaces are the frontend `ApiClient` class in `frontend/src/api.ts`, the workspace websocket in `frontend/src/useWorkspace.ts`, the document websocket in `frontend/src/useDocument.ts`, and backend workspace-scoped HTTP routes under `/api/workspaces/{workspaceId}`.
