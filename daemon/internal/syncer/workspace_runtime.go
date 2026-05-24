package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const workspaceReconcileMinInterval = 2 * time.Second
const localCreateReconcileWake = "__local_create__"

type workspaceRuntime struct {
	cfg              Config
	client           *http.Client
	mu               sync.Mutex
	replica          *workspaceReplica
	docCache         *documentCache
	reconcileQueue   *reconcileQueue
	localCreates     *localCreateQueue
	documentSyncs    map[string]*managedDocumentSync
	initialWorkspace *workspaceResponse
}

func (r *workspaceRuntime) fetchWorkspace(ctx context.Context) (*workspaceResponse, error) {
	req, err := r.newBackendRequest(ctx, http.MethodGet, "/api/workspace", nil)
	if err != nil {
		return nil, err
	}
	res, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
		return nil, &backendStatusError{
			Method:     req.Method,
			URL:        req.URL.String(),
			StatusCode: res.StatusCode,
			Body:       string(body),
		}
	}

	var workspace workspaceResponse
	if err := json.NewDecoder(res.Body).Decode(&workspace); err != nil {
		return nil, err
	}
	return &workspace, nil
}

func newWorkspaceRuntime(cfg Config, client *http.Client, rootDir, actorID, actorType string) (*workspaceRuntime, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	cache, err := newDocumentCache(workspaceDocumentStateDir(rootDir))
	if err != nil {
		return nil, err
	}
	queue := newReconcileQueue()
	localCreates := newLocalCreateQueue()
	runtime := &workspaceRuntime{
		cfg:            cfg,
		client:         client,
		docCache:       cache,
		reconcileQueue: queue,
		localCreates:   localCreates,
		documentSyncs:  map[string]*managedDocumentSync{},
	}
	replica, err := newWorkspaceReplica(cfg, rootDir, actorID, actorType, runtime.markDocumentDirty, runtime.markLocalCreate)
	if err != nil {
		return nil, err
	}
	replica.client = client
	replica.docCache = cache
	runtime.replica = replica
	return runtime, nil
}

func (r *workspaceRuntime) markLocalCreate(candidate localCreateCandidate) {
	if r == nil || r.localCreates == nil {
		return
	}
	r.localCreates.Mark(candidate)
	r.markDocumentDirty(localCreateReconcileWake)
}

func workspaceDocumentStateDir(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".notty", "documents")
}

func (r *workspaceRuntime) Run(ctx context.Context) error {
	if r == nil || r.replica == nil {
		return nil
	}
	if r.initialWorkspace != nil {
		if err := r.applyWorkspace(ctx, r.initialWorkspace); err != nil {
			return err
		}
		r.initialWorkspace = nil
	}

	replicaCtx, cancelReplica := context.WithCancel(ctx)
	defer cancelReplica()
	go func() {
		if err := r.replica.Run(replicaCtx); err != nil && replicaCtx.Err() == nil {
			log.Printf("%s workspace replica error: %v", r.replica.actorID, err)
		}
	}()

	go r.reconcileLoop(ctx)

	<-ctx.Done()
	cancelReplica()
	r.closeDocumentSyncs()
	return nil
}

func (r *workspaceRuntime) applyWorkspace(ctx context.Context, workspace *workspaceResponse) error {
	if r == nil {
		return nil
	}
	if r.replica != nil {
		if err := r.replica.applyWorkspace(ctx, workspace); err != nil {
			return err
		}
	}
	documents := []*document(nil)
	if workspace != nil {
		documents = workspace.Documents
	}
	if err := r.reconcileDocumentSyncs(ctx, documents); err != nil {
		return err
	}
	for _, document := range documents {
		if document != nil && document.ID != "" && !isIgnoredDocumentPath(document.Path) {
			r.markDocumentDirty(document.ID)
		}
	}
	return nil
}

func (r *workspaceRuntime) reconcileLoop(ctx context.Context) {
	var lastRun time.Time
	var timer *time.Timer
	var timerC <-chan time.Time

	stopTimer := func() {
		if timer == nil {
			timerC = nil
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	defer stopTimer()

	arm := func(delay time.Duration) {
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerC = timer.C
	}

	runOrDelay := func() {
		if r == nil || r.reconcileQueue == nil || r.reconcileQueue.Len() == 0 {
			stopTimer()
			return
		}
		now := time.Now()
		nextAllowed := lastRun.Add(workspaceReconcileMinInterval)
		if !lastRun.IsZero() && now.Before(nextAllowed) {
			arm(nextAllowed.Sub(now))
			return
		}
		stopTimer()
		lastRun = now
		localCreateErr := r.processLocalCreates(ctx)
		if localCreateErr != nil && ctx.Err() == nil {
			fmt.Printf("local create reconcile error: %v\n", localCreateErr)
		}
		if err := r.reconcileDirtyDocuments(ctx); err != nil && ctx.Err() == nil {
			fmt.Printf("document reconcile error: %v\n", err)
		}
		if localCreateErr != nil && ctx.Err() == nil {
			r.markDocumentDirty(localCreateReconcileWake)
		}
		if r.reconcileQueue.Len() > 0 {
			arm(workspaceReconcileMinInterval)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.reconcileQueue.Wake():
			runOrDelay()
		case <-timerC:
			timerC = nil
			runOrDelay()
		}
	}
}
