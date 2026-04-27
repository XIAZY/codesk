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
)

type appServerEvent struct {
	Method string
	Params json.RawMessage
}

type appServerClient interface {
	Start(ctx context.Context) error
	Stop() error
	ThreadStart(ctx context.Context, cwd string, instructions string) (string, error)
	ThreadResume(ctx context.Context, threadID string, cwd string, instructions string) error
	TurnStart(ctx context.Context, threadID string, prompt string, cwd string) (string, error)
	TurnSteer(ctx context.Context, threadID string, turnID string, message string) error
	TurnInterrupt(ctx context.Context, threadID string, turnID string) error
	Events() <-chan appServerEvent
	PID() int
}

type appServerFactory func(cfg Config, workdir string, toolToken string, logName string) appServerClient

func newCodexAppServer(cfg Config, workdir string, toolToken string, logName string) appServerClient {
	return &codexAppServer{
		cfg:       cfg,
		workdir:   workdir,
		toolToken: toolToken,
		logName:   logName,
		pending:   map[int64]chan appServerResponse{},
		events:    make(chan appServerEvent, 128),
		done:      make(chan error, 1),
	}
}

type codexAppServer struct {
	cfg       Config
	workdir   string
	toolToken string
	logName   string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan appServerResponse
	events  chan appServerEvent
	done    chan error
	log     *agentLog
}

type appServerResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *codexAppServer) Start(ctx context.Context) error {
	if agentLog, err := openAgentLog(c.workdir, c.logName); err == nil {
		c.log = agentLog
		c.log.Printf("starting codex app-server command=%s workdir=%s", c.cfg.CodexCommand, c.workdir)
	} else {
		log.Printf("agent log open failed workdir=%s err=%v", c.workdir, err)
	}
	cmd := exec.CommandContext(ctx, c.cfg.CodexCommand, "app-server")
	cmd.Dir = c.workdir
	cmd.Env = append(os.Environ(), buildCodexEnv(c.cfg, c.toolToken)...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.log.Printf("stdin pipe failed err=%v", err)
		c.log.Close()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.log.Printf("stdout pipe failed err=%v", err)
		c.log.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.log.Printf("stderr pipe failed err=%v", err)
		c.log.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		c.log.Printf("codex app-server start failed err=%v", err)
		c.log.Close()
		return err
	}
	c.cmd = cmd
	c.stdin = stdin
	c.log.Printf("codex app-server started pid=%d", cmd.Process.Pid)
	go c.readLoop(stdout)
	go c.stderrLoop(stderr)
	go func() {
		err := cmd.Wait()
		c.log.Printf("codex app-server exited err=%v", err)
		c.done <- err
		close(c.events)
	}()
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

func (c *codexAppServer) Stop() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	c.log.Printf("stopping codex app-server pid=%d", c.cmd.Process.Pid)
	defer c.log.Close()
	_ = c.stdin.Close()
	return c.cmd.Process.Kill()
}

func (c *codexAppServer) Events() <-chan appServerEvent {
	return c.events
}

func (c *codexAppServer) PID() int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
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
			return nil, fmt.Errorf("app-server %s failed: %s", method, res.Error.Message)
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
	c.log.Printf("jsonrpc send %s", string(bytes))
	return nil
}

func (c *codexAppServer) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		c.log.Printf("jsonrpc recv %s", string(line))
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			c.log.Printf("jsonrpc recv malformed bytes=%d", len(line))
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
		select {
		case c.events <- appServerEvent{Method: notification.Method, Params: notification.Params}:
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		c.log.Printf("jsonrpc stdout scan error err=%v", err)
	}
}

func (c *codexAppServer) stderrLoop(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		c.log.Printf("stderr %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		c.log.Printf("stderr scan error err=%v", err)
	}
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
	c.log.Printf("jsonrpc send %s", string(bytes))
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
