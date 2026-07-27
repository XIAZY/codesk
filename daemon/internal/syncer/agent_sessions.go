package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const forMeSteerMessage = "You have new for-me items in your notification center. Continue your current task, but consider them when appropriate."
const agentStatusUpdateTimeout = 30 * time.Second

type updateAgentSessionRequest struct {
	Status          string `json:"status"`
	SessionID       string `json:"sessionId,omitempty"`
	CurrentTurnID   string `json:"currentTurnId,omitempty"`
	CurrentActivity string `json:"currentActivity,omitempty"`
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
}

type agentSessionUpdater func(context.Context, string, updateAgentSessionRequest) error

type agentSessionSupervisor struct {
	cfg          Config
	status       *agentStatusSyncer
	runtimes     *runtimeRegistry
	wakeAgent    func(string)
	restartSleep func(time.Duration)

	// Liveness/heartbeat (item #5): heartbeatInterval is how often a working
	// session's liveness is re-evaluated and its heartbeat refreshed;
	// stallAfter is the silence past which a still-"working" session with no
	// runtime activity is surfaced as `stalled` (never killed). now is the clock,
	// injectable so liveness tests are deterministic without wall-clock waits.
	heartbeatInterval time.Duration
	stallAfter        time.Duration
	now               func() time.Time
	// terminalExitReason classifies a process death as a terminal provider
	// failure (returns the reason) or not (returns ""). Defaults to
	// defaultTerminalExitReason (empty set until a CLI-proven signal exists);
	// injectable so the terminal branch is verified, not dormant.
	terminalExitReason func(RuntimeExitInfo) string

	// testHookRestartComplete, when set, is invoked when a restart goroutine
	// finishes (whether it respawned or bailed on shutdown). Test seam only.
	testHookRestartComplete func()

	// testHookDeathHandled, when set, fires once consumeEvents has classified a
	// process death and taken its action (expected/terminal/transient), passing
	// the classification. Test seam only.
	testHookDeathHandled func(classification string)

	// testHookRestartLaunched, when set, fires each time launchRestartWorker
	// actually launches a restart worker — used to prove the single-owner claim
	// launches exactly one restarter under a natural-exit-vs-stall race. Test seam.
	testHookRestartLaunched func()

	// testHookLivenessPrePublish, when set, fires inside evaluateLiveness while
	// s.mu is held, immediately before the status publish — used to prove the
	// publish is serialized with the state decision. Test seam only.
	testHookLivenessPrePublish func()

	// testHookInitialPublish, when set, fires inside startSession while s.mu is held,
	// after the session is stored and before the initial idle enqueue — used to prove
	// a concurrent Schedule cannot publish working before initial-idle ordering is
	// established. Test seam only.
	testHookInitialPublish func()

	// shutdownOnce guards Shutdown so its teardown (baseCtx cancel, session stop,
	// construction drain, status stop) runs exactly once even if Shutdown is
	// called multiple times (the #145 idempotency invariant).
	shutdownOnce sync.Once

	mu       sync.Mutex
	sessions map[string]*managedAgentSession
	starting map[string]*agentSessionStart
	// shutdown is the closed-state flag (the #145 `closed`): once set under s.mu
	// no start path may admit a new session/claim/construction, and publish is
	// refused. It is the authority both the crash-classification restart gate and
	// the #145 lifecycle checks consult.
	shutdown bool
	// restartAttempts is the single per-agent failure ledger. It survives process
	// replacement and fresh construction claims, so an explicit profile that can
	// never establish a session cannot evade its finite cap through reconcile.
	// Proven turn completion, removal, or an authoritative profile change resets
	// it; a bare respawn/reconcile does not.
	restartAttempts map[string]runtimeRestartState
	// observationSequence orders token-free diagnostic events within this service;
	// runtimeGeneration disambiguates process replacement and PID reuse. Both are
	// guarded by mu with the session state they describe.
	observationSequence uint64
	runtimeGeneration   uint64
	observationQueue    []RuntimeObservation
	observationWake     chan struct{}
	observationDone     chan struct{}
	observationClosed   bool

	// baseCtx is the parent of every (fresh AND restart) detached construction AND
	// every live consumeEvents loop; baseCancel (fired by Shutdown) cancels all
	// in-flight Spawn/Start/handshake calls and event loops. In production baseCtx
	// is a child of the service run context (bound once at run() entry via
	// bindServiceContext), so a service-context cancellation — including during the
	// synchronous startup refresh, before Shutdown is reachable — propagates to
	// every construction. Direct tests keep the Background-derived baseCtx from the
	// constructor.
	baseCtx      context.Context
	baseCancel   context.CancelFunc
	serviceBound bool

	// constructionWG tracks every detached construction worker (fresh and restart)
	// AND every live consumeEvents loop. Shutdown cancels baseCtx (which makes a
	// real provider Start/handshake return and every event loop exit), then waits
	// on this barrier before status teardown — so it never returns while a detached
	// worker still owns an unpublished process or an event loop can still publish.
	// Add is done under s.mu on the same side as the shutdown check, so it can
	// never race Wait and nothing is admitted after shutdown.
	constructionWG sync.WaitGroup
	// removalWG tracks Reconcile-owned session removals. An owner is registered
	// under mu before the session leaves the map, then stops and joins that exact
	// runtime generation before enqueueing stopped_expected. Shutdown closes the
	// observation dispatcher only after every racing removal owner has finished.
	removalWG sync.WaitGroup
}

type agentSessionStart struct {
	done chan struct{}
	err  error
	// cancelled is set by Reconcile when the agent is removed while this fresh
	// construction is in flight; cancel interrupts its Spawn/Start/handshake. The
	// conditional publish refuses to store a session for a cancelled claim, so a
	// nil-slot fresh start cannot resurrect a removed agent.
	cancelled bool
	cancel    context.CancelFunc
	// agent is the LATEST desired spec the sole owner is building; a changed-spec
	// reconcile retargets it and bumps rev, and the owner rebuilds from it. The
	// publish CAS fences on rev so a cancellation-ignoring old attempt cannot store
	// a stale-spec runtime.
	agent *agent
	rev   uint64
}

type runtimeRestartState struct {
	attempts    int
	nextAttempt time.Time
	profile     RuntimeProfile
	lastFailure string
	exhausted   bool
}

type agentSessionStartupError struct {
	AgentID string
	Handle  string
	Err     error
}

func (e *agentSessionStartupError) Error() string {
	if e == nil {
		return ""
	}
	label := strings.TrimSpace(e.AgentID)
	if strings.TrimSpace(e.Handle) != "" {
		label = label + " (" + strings.TrimSpace(e.Handle) + ")"
	}
	if e.Err == nil {
		return "agent session " + label + " failed during startup"
	}
	return "agent session " + label + " failed during startup: " + e.Err.Error()
}

func (e *agentSessionStartupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type managedAgentSession struct {
	agent     *agent
	process   RuntimeProcess
	toolToken string
	workdir   string
	sessionID string

	activeTurn string
	state      string

	runtimeKind       RuntimeKind
	runtimeGeneration uint64
	runtimePID        int
	turnSequence      uint64
	observedTurnID    string
	expectedStopOwned bool
	lifecycleCancel   context.CancelFunc
	eventLoopDone     chan struct{}
	heartbeatLoopDone chan struct{}

	// lastActivitySeq is the driver activity generation (process.ActivitySeq()) last
	// observed for this session; lastActivityAt is the supervisor's OWN monotonic
	// time (s.now()) when that generation last advanced OR a lifecycle transition
	// occurred. Silence is measured only as s.now() - lastActivityAt, so it is
	// monotonic-sound and never depends on comparing a wall-clock timestamp across a
	// clock step (blocker 12). Recovery from `stalled` requires the generation to
	// advance — a genuinely new frame — which resets lastActivityAt to now.
	lastActivitySeq uint64
	lastActivityAt  time.Time

	// stallDiagnostic is the human-facing CurrentActivity captured ONCE at the
	// working->stalled transition. Every subsequent stalled heartbeat republishes
	// this exact string (proving the daemon is still alive by advancing
	// LastHeartbeatAt) rather than recomputing a duration-bearing line each minute —
	// which would be a semantic activity change per tick and recreate the permanent
	// activity flood the backend comparator suppresses (blocker 16).
	stallDiagnostic string

	// restartPending marks that a backoff restart owns this (dead) session
	// generation: no other start path may replace it until the delayed restarter
	// releases it. dead marks that this exact process generation has died — it
	// gates authorization (a dead generation's tool token is invalid) and is set
	// only for the generation whose process actually exited.
	restartPending bool
	dead           bool
	// terminalProfileReason records a typed provider rejection observed while the
	// process was live. It dominates the generic result/exit that follows, even
	// when Stop makes that later exit look expected.
	terminalProfileReason string

	// constructCancel interrupts an in-flight restart construction for this
	// (parked) token; Reconcile removal fires it so a blocked replacement
	// Spawn/Start/handshake is cancelled and reaped promptly, not after the CAS.
	constructCancel context.CancelFunc

	// agentRev is a monotonic revision of the desired agent spec (`agent`). A
	// parked restarter captures it with the spec at construction and the
	// conditional swap requires it unchanged at publication, so a reconcile that
	// refreshes the spec mid-construction forces a rebuild from the latest spec
	// instead of publishing a process built from stale instructions.
	agentRev uint64

	// turnOpSeq is a monotonic per-session operation nonce. A StartTurn write
	// captures it at its decision point and re-checks it at completion; any
	// intervening authoritative transition (markIdle for a completed/failed/idle
	// turn, the death owned-block, or a newer notification write) bumps it so a
	// stale completion that lands after the transition becomes a no-op instead of
	// reviving already-settled state.
	turnOpSeq uint64

	// turnStartAttempts counts consecutive admission-rejected StartTurn RPCs
	// (JSONRPC -32001); turnBackoffUntil is the earliest time the next attempt is
	// allowed. Resets only on an accepted StartTurn, not on turn completion or
	// process restart. Session replacement resets implicitly (new struct).
	turnStartAttempts int
	turnBackoffUntil  time.Time

	deliveredForMeSig   string
	deliveredGeneralSig string
	activeForMeSig      string
	activeGeneralSig    string
	followupForMeSig    string
	followupGeneralSig  string
	steeredForMeSig     string
}

type agentStatusSyncer struct {
	updater agentSessionUpdater

	mu      sync.Mutex
	workers map[string]*agentStatusWorker
	closed  bool
}

type agentStatusWorker struct {
	agentID string
	updater agentSessionUpdater

	mu       sync.Mutex
	latest   updateAgentSessionRequest
	dirty    bool
	wake     chan struct{}
	stopped  chan struct{}
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
}

func newAgentStatusSyncer(updater agentSessionUpdater) *agentStatusSyncer {
	return &agentStatusSyncer{
		updater: updater,
		workers: map[string]*agentStatusWorker{},
	}
}

func (s *agentStatusSyncer) Publish(agentID string, payload updateAgentSessionRequest) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	worker := s.workers[agentID]
	if worker == nil {
		worker = newAgentStatusWorker(agentID, s.updater)
		s.workers[agentID] = worker
	}
	s.mu.Unlock()
	worker.Publish(payload)
}

func (s *agentStatusSyncer) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	workers := make([]*agentStatusWorker, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	s.workers = map[string]*agentStatusWorker{}
	s.mu.Unlock()
	for _, worker := range workers {
		worker.Stop()
	}
}

