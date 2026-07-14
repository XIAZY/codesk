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

	mu       sync.Mutex
	sessions map[string]*managedAgentSession
	starting map[string]*agentSessionStart
	shutdown bool
	// restartAttempts counts consecutive transient restarts per agent, keyed by
	// agentID so it survives the per-restart session replacement. It grows the
	// backoff and is reset only on proven uptime (a completed turn), never on a
	// bare respawn — else a post-handshake quota kill would spin at full rate.
	restartAttempts map[string]int

	// baseCtx is the parent of every (fresh AND restart) detached construction;
	// baseCancel (fired by Shutdown) cancels all in-flight Spawn/Start/handshake
	// calls. In production baseCtx is a child of the service run context (bound
	// once at run() entry via bindServiceContext), so a service-context
	// cancellation — including during the synchronous startup refresh, before
	// Shutdown is reachable — propagates to every construction. Direct tests keep
	// the Background-derived baseCtx from the constructor.
	baseCtx      context.Context
	baseCancel   context.CancelFunc
	serviceBound bool

	// constructionWG tracks every detached construction worker (fresh and
	// restart). Shutdown cancels baseCtx (which makes a real provider Start/
	// handshake return), then waits on this barrier before status teardown — so it
	// never returns while a detached worker still owns an unpublished process.
	// Add is done under s.mu on the same side as the shutdown check, so it can
	// never race Wait and no worker is admitted after shutdown.
	constructionWG sync.WaitGroup
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

	// restartPending marks that a backoff restart owns this (dead) session
	// generation: no other start path may replace it until the delayed restarter
	// releases it. dead marks that this exact process generation has died — it
	// gates authorization (a dead generation's tool token is invalid) and is set
	// only for the generation whose process actually exited.
	restartPending bool
	dead           bool

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
	worker := &agentStatusWorker{
		agentID: agentID,
		updater: updater,
		wake:    make(chan struct{}, 1),
		stopped: make(chan struct{}),
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
	})
	w.signal()
}

