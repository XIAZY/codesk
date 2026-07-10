package syncer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWorkspaceChangeIndexCoalescesDirtyCreatesMovesAndDeletes(t *testing.T) {
	now := time.Now()
	oldPath := "/workspace/docs/old.md"
	newPath := "/workspace/docs/new.md"
	identity := fileIdentity{dev: 1, ino: 42, valid: true}

	index := newWorkspaceChangeIndex()
	index.markDirtyDocument("doc_dirty")
	index.markDirtyDocument("doc_dirty")
	index.recordIdentity(oldPath, identity)
	index.markPendingMissing("doc_move", oldPath, now)
	index.markLocalCreate(localCreateCandidate{Root: "/workspace", Path: newPath}, identity, now)
	index.markLocalCreate(localCreateCandidate{Root: "/workspace", Path: "/workspace/docs/unmatched.md"}, fileIdentity{dev: 1, ino: 99, valid: true}, now)
	index.markDiscoverDir("/workspace/docs")

	changes, pending := index.drain(now.Add(workspaceMissingPathDelay + time.Millisecond))
	if pending {
		t.Fatal("matched move should not leave pending missing paths")
	}
	if got := changes.DirtyDocumentIDs; len(got) != 1 || got[0] != "doc_dirty" {
		t.Fatalf("dirty documents = %#v", got)
	}
	if got := changes.LocalMoves; len(got) != 1 || got[0].DocumentID != "doc_move" || got[0].OldPath != oldPath || got[0].NewPath != newPath {
		t.Fatalf("local moves = %#v", got)
	}
	if got := changes.LocalCreates; len(got) != 1 || got[0].Path != "/workspace/docs/unmatched.md" {
		t.Fatalf("local creates = %#v", got)
	}
	if len(changes.LocalDeletes) != 0 {
		t.Fatalf("unexpected local deletes: %#v", changes.LocalDeletes)
	}
	if got := index.drainDiscoverDirs(); len(got) != 1 || got[0] != "/workspace/docs" {
		t.Fatalf("discover dirs = %#v", got)
	}
}

func TestWorkspaceChangeIndexDelaysUnmatchedMissingBeforeDelete(t *testing.T) {
	now := time.Now()
	index := newWorkspaceChangeIndex()
	index.markPendingMissing("doc_gone", "/workspace/docs/gone.md", now)

	changes, pending := index.drain(now.Add(workspaceMissingPathDelay / 2))
	if !pending {
		t.Fatal("missing path should remain pending before delay")
	}
	if len(changes.LocalDeletes) != 0 {
		t.Fatalf("delete should not be emitted before delay: %#v", changes.LocalDeletes)
	}

	changes, pending = index.drain(now.Add(workspaceMissingPathDelay + time.Millisecond))
	if pending {
		t.Fatal("missing path should be drained after delay")
	}
	if got := changes.LocalDeletes; len(got) != 1 || got[0].DocumentID != "doc_gone" {
		t.Fatalf("local deletes = %#v", got)
	}
}

func TestWorkspaceChangeIndexDelaysUnmatchedIdentityCreateWithoutResettingDeadline(t *testing.T) {
	now := time.Now()
	path := "/workspace/docs/new.md"
	identity := fileIdentity{dev: 1, ino: 42, valid: true}
	index := newWorkspaceChangeIndex()
	index.markLocalCreate(localCreateCandidate{Root: "/workspace", Path: path}, identity, now)

	changes, pending := index.drain(now.Add(workspaceMissingPathDelay / 2))
	if !pending {
		t.Fatal("identity-bearing create should remain pending during the coalescing window")
	}
	if len(changes.LocalCreates) != 0 {
		t.Fatalf("create emitted before coalescing window: %#v", changes.LocalCreates)
	}

	// Duplicate write/create notifications must not extend the first observation's
	// deadline indefinitely for a genuinely new file.
	index.markLocalCreate(localCreateCandidate{Root: "/workspace", Path: path}, identity, now.Add(workspaceMissingPathDelay/2))
	changes, pending = index.drain(now.Add(workspaceMissingPathDelay + time.Millisecond))
	if pending {
		t.Fatal("expired create should not leave pending work")
	}
	if got := changes.LocalCreates; len(got) != 1 || got[0].Path != path {
		t.Fatalf("local creates = %#v, want only %q", got, path)
	}
}

