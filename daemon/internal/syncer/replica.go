package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type workspaceReplica struct {
	backendURL string
	cfg        Config
	rootDir    string
	actorID    string
	actorType  string
	markDirty  func(documentID string)
	markCreate func(localCreateCandidate)

	client   *http.Client
	watcher  *fsnotify.Watcher
	docCache *documentCache
	fs       *WorkspaceFS

	mu               sync.Mutex
	applyMu          sync.Mutex
	projectedByPath  map[string]*trackedFile
	projectedByID    map[string]*trackedFile
	initialWorkspace *workspaceResponse
}

func newWorkspaceReplica(cfg Config, rootDir, actorID, actorType string, markDirty func(string), markCreate func(localCreateCandidate)) (*workspaceReplica, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if actorType == "" {
		actorType = "daemon"
	}
	return &workspaceReplica{
		backendURL:      cfg.BackendURL,
		cfg:             cfg,
		rootDir:         rootDir,
		actorID:         actorID,
		actorType:       actorType,
		markDirty:       markDirty,
		markCreate:      markCreate,
		client:          &http.Client{Timeout: 10 * time.Second},
		watcher:         watcher,
		fs:              NewWorkspaceFS(rootDir),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
	}, nil
}

func (r *workspaceReplica) actingAgentID() string {
	if r == nil || r.actorType != "agent" {
		return ""
	}
	return r.actorID
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
	if err := r.watcher.Add(r.rootDir); err != nil {
		return err
	}
	if r.initialWorkspace != nil {
		if err := r.applyWorkspace(ctx, r.initialWorkspace); err != nil {
			return err
		}
		r.initialWorkspace = nil
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-r.watcher.Events:
			if isIgnoredWorkspaceAbsolutePath(r.rootDir, event.Name) {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					_ = r.watcher.Add(event.Name)
				}
			}
			if event.Op&fsnotify.Write != 0 {
				if err := r.handleLocalChange(event.Name); err != nil {
					log.Printf("%s local change error for %s: %v", r.actorID, event.Name, err)
				}
			}
			if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				if err := r.reconcileLocalWorkspace(ctx); err != nil {
					log.Printf("%s local reconcile error: %v", r.actorID, err)
				}
			}
		case err := <-r.watcher.Errors:
			if err != nil {
				log.Printf("%s watcher error: %v", r.actorID, err)
			}
		case <-ticker.C:
			if err := r.reconcileLocalWorkspace(ctx); err != nil {
				log.Printf("%s local reconcile error: %v", r.actorID, err)
			}
			if err := r.sendPresence(ctx); err != nil {
				log.Printf("%s presence error: %v", r.actorID, err)
			}
		}
	}
}

func (r *workspaceReplica) refresh(ctx context.Context) error {
	workspace, err := r.fetchWorkspace(ctx)
	if err != nil {
		return err
	}
	return r.applyWorkspace(ctx, workspace)
}

func (r *workspaceReplica) fetchWorkspace(ctx context.Context) (*workspaceResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.backendURL+r.cfg.workspaceAPIPath("/api/workspace"), nil)
	if err != nil {
		return nil, err
	}
	applyBackendAuth(req.Header, r.cfg, r.actingAgentID())
	res, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var workspace workspaceResponse
	if err := json.NewDecoder(res.Body).Decode(&workspace); err != nil {
		return nil, err
	}
	return &workspace, nil
}

func (r *workspaceReplica) applyWorkspace(ctx context.Context, workspace *workspaceResponse) error {
	if workspace == nil {
		return nil
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	activeIDs := make(map[string]struct{}, len(workspace.Documents))
	for _, document := range workspace.Documents {
		if document == nil || isIgnoredDocumentPath(document.Path) {
			continue
		}
		activeIDs[document.ID] = struct{}{}
		if err := r.ensureTracked(ctx, document); err != nil {
			return err
		}
	}
	return r.removeMissingTracked(activeIDs)
}

func (r *workspaceReplica) removeMissingTracked(activeIDs map[string]struct{}) error {
	r.mu.Lock()
	missing := make([]*trackedFile, 0)
	for documentID, tracked := range r.projectedByID {
		if _, ok := activeIDs[documentID]; ok {
			continue
		}
		if isIgnoredDocumentPath(tracked.DocumentPath) || isIgnoredWorkspaceAbsolutePath(r.rootDir, tracked.Path) {
			continue
		}
		tracked.markRemoteDeleted()
		missing = append(missing, tracked)
	}
	r.mu.Unlock()
	for _, tracked := range missing {
		r.markDocumentDirty(tracked.DocumentID)
	}
	return nil
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
	if r.watcher != nil {
		_ = r.watcher.Add(filepath.Dir(absolutePath))
	}

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
			tracked.WorkspaceRoot = workspaceRootForDocumentPath(absolutePath, document.Path)
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
	if tracked.isLocalDirty() {
		r.markDocumentDirty(tracked.DocumentID)
	}
	return nil
}

func (r *workspaceReplica) reconcileLocalWorkspace(ctx context.Context) error {
	actualFiles, err := scanWorkspaceFiles(r.rootDir)
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
	return nil
}

func (r *workspaceReplica) updateTrackedPath(tracked *trackedFile, nextPath string) {
	r.setTrackedPath(tracked, nextPath)
}

func (r *workspaceReplica) setTrackedPath(tracked *trackedFile, nextPath string) {
	if r == nil || tracked == nil || nextPath == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.projectedByPath, tracked.Path)
	tracked.Path = nextPath
	tracked.WorkspaceRoot = workspaceRootForDocumentPath(nextPath, tracked.DocumentPath)
	tracked.FS = r.fs
	r.projectedByPath[nextPath] = tracked
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
	tracked.clearLocalDirty()
	tracked.clearLocalDeleted()
	tracked.clearLocalMoved()
	tracked.clearRemoteDeleted()
}

func (r *workspaceReplica) sendPresence(ctx context.Context) error {
	payload, err := json.Marshal(upsertPresenceRequest{
		ActorID:   r.actorID,
		ActorType: r.actorKind(),
		FilePath:  "",
		Mode:      "syncing",
		Activity:  "materializing " + r.actorID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.backendURL+r.cfg.workspaceAPIPath("/api/presence"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	applyBackendAuth(req.Header, r.cfg, r.actingAgentID())
	req.Header.Set("Content-Type", "application/json")
	_, err = r.client.Do(req)
	return err
}
