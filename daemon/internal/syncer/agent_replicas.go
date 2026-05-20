package syncer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
)

func (s *Service) reconcileAgentReplicas(ctx context.Context, workspace *workspaceResponse) error {
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
	existing := make([]*workspaceReplica, 0, len(desired))
	for agentID := range desired {
		currentAgent := desired[agentID]
		if managed, ok := s.agentReplicas[agentID]; ok {
			if managed.replica.actorID == currentAgent.ID {
				existing = append(existing, managed.replica)
				continue
			}
			managed.cancel()
			delete(s.agentReplicas, agentID)
		}
		rootDir := filepath.Join(s.cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID))
		replica, err := newWorkspaceReplica(s.cfg, rootDir, currentAgent.ID, "agent", s.markDocumentDirty, s.localCreates.Mark)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		replica.client = s.client
		replica.docCache = s.docCache
		replica.initialWorkspace = workspace
		replicaCtx, cancel := context.WithCancel(ctx)
		s.agentReplicas[agentID] = &managedReplica{replica: replica, cancel: cancel}
		go func() {
			_ = replica.Run(replicaCtx)
		}()
	}

	staleIDs := make([]string, 0)
	for agentID := range s.agentReplicas {
		if _, ok := desired[agentID]; !ok {
			staleIDs = append(staleIDs, agentID)
		}
	}
	sort.Strings(staleIDs)
	stale := make([]*managedReplica, 0, len(staleIDs))
	for _, agentID := range staleIDs {
		stale = append(stale, s.agentReplicas[agentID])
		delete(s.agentReplicas, agentID)
	}
	s.mu.Unlock()

	for _, replica := range existing {
		if err := replica.applyWorkspace(ctx, workspace); err != nil {
			return err
		}
	}
	for index, agentID := range staleIDs {
		stale[index].cancel()
		_ = os.RemoveAll(filepath.Join(s.cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID)))
	}
	return nil
}

func (s *Service) closeAgentReplicas() {
	s.mu.Lock()
	replicas := make([]*managedReplica, 0, len(s.agentReplicas))
	for _, replica := range s.agentReplicas {
		replicas = append(replicas, replica)
	}
	s.agentReplicas = map[string]*managedReplica{}
	s.mu.Unlock()
	for _, replica := range replicas {
		replica.cancel()
	}
}
