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
	"unicode/utf8"
)

type agentSessionStatusUpdate struct {
	agentID string
	payload updateAgentSessionRequest
}

type fakeRuntimeDriver struct {
	mu              sync.Mutex
	processes       []*fakeRuntimeProcess
	spawnSpecs      []RuntimeSpawnSpec
	detection       RuntimeDetection
	startEntered    chan struct{}
	startRelease    chan struct{}
	startErr        error
	startSessionErr error
	steerErr        error
	failResume      bool
	startIgnoreCtx  bool
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
	startIgnoreCtx  bool
	exitInfo        RuntimeExitInfo

	// Optional StartTurn interception: when startTurnEntered/startTurnRelease are
	// set, a StartTurn write signals entry then blocks until released, WITHOUT
	// holding p.mu — so a concurrent death (which reads ExitInfo under p.mu) can
	// interleave. startTurnErr is returned after release.
	startTurnEntered chan struct{}
	startTurnRelease chan struct{}
	startTurnErr     error
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
		startIgnoreCtx:  f.startIgnoreCtx,
	}
	f.mu.Lock()
	f.processes = append(f.processes, process)
	f.spawnSpecs = append(f.spawnSpecs, spec)
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

func (f *fakeRuntimeDriver) onlySpawnSpec(t *testing.T) RuntimeSpawnSpec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.spawnSpecs) != 1 {
		t.Fatalf("expected one runtime spawn spec, got %d", len(f.spawnSpecs))
	}
	return f.spawnSpecs[0]
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
		if p.startIgnoreCtx {
			// Simulate a construction that finishes before cancellation propagates,
			// exercising the publish-time claim/CAS backstop rather than ctx cancel.
			<-p.startRelease
		} else {
			select {
			case <-p.startRelease:
			case <-ctx.Done():
				return ctx.Err()
			}
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
		entered := p.startTurnEntered
		releaseCh := p.startTurnRelease
		turnErr := p.startTurnErr
		if entered != nil || releaseCh != nil {
			// Drop the lock while blocking so an interleaved death can read ExitInfo.
			p.mu.Unlock()
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
			}
			if releaseCh != nil {
				<-releaseCh
			}
			p.mu.Lock()
			if turnErr != nil {
				return RuntimeWriteResult{}, turnErr
			}
		}
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

func (p *fakeRuntimeProcess) ExitInfo() RuntimeExitInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitInfo
}

func (p *fakeRuntimeProcess) setExitInfo(info RuntimeExitInfo) {
	p.mu.Lock()
	p.exitInfo = info
	p.mu.Unlock()
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

func TestAgentSessionWorkdirUsesAgentIdentityNotBackendWorkspaceRoot(t *testing.T) {
	tests := []struct {
		name          string
		agentID       string
		workspaceRoot string
		workspaceName string
	}{
		{name: "backend path cannot relocate agent", agentID: "agent_one", workspaceRoot: "teams/shared", workspaceName: "agent_one"},
		{name: "shared basename cannot merge agents", agentID: "agent_two", workspaceRoot: "other/shared", workspaceName: "agent_two"},
		{name: "metadata parent cannot escape root", agentID: "agent_three", workspaceRoot: "..", workspaceName: "agent_three"},
		{name: "empty metadata still sanitizes identity", agentID: "agent/four", workspaceRoot: "", workspaceName: "agent_four"},
		{name: "embedded dots remain part of identity", agentID: "agent.five", workspaceRoot: "agents/dotted", workspaceName: "agent.five"},
		{name: "dot identity cannot collapse to root", agentID: ".", workspaceRoot: "agents/dot", workspaceName: "_"},
		{name: "dot dot identity cannot escape root", agentID: "..", workspaceRoot: "agents/dot-dot", workspaceName: "__"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspaceRoot := filepath.Join(t.TempDir(), "agents")
			factory := newFakeRuntimeDriver()
			supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, workspaceRoot), nil, newFakeRuntimeRegistry(factory))
			defer supervisor.Shutdown()

			current := &agent{ID: test.agentID, Kind: "codex", WorkspaceRoot: test.workspaceRoot}
			if err := supervisor.ensureSession(context.Background(), current); err != nil {
				t.Fatalf("ensure session: %v", err)
			}

			got := factory.onlySpawnSpec(t).Workdir
			want := filepath.Join(workspaceRoot, test.workspaceName)
			if got != want {
				t.Fatalf("runtime workdir = %q, want identity-derived %q", got, want)
			}
		})
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

	supervisor.markWorking("agent_1", stale, "turn_stale")
	state, turn := sessionState(supervisor, "agent_1")
	if state != "idle" || turn != "" {
		t.Fatalf("stale turn/started mutated session: state=%q turn=%q", state, turn)
	}

	supervisor.markWorking("agent_1", active, "turn_live")
	state, turn = sessionState(supervisor, "agent_1")
	if state != "working" || turn != "turn_live" {
		t.Fatalf("active turn/started did not mark working: state=%q turn=%q", state, turn)
	}
	supervisor.markIdle("agent_1", stale, true)
	state, turn = sessionState(supervisor, "agent_1")
	if state != "working" || turn != "turn_live" {
		t.Fatalf("stale turn/completed mutated session: state=%q turn=%q", state, turn)
	}
	supervisor.markIdle("agent_1", active, true)
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
	supervisor.markIdle("agent_1", process, true)
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
	supervisor.markIdle("agent_1", process, true)
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
	supervisor.markIdle("agent_1", process, true)
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
	supervisor.markIdle("agent_1", process, false)
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

