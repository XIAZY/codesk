package desktopapp

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"notty/daemon/internal/desktop"
	"notty/daemon/internal/desktopstate"
)

const applicationTestTimeout = 3 * time.Second

type testConfigStore struct {
	mu      sync.Mutex
	config  desktopstate.Configuration
	exists  bool
	saved   chan desktopstate.Configuration
	deleted chan struct{}
}

func newTestConfigStore(config desktopstate.Configuration, exists bool) *testConfigStore {
	return &testConfigStore{
		config:  config,
		exists:  exists,
		saved:   make(chan desktopstate.Configuration, 8),
		deleted: make(chan struct{}, 8),
	}
}

func (s *testConfigStore) Load() (desktopstate.Configuration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exists {
		return desktopstate.Configuration{}, os.ErrNotExist
	}
	return s.config, nil
}

func (s *testConfigStore) Save(config desktopstate.Configuration) error {
	if err := config.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.config = config
	s.exists = true
	s.mu.Unlock()
	s.saved <- config
	return nil
}

func (s *testConfigStore) Delete() error {
	s.mu.Lock()
	s.config = desktopstate.Configuration{}
	s.exists = false
	s.mu.Unlock()
	s.deleted <- struct{}{}
	return nil
}

func (s *testConfigStore) snapshot() (desktopstate.Configuration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config, s.exists
}

type testSecretStore struct {
	mu         sync.Mutex
	data       map[string][]byte
	lastLoaded []byte
}

func newTestSecretStore(token string) *testSecretStore {
	store := &testSecretStore{data: make(map[string][]byte)}
	if token != "" {
		store.data[desktopstate.SecretKeyDaemonToken] = []byte(token)
	}
	return store
}

func (s *testSecretStore) Save(key string, secret []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = append([]byte(nil), secret...)
	return nil
}

func (s *testSecretStore) Load(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secret, ok := s.data[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	loaded := append([]byte(nil), secret...)
	s.lastLoaded = loaded
	return loaded, nil
}

func (s *testSecretStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *testSecretStore) token() string {
	secret, _ := s.Load(desktopstate.SecretKeyDaemonToken)
	return string(secret)
}

func (s *testSecretStore) loadedBufferCleared() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastLoaded) == 0 {
		return false
	}
	for _, value := range s.lastLoaded {
		if value != 0 {
			return false
		}
	}
	return true
}

type testLoginItem struct {
	mu              sync.Mutex
	enabled         bool
	verification    *bool
	verificationErr error
	enableCalls     chan struct{}
	disableCalls    chan struct{}
	isEnabledCalls  chan struct{}
}

func newTestLoginItem(enabled bool) *testLoginItem {
	return &testLoginItem{
		enabled:        enabled,
		enableCalls:    make(chan struct{}, 8),
		disableCalls:   make(chan struct{}, 8),
		isEnabledCalls: make(chan struct{}, 8),
	}
}

func (i *testLoginItem) Enable() error {
	i.mu.Lock()
	i.enabled = true
	i.mu.Unlock()
	i.enableCalls <- struct{}{}
	return nil
}

func (i *testLoginItem) Disable() error {
	i.mu.Lock()
	i.enabled = false
	i.mu.Unlock()
	i.disableCalls <- struct{}{}
	return nil
}

func (i *testLoginItem) IsEnabled() (bool, error) {
	i.mu.Lock()
	enabled := i.enabled
	if i.verification != nil {
		enabled = *i.verification
	}
	err := i.verificationErr
	i.mu.Unlock()
	i.isEnabledCalls <- struct{}{}
	return enabled, err
}

func (i *testLoginItem) setVerification(enabled bool, err error) {
	i.mu.Lock()
	i.verification = &enabled
	i.verificationErr = err
	i.mu.Unlock()
}

type testOpener struct {
	mu     sync.Mutex
	urls   []string
	opened chan string
	gate   <-chan struct{}
}

func newTestOpener() *testOpener {
	return &testOpener{opened: make(chan string, 16)}
}

func (o *testOpener) Open(target string) error {
	o.mu.Lock()
	o.urls = append(o.urls, target)
	o.mu.Unlock()
	o.opened <- target
	if o.gate != nil {
		<-o.gate
	}
	return nil
}

type testService struct {
	ready          chan struct{}
	started        chan struct{}
	stopped        chan struct{}
	cancelObserved chan struct{}
	runRelease     <-chan struct{}
}

