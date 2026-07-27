package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCodexRuntimeApp struct {
	started     bool
	stopped     bool
	startErr    error
	events      chan appServerEvent
	eventsOnce  sync.Once
	pid         int
	exitInfo    RuntimeExitInfo
	activitySeq uint64

	threadStartID string
	turnStartID   string
	calls         []fakeCodexRuntimeCall
	modelList     func(context.Context, string) (codexModelListPage, error)
	modelCursors  []string
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

func TestCodexDriverDetectVersionProbeUsesStdoutOnly(t *testing.T) {
	t.Run("successful stderr warning does not pollute version", func(t *testing.T) {
		codexPath := fakeVersionProbeCommand(
			t,
			"codex-cli 0.144.5\n",
			"WARNING: failed to clean up stale temp dirs\n",
		)
		app := newFakeCodexRuntimeApp()
		driver := &codexDriver{
			cfg: Config{CodexCommand: codexPath},
			factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
				return app
			},
		}

		detection := driver.Detect(context.Background())

		if !detection.Available || detection.Version != "codex-cli 0.144.5" || detection.Reason != "" {
			t.Fatalf("stdout-only successful version detection = %#v", detection)
		}
	})
}

func TestCodexDriverModelCatalogProjectsVisiblePaginatedModels(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	cursor2 := "opaque cursor / page 2"
	app.modelList = func(ctx context.Context, cursor string) (codexModelListPage, error) {
		_ = ctx
		switch cursor {
		case "":
			return codexModelListPage{
				Data: []codexModelWire{
					fakeCodexModel("gpt-5.6-sol", "GPT-5.6-Sol", true, "low", "low", "medium", "ultra"),
					func() codexModelWire {
						hidden := fakeCodexModel("", "", false, "unsupported", "")
						hidden.Hidden = true
						return hidden
					}(),
				},
				NextCursor: &cursor2,
			}, nil
		case cursor2:
			return codexModelListPage{
				Data: []codexModelWire{
					fakeCodexModel("gpt-5.6-luna", "GPT-5.6-Luna", false, "medium", "low", "medium", "max"),
				},
			}, nil
		default:
			return codexModelListPage{}, errors.New("unexpected cursor")
		}
	}
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	catalog := driver.detectModelCatalog(context.Background())

	if catalog.Error != "" {
		t.Fatalf("unexpected catalog error: %#v", catalog)
	}
	want := []RuntimeModel{
		{
			Model:                  "gpt-5.6-sol",
			DisplayName:            "GPT-5.6-Sol",
			IsDefault:              true,
			ReasoningEfforts:       []string{"low", "medium", "ultra"},
			DefaultReasoningEffort: "low",
		},
		{
			Model:                  "gpt-5.6-luna",
			DisplayName:            "GPT-5.6-Luna",
			ReasoningEfforts:       []string{"low", "medium", "max"},
			DefaultReasoningEffort: "medium",
		},
	}
	if !reflect.DeepEqual(catalog.Models, want) {
		t.Fatalf("projected catalog = %#v, want %#v", catalog.Models, want)
	}
	if !reflect.DeepEqual(app.modelCursors, []string{"", cursor2}) {
		t.Fatalf("model/list cursors = %#v", app.modelCursors)
	}
	if !app.started || !app.stopped {
		t.Fatalf("catalog app lifecycle start=%v stop=%v", app.started, app.stopped)
	}
}

func TestCodexDriverModelCatalogRejectsMalformedProjection(t *testing.T) {
	tests := []struct {
		name  string
		model codexModelWire
	}{
		{name: "missing model", model: fakeCodexModel("", "Display", true, "low", "low")},
		{name: "missing display name", model: fakeCodexModel("model", "", true, "low", "low")},
		{name: "empty effort", model: fakeCodexModel("model", "Display", true, "low", "")},
		{name: "unsupported default effort", model: fakeCodexModel("model", "Display", true, "max", "low")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newFakeCodexRuntimeApp()
			app.modelList = func(context.Context, string) (codexModelListPage, error) {
				return codexModelListPage{Data: []codexModelWire{test.model}}, nil
			}
			driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
				return app
			}}

			catalog := driver.detectModelCatalog(context.Background())

			if catalog.Error != "model catalog unavailable" || len(catalog.Models) != 0 {
				t.Fatalf("malformed catalog result = %#v", catalog)
			}
			if !app.stopped {
				t.Fatal("malformed catalog must stop its app-server")
			}
		})
	}
}

