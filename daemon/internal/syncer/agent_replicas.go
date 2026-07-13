package syncer

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	agentRuntimeRestartBaseDelay = 100 * time.Millisecond
	agentRuntimeRestartMaxDelay  = 5 * time.Second
	agentRuntimeStableWindow     = 30 * time.Second
)

type workspaceRuntimeBorrow struct {
	runtime *workspaceRuntime
	release func()
}

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
	existing := make([]workspaceRuntimeBorrow, 0, len(desired))
	retired := make([]*managedWorkspaceRuntime, 0)
	var setupErr error
	for agentID := range desired {
		currentAgent := desired[agentID]
		managed := s.agentRuntimes[agentID]
		if managed != nil {
			managed.workspace = workspace
		}
		if managed != nil && managed.runtime != nil {
			if managed.runtime.replica != nil && managed.runtime.replica.actorID == currentAgent.ID {
				if managed.stopped() {
					// The completion owner performs the classified restart. A refresh only
					// updates its desired-workspace snapshot and must not bypass backoff.
					continue
				}
				if runtime, release := managed.borrow(); runtime != nil {
					existing = append(existing, workspaceRuntimeBorrow{runtime: runtime, release: release})
					continue
				}
			}
		}
		rootDir := agentWorkspacePath(s.cfg, agentID)
		runtime, err := newWorkspaceRuntime(s.cfg, s.client, rootDir, currentAgent.ID, "agent")
		if err != nil {
			setupErr = err
			break
		}
		runtime.initialWorkspace = workspace
		replacement := s.startAgentWorkspaceRuntimeAttempt(ctx, agentID, runtime, workspace, 0)
		if managed != nil {
			managed.retire()
			if managed.cancel != nil {
				managed.cancel()
			}
			retired = append(retired, managed)
		}
		s.agentRuntimes[agentID] = replacement
	}

	staleIDs := make([]string, 0)
	stale := make([]*managedWorkspaceRuntime, 0)
	if setupErr == nil {
		for agentID := range s.agentRuntimes {
			if _, ok := desired[agentID]; !ok {
				staleIDs = append(staleIDs, agentID)
			}
		}
		sort.Strings(staleIDs)
		stale = make([]*managedWorkspaceRuntime, 0, len(staleIDs))
		for _, agentID := range staleIDs {
			managed := s.agentRuntimes[agentID]
			if managed != nil {
				managed.retire()
			}
			stale = append(stale, managed)
			delete(s.agentRuntimes, agentID)
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, borrowed := range existing {
		if setupErr == nil {
			if err := borrowed.runtime.applyWorkspace(ctx, workspace); err != nil {
				errs = append(errs, err)
			}
		}
		borrowed.release()
	}
	errs = append(errs, closeManagedWorkspaceRuntimes(retired))
	if setupErr != nil {
		return errors.Join(setupErr, errors.Join(errs...))
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
		if runtime != nil {
			runtime.retire()
		}
		runtimes = append(runtimes, runtime)
	}
	s.agentRuntimes = map[string]*managedWorkspaceRuntime{}
	s.mu.Unlock()
	_ = closeManagedWorkspaceRuntimes(runtimes)
	s.agentRuntimeSupervisors.Wait()
}

func (s *Service) startAgentWorkspaceRuntimeAttempt(
	ctx context.Context,
	agentID string,
	runtime *workspaceRuntime,
	workspace *workspaceResponse,
	restartAttempt int,
) *managedWorkspaceRuntime {
	return s.startAgentWorkspaceRuntimeAttemptWithReady(ctx, agentID, runtime, workspace, restartAttempt, nil)
}

func (s *Service) startAgentWorkspaceRuntimeAttemptWithReady(
	ctx context.Context,
	agentID string,
	runtime *workspaceRuntime,
	workspace *workspaceResponse,
	restartAttempt int,
	ready chan<- error,
) *managedWorkspaceRuntime {
	runtimeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	managed := &managedWorkspaceRuntime{
		runtime:        runtime,
		cancel:         cancel,
		done:           done,
		parentCtx:      ctx,
		runtimeCtx:     runtimeCtx,
		workspace:      workspace,
		startedAt:      time.Now(),
		restartAttempt: restartAttempt,
	}
	s.agentRuntimeSupervisors.Add(1)
	go func() {
		defer s.agentRuntimeSupervisors.Done()
		runErr := runtime.run(runtimeCtx, ready)
		managed.recordResult(runErr)
		close(done)
		s.superviseStoppedAgentWorkspaceRuntime(agentID, managed)
	}()
	return managed
}

func (s *Service) superviseStoppedAgentWorkspaceRuntime(agentID string, failed *managedWorkspaceRuntime) {
	if s == nil || failed == nil || failed.runtimeCtx == nil || failed.parentCtx == nil {
		return
	}
	runErr, stoppedAt := failed.result()
	if stoppedAt.IsZero() {
		stoppedAt = time.Now()
	}
	delayAttempt, nextAttempt := failed.restartPlan()
	for {
		deadline := stoppedAt.Add(managedWorkspaceRuntimeRestartDelay(delayAttempt, runErr))
		if !waitForManagedWorkspaceRuntimeRestart(failed.runtimeCtx, deadline) {
			return
		}

		s.mu.Lock()
		if failed.runtimeCtx.Err() != nil || failed.parentCtx.Err() != nil || s.agentRuntimes[agentID] != failed {
			s.mu.Unlock()
			return
		}
		workspace := failed.workspace
		if !workspaceContainsAgent(workspace, agentID) {
			s.mu.Unlock()
			return
		}
		runtime, err := newWorkspaceRuntime(
			s.cfg,
			s.client,
			agentWorkspacePath(s.cfg, agentID),
			agentID,
			"agent",
		)
		if err != nil {
			s.mu.Unlock()
			log.Printf("agent workspace runtime %s replacement construction failed: %v", agentID, err)
			runErr = err
			stoppedAt = time.Now()
			delayAttempt = nextAttempt
			nextAttempt++
			continue
		}
		runtime.initialWorkspace = workspace
		replacement := s.startAgentWorkspaceRuntimeAttempt(
			failed.parentCtx,
			agentID,
			runtime,
			workspace,
			nextAttempt,
		)
		failed.retire()
		if failed.cancel != nil {
			failed.cancel()
		}
		s.agentRuntimes[agentID] = replacement
		s.mu.Unlock()

		log.Printf(
			"agent workspace runtime %s stopped (%s): %v; restarting",
			agentID,
			managedWorkspaceRuntimeExitClass(runErr),
			runErr,
		)
		if err := closeManagedWorkspaceRuntime(failed); err != nil {
			log.Printf("agent workspace runtime %s retirement failed: %v", agentID, err)
		}
		return
	}
}

func waitForManagedWorkspaceRuntimeRestart(ctx context.Context, deadline time.Time) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	delay := time.Until(deadline)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func workspaceContainsAgent(workspace *workspaceResponse, agentID string) bool {
	if workspace == nil || agentID == "" {
		return false
	}
	for _, current := range workspace.Agents {
		if current != nil && current.ID == agentID {
			return true
		}
	}
	return false
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

func (runtime *managedWorkspaceRuntime) restartPlan() (delayAttempt, nextAttempt int) {
	if runtime == nil {
		return 0, 0
	}
	_, stoppedAt := runtime.result()
	if !runtime.startedAt.IsZero() && !stoppedAt.IsZero() && stoppedAt.Sub(runtime.startedAt) >= agentRuntimeStableWindow {
		return 0, 0
	}
	return runtime.restartAttempt, runtime.restartAttempt + 1
}

func (runtime *managedWorkspaceRuntime) borrow() (*workspaceRuntime, func()) {
	if runtime == nil {
		return nil, func() {}
	}
	runtime.borrowMu.Lock()
	if runtime.retiring || runtime.runtime == nil {
		runtime.borrowMu.Unlock()
		return nil, func() {}
	}
	if runtime.borrowCond == nil {
		runtime.borrowCond = sync.NewCond(&runtime.borrowMu)
	}
	runtime.borrowers++
	borrowed := runtime.runtime
	runtime.borrowMu.Unlock()

	var once sync.Once
	return borrowed, func() {
		once.Do(func() {
			runtime.borrowMu.Lock()
			if runtime.borrowers > 0 {
				runtime.borrowers--
			}
			if runtime.borrowers == 0 && runtime.borrowCond != nil {
				runtime.borrowCond.Broadcast()
			}
			runtime.borrowMu.Unlock()
		})
	}
}

func (runtime *managedWorkspaceRuntime) retire() {
	if runtime == nil {
		return
	}
	runtime.borrowMu.Lock()
	runtime.retiring = true
	runtime.borrowMu.Unlock()
}

func (runtime *managedWorkspaceRuntime) waitForBorrowers() {
	if runtime == nil {
		return
	}
	runtime.borrowMu.Lock()
	if runtime.borrowCond == nil {
		runtime.borrowCond = sync.NewCond(&runtime.borrowMu)
	}
	for runtime.borrowers > 0 {
		runtime.borrowCond.Wait()
	}
	runtime.borrowMu.Unlock()
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
	runtime.retire()
	if runtime.cancel != nil {
		runtime.cancel()
	}
	waitManagedWorkspaceRuntime(runtime)
	runtime.waitForBorrowers()
	if runtime.runtime == nil {
		return nil
	}
	return runtime.runtime.Close()
}

func closeManagedWorkspaceRuntimes(runtimes []*managedWorkspaceRuntime) error {
	for _, runtime := range runtimes {
		if runtime != nil {
			runtime.retire()
			if runtime.cancel != nil {
				runtime.cancel()
			}
		}
	}
	var errs []error
	for _, runtime := range runtimes {
		waitManagedWorkspaceRuntime(runtime)
		if runtime != nil {
			runtime.waitForBorrowers()
		}
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