func TestWorkspaceReplicaEventPathDoesNotFullScanForTargetedChanges(t *testing.T) {
	root := t.TempDir()
	trackedPath := filepath.Join(root, "docs", "tracked.md")
	if err := os.MkdirAll(filepath.Dir(trackedPath), 0o755); err != nil {
		t.Fatalf("mkdir tracked: %v", err)
	}
	if err := os.WriteFile(trackedPath, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	unrelatedPath := filepath.Join(root, "unrelated.md")
	if err := os.WriteFile(unrelatedPath, []byte("unrelated\n"), 0o644); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}

	var dirty []string
	var creates []localCreateCandidate
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(documentID string) {
			dirty = append(dirty, documentID)
		},
		markCreate: func(candidate localCreateCandidate) {
			creates = append(creates, candidate)
		},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	tracked := &trackedFile{
		DocumentID:    "doc_tracked",
		DocumentPath:  "docs/tracked.md",
		Path:          trackedPath,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent("base\n")
	replica.projectedByPath[trackedPath] = tracked
	replica.projectedByID[tracked.DocumentID] = tracked
	replica.recordTrackedIdentity(trackedPath)

	var scans atomic.Int32
	previousScan := scanWorkspaceFilesForReconcile
	scanWorkspaceFilesForReconcile = func(root string) (map[string]string, error) {
		scans.Add(1)
		return previousScan(root)
	}
	defer func() { scanWorkspaceFilesForReconcile = previousScan }()

	if err := replica.handleWatcherEvent(fsnotify.Event{Name: trackedPath, Op: fsnotify.Write}, time.Now()); err != nil {
		t.Fatalf("handle tracked write: %v", err)
	}
	if _, err := replica.drainPathChanges(context.Background(), time.Now()); err != nil {
		t.Fatalf("drain tracked write: %v", err)
	}
	if scans.Load() != 0 {
		t.Fatalf("event path called full workspace scan %d time(s)", scans.Load())
	}
	if !containsTestString(dirty, "doc_tracked") {
		t.Fatalf("tracked write did not mark document dirty: %#v", dirty)
	}
	if !tracked.isLocalDirty() {
		t.Fatal("tracked write did not set local dirty state")
	}
	if len(creates) != 0 {
		t.Fatalf("tracked write should not produce local creates: %#v", creates)
	}

	newPath := filepath.Join(root, "docs", "new.md")
	if err := os.WriteFile(newPath, []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	createTime := time.Now()
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: newPath, Op: fsnotify.Create}, createTime); err != nil {
		t.Fatalf("handle create: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), createTime); err != nil {
		t.Fatalf("drain create: %v", err)
	} else if !pending {
		t.Fatal("identity-bearing create should remain pending during the move coalescing window")
	}
	if scans.Load() != 0 {
		t.Fatalf("create event path called full workspace scan %d time(s)", scans.Load())
	}
	if len(creates) != 0 {
		t.Fatalf("local create emitted before coalescing delay: %#v", creates)
	}
	if pending, err := replica.drainPathChanges(context.Background(), createTime.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain delayed create: %v", err)
	} else if pending {
		t.Fatal("expired local create should not leave pending work")
	}
	if len(creates) != 1 || creates[0].Path != newPath {
		t.Fatalf("local creates after create = %#v", creates)
	}
}

func TestReconcileDirtyDocumentsDoesNotPreScanWorkspace(t *testing.T) {
	root := t.TempDir()
	var scans atomic.Int32
	previousScan := scanWorkspaceFilesForReconcile
	scanWorkspaceFilesForReconcile = func(root string) (map[string]string, error) {
		scans.Add(1)
		return previousScan(root)
	}
	defer func() { scanWorkspaceFilesForReconcile = previousScan }()

	runtime := &workspaceRuntime{
		replica: &workspaceReplica{
			rootDir: root,
		},
		reconcileQueue: newReconcileQueue(),
	}
	runtime.markDocumentDirty("doc_1")
	if err := runtime.reconcileDirtyDocuments(context.Background()); err != nil {
		t.Fatalf("reconcile dirty documents: %v", err)
	}
	if scans.Load() != 0 {
		t.Fatalf("dirty reconcile called full workspace scan %d time(s)", scans.Load())
	}
}

func TestWorkspaceReplicaEventPathCoalescesTrackedDeleteAndMove(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "docs", "old.md")
	newPath := filepath.Join(root, "renamed", "old.md")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir old: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}

	var dirty []string
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(documentID string) {
			dirty = append(dirty, documentID)
		},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	tracked := &trackedFile{
		DocumentID:    "doc_move",
		DocumentPath:  "docs/old.md",
		Path:          oldPath,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent("same\n")
	replica.projectedByPath[oldPath] = tracked
	replica.projectedByID[tracked.DocumentID] = tracked
	replica.recordTrackedIdentity(oldPath)

	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatalf("mkdir new: %v", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	now := time.Now()
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: oldPath, Op: fsnotify.Rename}, now); err != nil {
		t.Fatalf("handle rename old: %v", err)
	}
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: newPath, Op: fsnotify.Create}, now.Add(time.Millisecond)); err != nil {
		t.Fatalf("handle rename new: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(time.Millisecond)); err != nil {
		t.Fatalf("drain move: %v", err)
	} else if pending {
		t.Fatal("matched move should not leave pending missing work")
	}
	if tracked.Path != newPath {
		t.Fatalf("tracked path = %q, want %q", tracked.Path, newPath)
	}
	if !tracked.isLocalMoved() || !tracked.isLocalDirty() {
		t.Fatal("matched move should mark tracked file locally moved and dirty")
	}
	if !containsTestString(dirty, "doc_move") {
		t.Fatalf("move did not mark document dirty: %#v", dirty)
	}
}