func TestCodexDriverModelCatalogRejectsDuplicateModelAcrossPages(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	cursor := "page-2"
	app.modelList = func(_ context.Context, gotCursor string) (codexModelListPage, error) {
		switch gotCursor {
		case "":
			return codexModelListPage{
				Data:       []codexModelWire{fakeCodexModel("duplicate", "First", true, "low", "low")},
				NextCursor: &cursor,
			}, nil
		case cursor:
			return codexModelListPage{
				Data: []codexModelWire{fakeCodexModel(" duplicate ", "Second", false, "low", "low")},
			}, nil
		default:
			return codexModelListPage{}, errors.New("unexpected cursor")
		}
	}
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	catalog := driver.detectModelCatalog(context.Background())

	if catalog.Error != "model catalog unavailable" || len(catalog.Models) != 0 {
		t.Fatalf("duplicate model catalog = %#v", catalog)
	}
	if !reflect.DeepEqual(app.modelCursors, []string{"", cursor}) {
		t.Fatalf("model/list cursors = %#v", app.modelCursors)
	}
}

func TestCodexDriverModelCatalogRejectsDuplicateEffortAfterTrim(t *testing.T) {
	tests := []struct {
		name    string
		efforts []string
	}{
		{name: "exact duplicate", efforts: []string{"low", "low"}},
		{name: "duplicate after trim", efforts: []string{"low", " low "}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newFakeCodexRuntimeApp()
			app.modelList = func(context.Context, string) (codexModelListPage, error) {
				return codexModelListPage{
					Data: []codexModelWire{fakeCodexModel("model", "Model", true, "low", test.efforts...)},
				}, nil
			}
			driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
				return app
			}}

			catalog := driver.detectModelCatalog(context.Background())

			if catalog.Error != "model catalog unavailable" || len(catalog.Models) != 0 {
				t.Fatalf("duplicate effort catalog = %#v", catalog)
			}
		})
	}
}

func TestCodexDriverModelCatalogRejectsEmptyNextCursor(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	emptyCursor := ""
	app.modelList = func(context.Context, string) (codexModelListPage, error) {
		return codexModelListPage{
			Data:       []codexModelWire{fakeCodexModel("model", "Model", true, "low", "low")},
			NextCursor: &emptyCursor,
		}, nil
	}
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	catalog := driver.detectModelCatalog(context.Background())

	if catalog.Error != "model catalog unavailable" || len(catalog.Models) != 0 {
		t.Fatalf("empty next cursor catalog = %#v", catalog)
	}
	if !reflect.DeepEqual(app.modelCursors, []string{""}) {
		t.Fatalf("model/list cursors = %#v", app.modelCursors)
	}
}

func TestCodexDriverModelCatalogDiscardsEarlierPagesAfterLaterFailure(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	cursor := "page-2"
	app.modelList = func(_ context.Context, gotCursor string) (codexModelListPage, error) {
		if gotCursor == "" {
			return codexModelListPage{
				Data:       []codexModelWire{fakeCodexModel("model", "Model", true, "low", "low")},
				NextCursor: &cursor,
			}, nil
		}
		return codexModelListPage{}, errors.New("page two failed")
	}
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	catalog := driver.detectModelCatalog(context.Background())

	if catalog.Error != "model catalog unavailable" || len(catalog.Models) != 0 {
		t.Fatalf("partially published catalog = %#v", catalog)
	}
	if !reflect.DeepEqual(app.modelCursors, []string{"", cursor}) {
		t.Fatalf("model/list cursors = %#v", app.modelCursors)
	}
}

func TestCodexDriverDetectDefersModelCatalogDiscovery(t *testing.T) {
	codexPath := fakeProcessCommand(t, fakeProcessCodexDetectAvailable)
	app := newFakeCodexRuntimeApp()
	app.modelList = func(context.Context, string) (codexModelListPage, error) {
		return codexModelListPage{}, errors.New("provider catalog failed")
	}
	driver := &codexDriver{
		cfg: Config{CodexCommand: codexPath},
		factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
			return app
		},
	}

	detection := driver.Detect(context.Background())

	if !detection.Available || detection.Version != "codex 0.144.5" {
		t.Fatalf("runtime metadata detection = %#v", detection)
	}
	if detection.ModelCatalog != nil {
		t.Fatalf("startup detection performed catalog discovery: %#v", detection)
	}
	if app.started || app.stopped || len(app.modelCursors) != 0 {
		t.Fatalf("startup touched catalog app-server: started=%v stopped=%v cursors=%#v", app.started, app.stopped, app.modelCursors)
	}
}

