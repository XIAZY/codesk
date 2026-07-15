package desktop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"notty/daemon/internal/syncer"
)

const controllerTestTimeout = 2 * time.Second

type activityCounter struct {
	mu        sync.Mutex
	active    int
	maxActive int
}

func (c *activityCounter) enter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
}

func (c *activityCounter) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
}

func (c *activityCounter) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active, c.maxActive
}

type fakeService struct {
	ready      chan struct{}
	exit       chan error
	started    chan struct{}
	canceled   chan struct{}
	joined     chan struct{}
	runContext chan context.Context
	cancelGate <-chan struct{}
	activity   *activityCounter
	readyOnce  sync.Once
}

func newFakeService(activity *activityCounter) *fakeService {
	return &fakeService{
		ready:      make(chan struct{}),
		exit:       make(chan error, 1),
		started:    make(chan struct{}),
		canceled:   make(chan struct{}),
		joined:     make(chan struct{}),
		runContext: make(chan context.Context, 1),
		activity:   activity,
	}
}

func (s *fakeService) Ready() <-chan struct{} {
	return s.ready
}

func (s *fakeService) markReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

func (s *fakeService) fail(err error) {
	s.exit <- err
}

func (s *fakeService) Run(ctx context.Context) error {
	s.activity.enter()
	s.runContext <- ctx
	close(s.started)
	defer func() {
		s.activity.leave()
		close(s.joined)
	}()

	select {
	case err := <-s.exit:
		return err
	case <-ctx.Done():
		close(s.canceled)
		if s.cancelGate != nil {
			<-s.cancelGate
		}
		return ctx.Err()
	}
}

func waitRunContext(t *testing.T, service *fakeService) context.Context {
	t.Helper()
	select {
	case ctx := <-service.runContext:
		return ctx
	case <-time.After(controllerTestTimeout):
		t.Fatal("timed out waiting for service run context")
		return nil
	}
}

type nilReadyService struct {
	started chan struct{}
}

func (s *nilReadyService) Ready() <-chan struct{} { return nil }

func (s *nilReadyService) Run(context.Context) error {
	close(s.started)
	return nil
}

type factoryResult struct {
	service DaemonService
	err     error
}

type fakeFactory struct {
	mu       sync.Mutex
	results  []factoryResult
	fallback func() factoryResult
	calls    int
	created  chan int
}

func newFakeFactory(results ...factoryResult) *fakeFactory {
	return &fakeFactory{
		results: results,
		created: make(chan int, 128),
	}
}

func (f *fakeFactory) newService() (DaemonService, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	var result factoryResult
	switch {
	case len(f.results) > 0:
		result = f.results[0]
		f.results = f.results[1:]
	case f.fallback != nil:
		result = f.fallback()
	default:
		result.err = errors.New("fake factory exhausted")
	}
	f.mu.Unlock()

	f.created <- call
	return result.service, result.err
}

func (f *fakeFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeClock struct {
	created chan *fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{created: make(chan *fakeTimer, 128)}
}

func (c *fakeClock) NewTimer(delay time.Duration) Timer {
	timer := &fakeTimer{delay: delay, tick: make(chan time.Time, 1)}
	c.created <- timer
	return timer
}

type fakeTimer struct {
	mu      sync.Mutex
	delay   time.Duration
	tick    chan time.Time
	stopped bool
	fired   bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.tick }

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (t *fakeTimer) fire() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.fired = true
	t.tick <- time.Unix(1, 0)
	return true
}

func (t *fakeTimer) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