func TestAgentSessionDoesNotRestartAfterShutdown(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	// Deterministically park the restart callback at its backoff delay so we can
	// interleave Shutdown mid-flight, and observe when the callback finishes — no
	// wall-clock dependency.
	parked := make(chan struct{})
	release := make(chan struct{})
	restartDone := make(chan struct{})
	supervisor.restartSleep = func(time.Duration) { close(parked); <-release }
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)

	// Kill the runtime: closing Events drives consumeEvents into its restart
	// path, which schedules the delayed respawn — parked at the injected delay.
	close(process.events)
	<-parked

	// Shutdown wins while the restart is parked, then release it. A restart
	// scheduled before shutdown must not spawn a runtime or re-store a session
	// after it — otherwise the respawn escapes supervision as a zombie process.
	supervisor.Shutdown()
	close(release)
	<-restartDone

	factory.mu.Lock()
	spawned := len(factory.processes)
	factory.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("restart spawned a runtime after Shutdown: want 1 process total, got %d (untracked zombie)", spawned)
	}
	supervisor.mu.Lock()
	remaining := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("restart re-stored a session after Shutdown: want 0 sessions, got %d", remaining)
	}
}

func TestAgentSessionExpectedStopIsNotRestarted(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	supervisor.restartSleep = func(time.Duration) {}
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }
	restarted := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restarted) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)

	// The daemon deliberately Stop()ped this process — its exit reports Expected.
	// A deliberate stop must be classified expected and never restarted (else a
	// shutdown/reconcile Stop would respawn what we just tore down).
	process.setExitInfo(RuntimeExitInfo{Expected: true})
	close(process.events)

	select {
	case c := <-handled:
		if c != "expected" {
			t.Fatalf("deliberate stop classified %q, want expected", c)
		}
	case <-restarted:
		t.Fatal("a deliberately-stopped process was restarted")
	case <-time.After(2 * time.Second):
		t.Fatal("consumeEvents did not handle the process death")
	}
	factory.mu.Lock()
	spawned := len(factory.processes)
	factory.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("expected stop spawned a replacement: want 1 process total, got %d", spawned)
	}
}

func TestRestartBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Second}, {1, 2 * time.Second}, {2, 4 * time.Second}, {3, 8 * time.Second},
		{4, 16 * time.Second}, {5, 32 * time.Second}, {6, 60 * time.Second}, {10, 60 * time.Second},
	}
	for _, c := range cases {
		if got := restartBackoff(c.attempt); got != c.want {
			t.Fatalf("restartBackoff(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
}

func latestFakeProcess(f *fakeRuntimeDriver) *fakeRuntimeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.processes[len(f.processes)-1]
}

func waitRestartAttempts(t *testing.T, s *agentSessionSupervisor, agentID string, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		got := s.restartAttempts[agentID]
		s.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("restartAttempts[%s] = %d, want %d", agentID, got, want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAgentSessionTransientRestartBackoffGrowsAcrossRespawns(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	var mu sync.Mutex
	var delays []time.Duration
	supervisor.restartSleep = func(d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
	}
	restarted := make(chan struct{}, 8)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	// Three consecutive transient crashes with no proven uptime in between; the
	// counter is per-agent so it survives each respawn and the backoff grows.
	for i := 0; i < 3; i++ {
		close(latestFakeProcess(factory).events)
		<-restarted
	}
	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("restart delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart delay[%d] = %s, want %s (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestAgentSessionRestartCounterResetsOnCompletedTurn(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	var mu sync.Mutex
	var delays []time.Duration
	supervisor.restartSleep = func(d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
	}
	restarted := make(chan struct{}, 8)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	// First transient crash -> attempt 1 -> 2s backoff.
	close(latestFakeProcess(factory).events)
	<-restarted

	// The respawned agent completes a turn — proven uptime — which must reset the
	// counter (a bare respawn must not). The next crash then starts over at 2s.
	proc := latestFakeProcess(factory)
	proc.events <- RuntimeEvent{Kind: RuntimeEventTurnCompleted}
	waitRestartAttempts(t, supervisor, "agent_1", 0)
	close(proc.events)
	<-restarted

	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	want := []time.Duration{2 * time.Second, 2 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("restart delays = %v, want %v (counter should reset after a completed turn)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart delay[%d] = %s, want %s — counter did not reset on proven uptime", i, got[i], want[i])
		}
	}
}

func TestAgentSessionUnrequestedCleanExitIsTransient(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)

	// Exit code 0 but NOT Stop()ped: an unrequested clean exit is not "expected".
	// Expected must come from a deliberate Stop(), never a bare exit code — a
	// clean exit we didn't ask for must restart (transient), not be silently
	// treated as a deliberate teardown.
	process.setExitInfo(RuntimeExitInfo{ExitCode: 0, Expected: false})
	close(process.events)
	if got := <-handled; got != "transient" {
		t.Fatalf("unrequested clean exit classified %q, want transient", got)
	}
}

func TestAgentSessionStaleProcessExitIsIgnored(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) {
		select {
		case handled <- c:
		default:
		}
	}
	restarted := make(chan struct{}, 4)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	live := factory.only(t)

	// A different, already-replaced process reports an exit. Because the session's
	// current process is `live`, the stale process's exit snapshot must not
	// classify, restart, or overwrite the live session — exit info is per-instance
	// and gated by the process-identity check.
	stale := &fakeRuntimeProcess{events: make(chan RuntimeEvent, 1)}
	stale.setExitInfo(RuntimeExitInfo{ExitCode: 1})
	done := make(chan struct{})
	go func() {
		supervisor.consumeEvents("agent_1", stale)
		close(done)
	}()
	close(stale.events)
	<-done

	select {
	case c := <-handled:
		t.Fatalf("stale process exit was classified %q; it must be ignored", c)
	default:
	}
	select {
	case <-restarted:
		t.Fatal("stale process exit triggered a restart")
	default:
	}
	supervisor.mu.Lock()
	sameProcess := supervisor.sessions["agent_1"] != nil && supervisor.sessions["agent_1"].process == live
	supervisor.mu.Unlock()
	if !sameProcess {
		t.Fatal("stale process exit replaced the live session's process")
	}
}

func TestAgentSessionGenericExitDeathDefaultsToTransient(t *testing.T) {
	// The live-CLI probe's observed negative shape: exit code 1, no distinctive
	// structured signal, empty stderr. With the proven-empty terminal set, this
	// must classify transient (disconnected + capped retry), never `failed` — a
	// false terminal would permanently strand a recoverable session.
	factory := newFakeRuntimeDriver()
	var pubMu sync.Mutex
	var statuses []string
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		pubMu.Lock()
		statuses = append(statuses, payload.Status)
		pubMu.Unlock()
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }
	restarted := make(chan struct{}, 2)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)
	process.setExitInfo(RuntimeExitInfo{ExitCode: 1})
	close(process.events)

	if got := <-handled; got != "transient" {
		t.Fatalf("generic exit-1 death classified %q, want transient", got)
	}
	<-restarted // transient path restarts (with capped backoff)
	pubMu.Lock()
	pub := append([]string(nil), statuses...)
	pubMu.Unlock()
	for _, s := range pub {
		if s == "failed" {
			t.Fatalf("generic exit-1 death published failed; an unproven signal must never terminal-classify (%v)", pub)
		}
	}
}

func TestAgentSessionInjectedTerminalReasonRoutesToFailed(t *testing.T) {
	// The production terminal set is empty by proof; inject a call-scoped (per
	// supervisor instance, no global state) fake distinctive reason to verify the
	// dormant terminal branch is actually wired: it must publish `failed` with the
	// reason and NOT restart. This tests routing mechanics only — it claims no
	// real provider signal exists.
	factory := newFakeRuntimeDriver()
	failedPub := make(chan updateAgentSessionRequest, 4)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		if payload.Status == "failed" {
			failedPub <- payload
		}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	supervisor.terminalExitReason = func(RuntimeExitInfo) string { return "This model has been retired." }
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }
	restarted := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restarted) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := factory.only(t)
	process.setExitInfo(RuntimeExitInfo{ExitCode: 1})
	close(process.events)

	if got := <-handled; got != "terminal" {
		t.Fatalf("injected terminal reason classified %q, want terminal", got)
	}
	select {
	case <-restarted:
		t.Fatal("terminal failure was restarted")
	default:
	}
	got := <-failedPub
	if got.CurrentActivity != "This model has been retired." {
		t.Fatalf("failed publish activity = %q, want the injected reason", got.CurrentActivity)
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
		stdin:    stdin,
		pending:  map[int64]chan appServerResponse{7: pending},
		events:   make(chan appServerEvent, 2),
		stopping: make(chan struct{}),
		readDone: make(chan struct{}),
		log:      logger,
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

func nthSpawnToolToken(t *testing.T, f *fakeRuntimeDriver, n int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.spawnSpecs) {
		t.Fatalf("spawn spec %d not present (have %d)", n, len(f.spawnSpecs))
	}
	return f.spawnSpecs[n].ToolToken
}

