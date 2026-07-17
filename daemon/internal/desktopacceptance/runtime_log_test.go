package desktopacceptance

import (
	"strings"
	"testing"
)

func TestParseRuntimeLogOrdersServiceRuntimeAndPIDReuse(t *testing.T) {
	data := []byte(strings.Join([]string{
		"codesk-desktop: 2026/07/17 07:00:00 runtime service_generation=1 observation_sequence=1 runtime_generation=1 kind=codex pid=41 turn_sequence=0 state=ready",
		"codesk-desktop: 2026/07/17 07:00:01 runtime service_generation=1 observation_sequence=2 runtime_generation=1 kind=codex pid=41 turn_sequence=1 state=turn_started",
		"codesk-desktop: 2026/07/17 07:00:02 runtime service_generation=1 observation_sequence=3 runtime_generation=1 kind=codex pid=41 turn_sequence=1 state=turn_completed",
		"codesk-desktop: 2026/07/17 07:00:03 runtime service_generation=1 observation_sequence=4 runtime_generation=2 kind=codex pid=41 turn_sequence=0 state=ready",
		"codesk-desktop: 2026/07/17 07:00:04 runtime service_generation=1 observation_sequence=5 runtime_generation=1 kind=codex pid=41 turn_sequence=0 state=stopped_transient",
		"codesk-desktop: 2026/07/17 07:00:05 runtime service_generation=1 observation_sequence=6 runtime_generation=2 kind=codex pid=41 turn_sequence=1 state=turn_started",
		"codesk-desktop: 2026/07/17 07:00:06 runtime service_generation=1 observation_sequence=7 runtime_generation=2 kind=codex pid=41 turn_sequence=1 state=turn_completed",
	}, "\n"))
	state := ParseRuntimeLog(data)
	if state.ServiceGeneration != 1 || state.ObservationSequence != 7 || state.Generation != 2 || state.PID != 41 ||
		state.TurnStartedSequence != 1 || state.TurnTerminalSequence != 1 || state.TurnStatus != "turn_completed" {
		t.Fatalf("state = %+v", state)
	}
}

func TestParseRuntimeLogAcceptsObservationResetForNewService(t *testing.T) {
	data := []byte(strings.Join([]string{
		"runtime service_generation=1 observation_sequence=99 runtime_generation=8 kind=codex pid=41 turn_sequence=7 state=turn_completed",
		"runtime service_generation=2 observation_sequence=1 runtime_generation=1 kind=codex pid=52 turn_sequence=0 state=ready",
		"runtime service_generation=1 observation_sequence=100 runtime_generation=9 kind=codex pid=53 turn_sequence=8 state=turn_completed",
		"runtime service_generation=2 observation_sequence=2 runtime_generation=1 kind=codex pid=52 turn_sequence=1 state=turn_started",
		"runtime service_generation=2 observation_sequence=3 runtime_generation=1 kind=codex pid=52 turn_sequence=1 state=turn_completed",
	}, "\n"))
	state := ParseRuntimeLog(data)
	if state.ServiceGeneration != 2 || state.ObservationSequence != 3 || state.Generation != 1 || state.PID != 52 || state.TurnStatus != "turn_completed" {
		t.Fatalf("state = %+v", state)
	}
}

func TestParseRuntimeLogRejectsRegressionsMalformedAndOverflow(t *testing.T) {
	data := []byte(strings.Join([]string{
		"runtime service_generation=1 observation_sequence=1 runtime_generation=1 kind=codex pid=51 turn_sequence=1 state=turn_started",
		"runtime service_generation=1 observation_sequence=2 runtime_generation=1 kind=codex pid=52 turn_sequence=1 state=turn_completed",
		"runtime service_generation=1 observation_sequence=999999999999999999999999 runtime_generation=1 kind=codex pid=51 turn_sequence=1 state=turn_completed",
		"runtime service_generation=1 observation_sequence=2 runtime_generation=1 kind=codex pid=51 turn_sequence=1 state=turn_completed extra=payload",
		"runtime service_generation=1 observation_sequence=3 runtime_generation=1 kind=codex pid=51 turn_sequence=1 state=turn_completed",
	}, "\n"))
	state := ParseRuntimeLog(data)
	if state.ObservationSequence != 3 || state.PID != 51 || state.TurnStatus != "turn_completed" {
		t.Fatalf("state = %+v", state)
	}
}

func TestParseRuntimeLogRejectsTerminalWithoutMatchingStart(t *testing.T) {
	data := []byte(strings.Join([]string{
		"runtime service_generation=1 observation_sequence=1 runtime_generation=1 kind=codex pid=51 turn_sequence=1 state=turn_started",
		"runtime service_generation=1 observation_sequence=2 runtime_generation=1 kind=codex pid=51 turn_sequence=2 state=turn_completed",
	}, "\n"))
	state := ParseRuntimeLog(data)
	if state.TurnStartedSequence != 1 || state.TurnTerminalSequence != 0 || state.TurnStatus != "" {
		t.Fatalf("state = %+v, want unmatched terminal ignored", state)
	}
}

func TestParseServiceGenerationLogUsesLastValidOnlineLine(t *testing.T) {
	data := []byte("codesk-desktop: 2026/07/17 service generation=1 state=online sequence=2\r\n" +
		"service generation=2 state=starting sequence=3\n" +
		"service generation=2 state=online sequence=4\n" +
		"service generation=1 state=online sequence=99\n")
	if got := ParseServiceGenerationLog(data); got != 2 {
		t.Fatalf("generation = %d, want 2", got)
	}
}
