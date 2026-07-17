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

type blockingRuntimeObserver struct {
	*recordingRuntimeObserver
	blockSequence uint64
	entered       chan struct{}
	release       chan struct{}
}

func newBlockingRuntimeObserver(blockSequence uint64) *blockingRuntimeObserver {
	return &blockingRuntimeObserver{
		recordingRuntimeObserver: newRecordingRuntimeObserver(),
		blockSequence:            blockSequence,
		entered:                  make(chan struct{}, 1),
		release:                  make(chan struct{}),
	}
}

func (o *blockingRuntimeObserver) ObserveRuntime(observation RuntimeObservation) {
	if observation.Sequence == o.blockSequence {
		select {
		case o.entered <- struct{}{}:
		default:
		}
		<-o.release
	}
	o.recordingRuntimeObserver.ObserveRuntime(observation)
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

func (o *recordingRuntimeObserver) waitForState(t *testing.T, state RuntimeObservationState) []RuntimeObservation {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		observations := o.snapshot()
		for _, observation := range observations {
			if observation.State == state {
				return observations
			}
		}
		select {
		case <-o.wake:
		case <-deadline.C:
			t.Fatalf("runtime observations = %#v, want state %s", observations, state)
		}
	}
}

func (o *recordingRuntimeObserver) snapshot() []RuntimeObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]RuntimeObservation(nil), o.observations...)
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
	process.closeEvents()
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
	first.closeEvents()
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

func TestRuntimeObservationDispatcherPreventsCallbackOvertake(t *testing.T) {
	observer := newBlockingRuntimeObserver(2)
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))
	defer supervisor.Shutdown()

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	observer.wait(t, 1)
	process := driver.only(t)

	workingDone := make(chan struct{})
	go func() {
		supervisor.markWorking(current.ID, process, "turn_1")
		close(workingDone)
	}()
	waitRuntimeTestSignal(t, observer.entered, "blocked turn-start callback")

	idleDone := make(chan struct{})
	go func() {
		supervisor.markIdle(current.ID, process, true)
		close(idleDone)
	}()
	waitRuntimeTestSignal(t, idleDone, "idle enqueue behind blocked callback")
	if got := observer.snapshot(); len(got) != 1 || got[0].State != RuntimeObservationReady {
		t.Fatalf("callback overtook blocked sequence: %#v", got)
	}

	close(observer.release)
	waitRuntimeTestSignal(t, workingDone, "turn-start enqueue completion")
	observations := observer.wait(t, 3)
	want := []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationTurnStarted,
		RuntimeObservationTurnIdle,
	}
	assertRuntimeObservationStates(t, observations, want)
}

func TestRuntimeObservationShutdownDrainsPendingCallbacks(t *testing.T) {
	observer := newBlockingRuntimeObserver(2)
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	observer.wait(t, 1)
	process := driver.only(t)
	supervisor.markWorking(current.ID, process, "turn_1")
	waitRuntimeTestSignal(t, observer.entered, "blocked turn-start callback")
	supervisor.markIdle(current.ID, process, true)

	shutdownDone := make(chan struct{})
	go func() {
		supervisor.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before the blocked observation and queued tail drained")
	case <-time.After(50 * time.Millisecond):
	}

	close(observer.release)
	waitRuntimeTestSignal(t, shutdownDone, "Shutdown dispatcher drain")
	assertRuntimeObservationStates(t, observer.snapshot(), []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationTurnStarted,
		RuntimeObservationTurnIdle,
		RuntimeObservationStoppedExpected,
	})
}

func TestRuntimeObservationReconcileStopsAndJoinsBeforeExpected(t *testing.T) {
	observer := newRecordingRuntimeObserver()
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))
	t.Cleanup(supervisor.Shutdown)

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	observer.wait(t, 1)
	process := driver.only(t)
	supervisor.mu.Lock()
	session := supervisor.sessions[current.ID]
	supervisor.mu.Unlock()
	if session == nil {
		t.Fatal("missing resident session")
	}
	process.stopEntered = make(chan struct{}, 1)
	process.stopRelease = make(chan struct{})
	releaseStop := sync.OnceFunc(func() { close(process.stopRelease) })
	// Registered after Shutdown so LIFO cleanup always releases a blocked Stop
	// before Shutdown attempts to join the same generation after a test failure.
	t.Cleanup(releaseStop)

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- supervisor.Reconcile(context.Background(), nil) }()
	waitRuntimeTestSignal(t, process.stopEntered, "Reconcile-owned process stop")
	assertRuntimeSupervisorLockAvailable(t, supervisor, "Reconcile process stop")

	// Queue a callback barrier behind anything the removal path could have
	// enqueued before entering Stop. FIFO dispatch means observing this barrier
	// proves every earlier callback was delivered; an early stopped_expected
	// mutation therefore fails deterministically while Stop is still held.
	supervisor.mu.Lock()
	supervisor.runtimeObservationLocked(session, RuntimeObservationTurnIdle)
	supervisor.mu.Unlock()
	beforeRelease := observer.waitForState(t, RuntimeObservationTurnIdle)
	for _, observation := range beforeRelease {
		if observation.State == RuntimeObservationStoppedExpected {
			t.Fatalf("stopped_expected callback ran before Stop returned: %#v", beforeRelease)
		}
	}
	assertRuntimeObservationStates(t, beforeRelease, []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationTurnIdle,
	})

	releaseStop()
	if err := waitRuntimeTestResult(t, reconcileDone, "Reconcile removal"); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}
	observations := observer.wait(t, 3)
	assertRuntimeObservationStates(t, observations, []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationTurnIdle,
		RuntimeObservationStoppedExpected,
	})
	assertRuntimeSessionStoppedAndJoined(t, process, session)

	// A later Shutdown owns no copy of the removed generation and cannot emit a
	// duplicate terminal observation.
	supervisor.Shutdown()
	if got := observer.snapshot(); len(got) != 3 {
		t.Fatalf("removed generation emitted more than one stopped_expected: %#v", got)
	}
}

