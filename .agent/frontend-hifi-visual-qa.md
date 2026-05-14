# Frontend Hi-Fi Visual QA and Interaction Pass

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md` in this repository. It is intentionally self-contained so another contributor can continue the frontend QA work without relying on prior conversation.

## Purpose / Big Picture

The product has a rewritten React frontend that should match the static hi-fi sketches in `hi-fi/`. The user needs the live app to feel like the designed product, not just expose the right backend features. After this work, a human can open the local frontend, register or log in, choose a workspace, read and edit documents, inspect threads, manage daemons, and create agents with UI structure and visual hierarchy aligned to the five sketches.

## Progress

- [x] (2026-05-11T06:57:40Z) Read `AGENTS.md` and `.agent/PLANS.md`; confirmed significant frontend iteration should be tracked with an ExecPlan.
- [x] (2026-05-11T06:57:40Z) Created this visual QA plan before further edits.
- [x] (2026-05-11T07:18:24Z) Compared live authentication and workspace selection against `hi-fi/01-auth.html`; registration and workspace picker visually match the intended compact card flow.
- [x] (2026-05-11T07:18:24Z) Compared live workspace shell and document reading/editing against `hi-fi/02-shell.html`; shell structure, left navigation, document canvas, and right context match the three-pane intent.
- [x] (2026-05-11T07:18:24Z) Compared thread list, thread detail, and thread creation against `hi-fi/03-threads.html`; right-pane list/detail and anchored thread creation work from real selection.
- [x] (2026-05-11T07:18:24Z) Compared daemon management and token reveal flow against `hi-fi/04-daemons.html`; list, metrics, token reveal, and daemon detail are functional.
- [x] (2026-05-11T07:18:24Z) Compared agent creation and roster management against `hi-fi/05-agents.html`; global New agent flow works and roster groups agents by daemon.
- [x] (2026-05-11T07:18:24Z) Patched concrete mismatches discovered during browser use.
- [x] (2026-05-11T07:18:24Z) Ran frontend tests and build.
- [x] (2026-05-11T07:18:24Z) Recorded final browser verification results.

## Surprises & Discoveries

- Observation: Creating a new account and accepting the default workspace name failed if another workspace already used the slug `product-workspace`.
  Evidence: Browser submission returned `duplicate key value violates unique constraint "workspaces_slug_key"` from `POST /api/workspaces`.
- Observation: Modal close buttons were visually placed under the title instead of top-right.
  Evidence: Browser screenshots of New document, New daemon, and New agent modals showed `×` on a second line until `.modal-header` was made a flex row.
- Observation: Selecting all text in a one-line document was labeled as `Selection across lines 1-2`.
  Evidence: Browser selection of `# Untitled` produced the wrong label; `selectionLabel` now uses `end - 1` for non-empty ranges and has a regression test.
- Observation: Agent creation was disabled for disconnected daemons, blocking the intended flow where users can define agents before deploying the daemon.
  Evidence: The New agent modal showed the daemon radio disabled and Create agent disabled after creating a daemon token.
- Observation: Agents owned by disconnected daemons were shown as `idle` in several UI surfaces.
  Evidence: The agent roster and daemon detail displayed `@codex-agent · idle` even though the owning daemon was disconnected; the UI now derives visible agent status from daemon liveness.
- Observation: Browser screenshot capture became unreliable after container rebuilds, but DOM snapshots and clicks continued to work after reconnecting the browser session.
  Evidence: final verification used fresh DOM snapshots after reconnecting the Browser session.

## Decision Log

- Decision: Treat the static files under `hi-fi/` as visual and interaction references, but preserve product requirements that are now implemented in the backend, such as daemon-scoped agent creation and authenticated workspace membership.
  Rationale: The sketches describe intended shape and hierarchy, while the live app must remain wired to the current product model.
  Date/Author: 2026-05-11 / Codex.
- Decision: Allocate unique workspace slugs on the backend when users reuse ordinary workspace names.
  Rationale: The frontend does not ask for a globally unique URL slug. A hidden global uniqueness requirement makes first-run onboarding fail for normal users.
  Date/Author: 2026-05-11 / Codex.
- Decision: Let users create agents under disconnected daemons.
  Rationale: A daemon token may be created before the local daemon checks in. Agent definitions are workspace configuration and should be allowed before runtime availability.
  Date/Author: 2026-05-11 / Codex.
