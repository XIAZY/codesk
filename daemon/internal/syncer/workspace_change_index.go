package syncer

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const workspaceMissingPathDelay = 250 * time.Millisecond

type fileIdentity struct {
	dev   uint64
	ino   uint64
	valid bool
}

func sameFileIdentity(left, right fileIdentity) bool {
	return left.valid && right.valid && left.dev == right.dev && left.ino == right.ino
}

type localMoveCandidate struct {
	DocumentID string
	OldPath    string
	NewPath    string
}

type localDeleteCandidate struct {
	DocumentID string
	Path       string
	generation uint64
}

type workspaceChanges struct {
	DirtyDocumentIDs []string
	LocalCreates     []localCreateCandidate
	LocalMoves       []localMoveCandidate
	LocalDeletes     []localDeleteCandidate
}

type pendingMissingPath struct {
	documentID string
	path       string
	identity   fileIdentity
	readyAt    time.Time
	generation uint64
}

type pendingCreatedPath struct {
	candidate localCreateCandidate
	identity  fileIdentity
}

type workspaceChangeIndex struct {
	mu                    sync.Mutex
	dirtyDocuments        map[string]struct{}
	creates               map[string]pendingCreatedPath
	missing               map[string]pendingMissingPath
	discoverDirs          map[string]struct{}
	identities            map[string]fileIdentity
	nextMissingGeneration uint64
}

func newWorkspaceChangeIndex() *workspaceChangeIndex {
	return &workspaceChangeIndex{
		dirtyDocuments: map[string]struct{}{},
		creates:        map[string]pendingCreatedPath{},
		missing:        map[string]pendingMissingPath{},
		discoverDirs:   map[string]struct{}{},
		identities:     map[string]fileIdentity{},
	}
}

func (c *workspaceChangeIndex) recordIdentity(path string, identity fileIdentity) {
	if c == nil || strings.TrimSpace(path) == "" || !identity.valid {
		return
	}
	c.mu.Lock()
	c.identities[path] = identity
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) rekeyTrackedPath(documentID, oldPath, newPath string, identity fileIdentity) {
	if c == nil {
		return
	}
	c.mu.Lock()
	// The authoritative rekey owns newPath, so watcher artifacts for the old absence or new creation
	// are observations of this same projected move and cannot represent independent local changes.
	if missing, ok := c.missing[documentID]; ok && missing.path == oldPath {
		delete(c.missing, documentID)
	}
	delete(c.creates, newPath)
	delete(c.identities, oldPath)
	if strings.TrimSpace(newPath) != "" && identity.valid {
		c.identities[newPath] = identity
	}
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) removeIdentity(path string) {
	if c == nil || strings.TrimSpace(path) == "" {
		return
	}
	c.mu.Lock()
	delete(c.identities, path)
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) markDirtyDocument(documentID string) {
	if c == nil || strings.TrimSpace(documentID) == "" {
		return
	}
	c.mu.Lock()
	c.dirtyDocuments[documentID] = struct{}{}
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) markLocalCreate(candidate localCreateCandidate, identity fileIdentity) {
	if c == nil || strings.TrimSpace(candidate.Path) == "" {
		return
	}
	c.mu.Lock()
	c.creates[candidate.Path] = pendingCreatedPath{candidate: candidate, identity: identity}
	if identity.valid {
		c.identities[candidate.Path] = identity
	}
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) markPendingMissing(documentID, path string, now time.Time) {
	if c == nil || strings.TrimSpace(documentID) == "" || strings.TrimSpace(path) == "" {
		return
	}
	c.mu.Lock()
	identity := c.identities[path]
	c.nextMissingGeneration++
	c.missing[documentID] = pendingMissingPath{
		documentID: documentID,
		path:       path,
		identity:   identity,
		readyAt:    now.Add(workspaceMissingPathDelay),
		generation: c.nextMissingGeneration,
	}
	delete(c.identities, path)
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) markTrackedPresent(documentID, path string, identity fileIdentity) {
	if c == nil || strings.TrimSpace(documentID) == "" || strings.TrimSpace(path) == "" {
		return
	}
	c.mu.Lock()
	if missing, ok := c.missing[documentID]; ok && missing.path == path {
		delete(c.missing, documentID)
	}
	if identity.valid {
		c.identities[path] = identity
	}
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) resolvePendingMissing(candidate localDeleteCandidate) bool {
	if c == nil ||
		strings.TrimSpace(candidate.DocumentID) == "" ||
		strings.TrimSpace(candidate.Path) == "" ||
		candidate.generation == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	missing, ok := c.missing[candidate.DocumentID]
	if !ok || missing.path != candidate.Path || missing.generation != candidate.generation {
		return false
	}
	delete(c.missing, candidate.DocumentID)
	delete(c.identities, candidate.Path)
	return true
}