func newTestService() *testService {
	ready := make(chan struct{})
	close(ready)
	return &testService{
		ready:   ready,
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (s *testService) Ready() <-chan struct{} {
	return s.ready
}

func (s *testService) Run(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	if s.cancelObserved != nil {
		close(s.cancelObserved)
	}
	if s.runRelease != nil {
		<-s.runRelease
	}
	close(s.stopped)
	return ctx.Err()
}

type testServiceFactory struct {
	created        chan *testService
	ready          chan struct{}
	cancelObserved chan struct{}
	runRelease     <-chan struct{}
}

func newTestServiceFactory() *testServiceFactory {
	return &testServiceFactory{created: make(chan *testService, 16)}
}

func (f *testServiceFactory) New() (desktop.DaemonService, error) {
	service := newTestService()
	if f.ready != nil {
		service.ready = f.ready
	}
	service.cancelObserved = f.cancelObserved
	service.runRelease = f.runRelease
	f.created <- service
	return service, nil
}

func TestApplicationPublishesInitialMenu(t *testing.T) {
	app, factory, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
	startTestApplication(t, app)
	waitForStartedService(t, factory)

	model := waitForMenu(t, app.Updates(), func(model desktop.MenuModel) bool {
		connect, err := model.Item(desktop.MenuItemConnect)
		return err == nil && connect.Enabled
	})
	if err := model.Validate(); err != nil {
		t.Fatalf("initial menu is invalid: %v", err)
	}
	openCodesk, err := model.Item(desktop.MenuItemOpenCodesk)
	if err != nil {
		t.Fatal(err)
	}
	if openCodesk.Enabled {
		t.Fatal("Open Codesk must be disabled before configuration")
	}
	version, err := model.Item(desktop.MenuItemVersion)
	if err != nil {
		t.Fatal(err)
	}
	if version.Title != "Version test-version" {
		t.Fatalf("version title = %q, want %q", version.Title, "Version test-version")
	}

	app.Shutdown()
}

func TestApplicationRoutesOpenRestartLogsAndQuitActions(t *testing.T) {
	opener := newTestOpener()
	app, factory, _, _, _, dirs := newTestApplication(t, true, "old-token", opener)
	startTestApplication(t, app)
	first := waitForStartedService(t, factory)
	waitForControllerState(t, app, desktop.StateOnline)

	app.Actions() <- desktop.MenuActionOpenCodesk
	if got := waitForOpened(t, opener); got != testConfiguration().WorkspaceURL {
		t.Fatalf("opened target = %q, want workspace URL %q", got, testConfiguration().WorkspaceURL)
	}

	app.Actions() <- desktop.MenuActionOpenLogs
	if got := waitForOpened(t, opener); got != dirs.Logs {
		t.Fatalf("opened target = %q, want logs directory %q", got, dirs.Logs)
	}
	info, err := os.Stat(dirs.Logs)
	if err != nil {
		t.Fatalf("stat logs directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("logs path %q is not a directory", dirs.Logs)
	}

	app.Actions() <- desktop.MenuActionRestart
	second := waitForStartedService(t, factory)
	waitForSignal(t, first.stopped, "first service stop")

	app.Actions() <- desktop.MenuActionQuit
	waitForSignal(t, app.Context().Done(), "application context cancellation")
	waitForSignal(t, second.stopped, "second service stop")
	waitForUpdatesClosed(t, app.Updates())
	app.Shutdown()
}

func TestApplicationLogsDistinctOnlineServiceGenerationsAfterRestart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "desktop.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	app, factory, _, _, _, _ := newTestApplication(t, true, "old-token", newTestOpener())
	app.logger = log.New(logFile, "codesk-desktop: ", 0)
	startTestApplication(t, app)
	waitForStartedService(t, factory)
	waitForControllerState(t, app, desktop.StateOnline)
	waitForLogText(t, logPath, "service generation=1 state=online")

	app.Actions() <- desktop.MenuActionRestart
	waitForStartedService(t, factory)
	waitForControllerState(t, app, desktop.StateOnline)
	waitForLogText(t, logPath, "service generation=2 state=online")

	app.Shutdown()
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationClearsLoadedSecretBuffer(t *testing.T) {
	app, _, _, secrets, _, _ := newTestApplication(t, true, "loaded-token", newTestOpener())
	if app.token != "loaded-token" {
		t.Fatalf("loaded token = %q, want loaded-token", app.token)
	}
	if !secrets.loadedBufferCleared() {
		t.Fatal("loadDurableState retained the caller-owned secret buffer")
	}
}

func TestApplicationConnectCommitsConfigurationAndRestarts(t *testing.T) {
	opener := newTestOpener()
	app, factory, configs, secrets, _, _ := newTestApplication(t, true, "old-token", opener)
	startTestApplication(t, app)
	first := waitForStartedService(t, factory)
	waitForControllerState(t, app, desktop.StateOnline)

	app.Actions() <- desktop.MenuActionConnect
	connectURL := waitForOpened(t, opener)
	callbackURL := callbackFromConnectURL(t, connectURL)
	response := postConnectionPayload(t, callbackURL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect response = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}

	var saved desktopstate.Configuration
	select {
	case saved = <-configs.saved:
	case <-time.After(applicationTestTimeout):
		t.Fatal("timed out waiting for connection metadata commit")
	}
	second := waitForStartedService(t, factory)
	waitForSignal(t, first.stopped, "pre-connect service stop")
	waitForControllerState(t, app, desktop.StateOnline)

	if saved.WorkspaceID != "workspace-new" || saved.WorkspaceName != "Workspace New" {
		t.Fatalf("saved configuration = %#v", saved)
	}
	stored, exists := configs.snapshot()
	if !exists || stored != saved {
		t.Fatalf("stored configuration = %#v, exists=%v; want %#v", stored, exists, saved)
	}
	if got := secrets.token(); got != "nottyd_new_secret_token" {
		t.Fatalf("stored token = %q, want new token", got)
	}
	status, err := app.Menu().Item(desktop.MenuItemStatus)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Title, "Workspace New") {
		t.Fatalf("status title = %q, want new workspace name", status.Title)
	}

	app.Actions() <- desktop.MenuActionOpenCodesk
	if got := waitForOpened(t, opener); got != "https://app.getcodesk.com/w/workspace-new" {
		t.Fatalf("opened target = %q, want new workspace URL", got)
	}

	app.Shutdown()
	waitForSignal(t, second.stopped, "post-connect service stop")
}

func TestApplicationConnectIsSingleFlightAndPublishesState(t *testing.T) {
	releaseOpen := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseOpen)
		}
	}()
	opener := newTestOpener()
	opener.gate = releaseOpen
	app, factory, configs, _, _, _ := newTestApplication(t, false, "", opener)
	startTestApplication(t, app)
	waitForStartedService(t, factory)
	drainMenus(app.Updates())

	app.Actions() <- desktop.MenuActionConnect
	connectURL := waitForOpened(t, opener)
	model := waitForMenu(t, app.Updates(), func(model desktop.MenuModel) bool {
		connect, err := model.Item(desktop.MenuItemConnect)
		return err == nil && connect.Title == "Connecting..."
	})
	connect, _ := model.Item(desktop.MenuItemConnect)
	if connect.Enabled {
		t.Fatal("Connect remained enabled while a handoff was active")
	}

	app.Actions() <- desktop.MenuActionConnect
	select {
	case second := <-opener.opened:
		t.Fatalf("second Connect launched concurrent handoff %q", second)
	case <-time.After(75 * time.Millisecond):
	}

	response := postConnectionPayload(t, callbackFromConnectURL(t, connectURL))
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect response = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}
	drainMenus(app.Updates())
	close(releaseOpen)
	released = true
	select {
	case <-configs.saved:
	case <-time.After(applicationTestTimeout):
		t.Fatal("timed out waiting for connection metadata commit")
	}

	model = waitForMenu(t, app.Updates(), func(model desktop.MenuModel) bool {
		connect, err := model.Item(desktop.MenuItemConnect)
		return err == nil && connect.Title != "Connecting..."
	})
	connect, _ = model.Item(desktop.MenuItemConnect)
	if connect.Title == "Connecting..." {
		t.Fatal("connecting state did not clear after handoff completion")
	}
}

