package reversetombstone

import (
	"strings"
	"testing"
)

func TestGenerationCASRejectsSupersededOperationReplay(t *testing.T) {
	bounds := DefaultBounds()
	state := buildDelayedReplayPrefix(t, bounds)

	if state.Window.Generation != 2 || state.Window.Operation != (Operation{Origin: ReplicaB, Sequence: 1}) {
		t.Fatalf("prefix did not end at B1 generation 2: %s", StateSummary(state))
	}
	if got := state.Messages[0]; got.Kind != MessageTombstone || got.TombstoneOperation != (Operation{Origin: ReplicaA, Sequence: 1}) || got.ExpectedWindowGeneration != 0 {
		t.Fatalf("oldest delayed request = %#v, want A1 prepared at generation 0", got)
	}

	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	if state.Window.Generation != 2 || state.Window.Operation != (Operation{Origin: ReplicaB, Sequence: 1}) {
		t.Fatalf("stale A1 changed current B1 window: %s", StateSummary(state))
	}
	if got := state.TombstoneAccepts[ReplicaA][1]; got != 1 {
		t.Fatalf("A1 accepted %d times, want exactly once", got)
	}

	// The delayed restore is also harmless because the stale tombstone never
	// reopens A1's window.
	for index := uint8(0); index < state.MessageCount; index++ {
		if state.Messages[index].Kind == MessageRestore {
			state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: index})
			break
		}
	}
	if state.Root != RootTombstoned || state.RestoreCommits[ReplicaA][1] != 1 {
		t.Fatalf("delayed A1 restore crossed B1: %s", StateSummary(state))
	}
}

func TestCausalMutationWithoutGenerationCASFindsABA(t *testing.T) {
	bounds := DefaultBounds()
	state := buildDelayedReplayPrefix(t, bounds)

	_, enabled, err := apply(state, Event{Kind: EventDeliverMessage, MessageIndex: 0}, bounds, Faults{AllowSupersededTombstoneReplay: true})
	if !enabled {
		t.Fatal("delayed A1 request was not delivered")
	}
	if err == nil || !strings.Contains(err.Error(), "accepted 2 times") {
		t.Fatalf("generation-CAS mutation error = %v, want duplicate-acceptance invariant", err)
	}
}

func TestExactCurrentTombstoneRetryDoesNotExtendDeadline(t *testing.T) {
	bounds := DefaultBounds()
	state := InitialState(bounds)
	state = mustApply(t, state, bounds, Event{Kind: EventObserveAbsent, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventBeginTombstone, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueTombstone, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueTombstone, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	deadline := state.Window.Deadline
	state = mustApply(t, state, bounds, Event{Kind: EventAdvanceTime})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})

	if state.Window.Deadline != deadline || state.Window.Generation != 1 || state.TombstoneAccepts[ReplicaA][1] != 1 {
		t.Fatalf("exact retry mutated window: %s", StateSummary(state))
	}
}

