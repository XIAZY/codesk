package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

type fakeAppServerFactory struct {
	mu             sync.Mutex
	apps           []*fakeAppServer
	startEntered   chan struct{}
	startRelease   chan struct{}
	startErr       error
	threadStartErr error
	turnSteerErr   error
}

type fakeTurnStart struct {
	threadID string
	prompt   string
	cwd      string
}

type fakeTurnSteer struct {
	threadID string
	turnID   string
	message  string
}

type fakeAppServer struct {
	mu sync.Mutex

	events chan appServerEvent

	started            bool
	stopped            bool
	threadStartCount   int
	threadResumeIDs    []string
	threadInstructions []string
	failResume         bool
	turnStarts         []fakeTurnStart
	turnSteers         []fakeTurnSteer
	nextTurn           int
	startEntered       chan struct{}
	startRelease       chan struct{}
	startErr           error
	threadStartErr     error
	turnSteerErr       error
}

func newFakeAppServerFactory() *fakeAppServerFactory {
	return &fakeAppServerFactory{}
}

func (f *fakeAppServerFactory) new(cfg Config, workdir string, toolToken string, agentID string) appServerClient {
	app := &fakeAppServer{
		events:         make(chan appServerEvent, 16),
		startEntered:   f.startEntered,
		startRelease:   f.startRelease,
		startErr:       f.startErr,
		threadStartErr: f.threadStartErr,
		turnSteerErr:   f.turnSteerErr,
	}
	f.mu.Lock()
	f.apps = append(f.apps, app)
	f.mu.Unlock()
	return app
}

func agentSessionTestConfig(t *testing.T, workspaceRoot string) Config {
	t.Helper()
	return Config{
		DataDir:            t.TempDir(),
		WorkspaceID:        "workspace:test",
		AgentWorkspaceRoot: workspaceRoot,
	}
}

func (f *fakeAppServerFactory) only(t *testing.T) *fakeAppServer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.apps) != 1 {
		t.Fatalf("expected one app server, got %d", len(f.apps))
	}
	return f.apps[0]
}

func (a *fakeAppServer) Start(ctx context.Context) error {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	if a.startEntered != nil {
		select {
		case a.startEntered <- struct{}{}:
		default:
		}
	}
	if a.startRelease != nil {
		select {
		case <-a.startRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.startErr != nil {
		return a.startErr
	}
	return nil
}

func (a *fakeAppServer) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	return nil
}

func (a *fakeAppServer) ThreadStart(ctx context.Context, cwd string, instructions string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.threadStartCount++
	a.threadInstructions = append(a.threadInstructions, instructions)
	if a.threadStartErr != nil {
		return "", a.threadStartErr
	}
	return "thread_new", nil
}

func (a *fakeAppServer) ThreadResume(ctx context.Context, threadID string, cwd string, instructions string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.threadResumeIDs = append(a.threadResumeIDs, threadID)
	a.threadInstructions = append(a.threadInstructions, instructions)
	if a.failResume {
		return fmt.Errorf("resume failed")
	}
	return nil
}

func (a *fakeAppServer) TurnStart(ctx context.Context, threadID string, prompt string, cwd string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextTurn++
	turnID := fmt.Sprintf("turn_%d", a.nextTurn)
	a.turnStarts = append(a.turnStarts, fakeTurnStart{threadID: threadID, prompt: prompt, cwd: cwd})
	return turnID, nil
}

func (a *fakeAppServer) TurnSteer(ctx context.Context, threadID string, turnID string, message string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnSteers = append(a.turnSteers, fakeTurnSteer{threadID: threadID, turnID: turnID, message: message})
	return a.turnSteerErr
}

func (a *fakeAppServer) TurnInterrupt(ctx context.Context, threadID string, turnID string) error {
	return nil
}

func (a *fakeAppServer) Events() <-chan appServerEvent {
	return a.events
}

func (a *fakeAppServer) PID() int {
	return 123
}

func TestAgentSessionStartsThreadWithDeveloperInstructions(t *testing.T) {
	factory := newFakeAppServerFactory()
	type statusUpdate struct {
		agentID string
		payload updateAgentSessionRequest
	}
	updates := make(chan statusUpdate, 8)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		updates <- statusUpdate{agentID: agentID, payload: payload}
		return nil
	}
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Handle: "swe", Kind: "codex", SystemPrompt: "shared prompt"}

	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	app := factory.only(t)
	if !app.started || app.threadStartCount != 1 {
		t.Fatalf("expected app start and thread start, app=%#v", app)
	}
	if len(app.threadInstructions) != 1 || app.threadInstructions[0] != "shared prompt" {
		t.Fatalf("expected shared prompt in thread instructions, got %#v", app.threadInstructions)
	}
	update := <-updates
	if update.agentID != "agent_1" || update.payload.Status != "idle" || update.payload.CodexThreadID != "thread_new" {
		t.Fatalf("unexpected session update: %#v", update)
	}
}

