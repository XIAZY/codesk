package desktop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"notty/daemon/internal/syncer"
)

const (
	defaultInitialRetryDelay = time.Second
	defaultMaximumRetryDelay = 30 * time.Second
)

var (
	ErrControllerAlreadyStarted = errors.New("desktop: controller already started")
	ErrControllerNotStarted     = errors.New("desktop: controller not started")
	ErrControllerStopped        = errors.New("desktop: controller stopped")
)

type State uint8

const (
	StateStarting State = iota
	StateOnline
	StateRetrying
	StateReconnectRequired
	StateQuitting
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateOnline:
		return "online"
	case StateRetrying:
		return "retrying"
	case StateReconnectRequired:
		return "reconnect-required"
	case StateQuitting:
		return "quitting"
	default:
		return fmt.Sprintf("state(%d)", s)
	}
}

type Snapshot struct {
	State        State
	Generation   uint64
	RetryAttempt int
	RetryDelay   time.Duration
	Sequence     uint64
}

type DaemonService interface {
	Run(context.Context) error
	Ready() <-chan struct{}
}

type ServiceFactory func() (DaemonService, error)

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Clock interface {
	NewTimer(time.Duration) Timer
}

type ControllerOptions struct {
	Clock             Clock
	InitialRetryDelay time.Duration
	MaximumRetryDelay time.Duration
}

type Controller struct {
	factory ServiceFactory
	clock   Clock
	backoff retryBackoff

	commands chan controllerCommand
	done     chan struct{}
	updates  chan Snapshot

	mu       sync.RWMutex
	snapshot Snapshot
	started  bool
	finished bool
	finish   sync.Once
}

type commandKind uint8

const (
	commandRestart commandKind = iota
	commandQuit
)

type controllerCommand struct {
	kind commandKind
	ack  chan error
}

type serviceGeneration struct {
	cancel context.CancelFunc
	ready  <-chan struct{}
	done   <-chan error
}

type retryBackoff struct {
	initial time.Duration
	maximum time.Duration
}

func (b retryBackoff) delay(attempt int) time.Duration {
	delay := b.initial
	for i := 1; i < attempt && delay < b.maximum; i++ {
		if delay > b.maximum/2 {
			return b.maximum
		}
		delay *= 2
	}
	if delay > b.maximum {
		return b.maximum
	}
	return delay
}

type wallClock struct{}

func (wallClock) NewTimer(delay time.Duration) Timer {
	return wallTimer{Timer: time.NewTimer(delay)}
}

type wallTimer struct {
	*time.Timer
}

func (t wallTimer) C() <-chan time.Time {
	return t.Timer.C
}

func NewController(factory ServiceFactory, options ControllerOptions) (*Controller, error) {
	if factory == nil {
		return nil, errors.New("desktop: service factory is required")
	}
	if options.Clock == nil {
		options.Clock = wallClock{}
	}
	if options.InitialRetryDelay == 0 {
		options.InitialRetryDelay = defaultInitialRetryDelay
	}
	if options.MaximumRetryDelay == 0 {
		options.MaximumRetryDelay = defaultMaximumRetryDelay
	}
	if options.InitialRetryDelay < 0 {
		return nil, errors.New("desktop: initial retry delay must be positive")
	}
	if options.MaximumRetryDelay < options.InitialRetryDelay {
		return nil, errors.New("desktop: maximum retry delay must be at least the initial delay")
	}

	return &Controller{
		factory:  factory,
		clock:    options.Clock,
		backoff:  retryBackoff{initial: options.InitialRetryDelay, maximum: options.MaximumRetryDelay},
		commands: make(chan controllerCommand, 4),
		done:     make(chan struct{}),
		updates:  make(chan Snapshot, 16),
		snapshot: Snapshot{State: StateStarting},
	}, nil
}

func (c *Controller) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return ErrControllerStopped
	}
	if c.started {
		return ErrControllerAlreadyStarted
	}
	c.started = true
	go c.run(ctx)
	return nil
}

func (c *Controller) Restart() error {
	return c.sendCommand(commandRestart)
}

func (c *Controller) Quit() error {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return nil
	}
	if !c.started {
		// Reserve the controller before releasing the lock so Start cannot race
		// a pre-start Quit and launch an unowned event loop.
		c.started = true
		c.mu.Unlock()
		c.publish(StateQuitting, 0, 0, 0)
		c.finishRun()
		return nil
	}
	c.mu.Unlock()

	err := c.sendCommand(commandQuit)
	if errors.Is(err, ErrControllerStopped) {
		return nil
	}
	<-c.done
	return err
}

func (c *Controller) Done() <-chan struct{} {
	return c.done
}

// Updates carries state changes without blocking the controller. A slow
// consumer may observe coalesced changes and should always reconcile from
// Snapshot after receiving a notification.
func (c *Controller) Updates() <-chan Snapshot {
	return c.updates
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot
}

