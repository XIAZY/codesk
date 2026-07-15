package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const claudeTestHandshakeWait = 500 * time.Millisecond

func writeFakeClaude(t *testing.T) string {
	t.Helper()
	return fakeProcessCommand(t, fakeProcessClaude)
}

func newTestClaudeProcess(t *testing.T, claudePath string) *claudeRuntimeProcess {
	t.Helper()
	driver := &claudeDriver{
		cfg:           Config{ClaudeCommand: claudePath, DataDir: t.TempDir()},
		handshakeWait: claudeTestHandshakeWait,
	}
	process, err := driver.Spawn(context.Background(), RuntimeSpawnSpec{
		AgentID:      "agent_claude",
		Workdir:      t.TempDir(),
		ToolToken:    "tool_token",
		Instructions: "be helpful",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	claudeProcess, ok := process.(*claudeRuntimeProcess)
	if !ok {
		t.Fatalf("expected claude runtime process, got %T", process)
	}
	t.Cleanup(func() { _ = claudeProcess.Stop() })
	return claudeProcess
}

func TestClaudeDriverDetect(t *testing.T) {
	driver := newClaudeDriver(Config{ClaudeCommand: writeFakeClaude(t)})

	detection := driver.Detect(context.Background())

	if detection.Kind != RuntimeClaudeCode {
		t.Fatalf("expected claude-code detection kind, got %#v", detection)
	}
	if !detection.Available {
		t.Fatalf("expected fake claude to be available, got %#v", detection)
	}
	if detection.Version != "9.9.9 (Claude Code)" {
		t.Fatalf("expected version from --version probe, got %q", detection.Version)
	}
}

func TestClaudeDriverDetectMissingCommand(t *testing.T) {
	driver := newClaudeDriver(Config{ClaudeCommand: filepath.Join(t.TempDir(), "claude-not-installed")})

	detection := driver.Detect(context.Background())

	if detection.Available {
		t.Fatalf("expected missing claude to be unavailable, got %#v", detection)
	}
	if !strings.Contains(detection.Reason, "not found") {
		t.Fatalf("expected not-found reason, got %#v", detection)
	}
}

func TestClaudeStartSessionSpawnsWithGeneratedSessionID(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_CLAUDE_ARGS_FILE", argsFile)
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	result, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession, CWD: process.workdir})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := uuid.Parse(result.SessionID); err != nil {
		t.Fatalf("expected UUID session id, got %q: %v", result.SessionID, err)
	}

	args := readLines(t, argsFile)
	for _, want := range [][]string{
		{"--session-id", result.SessionID},
		{"--print"},
		{"--input-format", "stream-json"},
		{"--output-format", "stream-json"},
		{"--dangerously-skip-permissions"},
		{"--permission-mode", "bypassPermissions"},
		{"--disallowed-tools", claudeDisallowedTools},
		{"--append-system-prompt", "be helpful"},
	} {
		if !containsSequence(args, want) {
			t.Fatalf("expected spawn args to contain %v, got %v", want, args)
		}
	}
}

func TestClaudeResumeSessionSpawnsWithResume(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_CLAUDE_ARGS_FILE", argsFile)
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	result, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputResumeSession,
		SessionID: " session_existing ",
		CWD:       process.workdir,
	})
	if err != nil {
		t.Fatalf("resume session: %v", err)
	}
	if result.SessionID != "session_existing" {
		t.Fatalf("expected resumed session id, got %#v", result)
	}
	if !containsSequence(readLines(t, argsFile), []string{"--resume", "session_existing"}) {
		t.Fatalf("expected --resume flag in spawn args")
	}
}

func TestClaudeResumeSessionRequiresSessionID(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	_, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputResumeSession})
	if err == nil || !strings.Contains(err.Error(), "session id is required") {
		t.Fatalf("expected missing session id error, got %v", err)
	}
	if pid := process.PID(); pid != 0 {
		t.Fatalf("expected no process spawn without session id, got pid %d", pid)
	}
}

