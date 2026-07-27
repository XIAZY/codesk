package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// isValidRuntimeFrame reports whether line is a syntactically valid JSON object
// frame — the read-boundary liveness signal shared by the runtime drivers. It
// accepts any well-formed object (including one carrying fields a driver does not
// map yet) and rejects malformed input and non-object JSON. So a genuinely
// communicating runtime refreshes liveness even as the wire format evolves, while
// a process spewing junk or partial output cannot manufacture a heartbeat. It is
// deliberately syntactic, not semantic: liveness must not depend on which frame
// types the parser currently recognizes.
func isValidRuntimeFrame(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var probe map[string]json.RawMessage
	return json.Unmarshal(trimmed, &probe) == nil
}

type RuntimeKind string

const RuntimeCodex RuntimeKind = "codex"

type RuntimeDetection struct {
	Kind         RuntimeKind          `json:"kind"`
	Available    bool                 `json:"available"`
	Version      string               `json:"version,omitempty"`
	Path         string               `json:"path,omitempty"`
	Reason       string               `json:"reason,omitempty"`
	ModelCatalog *RuntimeModelCatalog `json:"modelCatalog,omitempty"`
}

type RuntimeModelCatalog struct {
	Models                    []RuntimeModel `json:"models"`
	ModelProvenance           string         `json:"modelProvenance,omitempty"`
	ReasoningEfforts          []string       `json:"reasoningEfforts,omitempty"`
	ReasoningEffortProvenance string         `json:"reasoningEffortProvenance,omitempty"`
	Error                     string         `json:"error,omitempty"`
}

type RuntimeModel struct {
	Model                  string   `json:"model"`
	DisplayName            string   `json:"displayName"`
	IsDefault              bool     `json:"isDefault"`
	ReasoningEfforts       []string `json:"reasoningEfforts"`
	DefaultReasoningEffort string   `json:"defaultReasoningEffort,omitempty"`
}

type RuntimeProfile struct {
	Model           string
	ReasoningEffort string
}

type RuntimeSpawnSpec struct {
	AgentID      string
	Workdir      string
	ToolToken    string
	Instructions string
	Profile      RuntimeProfile
}

type RuntimeDriver interface {
	Kind() RuntimeKind
	Detect(ctx context.Context) RuntimeDetection
	Spawn(ctx context.Context, spec RuntimeSpawnSpec) (RuntimeProcess, error)
}

type runtimeModelCatalogDetector interface {
	detectModelCatalog(context.Context) *RuntimeModelCatalog
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
	Kind        RuntimeEventKind
	SessionID   string
	TurnID      string
	Error       string
	FailureKind RuntimeFailureKind
}

type RuntimeFailureKind string

const RuntimeFailureTerminalProfile RuntimeFailureKind = "terminalProfile"

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
	// Stop is idempotent and does not return until the child has exited and the
	// Events stream is closed. Supervisor removal relies on that join before it
	// publishes stopped_expected.
	Stop() error
	WriteStdin(ctx context.Context, input RuntimeInput) (RuntimeWriteResult, error)
	Events() <-chan RuntimeEvent
	// ExitInfo reports why the process ended; valid only after Events() closes.
	ExitInfo() RuntimeExitInfo
	// ActivitySeq is a monotonically increasing count of syntactically valid
	// provider frames the driver has decoded from the runtime's stream, incremented
	// at the read boundary before any method/type mapping. It is the liveness signal
	// the supervisor's heartbeat polls: the supervisor records ITS OWN monotonic
	// time whenever this generation advances and measures silence from that, so
	// liveness never depends on comparing a wall-clock timestamp across a clock step.
	// 0 means the driver has decoded no valid frame yet.
	ActivitySeq() uint64
	PID() int
}

type runtimeRegistry struct {
	mu            sync.Mutex
	drivers       map[RuntimeKind]RuntimeDriver
	detections    map[RuntimeKind]RuntimeDetection
	catalogProbes map[RuntimeKind]runtimeCatalogProbe
}

type runtimeCatalogProbe struct {
	nextAttempt time.Time
	inFlight    bool
}

const failedCatalogRetryInterval = 5 * time.Minute

func newRuntimeRegistry(drivers ...RuntimeDriver) *runtimeRegistry {
	registry := &runtimeRegistry{
		drivers:       map[RuntimeKind]RuntimeDriver{},
		detections:    map[RuntimeKind]RuntimeDetection{},
		catalogProbes: map[RuntimeKind]runtimeCatalogProbe{},
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
		_, discoversCatalog := driver.(runtimeModelCatalogDetector)
		catalogPending := detection.Available && discoversCatalog &&
			(detection.ModelCatalog == nil || detection.ModelCatalog.Error != "")
		if detection.Available && discoversCatalog && detection.ModelCatalog == nil {
			detection.ModelCatalog = &RuntimeModelCatalog{Models: []RuntimeModel{}}
		}
		detections = append(detections, detection)
		r.mu.Lock()
		r.detections[driver.Kind()] = detection
		if catalogPending {
			r.catalogProbes[driver.Kind()] = runtimeCatalogProbe{}
		} else {
			delete(r.catalogProbes, driver.Kind())
		}
		r.mu.Unlock()
	}
	return detections
}

// discoverModelCatalogs runs outside daemon startup. DetectAll publishes runtime
// availability metadata with an empty catalog first; the first post-start
// heartbeat owns the initial probe. Failures retry at a bounded cadence, while a
// successful result (including a genuinely empty catalog) completes discovery.
// It reports whether any catalog completed so the service can immediately
// reconcile agents that were held by the pre-discovery empty state.
func (r *runtimeRegistry) discoverModelCatalogs(ctx context.Context, now time.Time) bool {
	if r == nil {
		return false
	}
	type candidate struct {
		kind     RuntimeKind
		detector runtimeModelCatalogDetector
	}

	r.mu.Lock()
	candidates := make([]candidate, 0, len(r.catalogProbes))
	for kind, probe := range r.catalogProbes {
		if probe.inFlight || (!probe.nextAttempt.IsZero() && now.Before(probe.nextAttempt)) {
			continue
		}
		detection := r.detections[kind]
		detector, ok := r.drivers[kind].(runtimeModelCatalogDetector)
		if !ok || !detection.Available {
			delete(r.catalogProbes, kind)
			continue
		}
		probe.inFlight = true
		r.catalogProbes[kind] = probe
		candidates = append(candidates, candidate{kind: kind, detector: detector})
	}
	r.mu.Unlock()

	completed := false
	for _, candidate := range candidates {
		catalog := candidate.detector.detectModelCatalog(ctx)

		r.mu.Lock()
		probe, pending := r.catalogProbes[candidate.kind]
		detection := r.detections[candidate.kind]
		if !pending || !probe.inFlight || !detection.Available {
			r.mu.Unlock()
			continue
		}
		if catalog != nil {
			detection.ModelCatalog = catalog
			r.detections[candidate.kind] = detection
		}
		if catalog != nil && catalog.Error == "" {
			delete(r.catalogProbes, candidate.kind)
			completed = true
		} else {
			r.catalogProbes[candidate.kind] = runtimeCatalogProbe{
				nextAttempt: now.Add(failedCatalogRetryInterval),
			}
		}
		r.mu.Unlock()
	}
	return completed
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
