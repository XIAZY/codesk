package syncer

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"time"
)

const (
	agentRuntimeRestartBaseDelay = 100 * time.Millisecond
	agentRuntimeRestartMaxDelay  = 5 * time.Second
	agentRuntimeStableWindow     = 30 * time.Second
)

func (s *Service) syncAgentRuntimes(ctx context.Context, workspace *workspaceResponse) error {
	if s.agentRuntimes == nil {
		s.agentRuntimes = map[string]*managedWorkspaceRuntime{}
	}
	agents := []*agent(nil)
	if workspace != nil {
		agents = workspace.Agents
	}
	desired := make(map[string]*agent, len(agents))
	for _, current := range agents {
		if current == nil {
			continue
		}
		desired[current.ID] = current
	}

	s.mu.Lock()
	existing := make([]*workspaceRuntime, 0, len(desired))
	retired := make([]*managedWorkspaceRuntime, 0)
	now := time.Now()
	for agentID := range desired {
		currentAgent := desired[agentID]
		restartAttempt := 0
		if managed, ok := s.agentRuntimes[agentID]; ok && managed != nil && managed.runtime != nil {
			if managed.runtime.replica != nil && managed.runtime.replica.actorID == currentAgent.ID {
				if !managed.stopped() {
					existing = append(existing, managed.runtime)
					continue
				}
				runErr, stoppedAt := managed.result()
				delay := managedWorkspaceRuntimeRestartDelay(managed.restartAttempt, runErr)
				if delay > 0 && !stoppedAt.IsZero() && now.Before(stoppedAt.Add(delay)) {
					log.Printf(
						"agent workspace runtime %s stopped (%s): %v; retrying in %s",
						agentID,
						managedWorkspaceRuntimeExitClass(runErr),
						runErr,
						stoppedAt.Add(delay).Sub(now).Round(time.Millisecond),
					)
					continue
				}
				restartAttempt = managed.nextRestartAttempt()
				log.Printf(
					"agent workspace runtime %s stopped (%s): %v; restarting",
					agentID,
					managedWorkspaceRuntimeExitClass(runErr),
					runErr,
				)
			}
			if managed.cancel != nil {
				managed.cancel()
			}
			retired = append(retired, managed)
			delete(s.agentRuntimes, agentID)
		}
		rootDir := agentWorkspacePath(s.cfg, agentID)
		runtime, err := newWorkspaceRuntime(s.cfg, s.client, rootDir, currentAgent.ID, "agent")
		if err != nil {
			s.mu.Unlock()
			return errors.Join(err, closeManagedWorkspaceRuntimes(retired))
		}
		runtime.initialWorkspace = workspace
		s.agentRuntimes[agentID] = startManagedWorkspaceRuntimeAttempt(ctx, runtime, restartAttempt)
	}

	staleIDs := make([]string, 0)
	for agentID := range s.agentRuntimes {
		if _, ok := desired[agentID]; !ok {
			staleIDs = append(staleIDs, agentID)
		}
	}
	sort.Strings(staleIDs)
	stale := make([]*managedWorkspaceRuntime, 0, len(staleIDs))
	for _, agentID := range staleIDs {
		stale = append(stale, s.agentRuntimes[agentID])
		delete(s.agentRuntimes, agentID)
	}
	s.mu.Unlock()

	var errs []error
	errs = append(errs, closeManagedWorkspaceRuntimes(retired))
	for _, runtime := range existing {
		if err := runtime.applyWorkspace(ctx, workspace); err != nil {
			errs = append(errs, err)
		}
	}
	for index, agentID := range staleIDs {
		errs = append(errs, closeManagedWorkspaceRuntime(stale[index]))
		if err := os.RemoveAll(agentWorkspacePath(s.cfg, agentID)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) closeAgentRuntimes() {
	s.mu.Lock()
	runtimes := make([]*managedWorkspaceRuntime, 0, len(s.agentRuntimes))
	for _, runtime := range s.agentRuntimes {
		runtimes = append(runtimes, runtime)
	}
	s.agentRuntimes = map[string]*managedWorkspaceRuntime{}
	s.mu.Unlock()
	_ = closeManagedWorkspaceRuntimes(runtimes)
}

func startManagedWorkspaceRuntime(ctx context.Context, runtime *workspaceRuntime) *managedWorkspaceRuntime {
	return startManagedWorkspaceRuntimeAttempt(ctx, runtime, 0)
}

func startManagedWorkspaceRuntimeAttempt(
	ctx context.Context,
	runtime *workspaceRuntime,
	restartAttempt int,
) *managedWorkspaceRuntime {
	runtimeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	managed := &managedWorkspaceRuntime{
		runtime:        runtime,
		cancel:         cancel,
		done:           done,
		startedAt:      time.Now(),
		restartAttempt: restartAttempt,
	}
	go func() {
		runErr := runtime.run(runtimeCtx, nil)
		managed.recordResult(runErr)
		close(done)
	}()
	return managed
}

func (runtime *managedWorkspaceRuntime) stopped() bool {
	if runtime == nil || runtime.done == nil {
		return false
	}
	select {
	case <-runtime.done:
		return true
	default:
		return false
	}
}

func (runtime *managedWorkspaceRuntime) recordResult(err error) {
	if runtime == nil {
		return
	}
	runtime.resultMu.Lock()
	runtime.runErr = err
	runtime.stoppedAt = time.Now()
	runtime.resultMu.Unlock()
}

func (runtime *managedWorkspaceRuntime) result() (error, time.Time) {
	if runtime == nil {
		return nil, time.Time{}
	}
	runtime.resultMu.Lock()
	defer runtime.resultMu.Unlock()
	return runtime.runErr, runtime.stoppedAt
}

func (runtime *managedWorkspaceRuntime) nextRestartAttempt() int {
	if runtime == nil {
		return 0
	}
	_, stoppedAt := runtime.result()
	if !runtime.startedAt.IsZero() && !stoppedAt.IsZero() && stoppedAt.Sub(runtime.startedAt) >= agentRuntimeStableWindow {
		return 0
	}
	return runtime.restartAttempt + 1
}

func managedWorkspaceRuntimeExitClass(err error) string {
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return "stopped"
	case isTerminalAuthError(err):
		return "terminal-auth"
	default:
		return "runtime-failure"
	}
}

func managedWorkspaceRuntimeRestartDelay(attempt int, err error) time.Duration {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	if isTerminalAuthError(err) {
		return agentRuntimeRestartMaxDelay
	}
	if attempt <= 0 {
		return 0
	}
	delay := agentRuntimeRestartBaseDelay
	for current := 1; current < attempt && delay < agentRuntimeRestartMaxDelay; current++ {
		delay *= 2
	}
	if delay > agentRuntimeRestartMaxDelay {
		return agentRuntimeRestartMaxDelay
	}
	return delay
}

func closeManagedWorkspaceRuntime(runtime *managedWorkspaceRuntime) error {
	if runtime == nil {
		return nil
	}
	if runtime.cancel != nil {
		runtime.cancel()
	}
	waitManagedWorkspaceRuntime(runtime)
	if runtime.runtime == nil {
		return nil
	}
	return runtime.runtime.Close()
}

func closeManagedWorkspaceRuntimes(runtimes []*managedWorkspaceRuntime) error {
	for _, runtime := range runtimes {
		if runtime != nil && runtime.cancel != nil {
			runtime.cancel()
		}
	}
	var errs []error
	for _, runtime := range runtimes {
		waitManagedWorkspaceRuntime(runtime)
		if runtime != nil && runtime.runtime != nil {
			errs = append(errs, runtime.runtime.Close())
		}
	}
	return errors.Join(errs...)
}

func waitManagedWorkspaceRuntime(runtime *managedWorkspaceRuntime) {
	if runtime == nil || runtime.done == nil {
		return
	}
	<-runtime.done
}
