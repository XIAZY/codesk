// Package reversetombstone is an executable, bounded reference model for the
// reversible root tombstone protocol. It deliberately contains no production
// implementation code. Production tests project their observations into these
// states and events, then use ValidateTrace to detect protocol drift.
package reversetombstone

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	replicaCount     = 2
	maxSequenceLimit = 2
	maxMessageLimit  = 8
)

// Replica is an authenticated local runtime. The model intentionally uses two
// replicas because one origin cannot expose supersession and replay races.
type Replica uint8

const (
	ReplicaA Replica = iota
	ReplicaB
)

func (r Replica) String() string {
	switch r {
	case ReplicaA:
		return "A"
	case ReplicaB:
		return "B"
	default:
		return fmt.Sprintf("replica(%d)", r)
	}
}

// Operation is a durable, replica-local operation identifier. Sequence zero
// is the invalid value. The production UUID is abstracted to (origin, sequence).
type Operation struct {
	Origin   Replica
	Sequence uint8
}

func (op Operation) Valid(bounds Bounds) bool {
	return op.Origin < replicaCount && op.Sequence > 0 && op.Sequence <= bounds.MaxSequence
}

func (op Operation) String() string {
	if op.Sequence == 0 {
		return "-"
	}
	return fmt.Sprintf("%s%d", op.Origin, op.Sequence)
}

// RestoreOperation is a durable identifier in a namespace distinct from the
// tombstone operation. A response-loss retry must preserve this value, while a
// later logical restore attempt must allocate a new one.
type RestoreOperation struct {
	Origin   Replica
	Sequence uint8
}

func (op RestoreOperation) Valid(bounds Bounds) bool {
	return op.Origin < replicaCount && op.Sequence > 0 && op.Sequence <= bounds.MaxSequence
}

func (op RestoreOperation) String() string {
	if op.Sequence == 0 {
		return "-"
	}
	return fmt.Sprintf("restore-%s%d", op.Origin, op.Sequence)
}

type RootState uint8

const (
	RootActive RootState = iota
	RootTombstoned
)

func (s RootState) String() string {
	if s == RootTombstoned {
		return "tombstoned"
	}
	return "active"
}

type NamespaceState uint8

const (
	NamespaceMatching NamespaceState = iota
	NamespaceChanged
	NamespaceConflicting
)

func (s NamespaceState) String() string {
	switch s {
	case NamespaceMatching:
		return "matching"
	case NamespaceChanged:
		return "changed"
	case NamespaceConflicting:
		return "conflicting"
	default:
		return fmt.Sprintf("namespace(%d)", s)
	}
}

type Occupant uint8

const (
	OccupantContent Occupant = iota
	OccupantAbsent
	OccupantNonContent
)

func (o Occupant) String() string {
	switch o {
	case OccupantContent:
		return "content"
	case OccupantAbsent:
		return "absent"
	case OccupantNonContent:
		return "non-content"
	default:
		return fmt.Sprintf("occupant(%d)", o)
	}
}

type Phase uint8

const (
	PhaseIdle Phase = iota
	PhaseTombstonePending
	PhaseWindowOpen
	PhaseContentSyncing
	PhaseRestorePending
	PhaseProjectionPending
)

func (p Phase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseTombstonePending:
		return "tombstone_pending"
	case PhaseWindowOpen:
		return "window_open"
	case PhaseContentSyncing:
		return "content_syncing"
	case PhaseRestorePending:
		return "restore_pending"
	case PhaseProjectionPending:
		return "projection_pending"
	default:
		return fmt.Sprintf("phase(%d)", p)
	}
}

// Runtime contains only durable or reconstructible protocol facts. A
// CrashRestart event may increment Restarts but must preserve every other field.
type Runtime struct {
	Phase                    Phase
	Occupant                 Occupant
	WorkflowOperation        Operation
	RestoreOperation         RestoreOperation
	ExpectedWindowGeneration uint8
	AcceptedWindowGeneration uint8
	NextSequence             uint8
	NextRestoreSequence      uint8
	LocalFrontier            uint8
	MergedFrontier           uint8
	RequiredFrontier         uint8
	TombstoneAttempts        uint8
	RestoreAttempts          uint8
	Restarts                 uint8
	CompletedGeneration      uint8
}

// Window is the one current backend reverse-window row. Generation never
// resets when the row is consumed or expires.
type Window struct {
	Present                    bool
	Generation                 uint8
	Operation                  Operation
	Origin                     Replica
	AcceptedExpectedGeneration uint8
	Deadline                   uint8
	Consumed                   bool
	RestoreOperation           RestoreOperation
}

type MessageKind uint8

