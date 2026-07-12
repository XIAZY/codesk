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
	"testing"
	"time"
)

type agentSessionStatusUpdate struct {
	agentID string
	payload updateAgentSessionRequest
}

type fakeRuntimeDriver struct {
	mu              sync.Mutex
	processes       []*fakeRuntimeProcess
	detection       RuntimeDetection
	startEntered    chan struct{}
	startRelease    chan struct{}
	startErr        error
	startSessionErr error
	steerErr        error
	failResume      bool
}

type fakeRuntimeProcess struct {
	mu sync.Mutex

	events chan RuntimeEvent

	started         bool
	stopped         bool
	inputs          []RuntimeInput
	nextTurn        int
	startEntered    chan struct{}
	startRelease    chan struct{}
	startErr        error
	startSessionErr error
	steerErr        error
	failResume      bool
}

func newFakeRuntimeDriver() *fakeRuntimeDriver {
	return &fakeRuntimeDriver{detection: RuntimeDetection{Kind: RuntimeCodex, Available: true}}
}

func newFakeRuntimeRegistry(driver *fakeRuntimeDriver) *runtimeRegistry {
	registry := newRuntimeRegistry(driver)
	registry.detections[driver.Kind()] = driver.detection
	return registry
}

func (f *fakeRuntimeDriver) Kind() RuntimeKind {
	return RuntimeCodex
}

func (f *fakeRuntimeDriver) Detect(ctx context.Context) RuntimeDetection {
	_ = ctx
	if f.detection.Kind == "" {
		f.detection.Kind = RuntimeCodex
	}
	return f.detection
}

func (f *fakeRuntimeDriver) Spawn(ctx context.Context, spec RuntimeSpawnSpec) (RuntimeProcess, error) {
	_ = ctx
	process := &fakeRuntimeProcess{
		events:          make(chan RuntimeEvent, 16),
		startEntered:    f.startEntered,
		startRelease:    f.startRelease,
		startErr:        f.startErr,
		startSessionErr: f.startSessionErr,
		steerErr:        f.steerErr,
		failResume:      f.failResume,
	}
	f.mu.Lock()
	f.processes = append(f.processes, process)
	f.mu.Unlock()
	return process, nil
}

func agentSessionTestConfig(t *testing.T, workspaceRoot string) Config {
	t.Helper()
	return Config{
		DataDir:            t.TempDir(),
		WorkspaceID:        "workspace:test",
		AgentWorkspaceRoot: workspaceRoot,
	}
}

func (f *fakeRuntimeDriver) only(t *testing.T) *fakeRuntimeProcess {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.processes) != 1 {
		t.Fatalf("expected one runtime process, got %d", len(f.processes))
	}
	return f.processes[0]
}

func (p *fakeRuntimeProcess) Start(ctx context.Context) error {
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	if p.startEntered != nil {
		select {
		case p.startEntered <- struct{}{}:
		default:
		}
	}
	if p.startRelease != nil {
		select {
		case <-p.startRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.startErr != nil {
		return p.startErr
	}
	return nil
}

func (p *fakeRuntimeProcess) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	return nil
}

func (p *fakeRuntimeProcess) WriteStdin(ctx context.Context, input RuntimeInput) (RuntimeWriteResult, error) {
	_ = ctx
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inputs = append(p.inputs, input)
	switch input.Kind {
	case RuntimeInputResumeSession:
		if p.failResume {
			return RuntimeWriteResult{}, fmt.Errorf("resume failed")
		}
		return RuntimeWriteResult{SessionID: input.SessionID}, nil
	case RuntimeInputStartSession:
		if p.startSessionErr != nil {
			return RuntimeWriteResult{}, p.startSessionErr
		}
		return RuntimeWriteResult{SessionID: "thread_new"}, nil
	case RuntimeInputStartTurn:
		p.nextTurn++
		return RuntimeWriteResult{TurnID: fmt.Sprintf("turn_%d", p.nextTurn)}, nil
	case RuntimeInputSteerTurn:
		return RuntimeWriteResult{}, p.steerErr
	case RuntimeInputInterruptTurn:
		return RuntimeWriteResult{}, nil
	default:
		return RuntimeWriteResult{}, fmt.Errorf("unexpected input kind %q", input.Kind)
	}
}

func (p *fakeRuntimeProcess) Events() <-chan RuntimeEvent {
	return p.events
}

func (p *fakeRuntimeProcess) PID() int {
	return 123
}

func (p *fakeRuntimeProcess) inputsByKind(kind RuntimeInputKind) []RuntimeInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	var inputs []RuntimeInput
	for _, input := range p.inputs {
		if input.Kind == kind {
			inputs = append(inputs, input)
		}
	}
	return inputs
}

