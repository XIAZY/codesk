package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexAppServerPublishedChildOutlivesConstructionContext(t *testing.T) {
	codexPath := fakeProcessCommand(t, fakeProcessCodexPersistent)
	client := newCodexAppServer(Config{CodexCommand: codexPath, DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")

	constructionCtx, cancelConstruction := context.WithCancel(context.Background())
	if err := client.Start(constructionCtx); err != nil {
		cancelConstruction()
		t.Fatalf("start app-server: %v", err)
	}
	threadID, err := client.ThreadStart(constructionCtx, t.TempDir(), "instructions")
	if err != nil {
		cancelConstruction()
		t.Fatalf("start thread: %v", err)
	}
	pid := client.PID()
	if pid <= 0 {
		cancelConstruction()
		t.Fatalf("published app-server PID = %d", pid)
	}

	// The supervisor performs this cancel immediately after publishing the
	// RuntimeProcess. It must cancel only construction RPCs, not the OS child.
	cancelConstruction()
	assertCodexAppServerAlive(t, client, 200*time.Millisecond)

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 2*time.Second)
	turnID, err := client.TurnStart(requestCtx, threadID, "after cancel", t.TempDir())
	cancelRequest()
	if err != nil {
		t.Fatalf("start turn after construction cancel: %v", err)
	}
	if turnID != "turn_after_cancel" {
		t.Fatalf("turn id = %q, want turn_after_cancel", turnID)
	}
	if got := client.PID(); got != pid {
		t.Fatalf("app-server PID changed after construction cancel: got %d, want %d", got, pid)
	}
	if err := client.Stop(); err != nil {
		t.Fatalf("stop app-server: %v", err)
	}
}

func TestCodexAppServerConcurrentStopBroadcastsAfterJoinedExit(t *testing.T) {
	codexPath := fakeProcessCommand(t, fakeProcessCodexPersistent)
	client := newCodexAppServer(Config{CodexCommand: codexPath, DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")
	exitRecorded := make(chan struct{})
	releaseExit := make(chan struct{})
	client.testHookBeforeExitComplete = func() {
		close(exitRecorded)
		<-releaseExit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start app-server: %v", err)
	}

	const callers = 16
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- client.Stop() }()
	}
	select {
	case <-exitRecorded:
	case <-time.After(2 * time.Second):
		t.Fatal("exit was not recorded after Stop killed the child")
	}
	select {
	case err := <-results:
		t.Fatalf("Stop returned before app event/exited completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	assertClosed(t, client.stderrDone, "stderr reader")
	assertClosed(t, client.readDone, "stdout reader")
	if client.cmd == nil || client.cmd.ProcessState == nil {
		t.Fatalf("process was not waited before exit completion: cmd=%v", client.cmd)
	}
	if info := client.ExitInfo(); !info.Expected {
		t.Fatalf("ExitInfo before completion = %#v, want Expected=true", info)
	}
	select {
	case _, ok := <-client.Events():
		if !ok {
			t.Fatal("app event stream closed before the completion boundary")
		}
	default:
	}

	close(releaseExit)
	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent Stop returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Stop caller did not observe broadcast completion")
		}
	}
	assertClosed(t, client.exited, "app-server exit")
	assertAppServerEventsClosed(t, client.Events())

	returned := make(chan error, 1)
	go func() { returned <- client.Stop() }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("later Stop returned error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("later Stop did not reuse the closed completion broadcast")
	}
}

func TestCodexRuntimeStartFailureAndSupervisorStopJoinChild(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		wantError string
		timeout   time.Duration
	}{
		{name: "provider failure", env: "FAKE_CODEX_FAIL_INITIALIZE", wantError: "fake initialize failure", timeout: 5 * time.Second},
		{name: "construction cancel", env: "FAKE_CODEX_BLOCK_INITIALIZE", wantError: context.DeadlineExceeded.Error(), timeout: 100 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.env, "1")
			cfg := Config{CodexCommand: fakeProcessCommand(t, fakeProcessCodexPersistent), DataDir: t.TempDir()}
			runtime, err := newCodexDriver(cfg).Spawn(context.Background(), RuntimeSpawnSpec{
				AgentID: "agent_codex",
				Workdir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("spawn runtime: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			err = runtime.Start(ctx)
			cancel()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Start error = %v, want %q", err, test.wantError)
			}
			if test.env == "FAKE_CODEX_BLOCK_INITIALIZE" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("canceled handshake error = %v, want deadline exceeded", err)
			}

			process := runtime.(*codexRuntimeProcess)
			client := process.app.(*codexAppServer)
			assertClosed(t, client.exited, "failed app-server exit")
			if client.cmd == nil || client.cmd.ProcessState == nil {
				t.Fatalf("failed unpublished child was not joined: cmd=%v", client.cmd)
			}
			if info := client.ExitInfo(); !info.Expected {
				t.Fatalf("failed unpublished child exit = %#v, want Expected=true", info)
			}

			// startSession's deferred Stop runs after RuntimeProcess.Start returns an
			// error. Concurrent calls model that supervisor cleanup plus callers that
			// also observe cancellation; all must reuse the completed app join.
			const callers = 8
			results := make(chan error, callers)
			for i := 0; i < callers; i++ {
				go func() { results <- runtime.Stop() }()
			}
			for i := 0; i < callers; i++ {
				select {
				case err := <-results:
					if err != nil {
						t.Fatalf("cleanup Stop returned error: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cleanup Stop hung after failed Start")
				}
			}
			if got := collectRuntimeEvents(t, runtime.Events()); len(got) != 0 {
				t.Fatalf("failed runtime emitted events: %#v", got)
			}
		})
	}
}