func TestRestoreAdmissionGuardsAreLoadBearing(t *testing.T) {
	t.Run("frontier", func(t *testing.T) {
		bounds := DefaultBounds()
		state := acceptedWindow(t, bounds, ReplicaA)
		message := Message{
			Kind:               MessageRestore,
			Actor:              ReplicaA,
			TombstoneOperation: Operation{Origin: ReplicaA, Sequence: 1},
			RestoreOperation:   RestoreOperation{Origin: ReplicaA, Sequence: 1},
			WindowGeneration:   1,
			RequiredFrontier:   1,
		}
		mustEnqueue(t, &state, message, bounds)
		state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
		if state.Root != RootTombstoned || state.RestoreCommits[ReplicaA][1] != 0 {
			t.Fatalf("restore without proof changed state: %s", StateSummary(state))
		}

		state = acceptedWindow(t, bounds, ReplicaA)
		mustEnqueue(t, &state, message, bounds)
		_, enabled, err := apply(state, Event{Kind: EventDeliverMessage, MessageIndex: 0}, bounds, Faults{AllowRestoreWithoutFrontier: true})
		if !enabled || err == nil || !strings.Contains(err.Error(), "before frontier proof") {
			t.Fatalf("frontier mutation enabled=%t err=%v", enabled, err)
		}
	})

	t.Run("origin", func(t *testing.T) {
		bounds := DefaultBounds()
		message := Message{
			Kind:               MessageRestore,
			Actor:              ReplicaB,
			TombstoneOperation: Operation{Origin: ReplicaA, Sequence: 1},
			RestoreOperation:   RestoreOperation{Origin: ReplicaA, Sequence: 1},
			WindowGeneration:   1,
		}
		state := acceptedWindow(t, bounds, ReplicaA)
		mustEnqueue(t, &state, message, bounds)
		state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
		if state.Root != RootTombstoned {
			t.Fatalf("wrong origin restored root: %s", StateSummary(state))
		}

		state = acceptedWindow(t, bounds, ReplicaA)
		mustEnqueue(t, &state, message, bounds)
		_, enabled, err := apply(state, Event{Kind: EventDeliverMessage, MessageIndex: 0}, bounds, Faults{AllowWrongOriginRestore: true})
		if !enabled || err == nil || !strings.Contains(err.Error(), "wrong actor") {
			t.Fatalf("origin mutation enabled=%t err=%v", enabled, err)
		}
	})

	t.Run("strict deadline", func(t *testing.T) {
		bounds := DefaultBounds()
		message := Message{
			Kind:               MessageRestore,
			Actor:              ReplicaA,
			TombstoneOperation: Operation{Origin: ReplicaA, Sequence: 1},
			RestoreOperation:   RestoreOperation{Origin: ReplicaA, Sequence: 1},
			WindowGeneration:   1,
		}
		state := acceptedWindow(t, bounds, ReplicaA)
		for state.Now < state.Window.Deadline {
			state = mustApply(t, state, bounds, Event{Kind: EventAdvanceTime})
		}
		mustEnqueue(t, &state, message, bounds)
		state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
		if state.Root != RootTombstoned {
			t.Fatalf("restore at deadline changed root: %s", StateSummary(state))
		}

		state = acceptedWindow(t, bounds, ReplicaA)
		for state.Now < state.Window.Deadline {
			state = mustApply(t, state, bounds, Event{Kind: EventAdvanceTime})
		}
		mustEnqueue(t, &state, message, bounds)
		_, enabled, err := apply(state, Event{Kind: EventDeliverMessage, MessageIndex: 0}, bounds, Faults{AllowExpiredRestore: true})
		if !enabled || err == nil || !strings.Contains(err.Error(), "at or after deadline") {
			t.Fatalf("deadline mutation enabled=%t err=%v", enabled, err)
		}
	})
}

func TestRestoreBindsAcceptedWindowGeneration(t *testing.T) {
	bounds := DefaultBounds()
	message := Message{
		Kind:               MessageRestore,
		Actor:              ReplicaA,
		TombstoneOperation: Operation{Origin: ReplicaA, Sequence: 1},
		RestoreOperation:   RestoreOperation{Origin: ReplicaA, Sequence: 1},
		WindowGeneration:   2,
	}

	state := acceptedWindow(t, bounds, ReplicaA)
	mustEnqueue(t, &state, message, bounds)
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	if state.Root != RootTombstoned || state.RestoreCommits[ReplicaA][1] != 0 {
		t.Fatalf("wrong-generation restore changed state: %s", StateSummary(state))
	}

	state = acceptedWindow(t, bounds, ReplicaA)
	mustEnqueue(t, &state, message, bounds)
	_, enabled, err := apply(state, Event{Kind: EventDeliverMessage, MessageIndex: 0}, bounds, Faults{AllowWrongRestoreGeneration: true})
	if !enabled || err == nil || !strings.Contains(err.Error(), "wrong window generation") {
		t.Fatalf("generation mutation enabled=%t err=%v", enabled, err)
	}
}

func TestConsumedRestoreReplayCommitsOnceAfterResponseLoss(t *testing.T) {
	bounds := DefaultBounds()
	state := acceptedWindow(t, bounds, ReplicaA)
	message := Message{
		Kind:               MessageRestore,
		Actor:              ReplicaA,
		TombstoneOperation: Operation{Origin: ReplicaA, Sequence: 1},
		RestoreOperation:   RestoreOperation{Origin: ReplicaA, Sequence: 1},
		WindowGeneration:   1,
	}
	mustEnqueue(t, &state, message, bounds)
	mustEnqueue(t, &state, message, bounds)
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	state = mustApply(t, state, bounds, Event{Kind: EventAdvanceTime})
	state = mustApply(t, state, bounds, Event{Kind: EventAdvanceTime})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})

	if state.RestoreCommits[ReplicaA][1] != 1 || !state.Window.Consumed || state.Root != RootActive {
		t.Fatalf("restore replay was not idempotent: %s", StateSummary(state))
	}
}

