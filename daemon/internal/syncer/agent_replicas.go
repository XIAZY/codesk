package syncer

import (
	"context"
	"errors"
	"os"
	"sort"
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
	for agentID := range desired {
		currentAgent := desired[agentID]
		if managed, ok := s.agentRuntimes[agentID]; ok && managed != nil && managed.runtime != nil {
			if managed.runtime.replica != nil && managed.runtime.replica.actorID == currentAgent.ID {
				existing = append(existing, managed.runtime)
				continue
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
		s.agentRuntimes[agentID] = startManagedWorkspaceRuntime(ctx, runtime)
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
	runtimeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	managed := &managedWorkspaceRuntime{runtime: runtime, cancel: cancel, done: done}
	go func() {
		defer close(done)
		_ = runtime.run(runtimeCtx, nil)
	}()
	return managed
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
