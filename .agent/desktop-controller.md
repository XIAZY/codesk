# Build the shared desktop daemon controller and tray contract

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This repository contains `.agent/PLANS.md`; this document must be maintained in accordance with that file.

## Purpose / Big Picture

Codesk needs a small Windows and macOS tray application that owns the existing synchronization daemon in-process. After this change, later platform packages can start one daemon generation, show its real readiness in a native tray menu, restart transient failures with bounded backoff, stop retrying when credentials require reconnection, and quit only after daemon teardown has joined. The browser remains the only full user interface, and this change does not implement authentication, secret storage adapters, installers, or platform signing.

The behavior is observable through deterministic Go tests. The tests run fake daemon generations through startup, readiness, transient failure, reconnect-required failure, manual restart, and quit. They prove that no two generations overlap and that Quit does not complete before the active generation has returned from `Run`.

## Progress

- [x] (2026-07-15 02:00Z) Read the approved design, task #36 contract, merged loopback receiver, merged desktop directory/contracts package, and the existing `syncer.Service` lifecycle.
- [x] (2026-07-15 02:05Z) Added the real per-generation syncer readiness accessor and typed reconnect-required error boundary; focused tests pass.
- [x] (2026-07-15 02:17Z) Implemented the shared controller and deterministic clock/service seams; the full lifecycle matrix passes 50 times under the race detector.
- [x] (2026-07-15 02:36Z) Added the exact pure tray menu model and a Windows/macOS-tagged `fyne.io/systray` renderer that forwards typed actions only; Windows AMD64 and ARM64 source packages cross-link with Zig.
- [x] (2026-07-15 04:20Z) Rebased onto current main and completed the focused race, full Go, vet, build, formatting, diff, Windows dual-architecture construction, and pristine-main comparison gates.
- [x] (2026-07-15 04:06Z) Published scoped draft PR #165; the final remote-resolved head is recorded in the task thread for review.
- [x] (2026-07-15 06:24Z) Rebased onto PR #164/#168-updated main `366c028`, added the causal successful-`Run` readiness assertion, hardened the terminal-auth test watchdog against shared-runner load, and completed the focused race plus full Go gates.

## Surprises & Discoveries

- Observation: `syncer.Service` already closes an internal `ready` channel only after the primary workspace watcher, status reporter, heartbeat, and event loop are established, but production `New` does not initialize that channel and no public accessor exists.
  Evidence: `daemon/internal/syncer/service.go` calls `s.signalReady()` after startup, while only lifecycle tests currently construct `Service{ready: make(chan struct{})}`.

- Observation: terminal authentication and drain failures are already classified internally from HTTP 401, 403, and 410, but callers receive the private backend error type and would have to parse or duplicate private logic.
  Evidence: `isTerminalAuthError` is private in `daemon/internal/syncer/service.go` and is used by both initial refresh and steady-state event handling.

- Observation: a service instance is not a reusable daemon generation because readiness is one-shot and teardown shuts down internal supervisors. The approved design explicitly says the controller creates `syncer.New(cfg)` for each generation.
  Evidence: the design thread states `create syncer.New(cfg) -> Run(ctx) -> cancel/join on Stop, Restart, re-auth, or Quit`.

- Observation: the shared Linux host had only 1.3 GiB free, so the existing syncer append-pressure suite and a later ARM64 Rust cross-build initially failed with `no space left on device`. Clearing the disposable Go build cache before each high-water build made both product gates pass; no product test failed after space was restored.
  Evidence: the rerun of `go test ./daemon/internal/syncer -count=1` passed, and both Windows bridge packages cross-linked after rebuilding the matching Yrs archives.

- Observation: a naturally returned service generation had joined, but dropping its `context.CancelFunc` without calling it retained the child context in the desktop parent until application exit. Unlimited transient retries would therefore accumulate completed generation contexts.
  Evidence: the fake service now exposes its exact `Run` context, and `TestControllerReleasesNaturallyExitedGenerationContext` proves the context is canceled before the controller publishes the retry.

- Observation: native exit has two distinct callback cycles. On Windows, `fyne.io/systray.Quit` invokes the registered exit callback synchronously, so the callback cannot join the goroutine calling `Quit`. On Darwin, menu setters use `performSelectorOnMainThread(... waitUntilDone:YES)` while `onExit` itself runs on that main event-loop thread, so the callback cannot wait for an in-flight asynchronous `onReady` either.
  Evidence: inspection of `systray_windows.go`, `systray_darwin.go`, `systray_darwin.m`, and `systray.go` shows both cycles. The final bridge keeps the Quit caller outside the action/update worker group, uses a once-closed readiness gate to make all `WaitGroup.Add` calls precede joins, lets `onExit` wait only after `onReady` is already complete, and performs unconditional final joins after `systray.Run` returns.

