package desktopapp

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"notty/daemon/internal/syncer"
)

func TestDesktopRuntimeObserverWritesFixedTokenFreeSchema(t *testing.T) {
	var output bytes.Buffer
	observer := desktopRuntimeObserver{
		serviceGeneration: 7,
		logger:            log.New(&output, "codesk-desktop: ", 0),
	}
	observer.ObserveRuntime(syncer.RuntimeObservation{
		Sequence:          11,
		RuntimeGeneration: 3,
		RuntimeKind:       syncer.RuntimeCodex,
		PID:               4242,
		TurnSequence:      2,
		State:             syncer.RuntimeObservationTurnCompleted,
	})

	want := "codesk-desktop: runtime service_generation=7 observation_sequence=11 runtime_generation=3 kind=codex pid=4242 turn_sequence=2 state=turn_completed\n"
	if got := output.String(); got != want {
		t.Fatalf("desktop runtime observation = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"agent_id", "session_id", "turn_id", "token", "prompt", "provider_output"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("desktop runtime observation schema contains forbidden field %q", forbidden)
		}
	}
}

func TestDesktopRuntimeObserverAllowsNoLogger(t *testing.T) {
	desktopRuntimeObserver{}.ObserveRuntime(syncer.RuntimeObservation{})
}