func waitAgentSessionStatus(t *testing.T, updates <-chan agentSessionStatusUpdate, agentID string, status string) agentSessionStatusUpdate {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case update := <-updates:
			if update.agentID == agentID && update.payload.Status == status {
				return update
			}
		case <-deadline:
			t.Fatalf("timed out waiting for agent %s status %s", agentID, status)
		}
	}
}

func TestAgentSessionStartsThreadWithDeveloperInstructions(t *testing.T) {
	factory := newFakeRuntimeDriver()
	type statusUpdate struct {
		agentID string
		payload updateAgentSessionRequest
	}
	updates := make(chan statusUpdate, 8)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		updates <- statusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Handle: "swe", Kind: "codex", SystemPrompt: "shared prompt"}

	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)
	starts := process.inputsByKind(RuntimeInputStartSession)
	if !process.started || len(starts) != 1 {
		t.Fatalf("expected process start and session start, process=%#v inputs=%#v", process, starts)
	}
	if starts[0].Instructions != "shared prompt" {
		t.Fatalf("expected shared prompt in session instructions, got %#v", starts[0])
	}
	update := <-updates
	if update.agentID != "agent_1" || update.payload.Status != "idle" || update.payload.SessionID != "thread_new" {
		t.Fatalf("unexpected session update: %#v", update)
	}
}

func TestAgentSessionReconcileReturnsStartupError(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.startErr = errors.New("codex missing")
	type statusUpdate struct {
		agentID string
		payload updateAgentSessionRequest
	}
	updates := make(chan statusUpdate, 1)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		updates <- statusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Handle: "swe", Kind: "codex"}})
	if err == nil {
		t.Fatal("expected reconcile startup error")
	}
	var startupErr *agentSessionStartupError
	if !errors.As(err, &startupErr) || startupErr.AgentID != "agent_1" {
		t.Fatalf("expected agent startup error, got %T %v", err, err)
	}
	update := <-updates
	if update.agentID != "agent_1" || update.payload.Status != "disconnected" || !strings.Contains(update.payload.CurrentActivity, "codex missing") {
		t.Fatalf("unexpected disconnected update: %#v", update)
	}
}

func TestAgentSessionMissingRuntimePublishesDisconnectedWithoutSpawn(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.detection = RuntimeDetection{Kind: RuntimeCodex, Available: false, Reason: "codex command not found"}
	updates := make(chan agentSessionStatusUpdate, 4)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		updates <- agentSessionStatusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	if err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Handle: "swe", Kind: "codex"}}); err != nil {
		t.Fatalf("missing runtime should not fail reconcile: %v", err)
	}
	factory.mu.Lock()
	processCount := len(factory.processes)
	factory.mu.Unlock()
	if processCount != 0 {
		t.Fatalf("missing runtime should not spawn a process, got %d", processCount)
	}
	update := waitAgentSessionStatus(t, updates, "agent_1", "disconnected")
	if !strings.Contains(update.payload.CurrentActivity, "codex runtime unavailable") ||
		!strings.Contains(update.payload.CurrentActivity, "codex command not found") {
		t.Fatalf("unexpected disconnected activity: %#v", update.payload)
	}
}

func TestAgentSessionUnregisteredRuntimePublishesDisconnectedWithoutSpawn(t *testing.T) {
	factory := newFakeRuntimeDriver()
	updates := make(chan agentSessionStatusUpdate, 4)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		updates <- agentSessionStatusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	if err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Handle: "swe", Kind: "python"}}); err != nil {
		t.Fatalf("unregistered runtime should not fail reconcile: %v", err)
	}
	factory.mu.Lock()
	processCount := len(factory.processes)
	factory.mu.Unlock()
	if processCount != 0 {
		t.Fatalf("unregistered runtime should not spawn a process, got %d", processCount)
	}
	update := waitAgentSessionStatus(t, updates, "agent_1", "disconnected")
	if !strings.Contains(update.payload.CurrentActivity, "python runtime unavailable") ||
		!strings.Contains(update.payload.CurrentActivity, "runtime driver is not registered") {
		t.Fatalf("unexpected disconnected activity: %#v", update.payload)
	}
}