func TestAgentSessionReconcileReturnsStartupError(t *testing.T) {
	factory := newFakeAppServerFactory()
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
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), updater, factory.new)
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

func TestAgentSessionReconcileReturnsThreadStartError(t *testing.T) {
	factory := newFakeAppServerFactory()
	factory.threadStartErr = errors.New("thread start failed")
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()

	err := supervisor.Reconcile(context.Background(), []*agent{{ID: "agent_1", Handle: "swe", Kind: "codex"}})
	if err == nil {
		t.Fatal("expected reconcile thread start error")
	}
	var startupErr *agentSessionStartupError
	if !errors.As(err, &startupErr) || !strings.Contains(startupErr.Err.Error(), "thread start failed") {
		t.Fatalf("expected wrapped thread start error, got %T %v", err, err)
	}
	app := factory.only(t)
	if !app.stopped {
		t.Fatal("failed thread start should stop the app server")
	}
}

func TestAgentSessionConcurrentEnsureStartsOneAppServer(t *testing.T) {
	factory := newFakeAppServerFactory()
	factory.startEntered = make(chan struct{}, 1)
	factory.startRelease = make(chan struct{})
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
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
	startedBeforeRelease := len(factory.apps)
	factory.mu.Unlock()
	if startedBeforeRelease != 1 {
		t.Fatalf("expected one in-flight app server before release, got %d", startedBeforeRelease)
	}
	close(factory.startRelease)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensure session returned error: %v", err)
		}
	}
	app := factory.only(t)
	if !app.started || app.threadStartCount != 1 {
		t.Fatalf("expected one app start and one thread start, app=%#v", app)
	}
}

func TestAgentSessionIgnoresEventsFromStaleAppServer(t *testing.T) {
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	active := factory.only(t)
	stale := &fakeAppServer{events: make(chan appServerEvent, 1)}

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

func TestAgentSessionResumesExistingThread(t *testing.T) {
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex", CodexThreadID: "thread_existing"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	app := factory.only(t)
	if len(app.threadResumeIDs) != 1 || app.threadResumeIDs[0] != "thread_existing" {
		t.Fatalf("expected thread resume, got %#v", app.threadResumeIDs)
	}
	if app.threadStartCount != 0 {
		t.Fatalf("expected no new thread when resume succeeds")
	}
}

func TestAgentSessionIdleNotificationStartsOncePerInboxSignature(t *testing.T) {
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "first", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule first: %v", err)
	}
	app := factory.only(t)
	if len(app.turnStarts) != 1 {
		t.Fatalf("expected first turn start, got %d", len(app.turnStarts))
	}
	supervisor.markIdle("agent_1", app, true)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "same", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule same: %v", err)
	}
	if len(app.turnStarts) != 1 {
		t.Fatalf("expected same inbox signature to be suppressed, got %d starts", len(app.turnStarts))
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "changed", "for-me:v2", ""); err != nil {
		t.Fatalf("schedule changed: %v", err)
	}
	if len(app.turnStarts) != 2 {
		t.Fatalf("expected changed signature to start another turn, got %d", len(app.turnStarts))
	}
}