// The supervisor falls back from a failed resume to a fresh startSession on
// the same RuntimeProcess; the driver must report the stale resume
// synchronously and stay usable for the respawn.
func TestClaudeResumeUnknownSessionFailsFastThenStartsFresh(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_FAIL_RESUME", "1")
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	_, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputResumeSession,
		SessionID: "session_deleted",
	})
	if err == nil || !strings.Contains(err.Error(), "No conversation found") {
		t.Fatalf("expected stale resume error with stderr detail, got %v", err)
	}

	started, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession})
	if err != nil {
		t.Fatalf("start session after failed resume: %v", err)
	}
	if started.SessionID == "" || started.SessionID == "session_deleted" {
		t.Fatalf("expected fresh session id, got %#v", started)
	}

	turn, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "hello"})
	if err != nil {
		t.Fatalf("start turn after respawn: %v", err)
	}
	expectRuntimeEvent(t, process.Events(), RuntimeEvent{
		Kind:      RuntimeEventTurnCompleted,
		SessionID: started.SessionID,
		TurnID:    turn.TurnID,
	})
}

func TestClaudeTurnLifecycle(t *testing.T) {
	ioFile := filepath.Join(t.TempDir(), "io")
	t.Setenv("FAKE_CLAUDE_IO_FILE", ioFile)
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	session, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	turn, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "do work"})
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	if turn.TurnID == "" {
		t.Fatal("expected synthesized turn id")
	}
	expectRuntimeEvent(t, process.Events(), RuntimeEvent{
		Kind:      RuntimeEventTurnCompleted,
		SessionID: session.SessionID,
		TurnID:    turn.TurnID,
	})

	failed, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "fail this turn"})
	if err != nil {
		t.Fatalf("start failing turn: %v", err)
	}
	expectRuntimeEvent(t, process.Events(), RuntimeEvent{
		Kind:      RuntimeEventTurnFailed,
		SessionID: session.SessionID,
		TurnID:    failed.TurnID,
		Error:     "boom",
	})

	lines := readLines(t, ioFile)
	if len(lines) != 2 || !strings.Contains(lines[0], "do work") || !strings.Contains(lines[1], "fail this turn") {
		t.Fatalf("expected two user messages on stdin, got %v", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, `"session_id":"`+session.SessionID+`"`) {
			t.Fatalf("expected session id on stdin message, got %s", line)
		}
	}
}

func TestClaudeSteerRequiresActiveTurn(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	_, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputSteerTurn, Text: "nudge"})
	if err == nil || !isNoActiveTurnToSteerError(err) {
		t.Fatalf("expected no-active-turn error recognized by the supervisor, got %v", err)
	}
}

func TestClaudeSteerAndInterruptWriteToStdin(t *testing.T) {
	ioFile := filepath.Join(t.TempDir(), "io")
	t.Setenv("FAKE_CLAUDE_IO_FILE", ioFile)
	// Hold the turn open (the fake emits no result until the interrupt) so the
	// steer and interrupt are provably delivered while the turn is still active,
	// not after it has already completed.
	t.Setenv("FAKE_CLAUDE_HOLD_TURN", "1")
	process := newTestClaudeProcess(t, writeFakeClaude(t))
	session, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	turn, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "long task"})
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	// The turn is held open, so no completion event fires: the turn is genuinely
	// active when we steer (steer requires an active turn).
	expectNoRuntimeEventWithin(t, process.Events(), 150*time.Millisecond)
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:   RuntimeInputSteerTurn,
		TurnID: turn.TurnID,
		Text:   "important follow-up",
	}); err != nil {
		t.Fatalf("steer turn: %v", err)
	}
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputInterruptTurn, TurnID: turn.TurnID}); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	// The interrupt ends the held turn — its completion event confirms the steer
	// and interrupt were processed mid-turn, not against a completed one.
	expectRuntimeEvent(t, process.Events(), RuntimeEvent{
		Kind:      RuntimeEventTurnCompleted,
		SessionID: session.SessionID,
		TurnID:    turn.TurnID,
	})

	lines := readLines(t, ioFile)
	if len(lines) < 3 {
		t.Fatalf("expected user + steer + interrupt on stdin, got %v", lines)
	}
	if !strings.Contains(lines[0], "long task") {
		t.Fatalf("expected the turn message first on stdin, got %v", lines)
	}
	if !strings.Contains(lines[1], "important follow-up") {
		t.Fatalf("expected steer message on stdin, got %v", lines)
	}
	if !strings.Contains(lines[2], `"type":"control_request"`) || !strings.Contains(lines[2], `"subtype":"interrupt"`) {
		t.Fatalf("expected interrupt control request on stdin, got %v", lines)
	}
}

