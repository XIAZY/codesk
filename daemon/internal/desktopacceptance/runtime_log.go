package desktopacceptance

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	serviceGenerationPattern  = regexp.MustCompile(`^service generation=([0-9]+) state=online sequence=[0-9]+$`)
	runtimeObservationPattern = regexp.MustCompile(`^runtime service_generation=([0-9]+) observation_sequence=([0-9]+) runtime_generation=([0-9]+) kind=(codex|claude-code) pid=([0-9]+) turn_sequence=([0-9]+) state=(ready|turn_started|turn_completed|turn_failed|turn_idle|stopped_expected|stopped_transient|stopped_terminal)$`)
)

// ParseServiceGenerationLog returns the most recently emitted online service
// generation from one desktop log epoch.
func ParseServiceGenerationLog(data []byte) uint64 {
	var generation uint64
	for _, line := range logPayloadLines(data) {
		match := serviceGenerationPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value, err := strconv.ParseUint(match[1], 10, 64)
		if err == nil && value > generation {
			generation = value
		}
	}
	return generation
}

// ParseRuntimeLog reduces the fixed token-free runtime observation schema into
// the latest coherent state. Service generation is the outer ordering domain;
// observation sequence orders events within it, and runtime generation prevents
// a late stop from an older process from replacing a newer runtime (including
// when the operating system reuses the PID).
func ParseRuntimeLog(data []byte) RuntimeState {
	var state RuntimeState
	for _, line := range logPayloadLines(data) {
		match := runtimeObservationPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		serviceGeneration, serviceErr := strconv.ParseUint(match[1], 10, 64)
		observationSequence, observationErr := strconv.ParseUint(match[2], 10, 64)
		runtimeGeneration, runtimeErr := strconv.ParseUint(match[3], 10, 64)
		pidValue, pidErr := strconv.ParseUint(match[5], 10, 31)
		turnSequence, turnErr := strconv.ParseUint(match[6], 10, 64)
		if serviceErr != nil || observationErr != nil || runtimeErr != nil || pidErr != nil || turnErr != nil ||
			serviceGeneration == 0 || observationSequence == 0 || runtimeGeneration == 0 || pidValue == 0 {
			continue
		}

		newService := serviceGeneration > state.ServiceGeneration
		if serviceGeneration < state.ServiceGeneration ||
			!newService && observationSequence <= state.ObservationSequence ||
			!newService && runtimeGeneration < state.Generation ||
			!newService && runtimeGeneration == state.Generation && state.PID != 0 && int(pidValue) != state.PID {
			continue
		}
		newRuntime := newService || runtimeGeneration > state.Generation
		if newRuntime {
			state = RuntimeState{}
		}
		state.Kind = match[4]
		state.PID = int(pidValue)
		state.ServiceGeneration = serviceGeneration
		state.ObservationSequence = observationSequence
		state.Generation = runtimeGeneration

		switch eventState := match[7]; eventState {
		case "turn_started":
			if turnSequence > state.TurnStartedSequence {
				state.TurnStartedSequence = turnSequence
				state.TurnStatus = ""
			}
		case "turn_completed", "turn_failed":
			if turnSequence > 0 && turnSequence <= state.TurnStartedSequence && turnSequence >= state.TurnTerminalSequence {
				state.TurnTerminalSequence = turnSequence
				state.TurnStatus = eventState
			}
		case "stopped_expected", "stopped_transient", "stopped_terminal":
			state.TurnStatus = eventState
		}
	}
	return state
}

func logPayloadLines(data []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for index, line := range lines {
		if marker := strings.Index(line, "service generation="); marker >= 0 {
			lines[index] = line[marker:]
			continue
		}
		if marker := strings.Index(line, "runtime service_generation="); marker >= 0 {
			lines[index] = line[marker:]
		}
	}
	return lines
}
