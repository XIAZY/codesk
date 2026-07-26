package syncer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const RuntimeClaudeCode RuntimeKind = "claude-code"

// Claude Code has no app-server equivalent: one process hosts exactly one
// session, and session identity is fixed by CLI flags at launch. The daemon
// talks to it over the CLI's stream-json stdin/stdout protocol.
//
// Interactive-only tools would wedge a headless turn, and scheduling tools
// would spawn background work outside the supervisor's control.
const claudeDisallowedTools = "EnterPlanMode,ExitPlanMode,ScheduleWakeup,CronCreate,CronList,CronDelete"

// A claude process emits nothing at startup; its only synchronous failure
// signal is a fast exit (e.g. `--resume` with an unknown session ID). Spawn
// watches the process for this long before declaring the session usable, so
// a stale resume falls back to a fresh session instead of crash-looping
// through the supervisor's restart path.
const claudeSpawnHandshakeWait = 2 * time.Second

// claudeStdoutDrainWait bounds how long a startup-exit will wait for readLoop to
// finish draining stdout before snapshotting the (best-effort) diagnostic tail.
// It exists only so a readLoop parked in emitLifecycle on a full events channel
// cannot make provider diagnostics gate Start() teardown; the normal drain of a
// dead process's buffered stdout completes far inside it.
const claudeStdoutDrainWait = 250 * time.Millisecond

type claudeDriver struct {
	cfg           Config
	handshakeWait time.Duration
}

func newClaudeDriver(cfg Config) RuntimeDriver {
	return &claudeDriver{cfg: cfg, handshakeWait: claudeSpawnHandshakeWait}
}

func (d *claudeDriver) Kind() RuntimeKind {
	return RuntimeClaudeCode
}

func (d *claudeDriver) command() string {
	return firstNonEmptyText(strings.TrimSpace(d.cfg.ClaudeCommand), "claude")
}

func (d *claudeDriver) Detect(ctx context.Context) RuntimeDetection {
	path, err := exec.LookPath(d.command())
	if err != nil {
		return RuntimeDetection{Kind: RuntimeClaudeCode, Available: false, Reason: "claude command not found"}
	}
	detectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := managedBackgroundCommandContext(detectCtx, path, "--version").Output()
	if err != nil {
		return RuntimeDetection{Kind: RuntimeClaudeCode, Available: false, Path: path, Reason: "claude --version failed"}
	}
	return RuntimeDetection{Kind: RuntimeClaudeCode, Available: true, Version: strings.TrimSpace(string(output)), Path: path}
}

func (d *claudeDriver) Spawn(ctx context.Context, spec RuntimeSpawnSpec) (RuntimeProcess, error) {
	return &claudeRuntimeProcess{
		cfg:           d.cfg,
		command:       d.command(),
		agentID:       spec.AgentID,
		workdir:       spec.Workdir,
		toolToken:     spec.ToolToken,
		instructions:  spec.Instructions,
		handshakeWait: d.handshakeWait,
		events:        make(chan RuntimeEvent, 128),
		eventsDone:    make(chan struct{}),
		stopping:      make(chan struct{}),
	}, nil
}

type claudeRuntimeProcess struct {
	cfg           Config
	command       string
	agentID       string
	workdir       string
	toolToken     string
	instructions  string
	handshakeWait time.Duration

	events     chan RuntimeEvent
	eventsDone chan struct{}
	closeOnce  sync.Once
	// stopping is closed by Stop (once, ever) and releases a lifecycle emit
	// blocked on a full events channel; see emitLifecycle. It spans the whole
	// process lifetime, like events — a failed spawn attempt that the
	// supervisor retries on this same process must not close it.
	stopping chan struct{}
	stopOnce sync.Once

	// lastActivity is the count of syntactically valid stream frames the read loop
	// has decoded, incremented atomically (one Add per valid frame, no lock) so the
	// supervisor heartbeat can poll the activity generation without contending with
	// WriteStdin/lifecycle handling. 0 means no valid frame decoded yet.
	lastActivity atomic.Int64

	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	sessionID  string
	activeTurn string
	stopped    bool
	exitInfo   RuntimeExitInfo
	log        *agentLog
}

