// Package desktopapp coordinates the platform-neutral desktop application
// lifecycle from native adapters supplied by an operating-system composition
// root.
package desktopapp

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"

	"notty/daemon/internal/desktop"
	"notty/daemon/internal/syncer"
)

var errDesktopNotConfigured = errors.New("desktop connection is not configured")

// Options contains the shared coordinator inputs supplied by a platform
// composition root.
type Options struct {
	Dirs          desktop.Dirs
	CodeskOrigin  string
	BackendOrigin string
	Version       string
	ConfigStore   desktop.ConfigurationStore
	Secrets       desktop.SecretStore
	LoginItem     desktop.LoginItem
	Opener        desktop.OpenURL
	Logger        *log.Logger
}

// Application owns the embedded daemon controller, menu actions, connection
// handoffs, and coordinated shutdown. Platform composition roots supply only
// the adapters in Options and render the typed menu channels.
type Application struct {
	dirs          desktop.Dirs
	codeskOrigin  string
	backendOrigin string
	version       string
	configStore   desktop.ConfigurationStore
	secrets       desktop.SecretStore
	loginItem     desktop.LoginItem
	opener        desktop.OpenURL
	logger        *log.Logger

	controller *desktop.Controller
	actions    chan desktop.MenuAction
	updates    chan desktop.MenuModel
	wake       chan struct{}
	loopDone   chan struct{}

	stateMu       sync.RWMutex
	config        desktop.Configuration
	hasConfig     bool
	token         string
	connecting    bool
	launchAtLogin bool

	lifecycleMu sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	loopStarted bool
	startErr    error
	start       sync.Once
	shutdown    sync.Once
	connects    sync.WaitGroup
}

// New constructs the shared desktop application coordinator.
func New(options Options) (*Application, error) {
	return newApplication(options, nil)
}

func newApplication(options Options, serviceFactory desktop.ServiceFactory) (*Application, error) {
	if err := options.Dirs.Validate(); err != nil {
		return nil, err
	}
	if options.ConfigStore == nil || options.Secrets == nil || options.LoginItem == nil || options.Opener == nil {
		return nil, errors.New("codesk desktop: platform adapters are required")
	}
	if options.Logger == nil {
		options.Logger = log.New(os.Stderr, "codesk-desktop: ", log.LstdFlags|log.LUTC)
	}
	app := &Application{
		dirs:          options.Dirs,
		codeskOrigin:  options.CodeskOrigin,
		backendOrigin: options.BackendOrigin,
		version:       options.Version,
		configStore:   options.ConfigStore,
		secrets:       options.Secrets,
		loginItem:     options.LoginItem,
		opener:        options.Opener,
		logger:        options.Logger,
		actions:       make(chan desktop.MenuAction, 8),
		updates:       make(chan desktop.MenuModel, 8),
		wake:          make(chan struct{}, 1),
		loopDone:      make(chan struct{}),
	}
	app.loadDurableState()
	if serviceFactory == nil {
		serviceFactory = app.newService
	}
	controller, err := desktop.NewController(serviceFactory, desktop.ControllerOptions{})
	if err != nil {
		return nil, err
	}
	app.controller = controller
	return app, nil
}

func (a *Application) loadDurableState() {
	config, configErr := a.configStore.Load()
	if configErr == nil {
		a.config = config
		a.hasConfig = true
	} else if !errors.Is(configErr, os.ErrNotExist) {
		a.logger.Printf("load configuration: %v", configErr)
	}
	token, tokenErr := a.secrets.Load(desktop.SecretKeyDaemonToken)
	if tokenErr == nil {
		defer clear(token)
		a.token = string(token)
	} else if !errors.Is(tokenErr, os.ErrNotExist) {
		a.logger.Printf("load protected credential: %v", tokenErr)
	}
	launchAtLogin, err := a.loginItem.IsEnabled()
	if err != nil {
		a.logger.Printf("read login item: %v", err)
	} else {
		a.launchAtLogin = launchAtLogin
	}
}

func (a *Application) newService() (desktop.DaemonService, error) {
	a.stateMu.RLock()
	config := a.config
	token := a.token
	hasConfig := a.hasConfig
	a.stateMu.RUnlock()
	if !hasConfig || token == "" {
		return nil, &syncer.ReconnectRequiredError{Err: errDesktopNotConfigured}
	}
	syncerConfig, err := desktop.SyncerConfig(a.dirs, a.backendOrigin, config.WorkspaceID, token, a.version)
	if err != nil {
		return nil, err
	}
	return syncer.New(syncerConfig)
}

// Start starts the embedded daemon controller and menu action loop.
func (a *Application) Start(parent context.Context) error {
	a.start.Do(func() {
		if parent == nil {
			parent = context.Background()
		}
		a.lifecycleMu.Lock()
		defer a.lifecycleMu.Unlock()
		a.ctx, a.cancel = context.WithCancel(parent)
		if err := a.controller.Start(a.ctx); err != nil {
			a.startErr = err
			a.cancel()
			return
		}
		a.loopStarted = true
		go a.run()
	})
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	return a.startErr
}

// Context is canceled when the application begins shutdown.
func (a *Application) Context() context.Context {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	return a.ctx
}

