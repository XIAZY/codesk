package syncer

import (
	"context"
	"strings"
	"sync"
)

type RuntimeKind string

const RuntimeCodex RuntimeKind = "codex"

type RuntimeDetection struct {
	Kind      RuntimeKind `json:"kind"`
	Available bool        `json:"available"`
	Version   string      `json:"version,omitempty"`
	Path      string      `json:"path,omitempty"`
	Reason    string      `json:"reason,omitempty"`
}

type RuntimeSpawnSpec struct {
	AgentID      string
	Workdir      string
	ToolToken    string
	Instructions string
}

type RuntimeDriver interface {
	Kind() RuntimeKind
	Detect(ctx context.Context) RuntimeDetection
	Spawn(ctx context.Context, spec RuntimeSpawnSpec) (RuntimeProcess, error)
}

type RuntimeInputKind string

const (
	RuntimeInputResumeSession RuntimeInputKind = "resumeSession"
	RuntimeInputStartSession  RuntimeInputKind = "startSession"
	RuntimeInputStartTurn     RuntimeInputKind = "startTurn"
	RuntimeInputSteerTurn     RuntimeInputKind = "steerTurn"
	RuntimeInputInterruptTurn RuntimeInputKind = "interruptTurn"
)

type RuntimeImportance string

const (
	RuntimeImportanceNormal    RuntimeImportance = "normal"
	RuntimeImportanceImportant RuntimeImportance = "important"
)

type RuntimeInput struct {
	Kind         RuntimeInputKind
	Importance   RuntimeImportance
	SessionID    string
	TurnID       string
	CWD          string
	Text         string
	Instructions string
}

type RuntimeWriteResult struct {
	SessionID string
	TurnID    string
}

type RuntimeEventKind string

const (
	RuntimeEventTurnStarted   RuntimeEventKind = "turnStarted"
	RuntimeEventTurnCompleted RuntimeEventKind = "turnCompleted"
	RuntimeEventTurnFailed    RuntimeEventKind = "turnFailed"
	RuntimeEventIdle          RuntimeEventKind = "idle"
)

type RuntimeEvent struct {
	Kind      RuntimeEventKind
	SessionID string
	TurnID    string
	Error     string
}

// RuntimeExitInfo describes why a runtime process ended. It is valid only once
// Events() has closed — the exit goroutine records it before the close, so a
// consumer that observes the channel close can read it without a race.
type RuntimeExitInfo struct {
	ExitCode int      // process exit code, or -1 if unknown/killed by signal
	Signal   string   // termination signal name, if the process was signalled
	Stderr   []string // bounded ring of the process's most recent stderr lines
	Expected bool     // true when the daemon deliberately Stop()ped the process
	Err      string   // cmd.Wait() error text, if any
}

type RuntimeProcess interface {
	Start(ctx context.Context) error
	Stop() error
	WriteStdin(ctx context.Context, input RuntimeInput) (RuntimeWriteResult, error)
	Events() <-chan RuntimeEvent
	// ExitInfo reports why the process ended; valid only after Events() closes.
	ExitInfo() RuntimeExitInfo
	PID() int
}

type runtimeRegistry struct {
	mu         sync.Mutex
	drivers    map[RuntimeKind]RuntimeDriver
	detections map[RuntimeKind]RuntimeDetection
}

func newRuntimeRegistry(drivers ...RuntimeDriver) *runtimeRegistry {
	registry := &runtimeRegistry{
		drivers:    map[RuntimeKind]RuntimeDriver{},
		detections: map[RuntimeKind]RuntimeDetection{},
	}
	for _, driver := range drivers {
		if driver == nil {
			continue
		}
		registry.drivers[driver.Kind()] = driver
	}
	return registry
}

func defaultRuntimeRegistry(cfg Config) *runtimeRegistry {
	return newRuntimeRegistry(newCodexDriver(cfg), newClaudeDriver(cfg))
}

func (r *runtimeRegistry) DetectAll(ctx context.Context) []RuntimeDetection {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	drivers := make([]RuntimeDriver, 0, len(r.drivers))
	for _, driver := range r.drivers {
		drivers = append(drivers, driver)
	}
	r.mu.Unlock()

	detections := make([]RuntimeDetection, 0, len(drivers))
	for _, driver := range drivers {
		detection := driver.Detect(ctx)
		if detection.Kind == "" {
			detection.Kind = driver.Kind()
		}
		detections = append(detections, detection)
		r.mu.Lock()
		r.detections[driver.Kind()] = detection
		r.mu.Unlock()
	}
	return detections
}

func (r *runtimeRegistry) Lookup(kind string) (RuntimeDriver, RuntimeDetection, bool) {
	if r == nil {
		return nil, RuntimeDetection{}, false
	}
	runtimeKind := RuntimeKind(strings.TrimSpace(strings.ToLower(kind)))
	r.mu.Lock()
	defer r.mu.Unlock()
	driver := r.drivers[runtimeKind]
	if driver == nil {
		return nil, RuntimeDetection{Kind: runtimeKind, Available: false, Reason: "runtime driver is not registered"}, false
	}
	detection, ok := r.detections[runtimeKind]
	if !ok {
		detection = RuntimeDetection{Kind: runtimeKind, Available: true}
	}
	return driver, detection, true
}

func (r *runtimeRegistry) cachedDetections() []RuntimeDetection {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	detections := make([]RuntimeDetection, 0, len(r.detections))
	for _, detection := range r.detections {
		detections = append(detections, detection)
	}
	return detections
}