// Cluster A spoke 1: the backoff must be authoritative. While a transient
// restart is parked, no other start path (here: a Reconcile) may replace the
// dead generation and spawn immediately, which would bypass the cap entirely.
func TestAgentSessionReconcileDuringBackoffParkDoesNotBypassCap(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	parked := make(chan struct{})
	release := make(chan struct{})
	restartDone := make(chan struct{})
	supervisor.restartSleep = func(time.Duration) { close(parked); <-release }
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	close(proc1.events)
	<-parked

	if err := supervisor.Reconcile(context.Background(), []*agent{current}); err != nil {
		t.Fatalf("reconcile during park: %v", err)
	}
	factory.mu.Lock()
	during := len(factory.processes)
	factory.mu.Unlock()
	if during != 1 {
		t.Fatalf("reconcile during backoff park bypassed the cap: want 1 process, got %d", during)
	}

	close(release)
	<-restartDone
	factory.mu.Lock()
	after := len(factory.processes)
	factory.mu.Unlock()
	if after != 2 {
		t.Fatalf("expected exactly one gated respawn after release: want 2 processes, got %d", after)
	}
}

// Cluster A spoke 2: only a proven TurnCompleted resets the backoff. A failed
// turn or a bare idle/status event between crashes must NOT reset it, or a
// crash->idle->crash agent hot-loops at the 2s floor.
func TestAgentSessionTurnFailedAndIdleDoNotResetBackoff(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	var mu sync.Mutex
	var delays []time.Duration
	supervisor.restartSleep = func(d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
	}
	restarted := make(chan struct{}, 8)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	close(latestFakeProcess(factory).events)
	<-restarted

	proc := latestFakeProcess(factory)
	proc.events <- RuntimeEvent{Kind: RuntimeEventTurnFailed}
	proc.events <- RuntimeEvent{Kind: RuntimeEventIdle}
	// Counter must remain at 1 through the non-completion events; the ordered,
	// single consumeEvents goroutine processes them before the close below.
	waitRestartAttempts(t, supervisor, "agent_1", 1)
	close(proc.events)
	<-restarted

	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("TurnFailed/Idle reset the backoff: delays=%v, want %v", got, want)
	}
}

// Cluster A spoke 3: the parked restarter is generation-fenced. If the backend
// removes the agent during the park, the captured restartAgent must not
// resurrect it.
func TestAgentSessionRemovalDuringBackoffParkDoesNotResurrect(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	parked := make(chan struct{})
	release := make(chan struct{})
	restartDone := make(chan struct{})
	supervisor.restartSleep = func(time.Duration) { close(parked); <-release }
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	close(proc1.events)
	<-parked

	if err := supervisor.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}
	close(release)
	<-restartDone

	factory.mu.Lock()
	spawned := len(factory.processes)
	factory.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("removed agent was resurrected by the parked restarter: want 1 process, got %d", spawned)
	}
	supervisor.mu.Lock()
	remaining := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("removed agent left a session behind: want 0, got %d", remaining)
	}
}

// Cluster A spoke 4: a terminally-failed generation is human-gated — a
// notification must not write to the dead runtime nor spawn a replacement, and
// the failed state must survive a subsequent reconcile.
func TestAgentSessionTerminalFailedBlocksNotificationWrite(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	supervisor.terminalExitReason = func(RuntimeExitInfo) string { return "provider terminal: quota exhausted" }
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	proc1.setExitInfo(RuntimeExitInfo{Expected: false})
	close(proc1.events)
	if c := <-handled; c != "terminal" {
		t.Fatalf("death classified %q, want terminal", c)
	}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "please continue", "sig-forme", ""); err != nil {
		t.Fatalf("schedule notification: %v", err)
	}
	if got := len(proc1.inputsByKind(RuntimeInputStartTurn)); got != 0 {
		t.Fatalf("notification wrote a StartTurn to the dead runtime: got %d", got)
	}
	factory.mu.Lock()
	spawned := len(factory.processes)
	factory.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("notification spawned a replacement for a failed generation: want 1, got %d", spawned)
	}
	if state, _ := sessionState(supervisor, "agent_1"); state != "failed" {
		t.Fatalf("failed state not preserved after notification: state=%q", state)
	}

	if err := supervisor.Reconcile(context.Background(), []*agent{current}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	factory.mu.Lock()
	spawned = len(factory.processes)
	factory.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("reconcile respawned a failed generation: want 1, got %d", spawned)
	}
	if state, _ := sessionState(supervisor, "agent_1"); state != "failed" {
		t.Fatalf("failed state not preserved after reconcile: state=%q", state)
	}
}

