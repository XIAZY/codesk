package syncer

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCodexRuntimeApp struct {
	started     bool
	stopped     bool
	events      chan appServerEvent
	eventsOnce  sync.Once
	pid         int
	exitInfo    RuntimeExitInfo
	activitySeq uint64

	threadStartID string
	turnStartID   string
	calls         []fakeCodexRuntimeCall
}

type fakeCodexRuntimeCall struct {
	method       string
	sessionID    string
	turnID       string
	cwd          string
	text         string
	instructions string
}

type countingStopCodexRuntimeApp struct {
	*fakeCodexRuntimeApp
	mu        sync.Mutex
	stopCalls int
}

type blockingStopCodexRuntimeApp struct {
	*fakeCodexRuntimeApp
	stopEntered chan struct{}
	releaseStop chan struct{}
}

func (f *countingStopCodexRuntimeApp) Stop() error {
	f.mu.Lock()
	f.stopCalls++
	f.mu.Unlock()
	return f.fakeCodexRuntimeApp.Stop()
}

func (f *countingStopCodexRuntimeApp) stopCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalls
}

func newFakeCodexRuntimeApp() *fakeCodexRuntimeApp {
	return &fakeCodexRuntimeApp{
		events:        make(chan appServerEvent, 8),
		pid:           4321,
		threadStartID: "thread_new",
		turnStartID:   "turn_new",
	}
}

func TestCodexDriverDetectRequiresAppServer(t *testing.T) {
	codexPath := fakeProcessCommand(t, fakeProcessCodexWithoutAppServer)
	driver := newCodexDriver(Config{CodexCommand: codexPath})

	detection := driver.Detect(context.Background())

	if detection.Kind != RuntimeCodex {
		t.Fatalf("expected codex runtime detection, got %#v", detection)
	}
	if detection.Available {
		t.Fatalf("expected old Codex without app-server to be unavailable, got %#v", detection)
	}
	if detection.Path != codexPath {
		t.Fatalf("expected codex path %q, got %q", codexPath, detection.Path)
	}
	if detection.Version != "codex 0.1.0" {
		t.Fatalf("expected version from --version probe, got %q", detection.Version)
	}
	if !strings.Contains(detection.Reason, "app-server") {
		t.Fatalf("expected app-server unavailable reason, got %#v", detection)
	}
}

func (f *fakeCodexRuntimeApp) Start(ctx context.Context) error {
	_ = ctx
	f.started = true
	return nil
}

func (f *fakeCodexRuntimeApp) Stop() error {
	f.stopped = true
	f.exitInfo = RuntimeExitInfo{Expected: true}
	f.closeEvents()
	return nil
}

func (f *fakeCodexRuntimeApp) closeEvents() {
	f.eventsOnce.Do(func() { close(f.events) })
}

func (f *blockingStopCodexRuntimeApp) Stop() error {
	close(f.stopEntered)
	<-f.releaseStop
	return f.fakeCodexRuntimeApp.Stop()
}

func (f *fakeCodexRuntimeApp) Events() <-chan appServerEvent {
	return f.events
}

func (f *fakeCodexRuntimeApp) PID() int {
	return f.pid
}

func (f *fakeCodexRuntimeApp) ExitInfo() RuntimeExitInfo {
	return f.exitInfo
}

func (f *fakeCodexRuntimeApp) ActivitySeq() uint64 {
	return f.activitySeq
}

func (f *fakeCodexRuntimeApp) ThreadResume(ctx context.Context, sessionID string, cwd string, instructions string) error {
	_ = ctx
	f.calls = append(f.calls, fakeCodexRuntimeCall{
		method:       "ThreadResume",
		sessionID:    sessionID,
		cwd:          cwd,
		instructions: instructions,
	})
	return nil
}

func (f *fakeCodexRuntimeApp) ThreadStart(ctx context.Context, cwd string, instructions string) (string, error) {
	_ = ctx
	f.calls = append(f.calls, fakeCodexRuntimeCall{
		method:       "ThreadStart",
		cwd:          cwd,
		instructions: instructions,
	})
	return f.threadStartID, nil
}

func (f *fakeCodexRuntimeApp) TurnStart(ctx context.Context, sessionID string, text string, cwd string) (string, error) {
	_ = ctx
	f.calls = append(f.calls, fakeCodexRuntimeCall{
		method:    "TurnStart",
		sessionID: sessionID,
		cwd:       cwd,
		text:      text,
	})
	return f.turnStartID, nil
}

func (f *fakeCodexRuntimeApp) TurnSteer(ctx context.Context, sessionID string, turnID string, text string) error {
	_ = ctx
	f.calls = append(f.calls, fakeCodexRuntimeCall{
		method:    "TurnSteer",
		sessionID: sessionID,
		turnID:    turnID,
		text:      text,
	})
	return nil
}