func TestCodexDriverModelCatalogBoundsTimeoutAndStops(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	app.modelList = func(ctx context.Context, cursor string) (codexModelListPage, error) {
		_ = cursor
		<-ctx.Done()
		return codexModelListPage{}, ctx.Err()
	}
	driver := &codexDriver{
		catalogTimeout: 20 * time.Millisecond,
		factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
			return app
		},
	}

	started := time.Now()
	catalog := driver.detectModelCatalog(context.Background())

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("catalog timeout was not bounded: %s", elapsed)
	}
	if catalog.Error != "model catalog unavailable" || !app.stopped {
		t.Fatalf("timeout catalog result=%#v stopped=%v", catalog, app.stopped)
	}
}

func TestCodexDriverModelCatalogStopsAfterStartFailure(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	app.startErr = errors.New("initialize failed")
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	catalog := driver.detectModelCatalog(context.Background())

	if catalog.Error != "model catalog unavailable" || !app.stopped {
		t.Fatalf("start failure catalog result=%#v stopped=%v", catalog, app.stopped)
	}
	if len(app.modelCursors) != 0 {
		t.Fatalf("start failure should not request model/list: %#v", app.modelCursors)
	}
}

func TestCodexDriverModelCatalogStopsDuringProviderPanic(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	app.modelList = func(context.Context, string) (codexModelListPage, error) {
		panic("provider panic")
	}
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	func() {
		defer func() {
			if recovered := recover(); recovered != "provider panic" {
				t.Fatalf("panic = %#v, want provider panic", recovered)
			}
		}()
		_ = driver.detectModelCatalog(context.Background())
	}()

	if !app.stopped {
		t.Fatal("provider panic must still stop the catalog app-server")
	}
}

func TestCodexDriverModelCatalogRejectsRepeatedCursor(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	cursor := "same opaque cursor"
	app.modelList = func(context.Context, string) (codexModelListPage, error) {
		return codexModelListPage{NextCursor: &cursor}, nil
	}
	driver := &codexDriver{factory: func(Config, string, string, string, RuntimeProfile) codexRuntimeApp {
		return app
	}}

	catalog := driver.detectModelCatalog(context.Background())

	if catalog.Error != "model catalog unavailable" {
		t.Fatalf("repeated cursor silently truncated the catalog: %#v", catalog)
	}
	if !reflect.DeepEqual(app.modelCursors, []string{"", cursor}) {
		t.Fatalf("model/list cursors = %#v", app.modelCursors)
	}
	if !app.stopped {
		t.Fatal("repeated cursor failure must stop the app-server")
	}
}

func fakeCodexModel(model string, displayName string, isDefault bool, defaultEffort string, efforts ...string) codexModelWire {
	candidate := codexModelWire{
		Model:                  model,
		DisplayName:            displayName,
		IsDefault:              isDefault,
		DefaultReasoningEffort: defaultEffort,
	}
	for _, effort := range efforts {
		candidate.SupportedReasoningEfforts = append(candidate.SupportedReasoningEfforts, struct {
			ReasoningEffort string `json:"reasoningEffort"`
		}{ReasoningEffort: effort})
	}
	return candidate
}