// Cluster A spoke 5: a StartTurn completion is fenced to its exact generation.
// If the generation dies (transient -> parked) while WriteStdin is in flight,
// the error completion must NOT demote the dead session to idle (which would
// strand the gated respawn); state stays parked and exactly one gated respawn
// happens on release.
func TestAgentSessionStaleTurnCompletionDoesNotClobberParkedGeneration(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	restartParked := make(chan struct{})
	restartRelease := make(chan struct{})
	restartDone := make(chan struct{})
	supervisor.restartSleep = func(time.Duration) { close(restartParked); <-restartRelease }
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	proc1.mu.Lock()
	proc1.startTurnEntered = make(chan struct{}, 1)
	proc1.startTurnRelease = make(chan struct{})
	proc1.startTurnErr = errors.New("write after death")
	proc1.mu.Unlock()

	// A notification drives a StartTurn that blocks mid-write.
	notifyDone := make(chan error, 1)
	go func() {
		notifyDone <- supervisor.ScheduleNotificationTurn(context.Background(), current, "do work", "sig-forme", "")
	}()
	<-proc1.startTurnEntered

	// The generation dies transiently while the write is blocked: it becomes
	// disconnected + parked (a gated respawn is scheduled).
	close(proc1.events)
	<-restartParked

	// Release the write with an error. The stale completion must not clobber the
	// parked/dead generation.
	close(proc1.startTurnRelease)
	if err := <-notifyDone; err == nil {
		t.Fatal("expected the blocked StartTurn to return its injected error")
	}
	if state, _ := sessionState(supervisor, "agent_1"); state != "disconnected" {
		t.Fatalf("stale error completion clobbered the parked generation: state=%q, want disconnected", state)
	}
	supervisor.mu.Lock()
	pending := supervisor.sessions["agent_1"] != nil && supervisor.sessions["agent_1"].restartPending
	supervisor.mu.Unlock()
	if !pending {
		t.Fatal("parked restart was cancelled by a stale completion")
	}

	// The gated respawn still happens exactly once on release.
	close(restartRelease)
	<-restartDone
	factory.mu.Lock()
	spawned := len(factory.processes)
	factory.mu.Unlock()
	if spawned != 2 {
		t.Fatalf("want exactly one gated respawn after a stale completion: 2 processes, got %d", spawned)
	}
}

// Cluster A spoke 6: a dead process generation's tool token is invalid the
// instant it dies — in the parked-transient window and after replacement.
func TestAgentSessionParkedGenerationToolTokenIsRejected(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	parked := make(chan struct{})
	release := make(chan struct{})
	restartDone := make(chan struct{})
	supervisor.restartSleep = func(time.Duration) { close(parked); <-release }
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	token1 := nthSpawnToolToken(t, factory, 0)
	if supervisor.agentByToolToken(token1) == nil {
		t.Fatal("a live generation's token must authorize")
	}

	close(latestFakeProcess(factory).events)
	<-parked
	if supervisor.agentByToolToken(token1) != nil {
		t.Fatal("dead (parked-transient) generation token still authorized — use-after-death auth leak")
	}

	close(release)
	<-restartDone
	token2 := nthSpawnToolToken(t, factory, 1)
	if token2 == token1 {
		t.Fatal("replacement generation reused the dead token")
	}
	if supervisor.agentByToolToken(token2) == nil {
		t.Fatal("replacement generation's fresh token must authorize")
	}
	if supervisor.agentByToolToken(token1) != nil {
		t.Fatal("dead token still authorized after replacement")
	}
}

// Cluster A spoke 6 (terminal variant): a failed generation's token is rejected
// indefinitely, not merely for a backoff window.
func TestAgentSessionFailedGenerationToolTokenIsRejected(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	supervisor.terminalExitReason = func(RuntimeExitInfo) string { return "provider terminal: auth revoked" }
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	token1 := nthSpawnToolToken(t, factory, 0)
	proc1 := factory.only(t)
	proc1.setExitInfo(RuntimeExitInfo{Expected: false})
	close(proc1.events)
	if c := <-handled; c != "terminal" {
		t.Fatalf("death classified %q, want terminal", c)
	}
	if supervisor.agentByToolToken(token1) != nil {
		t.Fatal("failed generation token still authorized — indefinite use-after-death auth leak")
	}
}

