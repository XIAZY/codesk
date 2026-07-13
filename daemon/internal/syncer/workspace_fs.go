package syncer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrDivergedWorkingCopy = errors.New("working copy diverged from expected hash")
var ErrPathCollision = errors.New("target path already exists")
var ErrUnsafeDelete = errors.New("refusing to delete non-matching working copy")
var ErrOutsideWorkspace = errors.New("path is outside workspace")

type WorkspaceFS struct {
	Root          string
	pathLocks     pathLockLeaseStore
	openPathLocks pathLockStoreOpener
}

type FileSnapshot struct {
	Path   string
	Exists bool
	Bytes  []byte
	Hash   projectedContentHash
}

type FSError struct {
	Op         string
	Path       string
	TargetPath string
	Err        error
}

func (e *FSError) Error() string {
	if e == nil {
		return ""
	}
	if e.TargetPath != "" {
		return fmt.Sprintf("%s %s -> %s: %v", e.Op, e.Path, e.TargetPath, e.Err)
	}
	return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err)
}

func (e *FSError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewWorkspaceFS(root string) *WorkspaceFS {
	return newWorkspaceFS(root, nil)
}

func newWorkspaceFS(root string, pathLocks pathLockLeaseStore) *WorkspaceFS {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	return &WorkspaceFS{Root: abs, pathLocks: pathLocks, openPathLocks: openPathLockStore}
}

func (fs *WorkspaceFS) pathLockStore() (pathLockLeaseStore, error) {
	opener := fs.openPathLocks
	if opener == nil {
		opener = openPathLockStore
	}
	return opener(fs.Root)
}

func (fs *WorkspaceFS) CleanupStaleLocks() error {
	if fs == nil || fs.Root == "" {
		return nil
	}
	if fs.pathLocks != nil {
		if err := fs.pathLocks.cleanupExpired(time.Now().UTC()); err != nil {
			return &FSError{Op: "cleanup-locks", Path: fs.Root, Err: err}
		}
		return nil
	}
	store, err := fs.pathLockStore()
	if err != nil {
		return &FSError{Op: "cleanup-locks", Path: fs.Root, Err: err}
	}
	if err := store.cleanupExpired(time.Now().UTC()); err != nil {
		_ = store.Close()
		return &FSError{Op: "cleanup-locks", Path: fs.Root, Err: err}
	}
	if err := store.Close(); err != nil {
		return &FSError{Op: "close-locks", Path: fs.Root, Err: err}
	}
	return nil
}

func (fs *WorkspaceFS) Read(path string) (FileSnapshot, error) {
	path, err := fs.cleanPath(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	unlock, err := fs.lockPaths(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	defer unlock()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileSnapshot{Path: path, Exists: false}, nil
	}
	if err != nil {
		return FileSnapshot{}, &FSError{Op: "read", Path: path, Err: err}
	}
	return FileSnapshot{
		Path:   path,
		Exists: true,
		Bytes:  content,
		Hash:   projectedHashBytes(content),
	}, nil
}

func (fs *WorkspaceFS) Append(path string, content string) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	unlock, err := fs.lockPaths(path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &FSError{Op: "append", Path: path, Err: err}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return &FSError{Op: "append", Path: path, Err: err}
	}
	defer file.Close()
	if err := writeFullString(file, content); err != nil {
		return &FSError{Op: "append", Path: path, Err: err}
	}
	return nil
}

// CreateEmptyOrRead creates an empty path when absent. If another writer has
// already created it, that local content wins and is returned unchanged.
func (fs *WorkspaceFS) CreateEmptyOrRead(path string) (FileSnapshot, error) {
	return fs.createEmptyOrRead(path, createEmptyFileExclusive)
}

func (fs *WorkspaceFS) createEmptyOrRead(path string, createEmpty func(string) error) (FileSnapshot, error) {
	path, err := fs.cleanPath(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	unlock, err := fs.lockPaths(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileSnapshot{}, &FSError{Op: "create-empty-or-read", Path: path, Err: err}
	}
	err = createEmpty(path)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return FileSnapshot{}, &FSError{Op: "create-empty-or-read", Path: path, Err: err}
	}
	existing, err := readFileObservation(path)
	if err != nil {
		return FileSnapshot{}, &FSError{Op: "create-empty-or-read", Path: path, Err: err}
	}
	return FileSnapshot{Path: path, Exists: true, Bytes: existing, Hash: projectedHashBytes(existing)}, nil
}

func createEmptyFileExclusive(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

func (fs *WorkspaceFS) WriteIfUnchanged(path string, expected projectedContentHash, content []byte) error {
	return fs.writeIfUnchangedWith(path, expected, content, replaceFileAtomically)
}

func (fs *WorkspaceFS) writeIfUnchangedWith(
	path string,
	expected projectedContentHash,
	content []byte,
	replaceFile func(path, content string, mode os.FileMode) error,
) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	unlock, err := fs.lockPaths(path)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := os.ReadFile(path)
	exists := true
	if errors.Is(err, os.ErrNotExist) {
		current = nil
		exists = false
	} else if err != nil {
		return &FSError{Op: "write", Path: path, Err: err}
	}
	currentHash := projectedHashBytes(current)
	if isKnownProjectedHash(expected) && currentHash != expected {
		return &FSError{Op: "write", Path: path, Err: ErrDivergedWorkingCopy}
	}
	if exists && currentHash == projectedHashBytes(content) {
		return nil
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := replaceFile(path, string(content), mode); err != nil {
		return &FSError{Op: "write", Path: path, Err: err}
	}
	return nil
}

func (fs *WorkspaceFS) DeleteIfUnchanged(path string, expected projectedContentHash) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	unlock, err := fs.lockPaths(path)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &FSError{Op: "delete", Path: path, Err: err}
	}
	if isKnownProjectedHash(expected) && projectedHashBytes(current) != expected {
		return &FSError{Op: "delete", Path: path, Err: ErrUnsafeDelete}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &FSError{Op: "delete", Path: path, Err: err}
	}
	return nil
}