const (
	MessageTombstone MessageKind = iota
	MessageRestore
)

func (k MessageKind) String() string {
	if k == MessageRestore {
		return "restore"
	}
	return "tombstone"
}

// Message is an already-sent request. Keeping requests independent from the
// current runtime phase is essential: it exposes delayed-request ABA races.
type Message struct {
	Kind                     MessageKind
	Actor                    Replica
	TombstoneOperation       Operation
	RestoreOperation         RestoreOperation
	ExpectedWindowGeneration uint8
	WindowGeneration         uint8
	RequiredFrontier         uint8
}

func (m Message) String() string {
	if m.Kind == MessageRestore {
		return fmt.Sprintf("restore(%s,tombstone=%s,restore=%s,generation=%d,required=%d)", m.Actor, m.TombstoneOperation, m.RestoreOperation, m.WindowGeneration, m.RequiredFrontier)
	}
	return fmt.Sprintf("tombstone(%s,op=%s,expected=%d)", m.Actor, m.TombstoneOperation, m.ExpectedWindowGeneration)
}

// State is comparable so exhaustive exploration can deduplicate it exactly.
type State struct {
	Now             uint8
	Root            RootState
	Namespace       NamespaceState
	Window          Window
	Runtime         [replicaCount]Runtime
	BackendFrontier uint8

	TombstoneAccepts [replicaCount][maxSequenceLimit + 1]uint8
	RestoreCommits   [replicaCount][maxSequenceLimit + 1]uint8
	CommitRequired   [replicaCount][maxSequenceLimit + 1]uint8
	CommitBackend    [replicaCount][maxSequenceLimit + 1]uint8
	CommitActor      [replicaCount][maxSequenceLimit + 1]Replica
	CommitAt         [replicaCount][maxSequenceLimit + 1]uint8
	CommitDeadline   [replicaCount][maxSequenceLimit + 1]uint8

	Messages     [maxMessageLimit]Message
	MessageCount uint8

	UnsafeProjection        bool
	UnsafeRestoreGeneration bool
	LostDurableIntent       bool
}

type Bounds struct {
	MaxTime             uint8
	WindowTicks         uint8
	MaxSequence         uint8
	MaxWindowGeneration uint8
	MaxFrontier         uint8
	MaxMessages         uint8
	MaxAttempts         uint8
	MaxRestarts         uint8
	MaxDepth            int
}

func DefaultBounds() Bounds {
	return Bounds{
		MaxTime:             4,
		WindowTicks:         2,
		MaxSequence:         2,
		MaxWindowGeneration: 4,
		MaxFrontier:         1,
		MaxMessages:         4,
		MaxAttempts:         2,
		MaxRestarts:         1,
		MaxDepth:            24,
	}
}

func (b Bounds) Validate() error {
	if b.WindowTicks == 0 {
		return errors.New("window ticks must be positive")
	}
	if b.MaxSequence == 0 || b.MaxSequence > maxSequenceLimit {
		return fmt.Errorf("max sequence must be in [1,%d]", maxSequenceLimit)
	}
	if b.MaxMessages == 0 || b.MaxMessages > maxMessageLimit {
		return fmt.Errorf("max messages must be in [1,%d]", maxMessageLimit)
	}
	if b.MaxDepth <= 0 {
		return errors.New("max depth must be positive")
	}
	if b.MaxWindowGeneration < 2 {
		return errors.New("max window generation must permit supersession")
	}
	return nil
}

func InitialState(bounds Bounds) State {
	state := State{
		Root:      RootActive,
		Namespace: NamespaceMatching,
	}
	for replica := ReplicaA; replica < replicaCount; replica++ {
		state.Runtime[replica] = Runtime{
			Phase:               PhaseIdle,
			Occupant:            OccupantContent,
			NextSequence:        1,
			NextRestoreSequence: 1,
		}
	}
	return state
}

type EventKind uint8

const (
	EventObserveContent EventKind = iota
	EventObserveAbsent
	EventObserveNonContent
	EventBeginTombstone
	EventQueueTombstone
	EventDeliverMessage
	EventObserveWindow
	EventRebaseTombstone
	EventMutateContent
	EventMergeContent
	EventSyncBackend
	EventObserveFrontier
	EventQueueRestore
	EventObserveRestore
	EventFinishProjection
	EventFreshDeleteAfterProof
	EventObserveExpiry
	EventObserveSuperseded
	EventAdvanceTime
	EventChangeNamespace
	EventExternalTombstone
	EventCrashRestart
)