func (c *Controller) sendCommand(kind commandKind) error {
	c.mu.RLock()
	started := c.started
	finished := c.finished
	c.mu.RUnlock()
	if finished {
		return ErrControllerStopped
	}
	if !started {
		return ErrControllerNotStarted
	}

	command := controllerCommand{kind: kind, ack: make(chan error, 1)}
	select {
	case c.commands <- command:
	case <-c.done:
		return ErrControllerStopped
	}
	select {
	case err := <-command.ack:
		return err
	case <-c.done:
		select {
		case err := <-command.ack:
			return err
		default:
			return ErrControllerStopped
		}
	}
}

func (c *Controller) run(parent context.Context) {
	defer c.finishRun()

	var generation *serviceGeneration
	var generationSequence uint64
	var currentGeneration uint64
	var retry Timer
	var retryC <-chan time.Time
	failures := 0
	wantStart := true

	stopRetry := func() {
		if retry != nil {
			retry.Stop()
			retry = nil
			retryC = nil
		}
	}
	stopGeneration := func() error {
		if generation == nil {
			return nil
		}
		generation.cancel()
		err := <-generation.done
		generation = nil
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	publishFailure := func(err error) {
		var reconnectErr *syncer.ReconnectRequiredError
		if errors.As(err, &reconnectErr) {
			failures = 0
			c.publish(StateReconnectRequired, currentGeneration, 0, 0)
			return
		}
		failures++
		delay := c.backoff.delay(failures)
		retry = c.clock.NewTimer(delay)
		retryC = retry.C()
		c.publish(StateRetrying, currentGeneration, failures, delay)
	}
	quit := func() error {
		c.publish(StateQuitting, currentGeneration, 0, 0)
		stopRetry()
		return stopGeneration()
	}
	handleCommand := func(command controllerCommand) bool {
		switch command.kind {
		case commandRestart:
			stopRetry()
			err := stopGeneration()
			failures = 0
			wantStart = true
			command.ack <- err
			return false
		case commandQuit:
			command.ack <- quit()
			return true
		default:
			command.ack <- errors.New("desktop: unknown controller command")
			return false
		}
	}

	for {
		if parent.Err() != nil {
			_ = quit()
			return
		}

		if wantStart {
			select {
			case command := <-c.commands:
				if handleCommand(command) {
					return
				}
				continue
			default:
			}

			wantStart = false
			generationSequence++
			currentGeneration = generationSequence
			c.publish(StateStarting, currentGeneration, 0, 0)
			service, err := c.factory()
			if err != nil {
				publishFailure(err)
				continue
			}
			if service == nil {
				publishFailure(errors.New("desktop: service factory returned nil service"))
				continue
			}
			ready := service.Ready()
			if ready == nil {
				publishFailure(errors.New("desktop: service returned nil readiness signal"))
				continue
			}
			generationCtx, cancel := context.WithCancel(parent)
			done := make(chan error, 1)
			generation = &serviceGeneration{cancel: cancel, ready: ready, done: done}
			go func() {
				done <- service.Run(generationCtx)
				close(done)
			}()
		}

		var ready <-chan struct{}
		var serviceDone <-chan error
		if generation != nil {
			ready = generation.ready
			serviceDone = generation.done
		}

		select {
		case <-parent.Done():
			_ = quit()
			return
		case command := <-c.commands:
			if handleCommand(command) {
				return
			}
		case <-retryC:
			stopRetry()
			wantStart = true
		case err := <-serviceDone:
			generation.cancel()
			generation = nil
			if parent.Err() != nil {
				_ = quit()
				return
			}
			publishFailure(err)
		case <-ready:
			generation.ready = nil
			select {
			case err := <-generation.done:
				generation.cancel()
				generation = nil
				if parent.Err() != nil {
					_ = quit()
					return
				}
				publishFailure(err)
			default:
				failures = 0
				c.publish(StateOnline, currentGeneration, 0, 0)
			}
		}
	}
}

func (c *Controller) publish(state State, generation uint64, retryAttempt int, retryDelay time.Duration) {
	c.mu.Lock()
	c.snapshot = Snapshot{
		State:        state,
		Generation:   generation,
		RetryAttempt: retryAttempt,
		RetryDelay:   retryDelay,
		Sequence:     c.snapshot.Sequence + 1,
	}
	snapshot := c.snapshot
	c.mu.Unlock()

	select {
	case c.updates <- snapshot:
	default:
		select {
		case <-c.updates:
		default:
		}
		select {
		case c.updates <- snapshot:
		default:
		}
	}
}

func (c *Controller) finishRun() {
	c.finish.Do(func() {
		c.mu.Lock()
		c.finished = true
		c.mu.Unlock()
		close(c.updates)
		close(c.done)
	})
}