func TestApplicationToggleLoginItemPublishesVerifiedState(t *testing.T) {
	app, factory, _, _, loginItem, _ := newTestApplication(t, true, "old-token", newTestOpener())
	startTestApplication(t, app)
	waitForStartedService(t, factory)
	waitForControllerState(t, app, desktop.StateOnline)
	drainMenus(app.Updates())

	app.Actions() <- desktop.MenuActionToggleLaunchAtLogin
	waitForSignal(t, loginItem.enableCalls, "login item enable")
	model := waitForMenu(t, app.Updates(), launchAtLoginChecked)
	item, _ := model.Item(desktop.MenuItemLaunchAtLogin)
	if !item.Checked {
		t.Fatal("launch-at-login menu item was not checked after enabling")
	}

	drainMenus(app.Updates())
	app.Actions() <- desktop.MenuActionToggleLaunchAtLogin
	waitForSignal(t, loginItem.disableCalls, "login item disable")
	model = waitForMenu(t, app.Updates(), func(model desktop.MenuModel) bool {
		item, err := model.Item(desktop.MenuItemLaunchAtLogin)
		return err == nil && !item.Checked
	})
	item, _ = model.Item(desktop.MenuItemLaunchAtLogin)
	if item.Checked {
		t.Fatal("launch-at-login menu item remained checked after disabling")
	}

	app.Shutdown()
}