func TestWorkspaceReplicaEventPathExpiresUnmatchedDelete(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "gone.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("gone\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	replica := &workspaceReplica{
		rootDir:         root,
		actorID:         "daemon_agent",
		actorType:       "daemon",
		markDirty:       func(string) {},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	tracked := &trackedFile{
		DocumentID:    "doc_gone",
		DocumentPath:  "docs/gone.md",
		Path:          path,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent("gone\n")
	replica.projectedByPath[path] = tracked
	replica.projectedByID[tracked.DocumentID] = tracked
	replica.recordTrackedIdentity(path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	now := time.Now()
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: path, Op: fsnotify.Remove}, now); err != nil {
		t.Fatalf("handle remove: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay/2)); err != nil {
		t.Fatalf("drain early delete: %v", err)
	} else if !pending {
		t.Fatal("delete should stay pending before delay")
	}
	if tracked.isLocalDeleted() {
		t.Fatal("tracked delete should not be marked before missing delay")
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain expired delete: %v", err)
	} else if pending {
		t.Fatal("delete should not stay pending after delay")
	}
	if !tracked.isLocalDeleted() || !tracked.isLocalDirty() {
		t.Fatal("expired missing path should mark tracked file locally deleted and dirty")
	}
}

func TestWorkspaceReplicaEventPathTrackedReplaceAtSamePathCancelsMissing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "tracked.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}

	var dirty []string
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(documentID string) {
			dirty = append(dirty, documentID)
		},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	tracked := &trackedFile{
		DocumentID:    "doc_replace",
		DocumentPath:  "docs/tracked.md",
		Path:          path,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent("base\n")
	replica.projectedByPath[path] = tracked
	replica.projectedByID[tracked.DocumentID] = tracked
	replica.recordTrackedIdentity(path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove base: %v", err)
	}
	now := time.Now()
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: path, Op: fsnotify.Remove}, now); err != nil {
		t.Fatalf("handle remove: %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: path, Op: fsnotify.Create}, now.Add(time.Millisecond)); err != nil {
		t.Fatalf("handle replacement create: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain same-path replacement: %v", err)
	} else if pending {
		t.Fatal("same-path replacement should not leave pending missing work")
	}
	if tracked.isLocalDeleted() {
		t.Fatal("same-path replacement should not become local delete")
	}
	if !tracked.isLocalDirty() {
		t.Fatal("same-path replacement should mark tracked file dirty")
	}
	if !containsTestString(dirty, "doc_replace") {
		t.Fatalf("same-path replacement did not mark document dirty: %#v", dirty)
	}
}

func TestWorkspaceReplicaEventPathTreatsUntrackedWriteAsLocalCreate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "new.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	var creates []localCreateCandidate
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(string) {},
		markCreate: func(candidate localCreateCandidate) {
			creates = append(creates, candidate)
		},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}

	var scans atomic.Int32
	previousScan := scanWorkspaceFilesForReconcile
	scanWorkspaceFilesForReconcile = func(root string) (map[string]string, error) {
		scans.Add(1)
		return previousScan(root)
	}
	defer func() { scanWorkspaceFilesForReconcile = previousScan }()

	now := time.Now()
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: path, Op: fsnotify.Write}, now); err != nil {
		t.Fatalf("handle untracked write: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now); err != nil {
		t.Fatalf("drain untracked write: %v", err)
	} else if !pending {
		t.Fatal("untracked write should remain pending during the move coalescing window")
	}
	if scans.Load() != 0 {
		t.Fatalf("untracked write called full workspace scan %d time(s)", scans.Load())
	}
	if len(creates) != 0 {
		t.Fatalf("untracked write emitted create before coalescing delay: %#v", creates)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain delayed untracked write: %v", err)
	} else if pending {
		t.Fatal("expired untracked write should not leave pending work")
	}
	if len(creates) != 1 || creates[0].Path != path {
		t.Fatalf("local creates after untracked write = %#v", creates)
	}
}