func newAgentStatusWorker(agentID string, updater agentSessionUpdater) *agentStatusWorker {
	ctx, cancel := context.WithCancel(context.Background())
	worker := &agentStatusWorker{
		agentID: agentID,
		updater: updater,
		wake:    make(chan struct{}, 1),
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	go worker.run()
	return worker
}

func (w *agentStatusWorker) Publish(payload updateAgentSessionRequest) {
	w.mu.Lock()
	w.latest = payload
	w.dirty = true
	w.mu.Unlock()
	w.signal()
}

func (w *agentStatusWorker) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopped)
		w.cancel()
	})
	w.signal()
	<-w.done
}

func (w *agentStatusWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *agentStatusWorker) run() {
	defer close(w.done)
	backoff := 250 * time.Millisecond
	for {
		select {
		case <-w.stopped:
			return
		case <-w.wake:
		}

		for {
			payload, ok := w.takeLatest()
			if !ok {
				break
			}
			ctx, cancel := context.WithTimeout(w.ctx, agentStatusUpdateTimeout)
			err := w.updater(ctx, w.agentID, payload)
			cancel()
			if err == nil {
				backoff = 250 * time.Millisecond
				continue
			}
			fmt.Printf("agent status error for %s: %v\n", w.agentID, err)
			w.requeueIfStillCurrent(payload)
			if !w.waitRetryOrUpdate(backoff) {
				return
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}
}

func (w *agentStatusWorker) takeLatest() (updateAgentSessionRequest, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dirty {
		return updateAgentSessionRequest{}, false
	}
	payload := w.latest
	w.dirty = false
	return payload, true
}

func (w *agentStatusWorker) requeueIfStillCurrent(payload updateAgentSessionRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dirty {
		return
	}
	w.latest = payload
	w.dirty = true
}

func (w *agentStatusWorker) waitRetryOrUpdate(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-w.stopped:
		return false
	case <-w.wake:
		return true
	case <-timer.C:
		return true
	}
}

func newAgentSessionSupervisor(cfg Config, updater agentSessionUpdater, runtimes *runtimeRegistry) *agentSessionSupervisor {
	if runtimes == nil {
		runtimes = defaultRuntimeRegistry(cfg)
	}
	if updater == nil {
		updater = func(context.Context, string, updateAgentSessionRequest) error { return nil }
	}
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s := &agentSessionSupervisor{
		cfg:                cfg,
		status:             newAgentStatusSyncer(updater),
		runtimes:           runtimes,
		terminalExitReason: defaultTerminalExitReason,
		heartbeatInterval:  60 * time.Second,
		stallAfter:         15 * time.Minute,
		now:                time.Now,
		sessions:           map[string]*managedAgentSession{},
		starting:           map[string]*agentSessionStart{},
		restartAttempts:    map[string]runtimeRestartState{},
		baseCtx:            baseCtx,
		baseCancel:         baseCancel,
	}
	if cfg.RuntimeObserver != nil {
		s.observationWake = make(chan struct{}, 1)
		s.observationDone = make(chan struct{})
		go s.dispatchRuntimeObservations()
	}
	// The default restart delay is baseCtx-aware so a parked restart unblocks
	// promptly on Shutdown instead of lingering for the full capped backoff.
	s.restartSleep = func(d time.Duration) {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.baseCtx.Done():
		}
	}
	return s
}

func (s *agentSessionSupervisor) runtimeObservationLocked(session *managedAgentSession, state RuntimeObservationState) {
	if s == nil || s.cfg.RuntimeObserver == nil || session == nil || s.observationClosed {
		return
	}
	s.observationSequence++
	s.observationQueue = append(s.observationQueue, RuntimeObservation{
		Sequence:          s.observationSequence,
		RuntimeGeneration: session.runtimeGeneration,
		RuntimeKind:       session.runtimeKind,
		PID:               session.runtimePID,
		TurnSequence:      session.turnSequence,
		State:             state,
	})
	s.signalRuntimeObservationDispatcherLocked()
}

func (s *agentSessionSupervisor) turnStartedObservationLocked(session *managedAgentSession, turnID string) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" || session == nil || session.observedTurnID == turnID {
		return
	}
	session.turnSequence++
	session.observedTurnID = turnID
	s.runtimeObservationLocked(session, RuntimeObservationTurnStarted)
}

func (s *agentSessionSupervisor) turnTerminalObservationLocked(session *managedAgentSession, state RuntimeObservationState) {
	if session == nil || session.observedTurnID == "" {
		return
	}
	s.runtimeObservationLocked(session, state)
	session.observedTurnID = ""
}

func (s *agentSessionSupervisor) signalRuntimeObservationDispatcherLocked() {
	if s.observationWake == nil {
		return
	}
	select {
	case s.observationWake <- struct{}{}:
	default:
	}
}

func (s *agentSessionSupervisor) dispatchRuntimeObservations() {
	defer close(s.observationDone)
	for range s.observationWake {
		for {
			s.mu.Lock()
			if len(s.observationQueue) == 0 {
				closed := s.observationClosed
				s.mu.Unlock()
				if closed {
					return
				}
				break
			}
			observation := s.observationQueue[0]
			s.observationQueue[0] = RuntimeObservation{}
			s.observationQueue = s.observationQueue[1:]
			s.mu.Unlock()
			s.cfg.RuntimeObserver.ObserveRuntime(observation)
		}
	}
}

func (s *agentSessionSupervisor) closeRuntimeObservationDispatcher() {
	if s == nil || s.observationDone == nil {
		return
	}
	s.mu.Lock()
	s.observationClosed = true
	s.signalRuntimeObservationDispatcherLocked()
	s.mu.Unlock()
	<-s.observationDone
}

// bindServiceContext re-parents baseCtx onto the service run context exactly
// ONCE, at run() entry before any construction. A service-context cancellation
// then propagates to every detached construction even during the synchronous
// startup refresh (before Shutdown/baseCancel is reachable). It is a one-time
// lifecycle operation — never a mutable reparent after first use.
func (s *agentSessionSupervisor) bindServiceContext(ctx context.Context) {
	if ctx == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serviceBound {
		return
	}
	s.serviceBound = true
	previousCancel := s.baseCancel
	s.baseCtx, s.baseCancel = context.WithCancel(ctx)
	if previousCancel != nil {
		previousCancel() // release the throwaway Background-derived context
	}
}

func (s *agentSessionSupervisor) SetIdleWake(wake func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wakeAgent = wake
}

