package syncer

import (
	"sync"
	"time"
)

type wedgeWatchdogMode string

const (
	wedgeWatchdogOff     wedgeWatchdogMode = "off"
	wedgeWatchdogObserve wedgeWatchdogMode = "observe"
	wedgeWatchdogEnforce wedgeWatchdogMode = "enforce"
)

// defaultWedgeWatchdogWindow is deliberately generous. The window must clear the *silent-tool tail* — a
// turn parked in an 8-minute build/test tool call emits nothing on the notification stream but is
// perfectly healthy — not typical inter-delta pauses. Erring large is the correct asymmetry: a false
// trip costs one interrupted turn (bounded — the session resumes with its id preserved), while too tight
// a window would kill legitimate long work. The evidence to justify a tighter value comes from observe
// mode's own trip logs; until then this default is intentionally conservative.
const defaultWedgeWatchdogWindow = 30 * time.Minute

// wedgeWatchdog detects a wedged agent turn: one that started but whose runtime notification stream has
// gone silent while the process is still alive — the dropped-turn-end class behind daemon hotfix #70,
// which otherwise leaves a session stuck "working" forever. It watches notification INACTIVITY, never
// total turn duration (a duration cap is exactly the legitimate-long-turn killer).
//
// Observe-first: mode "observe" (default) only LOGS a trip, making the detector its own evidence
// instrument — every trip is a real silence-gap sample, per driver, that justifies the arming window.
// Mode "enforce" additionally stops the process; that closes the event stream and hands recovery to the
// existing stream-close -> disconnected -> restart path, so the watchdog is a detector, not new recovery
// machinery. NOTE: observe mode does not close the wedging gap — a wedged session still never recovers
// until enforce arms the trip; what observe adds is a loud, identifiable wedge log (first-ever alert).
type wedgeWatchdog struct {
	cfg     Config
	agentID string
	driver  RuntimeKind
	mode    wedgeWatchdogMode
	window  time.Duration
	stop    func() error
	now     func() time.Time

	mu           sync.Mutex
	lastActivity time.Time
	activeTurn   string
	tripped      bool // one trip per turn, so a wedge logs once rather than every check

	done chan struct{}
}

// newWedgeWatchdog reads mode + window from the environment. Returns nil (a no-op) when disabled; every
// method is nil-safe so callers never branch on it. Config is env-driven from day one so enforce mode
// (phase 2) is a default flip, not a code change.
func newWedgeWatchdog(cfg Config, agentID string, driver RuntimeKind, stop func() error) *wedgeWatchdog {
	mode := wedgeWatchdogMode(getenv("NOTTY_WEDGE_WATCHDOG", string(wedgeWatchdogObserve)))
	switch mode {
	case wedgeWatchdogObserve, wedgeWatchdogEnforce:
	case wedgeWatchdogOff:
		return nil
	default:
		mode = wedgeWatchdogObserve
	}
	window := defaultWedgeWatchdogWindow
	if raw := getenv("NOTTY_WEDGE_WATCHDOG_WINDOW", ""); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			window = parsed
		}
	}
	return &wedgeWatchdog{
		cfg:     cfg,
		agentID: agentID,
		driver:  driver,
		mode:    mode,
		window:  window,
		stop:    stop,
		now:     time.Now,
		done:    make(chan struct{}),
	}
}

// noteActivity records a runtime notification — called on EVERY app-server event, including the ones the
// driver does not forward as lifecycle events. That raw stream is the fine-grained liveness signal the
// coarse turn events cannot provide.
func (w *wedgeWatchdog) noteActivity() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.lastActivity = w.now()
	w.mu.Unlock()
}

func (w *wedgeWatchdog) turnStarted(turnID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.activeTurn = turnID
	w.lastActivity = w.now()
	w.tripped = false
	w.mu.Unlock()
}

func (w *wedgeWatchdog) turnEnded() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.activeTurn = ""
	w.tripped = false
	w.mu.Unlock()
}

// checkOnce trips when a turn is active, not already tripped, and the notification stream has been silent
// longer than the window. It is called both by run()'s ticker and directly by tests (with an injected
// clock) for determinism. Returns whether it tripped.
func (w *wedgeWatchdog) checkOnce() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	if w.activeTurn == "" || w.tripped {
		w.mu.Unlock()
		return false
	}
	silence := w.now().Sub(w.lastActivity)
	if silence < w.window {
		w.mu.Unlock()
		return false
	}
	w.tripped = true
	turnID := w.activeTurn
	mode := w.mode
	stop := w.stop
	w.mu.Unlock()

	appendAgentLog(w.cfg, w.agentID, "wedge watchdog tripped: driver=%s turn=%s silent=%s window=%s mode=%s", w.driver, turnID, silence.Round(time.Second), w.window, mode)
	if mode == wedgeWatchdogEnforce && stop != nil {
		if err := stop(); err != nil {
			appendAgentLog(w.cfg, w.agentID, "wedge watchdog stop failed: turn=%s err=%v", turnID, err)
		}
	}
	return true
}

// run polls checkOnce until close(). Check interval is a fraction of the window, clamped so a long
// production window still detects within a bounded lag and a short test window is not busy-looped.
func (w *wedgeWatchdog) run() {
	if w == nil {
		return
	}
	interval := w.window / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.checkOnce()
		}
	}
}

func (w *wedgeWatchdog) close() {
	if w == nil {
		return
	}
	select {
	case <-w.done:
	default:
		close(w.done)
	}
}
