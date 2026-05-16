package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const forMeSteerMessage = "You have new for-me items in your notification center. Continue your current task, but consider them when appropriate."
const agentStatusUpdateTimeout = 30 * time.Second

type updateAgentSessionRequest struct {
	Status          string `json:"status"`
	CodexThreadID   string `json:"codexThreadId,omitempty"`
	CurrentTurnID   string `json:"currentTurnId,omitempty"`
	CurrentActivity string `json:"currentActivity,omitempty"`
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
}

type agentSessionUpdater func(context.Context, string, updateAgentSessionRequest) error

type agentSessionSupervisor struct {
	cfg       Config
	status    *agentStatusSyncer
	factory   appServerFactory
	wakeAgent func(string)

	mu       sync.Mutex
	sessions map[string]*managedAgentSession
	starting map[string]*agentSessionStart
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
	app       appServerClient
	toolToken string
	logName   string
	workdir   string
	threadID  string

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

func newAgentSessionSupervisor(cfg Config, updater agentSessionUpdater, factory appServerFactory) *agentSessionSupervisor {
	if factory == nil {
		factory = newCodexAppServer
	}
	if updater == nil {
		updater = func(context.Context, string, updateAgentSessionRequest) error { return nil }
	}
	return &agentSessionSupervisor{
		cfg:      cfg,
		status:   newAgentStatusSyncer(updater),
		factory:  factory,
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
		if strings.ToLower(current.Kind) != "codex" {
			continue
		}
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
		_ = session.app.Stop()
	}
	return errors.Join(errs...)
}

func (s *agentSessionSupervisor) Shutdown() {
	s.mu.Lock()
	sessions := make([]*managedAgentSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.sessions = map[string]*managedAgentSession{}
	s.mu.Unlock()
	for _, session := range sessions {
		_ = session.app.Stop()
	}
	s.status.Stop()
}

func (s *agentSessionSupervisor) ensureSession(ctx context.Context, current *agent) error {
	if current == nil || strings.TrimSpace(current.ID) == "" {
		return nil
	}
	for {
		s.mu.Lock()
		if session := s.sessions[current.ID]; session != nil {
			if session.state == "disconnected" {
				delete(s.sessions, current.ID)
				s.mu.Unlock()
				_ = session.app.Stop()
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
	workdir := s.workspacePath(current)
	if err := os.MkdirAll(s.cfg.RuntimeDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	logName := agentLogName(current)
	appendAgentLog(workdir, logName, "ensuring resident agent session agent=%s handle=%s", current.ID, current.Handle)
	toolToken, err := newToolToken()
	if err != nil {
		return err
	}
	app := s.factory(s.cfg, workdir, toolToken, logName)
	if err := app.Start(ctx); err != nil {
		return err
	}
	started := true
	defer func() {
		if started {
			_ = app.Stop()
		}
	}()
	threadID := firstNonEmptyText(current.CodexThreadID, current.SessionID)
	if threadID != "" {
		if err := app.ThreadResume(ctx, threadID, workdir, current.SystemPrompt); err != nil {
			appendAgentLog(workdir, logName, "thread resume failed thread=%s err=%v; starting new thread", threadID, err)
			threadID = ""
		} else {
			appendAgentLog(workdir, logName, "thread resumed thread=%s", threadID)
		}
	}
	if threadID == "" {
		threadID, err = app.ThreadStart(ctx, workdir, current.SystemPrompt)
		if err != nil {
			appendAgentLog(workdir, logName, "thread start failed err=%v", err)
			return err
		}
		appendAgentLog(workdir, logName, "thread started thread=%s", threadID)
	}
	session := &managedAgentSession{
		agent:     cloneAgentValue(current),
		app:       app,
		toolToken: toolToken,
		logName:   logName,
		workdir:   workdir,
		threadID:  threadID,
		state:     "idle",
	}
	s.mu.Lock()
	if existing := s.sessions[current.ID]; existing != nil {
		s.mu.Unlock()
		appendAgentLog(workdir, logName, "discarding duplicate app-server because session already exists")
		return nil
	}
	s.sessions[current.ID] = session
	s.mu.Unlock()
	started = false
	s.publish(current.ID, updateAgentSessionRequest{
		Status:          "idle",
		CodexThreadID:   threadID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	go s.consumeEvents(current.ID, app)
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
		app := session.app
		threadID := session.threadID
		turnID := session.activeTurn
		workdir := session.workdir
		logName := session.logName
		if shouldSteer {
			session.steeredForMeSig = forMeSig
		}
		s.mu.Unlock()
		if shouldSteer {
			appendAgentLog(workdir, logName, "steering active turn thread=%s turn=%s reason=for_me_notification", threadID, turnID)
			if err := app.TurnSteer(ctx, threadID, turnID, forMeSteerMessage); err != nil {
				appendAgentLog(workdir, logName, "turn steer failed thread=%s turn=%s err=%v", threadID, turnID, err)
				if isNoActiveTurnToSteerError(err) {
					return nil
				}
				return err
			}
			return nil
		}
		if queuedFollowupForMe || queuedFollowupGeneral {
			appendAgentLog(workdir, logName, "queued notification follow-up while busy for_me=%t general=%t", queuedFollowupForMe, queuedFollowupGeneral)
		}
		return nil
	}
	if session.state == "disconnected" {
		s.mu.Unlock()
		return nil
	}
	if (!hasForMe || forMeSig == session.deliveredForMeSig) && (!hasGeneral || generalSig == session.deliveredGeneralSig) {
		workdir := session.workdir
		logName := session.logName
		s.mu.Unlock()
		appendAgentLog(workdir, logName, "skipping unchanged notification inbox signatures for_me=%t general=%t", hasForMe, hasGeneral)
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
	app := session.app
	threadID := session.threadID
	workdir := session.workdir
	logName := session.logName
	s.mu.Unlock()

	turnID, err := app.TurnStart(ctx, threadID, prompt, workdir)
	if err != nil {
		s.mu.Lock()
		if currentSession := s.sessions[current.ID]; currentSession != nil {
			currentSession.state = "idle"
			currentSession.activeForMeSig = ""
			currentSession.activeGeneralSig = ""
		}
		s.mu.Unlock()
		appendAgentLog(workdir, logName, "turn start failed thread=%s err=%v", threadID, err)
		return err
	}
	s.mu.Lock()
	if currentSession := s.sessions[current.ID]; currentSession != nil {
		currentSession.activeTurn = turnID
		currentSession.state = "working"
	}
	s.mu.Unlock()
	s.publish(current.ID, updateAgentSessionRequest{
		Status:          "working",
		CodexThreadID:   threadID,
		CurrentTurnID:   turnID,
		CurrentActivity: "Working",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	appendAgentLog(workdir, logName, "turn started thread=%s turn=%s for_me=%t general=%t", threadID, turnID, hasForMe, hasGeneral)
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

func (s *agentSessionSupervisor) consumeEvents(agentID string, app appServerClient) {
	for event := range app.Events() {
		switch event.Method {
		case "turn/started":
			if turnID := turnIDFromEvent(event); turnID != "" {
				s.markWorking(agentID, app, turnID)
			}
		case "turn/completed":
			s.markIdle(agentID, app, true)
		case "turn/failed":
			s.markIdle(agentID, app, false)
		case "thread/status/changed":
			if threadStatusTypeFromEvent(event) == "idle" {
				s.markIdle(agentID, app, true)
			}
		}
	}
	var restartAgent *agent
	s.mu.Lock()
	if session := s.sessions[agentID]; session != nil && session.app == app {
		session.state = "disconnected"
		session.activeTurn = ""
		session.activeForMeSig = ""
		session.activeGeneralSig = ""
		session.steeredForMeSig = ""
		restartAgent = cloneAgentValue(session.agent)
		if restartAgent != nil && restartAgent.CodexThreadID == "" {
			restartAgent.CodexThreadID = session.threadID
		}
	}
	s.mu.Unlock()
	if restartAgent == nil {
		return
	}
	appendAgentLog(s.workspacePath(restartAgent), agentLogName(restartAgent), "app-server event stream closed; marking session disconnected")
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "disconnected",
		CurrentTurnID:   "",
		CurrentActivity: "Disconnected",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	go func() {
		time.Sleep(2 * time.Second)
		if err := s.ensureSession(context.Background(), restartAgent); err != nil {
			fmt.Printf("agent session %s restart error: %v\n", agentID, err)
			appendAgentLog(s.workspacePath(restartAgent), agentLogName(restartAgent), "session restart failed err=%v", err)
		}
	}()
}

func (s *agentSessionSupervisor) markWorking(agentID string, app appServerClient, turnID string) {
	s.mu.Lock()
	session := s.sessions[agentID]
	if session == nil || session.app != app {
		s.mu.Unlock()
		return
	}
	if session != nil {
		session.state = "working"
		session.activeTurn = turnID
	}
	threadID := ""
	workdir := ""
	logName := ""
	if session != nil {
		threadID = session.threadID
		workdir = session.workdir
		logName = session.logName
	}
	s.mu.Unlock()
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "working",
		CodexThreadID:   threadID,
		CurrentTurnID:   turnID,
		CurrentActivity: "Working",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if workdir != "" {
		appendAgentLog(workdir, logName, "event turn/started turn=%s", turnID)
	}
}

func (s *agentSessionSupervisor) markIdle(agentID string, app appServerClient, delivered bool) {
	s.mu.Lock()
	session := s.sessions[agentID]
	if session == nil || session.app != app {
		s.mu.Unlock()
		return
	}
	threadID := ""
	workdir := ""
	logName := ""
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
		threadID = session.threadID
		workdir = session.workdir
		logName = session.logName
	}
	s.mu.Unlock()
	s.publish(agentID, updateAgentSessionRequest{
		Status:          "idle",
		CodexThreadID:   threadID,
		CurrentTurnID:   "",
		CurrentActivity: "Idle",
		LastHeartbeatAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if workdir != "" {
		appendAgentLog(workdir, logName, "event turn finished delivered=%t", delivered)
	}
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

func (s *agentSessionSupervisor) workspacePath(current *agent) string {
	if current == nil {
		return s.cfg.AgentWorkspaceRoot
	}
	name := filepath.Base(strings.TrimSpace(current.WorkspaceRoot))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = strings.TrimSpace(current.ID)
	}
	if name == "" {
		return s.cfg.AgentWorkspaceRoot
	}
	return filepath.Join(s.cfg.AgentWorkspaceRoot, name)
}

func cloneAgentValue(current *agent) *agent {
	if current == nil {
		return nil
	}
	clone := *current
	return &clone
}

func turnIDFromEvent(event appServerEvent) string {
	var payload struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(event.Params, &payload)
	return payload.Turn.ID
}

func threadStatusTypeFromEvent(event appServerEvent) string {
	var payload struct {
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	_ = json.Unmarshal(event.Params, &payload)
	return payload.Status.Type
}

func isNoActiveTurnToSteerError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no active turn to steer")
}