func TestApplicationToggleLoginItemRetainsLastVerifiedState(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "native registration remains inactive"},
		{name: "native verification fails", err: errors.New("native verification failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, factory, _, _, loginItem, _ := newTestApplication(t, true, "old-token", newTestOpener())
			startTestApplication(t, app)
			waitForStartedService(t, factory)
			waitForControllerState(t, app, desktop.StateOnline)
			drainMenus(app.Updates())
			drainSignals(loginItem.isEnabledCalls)
			loginItem.setVerification(false, test.err)

			app.Actions() <- desktop.MenuActionToggleLaunchAtLogin
			waitForSignal(t, loginItem.enableCalls, "login item enable")
			waitForSignal(t, loginItem.isEnabledCalls, "native login item verification")

			item, err := app.Menu().Item(desktop.MenuItemLaunchAtLogin)
			if err != nil {
				t.Fatal(err)
			}
			if item.Checked {
				t.Fatal("launch-at-login state changed without verified native registration")
			}
		})
	}
}

func TestApplicationMenuUpdatesCoalesceToLatestSnapshot(t *testing.T) {
	app, _, _, _, _, _ := newTestApplication(t, true, "old-token", newTestOpener())
	for len(app.updates) < cap(app.updates) {
		app.publishMenu()
	}

	app.stateMu.Lock()
	app.launchAtLogin = true
	app.stateMu.Unlock()
	app.publishMenu()

	model := waitForMenu(t, app.Updates(), launchAtLoginChecked)
	item, _ := model.Item(desktop.MenuItemLaunchAtLogin)
	if !item.Checked {
		t.Fatal("coalesced menu did not retain the latest snapshot")
	}
}

func TestApplicationShutdownJoinsBlockedConnection(t *testing.T) {
	releaseOpen := make(chan struct{})
	opener := newTestOpener()
	opener.gate = releaseOpen
	app, factory, _, _, _, _ := newTestApplication(t, false, "", opener)
	startTestApplication(t, app)
	service := waitForStartedService(t, factory)
	_ = waitForMenu(t, app.Updates(), func(model desktop.MenuModel) bool {
		status, err := model.Item(desktop.MenuItemStatus)
		return err == nil && status.Title == "Codesk - Connected"
	})
	drainMenus(app.Updates())
	released := false
	defer func() {
		if !released {
			close(releaseOpen)
		}
	}()

	app.Actions() <- desktop.MenuActionConnect
	_ = waitForOpened(t, opener)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		app.Shutdown()
	}()
	waitForSignal(t, service.stopped, "service stop during shutdown")
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before the connection handoff joined")
	case <-time.After(75 * time.Millisecond):
	}

	close(releaseOpen)
	released = true
	waitForSignal(t, shutdownDone, "joined shutdown")
}

