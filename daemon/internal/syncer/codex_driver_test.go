package syncer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCodexRuntimeApp struct {
	started  bool
	stopped  bool
	events   chan appServerEvent
	pid      int
	exitInfo RuntimeExitInfo

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

func (f *countingStopCodexRuntimeApp) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	return nil
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
	codexPath := writeFakeCodex(t, `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo "codex 0.1.0"
	exit 0
fi
if [ "$1" = "app-server" ]; then
	exit 2
fi
exit 2
`)
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

func writeFakeCodex(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

func (f *fakeCodexRuntimeApp) Start(ctx context.Context) error {
	_ = ctx
	f.started = true
	return nil
}

func (f *fakeCodexRuntimeApp) Stop() error {
	f.stopped = true
	return nil
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
	close(app.events)

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
		app:      app,
		events:   make(chan RuntimeEvent),
		stopping: make(chan struct{}),
	}

	forwardDone := make(chan struct{})
	go func() {
		process.forwardEvents()
		close(forwardDone)
	}()
	app.events <- appServerEvent{Method: "turn/completed", Params: rawJSON(t, `{}`)}
	close(app.events)
	select {
	case <-forwardDone:
		t.Fatal("lifecycle forward returned without a runtime consumer")
	case <-time.After(100 * time.Millisecond):
	}

	if err := process.Stop(); err != nil {
		t.Fatalf("stop process: %v", err)
	}
	select {
	case <-forwardDone:
	case <-time.After(100 * time.Millisecond):
		// Release the held-head goroutine so the red test does not leak it.
		<-process.events
		<-forwardDone
		t.Fatal("Stop did not release lifecycle send blocked in codexRuntimeProcess.forwardEvents")
	}
}

func TestCodexRuntimeStopIsConcurrentAndIdempotent(t *testing.T) {
	app := &countingStopCodexRuntimeApp{fakeCodexRuntimeApp: newFakeCodexRuntimeApp()}
	process := &codexRuntimeProcess{
		app:      app,
		events:   make(chan RuntimeEvent),
		stopping: make(chan struct{}),
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
