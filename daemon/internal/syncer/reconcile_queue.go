package syncer

import (
	"sort"
	"strings"
	"sync"
)

type reconcileQueue struct {
	mu    sync.Mutex
	dirty map[string]struct{}
}

func newReconcileQueue() *reconcileQueue {
	return &reconcileQueue{dirty: map[string]struct{}{}}
}

func (q *reconcileQueue) Mark(documentID string) {
	documentID = strings.TrimSpace(documentID)
	if q == nil || documentID == "" {
		return
	}
	q.mu.Lock()
	q.dirty[documentID] = struct{}{}
	q.mu.Unlock()
}

func (q *reconcileQueue) Drain() []string {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.dirty) == 0 {
		return nil
	}
	ids := make([]string, 0, len(q.dirty))
	for documentID := range q.dirty {
		ids = append(ids, documentID)
	}
	q.dirty = map[string]struct{}{}
	sort.Strings(ids)
	return ids
}

func (q *reconcileQueue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.dirty)
}
