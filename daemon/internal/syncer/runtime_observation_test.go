package syncer

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingRuntimeObserver struct {
	mu           sync.Mutex
	observations []RuntimeObservation
	wake         chan struct{}
}

func newRecordingRuntimeObserver() *recordingRuntimeObserver {
	return &recordingRuntimeObserver{wake: make(chan struct{}, 32)}
}

func (o *recordingRuntimeObserver) ObserveRuntime(observation RuntimeObservation) {
	o.mu.Lock()
	o.observations = append(o.observations, observation)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *recordingRuntimeObserver) wait(t *testing.T, count int) []RuntimeObservation {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		o.mu.Lock()
		observations := append([]RuntimeObservation(nil), o.observations...)
		o.mu.Unlock()
		if len(observations) >= count {
			return observations
		}
		select {
		case <-o.wake:
		case <-deadline.C:
			t.Fatalf("runtime observations = %#v, want at least %d", observations, count)
		}
	}
}

func TestRuntimeObservationsAreOrderedTokenFreeAndDeduplicateTurnStarted(t *testing.T) {
	observer := newRecordingRuntimeObserver()
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))
	defer supervisor.Shutdown()

	current := &agent{ID: "agent-sensitive-id", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	process := driver.only(t)

	process.events <- RuntimeEvent{Kind: RuntimeEventTurnStarted, TurnID: "turn-sensitive-id"}
	process.events <- RuntimeEvent{Kind: RuntimeEventTurnStarted, TurnID: "turn-sensitive-id"}
	process.events <- RuntimeEvent{Kind: RuntimeEventTurnCompleted, TurnID: "turn-sensitive-id"}
	observations := observer.wait(t, 3)

	process.events <- RuntimeEvent{Kind: RuntimeEventTurnStarted, TurnID: "turn-sensitive-id"}
	process.events <- RuntimeEvent{Kind: RuntimeEventTurnFailed, TurnID: "turn-sensitive-id"}
	observations = observer.wait(t, 5)
	process.setExitInfo(RuntimeExitInfo{Expected: true})
	close(process.events)
	observations = observer.wait(t, 6)

	wantStates := []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationTurnStarted,
		RuntimeObservationTurnCompleted,
		RuntimeObservationTurnStarted,
		RuntimeObservationTurnFailed,
		RuntimeObservationStoppedExpected,
	}
	wantTurnSequences := []uint64{0, 1, 1, 2, 2, 2}
	if len(observations) != len(wantStates) {
		t.Fatalf("runtime observations = %#v, want exactly %d", observations, len(wantStates))
	}
	for index, observation := range observations {
		if observation.Sequence != uint64(index+1) {
			t.Errorf("observation %d sequence = %d, want %d", index, observation.Sequence, index+1)
		}
		if observation.RuntimeGeneration != 1 || observation.RuntimeKind != RuntimeCodex || observation.PID != 123 {
			t.Errorf("observation %d runtime identity = %#v", index, observation)
		}
		if observation.State != wantStates[index] || observation.TurnSequence != wantTurnSequences[index] {
			t.Errorf("observation %d = %#v, want state=%s turn_sequence=%d", index, observation, wantStates[index], wantTurnSequences[index])
		}
	}

	encoded, err := json.Marshal(observations)
	if err != nil {
		t.Fatalf("marshal observations: %v", err)
	}
	for _, forbidden := range []string{
		"agent-sensitive-id",
		"session-sensitive-id",
		"turn-sensitive-id",
		"token-sensitive-value",
		"prompt-sensitive-value",
		"provider-output-sensitive-value",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("runtime observation exposed forbidden value %q: %s", forbidden, encoded)
		}
	}
}

func TestRuntimeObservationsAreOptional(t *testing.T) {
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(agentSessionTestConfig(t, t.TempDir()), nil, newFakeRuntimeRegistry(driver))
	defer supervisor.Shutdown()
	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session without observer: %v", err)
	}
}

func TestRuntimeObservationsDisambiguateReplacementAtReusedPID(t *testing.T) {
	observer := newRecordingRuntimeObserver()
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))
	defer supervisor.Shutdown()
	supervisor.restartSleep = func(time.Duration) {}
	restarted := make(chan struct{}, 1)
	supervisor.testHookRestartComplete = func() { restarted <- struct{}{} }

	if err := supervisor.ensureSession(context.Background(), &agent{ID: "agent_1", Kind: "codex"}); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	first := driver.only(t)
	first.setExitInfo(RuntimeExitInfo{ExitCode: 1})
	close(first.events)
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime replacement did not complete")
	}

	observations := observer.wait(t, 3)
	wantStates := []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationStoppedTransient,
		RuntimeObservationReady,
	}
	wantGenerations := []uint64{1, 1, 2}
	if len(observations) != len(wantStates) {
		t.Fatalf("runtime observations = %#v, want exactly %d", observations, len(wantStates))
	}
	for index, observation := range observations {
		if observation.Sequence != uint64(index+1) || observation.State != wantStates[index] {
			t.Errorf("observation %d = %#v, want sequence=%d state=%s", index, observation, index+1, wantStates[index])
		}
		if observation.RuntimeGeneration != wantGenerations[index] {
			t.Errorf("observation %d runtime generation = %d, want %d", index, observation.RuntimeGeneration, wantGenerations[index])
		}
		if observation.PID != 123 {
			t.Errorf("observation %d PID = %d, want reused PID 123", index, observation.PID)
		}
	}
}