func (f *fakeCodexRuntimeApp) TurnInterrupt(ctx context.Context, sessionID string, turnID string) error {
	_ = ctx
	f.calls = append(f.calls, fakeCodexRuntimeCall{
		method:    "TurnInterrupt",
		sessionID: sessionID,
		turnID:    turnID,
	})
	return nil
}

func TestCodexRuntimeProcessMapsRuntimeInputsToAppServer(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	var gotWorkdir, gotToolToken, gotAgentID string
	driver := &codexDriver{
		factory: func(cfg Config, workdir string, toolToken string, agentID string) codexRuntimeApp {
			_ = cfg
			gotWorkdir = workdir
			gotToolToken = toolToken
			gotAgentID = agentID
			return app
		},
	}
	process, err := driver.Spawn(context.Background(), RuntimeSpawnSpec{
		AgentID:      "agent_1",
		Workdir:      "/tmp/agent",
		ToolToken:    "tool_token",
		Instructions: "driver instructions",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if gotWorkdir != "/tmp/agent" || gotToolToken != "tool_token" || gotAgentID != "agent_1" {
		t.Fatalf("unexpected factory args workdir=%q token=%q agent=%q", gotWorkdir, gotToolToken, gotAgentID)
	}

	if _, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputResumeSession, CWD: "/workspace"}); err == nil || !strings.Contains(err.Error(), "session id is required") {
		t.Fatalf("expected empty resume session id error, got %v", err)
	}
	if len(app.calls) != 0 {
		t.Fatalf("empty resume should not call appserver, got %#v", app.calls)
	}

	resumed, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputResumeSession,
		SessionID: " thread_existing ",
		CWD:       "/workspace",
	})
	if err != nil {
		t.Fatalf("resume session: %v", err)
	}
	if resumed.SessionID != "thread_existing" {
		t.Fatalf("unexpected resume result: %#v", resumed)
	}

	started, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:         RuntimeInputStartSession,
		CWD:          "/workspace/new",
		Instructions: "input instructions",
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if started.SessionID != "thread_new" {
		t.Fatalf("unexpected start session result: %#v", started)
	}

	turn, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputStartTurn,
		SessionID: "thread_new",
		CWD:       "/workspace/new",
		Text:      "do work",
	})
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	if turn.TurnID != "turn_new" {
		t.Fatalf("unexpected turn result: %#v", turn)
	}

	if _, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputSteerTurn,
		SessionID: "thread_new",
		TurnID:    "turn_new",
		Text:      "important follow-up",
	}); err != nil {
		t.Fatalf("steer turn: %v", err)
	}
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputInterruptTurn,
		SessionID: "thread_new",
		TurnID:    "turn_new",
	}); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}

	want := []fakeCodexRuntimeCall{
		{method: "ThreadResume", sessionID: "thread_existing", cwd: "/workspace", instructions: "driver instructions"},
		{method: "ThreadStart", cwd: "/workspace/new", instructions: "input instructions"},
		{method: "TurnStart", sessionID: "thread_new", cwd: "/workspace/new", text: "do work"},
		{method: "TurnSteer", sessionID: "thread_new", turnID: "turn_new", text: "important follow-up"},
		{method: "TurnInterrupt", sessionID: "thread_new", turnID: "turn_new"},
	}
	if !reflect.DeepEqual(app.calls, want) {
		t.Fatalf("unexpected appserver calls:\n got: %#v\nwant: %#v", app.calls, want)
	}
}

func TestCodexRuntimeProcessMapsAppServerEventsToRuntimeEvents(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	process := &codexRuntimeProcess{
		app:          app,
		instructions: "driver instructions",
		events:       make(chan RuntimeEvent, 8),
		eventsDone:   make(chan struct{}),
		stopping:     make(chan struct{}),
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start process: %v", err)
	}
	if !app.started || process.PID() != 4321 {
		t.Fatalf("expected appserver start and pid, started=%v pid=%d", app.started, process.PID())
	}

	app.events <- appServerEvent{Method: "turn/started", Params: rawJSON(t, `{"turn":{"id":"turn_1"}}`)}
	app.events <- appServerEvent{Method: "turn/completed", Params: rawJSON(t, `{}`)}
	app.events <- appServerEvent{Method: "turn/failed", Params: rawJSON(t, `{}`)}
	app.events <- appServerEvent{Method: "thread/status/changed", Params: rawJSON(t, `{"status":{"type":"working"}}`)}
	app.events <- appServerEvent{Method: "thread/status/changed", Params: rawJSON(t, `{"status":{"type":"idle"}}`)}
	app.events <- appServerEvent{Method: "unknown/event", Params: rawJSON(t, `{}`)}
	app.closeEvents()

	want := []RuntimeEvent{
		{Kind: RuntimeEventTurnStarted, TurnID: "turn_1"},
		{Kind: RuntimeEventTurnCompleted},
		{Kind: RuntimeEventTurnFailed},
		{Kind: RuntimeEventIdle},
	}
	if got := collectRuntimeEvents(t, process.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected runtime events:\n got: %#v\nwant: %#v", got, want)
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("stop process: %v", err)
	}
	if !app.stopped {
		t.Fatal("expected appserver stop")
	}
}

