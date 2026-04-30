package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

type workspaceReplica struct {
	backendURL string
	rootDir    string
	actorID    string
	label      string

	client   *http.Client
	watcher  *fsnotify.Watcher
	docCache *documentCache

	mu               sync.Mutex
	projectedByPath  map[string]*trackedFile
	projectedByID    map[string]*trackedFile
	initialWorkspace *workspaceResponse
}

func newWorkspaceReplica(backendURL, rootDir, actorID, label string) (*workspaceReplica, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &workspaceReplica{
		backendURL:      backendURL,
		rootDir:         rootDir,
		actorID:         actorID,
		label:           label,
		client:          &http.Client{Timeout: 10 * time.Second},
		watcher:         watcher,
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
	}, nil
}

func (r *workspaceReplica) Run(ctx context.Context) error {
	defer r.watcher.Close()
	if err := os.MkdirAll(r.rootDir, 0o755); err != nil {
		return err
	}
	if err := r.watcher.Add(r.rootDir); err != nil {
		return err
	}
	if r.initialWorkspace != nil {
		if err := r.applyWorkspace(ctx, r.initialWorkspace); err != nil {
			return err
		}
		r.initialWorkspace = nil
	} else {
		if err := r.refresh(ctx); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.closeConnections()
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
					log.Printf("%s local change error for %s: %v", r.label, event.Name, err)
				}
			}
			if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				if err := r.reconcileLocalWorkspace(ctx); err != nil {
					log.Printf("%s local reconcile error: %v", r.label, err)
				}
			}
		case err := <-r.watcher.Errors:
			if err != nil {
				log.Printf("%s watcher error: %v", r.label, err)
			}
		case <-ticker.C:
			if err := r.reconcileLocalWorkspace(ctx); err != nil {
				log.Printf("%s local reconcile error: %v", r.label, err)
			}
			if err := r.refresh(ctx); err != nil {
				log.Printf("%s refresh error: %v", r.label, err)
			}
			if err := r.sendPresence(ctx); err != nil {
				log.Printf("%s presence error: %v", r.label, err)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.backendURL+"/api/workspace", nil)
	if err != nil {
		return nil, err
	}
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
	defer r.mu.Unlock()
	for documentID, tracked := range r.projectedByID {
		if _, ok := activeIDs[documentID]; ok {
			continue
		}
		if tracked.Conn != nil {
			_ = tracked.Conn.Close()
		}
		delete(r.projectedByID, documentID)
		delete(r.projectedByPath, tracked.Path)
		if isIgnoredDocumentPath(tracked.DocumentPath) || isIgnoredWorkspaceAbsolutePath(r.rootDir, tracked.Path) {
			continue
		}
		if err := os.Remove(tracked.Path); err != nil && !errorsIsNotExist(err) {
			return err
		}
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
	_ = r.watcher.Add(filepath.Dir(absolutePath))

	r.mu.Lock()
	tracked, exists := r.projectedByID[document.ID]
	r.mu.Unlock()

	if exists {
		tracked.StateVector = document.StateVector
		if tracked.Path != absolutePath {
			nextContent := tracked.contentString()
			if err := moveLocalFile(tracked.Path, absolutePath, nextContent); err != nil {
				return err
			}
			r.mu.Lock()
			delete(r.projectedByPath, tracked.Path)
			tracked.Path = absolutePath
			tracked.DocumentPath = document.Path
			r.projectedByPath[absolutePath] = tracked
			r.mu.Unlock()
			tracked.setProjectedContent(nextContent)
			if err := tracked.storeProjectedBase(nextContent); err != nil {
				return err
			}
		}
		if tracked.getConn() == nil {
			if err := r.connectDocument(tracked); err != nil {
				return err
			}
		}
		return nil
	}

	tracked, err := materializeTrackedFile(ctx, r.docCache, document, absolutePath)
	if err != nil {
		return err
	}
	if err := r.connectDocument(tracked); err != nil {
		return err
	}

	r.mu.Lock()
	r.projectedByPath[absolutePath] = tracked
	r.projectedByID[document.ID] = tracked
	r.mu.Unlock()
	return nil
}