func TestAgentSessionBusyForMeSteersOnceAndQueuesFollowup(t *testing.T) {
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "", "general:v1"); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	app := factory.only(t)
	if len(app.turnStarts) != 1 {
		t.Fatalf("expected active turn start")
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule for-me while busy: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me duplicate", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule duplicate for-me while busy: %v", err)
	}
	if len(app.turnSteers) != 1 {
		t.Fatalf("expected exactly one steer for same for-me signature, got %d", len(app.turnSteers))
	}
	forMePending, generalPending := supervisor.Pending("agent_1")
	if !forMePending || generalPending {
		t.Fatalf("expected for-me followup only, got forMe=%v general=%v", forMePending, generalPending)
	}
	supervisor.markIdle("agent_1", app, true)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "followup", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule followup: %v", err)
	}
	if len(app.turnStarts) != 2 {
		t.Fatalf("expected follow-up turn after busy for-me change, got %d starts", len(app.turnStarts))
	}
}

func TestAgentSessionBusyGeneralQueuesFollowupWithoutSteer(t *testing.T) {
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	app := factory.only(t)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "general", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule general while busy: %v", err)
	}
	if len(app.turnSteers) != 0 {
		t.Fatalf("expected no steer for general-only change, got %d", len(app.turnSteers))
	}
	forMePending, generalPending := supervisor.Pending("agent_1")
	if forMePending || !generalPending {
		t.Fatalf("expected general followup only, got forMe=%v general=%v", forMePending, generalPending)
	}
	supervisor.markIdle("agent_1", app, true)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "followup", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule followup: %v", err)
	}
	if len(app.turnStarts) != 2 {
		t.Fatalf("expected follow-up turn after busy general change, got %d starts", len(app.turnStarts))
	}
}

func TestAgentSessionBusyGeneralCoalescesFollowupLogAndKeepsLatestSignature(t *testing.T) {
	factory := newFakeAppServerFactory()
	workspaceRoot := t.TempDir()
	cfg := agentSessionTestConfig(t, workspaceRoot)
	supervisor := newAgentSessionSupervisor(cfg, nil, factory.new)
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
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "first", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule first: %v", err)
	}
	app := factory.only(t)
	supervisor.markIdle("agent_1", app, false)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "retry", "for-me:v1", ""); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if len(app.turnStarts) != 2 {
		t.Fatalf("expected failed turn signature to be retried, got %d starts", len(app.turnStarts))
	}
}

func TestAgentSessionBusyForMeSteersEachChangedInboxSignature(t *testing.T) {
	factory := newFakeAppServerFactory()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "", "general:v1"); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	app := factory.only(t)
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me v1", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("schedule first for-me while busy: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me v2", "for-me:v2", "general:v1"); err != nil {
		t.Fatalf("schedule second for-me while busy: %v", err)
	}
	if len(app.turnSteers) != 2 {
		t.Fatalf("expected a steer for each changed for-me inbox signature, got %d", len(app.turnSteers))
	}
}

func TestAgentSessionNoActiveTurnSteerErrorIsNotFatal(t *testing.T) {
	factory := newFakeAppServerFactory()
	factory.turnSteerErr = fmt.Errorf("app-server turn/steer failed: no active turn to steer")
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, factory.new)
	defer supervisor.Shutdown()
	current := &agent{ID: "agent_1", Kind: "codex"}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "active", "", "general:v1"); err != nil {
		t.Fatalf("schedule active: %v", err)
	}
	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "for-me", "for-me:v1", "general:v1"); err != nil {
		t.Fatalf("no-active-turn steer should not fail automation: %v", err)
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
