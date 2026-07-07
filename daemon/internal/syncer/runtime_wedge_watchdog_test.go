package syncer

import (
	"context"
	"sync"
	"testing"
	"time"
)

// testClock is a manually-advanced clock so watchdog trips are deterministic (no real sleeps / no
// dependence on the run() ticker — tests drive checkOnce() directly).
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// stateForTest reads the watchdog's arm state and last-activity stamp under the lock, for driver-wiring
// assertions that a turn armed/disarmed and activity was recorded.
func (w *wedgeWatchdog) stateForTest() (activeTurn string, lastActivity time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeTurn, w.lastActivity
}

func newTestWatchdog(t *testing.T, mode wedgeWatchdogMode, window time.Duration, clock *testClock, stop func() error) *wedgeWatchdog {
	t.Helper()
	return &wedgeWatchdog{
		cfg:     Config{DataDir: t.TempDir()},
		agentID: "agent-test",
		driver:  RuntimeCodex,
		mode:    mode,
		window:  window,
		stop:    stop,
		now:     clock.now,
		done:    make(chan struct{}),
	}
}

func TestWedgeWatchdogObserveModeLogsWithoutStopping(t *testing.T) {
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	stops := 0
	w := newTestWatchdog(t, wedgeWatchdogObserve, time.Minute, clock, func() error { stops++; return nil })

	w.turnStarted("turn-1")
	clock.advance(2 * time.Minute) // silence past the window while a turn is active

	if !w.checkOnce() {
		t.Fatal("expected a wedge trip after silence exceeded the window")
	}
	if stops != 0 {
		t.Fatalf("observe mode must not stop the process, got %d stop calls", stops)
	}
}

func TestWedgeWatchdogEnforceModeStopsOnTrip(t *testing.T) {
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	stops := 0
	w := newTestWatchdog(t, wedgeWatchdogEnforce, time.Minute, clock, func() error { stops++; return nil })

	w.turnStarted("turn-1")
	clock.advance(90 * time.Second)

	if !w.checkOnce() {
		t.Fatal("expected a wedge trip")
	}
	if stops != 1 {
		t.Fatalf("enforce mode must stop exactly once on trip, got %d", stops)
	}
}

func TestWedgeWatchdogDoesNotTripWithoutAnActiveTurn(t *testing.T) {
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	w := newTestWatchdog(t, wedgeWatchdogEnforce, time.Minute, clock, func() error { return nil })

	// No turnStarted: an idle session that is legitimately silent must never be a wedge.
	w.noteActivity()
	clock.advance(10 * time.Minute)
	if w.checkOnce() {
		t.Fatal("silence while idle (no active turn) must not trip")
	}
}

func TestWedgeWatchdogActivityResetsTheWindow(t *testing.T) {
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	w := newTestWatchdog(t, wedgeWatchdogEnforce, time.Minute, clock, func() error { return nil })

	w.turnStarted("turn-1")
	clock.advance(50 * time.Second)
	if w.checkOnce() {
		t.Fatal("must not trip before the window elapses")
	}
	// A notification arrives — a healthy long turn emitting activity — resetting the clock.
	w.noteActivity()
	clock.advance(50 * time.Second) // 50s since last activity, still under the minute
	if w.checkOnce() {
		t.Fatal("activity must reset the inactivity window (healthy long turn not killed)")
	}
	clock.advance(20 * time.Second) // now 70s of silence
	if !w.checkOnce() {
		t.Fatal("expected a trip once silence past the window followed the last activity")
	}
}

func TestWedgeWatchdogTripsOncePerTurn(t *testing.T) {
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	stops := 0
	w := newTestWatchdog(t, wedgeWatchdogEnforce, time.Minute, clock, func() error { stops++; return nil })

	w.turnStarted("turn-1")
	clock.advance(2 * time.Minute)
	if !w.checkOnce() {
		t.Fatal("expected first trip")
	}
	clock.advance(2 * time.Minute)
	if w.checkOnce() {
		t.Fatal("a wedge must log/stop once, not on every subsequent check")
	}
	if stops != 1 {
		t.Fatalf("expected exactly one stop across repeated checks, got %d", stops)
	}

	// A new turn re-arms the detector.
	w.turnStarted("turn-2")
	clock.advance(2 * time.Minute)
	if !w.checkOnce() {
		t.Fatal("a new turn must re-arm the watchdog")
	}
}

func TestWedgeWatchdogTurnEndedDisarms(t *testing.T) {
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	w := newTestWatchdog(t, wedgeWatchdogEnforce, time.Minute, clock, func() error { return nil })

	w.turnStarted("turn-1")
	w.turnEnded()
	clock.advance(10 * time.Minute)
	if w.checkOnce() {
		t.Fatal("a completed turn must not trip on later silence")
	}
}

