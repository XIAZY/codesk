# Prove the desktop loopback token handoff before building the desktop app

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. Maintain this document in accordance with `.agent/PLANS.md`.

## Purpose / Big Picture

Codesk's future desktop app needs to receive the existing static daemon bearer token after a human signs in and chooses a workspace in the normal web application. Before the tray controller, secret stores, or installers are built, this spike proves that a secure web page can hand that token to a one-shot HTTP listener bound only to the same computer. A successful run opens a Codesk browser harness, creates a daemon through the existing API, navigates once to a random `http://127.0.0.1:<port>/desktop/connect/<nonce>` URL with an `application/x-www-form-urlencoded` POST, and shows a local success page. The token appears only in the POST body and in the receiver's in-memory result; it never appears in a URL, browser history, referrer, console output, command-line argument, or log.

This is a blocking feasibility gate, not the finished `/desktop/connect` product route. The final product route, credential persistence, and daemon startup ordering belong to later desktop tasks. The reusable transport code and its conformance harness land now so Windows Edge, Windows Chrome, and macOS Safari can all test exactly the same commit.

## Progress

- [x] (2026-07-14 22:05Z) Read the approved task contract, current frontend daemon-create path, routing/auth flow, and repository execution-plan rules.
- [x] (2026-07-14 22:15Z) Confirmed from current browser specifications that a form with method POST encodes fields in the request entity body; top-level navigations are currently excluded from mixed-content blocking, while browser vendors may still warn on insecure form submissions.
- [x] (2026-07-14 22:18Z) Confirmed Chrome 142+ Local Network Access gates fetches, subresources, and subframe navigations, while top-level local navigations remain a documented future area. Native proof remains mandatory because browser policy can diverge.
- [x] (2026-07-14 22:18Z) Implemented the reusable Go one-shot receiver and exhaustive protocol tests; focused race and vet gates pass.
- [x] (2026-07-14 22:22Z) Implemented the reusable browser form builder, literal-loopback validator, opaque-token handling, and frontend tests; typecheck and focused Vitest pass.
- [x] (2026-07-14 22:28Z) Implemented the test-only authenticated browser harness, separate QA-only Vite build, redacting spike command, and native evidence procedure.
- [x] (2026-07-14 22:46Z) Passed focused and full Go tests/vet, focused race tests, 100 repeated receiver runs, all 288 frontend tests, ordinary-build exclusion, and the explicit QA-only harness build on Linux ARM64.
- [x] (2026-07-14 22:31Z) Passed a diagnostic Chromium 149 top-level POST against the real Go listener: exact content type and six fields arrived, the opaque token was preserved, Referer was absent, success rendered, and URL/console/command output contained no token.
- [x] (2026-07-14 22:42Z) Cross-constructed the command for Windows AMD64/ARM64 and macOS AMD64/ARM64 and verified the resulting PE/Mach-O machine types. These builds are construction evidence only and will be rebuilt and hashed from the published exact head.
- [ ] Publish the exact branch head and obtain Windows Edge, Windows Chrome, and macOS Safari evidence bound to that head.
- [ ] Seal the contract, record browser/OS versions and evidence hashes, request independent review, and move task #35 to review.

## Surprises & Discoveries

- Observation: Chrome's Local Network Access permission shipped in Chrome 142 and covers public-to-loopback requests made through fetch, subresources, and subframes, but the proposal still lists top-level local navigations, especially POST navigations, as possible future work.
  Evidence: Chrome's official launch note names fetch, subresource loading, and subframe navigation; the WICG explainer's "Potential future changes" section separately discusses top-level navigations.

- Observation: The mixed-content standard permits top-level document navigation to an HTTP target and allows browsers to warn on insecure form submissions. The callback must therefore be tested in each required native browser rather than inferred from unit tests or Playwright engines.
  Evidence: the mixed-content "Form Submission" section makes warning behavior optional, and its fetch/response algorithms exempt a document destination whose target has no parent browsing context.

- Observation: The existing frontend already has the complete human bearer-token session, workspace summaries, and `ApiClient.createDaemon`. No backend schema or endpoint change is needed for this spike.
  Evidence: `frontend/src/App.tsx` stores the human session in `codesk.auth.token`, and `frontend/src/api.ts` posts to `/api/workspaces/{workspaceID}/daemons` and receives `{daemon, token}`.