func testController(t *testing.T, factory *fakeFactory, clock *fakeClock) *Controller {
	t.Helper()
	controller, err := NewController(factory.newService, ControllerOptions{
		Clock:             clock,
		InitialRetryDelay: time.Second,
		MaximumRetryDelay: 4 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}
	return controller
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(controllerTestTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitFactoryCall(t *testing.T, factory *fakeFactory, want int) {
	t.Helper()
	select {
	case got := <-factory.created:
		if got != want {
			t.Fatalf("factory call = %d, want %d", got, want)
		}
	case <-time.After(controllerTestTimeout):
		t.Fatalf("timed out waiting for factory call %d", want)
	}
}

func waitTimer(t *testing.T, clock *fakeClock, wantDelay time.Duration) *fakeTimer {
	t.Helper()
	select {
	case timer := <-clock.created:
		if timer.delay != wantDelay {
			t.Fatalf("timer delay = %s, want %s", timer.delay, wantDelay)
		}
		return timer
	case <-time.After(controllerTestTimeout):
		t.Fatalf("timed out waiting for %s timer", wantDelay)
		return nil
	}
}

func waitSnapshot(t *testing.T, controller *Controller, match func(Snapshot) bool, description string) Snapshot {
	t.Helper()
	deadline := time.NewTimer(controllerTestTimeout)
	defer deadline.Stop()
	for {
		snapshot := controller.Snapshot()
		if match(snapshot) {
			return snapshot
		}
		select {
		case _, ok := <-controller.Updates():
			if !ok {
				t.Fatalf("controller stopped before reaching %s; final snapshot = %+v", description, controller.Snapshot())
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s; snapshot = %+v", description, controller.Snapshot())
		}
	}
}

func waitState(t *testing.T, controller *Controller, state State) Snapshot {
	t.Helper()
	return waitSnapshot(t, controller, func(snapshot Snapshot) bool {
		return snapshot.State == state && snapshot.Sequence > 0
	}, state.String())
}

func waitRetry(t *testing.T, controller *Controller, attempt int, delay time.Duration) Snapshot {
	t.Helper()
	return waitSnapshot(t, controller, func(snapshot Snapshot) bool {
		return snapshot.State == StateRetrying && snapshot.RetryAttempt == attempt && snapshot.RetryDelay == delay
	}, "retry state")
}

func receiveError(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(controllerTestTimeout):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func assertPending(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("%s completed before service teardown joined", name)
	default:
	}
}

func TestControllerPublishesReadinessAndResetsBackoff(t *testing.T) {
	activity := &activityCounter{}
	first := newFakeService(activity)
	second := newFakeService(activity)
	factory := newFakeFactory(
		factoryResult{service: first},
		factoryResult{service: second},
	)
	clock := newFakeClock()
	controller := testController(t, factory, clock)

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitSignal(t, first.started, "first service start")
	waitState(t, controller, StateStarting)

	first.markReady()
	waitState(t, controller, StateOnline)
	first.fail(errors.New("connection lost"))
	waitRetry(t, controller, 1, time.Second)
	firstRetry := waitTimer(t, clock, time.Second)
	if !firstRetry.fire() {
		t.Fatal("first retry timer did not fire")
	}

	waitFactoryCall(t, factory, 2)
	waitSignal(t, second.started, "second service start")
	second.markReady()
	waitState(t, controller, StateOnline)
	second.fail(errors.New("connection lost again"))
	waitRetry(t, controller, 1, time.Second)
	waitTimer(t, clock, time.Second)

	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
	active, maxActive := activity.counts()
	if active != 0 || maxActive != 1 {
		t.Fatalf("service activity = active %d, max %d; want 0, 1", active, maxActive)
	}
}

func TestControllerCapsTransientRetryDelayWithoutStoppingRetries(t *testing.T) {
	activity := &activityCounter{}
	services := []*fakeService{
		newFakeService(activity),
		newFakeService(activity),
		newFakeService(activity),
		newFakeService(activity),
	}
	results := make([]factoryResult, 0, len(services))
	for _, service := range services {
		results = append(results, factoryResult{service: service})
	}
	factory := newFakeFactory(results...)
	clock := newFakeClock()
	controller := testController(t, factory, clock)

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	var lastTimer *fakeTimer
	for i, delay := range delays {
		waitFactoryCall(t, factory, i+1)
		waitSignal(t, services[i].started, "service start")
		services[i].fail(errors.New("transient failure"))
		waitRetry(t, controller, i+1, delay)
		lastTimer = waitTimer(t, clock, delay)
		if i < len(delays)-1 && !lastTimer.fire() {
			t.Fatalf("retry timer %d did not fire", i+1)
		}
	}

	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
	if !lastTimer.isStopped() {
		t.Fatal("Quit() did not stop the pending retry timer")
	}
}

func TestControllerReleasesNaturallyExitedGenerationContext(t *testing.T) {
	activity := &activityCounter{}
	service := newFakeService(activity)
	factory := newFakeFactory(factoryResult{service: service})
	clock := newFakeClock()
	controller := testController(t, factory, clock)

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitSignal(t, service.started, "service start")
	runContext := waitRunContext(t, service)
	service.fail(errors.New("transient failure"))
	waitRetry(t, controller, 1, time.Second)

	select {
	case <-runContext.Done():
	case <-time.After(controllerTestTimeout):
		t.Fatal("naturally exited generation retained its derived context")
	}

	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
}

func TestControllerReconnectRequiredWaitsForManualRestart(t *testing.T) {
	activity := &activityCounter{}
	first := newFakeService(activity)
	second := newFakeService(activity)
	factory := newFakeFactory(
		factoryResult{service: first},
		factoryResult{service: second},
	)
	clock := newFakeClock()
	controller := testController(t, factory, clock)

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitSignal(t, first.started, "first service start")
	first.fail(&syncer.ReconnectRequiredError{Err: errors.New("credentials expired")})
	waitState(t, controller, StateReconnectRequired)
	if got := len(clock.created); got != 0 {
		t.Fatalf("reconnect-required created %d retry timers, want 0", got)
	}
	if got := factory.callCount(); got != 1 {
		t.Fatalf("factory calls = %d, want 1 before manual restart", got)
	}

	if err := controller.Restart(); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	waitFactoryCall(t, factory, 2)
	waitSignal(t, second.started, "second service start")
	second.markReady()
	waitState(t, controller, StateOnline)
	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
}

func TestControllerRestartJoinsOldGenerationBeforeStartingNew(t *testing.T) {
	activity := &activityCounter{}
	cancelGate := make(chan struct{})
	first := newFakeService(activity)
	first.cancelGate = cancelGate
	second := newFakeService(activity)
	factory := newFakeFactory(
		factoryResult{service: first},
		factoryResult{service: second},
	)
	controller := testController(t, factory, newFakeClock())

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitSignal(t, first.started, "first service start")
	first.markReady()
	firstOnline := waitState(t, controller, StateOnline)
	if firstOnline.Generation != 1 {
		t.Fatalf("first service generation = %d, want 1", firstOnline.Generation)
	}

	restarted := make(chan error, 1)
	go func() { restarted <- controller.Restart() }()
	waitSignal(t, first.canceled, "first service cancellation")
	if got := factory.callCount(); got != 1 {
		t.Fatalf("factory calls before old generation joined = %d, want 1", got)
	}
	select {
	case err := <-restarted:
		t.Fatalf("Restart() returned before old generation joined: %v", err)
	default:
	}

	close(cancelGate)
	if err := receiveError(t, restarted, "Restart()"); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	waitSignal(t, first.joined, "first service join")
	waitFactoryCall(t, factory, 2)
	waitSignal(t, second.started, "second service start")
	second.markReady()
	secondOnline := waitState(t, controller, StateOnline)
	if secondOnline.Generation != firstOnline.Generation+1 {
		t.Fatalf("restarted service generation = %d, want %d", secondOnline.Generation, firstOnline.Generation+1)
	}
	active, maxActive := activity.counts()
	if active != 1 || maxActive != 1 {
		t.Fatalf("service activity after restart = active %d, max %d; want 1, 1", active, maxActive)
	}

	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
}

func TestControllerRestartBypassesPendingRetry(t *testing.T) {
	activity := &activityCounter{}
	first := newFakeService(activity)
	second := newFakeService(activity)
	factory := newFakeFactory(
		factoryResult{service: first},
		factoryResult{service: second},
	)
	clock := newFakeClock()
	controller := testController(t, factory, clock)

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitSignal(t, first.started, "first service start")
	first.fail(errors.New("offline"))
	waitRetry(t, controller, 1, time.Second)
	retry := waitTimer(t, clock, time.Second)

	if err := controller.Restart(); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if !retry.isStopped() {
		t.Fatal("Restart() did not stop the pending retry timer")
	}
	if retry.fire() {
		t.Fatal("stopped retry timer fired")
	}
	waitFactoryCall(t, factory, 2)
	waitSignal(t, second.started, "second service start")
	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
}

func TestControllerQuitWaitsForServiceJoin(t *testing.T) {
	for _, online := range []bool{false, true} {
		name := "starting"
		if online {
			name = "online"
		}
		t.Run(name, func(t *testing.T) {
			activity := &activityCounter{}
			cancelGate := make(chan struct{})
			service := newFakeService(activity)
			service.cancelGate = cancelGate
			factory := newFakeFactory(factoryResult{service: service})
			controller := testController(t, factory, newFakeClock())

			if err := controller.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			waitFactoryCall(t, factory, 1)
			waitSignal(t, service.started, "service start")
			if online {
				service.markReady()
				waitState(t, controller, StateOnline)
			}

			quit := make(chan error, 1)
			go func() { quit <- controller.Quit() }()
			waitSignal(t, service.canceled, "service cancellation")
			assertPending(t, controller.Done(), "controller Done")
			select {
			case err := <-quit:
				t.Fatalf("Quit() returned before service joined: %v", err)
			default:
			}

			close(cancelGate)
			if err := receiveError(t, quit, "Quit()"); err != nil {
				t.Fatalf("Quit() error = %v", err)
			}
			waitSignal(t, service.joined, "service join")
			waitSignal(t, controller.Done(), "controller Done")
			if snapshot := controller.Snapshot(); snapshot.State != StateQuitting {
				t.Fatalf("final state = %s, want quitting", snapshot.State)
			}
			active, maxActive := activity.counts()
			if active != 0 || maxActive != 1 {
				t.Fatalf("service activity = active %d, max %d; want 0, 1", active, maxActive)
			}
		})
	}
}

func TestControllerParentCancellationWaitsForServiceJoin(t *testing.T) {
	activity := &activityCounter{}
	cancelGate := make(chan struct{})
	service := newFakeService(activity)
	service.cancelGate = cancelGate
	factory := newFakeFactory(factoryResult{service: service})
	controller := testController(t, factory, newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())

	if err := controller.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitSignal(t, service.started, "service start")
	cancel()
	waitSignal(t, service.canceled, "service cancellation")
	assertPending(t, controller.Done(), "controller Done")
	close(cancelGate)
	waitSignal(t, controller.Done(), "controller Done")
	waitSignal(t, service.joined, "service join")
	active, maxActive := activity.counts()
	if active != 0 || maxActive != 1 {
		t.Fatalf("service activity = active %d, max %d; want 0, 1", active, maxActive)
	}
}

func TestControllerConcurrentRestartAndQuitNeverOverlapServices(t *testing.T) {
	activity := &activityCounter{}
	factory := newFakeFactory()
	factory.fallback = func() factoryResult {
		service := newFakeService(activity)
		service.markReady()
		return factoryResult{service: service}
	}
	controller := testController(t, factory, newFakeClock())
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFactoryCall(t, factory, 1)
	waitState(t, controller, StateOnline)

	const restarts = 32
	start := make(chan struct{})
	results := make(chan error, restarts+1)
	for i := 0; i < restarts; i++ {
		go func() {
			<-start
			results <- controller.Restart()
		}()
	}
	go func() {
		<-start
		results <- controller.Quit()
	}()
	close(start)

	for i := 0; i < restarts+1; i++ {
		err := receiveError(t, results, "concurrent controller command")
		if err != nil && !errors.Is(err, ErrControllerStopped) {
			t.Fatalf("concurrent controller command error = %v", err)
		}
	}
	waitSignal(t, controller.Done(), "controller Done")
	active, maxActive := activity.counts()
	if active != 0 || maxActive > 1 {
		t.Fatalf("service activity = active %d, max %d; want active 0 and max <= 1", active, maxActive)
	}
}

func TestControllerRejectsInvalidFactoryResults(t *testing.T) {
	tests := []struct {
		name   string
		result factoryResult
		check  func(*testing.T)
	}{
		{
			name:   "factory error",
			result: factoryResult{err: errors.New("cannot build service")},
		},
		{
			name:   "nil service",
			result: factoryResult{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := newFakeFactory(tt.result)
			clock := newFakeClock()
			controller := testController(t, factory, clock)
			if err := controller.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			waitFactoryCall(t, factory, 1)
			waitRetry(t, controller, 1, time.Second)
			waitTimer(t, clock, time.Second)
			if err := controller.Quit(); err != nil {
				t.Fatalf("Quit() error = %v", err)
			}
		})
	}

	t.Run("nil readiness signal", func(t *testing.T) {
		service := &nilReadyService{started: make(chan struct{})}
		factory := newFakeFactory(factoryResult{service: service})
		clock := newFakeClock()
		controller := testController(t, factory, clock)
		if err := controller.Start(context.Background()); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		waitFactoryCall(t, factory, 1)
		waitRetry(t, controller, 1, time.Second)
		waitTimer(t, clock, time.Second)
		select {
		case <-service.started:
			t.Fatal("service with nil readiness signal was run")
		default:
		}
		if err := controller.Quit(); err != nil {
			t.Fatalf("Quit() error = %v", err)
		}
	})
}

func TestControllerAPILifecycle(t *testing.T) {
	activity := &activityCounter{}
	service := newFakeService(activity)
	factory := newFakeFactory(factoryResult{service: service})
	controller := testController(t, factory, newFakeClock())

	if err := controller.Restart(); !errors.Is(err, ErrControllerNotStarted) {
		t.Fatalf("Restart() before Start error = %v, want %v", err, ErrControllerNotStarted)
	}
	if err := controller.Start(nil); err != nil {
		t.Fatalf("Start(nil) error = %v", err)
	}
	if err := controller.Start(context.Background()); !errors.Is(err, ErrControllerAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want %v", err, ErrControllerAlreadyStarted)
	}
	waitFactoryCall(t, factory, 1)
	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
	if err := controller.Quit(); err != nil {
		t.Fatalf("second Quit() error = %v", err)
	}
	if err := controller.Restart(); !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("Restart() after Quit error = %v, want %v", err, ErrControllerStopped)
	}
	if err := controller.Start(context.Background()); !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("Start() after Quit error = %v, want %v", err, ErrControllerStopped)
	}
}

func TestControllerQuitBeforeStartIsAtomic(t *testing.T) {
	factory := newFakeFactory()
	controller := testController(t, factory, newFakeClock())
	if err := controller.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
	waitSignal(t, controller.Done(), "controller Done")
	if snapshot := controller.Snapshot(); snapshot.State != StateQuitting {
		t.Fatalf("final state = %s, want quitting", snapshot.State)
	}
	if got := factory.callCount(); got != 0 {
		t.Fatalf("factory calls = %d, want 0", got)
	}
	if err := controller.Start(context.Background()); !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("Start() after pre-start Quit error = %v, want %v", err, ErrControllerStopped)
	}
}

func TestNewControllerValidatesOptions(t *testing.T) {
	factory := func() (DaemonService, error) { return nil, nil }
	tests := []struct {
		name    string
		factory ServiceFactory
		options ControllerOptions
	}{
		{name: "nil factory"},
		{
			name:    "negative initial delay",
			factory: factory,
			options: ControllerOptions{InitialRetryDelay: -time.Second},
		},
		{
			name:    "maximum below initial",
			factory: factory,
			options: ControllerOptions{InitialRetryDelay: 2 * time.Second, MaximumRetryDelay: time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewController(tt.factory, tt.options); err == nil {
				t.Fatal("NewController() error = nil, want validation error")
			}
		})
	}
}

func TestControllerStateString(t *testing.T) {
	tests := map[State]string{
		StateStarting:          "starting",
		StateOnline:            "online",
		StateRetrying:          "retrying",
		StateReconnectRequired: "reconnect-required",
		StateQuitting:          "quitting",
		State(99):              "state(99)",
	}
	for state, want := range tests {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}
