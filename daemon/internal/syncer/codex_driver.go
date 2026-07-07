package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

type codexDriver struct {
	cfg     Config
	factory codexAppServerFactory
}

type codexAppServerFactory func(cfg Config, workdir string, toolToken string, agentID string) codexRuntimeApp

type codexRuntimeApp interface {
	Start(context.Context) error
	Stop() error
	Events() <-chan appServerEvent
	PID() int
	ThreadResume(context.Context, string, string, string) error
	ThreadStart(context.Context, string, string) (string, error)
	TurnStart(context.Context, string, string, string) (string, error)
	TurnSteer(context.Context, string, string, string) error
	TurnInterrupt(context.Context, string, string) error
}

func newCodexRuntimeApp(cfg Config, workdir string, toolToken string, agentID string) codexRuntimeApp {
	return newCodexAppServer(cfg, workdir, toolToken, agentID)
}

func newCodexDriver(cfg Config) RuntimeDriver {
	return &codexDriver{
		cfg:     cfg,
		factory: newCodexRuntimeApp,
	}
}

func (d *codexDriver) Kind() RuntimeKind {
	return RuntimeCodex
}

func (d *codexDriver) Detect(ctx context.Context) RuntimeDetection {
	command := strings.TrimSpace(d.cfg.CodexCommand)
	if command == "" {
		command = "codex"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return RuntimeDetection{Kind: RuntimeCodex, Available: false, Reason: "codex command not found"}
	}
	version := ""
	detectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(detectCtx, path, "--version").CombinedOutput()
	if err != nil {
		return RuntimeDetection{Kind: RuntimeCodex, Available: false, Path: path, Reason: "codex --version failed"}
	}
	version = strings.TrimSpace(string(output))
	if _, err := exec.CommandContext(detectCtx, path, "app-server", "--help").CombinedOutput(); err != nil {
		return RuntimeDetection{Kind: RuntimeCodex, Available: false, Version: version, Path: path, Reason: "codex app-server is not available"}
	}
	return RuntimeDetection{Kind: RuntimeCodex, Available: true, Version: version, Path: path}
}

func (d *codexDriver) Spawn(ctx context.Context, spec RuntimeSpawnSpec) (RuntimeProcess, error) {
	_ = ctx
	factory := d.factory
	if factory == nil {
		factory = newCodexRuntimeApp
	}
	app := factory(d.cfg, spec.Workdir, spec.ToolToken, spec.AgentID)
	proc := &codexRuntimeProcess{
		app:          app,
		instructions: spec.Instructions,
		events:       make(chan RuntimeEvent, 128),
	}
	proc.watchdog = newWedgeWatchdog(d.cfg, spec.AgentID, RuntimeCodex, proc.Stop)
	return proc, nil
}

type codexRuntimeProcess struct {
	app          codexRuntimeApp
	instructions string
	events       chan RuntimeEvent
	watchdog     *wedgeWatchdog
}

func (p *codexRuntimeProcess) Start(ctx context.Context) error {
	if p == nil || p.app == nil {
		return errors.New("codex runtime process is missing app-server")
	}
	if err := p.app.Start(ctx); err != nil {
		close(p.events)
		return err
	}
	go p.watchdog.run()
	go p.forwardEvents()
	return nil
}

func (p *codexRuntimeProcess) Stop() error {
	if p == nil || p.app == nil {
		return nil
	}
	return p.app.Stop()
}

func (p *codexRuntimeProcess) WriteStdin(ctx context.Context, input RuntimeInput) (RuntimeWriteResult, error) {
	if p == nil || p.app == nil {
		return RuntimeWriteResult{}, errors.New("codex runtime process is not started")
	}
	switch input.Kind {
	case RuntimeInputResumeSession:
		sessionID := strings.TrimSpace(input.SessionID)
		if sessionID == "" {
			return RuntimeWriteResult{}, errors.New("session id is required to resume Codex thread")
		}
		if err := p.app.ThreadResume(ctx, sessionID, input.CWD, firstNonEmptyText(input.Instructions, p.instructions)); err != nil {
			return RuntimeWriteResult{}, err
		}
		return RuntimeWriteResult{SessionID: sessionID}, nil
	case RuntimeInputStartSession:
		sessionID, err := p.app.ThreadStart(ctx, input.CWD, firstNonEmptyText(input.Instructions, p.instructions))
		if err != nil {
			return RuntimeWriteResult{}, err
		}
		return RuntimeWriteResult{SessionID: sessionID}, nil
	case RuntimeInputStartTurn:
		turnID, err := p.app.TurnStart(ctx, input.SessionID, input.Text, input.CWD)
		if err != nil {
			return RuntimeWriteResult{}, err
		}
		return RuntimeWriteResult{TurnID: turnID}, nil
	case RuntimeInputSteerTurn:
		return RuntimeWriteResult{}, p.app.TurnSteer(ctx, input.SessionID, input.TurnID, input.Text)
	case RuntimeInputInterruptTurn:
		return RuntimeWriteResult{}, p.app.TurnInterrupt(ctx, input.SessionID, input.TurnID)
	default:
		return RuntimeWriteResult{}, errors.New("unsupported runtime input kind " + string(input.Kind))
	}
}

func (p *codexRuntimeProcess) Events() <-chan RuntimeEvent {
	if p == nil {
		return nil
	}
	return p.events
}

func (p *codexRuntimeProcess) PID() int {
	if p == nil || p.app == nil {
		return 0
	}
	return p.app.PID()
}

func (p *codexRuntimeProcess) forwardEvents() {
	defer close(p.events)
	defer p.watchdog.close()
	for event := range p.app.Events() {
		// Bump on EVERY app-server notification, before the lifecycle filter — the raw stream is the
		// fine-grained liveness signal a wedge (silence with a live process) shows up against.
		p.watchdog.noteActivity()
		runtimeEvent, ok := codexRuntimeEvent(event)
		if !ok {
			continue
		}
		switch runtimeEvent.Kind {
		case RuntimeEventTurnStarted:
			p.watchdog.turnStarted(runtimeEvent.TurnID)
		case RuntimeEventTurnCompleted, RuntimeEventTurnFailed, RuntimeEventIdle:
			p.watchdog.turnEnded()
		}
		p.events <- runtimeEvent
	}
}

func codexRuntimeEvent(event appServerEvent) (RuntimeEvent, bool) {
	switch event.Method {
	case "turn/started":
		return RuntimeEvent{Kind: RuntimeEventTurnStarted, TurnID: turnIDFromEvent(event)}, true
	case "turn/completed":
		return RuntimeEvent{Kind: RuntimeEventTurnCompleted}, true
	case "turn/failed":
		return RuntimeEvent{Kind: RuntimeEventTurnFailed}, true
	case "thread/status/changed":
		if threadStatusTypeFromEvent(event) == "idle" {
			return RuntimeEvent{Kind: RuntimeEventIdle}, true
		}
	}
	return RuntimeEvent{}, false
}

func turnIDFromEvent(event appServerEvent) string {
	var payload struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(event.Params, &payload)
	return payload.Turn.ID
}

func threadStatusTypeFromEvent(event appServerEvent) string {
	var payload struct {
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	_ = json.Unmarshal(event.Params, &payload)
	return payload.Status.Type
}