func TestAgentSessionReconcileReturnsSessionStartError(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.startSessionErr = errors.New("session start failed")
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Handle: "swe", Kind: "codex"}})
	if err == nil {
		t.Fatal("expected reconcile session start error")
	}
	var startupErr *agentSessionStartupError
	if !errors.As(err, &startupErr) || !strings.Contains(startupErr.Err.Error(), "session start failed") {
		t.Fatalf("expected wrapped session start error, got %T %v", err, err)
	}
	process := factory.only(t)
	if !process.stopped {
		t.Fatal("failed session start should stop the runtime process")
	}
}

func TestAgentSessionConcurrentEnsureStartsOneAppServer(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Handle: "swe", Kind: "codex", SystemPrompt: "shared prompt"}

	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- supervisor.ensureSession(context.Background(), current)
		}()
	}
	<-factory.startEntered
	factory.mu.Lock()
	startedBeforeRelease := len(factory.processes)
	factory.mu.Unlock()
	if startedBeforeRelease != 1 {
		t.Fatalf("expected one in-flight runtime process before release, got %d", startedBeforeRelease)
	}
	close(factory.startRelease)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensure session returned error: %v", err)
		}
	}
	process := factory.only(t)
	if !process.started || len(process.inputsByKind(RuntimeInputStartSession)) != 1 {
		t.Fatalf("expected one process start and one session start, process=%#v", process)
	}
}

func TestAgentSessionIgnoresEventsFromStaleRuntimeProcess(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	active := factory.only(t)
	stale := &fakeRuntimeProcess{events: make(chan RuntimeEvent, 1)}

	supervisor.markWorking("agent_1", stale, "turn_stale", "")
	state, turn := sessionState(supervisor, "agent_1")
	if state != "idle" || turn != "" {
		t.Fatalf("stale turn/started mutated session: state=%q turn=%q", state, turn)
	}

	supervisor.markWorking("agent_1", active, "turn_live", "")
	state, turn = sessionState(supervisor, "agent_1")
	if state != "working" || turn != "turn_live" {
		t.Fatalf("active turn/started did not mark working: state=%q turn=%q", state, turn)
	}
	supervisor.markIdle("agent_1", stale, true, "")
	state, turn = sessionState(supervisor, "agent_1")
	if state != "working" || turn != "turn_live" {
		t.Fatalf("stale turn/completed mutated session: state=%q turn=%q", state, turn)
	}
	supervisor.markIdle("agent_1", active, true, "")
	state, turn = sessionState(supervisor, "agent_1")
	if state != "idle" || turn != "" {
		t.Fatalf("active turn/completed did not mark idle: state=%q turn=%q", state, turn)
	}
}

func TestAgentSessionResumesExistingSession(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex", SessionID: "thread_existing"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)
	resumes := process.inputsByKind(RuntimeInputResumeSession)
	if len(resumes) != 1 || resumes[0].SessionID != "thread_existing" {
		t.Fatalf("expected session resume, got %#v", resumes)
	}
	if len(process.inputsByKind(RuntimeInputStartSession)) != 0 {
		t.Fatalf("expected no new session when resume succeeds")
	}
}

func TestAgentSessionResumeFailureStartsNewSession(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.failResume = true
	updates := make(chan agentSessionStatusUpdate, 4)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		updates <- agentSessionStatusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex", SessionID: "thread_existing"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)
	resumes := process.inputsByKind(RuntimeInputResumeSession)
	if len(resumes) != 1 || resumes[0].SessionID != "thread_existing" {
		t.Fatalf("expected one failed resume attempt, got %#v", resumes)
	}
	starts := process.inputsByKind(RuntimeInputStartSession)
	if len(starts) != 1 {
		t.Fatalf("expected fallback new session start, got %#v", starts)
	}
	update := waitAgentSessionStatus(t, updates, "agent_1", "idle")
	if update.payload.SessionID != "thread_new" {
		t.Fatalf("expected fallback session id to be published, got %#v", update.payload)
	}
}