func TestCodexRuntimeStopReleasesBlockedLifecycleForward(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	process := &codexRuntimeProcess{
		app:        app,
		events:     make(chan RuntimeEvent),
		eventsDone: make(chan struct{}),
		stopping:   make(chan struct{}),
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start process: %v", err)
	}
	app.events <- appServerEvent{Method: "turn/completed", Params: rawJSON(t, `{}`)}
	deadline := time.Now().Add(time.Second)
	for len(app.events) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("forwarder did not receive the blocked lifecycle event")
		}
		time.Sleep(time.Millisecond)
	}

	if err := process.Stop(); err != nil {
		t.Fatalf("stop process: %v", err)
	}
	if got := collectRuntimeEvents(t, process.Events()); len(got) != 0 {
		t.Fatalf("stopped process emitted blocked lifecycle events: %#v", got)
	}
}

func TestCodexRuntimeStopIsConcurrentAndIdempotent(t *testing.T) {
	app := &countingStopCodexRuntimeApp{fakeCodexRuntimeApp: newFakeCodexRuntimeApp()}
	process := &codexRuntimeProcess{
		app:        app,
		events:     make(chan RuntimeEvent),
		eventsDone: make(chan struct{}),
		stopping:   make(chan struct{}),
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start process: %v", err)
	}

	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- process.Stop()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Stop returned error: %v", err)
		}
	}
	if got := app.stopCallCount(); got != 1 {
		t.Fatalf("underlying app Stop calls = %d, want 1", got)
	}
	select {
	case <-process.stopping:
	default:
		t.Fatal("process stop signal was not closed")
	}
}

func rawJSON(t *testing.T, value string) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		t.Fatalf("invalid test json %s: %v", value, err)
	}
	return raw
}

func collectRuntimeEvents(t *testing.T, events <-chan RuntimeEvent) []RuntimeEvent {
	t.Helper()
	var got []RuntimeEvent
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-deadline:
			t.Fatalf("timed out waiting for runtime events; got %#v", got)
		}
	}
}

// Blocker 8 (Cluster B): when Stop wins while a final lifecycle event is still
// pending, the wrapper must NOT close its public Events() before the underlying
// app has published ExitInfo. The app closes its own event channel only after
// its exit goroutine records ExitInfo; closing the wrapper early on `stopping`
// exposes a zero snapshot (Expected=false) and makes a deliberate Stop look
// like a transient crash that gets restarted.
func TestCodexRuntimeWrapperWaitsForAppExitInfoBeforeClosing(t *testing.T) {
	app := &blockingStopCodexRuntimeApp{
		fakeCodexRuntimeApp: newFakeCodexRuntimeApp(),
		stopEntered:         make(chan struct{}),
		releaseStop:         make(chan struct{}),
	}
	process := &codexRuntimeProcess{
		app:        app,
		events:     make(chan RuntimeEvent), // unbuffered: forward blocks on send with no consumer
		eventsDone: make(chan struct{}),
		stopping:   make(chan struct{}),
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start process: %v", err)
	}

	// A final lifecycle event is in flight; forwardEvents receives it and blocks
	// trying to hand it to a consumer that never reads.
	app.events <- appServerEvent{Method: "turn/completed", Params: rawJSON(t, `{}`)}
	deadline := time.Now().Add(time.Second)
	for len(app.events) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("forwarder did not receive the final lifecycle event")
		}
		time.Sleep(time.Millisecond)
	}

	stopResult := make(chan error, 1)
	go func() { stopResult <- process.Stop() }()
	select {
	case <-app.stopEntered:
	case <-time.After(time.Second):
		t.Fatal("RuntimeProcess.Stop did not enter the app stop")
	}

	select {
	case err := <-stopResult:
		t.Fatalf("RuntimeProcess.Stop returned before app exit publication: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	select {
	case _, ok := <-process.Events():
		if !ok {
			t.Fatal("wrapper closed public Events() before the app published ExitInfo")
		}
	default:
	}

	close(app.releaseStop)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("stop process: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RuntimeProcess.Stop did not join event publication")
	}
	if !process.ExitInfo().Expected {
		t.Fatal("ExitInfo lost: a deliberately-stopped process must report Expected=true after Events() closes")
	}
	if got := collectRuntimeEvents(t, process.Events()); len(got) != 0 {
		t.Fatalf("stopped process emitted final blocked lifecycle events: %#v", got)
	}
}