func (k EventKind) String() string {
	names := [...]string{
		"observe_content",
		"observe_absent",
		"observe_non_content",
		"begin_tombstone",
		"queue_tombstone",
		"deliver_message",
		"observe_window",
		"rebase_tombstone",
		"mutate_content",
		"merge_content",
		"sync_backend",
		"observe_frontier",
		"queue_restore",
		"observe_restore",
		"finish_projection",
		"fresh_delete_after_proof",
		"observe_expiry",
		"observe_superseded",
		"advance_time",
		"change_namespace",
		"external_tombstone",
		"crash_restart",
	}
	if int(k) >= len(names) {
		return fmt.Sprintf("event(%d)", k)
	}
	return names[k]
}

type Event struct {
	Kind         EventKind
	Replica      Replica
	MessageIndex uint8
	Namespace    NamespaceState
}

func (e Event) String() string {
	switch e.Kind {
	case EventDeliverMessage:
		return fmt.Sprintf("%s[%d]", e.Kind, e.MessageIndex)
	case EventChangeNamespace:
		return fmt.Sprintf("%s(%s)", e.Kind, e.Namespace)
	case EventAdvanceTime, EventExternalTombstone:
		return e.Kind.String()
	default:
		return fmt.Sprintf("%s(%s)", e.Kind, e.Replica)
	}
}

// Faults exist only for causal-mutation tests. A production trace is always
// validated with the zero value.
type Faults struct {
	AllowSupersededTombstoneReplay bool
	AllowRestoreWithoutFrontier    bool
	AllowWrongRestoreGeneration    bool
	AllowWrongOriginRestore        bool
	AllowExpiredRestore            bool
	AllowStaleProjection           bool
	CrashDropsWorkflow             bool
}

type restoreDeliveryOutcome uint8

const (
	restoreRejected restoreDeliveryOutcome = iota
	restoreCommitted
	restoreExactReplay
)

func Apply(state State, event Event, bounds Bounds) (State, bool, error) {
	return apply(state, event, bounds, Faults{})
}