func (p *claudeRuntimeProcess) Start(ctx context.Context) error {
	if p == nil {
		return errors.New("claude runtime process is nil")
	}
	// The claude CLI takes session identity (--session-id / --resume) as
	// launch flags, so the OS process is spawned lazily by the first
	// startSession/resumeSession input rather than here.
	if agentLog, err := openAgentLog(p.cfg, p.agentID); err == nil {
		p.mu.Lock()
		p.log = agentLog
		p.mu.Unlock()
		p.logf("claude runtime prepared command=%s agent=%s workdir=%s", p.command, p.agentID, p.workdir)
	} else {
		log.Printf("agent log open failed agent=%s data_dir=%s err=%v", p.agentID, p.cfg.DataDir, err)
	}
	return nil
}

func (p *claudeRuntimeProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.stopped = true
	cmd := p.cmd
	stdin := p.stdin
	p.mu.Unlock()
	// Release a lifecycle emit blocked on a full events channel before killing
	// the process: readLoop must be able to reach stdout EOF and return, or the
	// exit goroutine (which waits on readLoop) would never close the channel.
	p.signalStop()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		p.logf("stopping claude process pid=%d", cmd.Process.Pid)
		err := cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			// Closing stdin above can trigger a clean exit before the kill lands;
			// an already-finished process is a successful stop, not an error.
			err = nil
		}
		// The exit goroutine closes the events channel once cmd.Wait returns and
		// readLoop drains (which is why it, not Stop, owns the close). Wait for that
		// join so RuntimeProcess.Stop does not return while the child or event stream
		// is still live.
		p.waitEventsClosed()
		p.closeLog()
		return err
	}
	// Never spawned: no exit goroutine exists, so close the event stream here.
	p.closeEvents()
	p.waitEventsClosed()
	p.closeLog()
	return nil
}

func (p *claudeRuntimeProcess) Events() <-chan RuntimeEvent {
	if p == nil {
		return nil
	}
	return p.events
}

func (p *claudeRuntimeProcess) PID() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *claudeRuntimeProcess) WriteStdin(ctx context.Context, input RuntimeInput) (RuntimeWriteResult, error) {
	if p == nil {
		return RuntimeWriteResult{}, errors.New("claude runtime process is not started")
	}
	switch input.Kind {
	case RuntimeInputResumeSession:
		sessionID := strings.TrimSpace(input.SessionID)
		if sessionID == "" {
			return RuntimeWriteResult{}, errors.New("session id is required to resume claude session")
		}
		if err := p.spawn(ctx, sessionID, true); err != nil {
			return RuntimeWriteResult{}, err
		}
		return RuntimeWriteResult{SessionID: sessionID}, nil
	case RuntimeInputStartSession:
		sessionID := uuid.NewString()
		if err := p.spawn(ctx, sessionID, false); err != nil {
			return RuntimeWriteResult{}, err
		}
		return RuntimeWriteResult{SessionID: sessionID}, nil
	case RuntimeInputStartTurn:
		// Registered before the write so a fast result event can never
		// observe an empty active turn.
		turnID := "turn_" + uuid.NewString()
		p.mu.Lock()
		p.activeTurn = turnID
		p.mu.Unlock()
		if err := p.writeUserMessage(input.Text); err != nil {
			p.mu.Lock()
			if p.activeTurn == turnID {
				p.activeTurn = ""
			}
			p.mu.Unlock()
			return RuntimeWriteResult{}, err
		}
		return RuntimeWriteResult{TurnID: turnID}, nil
	case RuntimeInputSteerTurn:
		p.mu.Lock()
		active := p.activeTurn
		p.mu.Unlock()
		if active == "" {
			return RuntimeWriteResult{}, errors.New("no active turn to steer")
		}
		// Claude Code queues mid-turn user messages and folds them into the
		// running turn at the next safe stream boundary.
		return RuntimeWriteResult{}, p.writeUserMessage(input.Text)
	case RuntimeInputInterruptTurn:
		return RuntimeWriteResult{}, p.writeControlRequest("interrupt")
	default:
		return RuntimeWriteResult{}, errors.New("unsupported runtime input kind " + string(input.Kind))
	}
}