func TestConsumedReplayRequiresFullIdentityBeforeDeadline(t *testing.T) {
	bounds := DefaultBounds()
	state := acceptedWindow(t, bounds, ReplicaA)
	exact := Message{
		Kind:               MessageRestore,
		Actor:              ReplicaA,
		TombstoneOperation: Operation{Origin: ReplicaA, Sequence: 1},
		RestoreOperation:   RestoreOperation{Origin: ReplicaA, Sequence: 1},
		WindowGeneration:   1,
	}
	if outcome := deliverRestore(&state, exact, bounds, Faults{}); outcome != restoreCommitted {
		t.Fatalf("initial restore outcome = %d, want committed", outcome)
	}
	state.Now = state.Window.Deadline
	committed := state

	if outcome := deliverRestore(&state, exact, bounds, Faults{}); outcome != restoreExactReplay {
		t.Fatalf("exact post-deadline retry outcome = %d, want exact replay", outcome)
	}
	if state != committed {
		t.Fatalf("exact replay mutated state\nbefore=%s\n after=%s", StateSummary(committed), StateSummary(state))
	}

	wrongRestore := exact
	wrongRestore.RestoreOperation = RestoreOperation{Origin: ReplicaA, Sequence: 2}
	if outcome := deliverRestore(&state, wrongRestore, bounds, Faults{}); outcome != restoreRejected {
		t.Fatalf("wrong restore-id outcome = %d, want rejected", outcome)
	}
	wrongGeneration := exact
	wrongGeneration.WindowGeneration = 2
	if outcome := deliverRestore(&state, wrongGeneration, bounds, Faults{}); outcome != restoreRejected {
		t.Fatalf("wrong generation outcome = %d, want rejected", outcome)
	}
	wrongActor := exact
	wrongActor.Actor = ReplicaB
	if outcome := deliverRestore(&state, wrongActor, bounds, Faults{}); outcome != restoreRejected {
		t.Fatalf("wrong actor outcome = %d, want rejected", outcome)
	}
	if state != committed || state.RestoreCommits[ReplicaA][1] != 1 {
		t.Fatalf("non-exact consumed replay mutated state: %s", StateSummary(state))
	}
}

func TestProjectionAndCrashGuardsAreLoadBearing(t *testing.T) {
	bounds := DefaultBounds()
	state := restoredProjectionPending(t, bounds, ReplicaA)
	state = mustApply(t, state, bounds, Event{Kind: EventExternalTombstone})
	if _, enabled, err := Apply(state, Event{Kind: EventFinishProjection, Replica: ReplicaA}, bounds); err != nil || enabled {
		t.Fatalf("stale projection enabled=%t err=%v", enabled, err)
	}
	_, enabled, err := apply(state, Event{Kind: EventFinishProjection, Replica: ReplicaA}, bounds, Faults{AllowStaleProjection: true})
	if !enabled || err == nil || !strings.Contains(err.Error(), "unsafe projection") {
		t.Fatalf("projection mutation enabled=%t err=%v", enabled, err)
	}

	state = InitialState(bounds)
	state = mustApply(t, state, bounds, Event{Kind: EventObserveAbsent, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventBeginTombstone, Replica: ReplicaA})
	before := state.Runtime[ReplicaA]
	state = mustApply(t, state, bounds, Event{Kind: EventCrashRestart, Replica: ReplicaA})
	after := state.Runtime[ReplicaA]
	after.Restarts = before.Restarts
	if after != before {
		t.Fatalf("restart changed durable workflow\nbefore=%#v\n after=%#v", before, after)
	}

	state = InitialState(bounds)
	state = mustApply(t, state, bounds, Event{Kind: EventObserveAbsent, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventBeginTombstone, Replica: ReplicaA})
	_, enabled, err = apply(state, Event{Kind: EventCrashRestart, Replica: ReplicaA}, bounds, Faults{CrashDropsWorkflow: true})
	if !enabled || err == nil || !strings.Contains(err.Error(), "lost a durable workflow") {
		t.Fatalf("restart mutation enabled=%t err=%v", enabled, err)
	}
}