func apply(state State, event Event, bounds Bounds, faults Faults) (State, bool, error) {
	if err := bounds.Validate(); err != nil {
		return State{}, false, err
	}
	if err := ValidateState(state, bounds); err != nil {
		return State{}, false, fmt.Errorf("invalid pre-state: %w", err)
	}

	next := state
	if event.Replica >= replicaCount && event.Kind != EventDeliverMessage && event.Kind != EventAdvanceTime && event.Kind != EventChangeNamespace && event.Kind != EventExternalTombstone {
		return state, false, nil
	}

	switch event.Kind {
	case EventObserveContent:
		runtime := &next.Runtime[event.Replica]
		if runtime.Occupant == OccupantContent {
			return state, false, nil
		}
		runtime.Occupant = OccupantContent
		if runtime.Phase == PhaseWindowOpen || runtime.Phase == PhaseRestorePending {
			runtime.Phase = PhaseContentSyncing
		}

	case EventObserveAbsent:
		runtime := &next.Runtime[event.Replica]
		if runtime.Occupant == OccupantAbsent {
			return state, false, nil
		}
		runtime.Occupant = OccupantAbsent

	case EventObserveNonContent:
		runtime := &next.Runtime[event.Replica]
		if runtime.Occupant == OccupantNonContent {
			return state, false, nil
		}
		runtime.Occupant = OccupantNonContent

	case EventBeginTombstone:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseIdle || runtime.Occupant != OccupantAbsent || next.Namespace != NamespaceMatching || runtime.NextSequence == 0 || runtime.NextSequence > bounds.MaxSequence {
			return state, false, nil
		}
		runtime.WorkflowOperation = Operation{Origin: event.Replica, Sequence: runtime.NextSequence}
		runtime.NextSequence++
		runtime.ExpectedWindowGeneration = next.Window.Generation
		runtime.AcceptedWindowGeneration = 0
		runtime.TombstoneAttempts = 0
		runtime.RestoreAttempts = 0
		runtime.Phase = PhaseTombstonePending

	case EventQueueTombstone:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseTombstonePending || !runtime.WorkflowOperation.Valid(bounds) || runtime.TombstoneAttempts >= bounds.MaxAttempts {
			return state, false, nil
		}
		message := Message{
			Kind:                     MessageTombstone,
			Actor:                    event.Replica,
			TombstoneOperation:       runtime.WorkflowOperation,
			ExpectedWindowGeneration: runtime.ExpectedWindowGeneration,
		}
		if !enqueueMessage(&next, message, bounds) {
			return state, false, nil
		}
		runtime = &next.Runtime[event.Replica]
		runtime.TombstoneAttempts++

	case EventDeliverMessage:
		message, ok := dequeueMessage(&next, event.MessageIndex)
		if !ok {
			return state, false, nil
		}
		if message.Kind == MessageTombstone {
			deliverTombstone(&next, message, bounds, faults)
		} else {
			_ = deliverRestore(&next, message, bounds, faults)
		}

	case EventObserveWindow:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseTombstonePending || !windowMatches(next.Window, event.Replica, runtime.WorkflowOperation) {
			return state, false, nil
		}
		if runtime.Occupant == OccupantContent {
			runtime.Phase = PhaseContentSyncing
		} else {
			runtime.Phase = PhaseWindowOpen
		}
		runtime.AcceptedWindowGeneration = next.Window.Generation

	case EventRebaseTombstone:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseTombstonePending || runtime.Occupant != OccupantAbsent || runtime.NextSequence == 0 || runtime.NextSequence > bounds.MaxSequence || runtime.ExpectedWindowGeneration == next.Window.Generation {
			return state, false, nil
		}
		runtime.WorkflowOperation = Operation{Origin: event.Replica, Sequence: runtime.NextSequence}
		runtime.NextSequence++
		runtime.ExpectedWindowGeneration = next.Window.Generation
		runtime.AcceptedWindowGeneration = 0
		runtime.RestoreOperation = RestoreOperation{}
		runtime.TombstoneAttempts = 0

	case EventMutateContent:
		runtime := &next.Runtime[event.Replica]
		if runtime.Occupant != OccupantContent || runtime.LocalFrontier >= bounds.MaxFrontier {
			return state, false, nil
		}
		runtime.LocalFrontier++
		if runtime.Phase == PhaseWindowOpen || runtime.Phase == PhaseRestorePending {
			runtime.Phase = PhaseContentSyncing
		}

	case EventMergeContent:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseContentSyncing || runtime.Occupant != OccupantContent || runtime.MergedFrontier == runtime.LocalFrontier {
			return state, false, nil
		}
		runtime.MergedFrontier = runtime.LocalFrontier

	case EventSyncBackend:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseContentSyncing || runtime.MergedFrontier <= next.BackendFrontier {
			return state, false, nil
		}
		next.BackendFrontier = runtime.MergedFrontier

	case EventObserveFrontier:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseContentSyncing || runtime.Occupant != OccupantContent || runtime.MergedFrontier != runtime.LocalFrontier || next.BackendFrontier < runtime.MergedFrontier {
			return state, false, nil
		}
		if !runtime.RestoreOperation.Valid(bounds) {
			if runtime.NextRestoreSequence == 0 || runtime.NextRestoreSequence > bounds.MaxSequence {
				return state, false, nil
			}
			runtime.RestoreOperation = RestoreOperation{Origin: event.Replica, Sequence: runtime.NextRestoreSequence}
			runtime.NextRestoreSequence++
		}
		runtime.RequiredFrontier = runtime.MergedFrontier
		runtime.RestoreAttempts = 0
		runtime.Phase = PhaseRestorePending

	case EventQueueRestore:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseRestorePending || runtime.RestoreAttempts >= bounds.MaxAttempts || !runtime.WorkflowOperation.Valid(bounds) || !runtime.RestoreOperation.Valid(bounds) {
			return state, false, nil
		}
		message := Message{
			Kind:               MessageRestore,
			Actor:              event.Replica,
			TombstoneOperation: runtime.WorkflowOperation,
			RestoreOperation:   runtime.RestoreOperation,
			WindowGeneration:   runtime.AcceptedWindowGeneration,
			RequiredFrontier:   runtime.RequiredFrontier,
		}
		if !enqueueMessage(&next, message, bounds) {
			return state, false, nil
		}
		runtime = &next.Runtime[event.Replica]
		runtime.RestoreAttempts++

	case EventObserveRestore:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseRestorePending || !windowMatches(next.Window, event.Replica, runtime.WorkflowOperation) || !next.Window.Consumed || next.Window.RestoreOperation != runtime.RestoreOperation {
			return state, false, nil
		}
		runtime.Phase = PhaseProjectionPending

	case EventFinishProjection:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseProjectionPending {
			return state, false, nil
		}
		safe := next.Root == RootActive && next.Namespace == NamespaceMatching && runtime.Occupant == OccupantContent && windowMatches(next.Window, event.Replica, runtime.WorkflowOperation) && next.Window.Consumed
		if !safe && !faults.AllowStaleProjection {
			return state, false, nil
		}
		if !safe {
			next.UnsafeProjection = true
		}
		runtime.CompletedGeneration = next.Window.Generation
		clearWorkflow(runtime)

	case EventFreshDeleteAfterProof:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseProjectionPending || runtime.Occupant == OccupantContent || next.Root != RootActive || next.Namespace != NamespaceMatching || runtime.NextSequence == 0 || runtime.NextSequence > bounds.MaxSequence {
			return state, false, nil
		}
		runtime.WorkflowOperation = Operation{Origin: event.Replica, Sequence: runtime.NextSequence}
		runtime.NextSequence++
		runtime.ExpectedWindowGeneration = next.Window.Generation
		runtime.AcceptedWindowGeneration = 0
		runtime.RestoreOperation = RestoreOperation{}
		runtime.TombstoneAttempts = 0
		runtime.RestoreAttempts = 0
		runtime.Phase = PhaseTombstonePending

	case EventObserveExpiry:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseWindowOpen && runtime.Phase != PhaseContentSyncing && runtime.Phase != PhaseRestorePending {
			return state, false, nil
		}
		if !windowMatches(next.Window, event.Replica, runtime.WorkflowOperation) || next.Window.Consumed || next.Now < next.Window.Deadline {
			return state, false, nil
		}
		clearWorkflow(runtime)

	case EventObserveSuperseded:
		runtime := &next.Runtime[event.Replica]
		if runtime.Phase != PhaseWindowOpen && runtime.Phase != PhaseContentSyncing && runtime.Phase != PhaseRestorePending && runtime.Phase != PhaseProjectionPending {
			return state, false, nil
		}
		if !next.Window.Present || next.Window.Operation == runtime.WorkflowOperation {
			return state, false, nil
		}
		clearWorkflow(runtime)

	case EventAdvanceTime:
		if next.Now >= bounds.MaxTime {
			return state, false, nil
		}
		next.Now++

	case EventChangeNamespace:
		if event.Namespace == NamespaceMatching || event.Namespace > NamespaceConflicting || next.Namespace == event.Namespace {
			return state, false, nil
		}
		next.Namespace = event.Namespace

	case EventExternalTombstone:
		if next.Root == RootTombstoned {
			return state, false, nil
		}
		next.Root = RootTombstoned

	case EventCrashRestart:
		runtime := &next.Runtime[event.Replica]
		if runtime.Restarts >= bounds.MaxRestarts {
			return state, false, nil
		}
		runtime.Restarts++
		if faults.CrashDropsWorkflow && runtime.Phase != PhaseIdle {
			clearWorkflow(runtime)
			next.LostDurableIntent = true
		}

	default:
		return State{}, false, fmt.Errorf("unknown event kind %d", event.Kind)
	}

	if err := ValidateState(next, bounds); err != nil {
		return next, true, err
	}
	return next, true, nil
}