func TestAgentSessionIdleNotificationStartsOncePerInboxSignature(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "first", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule first: %v", err)
	}
	process := factory.only(t)
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 1 {
		t.Fatalf("expected first turn start, got %d", len(process.inputsByKind(RuntimeInputStartTurn)))
	}
	supervisor.markIdle("agent_1", process, true, "")
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "same", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule same: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 1 {
		t.Fatalf("expected same inbox signature to be suppressed, got %d starts", len(process.inputsByKind(RuntimeInputStartTurn)))
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "changed", "for-me:v2", ""); err != nil {
		t.Fatalf("schedule changed: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 2 {
		t.Fatalf("expected changed signature to start another turn, got %d", len(process.inputsByKind(RuntimeInputStartTurn)))
	}
}

func TestAgentSessionBusyForMeSteersOnceAndQueuesFollowup(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "", "general:v1"); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	process := factory.only(t)
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 1 {
		t.Fatalf("expected active turn start")
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule for-me while busy: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me duplicate", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule duplicate for-me while busy: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputSteerTurn)) != 1 {
		t.Fatalf("expected exactly one steer for same for-me signature, got %d", len(process.inputsByKind(RuntimeInputSteerTurn)))
	}
	steers := process.inputsByKind(RuntimeInputSteerTurn)
	if steers[0].Importance != RuntimeImportanceImportant ||
		steers[0].SessionID != "thread_new" ||
		steers[0].TurnID != "turn_1" ||
		steers[0].Text != forMeSteerMessage {
		t.Fatalf("unexpected important steer input: %#v", steers[0])
	}
	forMePending, generalPending := supervisor.Pending("agent_1")
	if !forMePending || generalPending {
		t.Fatalf("expected for-me followup only, got forMe=%v general=%v", forMePending, generalPending)
	}
	supervisor.markIdle("agent_1", process, true, "")
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "followup", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule followup: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 2 {
		t.Fatalf("expected follow-up turn after busy for-me change, got %d starts", len(process.inputsByKind(RuntimeInputStartTurn)))
	}
}

func TestAgentSessionBusyGeneralQueuesFollowupWithoutSteer(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	process := factory.only(t)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "general", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule general while busy: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputSteerTurn)) != 0 {
		t.Fatalf("expected no steer for general-only change, got %d", len(process.inputsByKind(RuntimeInputSteerTurn)))
	}
	forMePending, generalPending := supervisor.Pending("agent_1")
	if forMePending || !generalPending {
		t.Fatalf("expected general followup only, got forMe=%v general=%v", forMePending, generalPending)
	}
	supervisor.markIdle("agent_1", process, true, "")
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "followup", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule followup: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 2 {
		t.Fatalf("expected follow-up turn after busy general change, got %d starts", len(process.inputsByKind(RuntimeInputStartTurn)))
	}
}

func TestAgentSessionBusyGeneralCoalescesFollowupLogAndKeepsLatestSignature(t *testing.T) {
	factory := newFakeRuntimeDriver()
	workspaceRoot := t.TempDir()
	cfg := agentSessionTestConfig(t, workspaceRoot)
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex", Handle: "tester"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "general v1", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule general v1 while busy: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "general v2", "for-me:v1", "general:v2"); err != nil {
		t.Fatalf("schedule general v2 while busy: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "general v3", "for-me:v1", "general:v3"); err != nil {
		t.Fatalf("schedule general v3 while busy: %v", err)
	}

	supervisor.mu.Lock()
	session := supervisor.sessions["agent_1"]
	var followupGeneralSig string
	if session != nil {
		followupGeneralSig = session.followupGeneralSig
	}
	supervisor.mu.Unlock()
	if followupGeneralSig != "general:v3" {
		t.Fatalf("expected latest general followup signature, got %q", followupGeneralSig)
	}

	logBytes, err := os.ReadFile(agentLogPath(cfg, current.ID))
	if err != nil {
		t.Fatalf("read agent log: %v", err)
	}
	if count := strings.Count(string(logBytes), "queued notification follow-up while busy"); count != 1 {
		t.Fatalf("expected one queued-followup log entry for coalesced general updates, got %d\n%s", count, string(logBytes))
	}
}

func TestAgentSessionFailedTurnDoesNotMarkInboxSignatureDelivered(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "first", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule first: %v", err)
	}
	process := factory.only(t)
	supervisor.markIdle("agent_1", process, false, "")
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "retry", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputStartTurn)) != 2 {
		t.Fatalf("expected failed turn signature to be retried, got %d starts", len(process.inputsByKind(RuntimeInputStartTurn)))
	}
}

