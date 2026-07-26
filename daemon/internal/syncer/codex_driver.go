package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type codexDriver struct {
	cfg            Config
	factory        codexAppServerFactory
	catalogTimeout time.Duration
}

type codexAppServerFactory func(cfg Config, workdir string, toolToken string, agentID string, profile RuntimeProfile) codexRuntimeApp

type codexRuntimeApp interface {
	Start(context.Context) error
	Stop() error
	Events() <-chan appServerEvent
	ExitInfo() RuntimeExitInfo
	ActivitySeq() uint64
	PID() int
	ThreadResume(context.Context, string, string, string) error
	ThreadStart(context.Context, string, string) (string, error)
	ModelList(context.Context, string) (codexModelListPage, error)
	TurnStart(context.Context, string, string, string) (string, error)
	TurnSteer(context.Context, string, string, string) error
	TurnInterrupt(context.Context, string, string) error
}

func newCodexRuntimeApp(cfg Config, workdir string, toolToken string, agentID string, profile RuntimeProfile) codexRuntimeApp {
	app := newCodexAppServer(cfg, workdir, toolToken, agentID)
	app.profile = profile
	return app
}

func newCodexDriver(cfg Config) RuntimeDriver {
	return &codexDriver{
		cfg:            cfg,
		factory:        newCodexRuntimeApp,
		catalogTimeout: 5 * time.Second,
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
	output, err := managedBackgroundCommandContext(detectCtx, path, "--version").Output()
	if err != nil {
		return RuntimeDetection{Kind: RuntimeCodex, Available: false, Path: path, Reason: "codex --version failed"}
	}
	version = strings.TrimSpace(string(output))
	if _, err := managedBackgroundCommandContext(detectCtx, path, "app-server", "--help").CombinedOutput(); err != nil {
		return RuntimeDetection{Kind: RuntimeCodex, Available: false, Version: version, Path: path, Reason: "codex app-server is not available"}
	}
	return RuntimeDetection{Kind: RuntimeCodex, Available: true, Version: version, Path: path}
}

type codexModelListPage struct {
	Data       []codexModelWire `json:"data"`
	NextCursor *string          `json:"nextCursor"`
}

type codexModelWire struct {
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	Hidden                    bool   `json:"hidden"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort string `json:"defaultReasoningEffort"`
	IsDefault              bool   `json:"isDefault"`
}

func (d *codexDriver) detectModelCatalog(ctx context.Context) *RuntimeModelCatalog {
	timeout := d.catalogTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	catalogCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	factory := d.factory
	if factory == nil {
		factory = newCodexRuntimeApp
	}
	app := factory(d.cfg, "", "", "runtime-detection", RuntimeProfile{})
	if app == nil {
		return codexModelCatalogError(errors.New("codex app-server factory returned nil"))
	}
	defer func() {
		if err := app.Stop(); err != nil {
			log.Printf("codex model catalog app-server stop failed: %v", err)
		}
	}()
	if err := app.Start(catalogCtx); err != nil {
		return codexModelCatalogError(fmt.Errorf("start codex app-server: %w", err))
	}

	models := []RuntimeModel{}
	seenModels := map[string]struct{}{}
	seenCursors := map[string]struct{}{}
	cursor := ""
	for {
		page, err := app.ModelList(catalogCtx, cursor)
		if err != nil {
			return codexModelCatalogError(fmt.Errorf("codex model/list cursor %q: %w", cursor, err))
		}
		for _, candidate := range page.Data {
			if candidate.Hidden {
				continue
			}
			model, err := projectCodexModel(candidate)
			if err != nil {
				return codexModelCatalogError(err)
			}
			if _, duplicate := seenModels[model.Model]; duplicate {
				return codexModelCatalogError(fmt.Errorf("codex model/list returned duplicate model %q", model.Model))
			}
			seenModels[model.Model] = struct{}{}
			models = append(models, model)
		}
		if page.NextCursor == nil {
			return &RuntimeModelCatalog{Models: models}
		}
		next := *page.NextCursor
		if next == "" {
			return codexModelCatalogError(errors.New("codex model/list returned an empty next cursor"))
		}
		if _, duplicate := seenCursors[next]; duplicate {
			return codexModelCatalogError(fmt.Errorf("codex model/list repeated cursor %q", next))
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
}

func projectCodexModel(candidate codexModelWire) (RuntimeModel, error) {
	model := strings.TrimSpace(candidate.Model)
	displayName := strings.TrimSpace(candidate.DisplayName)
	defaultEffort := strings.TrimSpace(candidate.DefaultReasoningEffort)
	if model == "" || displayName == "" {
		return RuntimeModel{}, errors.New("codex model/list returned a model without model or displayName")
	}
	efforts := make([]string, 0, len(candidate.SupportedReasoningEfforts))
	seen := map[string]struct{}{}
	for _, supported := range candidate.SupportedReasoningEfforts {
		effort := strings.TrimSpace(supported.ReasoningEffort)
		if effort == "" {
			return RuntimeModel{}, fmt.Errorf("codex model/list returned an empty effort for model %q", model)
		}
		if _, duplicate := seen[effort]; duplicate {
			return RuntimeModel{}, fmt.Errorf("codex model/list returned duplicate effort %q for model %q", effort, model)
		}
		seen[effort] = struct{}{}
		efforts = append(efforts, effort)
	}
	if defaultEffort != "" {
		if _, ok := seen[defaultEffort]; !ok {
			return RuntimeModel{}, fmt.Errorf("codex model/list default effort %q is unsupported by model %q", defaultEffort, model)
		}
	}
	return RuntimeModel{
		Model:                  model,
		DisplayName:            displayName,
		IsDefault:              candidate.IsDefault,
		ReasoningEfforts:       efforts,
		DefaultReasoningEffort: defaultEffort,
	}, nil
}

func codexModelCatalogError(err error) *RuntimeModelCatalog {
	log.Printf("codex model catalog unavailable: %v", err)
	return &RuntimeModelCatalog{Models: []RuntimeModel{}, Error: "model catalog unavailable"}
}

func (d *codexDriver) Spawn(ctx context.Context, spec RuntimeSpawnSpec) (RuntimeProcess, error) {
	_ = ctx
	factory := d.factory
	if factory == nil {
		factory = newCodexRuntimeApp
	}
	app := factory(d.cfg, spec.Workdir, spec.ToolToken, spec.AgentID, spec.Profile)
	return &codexRuntimeProcess{
		app:          app,
		instructions: spec.Instructions,
		events:       make(chan RuntimeEvent, 128),
		eventsDone:   make(chan struct{}),
		stopping:     make(chan struct{}),
	}, nil
}

type codexRuntimeProcess struct {
	app          codexRuntimeApp
	instructions string
	events       chan RuntimeEvent
	eventsDone   chan struct{}
	eventsOnce   sync.Once
	stopOnce     sync.Once
	stopping     chan struct{}
	stopErr      error

	mu      sync.Mutex
	started bool
}

func (p *codexRuntimeProcess) Start(ctx context.Context) error {
	if p == nil || p.app == nil {
		return errors.New("codex runtime process is missing app-server")
	}
	if err := p.app.Start(ctx); err != nil {
		p.closeEvents()
		return err
	}
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	go p.forwardEvents()
	return nil
}

func (p *codexRuntimeProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		close(p.stopping)
		if p.app != nil {
			p.stopErr = p.app.Stop()
		}
	})
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		// Start either never ran or failed before forwardEvents was launched.
		p.closeEvents()
	}
	<-p.eventsDone
	return p.stopErr
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

func (p *codexRuntimeProcess) ExitInfo() RuntimeExitInfo {
	if p == nil || p.app == nil {
		return RuntimeExitInfo{}
	}
	return p.app.ExitInfo()
}

func (p *codexRuntimeProcess) ActivitySeq() uint64 {
	if p == nil || p.app == nil {
		return 0
	}
	return p.app.ActivitySeq()
}

func (p *codexRuntimeProcess) forwardEvents() {
	defer p.closeEvents()
	for event := range p.app.Events() {
		runtimeEvent, ok := codexRuntimeEvent(event)
		if !ok {
			continue
		}
		select {
		case p.events <- runtimeEvent:
		case <-p.stopping:
			// Stop requested: stop pushing new events to a consumer that may
			// have gone away, but keep draining the app channel until IT closes.
			// The app closes its event channel only after its exit goroutine has
			// published ExitInfo, so closing our public channel early here would
			// expose a zero snapshot (Expected=false) and make a deliberate Stop
			// misclassify as a transient crash.
		}
	}
}

func (p *codexRuntimeProcess) closeEvents() {
	p.eventsOnce.Do(func() {
		close(p.events)
		close(p.eventsDone)
	})
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