// Acceptance gap: the terminal reason is sanitized, length-capped, and UTF-8
// safe at the PUBLICATION boundary, not just in the log suffix.
func TestAgentSessionTerminalReasonSanitizedAtPublishBoundary(t *testing.T) {
	factory := newFakeRuntimeDriver()
	updates := make(chan agentSessionStatusUpdate, 16)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		_ = ctx
		updates <- agentSessionStatusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	// Control chars, embedded newline, and well over the byte cap with a multibyte
	// rune straddling the boundary.
	rawReason := "quota\x00 exhausted\n" + strings.Repeat("é", 300)
	supervisor.terminalExitReason = func(RuntimeExitInfo) string { return rawReason }
	handled := make(chan string, 1)
	supervisor.testHookDeathHandled = func(c string) { handled <- c }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	proc1.setExitInfo(RuntimeExitInfo{Expected: false})
	close(proc1.events)
	if c := <-handled; c != "terminal" {
		t.Fatalf("death classified %q, want terminal", c)
	}

	update := waitAgentSessionStatus(t, updates, "agent_1", "failed")
	got := update.payload.CurrentActivity
	if strings.ContainsRune(got, '\x00') || strings.ContainsRune(got, '\n') {
		t.Fatalf("published terminal reason not sanitized of control chars: %q", got)
	}
	if len(got) > maxExitLineLen+len("…") {
		t.Fatalf("published terminal reason not length-capped: len=%d want <= %d", len(got), maxExitLineLen+len("…"))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("published terminal reason is not valid UTF-8 (rune split at the cap): %q", got)
	}
}

// Acceptance gap: a start racing Shutdown must not store a session. Park Start
// after Spawn and before the session store, let Shutdown win, then release; the
// spawned process must be stopped and no session stored.
func TestAgentSessionShutdownDuringStartDoesNotStoreSession(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))

	ensureDone := make(chan error, 1)
	go func() {
		ensureDone <- supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"})
	}()
	<-factory.startEntered
	supervisor.Shutdown()
	close(factory.startRelease)
	<-ensureDone

	factory.mu.Lock()
	spawned := len(factory.processes)
	var proc *fakeRuntimeProcess
	if spawned == 1 {
		proc = factory.processes[0]
	}
	factory.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("want exactly one spawned process, got %d", spawned)
	}
	proc.mu.Lock()
	stopped := proc.stopped
	proc.mu.Unlock()
	if !stopped {
		t.Fatal("a runtime spawned during shutdown must be stopped, not left as a zombie")
	}
	supervisor.mu.Lock()
	sessions := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("shutdown-raced start stored a session: want 0, got %d", sessions)
	}
}

// Spoke 12 (facet c): a removal that lands while the replacement is under
// construction must invalidate the conditional swap — the fresh process is
// reaped and the removed agent is not resurrected.
func TestAgentSessionRemovalDuringReplacementConstructionDoesNotResurrect(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	restartDone := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)

	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})

	close(proc1.events)
	<-factory.startEntered

	if err := supervisor.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}
	close(factory.startRelease)
	<-restartDone

	supervisor.mu.Lock()
	sessions := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("removed agent resurrected via a mid-construction replacement: want 0 sessions, got %d", sessions)
	}
	repl := latestFakeProcess(factory)
	repl.mu.Lock()
	stopped := repl.stopped
	repl.mu.Unlock()
	if !stopped {
		t.Fatal("the conditional-swap loss did not reap the freshly constructed process")
	}
}

// Spoke 12: a config refresh during the park (before construction) makes the
// replacement use the LATEST desired spec, not the death-time clone.
func TestAgentSessionConfigRefreshBeforeConstructionUsesLatestSpec(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	parked := make(chan struct{})
	release := make(chan struct{})
	restartDone := make(chan struct{})
	supervisor.restartSleep = func(time.Duration) { close(parked); <-release }
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex", SystemPrompt: "old instructions"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	close(proc1.events)
	<-parked

	if err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Kind: "codex", SystemPrompt: "new instructions"}}); err != nil {
		t.Fatalf("reconcile refresh: %v", err)
	}
	close(release)
	<-restartDone

	spec := latestSpawnSpec(t, factory)
	if spec.Instructions != "new instructions" {
		t.Fatalf("replacement used stale spec: Instructions=%q, want %q", spec.Instructions, "new instructions")
	}
}

// Spoke 12: a config refresh that lands WHILE construction is blocked bumps the
// spec revision; the conditional swap (token x revision) reaps the stale-spec
// process and the restarter rebuilds from the latest spec.
func TestAgentSessionConfigRefreshDuringConstructionRebuildsLatestSpec(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	restartDone := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex", SystemPrompt: "old instructions"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)

	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})

	close(proc1.events)
	<-factory.startEntered
	if err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Kind: "codex", SystemPrompt: "new instructions"}}); err != nil {
		t.Fatalf("reconcile refresh during construction: %v", err)
	}
	close(factory.startRelease)
	<-restartDone

	supervisor.mu.Lock()
	var got string
	if sess := supervisor.sessions["agent_1"]; sess != nil && sess.agent != nil {
		got = sess.agent.SystemPrompt
	}
	live := supervisor.sessions["agent_1"] != nil && !supervisor.sessions["agent_1"].dead
	supervisor.mu.Unlock()
	if !live {
		t.Fatal("no live replacement session published after a mid-construction refresh")
	}
	if got != "new instructions" {
		t.Fatalf("mid-construction refresh published stale spec: %q, want %q", got, "new instructions")
	}
}

