package syncer

// RuntimeObservationState is a bounded, provider-neutral runtime lifecycle
// state emitted for desktop diagnostics and native acceptance.
type RuntimeObservationState string

const (
	RuntimeObservationReady            RuntimeObservationState = "ready"
	RuntimeObservationTurnStarted      RuntimeObservationState = "turn_started"
	RuntimeObservationTurnCompleted    RuntimeObservationState = "turn_completed"
	RuntimeObservationTurnFailed       RuntimeObservationState = "turn_failed"
	RuntimeObservationTurnIdle         RuntimeObservationState = "turn_idle"
	RuntimeObservationStoppedExpected  RuntimeObservationState = "stopped_expected"
	RuntimeObservationStoppedTransient RuntimeObservationState = "stopped_transient"
	RuntimeObservationStoppedTerminal  RuntimeObservationState = "stopped_terminal"
)

// RuntimeObservation intentionally contains no agent, session, or turn
// identifiers, prompts, tokens, errors, or provider output. Sequence orders all
// observations within one service generation; RuntimeGeneration and
// TurnSequence disambiguate PID reuse and repeated turns without exposing
// product identifiers.
type RuntimeObservation struct {
	Sequence          uint64
	RuntimeGeneration uint64
	RuntimeKind       RuntimeKind
	PID               int
	TurnSequence      uint64
	State             RuntimeObservationState
}

// RuntimeObserver receives token-free runtime lifecycle observations from one
// ordered dispatcher. The implementation must return promptly so Shutdown can
// drain and join the dispatcher; observations are diagnostics, not a second
// source of product state.
type RuntimeObserver interface {
	ObserveRuntime(RuntimeObservation)
}
