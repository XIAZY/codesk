package syncer

import (
	"bufio"
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
)

type appServerEvent struct {
	Method string
	Params json.RawMessage
}

func newCodexAppServer(cfg Config, workdir string, toolToken string, agentID string) *codexAppServer {
	return &codexAppServer{
		cfg:        cfg,
		workdir:    workdir,
		toolToken:  toolToken,
		agentID:    agentID,
		pending:    map[int64]chan appServerResponse{},
		events:     make(chan appServerEvent, 128),
		done:       make(chan error, 1),
		stopping:   make(chan struct{}),
		readDone:   make(chan struct{}),
		stderrDone: make(chan struct{}),
		exited:     make(chan struct{}),
	}
}

type codexAppServer struct {
	cfg       Config
	workdir   string
	toolToken string
	agentID   string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nextID atomic.Int64
	// lastActivity is the count of valid JSON-RPC frames the read loop has decoded,
	// incremented atomically (one Add per valid frame, no lock) as the activity
	// generation the supervisor heartbeat polls. 0 means none decoded yet.
	lastActivity atomic.Int64

	mu         sync.Mutex
	pending    map[int64]chan appServerResponse
	events     chan appServerEvent
	done       chan error
	stopOnce   sync.Once
	stopping   chan struct{}
	readDone   chan struct{}
	stderrDone chan struct{}
	exited     chan struct{}
	exitOnce   sync.Once
	eventsOnce sync.Once
	logOnce    sync.Once
	stopErr    error
	log        *agentLog
	expected   bool            // true once Stop() deliberately killed the process
	stderrRing []string        // bounded ring of the most recent stderr lines
	exitInfo   RuntimeExitInfo // set by the exit goroutine before events closes

	// Test-only ordering seam. Production leaves this nil.
	testHookBeforeExitComplete func()
}

type appServerResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type appServerRPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *appServerRPCError) Error() string {
	return fmt.Sprintf("app-server %s failed: %s", e.Method, e.Message)
}

func (c *codexAppServer) Start(ctx context.Context) error {
	select {
	case <-c.stopping:
		return errors.New("codex app-server is stopped")
	default:
	}
	if agentLog, err := openAgentLog(c.cfg, c.agentID); err == nil {
		c.log = agentLog
		c.logf("starting codex app-server command=%s agent=%s workdir=%s", c.cfg.CodexCommand, c.agentID, c.workdir)
	} else {
		log.Printf("agent log open failed agent=%s data_dir=%s err=%v", c.agentID, c.cfg.DataDir, err)
	}
	// The context controls construction and the initialize handshake below. Once
	// Start succeeds, the published RuntimeProcess owns this child until Stop.
	// Binding the OS child to ctx would kill a healthy session when the supervisor
	// cancels its per-attempt construction context immediately after publication.
	cmd := exec.Command(c.cfg.CodexCommand, "app-server")
	cmd.Dir = c.workdir
	cmd.Env = append(os.Environ(), buildAgentToolEnv(c.cfg, c.toolToken)...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.logf("stdin pipe failed err=%v", err)
		c.completeWithoutProcess()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.logf("stdout pipe failed err=%v", err)
		_ = stdin.Close()
		c.completeWithoutProcess()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.logf("stderr pipe failed err=%v", err)
		_ = stdin.Close()
		_ = stdout.Close()
		c.completeWithoutProcess()
		return err
	}
	if err := cmd.Start(); err != nil {
		c.logf("codex app-server start failed err=%v", err)
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		c.completeWithoutProcess()
		return err
	}
	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.mu.Unlock()
	c.logf("codex app-server started pid=%d", cmd.Process.Pid)
	go c.readLoop(stdout)
	go c.stderrLoop(stderr)
	go c.waitForExit(cmd)
	if _, err := c.request(ctx, "initialize", map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]string{
			"name":    "notty",
			"title":   "notty daemon",
			"version": "0.1.0",
		},
	}); err != nil {
		_ = c.Stop()
		return err
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		_ = c.Stop()
		return err
	}
	return nil
}

func (c *codexAppServer) logf(format string, args ...any) {
	if c == nil || c.log == nil {
		return
	}
	c.log.Printf(format, args...)
}

func (c *codexAppServer) closeLog() {
	if c == nil {
		return
	}
	c.logOnce.Do(func() {
		if c.log != nil {
			c.log.Close()
		}
	})
}

func (c *codexAppServer) Stop() error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.stopping)
		c.mu.Lock()
		// ExitInfo snapshots this bit after Wait. Set it before closing stdin or
		// killing so every deliberate-stop exit is classified as expected.
		c.expected = true
		cmd := c.cmd
		stdin := c.stdin
		c.mu.Unlock()
		if cmd == nil || cmd.Process == nil {
			// A raw Stop before Start has no process/readers whose app event stream
			// this layer can close safely. The RuntimeProcess wrapper closes its public
			// stream; still broadcast terminal completion so later Stop calls cannot
			// hang. Start's own pre-cmd.Start failures use completeWithoutProcess.
			c.closeLog()
			c.closeExited()
			return
		}
		c.logf("stopping codex app-server pid=%d", cmd.Process.Pid)
		if stdin != nil {
			_ = stdin.Close()
		}
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.stopErr = err
		}
	})

	// The exit goroutine owns Wait, reader drainage, ExitInfo, and channel
	// closure. All Stop callers join that one broadcast completion; none consume
	// the one-shot process result in done or hold c.mu while waiting.
	<-c.exited
	return c.stopErr
}