// Spoke 13: a StartTurn success landing after a fast TurnCompleted (which set the
// same live generation idle during the unlocked WriteStdin) must NOT revive it.
func TestAgentSessionFastCompletionDoesNotReviveWorking(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	proc1.mu.Lock()
	proc1.startTurnEntered = make(chan struct{}, 1)
	proc1.startTurnRelease = make(chan struct{})
	proc1.mu.Unlock()

	notifyDone := make(chan error, 1)
	go func() {
		notifyDone <- supervisor.ScheduleNotificationTurn(context.Background(), current, "do work", "sig-forme", "")
	}()
	<-proc1.startTurnEntered

	proc1.events <- RuntimeEvent{Kind: RuntimeEventTurnCompleted}
	waitSessionState(t, supervisor, "agent_1", "idle")

	close(proc1.startTurnRelease)
	if err := <-notifyDone; err != nil {
		t.Fatalf("schedule notification: %v", err)
	}
	if st, turn := sessionState(supervisor, "agent_1"); st != "idle" || turn != "" {
		t.Fatalf("stale StartTurn success revived working: state=%q turn=%q, want idle/\"\"", st, turn)
	}
}

// Spoke 12 (edge 1): a replacement construction failure re-arms the next capped
// attempt against the same token instead of stranding it.
func TestAgentSessionFailedReplacementReArmsNextAttempt(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	var mu sync.Mutex
	var delays []time.Duration
	supervisor.restartSleep = func(d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		n := len(delays)
		mu.Unlock()
		if n == 2 {
			factory.mu.Lock()
			factory.startErr = nil
			factory.mu.Unlock()
		}
	}
	restarted := make(chan struct{}, 1)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	factory.mu.Lock()
	factory.startErr = errors.New("replacement start boom")
	factory.mu.Unlock()

	close(proc1.events)
	<-restarted

	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("failed replacement did not re-arm at the next capped delay: delays=%v, want %v", got, want)
	}
	if st, _ := sessionState(supervisor, "agent_1"); st != "idle" {
		t.Fatalf("re-armed replacement did not publish a live session: state=%q", st)
	}
	supervisor.mu.Lock()
	sess := supervisor.sessions["agent_1"]
	live := sess != nil && !sess.dead && !sess.restartPending
	supervisor.mu.Unlock()
	if !live {
		t.Fatal("token left stranded after re-armed replacement")
	}
}

// Spoke 12 (edge 1): Shutdown during a re-armed (post-failure) park cancels the
// restart — no replacement is published.
func TestAgentSessionShutdownCancelsReArmedReplacement(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))

	parked := make(chan struct{})
	release := make(chan struct{})
	restartDone := make(chan struct{})
	var n int
	supervisor.restartSleep = func(time.Duration) {
		n++
		if n == 2 {
			close(parked)
			<-release
		}
	}
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)
	factory.mu.Lock()
	factory.startErr = errors.New("replacement start boom")
	factory.mu.Unlock()

	close(proc1.events)
	<-parked
	supervisor.Shutdown()
	close(release)
	<-restartDone

	supervisor.mu.Lock()
	sessions := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("shutdown during a re-armed park still published: want 0 sessions, got %d", sessions)
	}
}

func latestSpawnSpec(t *testing.T, f *fakeRuntimeDriver) RuntimeSpawnSpec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.spawnSpecs) == 0 {
		t.Fatal("no spawn specs recorded")
	}
	return f.spawnSpecs[len(f.spawnSpecs)-1]
}

func waitSessionState(t *testing.T, s *agentSessionSupervisor, agentID, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if st, _ := sessionState(s, agentID); st == want {
			return
		}
		select {
		case <-deadline:
			st, _ := sessionState(s, agentID)
			t.Fatalf("session %s state=%q, want %q", agentID, st, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// Spoke 14: a fresh nil-slot start racing removal must not publish. Reconcile
// invalidates the fresh-start claim (and cancels its construction context), so
// the removed agent is never stored.
func TestAgentSessionFreshStartRemovalDoesNotStore(t *testing.T) {
	factory := newFakeRuntimeDriver()
	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})
	// The construction finishes (ignores ctx cancel) so this pins the publish-time
	// claim backstop: even a completed fresh construction must not store a removed
	// agent.
	factory.startIgnoreCtx = true
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()

	ensureDone := make(chan error, 1)
	go func() {
		ensureDone <- supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"})
	}()
	<-factory.startEntered // fresh construction blocked in Start

	// Backend removes the agent while the fresh construction is in flight, then the
	// construction completes anyway — the claim must refuse the store.
	if err := supervisor.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}
	close(factory.startRelease)
	<-ensureDone

	supervisor.mu.Lock()
	sessions := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("fresh start stored a removed agent: want 0 sessions, got %d", sessions)
	}
	proc := factory.only(t)
	proc.mu.Lock()
	stopped := proc.stopped
	proc.mu.Unlock()
	if !stopped {
		t.Fatal("the removed fresh construction's process was not reaped")
	}
}