func (w *agentStatusWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *agentStatusWorker) run() {
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
			ctx, cancel := context.WithTimeout(context.Background(), agentStatusUpdateTimeout)
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
		sessions:           map[string]*managedAgentSession{},
		starting:           map[string]*agentSessionStart{},
		restartAttempts:    map[string]int{},
		baseCtx:            baseCtx,
		baseCancel:         baseCancel,
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
			s.publish(current.ID, updateAgentSessionRequest{
				Status:          "disconnected",
				CurrentActivity: err.Error(),
				LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
	}
	stale := []*managedAgentSession{}
	s.mu.Lock()
	for agentID, session := range s.sessions {
		if _, ok := desired[agentID]; !ok {
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
	s.mu.Unlock()
	for _, session := range stale {
		_ = session.process.Stop()
	}
	return errors.Join(errs...)
}

func (s *agentSessionSupervisor) Shutdown() {
	s.mu.Lock()
	s.shutdown = true
	// Cancel every in-flight construction (fresh and restart) so a blocked
	// Spawn/Start/provider-handshake returns promptly and its process is reaped,
	// rather than stalling teardown until the construction completes on its own.
	s.baseCancel()
	sessions := make([]*managedAgentSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.sessions = map[string]*managedAgentSession{}
	s.mu.Unlock()
	for _, session := range sessions {
		_ = session.process.Stop()
	}
	// Join every detached construction worker (fresh + restart) before status
	// teardown: baseCancel above makes a real provider Start/handshake return, so
	// each worker's construction completes, the baseCtx.Err() publish fence reaps
	// it, and the worker drains — Shutdown never returns while a detached worker
	// still owns an unpublished process. No post-Shutdown zombie.
	s.constructionWG.Wait()
	s.status.Stop()
}

// refreshDesiredSpec updates a session's desired agent spec to the latest and
// bumps its revision ONLY when a spawn-relevant field actually changed (kind or
// instructions). An identical reconcile/notification must not advance the
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
//   - Kind         -> runtime driver selection.            [fingerprinted]
//   - SystemPrompt -> Spawn/resume/start-session Instructions. [fingerprinted]
//   - SessionID    -> resume continuity; for a restart it is taken from the
//     session, not the desired spec, so it is not a config change.
//
// Workdir is ID-derived (NOT WorkspaceRoot), ToolToken is generated, and env is
// built from cfg — none are desired-spec inputs. So Kind+SystemPrompt is the
// complete changeable set; extend this fingerprint if the constructor grows a
// new `current.*` input.
func (session *managedAgentSession) refreshDesiredSpec(current *agent) (changed bool) {
	changed = session.agent == nil ||
		session.agent.Kind != current.Kind ||
		session.agent.SystemPrompt != current.SystemPrompt
	session.agent = cloneAgentValue(current)
	if changed {
		session.agentRev++
	}
	return changed
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
// correctly omits it. Both cover Kind (driver) and SystemPrompt (Instructions);
// ID is the fixed identity, Workdir is ID-derived, ToolToken is generated.
func (start *agentSessionStart) spawnFingerprintChanged(current *agent) bool {
	return start.agent == nil ||
		start.agent.Kind != current.Kind ||
		start.agent.SystemPrompt != current.SystemPrompt ||
		// Normalized: startSession compares strings.TrimSpace(current.SessionID)
		// to decide resume vs start, so whitespace-only differences aren't a real
		// change and must not churn the construction.
		strings.TrimSpace(start.agent.SessionID) != strings.TrimSpace(current.SessionID)
}

func (s *agentSessionSupervisor) ensureSession(ctx context.Context, current *agent) error {
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
				if session.refreshDesiredSpec(current) && session.constructCancel != nil {
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
			session.refreshDesiredSpec(current)
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
			if start.spawnFingerprintChanged(current) {
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
	toolToken, err := newToolToken()
	if err != nil {
		return err
	}
	process, err := driver.Spawn(ctx, RuntimeSpawnSpec{
		AgentID:      current.ID,
		Workdir:      workdir,
		ToolToken:    toolToken,
		Instructions: current.SystemPrompt,
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
		agent:     cloneAgentValue(current),
		process:   process,
		toolToken: toolToken,
		workdir:   workdir,
		sessionID: sessionID,
		state:     "idle",
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
	s.sessions[current.ID] = session
	s.mu.Unlock()
	started = false
	s.publish(current.ID, updateAgentSessionRequest{
		Status:          "idle",
		SessionID:       sessionID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	go s.consumeEvents(current.ID, process)
	return nil
}

func (s *agentSessionSupervisor) ScheduleNotificationTurn(ctx context.Context, current *agent, prompt string, forMeSig string, generalSig string) error {
	if current == nil || strings.TrimSpace(prompt) == "" {
		return nil
	}
	if err := s.ensureSession(ctx, current); err != nil {
		return err
	}
	s.mu.Lock()
	session := s.sessions[current.ID]
	if session == nil {
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
	// This write is a new authoritative operation: bump the nonce and capture it,
	// so its own completion can detect whether any later transition (a settled
	// turn, death, or a newer notification) superseded it while s.mu was dropped.
	session.turnOpSeq++
	op := session.turnOpSeq
	session.state = "working"
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
		if currentSession := s.sessions[current.ID]; currentSession != nil && currentSession.process == process && !currentSession.dead && currentSession.turnOpSeq == op {
			currentSession.state = "idle"
			currentSession.activeForMeSig = ""
			currentSession.activeGeneralSig = ""
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
	stale := currentSession == nil || currentSession.process != process || currentSession.dead || currentSession.turnOpSeq != op
	if !stale {
		currentSession.activeTurn = turnID
		currentSession.state = "working"
	}
	s.mu.Unlock()
	if stale {
		// The generation we wrote to was replaced or died mid-write; do not mark a
		// different or dead session working, nor publish a stale working status.
		appendAgentLog(s.cfg, agentID, "turn start completed on a stale generation session=%s; dropping status", sessionID)
		return nil
	}
	s.publish(current.ID, updateAgentSessionRequest{
		Status:          "working",
		SessionID:       sessionID,
		CurrentTurnID:   turnID,
		CurrentActivity: "Working",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
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

func (s *agentSessionSupervisor) consumeEvents(agentID string, process RuntimeProcess) {
	for event := range process.Events() {
		switch event.Kind {
		case RuntimeEventTurnStarted:
			if strings.TrimSpace(event.TurnID) != "" {
				s.markWorking(agentID, process, event.TurnID)
			}
		case RuntimeEventTurnCompleted:
			s.markIdle(agentID, process, true)
			// A completed turn is the only proof of real uptime — the process ran
			// actual work, not just respawned. It alone resets the transient
			// backoff counter (never a failed turn or a bare idle/status event,
			// which would let a crash->idle->crash agent hot-loop at the floor).
			s.resetRestartBackoff(agentID, process)
		case RuntimeEventTurnFailed:
			s.markIdle(agentID, process, false)
		case RuntimeEventIdle:
			s.markIdle(agentID, process, true)
		}
	}
	// Classify the death from the exit forensics (facet 2), recorded before the
	// events channel closed. Default is transient: only an explicit positive
	// signal moves an exit to Expected (deliberate Stop) or terminal (a
	// CLI-proven, non-self-recovering provider failure) — a rate-limit/429 or any
	// unrecognized exit stays transient so we never one-way-door a recoverable
	// agent.
	exit := process.ExitInfo()
	terminalReason := ""
	if !exit.Expected {
		terminalReason = s.terminalExitReason(exit)
	}
	transient := !exit.Expected && terminalReason == ""

	owned := false
	attempt := 0
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
		if transient {
			s.restartAttempts[agentID]++
			attempt = s.restartAttempts[agentID]
			// A backoff restart now owns this dead generation: the parked session
			// stays authoritative in the map (no delete) until the restarter
			// publishes the replacement by a conditional swap against it, so no
			// other start path may replace it and a racing removal invalidates it.
			session.restartPending = true
		}
	}
	s.mu.Unlock()
	if !owned {
		// The events belong to a process this session has already replaced.
		return
	}

	// Expected: the daemon deliberately Stop()ped this process — clean teardown,
	// no restart, no status noise.
	if exit.Expected {
		appendAgentLog(s.cfg, agentID, "runtime stopped as expected; no restart")
		if s.testHookDeathHandled != nil {
			s.testHookDeathHandled("expected")
		}
		return
	}

	// Terminal: a CLI-proven, non-self-recovering provider failure. Surface it as
	// `failed` with the provider's own (sanitized) line; no restart until a human
	// acts. The match set is locked only from live-probe evidence.
	if terminalReason != "" {
		// Sanitize and bound the provider's line at the PUBLICATION boundary, not
		// only in the log suffix: it is persisted as CurrentActivity and shown to
		// humans, so control characters and unbounded length must never reach the
		// backend. Truncation is rune-safe (never splits a multibyte character).
		published := sanitizeExitLine(terminalReason)
		appendAgentLog(s.cfg, agentID, "runtime terminal failure; no restart: %s", published)
		s.publish(agentID, updateAgentSessionRequest{
			Status:          "failed",
			CurrentTurnID:   "",
			CurrentActivity: published,
			LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		if s.testHookDeathHandled != nil {
			s.testHookDeathHandled("terminal")
		}
		return
	}

	// Transient: an unexpected crash (or self-clearing throttle) — mark
	// disconnected and restart. Capped backoff lands in facet 4.
	delay := restartBackoff(attempt)
	appendAgentLog(s.cfg, agentID, "runtime event stream closed; marking session disconnected%s; restart attempt=%d backoff=%s", crashSuffix(exit), attempt, delay)
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "disconnected",
		CurrentTurnID:   "",
		CurrentActivity: "Disconnected",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	// Join the restarter through the same construction barrier as fresh workers.
	// Add under s.mu on the same side as the shutdown check; if shutdown already
	// won, do not launch a restart at all (Shutdown's Wait must not see a new Add).
	s.mu.Lock()
	launchRestart := !s.shutdown
	if launchRestart {
		s.constructionWG.Add(1)
	}
	s.mu.Unlock()
	if !launchRestart {
		if s.testHookDeathHandled != nil {
			s.testHookDeathHandled("transient")
		}
		return
	}
	go func() {
		defer s.constructionWG.Done()
		if s.testHookRestartComplete != nil {
			defer s.testHookRestartComplete()
		}
		// The restarter owns EVERY outcome of the construction window against its
		// one dead-generation token — publish, fail, refresh, or remove/shutdown —
		// so a construction failure re-arms the next capped attempt instead of
		// stranding the token `restartPending` forever (which would gate Reconcile
		// out permanently). It keeps looping until the token leaves the map (a
		// successful swap OR a removal/replacement) or shutdown.
		for {
			s.restartSleep(delay)
			s.mu.Lock()
			session := s.sessions[agentID]
			if s.shutdown || session == nil || session.process != process || !session.restartPending {
				// Removed / replaced / shutdown / no longer ours — cancel.
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
			// attempt against the same token, unless shutdown/removal intervened.
			if s.shutdown || !session.restartPending {
				s.mu.Unlock()
				return
			}
			s.restartAttempts[agentID]++
			delay = restartBackoff(s.restartAttempts[agentID])
			s.mu.Unlock()
		}
	}()
	if s.testHookDeathHandled != nil {
		s.testHookDeathHandled("transient")
	}
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
func restartGateClosed(session *managedAgentSession) bool {
	if session == nil {
		return false
	}
	switch session.state {
	case "failed":
		return true
	case "disconnected":
		return session.restartPending
	default:
		return false
	}
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
	if session == nil || session.process != process {
		s.mu.Unlock()
		return
	}
	if session != nil {
		session.state = "working"
		session.activeTurn = turnID
		// An authoritative TurnStarted is a state transition that must win over a
		// StartTurn RPC completion still in flight: bump the operation nonce so a
		// stale StartTurn error/timeout returning afterward cannot demote this live
		// turn to idle.
		session.turnOpSeq++
	}
	sessionID := ""
	if session != nil {
		sessionID = session.sessionID
	}
	s.mu.Unlock()
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "working",
		SessionID:       sessionID,
		CurrentTurnID:   turnID,
		CurrentActivity: "Working",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	appendAgentLog(s.cfg, agentID, "event turn started turn=%s", turnID)
}

func (s *agentSessionSupervisor) markIdle(agentID string, process RuntimeProcess, delivered bool) {
	s.mu.Lock()
	session := s.sessions[agentID]
	if session == nil || session.process != process {
		s.mu.Unlock()
		return
	}
	sessionID := ""
	if session != nil {
		session.state = "idle"
		session.activeTurn = ""
		// This idle/failed/completed transition is authoritative: bump the
		// operation nonce so a StartTurn completion still in flight against this
		// generation cannot revive it to working.
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
		sessionID = session.sessionID
	}
	s.mu.Unlock()
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "idle",
		SessionID:       sessionID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	appendAgentLog(s.cfg, agentID, "event turn finished delivered=%t", delivered)
	s.mu.Lock()
	wake := s.wakeAgent
	s.mu.Unlock()
	if wake != nil {
		wake(agentID)
	}
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