// Item #5 liveness, blockers 2/17 (Codex read boundary): an unmapped-but-valid
// JSON-RPC object advances the activity generation, while `null` (valid JSON but
// not an object) and malformed input do NOT — the same object-frame contract as
// the Claude boundary, via the shared validator, stamped before dispatch.
func TestCodexReadBoundaryStampsActivityOnObjectFramesOnly(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		advance bool
	}{
		{"unmapped_object", `{"telemetry":"unmapped","progress":0.5}`, true},
		{"mapped_notification", `{"method":"turn/started","params":{}}`, true},
		{"empty_object", `{}`, true},
		{"null", `null`, false},
		{"array", `[1,2,3]`, false},
		{"number", `42`, false},
		{"string", `"hello"`, false},
		{"malformed", `{ not json`, false},
		{"empty_line", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCodexAppServer(Config{DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")
			c.readLoop(strings.NewReader(tc.line + "\n"))
			advanced := c.ActivitySeq() > 0
			if advanced != tc.advance {
				t.Fatalf("line %q: activity advanced=%v, want %v (only a JSON object frame counts)", tc.line, advanced, tc.advance)
			}
		})
	}
}

func TestCodexRuntimePreStartFailureBroadcastsWithoutWaiting(t *testing.T) {
	cfg := Config{CodexCommand: filepath.Join(t.TempDir(), "missing-codex"), DataDir: t.TempDir()}
	runtime, err := newCodexDriver(cfg).Spawn(context.Background(), RuntimeSpawnSpec{
		AgentID: "agent_codex",
		Workdir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("spawn runtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err = runtime.Start(ctx)
	cancel()
	if err == nil {
		t.Fatal("Start unexpectedly succeeded with a missing executable")
	}
	process := runtime.(*codexRuntimeProcess)
	client := process.app.(*codexAppServer)
	assertClosed(t, client.exited, "pre-start failure")
	assertAppServerEventsClosed(t, client.Events())
	if got := client.PID(); got != 0 {
		t.Fatalf("pre-start failure PID = %d, want 0", got)
	}

	returned := make(chan error, 1)
	go func() { returned <- runtime.Stop() }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Stop after pre-start failure: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Stop waited for a nonexistent exit goroutine")
	}
}

func TestCodexAppServerRepeatedStartFailureClosesPartialPipes(t *testing.T) {
	const helperEnv = "NOTTY_SYNCER_FD_CLEANUP_HELPER"
	if os.Getenv(helperEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatalf("resolve descriptor-test executable: %v", err)
		}
		cmd := exec.Command(executable, "-test.run=^TestCodexAppServerRepeatedStartFailureClosesPartialPipes$")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated descriptor cleanup test: %v\n%s", err, output)
		}
		return
	}

	missingCommand := filepath.Join(t.TempDir(), "missing-codex")
	cfg := Config{CodexCommand: missingCommand, DataDir: t.TempDir()}

	// Warm the exec and agent-log paths before taking the descriptor baseline.
	warmup := newCodexAppServer(cfg, t.TempDir(), "", "agent_fd_warmup")
	if err := warmup.Start(context.Background()); err == nil {
		t.Fatal("warmup Start unexpectedly succeeded with a missing executable")
	}
	if err := warmup.Stop(); err != nil {
		t.Fatalf("warmup Stop: %v", err)
	}
	baseline, ok := openFileDescriptorCount()
	if !ok {
		t.Skip("open descriptor accounting requires /proc/self/fd")
	}

	const attempts = 64
	const stopCallers = 4
	for attempt := 0; attempt < attempts; attempt++ {
		client := newCodexAppServer(cfg, t.TempDir(), "", "agent_fd_failure")
		if err := client.Start(context.Background()); err == nil {
			t.Fatalf("attempt %d Start unexpectedly succeeded", attempt)
		}
		assertClosed(t, client.exited, "failed-start exit")
		assertAppServerEventsClosed(t, client.Events())
		if client.cmd != nil {
			t.Fatalf("attempt %d published a command before cmd.Start succeeded: %#v", attempt, client.cmd)
		}
		select {
		case <-client.readDone:
			t.Fatalf("attempt %d launched a stdout reader before cmd.Start succeeded", attempt)
		default:
		}
		select {
		case <-client.stderrDone:
			t.Fatalf("attempt %d launched a stderr reader before cmd.Start succeeded", attempt)
		default:
		}
		select {
		case err := <-client.done:
			t.Fatalf("attempt %d launched a Wait goroutine before cmd.Start succeeded: %v", attempt, err)
		default:
		}

		results := make(chan error, stopCallers)
		for caller := 0; caller < stopCallers; caller++ {
			go func() { results <- client.Stop() }()
		}
		for caller := 0; caller < stopCallers; caller++ {
			select {
			case err := <-results:
				if err != nil {
					t.Fatalf("attempt %d concurrent Stop: %v", attempt, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("attempt %d concurrent Stop did not observe failed-start completion", attempt)
			}
		}
	}

	after, ok := openFileDescriptorCount()
	if !ok {
		t.Fatal("/proc/self/fd disappeared during descriptor test")
	}
	if after != baseline {
		t.Fatalf("repeated cmd.Start failures did not return to the serialized descriptor baseline: before=%d after=%d", baseline, after)
	}
}

func openFileDescriptorCount() (int, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

func TestAgentSessionRealCodexFreshAndRestartChildrenOutliveAttemptCancel(t *testing.T) {
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.CodexCommand = fakeProcessCommand(t, fakeProcessCodexPersistent)
	driver := newCodexDriver(cfg)
	registry := newRuntimeRegistry(driver)
	registry.detections[RuntimeCodex] = RuntimeDetection{Kind: RuntimeCodex, Available: true, Path: cfg.CodexCommand}
	supervisor := newAgentSessionSupervisor(cfg, nil, registry)
	supervisor.restartSleep = func(time.Duration) {}
	restarted := make(chan struct{}, 1)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_real_codex", Kind: "codex"}); err != nil {
		t.Fatalf("ensure real Codex session: %v", err)
	}
	firstProcess, firstApp, firstSessionID := realCodexSession(t, supervisor, "agent_real_codex")
	assertCodexRuntimeAcceptsTurn(t, firstProcess, firstApp, firstSessionID)

	firstApp.mu.Lock()
	firstOSProcess := firstApp.cmd.Process
	firstApp.mu.Unlock()
	if err := firstOSProcess.Kill(); err != nil {
		t.Fatalf("crash first Codex child: %v", err)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not publish a replacement Codex child")
	}
	secondProcess, secondApp, secondSessionID := realCodexSession(t, supervisor, "agent_real_codex")
	if secondProcess == firstProcess {
		t.Fatal("restart retained the dead RuntimeProcess generation")
	}
	if secondApp.PID() <= 0 {
		t.Fatalf("replacement app-server PID = %d", secondApp.PID())
	}
	assertCodexRuntimeAcceptsTurn(t, secondProcess, secondApp, secondSessionID)

	supervisor.Shutdown()
	assertClosed(t, secondApp.exited, "replacement app-server exit")
	if secondApp.cmd == nil || secondApp.cmd.ProcessState == nil {
		t.Fatalf("Shutdown returned before replacement process join: cmd=%v", secondApp.cmd)
	}
	if info := secondApp.ExitInfo(); !info.Expected {
		t.Fatalf("replacement shutdown ExitInfo = %#v, want Expected=true", info)
	}
	select {
	case _, ok := <-secondProcess.Events():
		if ok {
			t.Fatal("RuntimeProcess events retained data after joined Shutdown")
		}
	default:
		t.Fatal("RuntimeProcess events remained open after joined Shutdown")
	}
}

func realCodexSession(t *testing.T, supervisor *agentSessionSupervisor, agentID string) (*codexRuntimeProcess, *codexAppServer, string) {
	t.Helper()
	supervisor.mu.Lock()
	session := supervisor.sessions[agentID]
	var sessionID string
	if session != nil {
		sessionID = session.sessionID
	}
	supervisor.mu.Unlock()
	if session == nil {
		t.Fatalf("agent %s has no published session", agentID)
	}
	process, ok := session.process.(*codexRuntimeProcess)
	if !ok {
		t.Fatalf("agent %s process type = %T, want *codexRuntimeProcess", agentID, session.process)
	}
	client, ok := process.app.(*codexAppServer)
	if !ok {
		t.Fatalf("agent %s app type = %T, want *codexAppServer", agentID, process.app)
	}
	return process, client, sessionID
}

func assertCodexRuntimeAcceptsTurn(t *testing.T, process *codexRuntimeProcess, client *codexAppServer, sessionID string) {
	t.Helper()
	pid := client.PID()
	assertCodexAppServerAlive(t, client, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	result, err := process.WriteStdin(ctx, RuntimeInput{
		Kind:      RuntimeInputStartTurn,
		SessionID: sessionID,
		CWD:       t.TempDir(),
		Text:      "after construction cancel",
	})
	cancel()
	if err != nil {
		t.Fatalf("start turn on published child: %v", err)
	}
	if result.TurnID != "turn_after_cancel" {
		t.Fatalf("turn id = %q, want turn_after_cancel", result.TurnID)
	}
	if got := client.PID(); got != pid {
		t.Fatalf("published child PID changed: got %d, want %d", got, pid)
	}
}

func assertCodexAppServerAlive(t *testing.T, client *codexAppServer, duration time.Duration) {
	t.Helper()
	select {
	case <-client.exited:
		t.Fatal("published Codex child exited when its construction context was canceled")
	case <-time.After(duration):
	}
}

func assertClosed(t *testing.T, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatalf("%s completion is still open", name)
	}
}

func assertAppServerEventsClosed(t *testing.T, events <-chan appServerEvent) {
	t.Helper()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-time.After(time.Second):
			t.Fatal("app-server events did not close")
		}
	}
}