func TestWorkspaceReplicaDirectoryCreateDiscoversOnlySubtree(t *testing.T) {
	root := t.TempDir()
	newDir := filepath.Join(root, "created")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir new dir: %v", err)
	}
	inside := filepath.Join(newDir, "inside.md")
	if err := os.WriteFile(inside, []byte("inside\n"), 0o644); err != nil {
		t.Fatalf("write inside: %v", err)
	}
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	var creates []localCreateCandidate
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(string) {},
		markCreate: func(candidate localCreateCandidate) {
			creates = append(creates, candidate)
		},
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: newDir, Op: fsnotify.Create}, time.Now()); err != nil {
		t.Fatalf("handle dir create: %v", err)
	}
	now := time.Now()
	if pending, err := replica.drainPathChanges(context.Background(), now); err != nil {
		t.Fatalf("drain dir create: %v", err)
	} else if !pending {
		t.Fatal("directory discovery should hold creates during the move coalescing window")
	}
	if len(creates) != 0 {
		t.Fatalf("directory discovery emitted creates before coalescing delay: %#v", creates)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain delayed dir create: %v", err)
	} else if pending {
		t.Fatal("expired directory discovery should not leave pending work")
	}
	if len(creates) != 1 || creates[0].Path != inside {
		t.Fatalf("directory discovery creates = %#v, want only %q", creates, inside)
	}
}

func TestWorkspaceReplicaDirectoryCreateCanCoalesceTrackedMove(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old")
	newDir := filepath.Join(root, "new")
	oldPath := filepath.Join(oldDir, "tracked.md")
	newPath := filepath.Join(newDir, "tracked.md")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("mkdir old dir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}

	var dirty []string
	var creates []localCreateCandidate
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(documentID string) {
			dirty = append(dirty, documentID)
		},
		markCreate: func(candidate localCreateCandidate) {
			creates = append(creates, candidate)
		},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	tracked := &trackedFile{
		DocumentID:    "doc_move",
		DocumentPath:  "old/tracked.md",
		Path:          oldPath,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent("same\n")
	replica.projectedByPath[oldPath] = tracked
	replica.projectedByID[tracked.DocumentID] = tracked
	replica.recordTrackedIdentity(oldPath)

	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatalf("rename dir: %v", err)
	}
	now := time.Now()
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: oldPath, Op: fsnotify.Rename}, now); err != nil {
		t.Fatalf("handle old path rename: %v", err)
	}
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: newDir, Op: fsnotify.Create}, now.Add(time.Millisecond)); err != nil {
		t.Fatalf("handle new dir create: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(time.Millisecond)); err != nil {
		t.Fatalf("drain directory move: %v", err)
	} else if pending {
		t.Fatal("directory move should not leave pending missing work")
	}
	if tracked.Path != newPath {
		t.Fatalf("tracked path = %q, want %q", tracked.Path, newPath)
	}
	if !tracked.isLocalMoved() || !tracked.isLocalDirty() {
		t.Fatal("directory move should mark tracked file locally moved and dirty")
	}
	if !containsTestString(dirty, "doc_move") {
		t.Fatalf("directory move did not mark document dirty: %#v", dirty)
	}
	if len(creates) != 0 {
		t.Fatalf("matched directory move should not create new local documents: %#v", creates)
	}
}