func (r *workspaceReplica) connectDocument(tracked *trackedFile) error {
	if current := tracked.getConn(); current != nil {
		return nil
	}
	paceDocumentConnect()
	wsURL, err := url.Parse(r.backendURL)
	if err != nil {
		return err
	}
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = "/ws/documents/" + tracked.DocumentID
	clientID, syncStep := tracked.initialSyncState()
	query := url.Values{
		"client_id":  {fmt.Sprintf("%d", clientID)},
		"actor_id":   {r.actorID},
		"actor_type": {"agent"},
	}
	wsURL.RawQuery = query.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		return err
	}
	log.Printf("%s ws open doc=%s url=%s", r.label, tracked.DocumentID, wsURL.String())
	tracked.setConn(conn)
	go r.readLoop(tracked, conn)
	if err := tracked.write(syncStep); err != nil {
		return err
	}
	if err := tracked.write(yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
		clientID: {
			Clock: 1,
			State: []byte(fmt.Sprintf(`{"actorId":"%s","activity":"Syncing %s"}`, r.actorID, filepath.Base(tracked.Path))),
		},
	}, []uint64{clientID})); err != nil {
		return err
	}
	return nil
}

func (r *workspaceReplica) readLoop(tracked *trackedFile, conn *websocket.Conn) {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			tracked.clearConn(conn)
			log.Printf("%s ws read close doc=%s err=%v", r.label, tracked.DocumentID, err)
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
		if err != nil {
			continue
		}
		switch topLevel {
		case yproto.MessageSync:
			reply, _, err := handleRemoteSyncMessage(reader, tracked, "remote")
			if err != nil {
				log.Printf("%s ws sync error doc=%s err=%v", r.label, tracked.DocumentID, err)
				continue
			}
			if len(reply) > 0 {
				_ = tracked.write(reply)
			}
		case yproto.MessageAwareness:
		}
	}
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
	return markTrackedLocalDirty(tracked, path)
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

	changed := false
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
			if err := r.moveRemoteDocument(ctx, tracked.DocumentID, workspaceRelativePath(r.rootDir, nextPath)); err != nil {
				return err
			}
			delete(remaining, nextPath)
			changed = true
			continue
		}
		if err := r.deleteRemoteDocument(ctx, tracked.DocumentID); err != nil {
			return err
		}
		changed = true
	}

	newPaths := make([]string, 0, len(remaining))
	for path := range remaining {
		newPaths = append(newPaths, path)
	}
	sort.Strings(newPaths)
	for _, path := range newPaths {
		if err := r.createRemoteDocument(ctx, path, remaining[path]); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return r.refresh(ctx)
	}
	return nil
}

func (r *workspaceReplica) closeConnections() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tracked := range r.projectedByID {
		if conn := tracked.getConn(); conn != nil {
			_ = conn.Close()
		}
	}
}

func (r *workspaceReplica) sendPresence(ctx context.Context) error {
	payload, err := json.Marshal(upsertPresenceRequest{
		ActorID:   r.actorID,
		ActorType: "agent",
		FilePath:  "",
		Mode:      "syncing",
		Activity:  "materializing " + r.label,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.backendURL+"/api/presence", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = r.client.Do(req)
	return err
}

func (r *workspaceReplica) createRemoteDocument(ctx context.Context, path, content string) error {
	relativePath := workspaceRelativePath(r.rootDir, path)
	req := createDocumentRequest{Path: relativePath, Content: content}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.backendURL+"/api/documents?actor="+url.QueryEscape(r.actorID)+"&actor_type=agent", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("create document failed: %s", res.Status)
	}
	return nil
}

func (r *workspaceReplica) moveRemoteDocument(ctx context.Context, documentID, path string) error {
	payload, err := json.Marshal(updateDocumentRequest{Path: path})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, r.backendURL+"/api/documents/"+documentID+"?actor="+url.QueryEscape(r.actorID)+"&actor_type=agent", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("move document failed: %s", res.Status)
	}
	return nil
}

func (r *workspaceReplica) deleteRemoteDocument(ctx context.Context, documentID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, r.backendURL+"/api/documents/"+documentID+"?actor="+url.QueryEscape(r.actorID)+"&actor_type=agent", nil)
	if err != nil {
		return err
	}
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("delete document failed: %s", res.Status)
	}
	return nil
}