func deliverTombstone(state *State, message Message, bounds Bounds, faults Faults) {
	if !message.TombstoneOperation.Valid(bounds) || message.Actor != message.TombstoneOperation.Origin {
		return
	}

	if windowMatches(state.Window, message.Actor, message.TombstoneOperation) {
		// Fingerprint equality includes the generation precondition. An exact
		// current retry never changes the deadline or generation.
		if state.Window.AcceptedExpectedGeneration == message.ExpectedWindowGeneration {
			return
		}
		return
	}

	casMatches := message.ExpectedWindowGeneration == state.Window.Generation
	if (!casMatches && !faults.AllowSupersededTombstoneReplay) || state.Namespace != NamespaceMatching || state.Window.Generation >= bounds.MaxWindowGeneration {
		return
	}

	operation := message.TombstoneOperation
	state.Window = Window{
		Present:                    true,
		Generation:                 state.Window.Generation + 1,
		Operation:                  operation,
		Origin:                     message.Actor,
		AcceptedExpectedGeneration: message.ExpectedWindowGeneration,
		Deadline:                   state.Now + bounds.WindowTicks,
	}
	state.Root = RootTombstoned
	state.TombstoneAccepts[operation.Origin][operation.Sequence]++
}

func deliverRestore(state *State, message Message, bounds Bounds, faults Faults) restoreDeliveryOutcome {
	if !message.TombstoneOperation.Valid(bounds) || !message.RestoreOperation.Valid(bounds) {
		return restoreRejected
	}
	if windowMatches(state.Window, message.Actor, message.TombstoneOperation) && state.Window.Generation == message.WindowGeneration && state.Window.Consumed && state.Window.RestoreOperation == message.RestoreOperation {
		return restoreExactReplay
	}

	generationMatches := state.Window.Present && state.Window.Generation == message.WindowGeneration
	if !generationMatches && !faults.AllowWrongRestoreGeneration {
		return restoreRejected
	}
	originMatches := windowMatches(state.Window, message.Actor, message.TombstoneOperation)
	if !originMatches && !faults.AllowWrongOriginRestore {
		return restoreRejected
	}
	if !state.Window.Present || state.Window.Consumed || state.Root != RootTombstoned || state.Namespace != NamespaceMatching {
		return restoreRejected
	}
	if state.Now >= state.Window.Deadline && !faults.AllowExpiredRestore {
		return restoreRejected
	}
	if state.BackendFrontier < message.RequiredFrontier && !faults.AllowRestoreWithoutFrontier {
		return restoreRejected
	}

	operation := message.TombstoneOperation
	state.Root = RootActive
	state.Window.Consumed = true
	state.Window.RestoreOperation = message.RestoreOperation
	if !generationMatches {
		state.UnsafeRestoreGeneration = true
	}
	state.RestoreCommits[operation.Origin][operation.Sequence]++
	state.CommitRequired[operation.Origin][operation.Sequence] = message.RequiredFrontier
	state.CommitBackend[operation.Origin][operation.Sequence] = state.BackendFrontier
	state.CommitActor[operation.Origin][operation.Sequence] = message.Actor
	state.CommitAt[operation.Origin][operation.Sequence] = state.Now
	state.CommitDeadline[operation.Origin][operation.Sequence] = state.Window.Deadline
	return restoreCommitted
}

