package syncer

import (
	"bytes"
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
const localPathChangeReconcileWake = "__local_path_change__"
const rootDocumentPath = ".notty/root"

type workspaceRuntime struct {
	cfg                Config
	client             *http.Client
	mu                 sync.Mutex
	replica            *workspaceReplica
	docCache           *documentCache
	reconcileQueue     *reconcileQueue
	localCreates       *localCreateQueue
	documentSocket     *workspaceDocumentSocket
	threadDeliveryWake chan struct{}
	initialWorkspace   *workspaceResponse
	rootDocumentID     string
	sendDocumentUpdate func(context.Context, string, outboxUpdateRecord) error
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
	cache, err := newDocumentCache(workspaceSyncDBPath(rootDir))
	if err != nil {
		return nil, err
	}
	queue := newReconcileQueue()
	localCreates := newLocalCreateQueue()
	runtime := &workspaceRuntime{
		cfg:                cfg,
		client:             client,
		docCache:           cache,
		reconcileQueue:     queue,
		localCreates:       localCreates,
		threadDeliveryWake: make(chan struct{}, 1),
	}
	runtime.documentSocket = newWorkspaceDocumentSocket(runtime)
	replica, err := newWorkspaceReplica(cfg, rootDir, actorID, actorType, runtime.markDocumentDirty, runtime.markLocalCreate)
	if err != nil {
		return nil, err
	}
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

func workspaceSyncDBPath(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".notty", "sync.db")
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
	r.enqueueStartupStoreWork()

	replicaCtx, cancelReplica := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancelReplica()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.replica.Run(replicaCtx); err != nil && replicaCtx.Err() == nil {
			log.Printf("%s workspace replica error: %v", r.replica.actorID, err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.reconcileLoop(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.threadDeliveryLoop(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.presenceLoop(ctx)
	}()
	if r.documentSocket != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.documentSocket.Run(ctx)
		}()
	}

	<-ctx.Done()
	cancelReplica()
	return nil
}

func (r *workspaceRuntime) enqueueStartupStoreWork() {
	if r == nil || r.docCache == nil {
		return
	}
	documentIDs, wakeDelivery, err := r.docCache.documentsNeedingReconcile()
	if err != nil {
		log.Printf("%s startup store recovery error: %v", r.actorID(), err)
		return
	}
	for _, documentID := range documentIDs {
		r.markDocumentDirty(documentID)
	}
	if wakeDelivery {
		r.wakeThreadDelivery()
	}
}

func (r *workspaceRuntime) presenceLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.sendPresence(ctx); err != nil && ctx.Err() == nil {
				log.Printf("%s presence error: %v", r.actorID(), err)
			}
		}
	}
}

func (r *workspaceRuntime) actorID() string {
	if r != nil && r.replica != nil && strings.TrimSpace(r.replica.actorID) != "" {
		return r.replica.actorID
	}
	if r != nil && strings.TrimSpace(r.cfg.AgentID) != "" {
		return r.cfg.AgentID
	}
	return "daemon"
}

func (r *workspaceRuntime) actorKind() string {
	if r != nil && r.replica != nil {
		return r.replica.actorKind()
	}
	return "daemon"
}

func (r *workspaceRuntime) actingAgentID() string {
	if r == nil || r.replica == nil || r.replica.actorType != "agent" {
		return ""
	}
	return r.replica.actorID
}

func (r *workspaceRuntime) sendPresence(ctx context.Context) error {
	payload, err := json.Marshal(upsertPresenceRequest{
		ActorID:   r.actorID(),
		ActorType: r.actorKind(),
		FilePath:  "",
		Mode:      "syncing",
		Activity:  "materializing " + r.actorID(),
	})
	if err != nil {
		return err
	}
	req, err := r.newBackendRequest(ctx, http.MethodPost, "/api/presence", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	applyBackendAuth(req.Header, r.cfg, r.actingAgentID())
	req.Header.Set("Content-Type", "application/json")
	_, err = r.client.Do(req)
	return err
}

func (r *workspaceRuntime) applyWorkspace(ctx context.Context, workspace *workspaceResponse) error {
	if r == nil {
		return nil
	}
	if workspace != nil {
		r.rootDocumentID = strings.TrimSpace(workspace.RootDocumentID)
	}
	if err := r.updateDesiredDocumentsFromRootProjection(); err != nil {
		return err
	}
	if r.rootDocumentID != "" {
		r.markDocumentDirty(r.rootDocumentID)
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
		requeuePathChanges := false
		requeuePathChanges, pathChangeErr := r.processPathChanges(ctx)
		if pathChangeErr != nil && ctx.Err() == nil {
			fmt.Printf("path change reconcile error: %v\n", pathChangeErr)
		}
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
		if pathChangeErr != nil && ctx.Err() == nil {
			r.markDocumentDirty(localPathChangeReconcileWake)
		}
		if requeuePathChanges {
			r.markDocumentDirty(localPathChangeReconcileWake)
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