// Spoke 15: a byte-identical desired refresh during a blocked replacement
// construction must NOT reap it (agentRev only advances on a real spec change).
func TestAgentSessionIdenticalRefreshDuringConstructionPreservesReplacement(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	restartDone := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	spec := &agent{ID: "agent_1", Kind: "codex", SystemPrompt: "same instructions"}
	if err := supervisor.ensureSession(context.Background(), spec); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)

	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})

	close(proc1.events)
	<-factory.startEntered

	// Identical desired spec (no spawn-relevant change) reconciled during construction.
	if err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Kind: "codex", SystemPrompt: "same instructions"}}); err != nil {
		t.Fatalf("identical reconcile: %v", err)
	}
	close(factory.startRelease)
	<-restartDone

	factory.mu.Lock()
	procs := len(factory.processes)
	factory.mu.Unlock()
	if procs != 2 {
		t.Fatalf("identical refresh reaped/churned the in-flight replacement: want 2 processes, got %d", procs)
	}
	supervisor.mu.Lock()
	live := supervisor.sessions["agent_1"] != nil && !supervisor.sessions["agent_1"].dead
	supervisor.mu.Unlock()
	if !live {
		t.Fatal("replacement was not published after an identical refresh")
	}
}

// Spoke 16: Shutdown cancels a replacement construction blocked in Start and
// reaps it promptly, without a manual release (Deniz's teardown-blocking cut).
func TestAgentSessionShutdownCancelsBlockedReplacementConstruction(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	supervisor.restartSleep = func(time.Duration) {}
	restartDone := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)

	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{}) // never released

	close(proc1.events)
	<-factory.startEntered // replacement blocked in Start

	supervisor.Shutdown() // must cancel the blocked construction, not wait for release
	select {
	case <-restartDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not cancel the blocked replacement construction (teardown stalled)")
	}

	repl := latestFakeProcess(factory)
	repl.mu.Lock()
	stopped := repl.stopped
	repl.mu.Unlock()
	if !stopped {
		t.Fatal("blocked replacement construction was not reaped on Shutdown")
	}
	supervisor.mu.Lock()
	sessions := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("Shutdown-cancelled construction still published: want 0 sessions, got %d", sessions)
	}
}

// Spoke 16: removal cancels a replacement construction blocked in Start and
// reaps it promptly, without a manual release.
func TestAgentSessionRemovalCancelsBlockedReplacementConstruction(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	restartDone := make(chan struct{})
	supervisor.testHookRestartComplete = func() { close(restartDone) }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc1 := factory.only(t)

	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{}) // never released

	close(proc1.events)
	<-factory.startEntered

	if err := supervisor.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}
	select {
	case <-restartDone:
	case <-time.After(2 * time.Second):
		t.Fatal("removal did not cancel the blocked replacement construction")
	}

	repl := latestFakeProcess(factory)
	repl.mu.Lock()
	stopped := repl.stopped
	repl.mu.Unlock()
	if !stopped {
		t.Fatal("blocked replacement construction was not reaped on removal")
	}
	supervisor.mu.Lock()
	sessions := len(supervisor.sessions)
	supervisor.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("removal-cancelled construction still published: want 0 sessions, got %d", sessions)
	}
}

// Spoke 16 guard: the deferred Stop is armed right after Spawn to reap on failure
// paths, so the publish CAS must transfer ownership (started=false) — a
// successfully published runtime must NOT be reaped by the defer.
func TestAgentSessionPublishedRuntimeIsNotReaped(t *testing.T) {
	factory := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(factory))
	defer supervisor.Shutdown()
	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	proc := factory.only(t)
	proc.mu.Lock()
	started, stopped := proc.started, proc.stopped
	proc.mu.Unlock()
	if !started || stopped {
		t.Fatalf("published runtime reaped by the construction defer: started=%v stopped=%v", started, stopped)
	}
	supervisor.mu.Lock()
	live := supervisor.sessions["agent_1"] != nil && !supervisor.sessions["agent_1"].dead
	supervisor.mu.Unlock()
	if !live {
		t.Fatal("session was not published")
	}
}