// Item #5 liveness, blocker 25: the Codex read boundary increments the activity
// generation BEFORE the synchronous logf — a blocked log sink cannot stall
// liveness. Holding the global log mutex, ActivitySeq must advance while readLoop
// is still blocked on the log. Moving noteActivity below logf makes this RED.
func TestCodexReadBoundaryStampsActivityBeforeBlockedLog(t *testing.T) {
	logg, err := openAgentLog(Config{DataDir: t.TempDir()}, "agent_codex")
	if err != nil {
		t.Fatalf("open agent log: %v", err)
	}
	c := newCodexAppServer(Config{DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")
	c.log = logg
	agentLogWriteMu.Lock()
	readDone := make(chan struct{})
	go func() {
		c.readLoop(strings.NewReader(`{"telemetry":"unmapped"}` + "\n"))
		close(readDone)
	}()
	deadline := time.After(2 * time.Second)
	for c.ActivitySeq() == 0 {
		select {
		case <-deadline:
			agentLogWriteMu.Unlock()
			t.Fatal("ActivitySeq must advance before the (blocked) log write completes")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-readDone:
		agentLogWriteMu.Unlock()
		t.Fatal("readLoop finished before the log was released — cannot prove increment-before-log ordering")
	default:
	}
	agentLogWriteMu.Unlock()
	<-readDone
}

// The shared read-boundary validator both drivers gate on, pinned directly.
func TestIsValidRuntimeFrame(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`{"a":1}`, true},
		{`{}`, true},
		{`  {"x":true}  `, true},
		{`null`, false},
		{`[1]`, false},
		{`42`, false},
		{`"s"`, false},
		{`{ broken`, false},
		{``, false},
		{`   `, false},
	}
	for _, tc := range cases {
		if got := isValidRuntimeFrame([]byte(tc.line)); got != tc.want {
			t.Fatalf("isValidRuntimeFrame(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// writeFakeCodex writes an executable shell script to a temp path so a test can
// drive the real codex app-server against scripted stdio/stderr behavior.
func writeFakeCodex(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

// The fake process fills the app-server event channel with telemetry, emits a
// real lifecycle notification, and exits. The lifecycle event must survive,
// and process exit must not close the channel until readLoop has delivered it.
func TestCodexAppServerExitDrainsGuaranteedLifecycleNotification(t *testing.T) {
	codexPath := fakeProcessCommand(t, fakeProcessCodexLifecycleFlood)
	client := newCodexAppServer(Config{CodexCommand: codexPath, DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")
	defer client.closeLog()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start app-server: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(client.events) < cap(client.events) {
		if time.Now().After(deadline) {
			t.Fatalf("app-server channel did not fill: len=%d cap=%d", len(client.events), cap(client.events))
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-client.done:
	case <-time.After(3 * time.Second):
		t.Fatal("fake app-server did not exit while lifecycle delivery was blocked")
	}

	telemetry := 0
	foundLifecycle := false
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				if !foundLifecycle {
					t.Fatalf("turn/completed was lost before process-exit channel close; telemetry=%d", telemetry)
				}
				return
			}
			if event.Method == "turn/completed" {
				foundLifecycle = true
			} else if event.Method == "item/agentMessage/delta" {
				telemetry++
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("events did not close after readLoop drained; telemetry=%d lifecycle=%v", telemetry, foundLifecycle)
		}
	}
}

func TestCodexAppServerFullChannelDropsTelemetryWithoutBlockingReadLoop(t *testing.T) {
	client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
	for i := 0; i < cap(client.events); i++ {
		client.events <- appServerEvent{Method: "filler"}
	}

	readDone := make(chan struct{})
	go func() {
		client.readLoop(strings.NewReader(`{"method":"item/agentMessage/delta","params":{"delta":"text"}}` + "\n"))
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("telemetry notification blocked on a full channel")
	}
	if got := len(client.events); got != cap(client.events) {
		t.Fatalf("telemetry should be dropped without changing the full channel: len=%d cap=%d", got, cap(client.events))
	}
}

func TestCodexAppServerFullChannelDropsNonIdleStatusTelemetry(t *testing.T) {
	for _, status := range []string{"working", "future-status"} {
		t.Run(status, func(t *testing.T) {
			client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
			for i := 0; i < cap(client.events); i++ {
				client.events <- appServerEvent{Method: "filler"}
			}

			readDone := make(chan struct{})
			go func() {
				line := `{"method":"thread/status/changed","params":{"status":{"type":"` + status + `"}}}`
				client.readLoop(strings.NewReader(line + "\n"))
				close(readDone)
			}()
			select {
			case <-readDone:
			case <-time.After(100 * time.Millisecond):
				_ = client.Stop()
				<-readDone
				t.Fatalf("non-idle status %q blocked despite being discarded by codexRuntimeEvent", status)
			}
			if got := len(client.events); got != cap(client.events) {
				t.Fatalf("non-idle status should be dropped without changing the full channel: len=%d cap=%d", got, cap(client.events))
			}
		})
	}
}

func TestCodexAppServerGuaranteesEveryMappedLifecycleMethod(t *testing.T) {
	tests := []struct {
		method string
		params string
	}{
		{method: "turn/started", params: `{"turn":{"id":"turn_1"}}`},
		{method: "turn/completed", params: `{}`},
		{method: "turn/failed", params: `{}`},
		{method: "thread/status/changed", params: `{"status":{"type":"idle"}}`},
	}

	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
			for i := 0; i < cap(client.events); i++ {
				client.events <- appServerEvent{Method: "filler"}
			}

			readDone := make(chan struct{})
			go func() {
				line := `{"method":"` + test.method + `","params":` + test.params + `}`
				client.readLoop(strings.NewReader(line + "\n"))
				close(readDone)
			}()
			select {
			case <-readDone:
				t.Fatalf("%s was dropped instead of blocking on the full channel", test.method)
			case <-time.After(100 * time.Millisecond):
			}

			for i := 0; i < cap(client.events); i++ {
				<-client.events
			}
			select {
			case event := <-client.events:
				if event.Method != test.method {
					t.Fatalf("lifecycle method = %q, want %q", event.Method, test.method)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s was not delivered after channel capacity became available", test.method)
			}
			select {
			case <-readDone:
			case <-time.After(2 * time.Second):
				t.Fatalf("readLoop did not return after delivering %s", test.method)
			}
		})
	}
}

func TestCodexAppServerStopWithoutCommandReleasesBlockedLifecycle(t *testing.T) {
	client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
	for i := 0; i < cap(client.events); i++ {
		client.events <- appServerEvent{Method: "filler"}
	}

	readDone := make(chan struct{})
	go func() {
		client.readLoop(strings.NewReader(`{"method":"turn/failed","params":{}}` + "\n"))
		close(readDone)
	}()
	select {
	case <-readDone:
		t.Fatal("lifecycle notification returned instead of blocking on the full live channel")
	case <-time.After(100 * time.Millisecond):
	}

	if err := client.Stop(); err != nil {
		t.Fatalf("stop app-server without command: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not release the blocked lifecycle notification when cmd was nil")
	}
}

// Blocker 6 (Cluster B): recordExitInfo must run only after the stderr reader is
// joined. cmd.Wait closes the pipes on process exit but does not wait for the
// reader goroutines, so a snapshot taken at Wait time can omit the process's
// final diagnostic line. The helper bursts stderr then emits a sentinel as its
// last line and exits; that sentinel must be present in ExitInfo after Events()
// closes.
func TestCodexAppServerExitInfoIncludesFinalStderrLine(t *testing.T) {
	codexPath := writeFakeCodex(t, `#!/bin/sh
while IFS= read -r line; do
	case "$line" in
	*'"method":"initialize"'*)
		printf '%s\n' '{"id":1,"result":{}}'
		;;
	*'"method":"initialized"'*)
		i=0
		while [ "$i" -lt 3000 ]; do
			printf 'stderr noise %s\n' "$i" >&2
			i=$((i + 1))
		done
		printf 'FINAL-STDERR-SENTINEL\n' >&2
		exit 7
		;;
	esac
done
`)
	client := newCodexAppServer(Config{CodexCommand: codexPath, DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")
	defer client.closeLog()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start app-server: %v", err)
	}

	// Drain Events() until it closes; the exit goroutine closes it only AFTER
	// recordExitInfo, so a closed channel means the snapshot is final.
Drain:
	for {
		select {
		case _, ok := <-client.Events():
			if !ok {
				break Drain
			}
		case <-time.After(5 * time.Second):
			t.Fatal("app-server events did not close after process exit")
		}
	}

	info := client.ExitInfo()
	found := false
	for _, line := range info.Stderr {
		if line == "FINAL-STDERR-SENTINEL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("final stderr line missing from ExitInfo after Events() closed: stderr=%#v", info.Stderr)
	}
}

func TestAppServerRPCErrorPreservesCode(t *testing.T) {
	err := &appServerRPCError{Method: "turn/start", Code: -32001, Message: "Server overloaded; retry later."}
	if err.Code != -32001 {
		t.Fatalf("Code = %d, want -32001", err.Code)
	}
	if err.Method != "turn/start" {
		t.Fatalf("Method = %q, want %q", err.Method, "turn/start")
	}
	want := "app-server turn/start failed: Server overloaded; retry later."
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestAppServerRPCErrorClassifiableViaErrorsAs(t *testing.T) {
	var wrapped error = fmt.Errorf("write stdin: %w", &appServerRPCError{
		Method: "turn/start", Code: -32001, Message: "Server overloaded; retry later.",
	})
	var rpcErr *appServerRPCError
	if !errors.As(wrapped, &rpcErr) {
		t.Fatal("errors.As should unwrap appServerRPCError")
	}
	if rpcErr.Code != -32001 {
		t.Fatalf("unwrapped Code = %d, want -32001", rpcErr.Code)
	}
}

func TestAppServerRPCErrorNegativeNeighborCodes(t *testing.T) {
	tests := []struct {
		name string
		code int
	}{
		{"internal_error", -32603},
		{"method_not_found", -32601},
		{"parse_error", -32700},
		{"zero", 0},
		{"positive_429", 429},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &appServerRPCError{Method: "turn/start", Code: tt.code, Message: "some error"}
			if err.Code == -32001 {
				t.Fatalf("code %d should not match -32001", tt.code)
			}
		})
	}
}

func TestRequestReturnsTypedRPCErrorOnJSONRPCError(t *testing.T) {
	client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")

	stdinReader, stdinWriter := io.Pipe()
	client.stdin = stdinWriter

	stdoutReader, stdoutWriter := io.Pipe()
	go client.readLoop(stdoutReader)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := client.request(ctx, "turn/start", map[string]any{})
		errCh <- err
	}()

	buf := make([]byte, 4096)
	n, _ := stdinReader.Read(buf)
	var req struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(buf[:n], &req)

	response := fmt.Sprintf(`{"id":%d,"error":{"code":-32001,"message":"Server overloaded; retry later."}}`, req.ID)
	stdoutWriter.Write([]byte(response + "\n"))

	err := <-errCh
	if err == nil {
		t.Fatal("request should return an error for a JSONRPC error response")
	}
	var rpcErr *appServerRPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error should be appServerRPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32001 {
		t.Fatalf("Code = %d, want -32001", rpcErr.Code)
	}
	if rpcErr.Method != "turn/start" {
		t.Fatalf("Method = %q, want %q", rpcErr.Method, "turn/start")
	}

	stdoutWriter.Close()
	stdinReader.Close()
}