func (c *codexAppServer) Events() <-chan appServerEvent {
	return c.events
}

func (c *codexAppServer) PID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *codexAppServer) waitForExit(cmd *exec.Cmd) {
	// Drain stderr to its natural EOF BEFORE cmd.Wait(): Wait closes the pipes on
	// process exit and does not wait for reader goroutines, so calling it first can
	// truncate the final diagnostic that death classification depends on.
	<-c.stderrDone
	err := cmd.Wait()
	c.logf("codex app-server exited err=%v", err)
	c.recordExitInfo(cmd, err)
	c.done <- err
	<-c.readDone
	if c.testHookBeforeExitComplete != nil {
		c.testHookBeforeExitComplete()
	}
	c.closeEvents()
	c.closeLog()
	c.closeExited()
}

func (c *codexAppServer) completeWithoutProcess() {
	c.closeEvents()
	c.closeLog()
	c.closeExited()
}

func (c *codexAppServer) closeEvents() {
	c.eventsOnce.Do(func() { close(c.events) })
}

func (c *codexAppServer) closeExited() {
	c.exitOnce.Do(func() { close(c.exited) })
}

func (c *codexAppServer) ThreadStart(ctx context.Context, cwd string, instructions string) (string, error) {
	result, err := c.request(ctx, "thread/start", c.threadParams(cwd, "", instructions))
	if err != nil {
		return "", err
	}
	return appServerThreadID(result)
}

func (c *codexAppServer) ThreadResume(ctx context.Context, threadID string, cwd string, instructions string) error {
	_, err := c.request(ctx, "thread/resume", c.threadParams(cwd, threadID, instructions))
	return err
}

func (c *codexAppServer) TurnStart(ctx context.Context, threadID string, prompt string, cwd string) (string, error) {
	result, err := c.request(ctx, "turn/start", map[string]any{
		"threadId":       threadID,
		"input":          []map[string]string{{"type": "text", "text": prompt}},
		"cwd":            cwd,
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "dangerFullAccess"},
	})
	if err != nil {
		return "", err
	}
	return appServerTurnID(result)
}

func (c *codexAppServer) TurnSteer(ctx context.Context, threadID string, turnID string, message string) error {
	_, err := c.request(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input":          []map[string]string{{"type": "text", "text": message}},
	})
	return err
}

func (c *codexAppServer) TurnInterrupt(ctx context.Context, threadID string, turnID string) error {
	_, err := c.request(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
	return err
}

func (c *codexAppServer) threadParams(cwd string, threadID string, instructions string) map[string]any {
	params := map[string]any{
		"cwd":            cwd,
		"approvalPolicy": "never",
		"sandbox":        "danger-full-access",
		"serviceName":    "notty",
	}
	if threadID != "" {
		params["threadId"] = threadID
		params["excludeTurns"] = true
	}
	if strings.TrimSpace(instructions) != "" {
		params["developerInstructions"] = instructions
	}
	return params
}

func (c *codexAppServer) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan appServerResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case res := <-ch:
		if res.Error != nil {
			return nil, &appServerRPCError{Method: method, Code: res.Error.Code, Message: res.Error.Message}
		}
		return res.Result, nil
	}
}

func (c *codexAppServer) notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *codexAppServer) write(payload map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		return errors.New("app-server stdin is not open")
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(bytes, '\n')); err != nil {
		return err
	}
	c.logf("jsonrpc send %s", string(bytes))
	return nil
}

func (c *codexAppServer) readLoop(stdout io.Reader) {
	defer close(c.readDone)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		// Liveness at the LITERAL read boundary: increment the activity generation the
		// instant a syntactically valid JSON-object frame is decoded — BEFORE the
		// synchronous logf (log/global-write mutexes + file I/O) and before dispatch —
		// via the SAME shared object validator as the Claude boundary. A contended log
		// sink must never stall liveness for a demonstrably-live runtime (blocker 17);
		// `null` and malformed input are non-objects and never count (blocker 2).
		if isValidRuntimeFrame(line) {
			c.noteActivity()
		}
		c.logf("jsonrpc recv %s", string(line))
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
			// Not a JSON-RPC object (malformed, or valid-but-non-object like `null`):
			// nothing to dispatch. Liveness was already (correctly) not counted above.
			c.logf("jsonrpc recv non-object bytes=%d", len(line))
			continue
		}
		if idRaw, ok := raw["id"]; ok {
			if _, isServerRequest := raw["method"]; isServerRequest {
				c.handleServerRequest(line)
				continue
			}
			var response appServerResponse
			if err := json.Unmarshal(line, &response); err != nil {
				continue
			}
			var id int64
			_ = json.Unmarshal(idRaw, &id)
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- response
			}
			continue
		}
		var notification struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &notification); err != nil || notification.Method == "" {
			continue
		}
		c.emitEvent(appServerEvent{Method: notification.Method, Params: notification.Params})
	}
	if err := scanner.Err(); err != nil {
		c.logf("jsonrpc stdout scan error err=%v", err)
	}
}