func TestApplicationShutdownJoinsSelectedConnectionBeforeWaiting(t *testing.T) {
	releaseOpen := make(chan struct{})
	opener := newTestOpener()
	opener.gate = releaseOpen
	app, factory, _, _, _, _ := newTestApplication(t, false, "", opener)
	app.actions = make(chan desktop.MenuAction)
	serviceReady := make(chan struct{})
	factory.ready = serviceReady
	startTestApplication(t, app)
	service := waitForStartedService(t, factory)
	starting := func(model desktop.MenuModel) bool {
		status, err := model.Item(desktop.MenuItemStatus)
		return err == nil && status.Title == "Codesk - Starting"
	}
	_ = waitForMenu(t, app.Updates(), starting)
	_ = waitForMenu(t, app.Updates(), starting)
	close(serviceReady)
	_ = waitForMenu(t, app.Updates(), func(model desktop.MenuModel) bool {
		status, err := model.Item(desktop.MenuItemStatus)
		return err == nil && status.Title == "Codesk - Connected"
	})
	drainMenus(app.Updates())
	released := false
	defer func() {
		if !released {
			close(releaseOpen)
		}
	}()

	app.stateMu.Lock()
	locked := true
	defer func() {
		if locked {
			app.stateMu.Unlock()
		}
	}()
	actionSelected := make(chan struct{})
	go func() {
		app.Actions() <- desktop.MenuActionConnect
		close(actionSelected)
	}()
	waitForSignal(t, actionSelected, "selected Connect action")

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		app.Shutdown()
	}()
	waitForSignal(t, service.stopped, "service stop during selected Connect")
	app.stateMu.Unlock()
	locked = false
	_ = waitForOpened(t, opener)

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before the selected connection joined")
	case <-time.After(75 * time.Millisecond):
	}

	close(releaseOpen)
	released = true
	waitForSignal(t, shutdownDone, "selected connection shutdown join")
}

func TestApplicationShutdownJoinsLoopBeforeWaitingForConnections(t *testing.T) {
	app, _, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
	app.lifecycleMu.Lock()
	app.ctx, app.cancel = context.WithCancel(context.Background())
	app.loopStarted = true
	app.lifecycleMu.Unlock()

	app.connects.Add(1)
	connectionReleased := false
	defer func() {
		if !connectionReleased {
			app.connects.Done()
		}
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		app.Shutdown()
	}()

	loopJoinObserved := make(chan struct{})
	go func() {
		app.loopDone <- struct{}{}
		close(loopJoinObserved)
	}()
	select {
	case <-loopJoinObserved:
	case <-time.After(applicationTestTimeout):
		app.connects.Done()
		connectionReleased = true
		waitForSignal(t, loopJoinObserved, "loop join after connection release")
		waitForSignal(t, shutdownDone, "shutdown after connection release")
		t.Fatal("Shutdown waited for connections before joining the application loop")
	}

	app.connects.Done()
	connectionReleased = true
	waitForSignal(t, shutdownDone, "ordered shutdown join")
}

func TestApplicationStartWithCanceledParentJoinsMenuLoop(t *testing.T) {
	type startOutcome struct {
		err        error
		panicValue any
	}
	app, _, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	app.stateMu.Lock()
	stateLocked := true
	defer func() {
		if stateLocked {
			app.stateMu.Unlock()
		}
	}()
	startDone := make(chan startOutcome, 1)
	go func() {
		outcome := startOutcome{}
		defer func() {
			outcome.panicValue = recover()
			startDone <- outcome
		}()
		outcome.err = app.Start(parent)
	}()

	select {
	case outcome := <-startDone:
		app.stateMu.Unlock()
		stateLocked = false
		if outcome.panicValue != nil {
			t.Fatalf("Start() panic = %v", outcome.panicValue)
		}
		if outcome.err != nil {
			t.Fatalf("Start() error = %v", outcome.err)
		}
	case _, ok := <-app.Updates():
		app.stateMu.Unlock()
		stateLocked = false
		outcome := <-startDone
		app.Shutdown()
		if ok {
			t.Fatal("menu update bypassed the held application state lock")
		}
		t.Fatalf("menu loop closed before Start returned; recovered panic = %v", outcome.panicValue)
	case <-time.After(applicationTestTimeout):
		app.stateMu.Unlock()
		stateLocked = false
		app.Shutdown()
		t.Fatal("Start neither returned nor delegated publication to the menu loop")
	}

	app.Shutdown()
	waitForUpdatesClosed(t, app.Updates())
}