func (p *claudeRuntimeProcess) buildArgs(sessionID string, resume bool) []string {
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--allow-dangerously-skip-permissions",
		"--dangerously-skip-permissions",
		"--permission-mode", "bypassPermissions",
		"--disallowed-tools", claudeDisallowedTools,
	}
	if resume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if strings.TrimSpace(p.instructions) != "" {
		args = append(args, "--append-system-prompt", p.instructions)
	}
	return args
}

func buildClaudeEnv(cfg Config, toolToken string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		// A nested CLAUDECODE marker (daemon itself launched from Claude
		// Code) changes CLI behavior; agents must not inherit it.
		if strings.HasPrefix(value, "CLAUDECODE=") {
			continue
		}
		env = append(env, value)
	}
	env = append(env, buildAgentToolEnv(cfg, toolToken)...)
	// IS_SANDBOX (which lets claude accept --dangerously-skip-permissions as
	// root) is declared by the daemon container's Dockerfile, the only supported
	// root context. Inferring it from euid==0 would also mark a misconfigured
	// bare-metal root daemon as a sandbox — a false claim; the environment
	// declares what it is instead.
	return env
}

// spawn launches the claude CLI for one session attempt. The supervisor
// retries a failed resume as a fresh startSession on the same RuntimeProcess,
// so a failed attempt resets state for respawn and must NOT close the events
// channel; only an established process closes it on exit (which is how the
// supervisor learns the session died).
func (p *claudeRuntimeProcess) spawn(ctx context.Context, sessionID string, resume bool) error {
	p.mu.Lock()
	if p.cmd != nil {
		p.mu.Unlock()
		return errors.New("claude process is already running")
	}
	p.mu.Unlock()

	args := p.buildArgs(sessionID, resume)
	// Intentionally not exec.CommandContext: the spawn ctx is a
	// per-reconcile request context, while the process must outlive it.
	cmd := managedBackgroundCommand(p.command, args...)
	cmd.Dir = p.workdir
	cmd.Env = buildClaudeEnv(p.cfg, p.toolToken)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Stderr goes to an in-memory sink instead of a pipe: exec then finishes
	// copying it before Wait returns, so a fast startup failure cannot race
	// its own diagnostic output.
	stderrTail := &claudeStderrTail{process: p}
	cmd.Stderr = stderrTail
	if err := cmd.Start(); err != nil {
		p.logf("claude start failed err=%v", err)
		return err
	}
	p.mu.Lock()
	p.cmd = cmd
	p.stdin = stdin
	p.sessionID = sessionID
	p.mu.Unlock()
	p.logf("claude started pid=%d resume=%t session=%s", cmd.Process.Pid, resume, sessionID)

	stdoutTail := &claudeStdoutTail{}
	readDone := make(chan struct{})
	exited := make(chan error, 1)
	established := make(chan bool, 1)
	go func() {
		p.readLoop(stdout, stdoutTail)
		close(readDone)
	}()
	go func() {
		err := cmd.Wait()
		p.logf("claude exited err=%v", err)
		p.recordExitInfo(cmd, err, stderrTail)
		exited <- err
		<-readDone
		// Closing the events channel is how the supervisor learns the process is
		// done, so the exit goroutine owns it (after readLoop drains, so no
		// send-on-closed race). It closes on exit by default; the one exception
		// is a failed spawn attempt the supervisor retries on this same
		// RuntimeProcess (stale-resume -> fresh-session), which reuses the
		// channel. A Stop() is always terminal, even inside the handshake window.
		if <-established || p.wasStopped() {
			p.closeEvents()
		}
	}()

	// claude prints nothing until the first user message, so the only
	// startup acknowledgment is surviving the handshake window. A fast exit
	// (unknown --resume session, bad flags, refused permissions mode) is
	// reported synchronously so the supervisor can fall back or surface it.
	wait := p.handshakeWait
	if wait <= 0 {
		wait = claudeSpawnHandshakeWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-exited:
		established <- false
		p.resetSpawn(stdin)
		// A fatal startup reason can land on stdout with empty stderr (bad flags,
		// refused permissions), and readLoop is the sole stdout reader. Prefer a
		// complete tail by waiting for readDone (closed when readLoop returns on
		// stdout EOF), but NEVER let it gate teardown: if readLoop is parked in
		// emitLifecycle on a full events channel it won't close readDone, so bound
		// the wait. record/String are mutex-guarded, so a partial snapshot here is
		// race-free — the wait only buys the final line.
		select {
		case <-readDone:
		case <-ctx.Done():
		case <-time.After(claudeStdoutDrainWait):
		}
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		return fmt.Errorf("claude exited during startup: %s",
			firstNonEmptyText(stderrTail.String(), stdoutTail.String(), errText, "no output"))
	case <-timer.C:
		established <- true
		return nil
	case <-ctx.Done():
		established <- false
		_ = cmd.Process.Kill()
		p.resetSpawn(stdin)
		return ctx.Err()
	}
}