func TestAgentSessionBusyForMeSteersEachChangedInboxSignature(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "", "general:v1"); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	process := factory.only(t)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me v1", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule first for-me while busy: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me v2", "for-me:v2", "general:v1"); err != nil {
		t.Fatalf("schedule second for-me while busy: %v", err)
	}
	if len(process.inputsByKind(RuntimeInputSteerTurn)) != 2 {
		t.Fatalf("expected a steer for each changed for-me inbox signature, got %d", len(process.inputsByKind(RuntimeInputSteerTurn)))
	}
}

func TestAgentSessionNoActiveTurnSteerErrorIsNotFatal(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.steerErr = fmt.Errorf("runtime steer failed: no active turn to steer")
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "", "general:v1"); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("no-active-turn steer should not fail automation: %v", err)
	}
}

func TestAgentSessionRuntimeEventsPublishGenericStatus(t *testing.T) {
	factory := newFakeRuntimeDriver()
	updates := make(chan agentSessionStatusUpdate, 8)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		updates <- agentSessionStatusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)
	process.events <- RuntimeEvent{Kind: RuntimeEventTurnStarted, TurnID: "turn_event"}
	working := waitAgentSessionStatus(t, updates, "agent_1", "working")
	if working.payload.SessionID != "thread_new" || working.payload.CurrentTurnID != "turn_event" {
		t.Fatalf("unexpected working update: %#v", working.payload)
	}
	process.events <- RuntimeEvent{Kind: RuntimeEventTurnCompleted}
	idle := waitAgentSessionStatus(t, updates, "agent_1", "idle")
	if idle.payload.SessionID != "thread_new" || idle.payload.CurrentTurnID != "" {
		t.Fatalf("unexpected idle update: %#v", idle.payload)
	}
}

func TestAgentSessionAdoptsForkedSessionIDFromStreamEvents(t *testing.T) {
	factory := newFakeRuntimeDriver()
	updates := make(chan agentSessionStatusUpdate, 8)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		updates <- agentSessionStatusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	// markIdle fires the idle-wake callback only after its completion-log append
	// (agent_sessions.go), giving a deterministic post-handler barrier to drain
	// that async append before t.TempDir cleanup — the fake process Stop never
	// closes Events, so Shutdown is not a join point.
	idleWake := make(chan string, 4)
	supervisor.SetIdleWake(func(agentID string) { idleWake <- agentID })

	// A fresh session is spawn-pinned to the start id "thread_new".
	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)

	// The runtime forks a NEW underlying session mid-turn (e.g. `claude --resume`
	// forks a new session id). The driver observes it on the init stream event and
	// threads it into every lifecycle event it emits.
	process.events <- RuntimeEvent{Kind: RuntimeEventTurnStarted, TurnID: "turn_event", SessionID: "thread_forked"}
	working := waitAgentSessionStatus(t, updates, "agent_1", "working")
	if working.payload.SessionID != "thread_forked" {
		t.Fatalf("supervisor published the stale spawn session id instead of the forked id observed on the stream: %#v", working.payload)
	}

	process.events <- RuntimeEvent{Kind: RuntimeEventTurnCompleted, SessionID: "thread_forked"}
	idle := waitAgentSessionStatus(t, updates, "agent_1", "idle")
	if idle.payload.SessionID != "thread_forked" {
		t.Fatalf("supervisor published the stale spawn session id on idle: %#v", idle.payload)
	}

	// The in-memory pin must also advance to the forked id, so a crash-restart
	// re-resumes the live session instead of rewinding to the discarded spawn id.
	if got := sessionSessionID(supervisor, "agent_1"); got != "thread_forked" {
		t.Fatalf("supervisor kept the stale spawn session id %q; a crash-restart would rewind/discard turns", got)
	}

	// Drain markIdle's post-publish completion-log append via the idle-wake
	// barrier before returning, so teardown can't race the in-flight write.
	select {
	case <-idleWake:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for idle-wake barrier before teardown")
	}
}

func TestAgentSessionSupervisorDoesNotUseCodexWireMethods(t *testing.T) {
	body, err := os.ReadFile("agent_sessions.go")
	if err != nil {
		t.Fatalf("read agent_sessions.go: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{
		"CodexThreadID",
		"codexThreadId",
		"thread/start",
		"thread/resume",
		"turn/start",
		"turn/steer",
		"turn/interrupt",
		"turn/started",
		"codexAppServer",
		"appServerEvent",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("agent session supervisor should not contain Codex wire term %q", forbidden)
		}
	}
}