func clearWorkflow(runtime *Runtime) {
	runtime.Phase = PhaseIdle
	runtime.WorkflowOperation = Operation{}
	runtime.RestoreOperation = RestoreOperation{}
	runtime.ExpectedWindowGeneration = 0
	runtime.AcceptedWindowGeneration = 0
	runtime.TombstoneAttempts = 0
	runtime.RestoreAttempts = 0
}

func windowMatches(window Window, actor Replica, operation Operation) bool {
	return window.Present && window.Origin == actor && window.Operation == operation
}

func enqueueMessage(state *State, message Message, bounds Bounds) bool {
	if state.MessageCount >= bounds.MaxMessages {
		return false
	}
	state.Messages[state.MessageCount] = message
	state.MessageCount++
	sort.Slice(state.Messages[:state.MessageCount], func(i, j int) bool {
		return messageLess(state.Messages[i], state.Messages[j])
	})
	return true
}

func dequeueMessage(state *State, index uint8) (Message, bool) {
	if index >= state.MessageCount {
		return Message{}, false
	}
	message := state.Messages[index]
	for i := index; i+1 < state.MessageCount; i++ {
		state.Messages[i] = state.Messages[i+1]
	}
	state.MessageCount--
	state.Messages[state.MessageCount] = Message{}
	return message, true
}

func messageLess(left, right Message) bool {
	leftKey := []uint8{uint8(left.Kind), uint8(left.Actor), uint8(left.TombstoneOperation.Origin), left.TombstoneOperation.Sequence, uint8(left.RestoreOperation.Origin), left.RestoreOperation.Sequence, left.ExpectedWindowGeneration, left.WindowGeneration, left.RequiredFrontier}
	rightKey := []uint8{uint8(right.Kind), uint8(right.Actor), uint8(right.TombstoneOperation.Origin), right.TombstoneOperation.Sequence, uint8(right.RestoreOperation.Origin), right.RestoreOperation.Sequence, right.ExpectedWindowGeneration, right.WindowGeneration, right.RequiredFrontier}
	for i := range leftKey {
		if leftKey[i] != rightKey[i] {
			return leftKey[i] < rightKey[i]
		}
	}
	return false
}