func (fs *WorkspaceFS) MoveIfNoTarget(from string, to string) error {
	from, err := fs.cleanPath(from)
	if err != nil {
		return err
	}
	to, err = fs.cleanPath(to)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	unlock, err := fs.lockPaths(from, to)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(to); err == nil {
		return &FSError{Op: "move", Path: from, TargetPath: to, Err: ErrPathCollision}
	} else if !errors.Is(err, os.ErrNotExist) {
		return &FSError{Op: "move", Path: from, TargetPath: to, Err: err}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return &FSError{Op: "move", Path: from, TargetPath: to, Err: err}
	}
	if err := os.Rename(from, to); err != nil {
		return &FSError{Op: "move", Path: from, TargetPath: to, Err: err}
	}
	return nil
}

func (fs *WorkspaceFS) Archive(path string, reason string) (string, error) {
	path, err := fs.cleanPath(path)
	if err != nil {
		return "", err
	}
	reason = safeDocumentCacheName(firstNonEmptyText(reason, "recovered"))
	for i := 0; i < 100; i++ {
		archivePath := fs.archivePath(path, reason, i)
		unlock, err := fs.lockPaths(path, archivePath)
		if err != nil {
			return "", err
		}
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			unlock()
			return "", nil
		} else if statErr != nil {
			unlock()
			return "", &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: statErr}
		}
		if _, statErr := os.Stat(archivePath); statErr == nil {
			unlock()
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			unlock()
			return "", &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: statErr}
		}
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			unlock()
			return "", &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: err}
		}
		err = os.Rename(path, archivePath)
		if err == nil {
			unlock()
			return archivePath, nil
		}
		if !isCrossDeviceError(err) {
			unlock()
			return "", &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: err}
		}
		if copyErr := copyThenRemove(path, archivePath); copyErr != nil {
			unlock()
			return "", &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: copyErr}
		}
		unlock()
		return archivePath, nil
	}
	return "", &FSError{Op: "archive", Path: path, Err: errors.New("failed to allocate recovered workspace file name")}
}

func (fs *WorkspaceFS) EnsureParent(path string) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &FSError{Op: "ensure-parent", Path: path, Err: err}
	}
	return nil
}

func (fs *WorkspaceFS) cleanPath(path string) (string, error) {
	if fs == nil || strings.TrimSpace(fs.Root) == "" {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", &FSError{Op: "resolve", Path: path, Err: err}
	}
	rel, err := filepath.Rel(fs.Root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &FSError{Op: "resolve", Path: path, Err: ErrOutsideWorkspace}
	}
	return abs, nil
}

func (fs *WorkspaceFS) lockPaths(paths ...string) (func(), error) {
	if fs == nil || fs.Root == "" || len(paths) == 0 {
		return func() {}, nil
	}
	lockPaths := make([]string, 0, len(paths))
	seen := map[string]struct{}{}
	for _, path := range paths {
		rel, err := filepath.Rel(fs.Root, path)
		if err != nil {
			return nil, &FSError{Op: "lock", Path: path, Err: err}
		}
		keyPath := filepath.ToSlash(rel)
		if _, ok := seen[keyPath]; ok {
			continue
		}
		seen[keyPath] = struct{}{}
		lockPaths = append(lockPaths, keyPath)
	}
	store := fs.pathLocks
	closeStore := false
	if store == nil {
		var err error
		store, err = fs.pathLockStore()
		if err != nil {
			return nil, &FSError{Op: "lock", Path: fs.Root, Err: err}
		}
		closeStore = true
	}
	leases, err := store.lock(lockPaths)
	if err != nil {
		if closeStore {
			_ = store.Close()
		}
		return nil, &FSError{Op: "lock", Path: fs.Root, Err: err}
	}
	var unlockOnce sync.Once
	return func() {
		unlockOnce.Do(func() {
			_ = store.release(leases)
			if closeStore {
				_ = store.Close()
			}
		})
	}, nil
}

func (fs *WorkspaceFS) archivePath(path, reason string, attempt int) string {
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		base = "document"
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	name := stamp + "-" + base
	if attempt > 0 {
		name = fmt.Sprintf("%s-%d-%s", stamp, attempt, base)
	}
	return filepath.Join(fs.Root, ".notty", "recovered", reason, name)
}

func copyThenRemove(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	return os.Remove(from)
}
