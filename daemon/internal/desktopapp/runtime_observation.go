package desktopapp

import (
	"log"

	"notty/daemon/internal/syncer"
)

// desktopRuntimeObserver appends a fixed, token-free lifecycle schema to the
// desktop log. The service generation is captured when the controller creates
// the service, so native acceptance can correlate process and turn evidence to
// the existing controller-generation records without parsing provider logs.
type desktopRuntimeObserver struct {
	serviceGeneration uint64
	logger            *log.Logger
}

func (o desktopRuntimeObserver) ObserveRuntime(observation syncer.RuntimeObservation) {
	if o.logger == nil {
		return
	}
	o.logger.Printf(
		"runtime service_generation=%d observation_sequence=%d runtime_generation=%d kind=%s pid=%d turn_sequence=%d state=%s",
		o.serviceGeneration,
		observation.Sequence,
		observation.RuntimeGeneration,
		observation.RuntimeKind,
		observation.PID,
		observation.TurnSequence,
		observation.State,
	)
}