func (p *claudeRuntimeProcess) resetSpawn(stdin io.Closer) {
	_ = stdin.Close()
	p.mu.Lock()
	p.cmd = nil
	p.stdin = nil
	p.mu.Unlock()
}

func (p *claudeRuntimeProcess) writeUserMessage(text string) error {
	p.mu.Lock()
	stdin := p.stdin
	sessionID := p.sessionID
	p.mu.Unlock()
	if stdin == nil {
		return errors.New("claude process is not running")
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	return p.writeLine(stdin, payload)
}

func (p *claudeRuntimeProcess) writeControlRequest(subtype string) error {
	p.mu.Lock()
	stdin := p.stdin
	p.mu.Unlock()
	if stdin == nil {
		return errors.New("claude process is not running")
	}
	return p.writeLine(stdin, map[string]any{
		"type":       "control_request",
		"request_id": "req_" + uuid.NewString(),
		"request":    map[string]string{"subtype": subtype},
	})
}

func (p *claudeRuntimeProcess) writeLine(stdin io.Writer, payload map[string]any) error {
	line, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Serialize the pipe write under p.mu so concurrent startTurn/steer/interrupt
	// writes cannot interleave their JSON+newline bytes into corrupt input (the
	// codex driver serializes writes the same way). logf reacquires p.mu, so it
	// stays outside this critical section.
	p.mu.Lock()
	_, err = stdin.Write(append(line, '\n'))
	p.mu.Unlock()
	if err != nil {
		return err
	}
	p.logf("stream send %s", truncateForLog(string(line)))
	return nil
}

func (p *claudeRuntimeProcess) readLoop(stdout io.Reader, stdoutTail *claudeStdoutTail) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Liveness at the LITERAL read boundary: increment the activity generation the
		// instant a syntactically valid frame is decoded — BEFORE the synchronous
		// logf (which takes the log/global-write mutexes and does file I/O) and before
		// any semantic mapping. Otherwise a contended/blocked log sink could stall the
		// generation and let the supervisor declare a demonstrably-live runtime stalled
		// (blocker 17). Malformed output must NOT count — gate on JSON-object validity.
		if isValidRuntimeFrame(line) {
			p.noteActivity()
		}
		p.logf("stream recv %s", truncateForLog(string(line)))
		// Retain every stdout line (frame or not, mapped or not) in the bounded tail
		// so a fatal startup diagnostic printed here survives — lines that map to no
		// event otherwise vanish at the continue below. Kept after the logf/liveness
		// above deliberately: that ordering must not move (blocker-17).
		stdoutTail.record(string(line))
		event := parseClaudeStreamLine(line)
		if event == nil {
			continue
		}
		switch event.kind {
		case claudeStreamInit:
			p.mu.Lock()
			if event.sessionID != "" {
				p.sessionID = event.sessionID
			}
			p.mu.Unlock()
		case claudeStreamTurnEnd:
			p.mu.Lock()
			turnID := p.activeTurn
			sessionID := p.sessionID
			p.activeTurn = ""
			p.mu.Unlock()
			kind := RuntimeEventTurnCompleted
			if event.failed {
				kind = RuntimeEventTurnFailed
			}
			p.emitLifecycle(RuntimeEvent{Kind: kind, SessionID: sessionID, TurnID: turnID, Error: event.errText})
		}
	}
	if err := scanner.Err(); err != nil {
		p.logf("stream stdout scan error err=%v", err)
	}
}