func TestClaudeStartTurnWithoutSpawnFails(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	_, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not-running error, got %v", err)
	}
}

// Item #5 liveness, row 4: a syntactically valid frame whose type the parser does
// NOT map still proves the runtime is alive, so it must advance the activity
// generation at the read boundary. Liveness is syntactic, not semantic — it must
// not depend on which frame types the parser currently recognizes.
func TestClaudeReadBoundaryUnknownValidFrameRefreshesLiveness(t *testing.T) {
	process := &claudeRuntimeProcess{events: make(chan RuntimeEvent, 4)}
	if process.ActivitySeq() != 0 {
		t.Fatalf("precondition: expected zero activity generation, got %d", process.ActivitySeq())
	}
	// Well-formed JSON object, but "type" is one the parser has no case for.
	process.readLoop(strings.NewReader(`{"type":"telemetry_unmapped","progress":0.5}` + "\n"))
	if process.ActivitySeq() == 0 {
		t.Fatal("an unknown-but-valid frame must advance the activity generation at the read boundary")
	}
}

// Item #5 liveness, row 5: malformed output must NOT advance the activity
// generation — a process spewing junk or partial bytes cannot manufacture a
// heartbeat and mask a wedge.
func TestClaudeReadBoundaryMalformedFrameDoesNotRefreshLiveness(t *testing.T) {
	process := &claudeRuntimeProcess{events: make(chan RuntimeEvent, 4)}
	process.readLoop(strings.NewReader("{ this is not valid json\n"))
	if process.ActivitySeq() != 0 {
		t.Fatalf("a malformed frame must not advance the activity generation, got %d", process.ActivitySeq())
	}
}

// A full events channel must never lose a lifecycle event (task #12). The old
// emit dropped on full, so a dropped turn-end wedged the session as "working"
// forever with no external cause. This pins the guarantee at the readLoop
// seam: fill the channel to capacity, feed readLoop a real result line, and
// the turn-end must still come out the other end once the consumer drains.
func TestClaudeFullChannelCannotLoseTurnEnd(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))
	process.mu.Lock()
	process.sessionID = "sess_full"
	process.activeTurn = "turn_full"
	process.mu.Unlock()
	for i := 0; i < cap(process.events); i++ {
		process.events <- RuntimeEvent{Kind: RuntimeEventIdle}
	}

	readDone := make(chan struct{})
	go func() {
		process.readLoop(strings.NewReader(`{"type":"result","subtype":"success","session_id":"sess_full"}` + "\n"))
		close(readDone)
	}()

	// Do not drain until readLoop has made its emit attempt against the full
	// channel — draining first frees a slot and the drop-on-full bug this test
	// pins would never trigger. A lossy readLoop returns immediately after
	// dropping (readDone closes); the fixed one blocks in the emit, so the
	// timeout arm is how the green path proceeds.
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
	}

	deadline := time.After(5 * time.Second)
	drained := 0
	for {
		select {
		case event := <-process.events:
			if event.Kind == RuntimeEventIdle {
				drained++
				continue
			}
			if event.Kind != RuntimeEventTurnCompleted || event.TurnID != "turn_full" || event.SessionID != "sess_full" {
				t.Fatalf("expected the blocked turn-end, got %#v", event)
			}
		case <-deadline:
			t.Fatalf("turn-end was lost: drained %d filler events and it never arrived", drained)
		}
		break
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not return after its turn-end was delivered")
	}
}