func (s *agentSessionSupervisor) Reconcile(ctx context.Context, agents []*agent) error {
	desired := map[string]*agent{}
	var errs []error
	for _, current := range agents {
		if current == nil {
			continue
		}
		desired[current.ID] = current
		if err := s.ensureSession(ctx, current); err != nil {
			fmt.Printf("agent session %s error: %v\n", current.ID, err)
			errs = append(errs, &agentSessionStartupError{
				AgentID: current.ID,
				Handle:  current.Handle,
				Err:     err,
			})
			// Publishing the construction failure caller-side is safe: the only
			// production Reconcile caller (Service.refresh) holds refreshMu across the
			// whole call, so authoritative Reconciles never overlap and this
			// disconnected cannot race a concurrent Reconcile's successful idle.
			status := "disconnected"
			activity := err.Error()
			s.mu.Lock()
			if failure := s.restartAttempts[current.ID]; failure.exhausted {
				status = "failed"
				activity = firstNonEmptyText(failure.lastFailure, activity)
			}
			s.mu.Unlock()
			s.publish(current.ID, updateAgentSessionRequest{
				Status:          status,
				CurrentActivity: activity,
				LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
	}
	stale := []*managedAgentSession{}
	s.mu.Lock()
	for agentID, session := range s.sessions {
		if _, ok := desired[agentID]; !ok {
			if !session.dead {
				session.expectedStopOwned = true
			}
			stale = append(stale, session)
			delete(s.sessions, agentID)
			// Explicit removal ends this agent's lifecycle: drop its transient
			// restart counter so a future re-add starts backoff from scratch, and
			// invalidate any in-flight parked restarter (its conditional swap now
			// fails because the token is gone). Cancel a blocked restart
			// construction so its process is reaped promptly, not after the CAS.
			delete(s.restartAttempts, agentID)
			if session.constructCancel != nil {
				session.constructCancel()
			}
		}
	}
	// Invalidate any in-flight FRESH construction for a removed agent: mark the
	// claim cancelled (the conditional publish then refuses to store it) and
	// cancel its context so a blocked Spawn/Start/handshake returns and reaps.
	for agentID, start := range s.starting {
		if _, ok := desired[agentID]; !ok {
			start.cancelled = true
			if start.cancel != nil {
				start.cancel()
			}
		}
	}
	if len(stale) != 0 {
		// Registered under the same lock Shutdown uses to close admission. If this
		// Add wins, Shutdown necessarily observes it before Wait; if Shutdown wins,
		// it has already emptied sessions and no later Reconcile can register work.
		s.removalWG.Add(1)
	}
	s.mu.Unlock()
	if len(stale) != 0 {
		for _, session := range stale {
			s.stopAndJoinRemovedSession(session)
		}
		s.removalWG.Done()
	}
	return errors.Join(errs...)
}

func (s *agentSessionSupervisor) Shutdown() {
	if s == nil {
		return
	}
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.shutdown = true
		// Cancel every in-flight construction (fresh and restart) AND every live
		// consumeEvents loop so a blocked Spawn/Start/provider-handshake or event
		// loop returns promptly and its process is reaped, rather than stalling
		// teardown until it completes on its own.
		s.baseCancel()
		sessions := make([]*managedAgentSession, 0, len(s.sessions))
		for _, session := range s.sessions {
			if !session.dead {
				session.expectedStopOwned = true
			}
			sessions = append(sessions, session)
		}
		s.sessions = map[string]*managedAgentSession{}
		s.mu.Unlock()
		for _, session := range sessions {
			s.stopAndJoinRemovedSession(session)
		}
		// A Reconcile that removed a generation immediately before shutdown owns
		// that generation's stop/join/terminal observation. Keep the dispatcher
		// open until every such owner has enqueued its final observation.
		s.removalWG.Wait()
		// Join every detached construction worker (fresh + restart) AND every live
		// consumeEvents loop before status teardown: baseCancel above makes a real
		// provider Start/handshake return and every event loop exit, so each drains,
		// the baseCtx.Err() publish fence reaps any in-flight process, and Shutdown
		// never returns while a worker or event loop can still publish — no
		// post-Shutdown zombie and no status write after teardown.
		s.constructionWG.Wait()
		s.closeRuntimeObservationDispatcher()
		if s.status != nil {
			s.status.Stop()
		}
	})
}

func (s *agentSessionSupervisor) stopAndJoinRemovedSession(session *managedAgentSession) {
	if session == nil {
		return
	}
	if session.process != nil {
		_ = session.process.Stop()
	}
	if session.lifecycleCancel != nil {
		session.lifecycleCancel()
	}
	waitForSessionLoop(session.eventLoopDone)
	waitForSessionLoop(session.heartbeatLoopDone)

	s.mu.Lock()
	if session.expectedStopOwned {
		session.expectedStopOwned = false
		s.runtimeObservationLocked(session, RuntimeObservationStoppedExpected)
	}
	s.mu.Unlock()
}

func waitForSessionLoop(done <-chan struct{}) {
	if done != nil {
		<-done
	}
}

// refreshDesiredSpec updates a session's desired agent spec to the latest and
// bumps its revision ONLY when a spawn-relevant field actually changed. An
// identical reconcile/notification must not advance the
// revision: a parked token under construction would otherwise be reaped and
// rebuilt on every touch, starving the restart under frequent reconciles or
// notifications (which also call ensureSession).
//
// The fingerprint must cover EVERY desired-spec field startSession's
// construction consumes, or a mid-construction change to an unfingerprinted
// field would publish a stale-config process (the same #15 failure via another
// field). Enumerating the current construction inputs from `current`:
//   - ID           -> workdir (agentWorkspacePath(cfg, ID)) + AgentID; it is the
//     session's identity / map key and cannot change in place.
//   - Kind            -> runtime driver selection.               [fingerprinted]
//   - Model           -> runtime profile model.                  [fingerprinted]
//   - ReasoningEffort -> runtime profile reasoning effort.       [fingerprinted]
//   - SystemPrompt    -> Spawn/resume/start-session Instructions. [fingerprinted]
//   - SessionID    -> resume continuity; for a restart it is taken from the
//     session, not the desired spec, so it is not a config change.
//
// Workdir is ID-derived (NOT WorkspaceRoot), ToolToken is generated, and env is
// built from cfg — none are desired-spec inputs.
func (session *managedAgentSession) refreshDesiredSpec(current *agent) (changed bool) {
	changed = session.agent == nil ||
		session.agent.Kind != current.Kind ||
		strings.TrimSpace(session.agent.Model) != strings.TrimSpace(current.Model) ||
		strings.TrimSpace(session.agent.ReasoningEffort) != strings.TrimSpace(current.ReasoningEffort) ||
		session.agent.SystemPrompt != current.SystemPrompt
	session.agent = cloneAgentValue(current)
	if changed {
		session.agentRev++
	}
	return changed
}

func runtimeProfileForAgent(current *agent) RuntimeProfile {
	if current == nil {
		return RuntimeProfile{}
	}
	return RuntimeProfile{
		Model:           strings.TrimSpace(current.Model),
		ReasoningEffort: strings.TrimSpace(current.ReasoningEffort),
	}
}

func explicitRuntimeProfile(profile RuntimeProfile) bool {
	return profile.Model != "" || profile.ReasoningEffort != ""
}

func runtimeProfileChanged(previous, current *agent) bool {
	return runtimeProfileForAgent(previous) != runtimeProfileForAgent(current)
}

// spawnFingerprintChanged reports whether the spawn-relevant desired spec differs
// from the latest spec this fresh-start claim is building.
//
// ASYMMETRY vs refreshDesiredSpec (restart): the FRESH construction reads
// current.SessionID to choose RuntimeInputResumeSession (resume) vs a fresh
// start, so SessionID is a spawn-relevant desired input here and MUST be in the
// fingerprint — a changed authoritative SessionID has to retarget the claim and
// rebuild the correct resume target. The restart path takes SessionID from the
// parked session (continuity), not the desired spec, so refreshDesiredSpec
// correctly omits it. Both cover Kind, Model, ReasoningEffort, and SystemPrompt;
// ID is the fixed identity, Workdir is ID-derived, ToolToken is generated.
func (start *agentSessionStart) spawnFingerprintChanged(current *agent) bool {
	return start.agent == nil ||
		start.agent.Kind != current.Kind ||
		strings.TrimSpace(start.agent.Model) != strings.TrimSpace(current.Model) ||
		strings.TrimSpace(start.agent.ReasoningEffort) != strings.TrimSpace(current.ReasoningEffort) ||
		start.agent.SystemPrompt != current.SystemPrompt ||
		// Normalized: startSession compares strings.TrimSpace(current.SessionID)
		// to decide resume vs start, so whitespace-only differences aren't a real
		// change and must not churn the construction.
		strings.TrimSpace(start.agent.SessionID) != strings.TrimSpace(current.SessionID)
}

// ensureSession is the AUTHORITATIVE entry point (Reconcile — the workspace
// refresh authority — and direct callers): it may advance the desired spec of a
// live session or an in-flight construction claim.
func (s *agentSessionSupervisor) ensureSession(ctx context.Context, current *agent) error {
	return s.ensureSessionDesired(ctx, current, true)
}

// ensureSessionDesired ensures a resident session for current. An authoritative
// caller may retarget the claim / refresh a live session's desired spec; a
// NON-authoritative caller (a notification or agent-worker cycle that may hold a
// stale s.latestWorkspace snapshot) only ensures + waits and must never roll the
// authoritative desired spec backward from its own snapshot (finding 29). Reconcile
// is the single desired-state authority; notifications/workers ensure but do not
// re-decide desired config.
func (s *agentSessionSupervisor) ensureSessionDesired(ctx context.Context, current *agent, authoritative bool) error {
	if current == nil || strings.TrimSpace(current.ID) == "" {
		return nil
	}
	for {
		s.mu.Lock()
		if s.shutdown {
			s.mu.Unlock()
			return nil
		}
		if session := s.sessions[current.ID]; session != nil {
			profileChanged := authoritative && runtimeProfileChanged(session.agent, current)
			if profileChanged {
				delete(s.restartAttempts, current.ID)
			}
			if restartGateClosed(session) {
				// A terminal `failed` generation (human-gated) or a `disconnected`
				// generation with an in-flight backoff park (owned by its delayed
				// restarter) must not be replaced by any other start path — that is
				// exactly the bypass that lets a reconcile/notification defeat the
				// cap or resurrect a terminal agent. Still refresh the desired spec
				// on the parked token (bumping its revision) so a pending respawn
				// uses the LATEST instructions, not the death-time clone, and an
				// in-flight construction against the old revision is rebuilt — but
				// only bump the revision on a real spec change (see refreshDesiredSpec).
				// Only an authoritative caller may advance the desired spec; a stale
				// notification/worker snapshot must not roll it back (finding 29).
				if authoritative && profileChanged && session.state == "failed" {
					session.refreshDesiredSpec(current)
					delete(s.sessions, current.ID)
					s.mu.Unlock()
					_ = session.process.Stop()
					continue
				}
				if authoritative && session.refreshDesiredSpec(current) && session.constructCancel != nil {
					// A genuine spec change with an in-flight restart construction:
					// cancel the blocked old-spec Spawn/Start/handshake now so the
					// restarter re-arms and rebuilds from the latest spec promptly,
					// instead of the rev-CAS only reaping after the handshake returns.
					session.constructCancel()
				}
				s.mu.Unlock()
				return nil
			}
			if session.state == "disconnected" {
				delete(s.sessions, current.ID)
				s.mu.Unlock()
				_ = session.process.Stop()
				continue
			}
			if authoritative {
				session.refreshDesiredSpec(current)
			}
			s.mu.Unlock()
			return nil
		}
		if start := s.starting[current.ID]; start != nil {
			// One claim-scoped worker owns the fresh construction (launched exactly
			// once at claim creation). Every caller — creator included — is a pure
			// WAITER: it selects the claim's `done` vs its own ctx and mutates no
			// claim state, so a caller leaving never stops a still-desired
			// construction. A changed desired fingerprint only RETARGETS the claim
			// (latest spec + rev) and cancels the current attempt; the SAME worker
			// rebuilds. There is no owner-departure to classify — findings 24/25
			// are structurally impossible.
			// Only an AUTHORITATIVE caller retargets; a non-authoritative caller
			// (stale snapshot) waits and must not roll the desired spec back (#29).
			if authoritative && start.spawnFingerprintChanged(current) {
				if runtimeProfileChanged(start.agent, current) {
					delete(s.restartAttempts, current.ID)
				}
				start.agent = cloneAgentValue(current)
				start.rev++
				if start.cancel != nil {
					start.cancel()
				}
			}
			done := start.done
			s.mu.Unlock()
			select {
			case <-done:
				s.mu.Lock()
				err := start.err
				s.mu.Unlock()
				// The claim completed — a published session, a soft disconnected
				// status (runtime unavailable), or a real construction failure.
				// Return its final result directly; never loop and re-create a claim
				// on a no-session soft outcome.
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// A NON-authoritative caller (stale snapshot) must not CREATE the resident
		// session from an empty slot — that would make it the desired-spec source
		// after a failed/soft authoritative outcome removed the claim (finding 30).
		// It may only wait on an existing claim / use an existing session; let
		// Reconcile (the authority) create.
		if !authoritative {
			s.mu.Unlock()
			return nil
		}
		// Admission fence (the B-specific guard): reject an ALREADY-cancelled caller
		// before it creates a claim. B decouples construction from the caller ctx,
		// so a claim admitted here would publish on baseCtx even though this caller
		// has departed — and if the caller was cancelled by authoritative removal
		// (syncAgentWorkers before Reconcile), nothing would cancel that new claim,
		// resurrecting the removed agent. A claim admitted BEFORE cancellation is
		// still caught by the following Reconcile; a call arriving AFTER cancellation
		// is refused here. Departure AFTER admission stays inert (the B invariant).
		if ctx.Err() != nil {
			s.mu.Unlock()
			return ctx.Err()
		}
		profile := runtimeProfileForAgent(current)
		if state, ok := s.restartAttempts[current.ID]; ok {
			if state.profile != profile {
				delete(s.restartAttempts, current.ID)
			} else if explicitRuntimeProfile(profile) {
				if state.exhausted {
					activity := firstNonEmptyText(state.lastFailure, "Runtime profile could not establish a session")
					s.publish(current.ID, updateAgentSessionRequest{
						Status:          "failed",
						CurrentTurnID:   "",
						CurrentActivity: activity,
						LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
					})
					s.mu.Unlock()
					return nil
				}
				if s.now().Before(state.nextAttempt) {
					s.mu.Unlock()
					return nil
				}
			}
		}
		start := &agentSessionStart{done: make(chan struct{}), agent: cloneAgentValue(current)}
		s.starting[current.ID] = start
		s.constructionWG.Add(1) // under s.mu, same side as the loop-top shutdown check
		s.mu.Unlock()
		go func() {
			defer s.constructionWG.Done()
			s.runFreshStart(current.ID, start)
		}()
		select {
		case <-start.done:
			s.mu.Lock()
			err := start.err
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runFreshStart is the single, claim-scoped, service-lifetime worker for a fresh
// construction — the fresh-path counterpart of the restart goroutine. It runs on
// baseCtx (service lifetime), never any caller's ctx, so a caller's departure
// never stops a still-desired construction. It reads the claim's latest spec +
// rev each attempt, publishes by CAS, and closes `done` exactly once with the
// final result. Retarget rebuilds from the latest; removal/Shutdown/service
// cancellation are terminal (never resurrect a removed or abandoned agent).
func (s *agentSessionSupervisor) runFreshStart(agentID string, start *agentSessionStart) {
	var err error
	for {
		s.mu.Lock()
		if s.shutdown || start.cancelled || s.baseCtx.Err() != nil {
			s.mu.Unlock()
			err = context.Canceled
			break
		}
		spec := cloneAgentValue(start.agent)
		rev := start.rev
		// Attempt ctx is baseCtx-derived (Shutdown / service context) plus the
		// per-attempt cancel used for removal/retarget. There is deliberately no
		// caller ctx here.
		cctx, ccancel := context.WithCancel(s.baseCtx)
		start.cancel = ccancel
		s.mu.Unlock()

		attemptErr := s.startSession(cctx, spec, nil, 0, start, rev)
		ccancel()

		s.mu.Lock()
		published := s.sessions[agentID] != nil
		terminal := s.shutdown || start.cancelled || s.baseCtx.Err() != nil || s.starting[agentID] != start
		revChanged := start.rev != rev
		s.mu.Unlock()
		if published {
			err = nil
			break
		}
		if terminal {
			err = attemptErr
			break
		}
		if revChanged {
			// The desired spec changed mid-construction; the stale-spec process was
			// reaped by the rev-CAS. Rebuild from the claim's latest spec.
			continue
		}
		// A genuine construction failure (Spawn/Start/handshake) — surfaces to a
		// waiting synchronous Reconcile as agentSessionStartupError.
		if attemptErr != nil {
			s.mu.Lock()
			s.recordConstructionFailureLocked(agentID, spec, attemptErr)
			s.mu.Unlock()
		}
		err = attemptErr
		break
	}
	s.mu.Lock()
	start.err = err
	if s.starting[agentID] == start {
		delete(s.starting, agentID)
	}
	close(start.done)
	s.mu.Unlock()
}

const explicitProfileConstructionAttemptCap = 3

func (s *agentSessionSupervisor) recordConstructionFailureLocked(agentID string, current *agent, err error) runtimeRestartState {
	state := s.nextRestartAttemptLocked(agentID, current)
	state.lastFailure = sanitizeExitLine(firstNonEmptyText(errorText(err), "Runtime profile could not establish a session"))
	if explicitRuntimeProfile(state.profile) && state.attempts >= explicitProfileConstructionAttemptCap {
		state.exhausted = true
	}
	s.restartAttempts[agentID] = state
	return state
}

func (s *agentSessionSupervisor) nextRestartAttemptLocked(agentID string, current *agent) runtimeRestartState {
	profile := runtimeProfileForAgent(current)
	state := s.restartAttempts[agentID]
	if state.profile != profile {
		state = runtimeRestartState{profile: profile}
	}
	state.attempts++
	state.nextAttempt = s.now().Add(restartBackoff(state.attempts))
	s.restartAttempts[agentID] = state
	return state
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// startSession spawns a runtime and publishes the session by an atomic
// conditional swap against expectPrior: it stores the new session only if the
// current map slot is still exactly expectPrior. A fresh start passes nil
// (slot must be absent); a restart passes the parked dead-generation session as
// the token (slot must still hold it). If a racing removal/replacement has
// changed the slot, the store is abandoned and the freshly spawned process is
// reaped by the deferred Stop — so a removed agent is never resurrected.
func (s *agentSessionSupervisor) startSession(ctx context.Context, current *agent, expectPrior *managedAgentSession, expectRev uint64, claim *agentSessionStart, claimRev uint64) error {
	workdir := agentWorkspacePath(s.cfg, current.ID)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	appendAgentLog(s.cfg, current.ID, "ensuring resident agent session agent=%s handle=%s", current.ID, current.Handle)
	driver, detection, ok := s.runtimes.Lookup(current.Kind)
	if !ok || !detection.Available {
		reason := strings.TrimSpace(detection.Reason)
		if reason == "" {
			reason = "runtime is unavailable"
		}
		activity := fmt.Sprintf("%s runtime unavailable: %s", firstNonEmptyText(strings.TrimSpace(current.Kind), "agent"), reason)
		appendAgentLog(s.cfg, current.ID, "runtime unavailable kind=%s reason=%s", current.Kind, reason)
		s.publish(current.ID, updateAgentSessionRequest{
			Status:          "disconnected",
			CurrentTurnID:   "",
			CurrentActivity: activity,
			LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		return nil
	}
	profile, profileErr := resolveRuntimeProfile(current, detection.ModelCatalog)
	if profileErr != nil {
		kind := firstNonEmptyText(strings.TrimSpace(current.Kind), "agent")
		activity := fmt.Sprintf("%s runtime profile unavailable: %s", kind, profileErr)
		appendAgentLog(s.cfg, current.ID, "runtime profile unavailable kind=%s model=%q effort=%q err=%v", current.Kind, current.Model, current.ReasoningEffort, profileErr)
		s.publish(current.ID, updateAgentSessionRequest{
			Status:          "disconnected",
			CurrentTurnID:   "",
			CurrentActivity: activity,
			LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		return nil
	}
	toolToken, err := newToolToken()
	if err != nil {
		return err
	}
	process, err := driver.Spawn(ctx, RuntimeSpawnSpec{
		AgentID:      current.ID,
		Workdir:      workdir,
		ToolToken:    toolToken,
		Instructions: current.SystemPrompt,
		Profile:      profile,
	})
	if err != nil {
		return err
	}
	// Arm the reap BEFORE Start: a Start/provider-handshake failure or a
	// cancellation must reap the freshly launched runtime, not just the
	// publish-CAS. started stays true on every exit except a successful publish.
	started := true
	defer func() {
		if started {
			_ = process.Stop()
		}
	}()
	if err := process.Start(ctx); err != nil {
		return err
	}
	sessionID := strings.TrimSpace(current.SessionID)
	if sessionID != "" {
		if _, err := process.WriteStdin(ctx, RuntimeInput{
			Kind:         RuntimeInputResumeSession,
			SessionID:    sessionID,
			CWD:          workdir,
			Instructions: current.SystemPrompt,
		}); err != nil {
			appendAgentLog(s.cfg, current.ID, "runtime session resume failed session=%s err=%v; starting new session", sessionID, err)
			sessionID = ""
		} else {
			appendAgentLog(s.cfg, current.ID, "runtime session resumed session=%s", sessionID)
		}
	}
	if sessionID == "" {
		result, err := process.WriteStdin(ctx, RuntimeInput{
			Kind:         RuntimeInputStartSession,
			CWD:          workdir,
			Instructions: current.SystemPrompt,
		})
		if err != nil {
			appendAgentLog(s.cfg, current.ID, "runtime session start failed err=%v", err)
			return err
		}
		sessionID = strings.TrimSpace(result.SessionID)
		appendAgentLog(s.cfg, current.ID, "runtime session started session=%s", sessionID)
	}
	session := &managedAgentSession{
		agent:           cloneAgentValue(current),
		process:         process,
		toolToken:       toolToken,
		workdir:         workdir,
		sessionID:       sessionID,
		state:           "idle",
		runtimeKind:     driver.Kind(),
		runtimePID:      process.PID(),
		lastActivityAt:  s.now(),
		lastActivitySeq: process.ActivitySeq(),
	}
	s.mu.Lock()
	if s.shutdown || s.baseCtx.Err() != nil {
		s.mu.Unlock()
		// Shutdown OR a service-context cancellation won the race after this process
		// spawned; abort the store so the deferred Stop() reaps it. baseCtx.Err() is
		// the mandatory fence for the service-cancel-before-explicit-Shutdown window
		// (findings 23-25): a cancellation-ignoring provider must never publish then.
		appendAgentLog(s.cfg, current.ID, "discarding runtime process because supervisor is shutting down or its service context was cancelled")
		return nil
	}
	if s.sessions[current.ID] != expectPrior {
		// The map slot is no longer the token we started against: a fresh start
		// found a duplicate, or a restart's parked generation was removed/replaced
		// (e.g. Reconcile deleted it) while we spawned. Abandon the store; the
		// deferred Stop() reaps this process instead of resurrecting a removed
		// agent or double-owning a live one.
		s.mu.Unlock()
		appendAgentLog(s.cfg, current.ID, "discarding runtime process because the session slot changed during (re)start")
		return nil
	}
	if expectPrior != nil && expectPrior.agentRev != expectRev {
		// Same token, but its desired spec was refreshed by a reconcile while we
		// were constructing. Publishing would ship a process built from stale
		// instructions/kind, so reap it and let the owning restarter rebuild from
		// the latest spec against the same token.
		s.mu.Unlock()
		appendAgentLog(s.cfg, current.ID, "discarding runtime process because the desired spec was refreshed during restart")
		return nil
	}
	if claim != nil && (s.starting[current.ID] != claim || claim.cancelled || claim.rev != claimRev) {
		// The fresh-start ownership claim was invalidated: the agent was removed
		// during construction (Reconcile cancelled the claim), or its desired spec
		// was retargeted (rev bumped) so this attempt built a now-stale fingerprint.
		// Abandon the store — a nil-slot fresh start must never resurrect a removed
		// agent nor publish a superseded old-spec runtime; the sole owner rebuilds.
		s.mu.Unlock()
		appendAgentLog(s.cfg, current.ID, "discarding runtime process because the fresh-start claim was invalidated (removed/retargeted)")
		return nil
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(s.baseCtx)
	session.lifecycleCancel = lifecycleCancel
	session.eventLoopDone = make(chan struct{})
	session.heartbeatLoopDone = make(chan struct{})
	s.runtimeGeneration++
	session.runtimeGeneration = s.runtimeGeneration
	s.sessions[current.ID] = session
	// Track the live event loop AND the heartbeat loop in the same barrier as
	// construction workers: both are added under s.mu on the passing side of the
	// shutdown fence above, so Shutdown (which sets s.shutdown and cancels baseCtx
	// under s.mu, then Waits) can never race these Adds, and both are drained before
	// status teardown — no publish after Stop (the #145 consumeEvents-join
	// invariant, now covering the heartbeat too).
	s.constructionWG.Add(2)
	started = false
	// Enqueue the initial idle UNDER s.mu, before exposing the session by unlocking:
	// the session is already in s.sessions, so a concurrent ScheduleNotificationTurn
	// can observe it and publish `working`. The initial idle must be ordered before
	// that (same locked side as the store) or a delayed initial idle could overwrite
	// an active turn's working in backend state (blocker 18). The enqueue is
	// nonblocking (separate status mutex), so holding s.mu is safe.
	if s.testHookInitialPublish != nil {
		s.testHookInitialPublish()
	}
	s.publish(current.ID, updateAgentSessionRequest{
		Status:          "idle",
		SessionID:       sessionID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	s.runtimeObservationLocked(session, RuntimeObservationReady)
	s.mu.Unlock()
	go func() {
		defer s.constructionWG.Done()
		defer close(session.eventLoopDone)
		s.consumeEvents(lifecycleCtx, current.ID, process)
	}()
	go func() {
		defer s.constructionWG.Done()
		defer close(session.heartbeatLoopDone)
		s.heartbeatLoop(lifecycleCtx, current.ID, process)
	}()
	return nil
}

func resolveRuntimeProfile(current *agent, catalog *RuntimeModelCatalog) (RuntimeProfile, error) {
	if current == nil {
		return RuntimeProfile{}, nil
	}
	modelName := strings.TrimSpace(current.Model)
	effort := strings.TrimSpace(current.ReasoningEffort)
	profile := RuntimeProfile{Model: modelName, ReasoningEffort: effort}
	if modelName == "" && effort == "" {
		return profile, nil
	}
	if catalog == nil {
		return RuntimeProfile{}, errors.New("model catalog is unavailable")
	}
	if strings.TrimSpace(catalog.Error) != "" {
		return RuntimeProfile{}, errors.New("model catalog probe failed")
	}

	var effective *RuntimeModel
	if modelName != "" {
		for i := range catalog.Models {
			if catalog.Models[i].Model == modelName {
				if effective != nil {
					return RuntimeProfile{}, fmt.Errorf("model %q is ambiguous in the runtime catalog", modelName)
				}
				effective = &catalog.Models[i]
			}
		}
		if effective == nil {
			return RuntimeProfile{}, fmt.Errorf("model %q is no longer available", modelName)
		}
	} else {
		ambiguousDefault := false
		for i := range catalog.Models {
			if catalog.Models[i].IsDefault {
				if effective != nil {
					ambiguousDefault = true
					break
				}
				effective = &catalog.Models[i]
			}
		}
		useDefaultEfforts := effective != nil && !ambiguousDefault && len(effective.ReasoningEfforts) > 0
		if !useDefaultEfforts {
			if effort != "" && len(catalog.ReasoningEfforts) > 0 {
				if runtimeReasoningEffortSupported(catalog.ReasoningEfforts, effort) {
					return profile, nil
				}
				return RuntimeProfile{}, fmt.Errorf("runtime default does not support reasoning effort %q", effort)
			}
			if effective == nil || ambiguousDefault {
				return RuntimeProfile{}, errors.New("runtime default model cannot be resolved")
			}
		}
	}

	if effort == "" {
		return profile, nil
	}
	efforts := effective.ReasoningEfforts
	if len(efforts) == 0 {
		efforts = catalog.ReasoningEfforts
	}
	if runtimeReasoningEffortSupported(efforts, effort) {
		return profile, nil
	}
	displayName := firstNonEmptyText(strings.TrimSpace(effective.DisplayName), effective.Model)
	if modelName == "" {
		return RuntimeProfile{}, fmt.Errorf("runtime default is now %s and does not support %q", displayName, effort)
	}
	return RuntimeProfile{}, fmt.Errorf("model %s does not support reasoning effort %q", displayName, effort)
}

func runtimeReasoningEffortSupported(efforts []string, want string) bool {
	for _, effort := range efforts {
		if effort == want {
			return true
		}
	}
	return false
}

func (s *agentSessionSupervisor) ScheduleNotificationTurn(ctx context.Context, current *agent, prompt string, forMeSig string, generalSig string) error {
	if current == nil || strings.TrimSpace(prompt) == "" {
		return nil
	}
	// A notification is NON-authoritative on desired spec: it may carry a stale
	// workspace snapshot, so it ensures + waits but must not retarget the claim /
	// refresh the live session's spec (finding 29). Reconcile is the authority.
	if err := s.ensureSessionDesired(ctx, current, false); err != nil {
		return err
	}
	s.mu.Lock()
	session := s.sessions[current.ID]
	if session == nil {
		s.mu.Unlock()
		return nil
	}
	if session.dead {
		// A dead generation (parked for restart, or a wedge already claimed for
		// replacement) never accepts a write regardless of its status label — the
		// parked restarter owns it. Authoritative on generation, not status, so a
		// buffered late lifecycle event that rewrote the label cannot open a write.
		s.mu.Unlock()
		return nil
	}
	forMeSig = strings.TrimSpace(forMeSig)
	generalSig = strings.TrimSpace(generalSig)
	hasForMe := forMeSig != ""
	hasGeneral := generalSig != ""
	if !hasForMe && !hasGeneral {
		s.mu.Unlock()
		return nil
	}
	if session.state == "stalled" {
		// `stalled` is a cached observation, not replacement authority. Re-evaluate
		// liveness at THIS decision point (not only at the next heartbeat tick): if
		// the runtime has emitted fresh frames since the stall, recover it to working
		// here and let the busy path handle this notification — never kill a process
		// whose activity generation already proves it is active again.
		if s.observeActivity(session, session.process) {
			// The activity generation advanced since the stall (a genuinely new frame),
			// so recover to working HERE, synchronously, and handle this notification on
			// the busy path — never killing a process the generation proves is active,
			// and without waiting for the next tick.
			session.state = "working"
			s.publish(current.ID, updateAgentSessionRequest{
				Status:          "working",
				SessionID:       session.sessionID,
				CurrentTurnID:   session.activeTurn,
				CurrentActivity: "Working",
				LastHeartbeatAt: s.now().UTC().Format(time.RFC3339Nano),
			})
			appendAgentLog(s.cfg, current.ID, "runtime recovered from stall at notification time: telemetry advanced past the watermark; handling as busy")
		}
	}
	if session.state == "working" {
		queuedFollowupForMe := false
		queuedFollowupGeneral := false
		if hasForMe && forMeSig != session.activeForMeSig {
			queuedFollowupForMe = session.followupForMeSig == ""
			session.followupForMeSig = forMeSig
		}
		if hasGeneral && generalSig != session.activeGeneralSig {
			queuedFollowupGeneral = session.followupGeneralSig == ""
			session.followupGeneralSig = generalSig
		}
		shouldSteer := hasForMe && session.activeTurn != "" && forMeSig != session.activeForMeSig && forMeSig != session.steeredForMeSig
		process := session.process
		sessionID := session.sessionID
		turnID := session.activeTurn
		agentID := current.ID
		if shouldSteer {
			session.steeredForMeSig = forMeSig
		}
		s.mu.Unlock()
		if shouldSteer {
			appendAgentLog(s.cfg, agentID, "steering active turn session=%s turn=%s reason=for_me_notification", sessionID, turnID)
			if _, err := process.WriteStdin(ctx, RuntimeInput{
				Kind:       RuntimeInputSteerTurn,
				Importance: RuntimeImportanceImportant,
				SessionID:  sessionID,
				TurnID:     turnID,
				Text:       forMeSteerMessage,
			}); err != nil {
				// Roll back the optimistic steer reservation ONLY when this exact failed
				// attempt is still the authoritative in-flight steer, so a retry of the same
				// for-me signature is allowed. The steer completion identity is the resident
				// live process + the exact active turn + the exact reserved signature.
				// turnOpSeq is deliberately NOT part of this fence: it is the StartTurn
				// completion token, and a legitimate same-turn TurnStarted(turnID) republished
				// while this SteerTurn was in flight advances turnOpSeq without superseding the
				// steer — markWorking assigns activeTurn=turnID and bumps the nonce with no
				// same-turn early-return — so a nonce equality guard would wrongly strand
				// steeredForMeSig and block the retry. writable() excludes a replaced/dead
				// generation; activeTurn==turnID excludes a settled/replaced/newer turn;
				// steeredForMeSig==forMeSig excludes a newer re-steer of a different signature.
				s.mu.Lock()
				if currentSession := s.sessions[agentID]; writable(currentSession, process) &&
					currentSession.activeTurn == turnID &&
					currentSession.steeredForMeSig == forMeSig {
					currentSession.steeredForMeSig = ""
				}
				s.mu.Unlock()
				appendAgentLog(s.cfg, agentID, "turn steer failed session=%s turn=%s err=%v", sessionID, turnID, err)
				if isNoActiveTurnToSteerError(err) {
					return nil
				}
				return err
			}
			return nil
		}
		if queuedFollowupForMe || queuedFollowupGeneral {
			appendAgentLog(s.cfg, agentID, "queued notification follow-up while busy for_me=%t general=%t", queuedFollowupForMe, queuedFollowupGeneral)
		}
		return nil
	}
	if session.state == "stalled" {
		// A wedged working turn. Replace it ONLY when work is genuinely blocked
		// behind it (an undelivered inbox signature) — a stall that might still
		// recover on its own must not be churned. When work IS pending, claim the
		// restart for this exact resident generation (false->true under s.mu) and
		// rebuild from the RESIDENT authoritative spec via the parked-token restarter
		// — a stale notification snapshot must not create/recreate a session (finding
		// #30), so we never recurse through notification delivery. Claiming before
		// Stop makes consumeEvents (which will see the wedge's Stop) defer to this as
		// the sole replacement owner. Do NOT mark the signature delivered: the next
		// anti-entropy worker cycle delivers it once into the replacement.
		// Policy (b), queued-behind only: replacement authority comes ONLY from work
		// genuinely QUEUED BEHIND the wedge — a signature component that differs from
		// both the in-flight (active) AND the delivered signature. The stalled turn's
		// own unchanged active signature is the in-flight work, not pending behind
		// itself, so it never confers replacement authority; the wedge stays visibly
		// `stalled` (human-facing diagnostic) until genuinely-new work is blocked.
		queuedForMe := hasForMe && forMeSig != session.activeForMeSig && forMeSig != session.deliveredForMeSig
		queuedGeneral := hasGeneral && generalSig != session.activeGeneralSig && generalSig != session.deliveredGeneralSig
		if !(queuedForMe || queuedGeneral) || session.restartPending || session.dead {
			s.mu.Unlock()
			return nil
		}
		process := session.process
		sessionID := session.sessionID
		agentID := current.ID
		session.dead = true // old tool token invalid immediately
		session.state = "disconnected"
		session.activeTurn = ""
		session.activeForMeSig = ""
		session.activeGeneralSig = ""
		session.steeredForMeSig = ""
		session.turnOpSeq++
		session.restartPending = true // claim: consumeEvents must not launch a 2nd restarter
		// Publish the disconnected transition UNDER s.mu (blocker 14) so a delayed
		// heartbeat can't land a stale working/stalled status after this claim.
		s.publish(agentID, updateAgentSessionRequest{
			Status:          "disconnected",
			CurrentTurnID:   "",
			CurrentActivity: "Disconnected",
			LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		s.mu.Unlock()
		appendAgentLog(s.cfg, agentID, "stalled runtime with pending work; replacing session=%s", sessionID)
		// Immediate replacement (delay 0): the worker Stops the wedged process and
		// rebuilds from the resident spec on baseCtx, joined by constructionWG.
		s.launchRestartWorker(agentID, process, 0)
		return nil
	}
	if session.state == "disconnected" || session.state == "failed" {
		// A dead generation — whether backing off (disconnected) or terminally
		// failed — must never be written to. `failed` in particular fell through
		// here before and got a StartTurn written to the corpse (then demoted
		// itself to idle on the error), breaking terminal/no-restart stability.
		s.mu.Unlock()
		return nil
	}
	if (!hasForMe || forMeSig == session.deliveredForMeSig) && (!hasGeneral || generalSig == session.deliveredGeneralSig) {
		agentID := current.ID
		s.mu.Unlock()
		appendAgentLog(s.cfg, agentID, "skipping unchanged notification inbox signatures for_me=%t general=%t", hasForMe, hasGeneral)
		return nil
	}
	if until := session.turnBackoffUntil; !until.IsZero() && s.now().Before(until) {
		agentID := current.ID
		s.mu.Unlock()
		appendAgentLog(s.cfg, agentID, "turn-start admission backoff until %s; deferring", until.Format(time.RFC3339))
		return nil
	}
	// This write is a new authoritative operation: bump the nonce and capture it,
	// so its own completion can detect whether any later transition (a settled
	// turn, death, or a newer notification) superseded it while s.mu was dropped.
	session.turnOpSeq++
	op := session.turnOpSeq
	session.state = "working"
	// Starting a turn is proof of liveness and resets the stall window: refresh the
	// floor BEFORE dropping s.mu so a heartbeat tick during the in-flight
	// WriteStdin(StartTurn) — before the provider's first frame — never judges this
	// freshly started turn against the prior idle timestamp. This path bypasses
	// markWorking, so it must refresh the floor itself.
	s.resetLivenessFloor(session, session.process)
	session.activeForMeSig = forMeSig
	session.activeGeneralSig = generalSig
	session.steeredForMeSig = ""
	if session.followupForMeSig == forMeSig {
		session.followupForMeSig = ""
	}
	if session.followupGeneralSig == generalSig {
		session.followupGeneralSig = ""
	}
	process := session.process
	sessionID := session.sessionID
	workdir := session.workdir
	agentID := current.ID
	s.mu.Unlock()

	result, err := process.WriteStdin(ctx, RuntimeInput{
		Kind:       RuntimeInputStartTurn,
		Importance: RuntimeImportanceNormal,
		SessionID:  sessionID,
		CWD:        workdir,
		Text:       prompt,
	})
	if err != nil {
		s.mu.Lock()
		// Fence the completion to the exact generation we wrote to, and never
		// clobber a terminal/parked state: if this session was replaced or its
		// generation died (disconnected/failed) while WriteStdin was in flight,
		// demoting it to idle would strand a gated respawn or revive a corpse for
		// later notifications.
		if currentSession := s.sessions[current.ID]; writable(currentSession, process) && currentSession.turnOpSeq == op {
			currentSession.state = "idle"
			currentSession.activeTurn = ""
			currentSession.activeForMeSig = ""
			currentSession.activeGeneralSig = ""
			s.resetLivenessFloor(currentSession, process)
			// Publish the idle demotion UNDER s.mu (blocker 13): a heartbeat may have
			// published provisional `working` before this RPC error returned, so the
			// nonce-fenced idle must enqueue in the same ordered decision or the backend
			// stays visibly working forever. A stale/superseded error enqueues nothing.
			s.publish(current.ID, updateAgentSessionRequest{
				Status:          "idle",
				SessionID:       currentSession.sessionID,
				CurrentTurnID:   "",
				CurrentActivity: "Idle",
				LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
		var rpcErr *appServerRPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32001 {
			if currentSession := s.sessions[current.ID]; writable(currentSession, process) && currentSession.turnOpSeq == op {
				currentSession.turnStartAttempts++
				delay := restartBackoff(currentSession.turnStartAttempts)
				currentSession.turnBackoffUntil = s.now().Add(delay)
				appendAgentLog(s.cfg, agentID, "turn-start admission rejected (-32001) attempt=%d backoff=%s", currentSession.turnStartAttempts, delay)
				wake := s.wakeAgent
				sleep := s.restartSleep
				ctx := s.baseCtx
				s.mu.Unlock()
				go func() {
					sleep(delay)
					if ctx.Err() != nil {
						return
					}
					if wake != nil {
						wake(agentID)
					}
				}()
				return nil
			}
		}
		s.mu.Unlock()
		appendAgentLog(s.cfg, agentID, "turn start failed session=%s err=%v", sessionID, err)
		return err
	}
	turnID := strings.TrimSpace(result.TurnID)
	s.mu.Lock()
	currentSession := s.sessions[current.ID]
	// Fence on BOTH the generation (process) and the operation nonce (turnOpSeq):
	// a settled turn / death / newer notification that landed during the unlocked
	// WriteStdin bumped the nonce, so this completion must not revive working.
	stale := !writable(currentSession, process) || currentSession.turnOpSeq != op
	if !stale {
		currentSession.activeTurn = turnID
		currentSession.state = "working"
		currentSession.turnStartAttempts = 0
		currentSession.turnBackoffUntil = time.Time{}
		// The accepted turn is now genuinely underway: refresh the floor again so the
		// live turn gets a full stall window measured from acceptance, not from the
		// pre-write decision point.
		s.resetLivenessFloor(currentSession, process)
		// Publish UNDER s.mu (see markWorking/markIdle): the turnOpSeq fence guards the
		// state mutation, but the STATUS enqueue must share the same ordering, or an
		// authoritative idle that drained during the unlocked WriteStdin could publish
		// first and this stale `working` land last.
		s.publish(current.ID, updateAgentSessionRequest{
			Status:          "working",
			SessionID:       sessionID,
			CurrentTurnID:   turnID,
			CurrentActivity: "Working",
			LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		s.turnStartedObservationLocked(currentSession, turnID)
	}
	s.mu.Unlock()
	if stale {
		// The generation we wrote to was replaced or died mid-write; do not mark a
		// different or dead session working, nor publish a stale working status.
		appendAgentLog(s.cfg, agentID, "turn start completed on a stale generation session=%s; dropping status", sessionID)
		return nil
	}
	appendAgentLog(s.cfg, agentID, "turn started session=%s turn=%s for_me=%t general=%t", sessionID, turnID, hasForMe, hasGeneral)
	return nil
}

func (s *agentSessionSupervisor) Pending(agentID string) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[agentID]
	if session == nil {
		return false, false
	}
	return session.followupForMeSig != "", session.followupGeneralSig != ""
}

func (s *agentSessionSupervisor) agentByToolToken(token string) *agentRun {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		if session.toolToken == token && session.agent != nil && !session.dead {
			// A dead process generation's tool token is invalid the instant that
			// generation exits — a surviving child holding NOTTY_AGENT_TOOL_TOKEN
			// must not keep mutating workspace data during the parked-backoff
			// window (disconnected) or indefinitely (failed). Keyed on generation
			// liveness, not a status label, so a live-but-idle/stalled runtime
			// stays authorized while only genuinely dead generations are rejected.
			return &agentRun{
				ID:            "session_" + session.agent.ID,
				AgentID:       session.agent.ID,
				AgentHandle:   session.agent.Handle,
				AgentName:     session.agent.Name,
				AgentKind:     session.agent.Kind,
				WorkspaceRoot: session.agent.WorkspaceRoot,
				WorkingDir:    ".",
				Status:        session.state,
			}
		}
	}
	return nil
}

func (s *agentSessionSupervisor) consumeEvents(ctx context.Context, agentID string, process RuntimeProcess) {
	events := process.Events()
	for {
		var event RuntimeEvent
		var ok bool
		select {
		case <-ctx.Done():
			return
		case event, ok = <-events:
			if !ok {
				goto disconnected
			}
		}
		switch event.Kind {
		case RuntimeEventTurnStarted:
			if strings.TrimSpace(event.TurnID) != "" {
				s.markWorking(agentID, process, event.TurnID)
			}
		case RuntimeEventTurnCompleted:
			s.markIdleObserved(agentID, process, true, RuntimeObservationTurnCompleted)
			// A completed turn is the only proof of real uptime — the process ran
			// actual work, not just respawned. It alone resets the transient
			// backoff counter (never a failed turn or a bare idle/status event,
			// which would let a crash->idle->crash agent hot-loop at the floor).
			s.resetRestartBackoff(agentID, process)
		case RuntimeEventTurnFailed:
			if event.FailureKind == RuntimeFailureTerminalProfile && s.markTerminalProfileFailure(agentID, process, event.Error) {
				// The typed provider frame is authoritative. Stop the process so its
				// later generic result/404/exit cannot run another turn; the stored
				// terminal reason dominates the expected exit caused by this Stop.
				_ = process.Stop()
			} else {
				s.markIdleObserved(agentID, process, false, RuntimeObservationTurnFailed)
			}
		case RuntimeEventIdle:
			s.markIdleObserved(agentID, process, true, RuntimeObservationTurnIdle)
		}
	}
disconnected:
	// Classify the death from the exit forensics (facet 2), recorded before the
	// events channel closed. Default is transient: only an explicit positive
	// signal moves an exit to Expected (deliberate Stop) or terminal (a
	// CLI-proven, non-self-recovering provider failure) — a rate-limit/429 or any
	// unrecognized exit stays transient so we never one-way-door a recoverable
	// agent.
	exit := process.ExitInfo()
	terminalReason := ""
	s.mu.Lock()
	if session := s.sessions[agentID]; session != nil && session.process == process {
		terminalReason = session.terminalProfileReason
	}
	s.mu.Unlock()
	if terminalReason == "" && !exit.Expected {
		terminalReason = s.terminalExitReason(exit)
	}
	transient := !exit.Expected && terminalReason == ""
	stopState := RuntimeObservationStoppedExpected
	if terminalReason != "" {
		stopState = RuntimeObservationStoppedTerminal
	} else if transient {
		stopState = RuntimeObservationStoppedTransient
	}

	published := ""
	if terminalReason != "" {
		// Sanitize/bound the provider's line at the PUBLICATION boundary (persisted as
		// CurrentActivity and shown to humans): control chars and unbounded length
		// must never reach the backend. Rune-safe truncation.
		published = sanitizeExitLine(terminalReason)
	}
	owned := false
	attempt := 0
	claimedRestart := false
	s.mu.Lock()
	if session := s.sessions[agentID]; session != nil && session.process == process {
		owned = true
		// This exact process generation has died. Mark it dead so every start,
		// write, and authorization path treats it as a corpse — its tool token is
		// no longer valid regardless of how long the (disconnected/failed) session
		// entry is retained. Bump the operation nonce so any StartTurn completion
		// still in flight against this generation becomes a no-op.
		session.dead = true
		session.turnOpSeq++
		if terminalReason != "" {
			session.state = "failed"
		} else {
			session.state = "disconnected"
		}
		session.activeTurn = ""
		session.activeForMeSig = ""
		session.activeGeneralSig = ""
		session.steeredForMeSig = ""
		if transient && !session.restartPending {
			// Claim the restart for this dead generation exactly once. If the
			// stall-replacement path (ScheduleNotificationTurn) already claimed it —
			// restartPending is set — consumeEvents must NOT launch a second
			// restarter; that path is the sole replacement owner. This CAS is what
			// makes a concurrent natural-exit-vs-stall race resolve to one restarter.
			attempt = s.nextRestartAttemptLocked(agentID, session.agent).attempts
			// A backoff restart now owns this dead generation: the parked session
			// stays authoritative in the map (no delete) until the restarter
			// publishes the replacement by a conditional swap against it, so no
			// other start path may replace it and a racing removal invalidates it.
			session.restartPending = true
			claimedRestart = true
		}
		// Publish the death status UNDER s.mu (blocker 14): the death is a resident-
		// session state mutation with a status payload, so it must enqueue in the same
		// ordered decision or a delayed heartbeat/stall publish could land after and
		// revive a dead generation in backend state. Expected stop publishes nothing
		// (clean teardown); terminal publishes failed; a transient death WE claimed
		// publishes disconnected (a stall-replacement owner publishes its own).
		if terminalReason != "" {
			s.publish(agentID, updateAgentSessionRequest{
				Status:          "failed",
				CurrentTurnID:   "",
				CurrentActivity: published,
				LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
		} else if !exit.Expected {
			if claimedRestart {
				s.publish(agentID, updateAgentSessionRequest{
					Status:          "disconnected",
					CurrentTurnID:   "",
					CurrentActivity: "Disconnected",
					LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
				})
			}
		}
		s.runtimeObservationLocked(session, stopState)
	}
	s.mu.Unlock()
	if !owned {
		// The events belong to a process this session has already replaced.
		return
	}

	// Expected: the daemon deliberately Stop()ped this process — clean teardown,
	// no restart, no status noise.
	// Terminal: a CLI-proven, non-self-recovering provider failure. Surface it as
	// `failed` with the provider's own (sanitized) line; no restart until a human
	// acts. The match set is locked only from live-probe evidence.
	if terminalReason != "" {
		// Status was published under s.mu above (blocker 14); here just log + notify.
		appendAgentLog(s.cfg, agentID, "runtime terminal failure; no restart: %s", published)
		if s.testHookDeathHandled != nil {
			s.testHookDeathHandled("terminal")
		}
		return
	}

	if exit.Expected {
		appendAgentLog(s.cfg, agentID, "runtime stopped as expected; no restart")
		if s.testHookDeathHandled != nil {
			s.testHookDeathHandled("expected")
		}
		return
	}

	// Transient: an unexpected crash (or self-clearing throttle). If the
	// stall-replacement path already claimed this generation's restart (above), it
	// is the sole replacement owner and owns the status — consumeEvents must not
	// re-publish a (now stale) disconnected status nor launch a second restarter.
	if !claimedRestart {
		if s.testHookDeathHandled != nil {
			s.testHookDeathHandled("transient")
		}
		return
	}
	delay := restartBackoff(attempt)
	// Disconnected status was published under s.mu above (blocker 14); launch the
	// restarter for the generation we claimed.
	appendAgentLog(s.cfg, agentID, "runtime event stream closed; marking session disconnected%s; restart attempt=%d backoff=%s", crashSuffix(exit), attempt, delay)
	s.launchRestartWorker(agentID, process, delay)
	if s.testHookDeathHandled != nil {
		s.testHookDeathHandled("transient")
	}
}

// launchRestartWorker starts exactly one detached restart worker for a dead
// generation whose restartPending the caller has already CLAIMED under s.mu
// (false->true on the resident session). It adds to constructionWG under s.mu on
// the passing side of the shutdown/base-context fence — so Shutdown's Wait cannot
// race the Add and a cancelled baseCtx never spins restarts — and does not launch
// if that fence has closed. The worker owns EVERY outcome against its one parked
// token (publish/fail/refresh/remove/shutdown), rebuilding from the resident
// session.agent + revision each attempt with a capped backoff; a construction
// failure re-arms instead of stranding the token restartPending forever.
func (s *agentSessionSupervisor) launchRestartWorker(agentID string, process RuntimeProcess, delay time.Duration) {
	s.mu.Lock()
	launch := !s.shutdown && s.baseCtx.Err() == nil
	if launch {
		s.constructionWG.Add(1)
	}
	s.mu.Unlock()
	if !launch {
		return
	}
	if s.testHookRestartLaunched != nil {
		s.testHookRestartLaunched()
	}
	go func() {
		defer s.constructionWG.Done()
		if s.testHookRestartComplete != nil {
			defer s.testHookRestartComplete()
		}
		for {
			s.restartSleep(delay)
			s.mu.Lock()
			session := s.sessions[agentID]
			if s.shutdown || s.baseCtx.Err() != nil || session == nil || session.process != process || !session.restartPending {
				// Removed / replaced / shutdown / base-context cancelled / no longer
				// ours — cancel. Checking baseCtx here stops the spin when the service
				// run context is cancelled before Shutdown sets s.shutdown: without it
				// restartSleep returns immediately (baseCtx-aware) and each iteration
				// would spawn-then-reap a process against the cancelled context.
				s.mu.Unlock()
				return
			}
			// Read the LATEST spec + its revision off the still-authoritative parked
			// token; the swap requires the revision unchanged at publication.
			spec := cloneAgentValue(session.agent)
			if spec == nil {
				s.mu.Unlock()
				return
			}
			spec.SessionID = session.sessionID
			rev := session.agentRev
			// A cancelable construction context (child of baseCtx) stored on the
			// token: Reconcile removal / Shutdown fire it to interrupt a blocked
			// Spawn/Start/handshake and reap promptly, not after the CAS.
			cctx, ccancel := context.WithCancel(s.baseCtx)
			session.constructCancel = ccancel
			s.mu.Unlock()

			_ = process.Stop() // idempotent; the old generation is already dead
			err := s.startSession(cctx, spec, session, rev, nil, 0)
			ccancel()
			if err != nil {
				appendAgentLog(s.cfg, agentID, "session restart construction failed err=%v", err)
			}

			s.mu.Lock()
			if session.constructCancel != nil {
				session.constructCancel = nil
			}
			cur := s.sessions[agentID]
			if cur != session {
				// The parked token left the map: startSession published the
				// replacement (cur is the new session) or a removal/replacement took
				// over. Either way our job is done.
				s.mu.Unlock()
				return
			}
			// Token still present: nothing was published (construction failure or a
			// mid-construction spec refresh reaped it). Re-arm the next capped
			// attempt against the same token, unless shutdown/base-context
			// cancellation/removal intervened.
			if s.shutdown || s.baseCtx.Err() != nil || !session.restartPending {
				s.mu.Unlock()
				return
			}
			if session.agentRev != rev {
				// An authoritative spec refresh invalidated this construction.
				// Rebuild immediately from the new revision; profile changes have
				// already reset the shared ledger in ensureSessionDesired.
				delay = 0
				s.mu.Unlock()
				continue
			}
			var state runtimeRestartState
			if err != nil {
				state = s.recordConstructionFailureLocked(agentID, session.agent, err)
			} else {
				state = s.nextRestartAttemptLocked(agentID, session.agent)
			}
			if state.exhausted {
				session.restartPending = false
				session.state = "failed"
				activity := firstNonEmptyText(state.lastFailure, "Runtime profile could not establish a session")
				s.publish(agentID, updateAgentSessionRequest{
					Status:          "failed",
					CurrentTurnID:   "",
					CurrentActivity: activity,
					LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
				})
				s.mu.Unlock()
				return
			}
			delay = restartBackoff(state.attempts)
			s.mu.Unlock()
		}
	}()
}

const maxExitLineLen = 200

const (
	restartBackoffBase = 2 * time.Second
	restartBackoffCap  = 60 * time.Second
)

// restartBackoff is the delay before the Nth consecutive transient restart:
// restartBackoffBase doubled per prior attempt, capped at restartBackoffCap. A
// persistent crash therefore backs off to one attempt per minute instead of a
// 0.5 Hz spin.
func restartBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := restartBackoffBase
	for i := 1; i < attempt; i++ {
		if d >= restartBackoffCap {
			return restartBackoffCap
		}
		d *= 2
	}
	if d > restartBackoffCap {
		return restartBackoffCap
	}
	return d
}

// terminalExitReason returns a sanitized, human-facing reason when a process
// exit matches a CLI-PROVEN, non-self-recovering provider failure (exhausted
// plan/quota, deprecated/removed model, invalid auth) — the only conditions
// that should stop restarts. It returns "" for everything else, INCLUDING
// self-clearing rate-limits/429s, so the caller defaults those to the
// transient/backoff path and never one-way-doors a recoverable agent.
//
// The match set is deliberately empty until the live-CLI probe proves which
// exit code / stderr wording each provider actually emits (the current-CLI bar
// from item #1): a substring guessed from a report can mis-classify a
// recoverable exit as permanent. When the probe locks conditions, prefer a
// structured signal (exit code) and use stderr substrings only where no
// structured signal exists. This helper is shared with the turn-loop 429 item
// (task #8), so its contract — positive-match-only, transient by default —
// must hold for both callers.
// defaultTerminalExitReason is the production classifier. The set is EMPTY:
// the live-CLI probe (item #3) found no distinctive, structured, reachable-at-
// death terminal signal in current claude/codex — a model/auth error prints to
// stdout with a generic exit code and most likely leaves the process alive
// (task #8 turn-loop, not a death consumeEvents sees). Per the confirmed
// contract, unproven → transient/backoff every time; a false terminal is the
// one-way door we refuse. A distinctive+structured+reachable signal, if a future
// probe proves one, becomes the first entry via a follow-up PR.
func defaultTerminalExitReason(exit RuntimeExitInfo) string {
	_ = exit
	return ""
}

// crashSuffix renders a short, bounded forensic suffix for the disconnected log
// line so a transient crash is not silent: exit code/signal plus the last
// stderr line, sanitized and length-capped.
func crashSuffix(exit RuntimeExitInfo) string {
	var parts []string
	if exit.Signal != "" {
		parts = append(parts, "signal="+exit.Signal)
	} else if exit.ExitCode >= 0 {
		parts = append(parts, fmt.Sprintf("code=%d", exit.ExitCode))
	}
	if last := lastNonEmpty(exit.Stderr); last != "" {
		parts = append(parts, "stderr="+sanitizeExitLine(last))
	} else if exit.Err != "" {
		parts = append(parts, "err="+sanitizeExitLine(exit.Err))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// sanitizeExitLine bounds and cleans a provider line before it reaches a log or
// a published status: collapse whitespace/control chars to spaces, drop other
// control bytes, and length-cap. It keeps arbitrary terminal output from
// flowing into the UI; the stderr ring must never carry credentials.
func sanitizeExitLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20:
			return -1
		default:
			return r
		}
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > maxExitLineLen {
		// Back up to a rune boundary so the byte cap never splits a multibyte
		// character (which would emit an invalid-UTF-8 tail into the status/log).
		cut := maxExitLineLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}

// restartGateClosed reports whether automatic lifecycle actions — respawning a
// dead runtime or writing a new turn to it — must be withheld for this session.
// It is the single authority every start path consults: a terminal `failed`
// exit is human-gated, and a `disconnected` generation with an in-flight backoff
// park is owned by its delayed restarter, which alone may replace it.
// writable is the single writability authority for a session generation: a
// lifecycle transition or a write may proceed only for the resident, live (not
// dead/claimed) generation matching process. Every lifecycle mutator and the
// notification write path routes through this one predicate, so a dead generation
// can never have its status/turn rewritten or receive a write regardless of a
// stale status label a buffered late event may have left (blockers 1/3) — and a
// future mutator that consults it cannot regress the class.
func writable(session *managedAgentSession, process RuntimeProcess) bool {
	return session != nil && session.process == process && !session.dead
}

func restartGateClosed(session *managedAgentSession) bool {
	if session == nil {
		return false
	}
	if session.state == "failed" {
		return true
	}
	// Authoritative on generation, not the mutable status label: a dead generation
	// with a parked restart is owned by its restarter regardless of a status a
	// buffered late lifecycle event may have rewritten. (restartPending is only set
	// on a generation already marked dead, so this also covers the disconnected+
	// restartPending park.)
	return session.dead && session.restartPending
}

// resetRestartBackoff clears the transient-restart counter for an agent, but
// only for the exact live process generation that proved uptime — the guard
// mirrors markIdle's identity check so a stale completion from a replaced
// generation cannot reset a newer generation's backoff.
func (s *agentSessionSupervisor) resetRestartBackoff(agentID string, process RuntimeProcess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[agentID]; session != nil && session.process == process {
		delete(s.restartAttempts, agentID)
	}
}

func lastNonEmpty(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func (s *agentSessionSupervisor) markWorking(agentID string, process RuntimeProcess, turnID string) {
	s.mu.Lock()
	session := s.sessions[agentID]
	if !writable(session, process) {
		// Not the resident live generation (nil / replaced / dead): a buffered event
		// emitted before Stop closed the stream must not overwrite a claimed
		// disconnected/parked corpse back to working and publish a stale status.
		s.mu.Unlock()
		return
	}
	session.state = "working"
	session.activeTurn = turnID
	// A lifecycle transition is itself proof of liveness: refresh the floor so a
	// just-started turn has the full stallAfter window before it could be judged
	// silent, and so it clears any prior `stalled` marking (event-driven recovery).
	s.resetLivenessFloor(session, process)
	// An authoritative TurnStarted is a state transition that must win over a
	// StartTurn RPC completion still in flight: bump the operation nonce so a stale
	// StartTurn error/timeout returning afterward cannot demote this live turn.
	session.turnOpSeq++
	session.turnStartAttempts = 0
	session.turnBackoffUntil = time.Time{}
	sessionID := session.sessionID
	// Publish UNDER s.mu so the state decision and its status enqueue are ordered
	// together: a concurrent transition (or a stale RPC completion) cannot land its
	// publish between this decision and enqueue and leave a stale status last.
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "working",
		SessionID:       sessionID,
		CurrentTurnID:   turnID,
		CurrentActivity: "Working",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	s.turnStartedObservationLocked(session, turnID)
	s.mu.Unlock()
	appendAgentLog(s.cfg, agentID, "event turn started turn=%s", turnID)
}

func (s *agentSessionSupervisor) markIdle(agentID string, process RuntimeProcess, delivered bool) {
	s.markIdleObserved(agentID, process, delivered, RuntimeObservationTurnIdle)
}

func (s *agentSessionSupervisor) markTerminalProfileFailure(agentID string, process RuntimeProcess, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[agentID]
	// Defense in depth: a driver may only classify this failure when it launched
	// an explicit model, and the supervisor independently verifies that the
	// authoritative desired profile still names one. Inherited/default models
	// never become human-gated from a provider's model_not_found frame.
	if !writable(session, process) || session.agent == nil || strings.TrimSpace(session.agent.Model) == "" {
		return false
	}
	published := sanitizeExitLine(firstNonEmptyText(reason, "Claude rejected the selected model"))
	session.terminalProfileReason = published
	session.dead = true
	session.restartPending = false
	session.state = "failed"
	session.activeTurn = ""
	session.activeForMeSig = ""
	session.activeGeneralSig = ""
	session.steeredForMeSig = ""
	session.turnOpSeq++
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "failed",
		SessionID:       session.sessionID,
		CurrentTurnID:   "",
		CurrentActivity: published,
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	s.runtimeObservationLocked(session, RuntimeObservationTurnFailed)
	return true
}

func (s *agentSessionSupervisor) markIdleObserved(agentID string, process RuntimeProcess, delivered bool, observationState RuntimeObservationState) {
	s.mu.Lock()
	session := s.sessions[agentID]
	if !writable(session, process) {
		// Not the resident live generation (see markWorking): a late idle event must
		// not resurrect a claimed corpse to `idle` and let the next notification fall
		// through to a WriteStdin on the dead process.
		s.mu.Unlock()
		return
	}
	session.state = "idle"
	session.activeTurn = ""
	// A lifecycle transition proves liveness and clears any `stalled` marking
	// (event-driven recovery).
	s.resetLivenessFloor(session, process)
	// This idle/failed/completed transition is authoritative: bump the operation
	// nonce so a StartTurn completion still in flight against this generation
	// cannot revive it to working.
	session.turnOpSeq++
	// NOTE: the transient-restart backoff counter is reset only by
	// resetRestartBackoff on a proven RuntimeEventTurnCompleted — never here.
	// markIdle also fires for TurnFailed and bare idle/status events, none of
	// which prove uptime; resetting on them would defeat the backoff.
	if delivered && session.activeForMeSig != "" {
		session.deliveredForMeSig = session.activeForMeSig
	}
	if delivered && session.activeGeneralSig != "" {
		session.deliveredGeneralSig = session.activeGeneralSig
	}
	session.activeForMeSig = ""
	session.activeGeneralSig = ""
	session.steeredForMeSig = ""
	sessionID := session.sessionID
	// Publish UNDER s.mu (see markWorking): the idle decision and its status enqueue
	// are ordered together so a stale in-flight StartTurn completion cannot land a
	// `working` publish after this authoritative idle.
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "idle",
		SessionID:       sessionID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	s.turnTerminalObservationLocked(session, observationState)
	wake := s.wakeAgent
	s.mu.Unlock()
	appendAgentLog(s.cfg, agentID, "event turn finished delivered=%t", delivered)
	if wake != nil {
		wake(agentID)
	}
}

// heartbeatLoop runs beside consumeEvents for a live session: it re-evaluates
// liveness every heartbeatInterval and returns when the session's generation is
// gone (replaced/removed) or the base context is cancelled (Shutdown / service
// teardown). It is tracked in constructionWG so Shutdown drains it.
func (s *agentSessionSupervisor) heartbeatLoop(ctx context.Context, agentID string, process RuntimeProcess) {
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.evaluateLiveness(agentID, process) {
				return
			}
		}
	}
}

// evaluateLiveness is one heartbeat tick for a session. It returns false when the
// caller's generation is no longer resident (the loop should stop), true
// otherwise. For a still-"working" session it either refreshes the heartbeat
// (recent runtime activity) or, once silent past stallAfter, surfaces a visible
// `stalled` status with a diagnostic — never killing the process; recovery is
// event-driven via markWorking/markIdle. Non-working sessions are left untouched.
func (s *agentSessionSupervisor) evaluateLiveness(agentID string, process RuntimeProcess) bool {
	// Publish is done WHILE holding s.mu (the status enqueue is non-blocking on a
	// separate mutex, no lock cycle) so the state decision and its status enqueue
	// are atomic: a concurrent lifecycle transition cannot interleave between them
	// and let a stale heartbeat overwrite the newer status.
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[agentID]
	if session == nil || session.process != process {
		// Our generation was replaced or removed; stop watching it.
		return false
	}
	if session.dead {
		// A claimed corpse (parked for restart / terminally failed but still resident
		// as the token) is owned by its restarter, not the poller. Return false so the
		// per-process heartbeat loop EXITS — otherwise a failed/parked-dead generation
		// would retain a ticker goroutine for the daemon lifetime (blocker 11).
		return false
	}
	if session.state != "working" && session.state != "stalled" {
		// idle/disconnected/failed are not liveness-evaluated. A `stalled` session IS
		// re-evaluated: resumed telemetry must be able to clear the stall.
		return true
	}
	// Observe the driver's activity generation: a new valid frame advances it and
	// resets lastActivityAt to supervisor-monotonic now.
	advanced := s.observeActivity(session, process)
	silent := s.now().Sub(session.lastActivityAt)
	sessionID := session.sessionID
	turnID := session.activeTurn
	heartbeat := s.now().UTC().Format(time.RFC3339Nano)

	if session.state == "stalled" {
		// Telemetry recovery keys off the activity GENERATION advancing — a genuinely
		// new frame this poll — NOT a now-vs-timestamp measure. That makes it exact
		// and immune to a wall-clock step: unchanged telemetry can never "recover" a
		// stall even if the clock moves. (Lifecycle recovery is separate: markWorking/
		// markIdle already cleared the stall before this poll.)
		if advanced {
			session.state = "working"
			session.stallDiagnostic = ""
			if s.testHookLivenessPrePublish != nil {
				s.testHookLivenessPrePublish()
			}
			s.publish(agentID, updateAgentSessionRequest{
				Status:          "working",
				SessionID:       sessionID,
				CurrentTurnID:   turnID,
				CurrentActivity: "Working",
				LastHeartbeatAt: heartbeat,
			})
			appendAgentLog(s.cfg, agentID, "runtime recovered from stall: activity generation advanced on turn %s", turnID)
			return true
		}
		// Still stalled: republish the SAME stable diagnostic so LastHeartbeatAt
		// advances (proving the daemon is alive while the runtime is wedged) without
		// a per-tick semantic change — the backend comparator then suppresses the
		// activity row (blocker 16).
		if s.testHookLivenessPrePublish != nil {
			s.testHookLivenessPrePublish()
		}
		s.publish(agentID, updateAgentSessionRequest{
			Status:          "stalled",
			SessionID:       sessionID,
			CurrentTurnID:   turnID,
			CurrentActivity: session.stallDiagnostic,
			LastHeartbeatAt: heartbeat,
		})
		return true
	}

	// Working: refresh the heartbeat while alive; declare stalled once silent past
	// the threshold, measured from supervisor-monotonic time.
	if silent < s.stallAfter {
		if s.testHookLivenessPrePublish != nil {
			s.testHookLivenessPrePublish()
		}
		s.publish(agentID, updateAgentSessionRequest{
			Status:          "working",
			SessionID:       sessionID,
			CurrentTurnID:   turnID,
			CurrentActivity: "Working",
			LastHeartbeatAt: heartbeat,
		})
		return true
	}
	// Working -> stalled: capture the watermark so recovery requires a strictly
	// newer frame. Visible diagnostic, not killed — it may still recover
	// (telemetry or lifecycle) and is replaced only if work is queued behind it.
	session.state = "stalled"
	// Capture the diagnostic ONCE at the transition; subsequent stalled heartbeats
	// republish this exact string (blocker 16).
	session.stallDiagnostic = fmt.Sprintf("Stalled: no runtime activity for %s during turn %s", silent.Round(time.Second), turnID)
	if s.testHookLivenessPrePublish != nil {
		s.testHookLivenessPrePublish()
	}
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "stalled",
		SessionID:       sessionID,
		CurrentTurnID:   turnID,
		CurrentActivity: session.stallDiagnostic,
		LastHeartbeatAt: heartbeat,
	})
	appendAgentLog(s.cfg, agentID, "runtime stalled: silent for %s during turn %s; awaiting recovery or pending-work replacement", silent.Round(time.Second), turnID)
	return true
}

// observeActivity records a fresh read-boundary frame: if the driver's activity
// generation has advanced since we last saw it, snapshot the new generation and
// stamp the supervisor's OWN monotonic time. Silence is then measured from
// lastActivityAt, so it never depends on comparing a wall-clock timestamp across a
// clock step. Must be called under s.mu.
// resetLivenessFloor marks a lifecycle transition as fresh liveness NOW: it resets
// the silence clock AND snapshots the current activity generation, so a frame
// already decoded before this transition (e.g. the handshake/init or a prior idle
// frame) is not later consumed by observeActivity as fresh turn telemetry and
// granted an extra stall window (blocker 19). Must be called under s.mu.
func (s *agentSessionSupervisor) resetLivenessFloor(session *managedAgentSession, process RuntimeProcess) {
	session.lastActivityAt = s.now()
	session.lastActivitySeq = process.ActivitySeq()
}

// It returns true iff the generation advanced this call (a genuinely new frame).
func (s *agentSessionSupervisor) observeActivity(session *managedAgentSession, process RuntimeProcess) bool {
	if seq := process.ActivitySeq(); seq != session.lastActivitySeq {
		session.lastActivitySeq = seq
		session.lastActivityAt = s.now()
		return true
	}
	return false
}

func (s *agentSessionSupervisor) publish(agentID string, payload updateAgentSessionRequest) {
	s.status.Publish(agentID, payload)
}

func cloneAgentValue(current *agent) *agent {
	if current == nil {
		return nil
	}
	clone := *current
	return &clone
}

func isNoActiveTurnToSteerError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no active turn to steer")
}