func (f *fakeCodexRuntimeApp) Start(ctx context.Context) error {
	_ = ctx
	f.started = true
	return f.startErr
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

func (f *fakeCodexRuntimeApp) ModelList(ctx context.Context, cursor string) (codexModelListPage, error) {
	f.modelCursors = append(f.modelCursors, cursor)
	if f.modelList != nil {
		return f.modelList(ctx, cursor)
	}
	return codexModelListPage{}, nil
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
		factory: func(cfg Config, workdir string, toolToken string, agentID string, profile RuntimeProfile) codexRuntimeApp {
			_ = cfg
			_ = profile
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

func TestCodexRuntimeProcessProductionFactoryCarriesProfileToAppServer(t *testing.T) {
	profile := RuntimeProfile{Model: "gpt-5.6-sol", ReasoningEffort: "ultra"}
	driver := newCodexDriver(Config{})

	process, err := driver.Spawn(context.Background(), RuntimeSpawnSpec{
		AgentID: "agent_profile_bridge",
		Profile: profile,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	runtimeProcess, ok := process.(*codexRuntimeProcess)
	if !ok {
		t.Fatalf("runtime process type = %T, want *codexRuntimeProcess", process)
	}
	app, ok := runtimeProcess.app.(*codexAppServer)
	if !ok {
		t.Fatalf("runtime app type = %T, want *codexAppServer", runtimeProcess.app)
	}
	if !reflect.DeepEqual(app.profile, profile) {
		t.Fatalf("production app profile = %#v, want %#v", app.profile, profile)
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("stop unstarted process: %v", err)
	}
}

// TestCodexLiveModelProfileSmoke validates the installed Codex CLI end to end
// without pinning provider-specific model names or catalog sizes:
//
//	NOTTY_CODEX_LIVE_TEST=1 go test ./daemon/internal/syncer -run TestCodexLiveModelProfileSmoke -count=2 -v
func TestCodexLiveModelProfileSmoke(t *testing.T) {
	if os.Getenv("NOTTY_CODEX_LIVE_TEST") != "1" {
		t.Skip("set NOTTY_CODEX_LIVE_TEST=1 to run against the real codex CLI")
	}
	cfg := LoadConfig()
	cfg.DataDir = t.TempDir()
	driver := newCodexDriver(cfg)
	detection := driver.Detect(context.Background())
	if !detection.Available {
		t.Fatalf("codex CLI unavailable: %s", detection.Reason)
	}
	if detection.ModelCatalog == nil || detection.ModelCatalog.Error != "" {
		t.Fatalf("model catalog unavailable: %#v", detection.ModelCatalog)
	}

	seenModels := map[string]struct{}{}
	defaults := []RuntimeModel{}
	for _, model := range detection.ModelCatalog.Models {
		if model.Model == "" || model.DisplayName == "" {
			t.Fatalf("catalog contains an invalid model: %#v", model)
		}
		if _, duplicate := seenModels[model.Model]; duplicate {
			t.Fatalf("catalog contains duplicate model %q", model.Model)
		}
		seenModels[model.Model] = struct{}{}
		seenEfforts := map[string]struct{}{}
		for _, effort := range model.ReasoningEfforts {
			if effort == "" {
				t.Fatalf("model %q contains an empty effort", model.Model)
			}
			if _, duplicate := seenEfforts[effort]; duplicate {
				t.Fatalf("model %q contains duplicate effort %q", model.Model, effort)
			}
			seenEfforts[effort] = struct{}{}
		}
		if model.DefaultReasoningEffort != "" {
			if _, supported := seenEfforts[model.DefaultReasoningEffort]; !supported {
				t.Fatalf("model %q default effort %q is unsupported", model.Model, model.DefaultReasoningEffort)
			}
		}
		if model.IsDefault {
			defaults = append(defaults, model)
		}
	}
	if len(seenModels) == 0 {
		t.Fatal("model catalog is empty")
	}
	if len(defaults) != 1 {
		t.Fatalf("model catalog defaults = %d, want exactly one", len(defaults))
	}

	selected := defaults[0]
	profile := RuntimeProfile{
		Model:           selected.Model,
		ReasoningEffort: selected.DefaultReasoningEffort,
	}
	workdir := t.TempDir()
	spec := RuntimeSpawnSpec{
		AgentID:      "agent_codex_live",
		Workdir:      workdir,
		Instructions: "Reply with the single word ok and do nothing else.",
		Profile:      profile,
	}
	process, err := driver.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = process.Stop() })
	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStart()
	if err := process.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	sessionCtx, cancelSession := context.WithTimeout(context.Background(), 30*time.Second)
	session, err := process.WriteStdin(sessionCtx, RuntimeInput{
		Kind: RuntimeInputStartSession,
		CWD:  workdir,
	})
	cancelSession()
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	turnCtx, cancelTurn := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = process.WriteStdin(turnCtx, RuntimeInput{
		Kind:      RuntimeInputStartTurn,
		SessionID: session.SessionID,
		CWD:       workdir,
		Text:      "Reply with exactly: ok",
	})
	cancelTurn()
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	waitForCodexLiveTurnCompletion(t, process.Events(), 3*time.Minute)
	if err := process.Stop(); err != nil {
		t.Fatalf("stop first process: %v", err)
	}

	resumed, err := driver.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("spawn for resume: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Stop() })
	resumeCtx, cancelResume := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelResume()
	if err := resumed.Start(resumeCtx); err != nil {
		t.Fatalf("start for resume: %v", err)
	}
	if _, err := resumed.WriteStdin(resumeCtx, RuntimeInput{
		Kind:      RuntimeInputResumeSession,
		SessionID: session.SessionID,
		CWD:       workdir,
	}); err != nil {
		t.Fatalf("resume session: %v", err)
	}
	if err := resumed.Stop(); err != nil {
		t.Fatalf("stop resumed process: %v", err)
	}
}

func waitForCodexLiveTurnCompletion(t *testing.T, events <-chan RuntimeEvent, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("codex event channel closed before turn completion")
			}
			switch event.Kind {
			case RuntimeEventTurnCompleted:
				return
			case RuntimeEventTurnFailed:
				t.Fatal("codex turn failed")
			}
		case <-timer.C:
			t.Fatal("timed out waiting for codex turn completion")
		}
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