func TestRuntimeObservationReconcileShutdownRaceEmitsExpectedOnce(t *testing.T) {
	observer := newRecordingRuntimeObserver()
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	observer.wait(t, 1)
	process := driver.only(t)
	process.stopEntered = make(chan struct{}, 1)
	process.stopRelease = make(chan struct{})

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- supervisor.Reconcile(context.Background(), nil) }()
	waitRuntimeTestSignal(t, process.stopEntered, "racing Reconcile-owned process stop")
	shutdownDone := make(chan struct{})
	go func() {
		supervisor.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown closed the dispatcher before the racing removal owner finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(process.stopRelease)
	if err := waitRuntimeTestResult(t, reconcileDone, "racing Reconcile removal"); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}
	waitRuntimeTestSignal(t, shutdownDone, "Shutdown after racing removal")
	observations := observer.snapshot()
	assertRuntimeObservationStates(t, observations, []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationStoppedExpected,
	})
}

func TestRuntimeObservationShutdownWinsReconcileRaceEmitsExpectedOnce(t *testing.T) {
	observer := newRecordingRuntimeObserver()
	cfg := agentSessionTestConfig(t, t.TempDir())
	cfg.RuntimeObserver = observer
	driver := newFakeRuntimeDriver()
	supervisor := newAgentSessionSupervisor(cfg, nil, newFakeRuntimeRegistry(driver))

	current := &agent{ID: "agent_1", Kind: "codex"}
	if err := supervisor.ensureSession(context.Background(), current); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	observer.wait(t, 1)
	process := driver.only(t)
	supervisor.mu.Lock()
	session := supervisor.sessions[current.ID]
	supervisor.mu.Unlock()
	if session == nil {
		t.Fatal("missing resident session")
	}
	process.stopEntered = make(chan struct{}, 1)
	process.stopRelease = make(chan struct{})

	shutdownDone := make(chan struct{})
	go func() {
		supervisor.Shutdown()
		close(shutdownDone)
	}()
	waitRuntimeTestSignal(t, process.stopEntered, "Shutdown-owned process stop")
	assertRuntimeSupervisorLockAvailable(t, supervisor, "Shutdown process stop")
	if err := supervisor.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("racing Reconcile after Shutdown won removal: %v", err)
	}
	if got := observer.snapshot(); len(got) != 1 {
		t.Fatalf("stopped_expected became visible before Shutdown's Stop returned: %#v", got)
	}

	close(process.stopRelease)
	waitRuntimeTestSignal(t, shutdownDone, "Shutdown-owned removal")
	observations := observer.snapshot()
	assertRuntimeObservationStates(t, observations, []RuntimeObservationState{
		RuntimeObservationReady,
		RuntimeObservationStoppedExpected,
	})
	assertRuntimeSessionStoppedAndJoined(t, process, session)
}

func assertRuntimeObservationStates(t *testing.T, observations []RuntimeObservation, want []RuntimeObservationState) {
	t.Helper()
	if len(observations) != len(want) {
		t.Fatalf("runtime observations = %#v, want states %v", observations, want)
	}
	for index, state := range want {
		if observations[index].Sequence != uint64(index+1) || observations[index].State != state {
			t.Errorf("observation %d = %#v, want sequence=%d state=%s", index, observations[index], index+1, state)
		}
	}
}

func assertRuntimeSessionStoppedAndJoined(t *testing.T, process *fakeRuntimeProcess, session *managedAgentSession) {
	t.Helper()
	process.mu.Lock()
	stopped := process.stopped
	process.mu.Unlock()
	if !stopped {
		t.Fatal("stopped_expected was delivered before the process stopped")
	}
	for name, done := range map[string]<-chan struct{}{
		"event":     session.eventLoopDone,
		"heartbeat": session.heartbeatLoopDone,
	} {
		select {
		case <-done:
		default:
			t.Fatalf("stopped_expected was delivered before the %s loop joined", name)
		}
	}
}

func assertRuntimeSupervisorLockAvailable(t *testing.T, supervisor *agentSessionSupervisor, operation string) {
	t.Helper()
	locked := make(chan struct{})
	go func() {
		supervisor.mu.Lock()
		supervisor.mu.Unlock()
		close(locked)
	}()
	waitRuntimeTestSignal(t, locked, operation+" retained supervisor lock")
}

func waitRuntimeTestSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitRuntimeTestResult(t *testing.T, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}
