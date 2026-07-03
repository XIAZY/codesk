package syncer

import (
	"context"
	"os"
	"path/filepath"
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
	for agentID := range desired {
		currentAgent := desired[agentID]
		if managed, ok := s.agentRuntimes[agentID]; ok && managed != nil && managed.runtime != nil {
			if managed.runtime.replica != nil && managed.runtime.replica.actorID == currentAgent.ID {
				existing = append(existing, managed.runtime)
				continue
			}
			managed.cancel()
			delete(s.agentRuntimes, agentID)
		}
		rootDir := filepath.Join(s.cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID))
		runtime, err := newWorkspaceRuntime(s.cfg, s.client, rootDir, currentAgent.ID, "agent")
		if err != nil {
			s.mu.Unlock()
			return err
		}
		runtime.initialWorkspace = workspace
		runtimeCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		s.agentRuntimes[agentID] = &managedWorkspaceRuntime{runtime: runtime, cancel: cancel, done: done}
		go func() {
			defer close(done)
			_ = runtime.Run(runtimeCtx)
		}()
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

	for _, runtime := range existing {
		if err := runtime.applyWorkspace(ctx, workspace); err != nil {
			return err
		}
	}
	for index, agentID := range staleIDs {
		if stale[index] != nil && stale[index].cancel != nil {
			stale[index].cancel()
		}
		waitManagedWorkspaceRuntime(stale[index])
		_ = os.RemoveAll(filepath.Join(s.cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID)))
	}
	return nil
}

func (s *Service) closeAgentRuntimes() {
	s.mu.Lock()
	runtimes := make([]*managedWorkspaceRuntime, 0, len(s.agentRuntimes))
	for _, runtime := range s.agentRuntimes {
		runtimes = append(runtimes, runtime)
	}
	s.agentRuntimes = map[string]*managedWorkspaceRuntime{}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		if runtime != nil {
			runtime.cancel()
		}
	}
	for _, runtime := range runtimes {
		waitManagedWorkspaceRuntime(runtime)
	}
}

func waitManagedWorkspaceRuntime(runtime *managedWorkspaceRuntime) {
	if runtime == nil || runtime.done == nil {
		return
	}
	<-runtime.done
}
