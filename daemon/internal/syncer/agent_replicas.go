package syncer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (s *Service) reconcileAgentReplicas(ctx context.Context, workspace *workspaceResponse) error {
	return s.reconcileAgentStreamProjections(ctx, workspace)
}

func (s *Service) reconcileAgentStreamProjections(ctx context.Context, workspace *workspaceResponse) error {
	agents := []*agent(nil)
	if workspace != nil {
		agents = workspace.Agents
	}
	s.mu.Lock()
	primary := s.primaryStream
	if s.agentStreams == nil {
		s.agentStreams = map[string]*managedStreamProjection{}
	}
	rootStreamID := ""
	if primary != nil {
		rootStreamID = primary.rootStreamID
	}
	s.mu.Unlock()
	if rootStreamID == "" {
		return nil
	}

	desired := make(map[string]*agent, len(agents))
	for _, current := range agents {
		if current == nil || strings.TrimSpace(current.ID) == "" {
			continue
		}
		desired[current.ID] = current
	}

	for agentID := range desired {
		s.mu.Lock()
		managed := s.agentStreams[agentID]
		s.mu.Unlock()
		if managed != nil {
			if managed.projection != nil {
				managed.projection.EnsureDocumentStreams(ctx, workspace)
			}
			continue
		}
		rootDir := filepath.Join(s.cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID))
		projection, err := newStreamProjection(ctx, s.cfg, s.client, rootDir, agentID, "agent", rootStreamID)
		if err != nil {
			return err
		}
		projection.EnsureDocumentStreams(ctx, workspace)
		projectionCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		if s.agentStreams == nil {
			s.agentStreams = map[string]*managedStreamProjection{}
		}
		if existing := s.agentStreams[agentID]; existing != nil {
			s.mu.Unlock()
			cancel()
			projection.Close()
			continue
		}
		s.agentStreams[agentID] = &managedStreamProjection{projection: projection, cancel: cancel}
		s.mu.Unlock()
		go projection.Run(projectionCtx)
	}

	staleIDs := make([]string, 0)
	s.mu.Lock()
	for agentID := range s.agentStreams {
		if _, ok := desired[agentID]; !ok {
			staleIDs = append(staleIDs, agentID)
		}
	}
	sort.Strings(staleIDs)
	stale := make([]*managedStreamProjection, 0, len(staleIDs))
	for _, agentID := range staleIDs {
		stale = append(stale, s.agentStreams[agentID])
		delete(s.agentStreams, agentID)
	}
	s.mu.Unlock()

	for index, agentID := range staleIDs {
		stale[index].cancel()
		if stale[index].projection != nil {
			stale[index].projection.Close()
		}
		_ = os.RemoveAll(filepath.Join(s.cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID)))
	}
	return nil
}

func (s *Service) closeAgentReplicas() {
	s.closeAgentStreamProjections()
}

func (s *Service) closeAgentStreamProjections() {
	s.mu.Lock()
	streams := make([]*managedStreamProjection, 0, len(s.agentStreams))
	for _, projection := range s.agentStreams {
		streams = append(streams, projection)
	}
	s.agentStreams = map[string]*managedStreamProjection{}
	s.mu.Unlock()
	for _, managed := range streams {
		managed.cancel()
		if managed.projection != nil {
			managed.projection.Close()
		}
	}
}