// ValidateState contains the safety invariants checked after every transition.
func ValidateState(state State, bounds Bounds) error {
	if err := bounds.Validate(); err != nil {
		return err
	}
	if state.Now > bounds.MaxTime {
		return fmt.Errorf("time %d exceeds bound %d", state.Now, bounds.MaxTime)
	}
	if state.Root > RootTombstoned || state.Namespace > NamespaceConflicting || state.BackendFrontier > bounds.MaxFrontier {
		return errors.New("state enum or frontier is out of bounds")
	}
	if state.MessageCount > bounds.MaxMessages {
		return fmt.Errorf("message count %d exceeds bound %d", state.MessageCount, bounds.MaxMessages)
	}
	for i := uint8(1); i < state.MessageCount; i++ {
		if messageLess(state.Messages[i], state.Messages[i-1]) {
			return errors.New("message queue is not canonical")
		}
	}
	for i := state.MessageCount; i < maxMessageLimit; i++ {
		if state.Messages[i] != (Message{}) {
			return errors.New("message queue has nonzero tail")
		}
	}

	if !state.Window.Present {
		if state.Window != (Window{}) {
			return errors.New("absent window carries state")
		}
	} else {
		if state.Window.Generation == 0 || state.Window.Generation > bounds.MaxWindowGeneration || !state.Window.Operation.Valid(bounds) || state.Window.Origin != state.Window.Operation.Origin {
			return errors.New("current window identity is invalid")
		}
		if state.Window.Consumed && !state.Window.RestoreOperation.Valid(bounds) {
			return errors.New("consumed window has no restore operation")
		}
		if !state.Window.Consumed && state.Window.RestoreOperation.Sequence != 0 {
			return errors.New("open window carries a restore operation")
		}
	}

	if state.UnsafeProjection {
		return errors.New("stale or unsafe projection completed")
	}
	if state.UnsafeRestoreGeneration {
		return errors.New("restore committed for the wrong window generation")
	}
	if state.LostDurableIntent {
		return errors.New("crash/restart lost a durable workflow")
	}

	for replica := ReplicaA; replica < replicaCount; replica++ {
		runtime := state.Runtime[replica]
		if runtime.Phase > PhaseProjectionPending || runtime.Occupant > OccupantNonContent || runtime.ExpectedWindowGeneration > bounds.MaxWindowGeneration || runtime.AcceptedWindowGeneration > bounds.MaxWindowGeneration || runtime.LocalFrontier > bounds.MaxFrontier || runtime.MergedFrontier > runtime.LocalFrontier || runtime.RequiredFrontier > runtime.MergedFrontier || runtime.Restarts > bounds.MaxRestarts || runtime.TombstoneAttempts > bounds.MaxAttempts || runtime.RestoreAttempts > bounds.MaxAttempts {
			return fmt.Errorf("runtime %s is out of bounds", replica)
		}
		if runtime.NextSequence == 0 || runtime.NextSequence > bounds.MaxSequence+1 {
			return fmt.Errorf("runtime %s next sequence %d is invalid", replica, runtime.NextSequence)
		}
		if runtime.NextRestoreSequence == 0 || runtime.NextRestoreSequence > bounds.MaxSequence+1 {
			return fmt.Errorf("runtime %s next restore sequence %d is invalid", replica, runtime.NextRestoreSequence)
		}
		if runtime.Phase == PhaseIdle {
			if runtime.WorkflowOperation.Sequence != 0 || runtime.RestoreOperation.Sequence != 0 {
				return fmt.Errorf("idle runtime %s retained workflow %s/%s", replica, runtime.WorkflowOperation, runtime.RestoreOperation)
			}
		} else if !runtime.WorkflowOperation.Valid(bounds) || runtime.WorkflowOperation.Origin != replica {
			return fmt.Errorf("runtime %s has invalid workflow %s", replica, runtime.WorkflowOperation)
		}
		if runtime.RestoreOperation.Sequence != 0 && (!runtime.RestoreOperation.Valid(bounds) || runtime.RestoreOperation.Origin != replica) {
			return fmt.Errorf("runtime %s has invalid restore operation %s", replica, runtime.RestoreOperation)
		}
		if runtime.Phase == PhaseRestorePending || runtime.Phase == PhaseProjectionPending {
			if !runtime.RestoreOperation.Valid(bounds) {
				return fmt.Errorf("runtime %s has invalid restore operation %s", replica, runtime.RestoreOperation)
			}
		} else if runtime.Phase != PhaseContentSyncing && runtime.RestoreOperation.Sequence != 0 {
			return fmt.Errorf("runtime %s retained restore operation %s in %s", replica, runtime.RestoreOperation, runtime.Phase)
		}
		if runtime.Phase == PhaseWindowOpen || runtime.Phase == PhaseContentSyncing || runtime.Phase == PhaseRestorePending || runtime.Phase == PhaseProjectionPending {
			op := runtime.WorkflowOperation
			if state.TombstoneAccepts[op.Origin][op.Sequence] == 0 {
				return fmt.Errorf("runtime %s entered %s without an accepted tombstone", replica, runtime.Phase)
			}
			if runtime.AcceptedWindowGeneration != runtime.ExpectedWindowGeneration+1 {
				return fmt.Errorf("runtime %s did not retain its accepted window generation", replica)
			}
		} else if runtime.AcceptedWindowGeneration != 0 {
			return fmt.Errorf("runtime %s retained an unobserved window generation", replica)
		}
		if runtime.Phase == PhaseProjectionPending {
			op := runtime.WorkflowOperation
			if state.RestoreCommits[op.Origin][op.Sequence] == 0 {
				return fmt.Errorf("runtime %s entered projection without a restore commit", replica)
			}
		}
	}

	for replica := ReplicaA; replica < replicaCount; replica++ {
		for sequence := uint8(1); sequence <= bounds.MaxSequence; sequence++ {
			accepts := state.TombstoneAccepts[replica][sequence]
			commits := state.RestoreCommits[replica][sequence]
			if accepts > 1 {
				return fmt.Errorf("tombstone operation %s%d was accepted %d times", replica, sequence, accepts)
			}
			if commits > 1 {
				return fmt.Errorf("restore operation %s%d committed %d times", replica, sequence, commits)
			}
			if commits == 1 {
				if accepts != 1 {
					return fmt.Errorf("restore %s%d committed without one tombstone acceptance", replica, sequence)
				}
				if state.CommitActor[replica][sequence] != replica {
					return fmt.Errorf("restore %s%d committed for wrong actor %s", replica, sequence, state.CommitActor[replica][sequence])
				}
				if state.CommitBackend[replica][sequence] < state.CommitRequired[replica][sequence] {
					return fmt.Errorf("restore %s%d committed before frontier proof", replica, sequence)
				}
				if state.CommitAt[replica][sequence] >= state.CommitDeadline[replica][sequence] {
					return fmt.Errorf("restore %s%d committed at or after deadline", replica, sequence)
				}
			}
		}
	}

	return nil
}