- Observation: the exact-tree `go test ./...` run reached one load-sensitive existing failure, `TestWorkspaceReplicaStartupDrainsEventsBeforeFirstAdd`, while all new desktop, handoff, and terminal-auth integration rows passed. The same row is independently recorded as failing on pristine `67583e35` in #daemon-gui:022a8ef1.
  Evidence: the failed full run reported only that row in `daemon/internal/syncer`; task #22 records the same baseline timing failure and calls for a separate deterministic-barrier fix.

- Observation: after rebasing from the task's starting base onto `origin/main` at `0507ebe`, the exact full `go test ./...` gate passed. Main advanced once more during publication when PR #162 merged, and the final review branch rebased conflict-free again onto `afa8dd8`. A simultaneous host Go snap update to 1.26.5 invalidated the cache; that fresh full run reached only newly merged PR #162 steer-retry fixture failures before runtime construction. The exact named rows fail identically on a detached pristine `afa8dd8` worktree on this seat, so task #36 is not causal.
  Evidence: `TestAgentSessionFailedSteerRetriesSameInboxSignature`, all three `TestAgentSessionStaleSteerFailureDoesNotClearNewDeliveryState` subcases, and `TestAgentSessionNoActiveTurnSteerErrorIsNotFatal` report `expected one runtime process, got 0` on both the task branch and pristine current main. Controller race 50x and the focused readiness/terminal-auth syncer rows race 20x remain green after the final rebase; the first-rebase full Go run, daemon vet, daemon build, and diff checks also passed.

- Observation: after PR #166 restored green main, the replacement rebase onto `021e6d2` was conflict-free. One first aggregate run exhausted the terminal-auth integration row's three-second test context under package load, although the immediate full rerun, the focused selector under race 20 times, and that exact integration test 100 times were green. Main then advanced through PR #163, PR #164, and PR #168; each follow-up rebase was conflict-free.
  Evidence: extending only the test watchdog to ten seconds preserved the retry-until-timeout failure mode while removing the shared-runner scheduling sensitivity. The post-change focused race and full repository Go tests are green on final base `366c028`.

## Decision Log

- Decision: The controller owns at most one active service generation and asks a factory for a new `syncer.Service` after a transient exit or manual restart.
  Rationale: this preserves one lifecycle owner and one active daemon while respecting the existing service's one-run lifecycle.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: Capped retry means exponential retry delay capped at a configured maximum, not a finite retry count.
  Rationale: the approved design says unexpected non-auth exits restart with capped backoff, while only terminal authentication stops retrying.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: Keep platform integrations behind the contracts already defined in `daemon/internal/desktop/desktop.go`; task #36 adds no DPAPI, Keychain, login-item, instance-lock, or URL-opening implementation.
  Rationale: those native adapters and their execution evidence belong to later Windows/macOS tasks.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: The systray dependency is isolated behind Windows/macOS build tags and is used only to render the native menu and forward clicks.
  Rationale: the Linux ARM64 development and CI host must test the controller without a GUI runtime, and the approved no-window design calls for `fyne.io/systray` rather than the full Fyne toolkit.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: Keep configured/unconfigured state out of the daemon controller and pass it into the pure menu model as an explicit boolean.
  Rationale: credential persistence and connect workflow belong to task #38, while task #36 still needs to express disabled `Connect`, enabled first-time `Connect`, and terminal-auth `Reconnect` without inventing a sixth lifecycle state.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: The phase-1 command path contains the native tray renderer but no composition-root `main` yet.
  Rationale: a runnable desktop entry point cannot correctly own configuration, secret loading, browser connect, or native adapters until tasks #38-#40 land. A placeholder executable would violate the app-running-means-daemon-running rule. The bridge compiles as a package and exposes the narrow channel seam those tasks will wire.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: Explicitly cancel every completed generation context, including natural service exits and readiness/exit races, before publishing the next state.
  Rationale: joining a goroutine does not release the parent context's child reference; the controller owns both the generation and its cancellation resource.
  Date/Author: 2026-07-15 / Vitaliy