// Menu returns the current typed tray model.
func (a *Application) Menu() desktop.MenuModel {
	a.stateMu.RLock()
	options := desktop.MenuOptions{
		Snapshot:       a.controller.Snapshot(),
		Configured:     a.hasConfig,
		Connecting:     a.connecting,
		WorkspaceName:  a.config.WorkspaceName,
		LaunchAtLogin:  a.launchAtLogin,
		DesktopVersion: a.version,
	}
	a.stateMu.RUnlock()
	return desktop.BuildMenu(options)
}

// Updates carries coalesced menu snapshots for the native renderer.
func (a *Application) Updates() <-chan desktop.MenuModel {
	return a.updates
}

// Actions accepts typed actions from the native renderer.
func (a *Application) Actions() chan<- desktop.MenuAction {
	return a.actions
}

func (a *Application) run() {
	defer close(a.loopDone)
	defer close(a.updates)
	a.publishMenu()
	for {
		select {
		case <-a.ctx.Done():
			return
		case snapshot, ok := <-a.controller.Updates():
			if !ok {
				return
			}
			a.logger.Printf("service generation=%d state=%s sequence=%d", snapshot.Generation, snapshot.State, snapshot.Sequence)
			a.publishMenu()
		case <-a.wake:
			a.publishMenu()
		case action := <-a.actions:
			a.handleAction(action)
		}
	}
}

func (a *Application) handleAction(action desktop.MenuAction) {
	switch action {
	case desktop.MenuActionOpenCodesk:
		a.stateMu.RLock()
		target := a.config.WorkspaceURL
		a.stateMu.RUnlock()
		if target != "" {
			a.report("open Codesk", a.opener.Open(target))
		}
	case desktop.MenuActionConnect:
		a.beginConnect()
	case desktop.MenuActionRestart:
		a.report("restart daemon", a.controller.Restart())
	case desktop.MenuActionToggleLaunchAtLogin:
		a.toggleLaunchAtLogin()
	case desktop.MenuActionOpenLogs:
		if err := os.MkdirAll(a.dirs.Logs, 0o700); err != nil {
			a.report("create logs directory", err)
			return
		}
		a.report("open logs", a.opener.Open(a.dirs.Logs))
	case desktop.MenuActionQuit:
		go a.Shutdown()
	}
}

func (a *Application) beginConnect() {
	a.stateMu.Lock()
	if a.connecting {
		a.stateMu.Unlock()
		return
	}
	a.connecting = true
	priorToken := append([]byte(nil), a.token...)
	a.stateMu.Unlock()
	a.signalMenu()

	a.connects.Add(1)
	go func() {
		defer a.connects.Done()
		defer clear(priorToken)
		defer func() {
			a.stateMu.Lock()
			a.connecting = false
			a.stateMu.Unlock()
			a.signalMenu()
		}()

		payload, err := desktop.Connect(a.ctx, a.codeskOrigin, a.secrets, a.opener)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				a.report("connect", err)
			}
			return
		}
		config, err := desktop.CommitConnectionMetadata(a.configStore, a.secrets, priorToken, payload)
		if err != nil {
			a.report("commit connection", err)
			return
		}

		a.stateMu.Lock()
		a.config = config
		a.hasConfig = true
		a.token = payload.Token()
		a.stateMu.Unlock()
		if err := a.controller.Restart(); err != nil && !errors.Is(err, desktop.ErrControllerStopped) {
			a.report("start connected daemon", err)
		}
	}()
}

func (a *Application) toggleLaunchAtLogin() {
	a.stateMu.RLock()
	enabled := a.launchAtLogin
	a.stateMu.RUnlock()
	var err error
	if enabled {
		err = a.loginItem.Disable()
	} else {
		err = a.loginItem.Enable()
	}
	if err != nil {
		a.report("update login item", err)
		return
	}
	actual, err := a.loginItem.IsEnabled()
	if err != nil {
		a.report("verify login item", err)
		return
	}
	a.stateMu.Lock()
	a.launchAtLogin = actual
	a.stateMu.Unlock()
	a.signalMenu()
}

func (a *Application) publishMenu() {
	model := a.Menu()
	select {
	case a.updates <- model:
	default:
		select {
		case <-a.updates:
		default:
		}
		select {
		case a.updates <- model:
		default:
		}
	}
}

func (a *Application) signalMenu() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Application) report(operation string, err error) {
	if err != nil {
		a.logger.Printf("%s: %v", operation, err)
	}
}

// Shutdown cancels the controller, waits for every connection handoff, and
// joins the application loop. It is safe to call more than once.
func (a *Application) Shutdown() {
	a.shutdown.Do(func() {
		a.lifecycleMu.Lock()
		cancel := a.cancel
		loopStarted := a.loopStarted
		if cancel != nil {
			cancel()
		}
		quitErr := a.controller.Quit()
		a.lifecycleMu.Unlock()
		if quitErr != nil && !errors.Is(quitErr, desktop.ErrControllerStopped) {
			a.report("quit controller", quitErr)
		}
		if loopStarted {
			<-a.loopDone
		}
		a.connects.Wait()
	})
}