func TestTraceValidatorRejectsImplementationDrift(t *testing.T) {
	bounds := DefaultBounds()
	initial := InitialState(bounds)
	first := mustApply(t, initial, bounds, Event{Kind: EventObserveAbsent, Replica: ReplicaA})
	second := mustApply(t, first, bounds, Event{Kind: EventBeginTombstone, Replica: ReplicaA})
	steps := []TraceStep{
		{Event: Event{Kind: EventObserveAbsent, Replica: ReplicaA}, After: first},
		{Event: Event{Kind: EventBeginTombstone, Replica: ReplicaA}, After: second},
	}
	if err := ValidateTrace(bounds, initial, steps); err != nil {
		t.Fatalf("valid trace: %v", err)
	}

	steps[1].After.Runtime[ReplicaA].ExpectedWindowGeneration++
	if err := ValidateTrace(bounds, initial, steps); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("drift error = %v, want divergence", err)
	}
}

func TestBoundedExplorationHasNoSafetyViolation(t *testing.T) {
	bounds := DefaultBounds()
	bounds.MaxTime = 2
	bounds.MaxWindowGeneration = 2
	bounds.MaxMessages = 2
	bounds.MaxAttempts = 1
	bounds.MaxRestarts = 0
	bounds.MaxDepth = 10
	report := Explore(bounds)
	if report.Violation != nil {
		t.Fatalf("bounded model violation after %d states/%d transitions:\n%s\ntrace=%v", report.States, report.Transitions, report.Violation, report.Trace)
	}
	if report.States < 100 || report.Transitions < report.States {
		t.Fatalf("exploration was unexpectedly shallow: %#v", report)
	}
	t.Logf("bounded model: states=%d transitions=%d max_depth=%d", report.States, report.Transitions, report.MaxDepth)
}

func buildDelayedReplayPrefix(t *testing.T, bounds Bounds) State {
	t.Helper()
	state := InitialState(bounds)

	// A prepares two identical tombstone requests. One is accepted; one is
	// delayed in the network.
	state = mustApply(t, state, bounds, Event{Kind: EventObserveAbsent, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventBeginTombstone, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueTombstone, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueTombstone, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	state = mustApply(t, state, bounds, Event{Kind: EventObserveWindow, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventObserveContent, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventObserveFrontier, Replica: ReplicaA})

	// A also sends two restores. One commits; one remains delayed after the
	// caller observes no response.
	state = mustApply(t, state, bounds, Event{Kind: EventQueueRestore, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueRestore, Replica: ReplicaA})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 1})

	// B prepares against generation 1 and accepts generation 2.
	state = mustApply(t, state, bounds, Event{Kind: EventObserveAbsent, Replica: ReplicaB})
	state = mustApply(t, state, bounds, Event{Kind: EventBeginTombstone, Replica: ReplicaB})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueTombstone, Replica: ReplicaB})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 1})
	return state
}

func acceptedWindow(t *testing.T, bounds Bounds, replica Replica) State {
	t.Helper()
	state := InitialState(bounds)
	state = mustApply(t, state, bounds, Event{Kind: EventObserveAbsent, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventBeginTombstone, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueTombstone, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	return state
}

func restoredProjectionPending(t *testing.T, bounds Bounds, replica Replica) State {
	t.Helper()
	state := acceptedWindow(t, bounds, replica)
	state = mustApply(t, state, bounds, Event{Kind: EventObserveWindow, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventObserveContent, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventObserveFrontier, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventQueueRestore, Replica: replica})
	state = mustApply(t, state, bounds, Event{Kind: EventDeliverMessage, MessageIndex: 0})
	state = mustApply(t, state, bounds, Event{Kind: EventObserveRestore, Replica: replica})
	return state
}

func mustApply(t *testing.T, state State, bounds Bounds, event Event) State {
	t.Helper()
	next, enabled, err := Apply(state, event, bounds)
	if err != nil {
		t.Fatalf("%s from %s: %v", event, StateSummary(state), err)
	}
	if !enabled {
		t.Fatalf("%s was not enabled from %s", event, StateSummary(state))
	}
	return next
}

func mustEnqueue(t *testing.T, state *State, message Message, bounds Bounds) {
	t.Helper()
	if !enqueueMessage(state, message, bounds) {
		t.Fatalf("failed to enqueue %s", message)
	}
	if err := ValidateState(*state, bounds); err != nil {
		t.Fatalf("enqueue %s: %v", message, err)
	}
}
