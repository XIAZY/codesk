package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
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
	cfg       Config
	status    *agentStatusSyncer
	runtimes  *runtimeRegistry
	wakeAgent func(string)
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	shutdown  sync.Once

	mu       sync.Mutex
	sessions map[string]*managedAgentSession
	starting map[string]*agentSessionStart
	closed   bool
}

type agentSessionStart struct {
	done chan struct{}
	err  error
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
	ctx, cancel := context.WithCancel(context.Background())
	return &agentSessionSupervisor{
		cfg:      cfg,
		status:   newAgentStatusSyncer(updater),
		runtimes: runtimes,
		ctx:      ctx,
		cancel:   cancel,
		sessions: map[string]*managedAgentSession{},
		starting: map[string]*agentSessionStart{},
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
		}
	}
	s.mu.Unlock()
	for _, session := range stale {
		_ = session.process.Stop()
	}
	return errors.Join(errs...)
}

func (s *agentSessionSupervisor) Shutdown() {
	if s == nil {
		return
	}
	s.shutdown.Do(func() {
		s.mu.Lock()
		s.closed = true
		sessions := make([]*managedAgentSession, 0, len(s.sessions))
		for _, session := range s.sessions {
			sessions = append(sessions, session)
		}
		s.sessions = map[string]*managedAgentSession{}
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		for _, session := range sessions {
			_ = session.process.Stop()
		}
		s.wg.Wait()
		if s.status != nil {
			s.status.Stop()
		}
	})
}

func (s *agentSessionSupervisor) ensureSession(ctx context.Context, current *agent) error {
	if current == nil || strings.TrimSpace(current.ID) == "" {
		return nil
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return context.Canceled
		}
		if session := s.sessions[current.ID]; session != nil {
			if session.state == "disconnected" {
				delete(s.sessions, current.ID)
				s.mu.Unlock()
				_ = session.process.Stop()
				continue
			}
			session.agent = cloneAgentValue(current)
			s.mu.Unlock()
			return nil
		}
		if start := s.starting[current.ID]; start != nil {
			done := start.done
			s.mu.Unlock()
			select {
			case <-done:
				if start.err != nil {
					return start.err
				}
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		start := &agentSessionStart{done: make(chan struct{})}
		s.starting[current.ID] = start
		s.mu.Unlock()

		err := s.startSession(ctx, current)
		s.mu.Lock()
		start.err = err
		if s.starting[current.ID] == start {
			delete(s.starting, current.ID)
			close(start.done)
		}
		s.mu.Unlock()
		return err
	}
}

func (s *agentSessionSupervisor) startSession(ctx context.Context, current *agent) error {
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
	if err := process.Start(ctx); err != nil {
		return err
	}
	started := true
	defer func() {
		if started {
			_ = process.Stop()
		}
	}()
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
	if s.closed {
		s.mu.Unlock()
		return context.Canceled
	}
	if existing := s.sessions[current.ID]; existing != nil {
		s.mu.Unlock()
		appendAgentLog(s.cfg, current.ID, "discarding duplicate runtime process because session already exists")
		return nil
	}
	s.sessions[current.ID] = session
	s.wg.Add(1)
	s.mu.Unlock()
	started = false
	s.publish(current.ID, updateAgentSessionRequest{
		Status:          "idle",
		SessionID:       sessionID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	go func() {
		defer s.wg.Done()
		s.consumeEvents(current.ID, process)
	}()
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
				s.mu.Lock()
				if currentSession := s.sessions[agentID]; currentSession != nil &&
					currentSession.process == process &&
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
	if session.state == "disconnected" {
		s.mu.Unlock()
		return nil
	}
	if (!hasForMe || forMeSig == session.deliveredForMeSig) && (!hasGeneral || generalSig == session.deliveredGeneralSig) {
		agentID := current.ID
		s.mu.Unlock()
		appendAgentLog(s.cfg, agentID, "skipping unchanged notification inbox signatures for_me=%t general=%t", hasForMe, hasGeneral)
		return nil
	}
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
		if currentSession := s.sessions[current.ID]; currentSession != nil {
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
	if currentSession := s.sessions[current.ID]; currentSession != nil {
		currentSession.activeTurn = turnID
		currentSession.state = "working"
	}
	s.mu.Unlock()
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
		if session.toolToken == token && session.agent != nil {
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
	events := process.Events()
	for {
		var event RuntimeEvent
		var ok bool
		select {
		case <-s.ctx.Done():
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
			s.markIdle(agentID, process, true)
		case RuntimeEventTurnFailed:
			s.markIdle(agentID, process, false)
		case RuntimeEventIdle:
			s.markIdle(agentID, process, true)
		}
	}

disconnected:
	var restartAgent *agent
	s.mu.Lock()
	if session := s.sessions[agentID]; session != nil && session.process == process {
		session.state = "disconnected"
		session.activeTurn = ""
		session.activeForMeSig = ""
		session.activeGeneralSig = ""
		session.steeredForMeSig = ""
		restartAgent = cloneAgentValue(session.agent)
		if restartAgent != nil {
			restartAgent.SessionID = session.sessionID
		}
	}
	s.mu.Unlock()
	if restartAgent == nil {
		return
	}
	appendAgentLog(s.cfg, agentID, "runtime event stream closed; marking session disconnected")
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "disconnected",
		CurrentTurnID:   "",
		CurrentActivity: "Disconnected",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-timer.C:
	}
	if err := s.ensureSession(s.ctx, restartAgent); err != nil && s.ctx.Err() == nil {
		fmt.Printf("agent session %s restart error: %v\n", agentID, err)
		appendAgentLog(s.cfg, agentID, "session restart failed err=%v", err)
	}
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
