package syncer

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

func (s *Service) legacyWorkspaceRuntime() *workspaceRuntime {
	if s == nil {
		return nil
	}
	if s.primaryRuntime != nil {
		return s.primaryRuntime
	}
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if s.reconcileQueue == nil {
		s.reconcileQueue = newReconcileQueue()
	}
	if s.localCreates == nil {
		s.localCreates = newLocalCreateQueue()
	}
	if s.documentSyncs == nil {
		s.documentSyncs = map[string]*managedDocumentSync{}
	}
	return &workspaceRuntime{
		cfg:            s.cfg,
		client:         client,
		replica:        s.primaryReplica,
		docCache:       s.docCache,
		reconcileQueue: s.reconcileQueue,
		localCreates:   s.localCreates,
		documentSyncs:  s.documentSyncs,
	}
}

func (s *Service) processLocalCreates(ctx context.Context) error {
	return s.legacyWorkspaceRuntime().processLocalCreates(ctx)
}

func (s *Service) createDocumentFromLocalCandidate(ctx context.Context, candidate localCreateCandidate) (*document, error) {
	return s.legacyWorkspaceRuntime().createDocumentFromLocalCandidate(ctx, candidate)
}

func (s *Service) reconcileDirtyDocuments(ctx context.Context) error {
	runtime := s.legacyWorkspaceRuntime()
	if runtime == nil || runtime.reconcileQueue == nil {
		return nil
	}
	return s.reconcileDocumentIDs(ctx, runtime.reconcileQueue.Drain())
}

func (s *Service) reconcileTrackedDocuments(ctx context.Context) error {
	runtime := s.legacyWorkspaceRuntime()
	if runtime == nil || runtime.docCache == nil {
		return nil
	}
	trackedByDocument := s.collectTrackedByDocument()
	documentIDs := make([]string, 0, len(trackedByDocument))
	for documentID := range trackedByDocument {
		documentIDs = append(documentIDs, documentID)
	}
	sort.Strings(documentIDs)
	return s.reconcileDocumentIDsWithTracked(ctx, documentIDs, trackedByDocument)
}

func (s *Service) reconcileDocumentIDs(ctx context.Context, documentIDs []string) error {
	runtime := s.legacyWorkspaceRuntime()
	if runtime == nil || runtime.docCache == nil || len(documentIDs) == 0 {
		return nil
	}
	trackedByDocument := s.collectTrackedByDocument()
	sort.Strings(documentIDs)
	return s.reconcileDocumentIDsWithTracked(ctx, documentIDs, trackedByDocument)
}

func (s *Service) reconcileDocumentIDsWithTracked(ctx context.Context, documentIDs []string, trackedByDocument map[string][]*trackedFile) error {
	runtime := s.legacyWorkspaceRuntime()
	if runtime == nil || runtime.docCache == nil || len(documentIDs) == 0 {
		return nil
	}
	var firstErr error
	for index, documentID := range documentIDs {
		if ctx.Err() != nil {
			for _, remainingID := range documentIDs[index:] {
				s.markDocumentDirty(remainingID)
			}
			return ctx.Err()
		}
		tracked := trackedByDocument[documentID]
		if len(tracked) == 0 {
			continue
		}
		if err := runtime.reconcileTrackedDocument(ctx, documentID, tracked); err != nil {
			s.markDocumentDirty(documentID)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", documentID, err)
			}
			continue
		}
		if runtime.documentNeedsReconcile(documentID, tracked) {
			s.markDocumentDirty(documentID)
		}
	}
	return firstErr
}

func (s *Service) collectTrackedByDocument() map[string][]*trackedFile {
	result := map[string][]*trackedFile{}
	if s == nil {
		return result
	}
	s.mu.Lock()
	replicas := make([]*workspaceReplica, 0, len(s.agentReplicas)+1)
	if s.primaryReplica != nil {
		replicas = append(replicas, s.primaryReplica)
	}
	for _, managed := range s.agentReplicas {
		if managed != nil && managed.replica != nil {
			replicas = append(replicas, managed.replica)
		}
	}
	s.mu.Unlock()
	for _, replica := range replicas {
		replica.mu.Lock()
		for documentID, tracked := range replica.projectedByID {
			result[documentID] = append(result[documentID], tracked)
		}
		replica.mu.Unlock()
	}
	return result
}

func (s *Service) markDocumentDirty(documentID string) {
	if s == nil {
		return
	}
	if s.primaryRuntime != nil {
		s.primaryRuntime.markDocumentDirty(documentID)
		return
	}
	if s.reconcileQueue != nil {
		s.reconcileQueue.Mark(documentID)
	}
}

func (s *Service) reconcileTrackedDocument(ctx context.Context, documentID string, trackedFiles []*trackedFile) error {
	return s.legacyWorkspaceRuntime().reconcileTrackedDocument(ctx, documentID, trackedFiles)
}

func (s *Service) reconcileDocumentSyncs(ctx context.Context, documents []*document) error {
	return s.legacyWorkspaceRuntime().reconcileDocumentSyncs(ctx, documents)
}

func (s *Service) closeDocumentSyncs() {
	if s == nil {
		return
	}
	runtime := s.legacyWorkspaceRuntime()
	if runtime != nil {
		runtime.closeDocumentSyncs()
	}
}