func TestApplicationShutdownKeepsLifecycleLockedThroughControllerQuit(t *testing.T) {
	runRelease := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(runRelease)
		}
	}()
	cancelObserved := make(chan struct{})
	app, factory, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
	factory.cancelObserved = cancelObserved
	factory.runRelease = runRelease
	startTestApplication(t, app)
	waitForStartedService(t, factory)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		app.Shutdown()
	}()
	waitForSignal(t, cancelObserved, "service cancellation")

	contextRead := make(chan context.Context, 1)
	go func() {
		contextRead <- app.Context()
	}()
	select {
	case <-contextRead:
		close(runRelease)
		released = true
		waitForSignal(t, shutdownDone, "shutdown after premature Context read")
		t.Fatal("Context returned before controller Quit completed")
	case <-time.After(75 * time.Millisecond):
	}

	close(runRelease)
	released = true
	waitForSignal(t, shutdownDone, "shutdown after service release")
	select {
	case ctx := <-contextRead:
		if ctx == nil || ctx.Err() == nil {
			t.Fatal("Context returned an uncanceled lifecycle after shutdown")
		}
	case <-time.After(applicationTestTimeout):
		t.Fatal("Context remained blocked after controller Quit completed")
	}
}

func TestApplicationStartAfterShutdownReturnsStableError(t *testing.T) {
	app, _, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
	app.Shutdown()

	for range 2 {
		if err := app.Start(context.Background()); !errors.Is(err, desktop.ErrControllerStopped) {
			t.Fatalf("Start() error = %v, want %v", err, desktop.ErrControllerStopped)
		}
	}
	if ctx := app.Context(); ctx == nil || ctx.Err() == nil {
		t.Fatal("failed Start did not retain its canceled application context")
	}
}

func TestApplicationShutdownAfterFailedStartDoesNotJoinMissingLoop(t *testing.T) {
	app, _, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
	if err := app.controller.Quit(); err != nil {
		t.Fatalf("controller Quit() error = %v", err)
	}
	if err := app.Start(context.Background()); !errors.Is(err, desktop.ErrControllerStopped) {
		t.Fatalf("Start() error = %v, want %v", err, desktop.ErrControllerStopped)
	}
	if ctx := app.Context(); ctx == nil || ctx.Err() == nil {
		t.Fatal("failed Start did not retain its canceled application context")
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		app.Shutdown()
	}()
	waitForSignal(t, shutdownDone, "shutdown after failed Start")
}

func TestApplicationConcurrentStartAndShutdownPublishLifecycleAtomically(t *testing.T) {
	for range 100 {
		app, _, _, _, _, _ := newTestApplication(t, false, "", newTestOpener())
		barrier := make(chan struct{})
		startResult := make(chan error, 1)
		shutdownDone := make(chan struct{})
		go func() {
			<-barrier
			startResult <- app.Start(context.Background())
		}()
		go func() {
			defer close(shutdownDone)
			<-barrier
			app.Shutdown()
		}()
		close(barrier)

		firstErr := <-startResult
		waitForSignal(t, shutdownDone, "concurrent shutdown")
		if firstErr != nil && !errors.Is(firstErr, desktop.ErrControllerStopped) {
			t.Fatalf("concurrent Start() error = %v", firstErr)
		}
		repeatedErr := app.Start(context.Background())
		if (firstErr == nil) != (repeatedErr == nil) ||
			(firstErr != nil && !errors.Is(repeatedErr, desktop.ErrControllerStopped)) {
			t.Fatalf("repeated Start() error = %v after first error %v", repeatedErr, firstErr)
		}
		if ctx := app.Context(); ctx == nil || ctx.Err() == nil {
			t.Fatal("concurrent lifecycle left a live or unpublished application context")
		}
	}
}

func newTestApplication(
	t *testing.T,
	configured bool,
	token string,
	opener *testOpener,
) (*Application, *testServiceFactory, *testConfigStore, *testSecretStore, *testLoginItem, desktop.Dirs) {
	t.Helper()
	root := t.TempDir()
	dirs := desktop.Dirs{
		Data:  filepath.Join(root, "data"),
		Logs:  filepath.Join(root, "logs"),
		Cache: filepath.Join(root, "cache"),
	}
	configs := newTestConfigStore(testConfiguration(), configured)
	secrets := newTestSecretStore(token)
	loginItem := newTestLoginItem(false)
	factory := newTestServiceFactory()
	app, err := newApplication(Options{
		Dirs:          dirs,
		CodeskOrigin:  "https://app.getcodesk.com",
		BackendOrigin: "https://api.getcodesk.com",
		Version:       "test-version",
		ConfigStore:   configs,
		Secrets:       secrets,
		LoginItem:     loginItem,
		Opener:        opener,
		Logger:        log.New(io.Discard, "", 0),
	}, factory.New)
	if err != nil {
		t.Fatalf("newApplication() error = %v", err)
	}
	return app, factory, configs, secrets, loginItem, dirs
}