// The blocking guarantee needs a release valve: the supervisor's failure paths
// can Stop a process before consumeEvents ever attaches, and a lifecycle emit
// blocked on a full channel must not pin readLoop (and the exit goroutine
// behind it) forever. The stop signal releases the emitter; the event is
// discarded because a stopped process is terminal. This drives signalStop
// directly rather than Stop() so the events channel stays open — closing it
// under a blocked emitter is exactly the race prod ordering forbids.
func TestClaudeStopSignalReleasesBlockedLifecycleEmit(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))
	for i := 0; i < cap(process.events); i++ {
		process.events <- RuntimeEvent{Kind: RuntimeEventIdle}
	}

	emitted := make(chan struct{})
	go func() {
		process.emitLifecycle(RuntimeEvent{Kind: RuntimeEventTurnCompleted, TurnID: "turn_blocked"})
		close(emitted)
	}()
	select {
	case <-emitted:
		t.Fatal("emit must block while the channel is full and the process is live")
	case <-time.After(100 * time.Millisecond):
	}

	process.signalStop()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("stop signal did not release the blocked lifecycle emit")
	}
}

func TestClaudeStopWithoutSpawnClosesEvents(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	if err := process.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case _, ok := <-process.Events():
		if ok {
			t.Fatal("expected closed event channel without events")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected event channel to close on stop")
	}
}

func TestClaudeStopKillsProcessAndClosesEvents(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if process.PID() == 0 {
		t.Fatal("expected running claude process")
	}

	if err := process.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if events := collectRuntimeEvents(t, process.Events()); len(events) != 0 {
		t.Fatalf("expected no runtime events, got %#v", events)
	}
}

// A Stop that lands inside the handshake window (process spawned but not yet
// established) must still close the events channel — otherwise the supervisor's
// event consumer blocks forever.
func TestClaudeStopDuringHandshakeClosesEvents(t *testing.T) {
	driver := &claudeDriver{
		cfg: Config{ClaudeCommand: writeFakeClaude(t), DataDir: t.TempDir()},
		// Generous window so Stop lands while spawn is still waiting on startup.
		handshakeWait: 2 * time.Second,
	}
	spawned, err := driver.Spawn(context.Background(), RuntimeSpawnSpec{AgentID: "agent_claude", Workdir: t.TempDir(), ToolToken: "tool_token"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := spawned.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	process := spawned.(*claudeRuntimeProcess)

	// spawn() blocks for the handshake window, so start the session from a
	// goroutine and stop the process while it is still mid-handshake.
	go func() { _, _ = process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession}) }()
	deadline := time.Now().Add(time.Second)
	for process.PID() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("claude process did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := process.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case _, ok := <-process.Events():
		if ok {
			t.Fatal("expected the events channel to close, got an event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("events channel did not close after Stop during the handshake window (leak)")
	}
}

// End-to-end through the supervisor: a claude-code agent with a stale
// session id must recover into a fresh session and complete a turn.
func TestAgentSessionSupervisorRunsClaudeRuntime(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_FAIL_RESUME", "1")
	// Hold the turn open until an explicit interrupt (task #13). The status
	// syncer coalesces to the LATEST state per agent — intermediate states are
	// droppable by design — so a fake claude that finishes its turn instantly
	// races "idle" over "working" and a loaded runner loses the "working"
	// update the old test asserted on. Holding the turn makes "working" the
	// stable latest state until the test has observed it and interrupts.
	t.Setenv("FAKE_CLAUDE_HOLD_TURN", "1")
	cfg := Config{
		ClaudeCommand:      writeFakeClaude(t),
		DataDir:            t.TempDir(),
		AgentWorkspaceRoot: t.TempDir(),
	}
	updates := make(chan updateAgentSessionRequest, 16)
	updater := func(ctx context.Context, agentID string, payload updateAgentSessionRequest) error {
		updates <- payload
		return nil
	}
	registry := newRuntimeRegistry(&claudeDriver{cfg: cfg, handshakeWait: claudeTestHandshakeWait})
	supervisor := newAgentSessionSupervisor(cfg, updater, registry)
	defer supervisor.Shutdown()

	current := &agent{
		ID:           "agent_claude",
		Handle:       "claude",
		Kind:         "claude-code",
		SessionID:    "session_stale",
		SystemPrompt: "be helpful",
	}
	if err := supervisor.Reconcile(context.Background(), []*agent{current}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	idle := waitForSessionStatus(t, updates, "idle")
	if idle.SessionID == "" || idle.SessionID == "session_stale" {
		t.Fatalf("expected fresh session id after stale resume, got %#v", idle)
	}

	if err := supervisor.ScheduleNotificationTurn(context.Background(), current, "check your inbox", "sig_1", ""); err != nil {
		t.Fatalf("schedule turn: %v", err)
	}
	working := waitForSessionStatus(t, updates, "working")
	if working.CurrentTurnID == "" {
		t.Fatalf("expected turn id while working, got %#v", working)
	}

	// End the held turn only now that "working" has been observed; the result
	// line the interrupt triggers drives turnCompleted -> idle.
	supervisor.mu.Lock()
	process := supervisor.sessions[current.ID].process
	supervisor.mu.Unlock()
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputInterruptTurn,
		SessionID: working.SessionID,
		TurnID:    working.CurrentTurnID,
	}); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	finished := waitForSessionStatus(t, updates, "idle")
	if finished.SessionID != idle.SessionID {
		t.Fatalf("expected stable session id across the turn, got %#v", finished)
	}
}

func waitForSessionStatus(t *testing.T, updates <-chan updateAgentSessionRequest, status string) updateAgentSessionRequest {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case payload := <-updates:
			if payload.Status == status {
				return payload
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q session status", status)
		}
	}
}

func TestParseClaudeStreamLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want *claudeStreamEvent
	}{
		{
			name: "init",
			line: `{"type":"system","subtype":"init","session_id":"sess_1"}`,
			want: &claudeStreamEvent{kind: claudeStreamInit, sessionID: "sess_1"},
		},
		{
			name: "success result",
			line: `{"type":"result","subtype":"success","is_error":false,"session_id":"sess_1"}`,
			want: &claudeStreamEvent{kind: claudeStreamTurnEnd, sessionID: "sess_1"},
		},
		{
			name: "error result with errors list",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"," extra "],"session_id":"sess_1"}`,
			want: &claudeStreamEvent{kind: claudeStreamTurnEnd, sessionID: "sess_1", failed: true, errText: "boom | extra"},
		},
		{
			name: "error result falls back to result text",
			line: `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"Max turns exceeded"}`,
			want: &claudeStreamEvent{kind: claudeStreamTurnEnd, failed: true, errText: "Max turns exceeded"},
		},
		{
			name: "error result without detail",
			line: `{"type":"result","is_error":true}`,
			want: &claudeStreamEvent{kind: claudeStreamTurnEnd, failed: true, errText: "claude turn failed"},
		},
		{name: "assistant ignored", line: `{"type":"assistant","message":{"content":[]}}`, want: nil},
		{name: "other system subtype ignored", line: `{"type":"system","subtype":"status"}`, want: nil},
		{name: "malformed ignored", line: `{not json`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClaudeStreamLine([]byte(tc.line))
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil event, got %#v", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("unexpected event:\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

// TestClaudeLiveSmoke drives the installed claude CLI end to end: fresh
// session, one turn, then resume in a new process and a second turn. It
// spends a few model tokens, so it is opt-in:
//
//	NOTTY_CLAUDE_LIVE_TEST=1 go test ./daemon/internal/syncer -run TestClaudeLiveSmoke -v
func TestClaudeLiveSmoke(t *testing.T) {
	if os.Getenv("NOTTY_CLAUDE_LIVE_TEST") != "1" {
		t.Skip("set NOTTY_CLAUDE_LIVE_TEST=1 to run against the real claude CLI")
	}
	cfg := Config{DataDir: t.TempDir()}
	driver := newClaudeDriver(cfg)
	if detection := driver.Detect(context.Background()); !detection.Available {
		t.Skipf("claude CLI unavailable: %s", detection.Reason)
	}
	workdir := t.TempDir()
	spec := RuntimeSpawnSpec{
		AgentID:      "agent_live",
		Workdir:      workdir,
		Instructions: "You are a smoke-test agent. Reply with the single word ok and do nothing else.",
	}

	process, err := driver.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = process.Stop() }()
	session, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession, CWD: workdir})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	turn, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "Reply with exactly: ok"})
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	expectRuntimeEventWithin(t, process.Events(), RuntimeEvent{
		Kind:      RuntimeEventTurnCompleted,
		SessionID: session.SessionID,
		TurnID:    turn.TurnID,
	}, 3*time.Minute)
	if err := process.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	resumed, err := driver.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("spawn for resume: %v", err)
	}
	if err := resumed.Start(context.Background()); err != nil {
		t.Fatalf("start for resume: %v", err)
	}
	defer func() { _ = resumed.Stop() }()
	if _, err := resumed.WriteStdin(context.Background(), RuntimeInput{
		Kind:      RuntimeInputResumeSession,
		SessionID: session.SessionID,
		CWD:       workdir,
	}); err != nil {
		t.Fatalf("resume session: %v", err)
	}
	turn2, err := resumed.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "Reply with exactly: ok again"})
	if err != nil {
		t.Fatalf("start resumed turn: %v", err)
	}
	expectRuntimeEventWithin(t, resumed.Events(), RuntimeEvent{
		Kind:      RuntimeEventTurnCompleted,
		SessionID: session.SessionID,
		TurnID:    turn2.TurnID,
	}, 3*time.Minute)
}