func TestNewWedgeWatchdogEnvConfig(t *testing.T) {
	t.Setenv("NOTTY_WEDGE_WATCHDOG", "off")
	if w := newWedgeWatchdog(Config{}, "a", RuntimeCodex, nil); w != nil {
		t.Fatal("off mode should return a nil (disabled) watchdog")
	}
	// nil watchdog methods must be no-ops (callers never branch on it).
	var nilw *wedgeWatchdog
	nilw.noteActivity()
	nilw.turnStarted("t")
	nilw.turnEnded()
	nilw.close()
	if nilw.checkOnce() {
		t.Fatal("nil watchdog must never trip")
	}

	t.Setenv("NOTTY_WEDGE_WATCHDOG", "")
	def := newWedgeWatchdog(Config{}, "a", RuntimeCodex, nil)
	if def == nil || def.mode != wedgeWatchdogObserve || def.window != defaultWedgeWatchdogWindow {
		t.Fatalf("default must be observe mode with the default window, got %#v", def)
	}

	t.Setenv("NOTTY_WEDGE_WATCHDOG", "enforce")
	t.Setenv("NOTTY_WEDGE_WATCHDOG_WINDOW", "45m")
	tuned := newWedgeWatchdog(Config{}, "a", RuntimeCodex, nil)
	if tuned == nil || tuned.mode != wedgeWatchdogEnforce || tuned.window != 45*time.Minute {
		t.Fatalf("env should set enforce mode + 45m window, got %#v", tuned)
	}

	t.Setenv("NOTTY_WEDGE_WATCHDOG", "nonsense")
	if fb := newWedgeWatchdog(Config{}, "a", RuntimeCodex, nil); fb == nil || fb.mode != wedgeWatchdogObserve {
		t.Fatalf("an unrecognized mode must fall back to observe, got %#v", fb)
	}

	// An invalid window falls back to the default (and logs loudly — see newWedgeWatchdog).
	t.Setenv("NOTTY_WEDGE_WATCHDOG", "observe")
	t.Setenv("NOTTY_WEDGE_WATCHDOG_WINDOW", "not-a-duration")
	if bad := newWedgeWatchdog(Config{}, "a", RuntimeCodex, nil); bad == nil || bad.window != defaultWedgeWatchdogWindow {
		t.Fatalf("an invalid window must fall back to the default, got %#v", bad)
	}
}

// TestCodexRuntimeProcessWedgeWatchdogWiredThroughForwardEvents proves the driver actually feeds the
// watchdog: a turn started on the real app-server event path arms it (and a non-lifecycle notification
// counts as activity), while a turn completion disarms it. Observe mode keeps the process alive so the
// assertion needs no Stop()/channel-close choreography; the enforce trip -> Stop() -> recovery path is
// covered by the component tests above and the existing stream-close recovery tests.
func TestCodexRuntimeProcessWedgeWatchdogWiredThroughForwardEvents(t *testing.T) {
	app := newFakeCodexRuntimeApp()
	process := &codexRuntimeProcess{
		app:          app,
		instructions: "driver instructions",
		events:       make(chan RuntimeEvent, 8),
	}
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	process.watchdog = &wedgeWatchdog{
		cfg:     Config{DataDir: t.TempDir()},
		agentID: "agent-test",
		driver:  RuntimeCodex,
		mode:    wedgeWatchdogObserve,
		window:  time.Minute,
		now:     clock.now,
		done:    make(chan struct{}),
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatalf("start process: %v", err)
	}

	// A non-lifecycle notification (activity only) then a turn start — both through the real path.
	// Reading the forwarded turn-started event synchronizes past both, so the watchdog is armed.
	app.events <- appServerEvent{Method: "codex/progress", Params: rawJSON(t, `{}`)}
	app.events <- appServerEvent{Method: "turn/started", Params: rawJSON(t, `{"turn":{"id":"turn_1"}}`)}
	if ev := <-process.Events(); ev.Kind != RuntimeEventTurnStarted || ev.TurnID != "turn_1" {
		t.Fatalf("expected forwarded turn-started, got %#v", ev)
	}

	clock.advance(2 * time.Minute)
	if !process.watchdog.checkOnce() {
		t.Fatal("a turn armed via forwardEvents must trip after the notification stream goes silent")
	}

	// A completion through the path disarms the detector.
	app.events <- appServerEvent{Method: "turn/completed", Params: rawJSON(t, `{}`)}
	if ev := <-process.Events(); ev.Kind != RuntimeEventTurnCompleted {
		t.Fatalf("expected forwarded turn-completed, got %#v", ev)
	}
	clock.advance(2 * time.Minute)
	if process.watchdog.checkOnce() {
		t.Fatal("a completed turn must not trip on later silence")
	}

	close(app.events)
	if err := process.Stop(); err != nil {
		t.Fatalf("stop process: %v", err)
	}
}