- Decision: Treat the native Quit caller and asynchronous readiness setup as lifecycle coordination, not ordinary tray workers.
  Rationale: action and update workers may be joined from `onExit` only after readiness setup is known complete. The goroutine that can synchronously invoke `onExit`, and an in-flight Darwin `onReady` that may be synchronously dispatching back to the event-loop thread, must instead be joined after the native loop returns. A mutex plus once-closed readiness gate prevents both `WaitGroup.Add` races and double-close on exit-before-ready.
  Date/Author: 2026-07-15 / Vitaliy

## Outcomes & Retrospective

The implementation satisfies the task #36 shared-controller acceptance boundary. `syncer.Service` now exposes its existing real startup signal and a typed terminal reconnect boundary without leaking its private backend error type. The desktop controller creates one fresh service per generation, publishes exactly the five approved states, caps exponential retry delay without stopping transient retries, halts on reconnect-required errors, and serializes Restart and Quit through cancel-and-join ownership. The pure eight-row menu model contains no Start/Stop mode, and the native bridge only renders that model and forwards typed actions.

The final lifecycle audit materially improved the first draft. It added explicit release of naturally completed child contexts and corrected two platform-specific shutdown cycles: Windows' synchronous `Quit` callback and Darwin's synchronous main-thread menu dispatch. The permanent controller matrix covers startup, readiness, retry cap/reset, reconnect, manual restart, join ordering, parent cancellation, atomic pre-start Quit, concurrent Restart/Quit pressure, and resource release. On final main `366c028`, controller race 50x and focused syncer readiness plus real 401/403/410 `Service.Run` rows race 20x are green. The real successful `Service.Run` path now causally seals the production readiness transition, and full `go test ./...`, Go vet/build, formatting, and diff checks pass on the final rebased tree.

Native evidence remains intentionally bounded. The Windows AMD64 and ARM64 source packages cross-link with Zig against matching Yrs archives, but this Linux ARM64 seat did not execute either binary and cannot prove native tray behavior. There is no placeholder composition-root `main`: tasks #38-#40 must wire accepted configuration, connect, instance-lock, login-item, URL, log, and platform lifecycle adapters before a runnable command can truthfully satisfy app-running-means-daemon-running. No macOS construction or runtime claim is made here.

## Context and Orientation

The repository root is the Go module `notty`. `daemon/internal/syncer/service.go` contains `syncer.Service`, the existing in-process synchronization daemon. Its `Run(context.Context)` method performs ordered startup and teardown. The real readiness point is after the primary workspace runtime is ready and all steady-state loops have started. A daemon generation means one service returned by `syncer.New` and one call to its `Run` method.

`daemon/internal/desktop/desktop.go` contains the platform-neutral contracts merged by task #37: `SecretStore`, `LoginItem`, `InstanceLock`, `OpenURL`, validated app directories, and explicit construction of `syncer.Config`. This task extends that package with the lifecycle controller and menu model, but it does not implement any of those platform adapters.

`daemon/cmd/codesk-desktop` is the new native entry-point package. Its systray bridge is intentionally thin: `fyne.io/systray` owns the native main loop and menu items, while platform-neutral code owns state and action semantics. Authentication and persistent configuration are added by task #38, so this PR must not invent a CLI environment fallback, put a token in command-line arguments, or implement a temporary plaintext config path.

The controller states are `starting`, `online`, `retrying`, `reconnect-required`, and `quitting`. A transient error is any unexpected service creation or run error that is not typed as reconnect-required. A reconnect-required error means credentials were rejected or the daemon principal was drained and the user must complete the browser connection workflow before a new generation can succeed.

## Plan of Work

First, update `daemon/internal/syncer/service.go` so `New` initializes a readiness channel and `Service.Ready` exposes it read-only. Add an exported `ReconnectRequiredError` that wraps the existing private terminal-auth error without exposing the private backend response type. `Service.Run` should wrap only errors that the existing internal classifier considers terminal. Preserve cancellation as a successful nil return. Add focused tests proving the readiness channel is fresh and one-shot and proving 401/403/410 are typed while transient and cancellation paths are not.