func (c *codexAppServer) emitEvent(event appServerEvent) {
	if isLifecycleNotification(event) {
		select {
		case c.events <- event:
		case <-c.stopping:
		}
		return
	}
	select {
	case c.events <- event:
	default:
	}
}

// Derive transport backpressure from the exact runtime mapping so telemetry
// cannot become blocking when lifecycle semantics evolve.
func isLifecycleNotification(event appServerEvent) bool {
	_, ok := codexRuntimeEvent(event)
	return ok
}

func (c *codexAppServer) stderrLoop(stderr io.Reader) {
	defer close(c.stderrDone)
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		c.logf("stderr %s", line)
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			c.mu.Lock()
			c.stderrRing = append(c.stderrRing, trimmed)
			if len(c.stderrRing) > 8 {
				c.stderrRing = c.stderrRing[len(c.stderrRing)-8:]
			}
			c.mu.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		c.logf("stderr scan error err=%v", err)
	}
}

// recordExitInfo captures why the app-server process ended, before the events
// channel closes so a consumer observing the close can read it via ExitInfo().
func (c *codexAppServer) recordExitInfo(cmd *exec.Cmd, waitErr error) {
	c.mu.Lock()
	info := RuntimeExitInfo{
		ExitCode: -1,
		Expected: c.expected,
		Stderr:   append([]string(nil), c.stderrRing...),
	}
	c.mu.Unlock()
	if waitErr != nil {
		info.Err = waitErr.Error()
	}
	if state := cmd.ProcessState; state != nil {
		info.ExitCode = state.ExitCode()
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			info.Signal = ws.Signal().String()
		}
	}
	c.mu.Lock()
	c.exitInfo = info
	c.mu.Unlock()
}

func (c *codexAppServer) ExitInfo() RuntimeExitInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitInfo
}

// noteActivity records that a valid JSON-RPC frame was just decoded (read
// boundary) by advancing the activity generation counter.
func (c *codexAppServer) noteActivity() {
	c.lastActivity.Add(1)
}

func (c *codexAppServer) ActivitySeq() uint64 {
	return uint64(c.lastActivity.Load())
}

func (c *codexAppServer) handleServerRequest(payload []byte) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(payload, &request); err != nil || request.Method == "" {
		return
	}
	if result, ok := approvalResponseForMethod(request.Method); ok {
		_ = c.writeRawResponse(request.ID, result, nil)
		return
	}
	switch request.Method {
	case "item/tool/requestUserInput", "mcpServer/elicitation/request":
		_ = c.writeRawResponse(request.ID, nil, map[string]any{
			"code":    -32000,
			"message": "notty daemon cannot request interactive input",
		})
	default:
		_ = c.writeRawResponse(request.ID, nil, map[string]any{
			"code":    -32601,
			"message": "notty daemon does not implement " + request.Method,
		})
	}
}

func approvalResponseForMethod(method string) (map[string]any, bool) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return map[string]any{"decision": "accept"}, true
	case "applyPatchApproval", "execCommandApproval":
		return map[string]any{"decision": "approved"}, true
	case "item/permissions/requestApproval":
		return map[string]any{
			"permissions": map[string]any{
				"fileSystem": map[string]any{
					"read":  []string{"/"},
					"write": []string{"/"},
				},
				"network": map[string]any{"enabled": true},
			},
			"scope": "session",
		}, true
	default:
		return nil, false
	}
}

func (c *codexAppServer) writeRawResponse(id json.RawMessage, result any, responseError any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		return errors.New("app-server stdin is not open")
	}
	payload := map[string]json.RawMessage{"id": id}
	if responseError != nil {
		bytes, err := json.Marshal(responseError)
		if err != nil {
			return err
		}
		payload["error"] = bytes
	} else {
		bytes, err := json.Marshal(result)
		if err != nil {
			return err
		}
		payload["result"] = bytes
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(bytes, '\n')); err != nil {
		return err
	}
	c.logf("jsonrpc send %s", string(bytes))
	return nil
}

func appServerThreadID(result json.RawMessage) (string, error) {
	var payload struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", err
	}
	if payload.Thread.ID == "" {
		return "", errors.New("app-server response missing thread id")
	}
	return payload.Thread.ID, nil
}

func appServerTurnID(result json.RawMessage) (string, error) {
	var payload struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", err
	}
	if payload.Turn.ID == "" {
		return "", errors.New("app-server response missing turn id")
	}
	return payload.Turn.ID, nil
}