- Observation: Full Go regression tests need both the host `libyrs.a` and frontend `node_modules` because the Yjs compatibility test invokes Node from the Go suite.
  Evidence: the first full run identified each missing local prerequisite separately; after restoring the current Linux ARM64 library and the unchanged sibling dependency tree, `go test ./...` passed without a product change.

## Decision Log

- Decision: Land production-quality transport primitives plus a test-only browser harness, but do not add the product `/desktop/connect` route in this task.
  Rationale: task #35 must prove browser feasibility without pulling task #38's product workflow, persistence ordering, or UX across the dependency boundary. The transport primitives are directly reusable by task #38; the harness is deliberately unlinked from the production Vite build.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Bind only `tcp4` address `127.0.0.1:0` and require the browser action URL to contain the exact literal host `127.0.0.1`, an explicit nonzero port, and a 43-character unpadded base64url nonce generated from 32 random bytes.
  Rationale: a literal IPv4 loopback removes DNS and dual-stack ambiguity. Regex validation before URL parsing prevents alternate numeric spellings from canonicalizing into an apparently acceptable loopback host.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Use a top-level HTML form POST instead of `fetch`, XHR, iframe, WebSocket, or a query-string redirect.
  Rationale: the HTML form algorithm puts URL-encoded fields in the POST entity body. This avoids CORS/preflight machinery and the current Chrome Local Network Access coverage for fetch/subresources/subframes. The token is absent from the action URL, browser history, and referrer.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Invalid requests do not consume the session; the first fully valid request atomically wins, all concurrent or later valid requests are rejected, and the listener stops accepting immediately after flushing the success response.
  Rationale: random internet noise, browser probes, or a malformed request must not strand a legitimate connect attempt. Atomic first-valid-wins semantics make duplicate behavior deterministic and testable.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Treat the daemon token as an opaque credential, not as a `nottyd_`-prefixed 32-byte base64url value.
  Rationale: the daemon-create API contracts the response as a string; today's generator shape is an implementation detail. The transport enforces only nonempty bounded well-formed UTF-8 and rejects control characters, preserving future credential formats byte-for-byte.
  Date/Author: 2026-07-14 / Vitaliy

- Decision: Keep the token field private in the Go `Payload`, expose it only through `Token()`, and implement `String`/`GoString` as fmt-specific redaction defenses.
  Rationale: later secret-store code still needs direct opaque access, but an unexported field removes accidental JSON and default struct-field exposure. Tests cover `%v`, `%+v`, and `%#v`; this is deliberately not described as a general serialization guarantee.
  Date/Author: 2026-07-14 / Vitaliy

## Outcomes & Retrospective

The transport receiver, browser form helper, redacting command, test-only authenticated harness, and native evidence procedure are implemented. All local deterministic, race, repository, frontend, build-isolation, and cross-construction gates pass. A real Chromium 149 diagnostic run also passed the complete top-level POST flow on Linux ARM64.

The task remains incomplete until the same published exact head passes the authorized HTTPS Codesk-origin rows in native Windows Edge, native Windows Chrome, and native macOS Safari. The Linux diagnostic and cross-compiled binaries do not satisfy those rows.

## Context and Orientation

The repository is a Go monorepo with the web frontend under `frontend` and the current command-line daemon under `daemon`. The existing web application keeps a human API token in browser local storage and uses `frontend/src/api.ts` to create a daemon. The backend returns a daemon record plus a one-time plaintext daemon token; only its hash is stored server-side. The future desktop process needs the plaintext value once so it can protect it with DPAPI on Windows or Keychain on macOS.

A loopback listener is an HTTP server reachable only on the local computer. "Pre-bind" means the desktop process opens the socket before it launches the browser, avoiding a race in which another process could take the advertised port. A nonce is a one-time unpredictable value. This spike uses 32 cryptographically random bytes encoded as 43 unpadded base64url characters in the callback path. Knowledge of that path is the callback authorization boundary for the existing static-token model.

The reusable receiver belongs in `daemon/internal/desktop/handoff`. The `internal` directory means only code below `daemon` can import it. The browser form helper belongs in `frontend/src/desktopHandoff.ts`. The manual native harness consists of `frontend/desktop-handoff-spike.html`, its TypeScript/CSS entry files, and `daemon/cmd/desktop-handoff-spike`. Vite serves the extra HTML file during development, but the ordinary `vite build` continues to use only `frontend/index.html`, so the spike page is not a production route or artifact.