func TestCodexAppServerThreadParamsUseSharedInstructionsAndSlimResume(t *testing.T) {
	client := &codexAppServer{}
	start := client.threadParams("/tmp/agent", "", "shared instructions")
	if start["developerInstructions"] != "shared instructions" {
		t.Fatalf("missing developer instructions: %#v", start)
	}
	if start["sandbox"] != "danger-full-access" || start["approvalPolicy"] != "never" {
		t.Fatalf("unexpected thread permissions: %#v", start)
	}
	if _, ok := start["excludeTurns"]; ok {
		t.Fatalf("new thread should not set resume-only excludeTurns: %#v", start)
	}

	resume := client.threadParams("/tmp/agent", "thread_1", "shared instructions")
	if resume["threadId"] != "thread_1" || resume["excludeTurns"] != true {
		t.Fatalf("resume params should include thread id and excludeTurns: %#v", resume)
	}
}

func sessionState(supervisor *agentSessionSupervisor, agentID string) (string, string) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	session := supervisor.sessions[agentID]
	if session == nil {
		return "", ""
	}
	return session.state, session.activeTurn
}

func sessionSessionID(supervisor *agentSessionSupervisor, agentID string) string {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	session := supervisor.sessions[agentID]
	if session == nil {
		return ""
	}
	return session.sessionID
}

func TestCodexAppServerReadLoopRoutesResponsesNotificationsAndServerRequests(t *testing.T) {
	cfg := Config{
		DataDir:     t.TempDir(),
		WorkspaceID: "workspace:test",
	}
	logger, err := openAgentLog(cfg, "agent_1")
	if err != nil {
		t.Fatalf("open agent log: %v", err)
	}
	stdin := &captureWriteCloser{}
	pending := make(chan appServerResponse, 1)
	client := &codexAppServer{
		stdin:   stdin,
		pending: map[int64]chan appServerResponse{7: pending},
		events:  make(chan appServerEvent, 2),
		log:     logger,
	}
	defer client.log.Close()
	input := strings.Join([]string{
		`{"id":7,"result":{"turn":{"id":"turn_7"}}}`,
		`{"id":"approval_1","method":"item/commandExecution/requestApproval","params":{}}`,
		`{"method":"turn/started","params":{"turn":{"id":"turn_7"}}}`,
	}, "\n") + "\n"

	client.readLoop(strings.NewReader(input))

	response := <-pending
	var responsePayload struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(response.Result, &responsePayload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if responsePayload.Turn.ID != "turn_7" {
		t.Fatalf("unexpected routed response: %#v", responsePayload)
	}
	if got := stdin.String(); !strings.Contains(got, `"id":"approval_1"`) || !strings.Contains(got, `"decision":"accept"`) {
		t.Fatalf("expected approval response, got %q", got)
	}
	event := <-client.events
	if event.Method != "turn/started" {
		t.Fatalf("unexpected event: %#v", event)
	}
	client.log.Close()
	logBytes, err := os.ReadFile(agentLogPath(cfg, "agent_1"))
	if err != nil {
		t.Fatalf("read agent log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "jsonrpc recv") || !strings.Contains(logText, "jsonrpc send") {
		t.Fatalf("expected protocol traffic in agent log, got %q", logText)
	}
}

func TestCodexAppServerStartHandlesAgentLogOpenFailure(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "notty-data-file")
	if err := os.WriteFile(dataFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write data file: %v", err)
	}
	client := &codexAppServer{
		cfg: Config{
			CodexCommand: filepath.Join(t.TempDir(), "missing-codex"),
			DataDir:      dataFile,
			WorkspaceID:  "workspace:test",
		},
		workdir:   t.TempDir(),
		agentID:   "agent_1",
		toolToken: "tool_token",
		pending:   map[int64]chan appServerResponse{},
		events:    make(chan appServerEvent, 1),
		done:      make(chan error, 1),
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Start should return a normal error when agent log open fails, panicked with %v", recovered)
		}
	}()
	if err := client.Start(context.Background()); err == nil {
		t.Fatal("expected missing Codex command error")
	}
}

type captureWriteCloser struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (c *captureWriteCloser) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builder.Write(p)
}

func (c *captureWriteCloser) Close() error {
	return nil
}

func (c *captureWriteCloser) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builder.String()
}