Second, add `daemon/internal/desktop/controller.go`. Define a narrow service interface with `Run(context.Context) error` and `Ready() <-chan struct{}`, a factory function, a stoppable timer/clock interface, snapshots, and the five states. The controller owns an event loop. It starts a generation, publishes `starting`, changes to `online` only when the real readiness channel closes, and handles service exits. Transient exits publish `retrying` and start exponential backoff capped at the configured maximum. Typed reconnect-required exits publish `reconnect-required` and wait for manual Restart. Restart cancels and joins the old generation before creating another. Quit publishes `quitting`, cancels and joins the active generation, then closes `Done`.

Third, add `daemon/internal/desktop/controller_test.go` with fake services, a fake factory, and a manually fired clock. Cover successful readiness, pre-readiness exit, capped retry, retry reset after readiness, terminal reconnect without timer creation, Restart from online/retrying/reconnect-required, Quit before readiness and while online, simultaneous restart/quit pressure, no overlapping generations, and parent-context cancellation. Run the matrix repeatedly under the race detector.

Fourth, add a pure menu model in `daemon/internal/desktop/menu.go` and tests. It must always describe status, Open Codesk, Connect or Reconnect, Restart, Launch at login, Open logs, version, and Quit. It must not expose Start/Stop. Add a Windows/macOS-only systray bridge under `daemon/cmd/codesk-desktop` that maps the model to `fyne.io/systray` and forwards menu clicks through typed actions. It must contain no secret/config loading and no platform adapter implementation; later tasks wire those dependencies.

Finally, format and validate the whole scoped change, inspect the diff for accidental generated or unrelated files, commit intentionally, push the branch, open a draft pull request, and report the exact remote-resolved head to the task thread.

## Concrete Steps

All commands run from the repository root `/home/ubuntu/.slock/agents/ef95e2c0-f14c-4819-8345-417f5c0f9ca4/work/notty-desktop-controller`.