// emitLifecycle delivers a lifecycle event (turn end today; anything driving
// the supervisor's session state machine) with guaranteed delivery: it blocks
// until the consumer takes the event or the process is stopped. The previous
// emit dropped on a full channel, and a dropped turn-end wedged the session as
// "working" forever — silence caused by our own code, which no external-runtime
// defense can attribute correctly. Blocking is safe here: the supervisor's
// consumeEvents goroutine drains the channel for the process's entire life
// (it exits only on channel close, and the channel closes only after readLoop
// returns — or in Stop's never-spawned branch, where no emitter exists — so a
// blocked send can never race the close). The stop case is the release valve
// for processes stopped before a consumer ever attached (the supervisor's
// pre-registration failure paths); a stopped process is terminal, so its
// events are moot. High-volume notification-class events must NOT use this
// path — if they ever share this channel, give them a lossy emit of their own.
func (p *claudeRuntimeProcess) emitLifecycle(event RuntimeEvent) {
	select {
	case p.events <- event:
	case <-p.stopping:
		p.logf("discarding lifecycle event kind=%s because the process is stopped", event.Kind)
	}
}

func (p *claudeRuntimeProcess) signalStop() {
	p.stopOnce.Do(func() { close(p.stopping) })
}

func (p *claudeRuntimeProcess) wasStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

func (p *claudeRuntimeProcess) closeEvents() {
	p.closeOnce.Do(func() {
		close(p.events)
		if p.eventsDone != nil {
			close(p.eventsDone)
		}
	})
}

func (p *claudeRuntimeProcess) waitEventsClosed() {
	if p.eventsDone != nil {
		<-p.eventsDone
	}
}

// recordExitInfo captures why the process ended, before the events channel is
// closed, so a consumer observing the close can read it via ExitInfo().
func (p *claudeRuntimeProcess) recordExitInfo(cmd *exec.Cmd, waitErr error, stderr *claudeStderrTail) {
	info := RuntimeExitInfo{
		ExitCode: -1,
		Expected: p.wasStopped(),
		Stderr:   stderr.linesCopy(),
	}
	if waitErr != nil {
		info.Err = waitErr.Error()
	}
	if state := cmd.ProcessState; state != nil {
		info.ExitCode = state.ExitCode()
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			info.Signal = ws.Signal().String()
		}
	}
	p.mu.Lock()
	p.exitInfo = info
	p.mu.Unlock()
}