func expectRuntimeEvent(t *testing.T, events <-chan RuntimeEvent, want RuntimeEvent) {
	t.Helper()
	expectRuntimeEventWithin(t, events, want, 5*time.Second)
}

func expectRuntimeEventWithin(t *testing.T, events <-chan RuntimeEvent, want RuntimeEvent, timeout time.Duration) {
	t.Helper()
	select {
	case got, ok := <-events:
		if !ok {
			t.Fatalf("event channel closed while waiting for %#v", want)
		}
		if got != want {
			t.Fatalf("unexpected runtime event:\n got: %#v\nwant: %#v", got, want)
		}
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for runtime event %#v", want)
	}
}

func expectNoRuntimeEventWithin(t *testing.T, events <-chan RuntimeEvent, timeout time.Duration) {
	t.Helper()
	select {
	case got, ok := <-events:
		if !ok {
			t.Fatal("event channel closed while asserting no event")
		}
		t.Fatalf("expected no runtime event within %s, got %#v", timeout, got)
	case <-time.After(timeout):
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func containsSequence(lines []string, sequence []string) bool {
	if len(sequence) == 0 {
		return true
	}
	for i := 0; i+len(sequence) <= len(lines); i++ {
		match := true
		for j, want := range sequence {
			if lines[i+j] != want {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestClaudeSpawnEnvContract pins the child process env the driver is
// responsible for: the CLAUDECODE marker is stripped, the agent-tool values are
// passed through, and IS_SANDBOX is not injected (the Dockerfile declares it).
func TestClaudeSpawnEnvContract(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("FAKE_CLAUDE_ENV_FILE", envFile)
	// A CLAUDECODE marker in the parent env must not reach the child, since a
	// nested marker changes claude CLI behavior.
	t.Setenv("CLAUDECODE", "1")
	// The driver must not inject IS_SANDBOX; make sure the parent env can't
	// smuggle it into the assertion.
	if orig, had := os.LookupEnv("IS_SANDBOX"); had {
		os.Unsetenv("IS_SANDBOX")
		t.Cleanup(func() { os.Setenv("IS_SANDBOX", orig) })
	}

	driver := &claudeDriver{
		cfg: Config{
			ClaudeCommand:    writeFakeClaude(t),
			DataDir:          t.TempDir(),
			AgentToolBaseURL: "http://tool.example",
		},
		handshakeWait: claudeTestHandshakeWait,
	}
	process, err := driver.Spawn(context.Background(), RuntimeSpawnSpec{
		AgentID:   "agent_claude",
		Workdir:   t.TempDir(),
		ToolToken: "tool_token",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	claudeProcess := process.(*claudeRuntimeProcess)
	t.Cleanup(func() { _ = claudeProcess.Stop() })
	if _, err := claudeProcess.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartSession}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	env := waitForEnvFile(t, envFile)
	if _, ok := envLookup(env, "CLAUDECODE"); ok {
		t.Fatal("expected CLAUDECODE stripped from the child env")
	}
	if got, _ := envLookup(env, "NOTTY_AGENT_TOOL_TOKEN"); got != "tool_token" {
		t.Fatalf("expected NOTTY_AGENT_TOOL_TOKEN=tool_token in child env, got %q", got)
	}
	if got, _ := envLookup(env, "NOTTY_AGENT_TOOL_BASE_URL"); got != "http://tool.example" {
		t.Fatalf("expected NOTTY_AGENT_TOOL_BASE_URL passed through, got %q", got)
	}
	if _, ok := envLookup(env, "IS_SANDBOX"); ok {
		t.Fatal("expected the driver not to inject IS_SANDBOX (the Dockerfile declares it)")
	}
}

func waitForEnvFile(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for env capture file %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, line := range env {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix), true
		}
	}
	return "", false
}

// TestClaudeStartTurnWriteErrorLeavesNoStaleActiveTurn pins the cleanup on the StartTurn write-error
// path: StartTurn optimistically arms the turn before writing (so a fast result can't observe an empty
// active turn), so if the write fails it must clear that turn — otherwise a later steer targets a
// phantom turn that will never complete.
func TestClaudeStartTurnWriteErrorLeavesNoStaleActiveTurn(t *testing.T) {
	process := newTestClaudeProcess(t, writeFakeClaude(t))

	// No session spawned yet, so the StartTurn write fails after arming the turn.
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputStartTurn, Text: "hi"}); err == nil {
		t.Fatal("expected StartTurn without a spawned session to fail on write")
	}
	// If the failed start left the turn armed, this steer would be accepted against a phantom turn.
	if _, err := process.WriteStdin(context.Background(), RuntimeInput{Kind: RuntimeInputSteerTurn, Text: "steer"}); err == nil || !strings.Contains(err.Error(), "no active turn") {
		t.Fatalf("a failed StartTurn must leave no active turn, got err=%v", err)
	}
}

// Blocker 7 (Cluster B): the bounded stderr snapshot used for exit forensics
// must include a final diagnostic that arrives without a trailing newline. On
// the pre-fix driver linesCopy() returned only completed lines and dropped the
// unterminated tail (still sitting in `partial`), losing the exact evidence
// death classification depends on.
func TestClaudeStderrTailSnapshotIncludesUnterminatedFinalLine(t *testing.T) {
	// A non-nil process with a nil log keeps Write's per-line logf a no-op.
	tail := &claudeStderrTail{process: &claudeRuntimeProcess{}}
	if _, err := tail.Write([]byte("first line\n")); err != nil {
		t.Fatalf("write first line: %v", err)
	}
	if _, err := tail.Write([]byte("fatal: provider quota exhausted")); err != nil {
		t.Fatalf("write unterminated tail: %v", err)
	}
	got := tail.linesCopy()
	if len(got) != 2 || got[0] != "first line" || got[1] != "fatal: provider quota exhausted" {
		t.Fatalf("unterminated final stderr line dropped from bounded snapshot: got %#v", got)
	}
	// String() must stay consistent with the snapshot it summarizes.
	if want := "first line | fatal: provider quota exhausted"; tail.String() != want {
		t.Fatalf("String()=%q, want %q", tail.String(), want)
	}
}
