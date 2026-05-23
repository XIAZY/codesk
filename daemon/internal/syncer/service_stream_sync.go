package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type workspaceBootstrapResponse struct {
	WorkspaceID  string `json:"workspaceId"`
	RootStreamID string `json:"rootStreamId"`
}

func (s *Service) ensureWorkspaceStreamSync(ctx context.Context, workspace *workspaceResponse) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	alreadyReady := s.primaryStream != nil
	s.mu.Unlock()
	if !alreadyReady {
		bootstrap, err := s.fetchWorkspaceBootstrap(ctx)
		if err != nil {
			return err
		}
		if strings.TrimSpace(bootstrap.RootStreamID) == "" {
			return fmt.Errorf("workspace bootstrap did not include a root stream")
		}
		projection, err := newStreamProjection(ctx, s.cfg, s.client, s.cfg.WorkspaceDir, s.cfg.AgentID, "daemon", bootstrap.RootStreamID)
		if err != nil {
			return err
		}
		projectionCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		if s.primaryStream != nil {
			s.primaryStream.Close()
		}
		s.primaryStream = projection
		s.state = projection.state
		s.workspaceFS = projection.fs
		s.syncLoop = projection.loop
		s.streamSender = projection.sender
		s.mu.Unlock()
		go func() {
			defer cancel()
			projection.Run(projectionCtx)
		}()
	}
	s.mu.Lock()
	projection := s.primaryStream
	s.mu.Unlock()
	if projection != nil {
		projection.EnsureDocumentStreams(ctx, workspace)
	}
	return nil
}

func (s *Service) fetchWorkspaceBootstrap(ctx context.Context) (workspaceBootstrapResponse, error) {
	req, err := s.newBackendRequest(ctx, http.MethodGet, "/api/bootstrap", nil)
	if err != nil {
		return workspaceBootstrapResponse{}, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return workspaceBootstrapResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return workspaceBootstrapResponse{}, &backendStatusError{
			Method:     req.Method,
			URL:        req.URL.String(),
			StatusCode: res.StatusCode,
			Body:       string(body),
		}
	}
	var response workspaceBootstrapResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return workspaceBootstrapResponse{}, err
	}
	return response, nil
}

func (s *Service) reconcileWorkspaceStreams(ctx context.Context) error {
	s.mu.Lock()
	projection := s.primaryStream
	s.mu.Unlock()
	if projection == nil {
		return nil
	}
	return projection.Reconcile(ctx)
}

func (s *Service) workspaceStreamSyncActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.primaryStream != nil && s.syncLoop != nil && s.state != nil
}

func (s *Service) activeWorkspaceStreamLoop() (*WorkspaceSyncLoop, *WorkspaceStateDB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncLoop, s.state
}

func (s *Service) markStreamDirty(streamID string) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return
	}
	s.mu.Lock()
	projection := s.primaryStream
	s.mu.Unlock()
	if projection != nil {
		projection.Mark(streamID)
	}
}

func (s *Service) ensureManagedStreamSync(ctx context.Context, streamID string, kind string) {
	streamID = strings.TrimSpace(streamID)
	if s == nil || streamID == "" {
		return
	}
	kind = firstNonEmptyText(kind, "unknown")
	s.mu.Lock()
	if s.state == nil {
		s.mu.Unlock()
		return
	}
	if s.streamSyncs == nil {
		s.streamSyncs = map[string]*managedStreamSync{}
	}
	if managed := s.streamSyncs[streamID]; managed != nil {
		managed.sync.update(streamID, kind)
		s.mu.Unlock()
		return
	}
	syncCtx, cancel := context.WithCancel(ctx)
	sync := newStreamSync(s.cfg, s.state, streamID, kind, s.markStreamDirty)
	s.streamSyncs[streamID] = &managedStreamSync{sync: sync, cancel: cancel}
	s.mu.Unlock()
	go sync.run(syncCtx)
}

func (s *Service) reconcileStreamSyncs(ctx context.Context, workspace *workspaceResponse) error {
	s.mu.Lock()
	loop := s.syncLoop
	state := s.state
	s.mu.Unlock()
	if loop == nil || state == nil {
		return nil
	}
	desired := map[string]string{}
	if loop.RootStreamID != "" {
		desired[loop.RootStreamID] = "root"
	}
	if workspace != nil {
		for _, document := range workspace.Documents {
			if document == nil || strings.TrimSpace(document.ID) == "" || isIgnoredDocumentPath(document.Path) {
				continue
			}
			desired[document.ID] = "content"
		}
	}

	s.mu.Lock()
	if s.streamSyncs == nil {
		s.streamSyncs = map[string]*managedStreamSync{}
	}
	for streamID, kind := range desired {
		if managed := s.streamSyncs[streamID]; managed != nil {
			managed.sync.update(streamID, kind)
			continue
		}
		syncCtx, cancel := context.WithCancel(ctx)
		sync := newStreamSync(s.cfg, state, streamID, kind, s.markStreamDirty)
		s.streamSyncs[streamID] = &managedStreamSync{sync: sync, cancel: cancel}
		go sync.run(syncCtx)
	}
	staleIDs := make([]string, 0)
	for streamID := range s.streamSyncs {
		if _, ok := desired[streamID]; !ok {
			staleIDs = append(staleIDs, streamID)
		}
	}
	sort.Strings(staleIDs)
	stale := make([]*managedStreamSync, 0, len(staleIDs))
	for _, streamID := range staleIDs {
		stale = append(stale, s.streamSyncs[streamID])
		delete(s.streamSyncs, streamID)
	}
	s.mu.Unlock()

	for _, managed := range stale {
		managed.cancel()
	}
	return nil
}

func (s *Service) closeStreamSyncs() {
	s.mu.Lock()
	primary := s.primaryStream
	s.primaryStream = nil
	agentStreams := make([]*managedStreamProjection, 0, len(s.agentStreams))
	for _, projection := range s.agentStreams {
		agentStreams = append(agentStreams, projection)
	}
	s.agentStreams = map[string]*managedStreamProjection{}
	syncs := make([]*managedStreamSync, 0, len(s.streamSyncs))
	for _, sync := range s.streamSyncs {
		syncs = append(syncs, sync)
	}
	s.streamSyncs = map[string]*managedStreamSync{}
	state := s.state
	s.state = nil
	s.workspaceFS = nil
	s.syncLoop = nil
	s.streamSender = nil
	s.mu.Unlock()
	for _, sync := range syncs {
		sync.cancel()
	}
	if primary != nil {
		primary.Close()
	}
	for _, projection := range agentStreams {
		projection.cancel()
		if projection.projection != nil {
			projection.projection.Close()
		}
	}
	if primary == nil && state != nil {
		_ = state.Close()
	}
}