Implement and format the focused packages:

    gofmt -w daemon/internal/syncer/service.go daemon/internal/syncer/*_test.go daemon/internal/desktop/*.go daemon/cmd/codesk-desktop/*.go

Run focused lifecycle gates:

    go test -race -count=50 ./daemon/internal/desktop
    go test -race -count=20 -run '^(TestNewServiceHasFreshReadinessSignal|TestNilServiceReadyIsNil|TestIsTerminalAuthError|TestWrapReconnectRequired|TestWrapReconnectRequiredDoesNotDoubleWrap|TestRunExposesReconnectRequiredError|TestRunWorkspaceEventStreamClassifiesHandshakeStatus)$' ./daemon/internal/syncer

Run repository Go gates after restoring the host Yrs archive if a targeted cross-build replaced it:

    ./scripts/build-yffi.sh
    go test ./...
    go vet ./daemon/...
    go build ./daemon/...
    git diff --check

Inspect scope and state:

    git status --short
    git diff --stat origin/main...HEAD
    git diff --check origin/main...HEAD

The expected focused result is that every new controller and syncer lifecycle row passes under race. The expected full result is no test, vet, build, formatting, or whitespace error.

## Validation and Acceptance

The controller acceptance is behavioral. A fake generation that closes readiness must produce `starting` then `online`. A fake generation that exits transiently must produce `retrying`, and manually firing the fake timer must start exactly one replacement generation. Repeated startup failures must request delays that double to the configured maximum and never exceed it. A generation returning `ReconnectRequiredError` must produce `reconnect-required`, create no retry timer, and remain stopped until Restart.

Restart must not create the replacement until the old fake service has observed cancellation and returned from `Run`. Quit must not return or close `Done` until that same join completes. Under concurrent Restart/Quit pressure, the maximum number of active fake services must remain one and the final state must be `quitting`. These tests run with `go test -race -count=50 ./daemon/internal/desktop`.

The syncer acceptance is that `Service.Ready()` exposes the existing real readiness point and that terminal HTTP 401, 403, and 410 errors returned by `Service.Run` satisfy `errors.As(err, *ReconnectRequiredError)`. Ordinary backend/network errors must not satisfy that type. Existing daemon lifecycle tests must remain green.

The menu acceptance is that every state maps to a stable status label and the menu contains exactly the approved commands, with `Connect` becoming `Reconnect` in reconnect-required state and with no Start or Stop command. The native bridge must compile for the supported target source packages; native visual/runtime execution belongs to tasks #39 and #40 and must not be claimed from this Linux ARM64 seat.

## Idempotence and Recovery

The implementation is additive and commands are safe to repeat. The controller tests use temporary/fake resources and leave no daemon process. If a test hangs, run the single named row with `-timeout=30s -count=1`, inspect which fake generation has not released, and fix the ownership protocol rather than adding sleeps. If a cross-build changes `third_party/y-crdt/target/release/libyrs.a`, run `./scripts/build-yffi.sh` without `RUST_TARGET` before host tests.

Do not delete or modify existing worktrees. This task uses branch `feat/desktop-controller` in the dedicated `work/notty-desktop-controller` worktree. If remote main advances, fetch and rebase only before review evidence is pinned; after a review is pinned, coordinate any head change in the task thread.

## Artifacts and Notes

The starting base was merge commit `00a49ddb94b96310fa9a2f772d15757658f716b0`, which contains the accepted loopback receiver from PR #160 and desktop config/contracts from PR #161. Before publication the scoped commit rebased conflict-free onto `0507ebe` (merged PR #158), then rebased conflict-free again onto current main `afa8dd8` after PR #162 merged during publication.

Draft PR #165 is <https://github.com/XIAZY/notty/pull/165>. Its final remote-resolved commit is reported outside this self-referential document in the task #36 thread.

The approved no-go boundaries are: no child daemon process, localhost control API, application window or webview, human JWT, Start/Stop mode, static token in URL/argv/log/plaintext config, or platform secret/login/installer implementation in this PR.

## Interfaces and Dependencies

In `daemon/internal/syncer/service.go`, expose:

    func (s *Service) Ready() <-chan struct{}

    type ReconnectRequiredError struct {
        Err error
    }

The type must implement `error` and `Unwrap() error`. `Service.Run` wraps an existing terminal-auth/drain error in this type exactly once.

In `daemon/internal/desktop/controller.go`, define a service interface that only requires `Run(context.Context) error` and `Ready() <-chan struct{}`. Define a factory function returning a fresh service generation and a clock interface returning stoppable timers. Expose controller construction, `Start`, `Restart`, `Quit`, `Done`, `Snapshot`, and `Updates` without exposing mutable internal channels.

Use `fyne.io/systray` v1.12.2 only from the Windows/macOS bridge. Do not depend on the full Fyne application toolkit.

Revision note (2026-07-15 02:00Z): Initial plan created after reading the accepted design and merged dependencies. It records the one-active-generation interpretation, typed syncer boundary, deterministic controller matrix, and strict separation from authentication and native adapters.

Revision note (2026-07-15 02:05Z): Marked the syncer lifecycle seam complete after the focused readiness and reconnect-error tests passed. No design change was required.

Revision note (2026-07-15 02:36Z): Marked the controller and tray/menu implementation complete. Recorded the explicit configured-state menu input, deferred composition-root decision, deterministic race evidence, Windows dual-architecture construction evidence, and host disk-pressure recovery.

Revision note (2026-07-15 03:27Z): Recorded the completed-generation context retention and Windows synchronous-exit deadlock found during final audit, their permanent regressions/coordination fixes, the real `Service.Run` terminal-auth integration rows, and the exact-tree baseline timing failure tracked separately by task #22.

Revision note (2026-07-15 03:36Z): Extended the native lifecycle audit to Darwin's synchronous main-thread menu dispatch. Moved in-flight readiness and unconditional worker joins out of `onExit`, while retaining an `onExit` fast join only when readiness setup is already complete.

Revision note (2026-07-15 04:04Z): Recorded the conflict-free rebase onto `0507ebe`, the now-green exact full Go gate, the final dual-architecture construction evidence, and the completed acceptance/outcome comparison. Publication and exact remote-head recording remain the only open steps.

Revision note (2026-07-15 04:06Z): Marked publication complete at draft PR #165. The task thread is the source of truth for the final remote-resolved head because embedding a commit's own hash in this file is self-referential.

Revision note (2026-07-15 04:08Z): Recorded the second conflict-free rebase onto current main `afa8dd8` after PR #162 landed during publication. The review pin is taken only after the rebased gates and final force-push complete.

Revision note (2026-07-15 04:20Z): Recorded the fresh full-suite PR #162 fixture failures after the host Go snap update and the identical detached-pristine-main proof on `afa8dd8`. Task #36's final focused race gates remain green; no unrelated steer-retry change enters this PR.

Revision note (2026-07-15 06:24Z): Recorded the conflict-free rebases through post-revert main `021e6d2` and the PR #163/#164/#168 updates to final main `366c028`, the causal successful-`Run` readiness seal, the load-tolerant terminal-auth test watchdog, and the green focused/full final-base gates.
