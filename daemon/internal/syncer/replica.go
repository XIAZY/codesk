package syncer

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const workspaceLocalScanInterval = 5 * time.Second

type workspaceReplica struct {
	rootDir    string
	actorID    string
	actorType  string
	markDirty  func(documentID string)
	markCreate func(localCreateCandidate)

	watcher  *fsnotify.Watcher
	docCache *documentCache
	fs       *WorkspaceFS
	watchMu  sync.Mutex
	watched  map[string]struct{}
	changes  *workspaceChangeIndex

	mu              sync.Mutex
	projectedByPath map[string]*trackedFile
	projectedByID   map[string]*trackedFile
}

func newWorkspaceReplica(_ Config, rootDir, actorID, actorType string, markDirty func(string), markCreate func(localCreateCandidate)) (*workspaceReplica, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if actorType == "" {
		actorType = "daemon"
	}
	return &workspaceReplica{
		rootDir:         rootDir,
		actorID:         actorID,
		actorType:       actorType,
		markDirty:       markDirty,
		markCreate:      markCreate,
		watcher:         watcher,
		fs:              NewWorkspaceFS(rootDir),
		watched:         map[string]struct{}{},
		changes:         newWorkspaceChangeIndex(),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
	}, nil
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

func (r *workspaceReplica) Run(ctx context.Context) error {
	defer r.watcher.Close()
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
		log.Printf("%s workspace watch refresh error: %v", r.actorID, err)
	}
	if err := r.reconcileLocalWorkspace(ctx); err != nil {
		log.Printf("%s initial local reconcile error: %v", r.actorID, err)
	}

	ticker := time.NewTicker(workspaceLocalScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-r.watcher.Events:
			if err := r.handleWatcherEvent(event, time.Now()); err != nil {
				log.Printf("%s local event error for %s: %v", r.actorID, event.Name, err)
			}
		case err := <-r.watcher.Errors:
			if err != nil {
				log.Printf("%s watcher error: %v", r.actorID, err)
			}
		case <-ticker.C:
			if err := r.reconcileLocalWorkspace(ctx); err != nil {
				log.Printf("%s local reconcile error: %v", r.actorID, err)
			}
		}
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

	tracked, err := materializeTrackedFile(ctx, r.docCache, document, absolutePath)
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
			_ = r.addWatchDir(path)
			r.changes.markDiscoverDir(path)
			r.markDocumentDirty(localPathChangeReconcileWake)
			return nil
		}
		if tracked != nil {
			r.changes.markTrackedPresent(tracked.DocumentID, path, identity)
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
			r.changes.markTrackedPresent(tracked.DocumentID, path, statFileIdentity(path))
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
		if exists {
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
	changes, hasPending := r.changes.drain(now)
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
		if tracked == nil || tracked.isProjecting() {
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
	return hasPending, nil
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
	if r == nil || r.watcher == nil || r.rootDir == "" {
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
	if r == nil || r.watcher == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	path = filepath.Clean(path)
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
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