The browser harness reads the existing `codesk.auth.token` value from the same web origin, calls `ApiClient.me()` to obtain workspaces, requires an explicit workspace choice, calls `ApiClient.createDaemon`, and immediately submits the returned daemon token through the reusable hidden form. It never renders or logs the daemon token. The Go spike command prints the connect URL and accepted daemon/workspace identifiers, but only prints `token_received=true`; it never prints the token itself.

## Plan of Work

Create `daemon/internal/desktop/handoff/session.go`. Define a `Payload` holding daemon ID, token, workspace ID/name/slug/URL and a `Session` that owns its listener and HTTP server. `NewSession` generates the nonce with `crypto/rand`, binds `tcp4` on `127.0.0.1:0`, builds the exact callback URL, and starts serving with conservative header/read/body timeouts. The handler must require the exact Host and path, an empty query, POST, `application/x-www-form-urlencoded`, a small bounded body, exactly one value for each known field, and no unknown fields. It must validate all values without including values in error text. The first fully valid payload wins via an atomic compare-and-swap. The success response must be cache-disabled, referrer-disabled, content-sniffing-disabled, and protected by a `default-src 'none'` content security policy. `Wait(ctx)` must shut down and return the payload, a cancellation/timeout error, a manual-close error, or an unexpected serve error without leaking form values.

Create `daemon/internal/desktop/handoff/session_test.go`. Cover the exact 256-bit nonce shape, literal IPv4 binding, happy path, wrong path, wrong Host, query rejection, wrong method, wrong content type, oversized and malformed bodies, missing/duplicate/unknown fields, invalid token/workspace URL, concurrent duplicate requests, listener closure after success, context timeout, cancellation, manual close, response headers, and the invariant that errors and responses never contain the token. Run race tests so first-valid-wins has evidence under concurrency.

Create `frontend/src/desktopHandoff.ts` and `frontend/src/desktopHandoff.test.ts`. Validate the callback string against the exact canonical literal-loopback pattern before constructing a `URL`; reject alternate IP spellings, credentials, zero/overflow ports, query, fragment, wrong path, and short nonce. Build a hidden form with method POST, `application/x-www-form-urlencoded`, UTF-8, and fixed snake_case field names. Keep construction separate from submission so tests can inspect the form and prove its action contains no token.

Create `frontend/desktop-handoff-spike.html`, `frontend/src/desktopHandoffSpike.ts`, and `frontend/src/desktopHandoffSpike.css`. Keep it a compact test tool that uses the OXO app icon and existing design colors. It must show callback validity, session state, a workspace selector, daemon name, and one Connect command. It must require an existing same-origin Codesk login, ask for explicit workspace selection, create through the existing API, and submit immediately. Do not add this file to Vite's production Rollup inputs.

Create `daemon/cmd/desktop-handoff-spike/main.go`. Accept a connect-page base URL and timeout, start the reusable session before constructing the browser URL, print safe instructions, handle interrupt/cancel, wait once, and print only nonsecret identifiers plus a boolean token receipt. Validate the connect-page URL and never accept a daemon token in flags or environment variables.

Create `docs/testing/desktop-loopback-handoff.md`. Give exact native steps that serve the Codesk browser harness from an HTTPS Codesk test origin with the existing backend/session, run the exact-head spike binary, capture browser and OS versions, inspect the top-level POST request without recording the secret value, confirm the callback URL/history/referrer/log invariants, and record the exact Git commit and binary hash. State that local HTTP runs and Playwright engines are preflight only. Include separate rows for Windows Edge, Windows Chrome, and macOS Safari, including Chrome/Edge Local Network Access prompt behavior.

## Concrete Steps

Work from the repository root `/home/ubuntu/.slock/agents/ef95e2c0-f14c-4819-8345-417f5c0f9ca4/work/notty-desktop-handoff` on branch `spike/desktop-loopback-handoff`, based on `origin/main` commit `355df3fe366950ed8346099e54b3468429f3a811`.