func TestWorkspaceReplicaDirectoryDiscoveryWaitsForTrackedRenamePair(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old", "tracked.md")
	newDir := filepath.Join(root, "new")
	newPath := filepath.Join(newDir, "tracked.md")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir old path: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("write old path: %v", err)
	}

	var dirty []string
	var creates []localCreateCandidate
	replica := &workspaceReplica{
		rootDir:   root,
		actorID:   "daemon_agent",
		actorType: "daemon",
		markDirty: func(documentID string) {
			dirty = append(dirty, documentID)
		},
		markCreate: func(candidate localCreateCandidate) {
			creates = append(creates, candidate)
		},
		fs:              NewWorkspaceFS(root),
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	tracked := &trackedFile{
		DocumentID:    "doc_move",
		DocumentPath:  "old/tracked.md",
		Path:          oldPath,
		WorkspaceRoot: root,
		FS:            replica.fs,
		Owner:         replica,
	}
	tracked.setProjectedContent("same\n")
	replica.projectedByPath[oldPath] = tracked
	replica.projectedByID[tracked.DocumentID] = tracked
	replica.recordTrackedIdentity(oldPath)

	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir new directory: %v", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename tracked file: %v", err)
	}
	now := time.Now()
	// mkdir precedes mv, so the new-directory event can be handled and drained
	// before the old-path rename event reaches the daemon.
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: newDir, Op: fsnotify.Create}, now); err != nil {
		t.Fatalf("handle new directory before old rename: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now); err != nil {
		t.Fatalf("drain directory discovery before old rename: %v", err)
	} else if !pending {
		t.Fatal("discovered file should wait for a matching rename during the coalescing window")
	}
	if len(creates) != 0 {
		t.Fatalf("directory discovery emitted a new document before rename pairing: %#v", creates)
	}

	if err := replica.handleWatcherEvent(fsnotify.Event{Name: oldPath, Op: fsnotify.Rename}, now.Add(time.Millisecond)); err != nil {
		t.Fatalf("handle delayed old-path rename: %v", err)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(time.Millisecond)); err != nil {
		t.Fatalf("drain paired local move: %v", err)
	} else if pending {
		t.Fatal("paired local move should not leave pending path changes")
	}
	if tracked.Path != newPath {
		t.Fatalf("tracked path = %q, want %q", tracked.Path, newPath)
	}
	if !tracked.isLocalMoved() || !tracked.isLocalDirty() {
		t.Fatal("paired local move should mark tracked file locally moved and dirty")
	}
	if tracked.isLocalDeleted() {
		t.Fatal("paired local move was misclassified as a local delete")
	}
	if !containsTestString(dirty, tracked.DocumentID) {
		t.Fatalf("paired local move did not mark document dirty: %#v", dirty)
	}
	if len(creates) != 0 {
		t.Fatalf("paired local move created a duplicate local document: %#v", creates)
	}
	if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
		t.Fatalf("drain after local move coalescing window: %v", err)
	} else if pending {
		t.Fatal("paired local move left delayed path work")
	}
	if tracked.isLocalDeleted() || len(creates) != 0 {
		t.Fatalf("paired local move matured false artifacts: deleted=%t creates=%#v", tracked.isLocalDeleted(), creates)
	}
}