// TraceStep is the semantic event and exact abstract state observed after it.
type TraceStep struct {
	Event Event
	After State
}

func ValidateTrace(bounds Bounds, initial State, steps []TraceStep) error {
	current := initial
	if err := ValidateState(current, bounds); err != nil {
		return fmt.Errorf("initial state: %w", err)
	}
	for index, step := range steps {
		next, enabled, err := Apply(current, step.Event, bounds)
		if err != nil {
			return fmt.Errorf("step %d %s: %w", index, step.Event, err)
		}
		if !enabled {
			return fmt.Errorf("step %d %s is not admitted from %s", index, step.Event, StateSummary(current))
		}
		if next != step.After {
			return fmt.Errorf("step %d %s diverged\nwant: %s\n got: %s", index, step.Event, StateSummary(next), StateSummary(step.After))
		}
		current = next
	}
	return nil
}

func StateSummary(state State) string {
	runtimes := make([]string, 0, replicaCount)
	for replica := ReplicaA; replica < replicaCount; replica++ {
		runtime := state.Runtime[replica]
		runtimes = append(runtimes, fmt.Sprintf("%s:%s/%s/tombstone=%s/restore=%s/expected=%d/accepted=%d/local=%d/merged=%d/required=%d", replica, runtime.Phase, runtime.Occupant, runtime.WorkflowOperation, runtime.RestoreOperation, runtime.ExpectedWindowGeneration, runtime.AcceptedWindowGeneration, runtime.LocalFrontier, runtime.MergedFrontier, runtime.RequiredFrontier))
	}
	window := "none(gen=0)"
	if state.Window.Present {
		window = fmt.Sprintf("gen=%d/op=%s/origin=%s/deadline=%d/consumed=%t", state.Window.Generation, state.Window.Operation, state.Window.Origin, state.Window.Deadline, state.Window.Consumed)
	}
	return fmt.Sprintf("t=%d root=%s namespace=%s window={%s} backend=%d runtimes=[%s] messages=%d", state.Now, state.Root, state.Namespace, window, state.BackendFrontier, strings.Join(runtimes, ", "), state.MessageCount)
}

func CandidateEvents(state State, bounds Bounds) []Event {
	events := make([]Event, 0, 48)
	for replica := ReplicaA; replica < replicaCount; replica++ {
		for _, kind := range []EventKind{
			EventObserveContent,
			EventObserveAbsent,
			EventObserveNonContent,
			EventBeginTombstone,
			EventQueueTombstone,
			EventObserveWindow,
			EventRebaseTombstone,
			EventMutateContent,
			EventMergeContent,
			EventSyncBackend,
			EventObserveFrontier,
			EventQueueRestore,
			EventObserveRestore,
			EventFinishProjection,
			EventFreshDeleteAfterProof,
			EventObserveExpiry,
			EventObserveSuperseded,
			EventCrashRestart,
		} {
			events = append(events, Event{Kind: kind, Replica: replica})
		}
	}
	for index := uint8(0); index < state.MessageCount; index++ {
		events = append(events, Event{Kind: EventDeliverMessage, MessageIndex: index})
	}
	events = append(events,
		Event{Kind: EventAdvanceTime},
		Event{Kind: EventChangeNamespace, Namespace: NamespaceChanged},
		Event{Kind: EventChangeNamespace, Namespace: NamespaceConflicting},
		Event{Kind: EventExternalTombstone},
	)
	return events
}