func (p *claudeRuntimeProcess) ExitInfo() RuntimeExitInfo {
	if p == nil {
		return RuntimeExitInfo{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitInfo
}

// noteActivity records that a valid stream frame was just decoded (read boundary)
// by advancing the activity generation counter.
func (p *claudeRuntimeProcess) noteActivity() {
	p.lastActivity.Add(1)
}

func (p *claudeRuntimeProcess) ActivitySeq() uint64 {
	if p == nil {
		return 0
	}
	return uint64(p.lastActivity.Load())
}

// claudeStderrTail is the process's stderr writer: it logs complete lines to
// the agent log and keeps the last few for startup-failure diagnostics.
type claudeStderrTail struct {
	process *claudeRuntimeProcess

	mu      sync.Mutex
	partial []byte
	lines   []string
}

func (t *claudeStderrTail) Write(data []byte) (int, error) {
	t.mu.Lock()
	t.partial = append(t.partial, data...)
	var complete []string
	for {
		newline := bytes.IndexByte(t.partial, '\n')
		if newline < 0 {
			break
		}
		complete = append(complete, string(t.partial[:newline]))
		t.partial = t.partial[newline+1:]
	}
	for _, line := range complete {
		t.appendLineLocked(line)
	}
	t.mu.Unlock()
	for _, line := range complete {
		t.process.logf("stderr %s", line)
	}
	return len(data), nil
}

func (t *claudeStderrTail) appendLineLocked(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > 8 {
		t.lines = t.lines[len(t.lines)-8:]
	}
}

func (t *claudeStderrTail) linesCopy() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := append([]string(nil), t.lines...)
	// Include the unterminated tail: a CLI that emits its final diagnostic
	// without a trailing newline leaves it in `partial`, and that is exactly the
	// line death classification may need. The bounded snapshot must not drop it.
	if trailing := strings.TrimSpace(string(t.partial)); trailing != "" {
		parts = append(parts, trailing)
	}
	return parts
}

func (t *claudeStderrTail) String() string {
	return strings.Join(t.linesCopy(), " | ")
}

// claudeStdoutTail keeps the last few stdout lines so a fatal startup reason
// Claude prints to stdout (e.g. bad flags, refused permissions) with empty
// stderr survives into the startup error instead of a bare "exit status 1".
// It is fed only by the sole readLoop reader — never a second reader on the
// pipe — and per-line/count bounds keep an oversized stream from growing it.
type claudeStdoutTail struct {
	mu    sync.Mutex
	lines []string
}

func (t *claudeStdoutTail) record(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	line = truncateForLog(line)
	t.mu.Lock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 8 {
		t.lines = t.lines[len(t.lines)-8:]
	}
	t.mu.Unlock()
}

func (t *claudeStdoutTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}

func (p *claudeRuntimeProcess) logf(format string, args ...any) {
	p.mu.Lock()
	agentLog := p.log
	p.mu.Unlock()
	if agentLog == nil {
		return
	}
	agentLog.Printf(format, args...)
}

func (p *claudeRuntimeProcess) closeLog() {
	p.mu.Lock()
	agentLog := p.log
	p.log = nil
	p.mu.Unlock()
	if agentLog != nil {
		agentLog.Close()
	}
}

const claudeLogLineLimit = 4 * 1024

// Tool results in the stream can be megabytes; cap what lands in agent logs.
func truncateForLog(line string) string {
	if len(line) <= claudeLogLineLimit {
		return line
	}
	return line[:claudeLogLineLimit] + fmt.Sprintf("... (%d bytes truncated)", len(line)-claudeLogLineLimit)
}

type claudeStreamEventKind string

const (
	claudeStreamInit    claudeStreamEventKind = "init"
	claudeStreamTurnEnd claudeStreamEventKind = "turnEnd"
)

type claudeStreamEvent struct {
	kind      claudeStreamEventKind
	sessionID string
	failed    bool
	errText   string
}

// parseClaudeStreamLine reduces one stream-json stdout line to what the
// supervisor cares about: session identity and turn boundaries. Assistant
// deltas, tool calls, and control responses are intentionally ignored.
func parseClaudeStreamLine(line []byte) *claudeStreamEvent {
	var payload struct {
		Type      string   `json:"type"`
		Subtype   string   `json:"subtype"`
		SessionID string   `json:"session_id"`
		IsError   bool     `json:"is_error"`
		Errors    []string `json:"errors"`
		Result    string   `json:"result"`
	}
	if err := json.Unmarshal(line, &payload); err != nil {
		return nil
	}
	switch payload.Type {
	case "system":
		if payload.Subtype == "init" {
			return &claudeStreamEvent{kind: claudeStreamInit, sessionID: strings.TrimSpace(payload.SessionID)}
		}
	case "result":
		event := &claudeStreamEvent{
			kind:      claudeStreamTurnEnd,
			sessionID: strings.TrimSpace(payload.SessionID),
			failed:    payload.IsError,
		}
		if payload.IsError {
			parts := make([]string, 0, len(payload.Errors)+1)
			for _, message := range payload.Errors {
				if trimmed := strings.TrimSpace(message); trimmed != "" {
					parts = append(parts, trimmed)
				}
			}
			if len(parts) == 0 && strings.TrimSpace(payload.Result) != "" {
				parts = append(parts, strings.TrimSpace(payload.Result))
			}
			event.errText = firstNonEmptyText(strings.Join(parts, " | "), "claude turn failed")
		}
		return event
	}
	return nil
}