- Decision: Display agents as disconnected when their owning daemon is disconnected.
  Rationale: The agent cannot run if its daemon is offline; showing `idle` misleads users about operational state.
  Date/Author: 2026-05-11 / Codex.

## Outcomes & Retrospective

The live frontend now supports the main product flows against the rebuilt stack: register, create workspace, create document, open/edit document, create a thread from selected text, open thread detail, create a daemon and view the one-time token, create an agent under that daemon, and inspect daemon/agent liveness. The largest remaining fidelity gap is that the live document editor uses a plain textarea while editing, then a styled markdown preview while reading; this is acceptable for now because the core flow and synchronization behavior remain clear.

## Context and Orientation

The frontend lives in `frontend/src/`. `frontend/src/App.tsx` contains the main React application, including authentication, workspace selection, the document shell, thread panels, daemon management, and agent management. `frontend/src/styles.css` contains the visual system. `frontend/src/api.ts` wraps backend HTTP calls. `frontend/src/useWorkspace.ts` loads workspace data, and `frontend/src/useDocument.ts` handles live document synchronization.

The hi-fi references are ordinary HTML files in `hi-fi/`: `01-auth.html`, `02-shell.html`, `03-threads.html`, `04-daemons.html`, and `05-agents.html`. They are not app code; they are static design targets. The live frontend is normally served at `http://localhost:5173` by Docker Compose.

## Plan of Work

First, inspect the current app and the sketches in a browser. For each flow, use the UI as a real user would: sign in or register, create or select a workspace, navigate documents, open thread detail, create a daemon token, and open the agent creation flow. For every mismatch that affects user comprehension or visual hierarchy, patch `frontend/src/App.tsx` or `frontend/src/styles.css`. Avoid broad rewrites unless the mismatch is structural.

After each patch, run `npm test` and `npm run build` from `frontend/` when the change touches logic or TypeScript. Then rebuild or restart only the frontend container if needed and verify the relevant screen again in the browser.

## Concrete Steps

Run commands from `/Users/zhongyangxia/Downloads/notty` unless noted otherwise.

Start or refresh the frontend if needed:

    docker compose up -d --build frontend

Run frontend validation:

    cd frontend
    npm test
    npm run build

Use the browser to visit:

    http://localhost:5173

Optionally serve static sketches for side-by-side comparison:

    cd hi-fi
    python3 -m http.server 4174

Then open:

    http://localhost:4174/01-auth.html
    http://localhost:4174/02-shell.html
    http://localhost:4174/03-threads.html
    http://localhost:4174/04-daemons.html
    http://localhost:4174/05-agents.html

## Validation and Acceptance

The work is accepted when the live app can be clicked through in a browser and the primary screens visually align with the matching hi-fi references. Authentication should show a centered editorial card. The workspace shell should show a compact left rail, a large document canvas, and a contextual right pane. Threads should support a readable list and a focused detail view. Daemons should show operational cards, a daemon table, and a one-time token reveal. Agents should be created by selecting a daemon and should appear grouped by daemon.

Tests should pass with `npm test`, and production build should pass with `npm run build`.

## Idempotence and Recovery

The browser verification may create local test users, workspaces, daemons, documents, or agents in the development database. This is acceptable for this pre-MVP local stack. If the UI gets into an unusable account state, use Sign out and register a new local account. If the frontend server serves stale assets, rebuild only the frontend container with `docker compose up -d --build frontend`.

## Artifacts and Notes

Evidence will be added as browser findings and test output are produced.

Validation run on 2026-05-11:

    cd frontend && npm test
    Test Files  2 passed (2)
    Tests  9 passed (9)

    cd frontend && npm run build
    vite build completed successfully

    go test ./backend/internal/notty
    ok  	notty/backend/internal/notty	0.559s

    docker compose up -d --build frontend
    frontend and backend containers rebuilt and restarted successfully

Browser verification on `http://localhost:5173`:

    Registered a new local account.
    Created a workspace named Product Workspace after backend slug allocation fix.
    Created docs/untitled.md.
    Selected the one-line document text and saw "Selection on line 1".
    Created a thread and opened its right-pane detail view.
    Created a daemon and saw the one-time daemon token reveal.
    Created @codex-agent under a disconnected daemon.
    Verified agent roster and daemon detail both show @codex-agent as disconnected.

## Interfaces and Dependencies

The live app remains a React application built by Vite. User actions call the existing `ApiClient` methods in `frontend/src/api.ts`. No backend API redesign is part of this plan. Any new UI behavior should reuse existing backend contracts unless browser testing proves a frontend-only bug.
