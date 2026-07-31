package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	workspaceLocalScanInterval  = 5 * time.Second
	workspaceWatcherEventBudget = 256
)

type workspaceWatcher interface {
	Add(string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyWorkspaceWatcher struct {
	watcher *fsnotify.Watcher
}

type workspaceWatcherWork struct {
	event     *fsnotify.Event
	barrier   chan struct{}
	initial   chan<- error
	reconcile bool
}

// workspaceWatcherQueue keeps the watcher backend independent from event
// handling. In particular, a handler may call watcher.Add without preventing
// the backend from delivering the event that unblocks that Add on Windows.
type workspaceWatcherQueue struct {
	mu              sync.Mutex
	work            []workspaceWatcherWork
	eventCount      int
	reconcileQueued bool
	wake            chan struct{}
}

func newWorkspaceWatcherQueue() *workspaceWatcherQueue {
	return &workspaceWatcherQueue{wake: make(chan struct{}, 1)}
}

func (q *workspaceWatcherQueue) push(work workspaceWatcherWork) {
	q.mu.Lock()
	switch {
	case work.reconcile:
		q.pushReconcileLocked()
	case work.event != nil:
		if q.eventCount < workspaceWatcherEventBudget {
			q.work = append(q.work, work)
			q.eventCount++
		} else {
			q.pushReconcileLocked()
		}
	default:
		q.work = append(q.work, work)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *workspaceWatcherQueue) pushReconcile() {
	q.push(workspaceWatcherWork{reconcile: true})
}

func (q *workspaceWatcherQueue) pushReconcileLocked() {
	if q.reconcileQueued {
		return
	}
	q.work = append(q.work, workspaceWatcherWork{reconcile: true})
	q.reconcileQueued = true
}

func (q *workspaceWatcherQueue) drain() []workspaceWatcherWork {
	q.mu.Lock()
	work := q.work
	q.work = nil
	q.eventCount = 0
	q.reconcileQueued = false
	q.mu.Unlock()
	return work
}

func newFSNotifyWorkspaceWatcher() (workspaceWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyWorkspaceWatcher{watcher: watcher}, nil
}

func (w *fsnotifyWorkspaceWatcher) Add(path string) error {
	return w.watcher.Add(path)
}

func (w *fsnotifyWorkspaceWatcher) Close() error {
	return w.watcher.Close()
}

func (w *fsnotifyWorkspaceWatcher) Events() <-chan fsnotify.Event {
	return w.watcher.Events
}

func (w *fsnotifyWorkspaceWatcher) Errors() <-chan error {
	return w.watcher.Errors
}

type workspaceReplica struct {
	rootDir    string
	actorID    string
	actorType  string
	markDirty  func(documentID string)
	markCreate func(localCreateCandidate)

	watcher            workspaceWatcher
	newWatcher         func() (workspaceWatcher, error)
	docCache           *documentCache
	fs                 *WorkspaceFS
	watchMu            sync.Mutex
	watched            map[string]struct{}
	changes            *workspaceChangeIndex
	reconcile          func(context.Context) error
	observeMissingPath func(string) (string, bool, error)

	mu              sync.Mutex
	projectedByPath map[string]*trackedFile
	projectedByID   map[string]*trackedFile
}

func newWorkspaceReplicaWithFS(
	rootDir, actorID, actorType string,
	markDirty func(string),
	markCreate func(localCreateCandidate),
	fs *WorkspaceFS,
) *workspaceReplica {
	if actorType == "" {
		actorType = "daemon"
	}
	replica := &workspaceReplica{
		rootDir:            rootDir,
		actorID:            actorID,
		actorType:          actorType,
		markDirty:          markDirty,
		markCreate:         markCreate,
		newWatcher:         newFSNotifyWorkspaceWatcher,
		fs:                 fs,
		watched:            map[string]struct{}{},
		changes:            newWorkspaceChangeIndex(),
		observeMissingPath: observeTrackedFileAfterMissingSignal,
		projectedByPath:    map[string]*trackedFile{},
		projectedByID:      map[string]*trackedFile{},
	}
	replica.reconcile = replica.reconcileLocalWorkspace
	return replica
}

func (r *workspaceReplica) actorKind() string {
	if r == nil || r.actorType == "" {
		return "daemon"
	}
	return r.actorType
}

func (r *workspaceReplica) markDocumentDirty(documentID string) {
	if r != nil && r.markDirty != nil {
		r.markDirty(documentID)
	}
}

func (r *workspaceReplica) run(ctx context.Context, ready chan<- error) (runErr error) {
	reportReady := func(err error) {
		if ready == nil {
			return
		}
		ready <- err
		ready = nil
	}
	defer func() { reportReady(runErr) }()
	if r == nil {
		return nil
	}
	if r.reconcile == nil {
		r.reconcile = r.reconcileLocalWorkspace
	}
	// The watcher exists only while this method is consuming both of its
	// channels. Creating it in the constructor can deadlock synchronous users.
	newWatcher := r.newWatcher
	if newWatcher == nil {
		newWatcher = newFSNotifyWorkspaceWatcher
	}
	watcher, err := newWatcher()
	if err != nil {
		return err
	}
	r.watchMu.Lock()
	if r.watcher != nil {
		r.watchMu.Unlock()
		_ = watcher.Close()
		return errors.New("workspace replica is already running")
	}
	r.watcher = watcher
	r.watched = map[string]struct{}{}
	r.watchMu.Unlock()

	// Keep both stages alive until watcher.Close has stopped ingress. The pump
	// must outlive Close because the Windows backend can be waiting for its
	// event delivery to be consumed before Close can finish.
	pipelineCtx, cancelPipeline := context.WithCancel(context.Background())
	queue := newWorkspaceWatcherQueue()
	fatal := make(chan error, 2)
	barriers := make(chan chan struct{})
	var pipeline sync.WaitGroup
	pipeline.Add(2)
	go func() {
		defer pipeline.Done()
		r.runWatcherPump(pipelineCtx, watcher, queue, barriers, fatal)
	}()
	go func() {
		defer pipeline.Done()
		r.runWatcherHandler(pipelineCtx, queue)
	}()
	defer func() {
		_ = watcher.Close()
		cancelPipeline()
		pipeline.Wait()
		r.watchMu.Lock()
		if r.watcher == watcher {
			r.watcher = nil
			r.watched = map[string]struct{}{}
		}
		r.watchMu.Unlock()
	}()
	if err := os.MkdirAll(r.rootDir, 0o755); err != nil {
		return err
	}
	if r.fs != nil {
		if err := r.fs.CleanupStaleLocks(); err != nil {
			return err
		}
	}
	if err := r.addWatchDir(r.rootDir); err != nil {
		return err
	}
	if err := r.ensureDirectoryWatches(); err != nil {
		return err
	}
	if err := awaitWorkspaceInitialReconcile(ctx, queue, fatal); err != nil {
		return err
	}
	if err := awaitWorkspaceWatcherBarrier(ctx, barriers, fatal); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	reportReady(nil)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		}
	}
}

func (r *workspaceReplica) runWatcherPump(
	ctx context.Context,
	watcher workspaceWatcher,
	queue *workspaceWatcherQueue,
	barriers <-chan chan struct{},
	fatal chan<- error,
) {
	events := watcher.Events()
	errorsCh := watcher.Errors()
	for {
		if events == nil && errorsCh == nil {
			reportWorkspaceReplicaFatal(fatal, errors.New("workspace watcher channels closed unexpectedly"))
			return
		}
		select {
		case <-ctx.Done():
			return
		case barrier := <-barriers:
			queue.push(workspaceWatcherWork{barrier: barrier})
		case event, ok := <-events:
			if !ok {
				events = nil
				reportWorkspaceReplicaFatal(fatal, errors.New("workspace watcher event channel closed unexpectedly"))
				continue
			}
			eventCopy := event
			queue.push(workspaceWatcherWork{event: &eventCopy})
		case err, ok := <-errorsCh:
			if !ok {
				errorsCh = nil
				reportWorkspaceReplicaFatal(fatal, errors.New("workspace watcher error channel closed unexpectedly"))
				continue
			}
			if err != nil {
				log.Printf("workspace watcher error; scheduling full reconcile: %v", err)
				queue.pushReconcile()
			}
		}
	}
}

func (r *workspaceReplica) runWatcherHandler(
	ctx context.Context,
	queue *workspaceWatcherQueue,
) {
	timer := time.NewTimer(workspaceLocalScanInterval)
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(workspaceLocalScanInterval)
	}
	startupComplete := false
	pendingReconcile := false
	reconcile := func() {
		if err := r.reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Printf("workspace reconcile error; will retry: %v", err)
		}
		resetTimer()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-queue.wake:
			for _, work := range queue.drain() {
				if work.barrier != nil {
					close(work.barrier)
					continue
				}
				if work.initial != nil {
					err := r.reconcile(ctx)
					if err == nil {
						startupComplete = true
						resetTimer()
					}
					work.initial <- err
					continue
				}
				if work.reconcile {
					pendingReconcile = true
					continue
				}
				if work.event != nil {
					if err := r.handleWatcherEvent(*work.event, time.Now()); err != nil {
						log.Printf("workspace event processing error; scheduling full reconcile: %v", err)
						pendingReconcile = true
					}
				}
			}
			if startupComplete && pendingReconcile {
				pendingReconcile = false
				reconcile()
			}
		case <-timer.C:
			if startupComplete {
				reconcile()
			} else {
				pendingReconcile = true
				resetTimer()
			}
		}
	}
}