func testConfiguration() desktopstate.Configuration {
	return desktopstate.Configuration{
		DaemonID:      "daemon-old",
		WorkspaceID:   "workspace-old",
		WorkspaceName: "Workspace Old",
		WorkspaceSlug: "workspace-old",
		WorkspaceURL:  "https://app.getcodesk.com/w/workspace-old",
	}
}

func startTestApplication(t *testing.T, app *Application) {
	t.Helper()
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(app.Shutdown)
}

func waitForStartedService(t *testing.T, factory *testServiceFactory) *testService {
	t.Helper()
	var service *testService
	select {
	case service = <-factory.created:
	case <-time.After(applicationTestTimeout):
		t.Fatal("timed out waiting for service construction")
	}
	waitForSignal(t, service.started, "service start")
	return service
}

func waitForControllerState(t *testing.T, app *Application, state desktop.State) {
	t.Helper()
	deadline := time.Now().Add(applicationTestTimeout)
	for time.Now().Before(deadline) {
		if app.controller.Snapshot().State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("controller state = %s, want %s", app.controller.Snapshot().State, state)
}

func waitForOpened(t *testing.T, opener *testOpener) string {
	t.Helper()
	select {
	case target := <-opener.opened:
		return target
	case <-time.After(applicationTestTimeout):
		t.Fatal("timed out waiting for opened target")
		return ""
	}
}

func waitForLogText(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(applicationTestTimeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("log never contained %q:\n%s", want, data)
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(applicationTestTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForMenu(t *testing.T, updates <-chan desktop.MenuModel, accept func(desktop.MenuModel) bool) desktop.MenuModel {
	t.Helper()
	timer := time.NewTimer(applicationTestTimeout)
	defer timer.Stop()
	for {
		select {
		case model, ok := <-updates:
			if !ok {
				t.Fatal("menu updates closed before expected model")
			}
			if accept(model) {
				return model
			}
		case <-timer.C:
			t.Fatal("timed out waiting for expected menu model")
		}
	}
}

func drainMenus(updates <-chan desktop.MenuModel) {
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func drainSignals(signals <-chan struct{}) {
	for {
		select {
		case <-signals:
		default:
			return
		}
	}
}

func waitForUpdatesClosed(t *testing.T, updates <-chan desktop.MenuModel) {
	t.Helper()
	timer := time.NewTimer(applicationTestTimeout)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for menu updates to close")
		}
	}
}

func launchAtLoginChecked(model desktop.MenuModel) bool {
	item, err := model.Item(desktop.MenuItemLaunchAtLogin)
	return err == nil && item.Checked
}

func callbackFromConnectURL(t *testing.T, connectURL string) string {
	t.Helper()
	parsed, err := url.Parse(connectURL)
	if err != nil {
		t.Fatalf("parse connect URL: %v", err)
	}
	callback := parsed.Query().Get("callback")
	if callback == "" {
		t.Fatal("connect URL has no callback")
	}
	return callback
}

var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func postConnectionPayload(t *testing.T, callbackURL string) *http.Response {
	t.Helper()
	fields := url.Values{
		"daemon_id":      {"daemon-new"},
		"token":          {"nottyd_new_secret_token"},
		"workspace_id":   {"workspace-new"},
		"workspace_name": {"Workspace New"},
		"workspace_slug": {"workspace-new"},
		"workspace_url":  {"https://app.getcodesk.com/w/workspace-new"},
	}
	response, err := noRedirectClient.Post(callbackURL, "application/x-www-form-urlencoded", strings.NewReader(fields.Encode()))
	if err != nil {
		t.Fatalf("POST connection payload: %v", err)
	}
	return response
}

func TestNewRequiresPlatformAdapters(t *testing.T) {
	root := t.TempDir()
	_, err := New(Options{
		Dirs: desktop.Dirs{
			Data:  filepath.Join(root, "data"),
			Logs:  filepath.Join(root, "logs"),
			Cache: filepath.Join(root, "cache"),
		},
	})
	if err == nil {
		t.Fatal("New() accepted missing platform adapters")
	}
	if !strings.Contains(err.Error(), "platform adapters") {
		t.Fatalf("New() error = %v, want platform adapter error", err)
	}
}