Format and run the focused Go package:

    gofmt -w daemon/internal/desktop/handoff/*.go daemon/cmd/desktop-handoff-spike/*.go
    go test ./daemon/internal/desktop/handoff ./daemon/cmd/desktop-handoff-spike
    go test -race ./daemon/internal/desktop/handoff
    go vet ./daemon/internal/desktop/handoff ./daemon/cmd/desktop-handoff-spike

Run frontend gates from `frontend`:

    npm ci
    npm test -- --runInBand
    npm run build

The repository's Vitest command does not need `--runInBand`; if that option is rejected, run the canonical `npm test` instead. The ordinary production build must not emit `desktop-handoff-spike.html`.

Run the full host regression after restoring any generated native library expected by the host toolchain:

    go test ./...
    go vet ./...

For a local preflight, start the normal frontend/backend stack, sign into the app, and run:

    go run ./daemon/cmd/desktop-handoff-spike --connect-page http://127.0.0.1:5173/desktop-handoff-spike.html

Open the printed URL in a browser. Select a workspace and connect. Expect the browser to navigate to a local page saying the handoff was accepted, and expect the command to print daemon/workspace metadata with `token_received=true` and no token value.

## Validation and Acceptance

Local acceptance requires every focused and repository regression test to pass, the race detector to pass, vet to pass, and the ordinary frontend build to omit the spike HTML. A source scan must find no logging or formatting of `Payload.Token()`. Unit tests must prove URL, error, and response bodies do not contain the fixture secret.

Native acceptance requires three independent exact-head rows: current stable Edge on Windows, current stable Chrome on Windows, and current stable Safari on macOS. Each row records OS build, browser version, exact Git commit, spike binary SHA-256, connect-page HTTPS origin, whether a local-network or insecure-navigation prompt appeared, whether the callback succeeded, the callback request method/content type/URL, and checks that the token was absent from URL/history/referrer/console/app output. The request body value may be inspected live but must be redacted from screenshots and shared transcripts. A browser block is a failed spike and stops the desktop program for redesign; no query-string token, plaintext handoff file, CORS workaround, or backend bootstrap protocol may be substituted.

## Idempotence and Recovery

Every receiver session binds a fresh port and nonce. A timeout, cancellation, malformed request, or successful handoff closes only that session's listener. Re-running the command is safe and creates a new session. The harness creates a real daemon row; native testers should delete abandoned spike daemon rows through the existing Codesk management UI after each failed or repeated run. They must never paste the returned daemon token into chat, logs, screenshots, or shell history.

If a browser blocks the navigation, preserve the browser version, console/network error, and whether the request reached the local listener. Stop there and report the failure. Do not weaken the transport. If the filesystem fills during Go or frontend builds, remove only this worktree's generated caches/artifacts or use a temporary build cache; do not delete active shared agent caches or the injected Raft wrapper.

## Artifacts and Notes

Authoritative protocol facts used by this plan:

- The HTML form submission algorithm serializes `application/x-www-form-urlencoded` fields into the POST entity body and navigates to the unchanged action URL.
- The mixed-content standard currently exempts top-level document navigation from mandatory blocking but permits browser warnings for insecure form submission.
- Chrome's Local Network Access shipped in Chrome 142 for fetch, subresources, and subframes; the WICG design still describes top-level local POST navigation as possible future scope.
- WebKit's history around HTTPS-to-loopback behavior makes current native Safari proof mandatory even though a WebKit engine test passes elsewhere.

The final evidence summary and exact hashes will be added here when native rows complete.

## Interfaces and Dependencies

In `daemon/internal/desktop/handoff/session.go`, provide these stable interfaces for later desktop work:

    type Payload struct {
        DaemonID     string
        WorkspaceID  string
        WorkspaceName string
        WorkspaceSlug string
        WorkspaceURL  string
        token         string
    }

    func (p Payload) Token() string

    type Session struct { /* private ownership fields */ }

    func NewSession() (*Session, error)
    func (s *Session) CallbackURL() string
    func (s *Session) Wait(ctx context.Context) (Payload, error)
    func (s *Session) Close() error

In `frontend/src/desktopHandoff.ts`, provide:

    export type DesktopHandoffPayload = {
      daemonId: string;
      token: string;
      workspaceId: string;
      workspaceName: string;
      workspaceSlug: string;
      workspaceUrl: string;
    };

    export function parseDesktopCallback(value: string): URL;
    export function createDesktopHandoffForm(doc: Document, callback: string, payload: DesktopHandoffPayload): HTMLFormElement;
    export function submitDesktopHandoff(callback: string, payload: DesktopHandoffPayload): void;

Use only the Go standard library and existing frontend dependencies. Do not add systray, DPAPI, Keychain, browser-opening, secret persistence, daemon lifecycle, backend schema, or backend auth code in this spike.

Revision note (2026-07-14, Vitaliy): Created the initial self-contained plan after repository and browser-policy research. The plan deliberately separates reusable transport proof from the later product route and desktop lifecycle tasks.

Revision note (2026-07-14 22:46Z, Vitaliy): Recorded the completed implementation, opaque-token interface correction, fmt redaction boundary, full local gates, Chromium diagnostic, and the remaining exact-head native acceptance rows.