func TestWorkspaceReplicaAuthoritativePathRekeyRetiresWatcherMoveArtifacts(t *testing.T) {
	for _, createBeforeRekey := range []bool{false, true} {
		name := "create-after-rekey"
		if createBeforeRekey {
			name = "create-before-rekey"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			oldPath := filepath.Join(root, "old", "tracked.md")
			newPath := filepath.Join(root, "new", "tracked.md")
			if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
				t.Fatalf("mkdir old path: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
				t.Fatalf("mkdir new path: %v", err)
			}
			if err := os.WriteFile(oldPath, []byte("same\n"), 0o644); err != nil {
				t.Fatalf("write old path: %v", err)
			}

			var creates []localCreateCandidate
			replica := &workspaceReplica{
				rootDir:   root,
				actorID:   "daemon_agent",
				actorType: "daemon",
				markDirty: func(string) {},
				markCreate: func(candidate localCreateCandidate) {
					creates = append(creates, candidate)
				},
				fs:              NewWorkspaceFS(root),
				projectedByPath: map[string]*trackedFile{},
				projectedByID:   map[string]*trackedFile{},
				changes:         newWorkspaceChangeIndex(),
			}
			tracked := &trackedFile{
				DocumentID:    "doc_projected_move",
				DocumentPath:  "new/tracked.md",
				Path:          oldPath,
				WorkspaceRoot: root,
				FS:            replica.fs,
				Owner:         replica,
			}
			tracked.setProjectedContent("same\n")
			replica.projectedByPath[oldPath] = tracked
			replica.projectedByID[tracked.DocumentID] = tracked
			replica.recordTrackedIdentity(oldPath)

			if err := os.Rename(oldPath, newPath); err != nil {
				t.Fatalf("project file move: %v", err)
			}
			now := time.Now()
			if err := replica.handleWatcherEvent(fsnotify.Event{Name: oldPath, Op: fsnotify.Rename}, now); err != nil {
				t.Fatalf("handle old-path rename: %v", err)
			}
			if createBeforeRekey {
				if err := replica.handleWatcherEvent(fsnotify.Event{Name: newPath, Op: fsnotify.Create}, now.Add(time.Millisecond)); err != nil {
					t.Fatalf("handle new-path create before rekey: %v", err)
				}
			}

			replica.setTrackedPath(tracked, newPath)

			if !createBeforeRekey {
				if err := replica.handleWatcherEvent(fsnotify.Event{Name: newPath, Op: fsnotify.Create}, now.Add(time.Millisecond)); err != nil {
					t.Fatalf("handle new-path create after rekey: %v", err)
				}
			}
			if pending, err := replica.drainPathChanges(context.Background(), now.Add(workspaceMissingPathDelay+time.Millisecond)); err != nil {
				t.Fatalf("drain projected move artifacts: %v", err)
			} else if pending {
				t.Fatal("projected move should not leave pending path changes")
			}
			if tracked.Path != newPath {
				t.Fatalf("tracked path = %q, want %q", tracked.Path, newPath)
			}
			if tracked.isLocalMoved() {
				t.Fatal("projected move was misclassified as a local move")
			}
			if tracked.isLocalDeleted() {
				t.Fatal("projected move was misclassified as a local delete")
			}
			if len(creates) != 0 {
				t.Fatalf("projected move created local documents: %#v", creates)
			}
		})
	}
}

func TestWorkspaceReplicaIgnoresDotAndTempPaths(t *testing.T) {
	root := t.TempDir()
	ignoredPath := filepath.Join(root, ".git", "config")
	if err := os.MkdirAll(filepath.Dir(ignoredPath), 0o755); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	if err := os.WriteFile(ignoredPath, []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	var dirty []string
	var creates []localCreateCandidate
	replica := &workspaceReplica{
		rootDir: root,
		markDirty: func(documentID string) {
			dirty = append(dirty, documentID)
		},
		markCreate: func(candidate localCreateCandidate) {
			creates = append(creates, candidate)
		},
		projectedByPath: map[string]*trackedFile{},
		projectedByID:   map[string]*trackedFile{},
		changes:         newWorkspaceChangeIndex(),
	}
	if err := replica.handleWatcherEvent(fsnotify.Event{Name: ignoredPath, Op: fsnotify.Create | fsnotify.Write}, time.Now()); err != nil {
		t.Fatalf("handle ignored event: %v", err)
	}
	if _, err := replica.drainPathChanges(context.Background(), time.Now()); err != nil {
		t.Fatalf("drain ignored event: %v", err)
	}
	if len(dirty) != 0 || len(creates) != 0 {
		t.Fatalf("ignored path created work: dirty=%#v creates=%#v", dirty, creates)
	}
}

func containsTestString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