func (c *workspaceChangeIndex) hasPendingMissing() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.missing) > 0
}

func (c *workspaceChangeIndex) markDiscoverDir(path string) {
	if c == nil || strings.TrimSpace(path) == "" {
		return
	}
	c.mu.Lock()
	c.discoverDirs[path] = struct{}{}
	c.mu.Unlock()
}

func (c *workspaceChangeIndex) drainDiscoverDirs() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dirs := sortedStringSet(c.discoverDirs)
	c.discoverDirs = map[string]struct{}{}
	return dirs
}

func (c *workspaceChangeIndex) drain(now time.Time) (workspaceChanges, bool) {
	if c == nil {
		return workspaceChanges{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	changes := workspaceChanges{
		DirtyDocumentIDs: sortedStringSet(c.dirtyDocuments),
	}
	c.dirtyDocuments = map[string]struct{}{}

	usedCreates := map[string]struct{}{}
	for documentID, missing := range c.missing {
		if !missing.identity.valid {
			continue
		}
		for path, created := range c.creates {
			if _, used := usedCreates[path]; used {
				continue
			}
			if sameFileIdentity(missing.identity, created.identity) {
				changes.LocalMoves = append(changes.LocalMoves, localMoveCandidate{
					DocumentID: missing.documentID,
					OldPath:    missing.path,
					NewPath:    created.candidate.Path,
				})
				usedCreates[path] = struct{}{}
				delete(c.missing, documentID)
				delete(c.identities, missing.path)
				if created.identity.valid {
					c.identities[created.candidate.Path] = created.identity
				}
				break
			}
		}
	}

	for path := range usedCreates {
		delete(c.creates, path)
	}
	for _, missing := range c.missing {
		if now.Before(missing.readyAt) {
			continue
		}
		changes.LocalDeletes = append(changes.LocalDeletes, localDeleteCandidate{
			DocumentID: missing.documentID,
			Path:       missing.path,
			generation: missing.generation,
		})
	}
	for path, created := range c.creates {
		changes.LocalCreates = append(changes.LocalCreates, created.candidate)
		delete(c.creates, path)
		if created.identity.valid {
			c.identities[path] = created.identity
		}
	}

	sort.Slice(changes.LocalCreates, func(i, j int) bool { return changes.LocalCreates[i].Path < changes.LocalCreates[j].Path })
	sort.Slice(changes.LocalMoves, func(i, j int) bool {
		if changes.LocalMoves[i].DocumentID != changes.LocalMoves[j].DocumentID {
			return changes.LocalMoves[i].DocumentID < changes.LocalMoves[j].DocumentID
		}
		return changes.LocalMoves[i].NewPath < changes.LocalMoves[j].NewPath
	})
	sort.Slice(changes.LocalDeletes, func(i, j int) bool {
		if changes.LocalDeletes[i].DocumentID != changes.LocalDeletes[j].DocumentID {
			return changes.LocalDeletes[i].DocumentID < changes.LocalDeletes[j].DocumentID
		}
		return changes.LocalDeletes[i].Path < changes.LocalDeletes[j].Path
	})

	return changes, len(c.missing) > 0
}

func sortedStringSet(values map[string]struct{}) []string {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	return items
}