func reportWorkspaceReplicaFatal(fatal chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case fatal <- err:
	default:
	}
}

func awaitWorkspaceInitialReconcile(
	ctx context.Context,
	queue *workspaceWatcherQueue,
	fatal <-chan error,
) error {
	result := make(chan error, 1)
	queue.push(workspaceWatcherWork{initial: result})
	select {
	case err := <-result:
		return err
	case err := <-fatal:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func awaitWorkspaceWatcherBarrier(
	ctx context.Context,
	barriers chan<- chan struct{},
	fatal <-chan error,
) error {
	barrier := make(chan struct{})
	select {
	case barriers <- barrier:
	case err := <-fatal:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-barrier:
		select {
		case err := <-fatal:
			return err
		default:
			return nil
		}
	case err := <-fatal:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *workspaceReplica) ensureTracked(ctx context.Context, document *document) error {
	if document == nil || isIgnoredDocumentPath(document.Path) {
		return nil
	}
	absolutePath := filepath.Join(r.rootDir, filepath.FromSlash(document.Path))
	if isIgnoredWorkspaceAbsolutePath(r.rootDir, absolutePath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return err
	}
	_ = r.addWatchDir(filepath.Dir(absolutePath))

	r.mu.Lock()
	tracked, exists := r.projectedByID[document.ID]
	r.mu.Unlock()

	if exists {
		tracked.ActorID = r.actorID
		tracked.ActorType = r.actorKind()
		tracked.FS = r.fs
		tracked.Owner = r
		tracked.clearRemoteDeleted()
		if tracked.WorkspaceRoot == "" {
			tracked.WorkspaceRoot = r.rootDir
		}
		if tracked.DocumentPath != document.Path {
			tracked.DocumentPath = document.Path
			r.markDocumentDirty(tracked.DocumentID)
		}
		if tracked.isLocalDirty() {
			r.markDocumentDirty(tracked.DocumentID)
		}
		return nil
	}

	tracked, err := materializeTrackedFileWithFS(ctx, r.docCache, document, absolutePath, r.fs)
	if err != nil {
		return err
	}
	tracked.ActorID = r.actorID
	tracked.ActorType = r.actorKind()
	tracked.FS = r.fs
	tracked.Owner = r

	r.mu.Lock()
	r.projectedByPath[absolutePath] = tracked
	r.projectedByID[document.ID] = tracked
	r.mu.Unlock()
	if tracked.isLocalDirty() {
		r.markDocumentDirty(tracked.DocumentID)
	}
	r.recordTrackedIdentity(absolutePath)
	return nil
}

func (r *workspaceReplica) handleWatcherEvent(event fsnotify.Event, now time.Time) error {
	path := filepath.Clean(event.Name)
	if isIgnoredWorkspaceAbsolutePath(r.rootDir, path) {
		return nil
	}
	if r.changes == nil {
		r.changes = newWorkspaceChangeIndex()
	}
	r.mu.Lock()
	tracked := r.projectedByPath[path]
	r.mu.Unlock()

	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if tracked != nil {
			r.changes.markPendingMissing(tracked.DocumentID, path, now)
			r.markDocumentDirty(localPathChangeReconcileWake)
		}
		return nil
	}

	if event.Op&fsnotify.Create != 0 {
		info, identity, err := statFileWithIdentity(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if err := r.addWatchDir(path); err != nil {
				return err
			}
			r.changes.markDiscoverDir(path)
			r.markDocumentDirty(localPathChangeReconcileWake)
			return nil
		}
		if tracked != nil {
			r.changes.markTrackedPresent(tracked.DocumentID, path, identity)
			tracked.clearLocalDeleted()
			if err := markTrackedLocalDirty(tracked, path); err != nil {
				return err
			}
			if tracked.isLocalDirty() {
				r.changes.markDirtyDocument(tracked.DocumentID)
				r.markDocumentDirty(localPathChangeReconcileWake)
			}
			return nil
		}
		r.changes.markLocalCreate(localCreateCandidate{
			Root:      r.rootDir,
			Path:      path,
			ActorID:   r.actorID,
			ActorType: r.actorKind(),
		}, identity)
		r.markDocumentDirty(localPathChangeReconcileWake)
		return nil
	}

	if event.Op&fsnotify.Write != 0 {
		if tracked != nil {
			_, exists, err := r.observeTrackedFileAfterMissingSignal(path)
			if err != nil {
				return fmt.Errorf("observe tracked file %q after write event: %w", path, err)
			}
			if !exists {
				return nil
			}
			r.changes.markTrackedPresent(tracked.DocumentID, path, statFileIdentity(path))
			tracked.clearLocalDeleted()
			if err := markTrackedLocalDirty(tracked, path); err != nil {
				return err
			}
			if tracked.isLocalDirty() {
				r.changes.markDirtyDocument(tracked.DocumentID)
				r.markDocumentDirty(localPathChangeReconcileWake)
			}
			return nil
		}
		info, identity, err := statFileWithIdentity(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		r.changes.markLocalCreate(localCreateCandidate{
			Root:      r.rootDir,
			Path:      path,
			ActorID:   r.actorID,
			ActorType: r.actorKind(),
		}, identity)
		r.markDocumentDirty(localPathChangeReconcileWake)
	}
	return nil
}

func (r *workspaceReplica) handleLocalChange(path string) error {
	if isIgnoredWorkspaceAbsolutePath(r.rootDir, path) {
		return nil
	}
	r.mu.Lock()
	tracked, ok := r.projectedByPath[path]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if err := markTrackedLocalDirty(tracked, path); err != nil {
		return err
	}
	if r.changes != nil {
		r.changes.recordIdentity(path, statFileIdentity(path))
	}
	if tracked.isLocalDirty() {
		r.markDocumentDirty(tracked.DocumentID)
	}
	return nil
}

func (r *workspaceReplica) reconcileLocalWorkspace(ctx context.Context) error {
	actualFiles, err := scanWorkspaceFilesForReconcile(r.rootDir)
	if err != nil {
		return err
	}

	r.mu.Lock()
	trackedFiles := make([]*trackedFile, 0, len(r.projectedByID))
	for _, tracked := range r.projectedByID {
		trackedFiles = append(trackedFiles, tracked)
	}
	r.mu.Unlock()

	sort.Slice(trackedFiles, func(i, j int) bool { return trackedFiles[i].Path < trackedFiles[j].Path })
	remaining := make(map[string]string, len(actualFiles))
	for path, content := range actualFiles {
		remaining[path] = content
	}

	for _, tracked := range trackedFiles {
		current, exists := remaining[tracked.Path]
		if exists {
			delete(remaining, tracked.Path)
		}
		if !tracked.hasProjectedContent() {
			continue
		}
		if tracked.isProjecting() {
			// File projection can briefly look like a local edit, move, or delete.
			continue
		}
		if !exists {
			// WalkDir is not a namespace snapshot. A concurrent same-name
			// replacement can be omitted from one scan, so re-observe a tracked
			// path before classifying the miss as a move or deletion.
			current, exists, err = r.observeTrackedFileAfterMissingSignal(tracked.Path)
			if err != nil {
				return fmt.Errorf("re-observe tracked file %q after scan miss: %w", tracked.Path, err)
			}
		}
		if exists {
			tracked.clearLocalDeleted()
			if r.changes != nil {
				r.changes.markTrackedPresent(tracked.DocumentID, tracked.Path, statFileIdentity(tracked.Path))
			}
			if !tracked.matchesProjectedString(current) {
				if err := r.handleLocalChange(tracked.Path); err != nil {
					return err
				}
			}
			continue
		}
		nextPath, foundMove := findMovedPath(remaining, tracked.matchesProjectedString)
		if foundMove {
			r.updateTrackedPath(tracked, nextPath)
			tracked.markLocalMoved()
			delete(remaining, nextPath)
			r.markDocumentDirty(tracked.DocumentID)
			r.recordTrackedIdentity(nextPath)
			continue
		}
		tracked.markLocalDeleted()
		r.markDocumentDirty(tracked.DocumentID)
	}

	newPaths := make([]string, 0, len(remaining))
	for path := range remaining {
		newPaths = append(newPaths, path)
	}
	sort.Strings(newPaths)
	for _, path := range newPaths {
		if r.markCreate != nil {
			r.markCreate(localCreateCandidate{
				Root:      r.rootDir,
				Path:      path,
				ActorID:   r.actorID,
				ActorType: r.actorKind(),
			})
		}
	}
	if err := r.ensureDirectoryWatches(); err != nil {
		return err
	}
	return nil
}

func observeTrackedFileAfterMissingSignal(path string) (string, bool, error) {
	occupant, err := classifyWorkspacePathOccupant(path)
	if err != nil {
		return "", false, err
	}
	providesContent, err := workspacePathProvidesFileContent(path, occupant)
	if err != nil {
		return "", false, err
	}
	if !providesContent {
		return "", false, nil
	}

	content, err := readFileObservation(path)
	switch {
	case err == nil:
		return string(content), true, nil
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	default:
		return "", false, err
	}
}

func (r *workspaceReplica) observeTrackedFileAfterMissingSignal(path string) (string, bool, error) {
	if r != nil && r.observeMissingPath != nil {
		return r.observeMissingPath(path)
	}
	return observeTrackedFileAfterMissingSignal(path)
}

func (r *workspaceReplica) drainPathChanges(ctx context.Context, now time.Time) (bool, error) {
	if r == nil || r.changes == nil {
		return false, nil
	}
	for _, dir := range r.changes.drainDiscoverDirs() {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if err := r.discoverLocalCreatesInDir(dir); err != nil {
			r.changes.markDiscoverDir(dir)
			return true, err
		}
	}
	changes, _ := r.changes.drain(now)
	for _, move := range changes.LocalMoves {
		r.mu.Lock()
		tracked := r.projectedByID[move.DocumentID]
		r.mu.Unlock()
		if tracked == nil || tracked.isProjecting() {
			continue
		}
		r.updateTrackedPath(tracked, move.NewPath)
		tracked.markLocalMoved()
		r.markDocumentDirty(tracked.DocumentID)
		r.recordTrackedIdentity(move.NewPath)
	}
	for _, deletion := range changes.LocalDeletes {
		r.mu.Lock()
		tracked := r.projectedByID[deletion.DocumentID]
		r.mu.Unlock()
		if tracked == nil || tracked.isProjecting() || tracked.Path != deletion.Path {
			r.changes.resolvePendingMissing(deletion)
			continue
		}
		current, exists, err := r.observeTrackedFileAfterMissingSignal(deletion.Path)
		if err != nil {
			return r.changes.hasPendingMissing(), fmt.Errorf(
				"re-observe tracked file %q before pending delete: %w",
				deletion.Path,
				err,
			)
		}
		if exists {
			r.changes.markTrackedPresent(
				tracked.DocumentID,
				deletion.Path,
				statFileIdentity(deletion.Path),
			)
			tracked.clearLocalDeleted()
			if !tracked.matchesProjectedString(current) {
				if err := r.handleLocalChange(deletion.Path); err != nil {
					return r.changes.hasPendingMissing(), err
				}
			}
			continue
		}
		if !r.changes.resolvePendingMissing(deletion) {
			continue
		}
		tracked.markLocalDeleted()
		r.markDocumentDirty(tracked.DocumentID)
		if r.changes != nil {
			r.changes.removeIdentity(deletion.Path)
		}
	}
	for _, candidate := range changes.LocalCreates {
		if r.markCreate != nil {
			r.markCreate(candidate)
		}
	}
	for _, documentID := range changes.DirtyDocumentIDs {
		r.markDocumentDirty(documentID)
	}
	return r.changes.hasPendingMissing(), nil
}

func (r *workspaceReplica) discoverLocalCreatesInDir(dir string) error {
	if r == nil || strings.TrimSpace(dir) == "" {
		return nil
	}
	dir = filepath.Clean(dir)
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if path != r.rootDir && isIgnoredWorkspaceAbsolutePath(r.rootDir, path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, identity, err := statFileWithIdentity(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return r.addWatchDir(path)
		}
		r.mu.Lock()
		_, tracked := r.projectedByPath[path]
		r.mu.Unlock()
		if tracked {
			if r.changes != nil {
				r.changes.recordIdentity(path, identity)
			}
			return nil
		}
		if r.changes != nil {
			r.changes.markLocalCreate(localCreateCandidate{
				Root:      r.rootDir,
				Path:      path,
				ActorID:   r.actorID,
				ActorType: r.actorKind(),
			}, identity)
		}
		return nil
	})
}

func (r *workspaceReplica) ensureDirectoryWatches() error {
	if r == nil || r.rootDir == "" {
		return nil
	}
	return filepath.WalkDir(r.rootDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		if path != r.rootDir && isIgnoredWorkspaceAbsolutePath(r.rootDir, path) {
			return filepath.SkipDir
		}
		if err := r.addWatchDir(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		return nil
	})
}

func (r *workspaceReplica) addWatchDir(path string) error {
	if r == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	path = filepath.Clean(path)
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.watcher == nil {
		return nil
	}
	if _, ok := r.watched[path]; ok {
		return nil
	}
	if err := r.watcher.Add(path); err != nil {
		return err
	}
	r.watched[path] = struct{}{}
	return nil
}

func (r *workspaceReplica) updateTrackedPath(tracked *trackedFile, nextPath string) {
	r.setTrackedPath(tracked, nextPath)
}

func (r *workspaceReplica) setTrackedPath(tracked *trackedFile, nextPath string) {
	if r == nil || tracked == nil || nextPath == "" {
		return
	}
	oldPath := tracked.Path
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.projectedByPath, oldPath)
	tracked.Path = nextPath
	tracked.WorkspaceRoot = r.rootDir
	tracked.FS = r.fs
	r.projectedByPath[nextPath] = tracked
	if r.changes != nil {
		r.changes.rekeyTrackedPath(tracked.DocumentID, oldPath, nextPath, statFileIdentity(nextPath))
	}
}

func (r *workspaceReplica) untrack(tracked *trackedFile) {
	if r == nil || tracked == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.projectedByID[tracked.DocumentID]; current == tracked {
		delete(r.projectedByID, tracked.DocumentID)
	}
	if current := r.projectedByPath[tracked.Path]; current == tracked {
		delete(r.projectedByPath, tracked.Path)
	}
	if r.changes != nil {
		r.changes.removeIdentity(tracked.Path)
	}
	tracked.clearLocalDirty()
	tracked.clearLocalDeleted()
	tracked.clearLocalMoved()
	tracked.clearRemoteDeleted()
}

func (r *workspaceReplica) recordTrackedIdentity(path string) {
	if r == nil || r.changes == nil || strings.TrimSpace(path) == "" {
		return
	}
	r.changes.recordIdentity(path, statFileIdentity(path))
}

func statFileIdentity(path string) fileIdentity {
	return fileIdentityForPath(path)
}
