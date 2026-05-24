package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ErrDivergedWorkingCopy = errors.New("working copy diverged from expected hash")
var ErrPathCollision = errors.New("target path already exists")
var ErrUnsafeDelete = errors.New("refusing to delete non-matching working copy")
var ErrOutsideWorkspace = errors.New("path is outside workspace")

type WorkspaceFS struct {
	Root  string
	Locks *FSLockDB
	State *WorkspaceStateDB
}

type FileSnapshot struct {
	Path   string
	Exists bool
	Stat   FileStat
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
	fs, err := OpenWorkspaceFS(root)
	if err != nil {
		panic(fmt.Sprintf("open workspace fs: %v", err))
	}
	return fs
}

func OpenWorkspaceFS(root string) (*WorkspaceFS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	lock, err := OpenFSLockDB(abs, "workspace-fs")
	if err != nil {
		return nil, err
	}
	return &WorkspaceFS{Root: abs, Locks: lock}, nil
}

func (fs *WorkspaceFS) Close() error {
	if fs == nil || fs.Locks == nil {
		return nil
	}
	err := fs.Locks.Close()
	fs.Locks = nil
	return err
}

func (fs *WorkspaceFS) Read(path string) (FileSnapshot, error) {
	path, err := fs.cleanPath(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	var snapshot FileSnapshot
	err = fs.withFilesystemLock("read", path, "", func() error {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshot = FileSnapshot{Path: path, Exists: false}
			return nil
		}
		if err != nil {
			return &FSError{Op: "read", Path: path, Err: err}
		}
		info, statErr := os.Lstat(path)
		stat := FileStat{Path: path, Exists: true}
		if statErr == nil {
			stat = fileStatFromInfo(path, info)
		}
		snapshot = FileSnapshot{
			Path:   path,
			Exists: true,
			Stat:   stat,
			Bytes:  content,
			Hash:   projectedHashBytes(content),
		}
		return nil
	})
	if err != nil {
		return FileSnapshot{}, err
	}
	return snapshot, nil
}

func (fs *WorkspaceFS) Append(path string, content string) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	return fs.withFilesystemLock("append", path, "", func() error {
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
	})
}

func (fs *WorkspaceFS) WriteIfUnchanged(path string, expected projectedContentHash, content []byte) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	return fs.withFilesystemLock("write-if-unchanged", path, "", func() error {
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
		if err := replaceFileAtomically(path, string(content), mode); err != nil {
			return &FSError{Op: "write", Path: path, Err: err}
		}
		return nil
	})
}

func (fs *WorkspaceFS) WriteIfSHA256Unchanged(ctx context.Context, path string, expectedSHA256 string, content []byte) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	expectedSHA256 = strings.TrimSpace(expectedSHA256)
	return fs.withFilesystemLockContext(ctx, "write-if-sha256-unchanged", path, "", func() error {
		current, err := os.ReadFile(path)
		exists := true
		if errors.Is(err, os.ErrNotExist) {
			current = nil
			exists = false
		} else if err != nil {
			return &FSError{Op: "write", Path: path, Err: err}
		}
		if expectedSHA256 != "" && sha256HexBytes(current) != expectedSHA256 {
			return &FSError{Op: "write", Path: path, Err: ErrDivergedWorkingCopy}
		}
		if exists && sha256HexBytes(current) == sha256HexBytes(content) {
			return nil
		}
		mode := os.FileMode(0o644)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if err := replaceFileAtomically(path, string(content), mode); err != nil {
			return &FSError{Op: "write", Path: path, Err: err}
		}
		return nil
	})
}

func (fs *WorkspaceFS) DeleteIfUnchanged(path string, expected projectedContentHash) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	return fs.withFilesystemLock("delete-if-unchanged", path, "", func() error {
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
	})
}

func (fs *WorkspaceFS) DeleteIfSHA256Unchanged(ctx context.Context, path string, expectedSHA256 string) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	expectedSHA256 = strings.TrimSpace(expectedSHA256)
	return fs.withFilesystemLockContext(ctx, "delete-if-sha256-unchanged", path, "", func() error {
		current, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return &FSError{Op: "delete", Path: path, Err: err}
		}
		if expectedSHA256 != "" && sha256HexBytes(current) != expectedSHA256 {
			return &FSError{Op: "delete", Path: path, Err: ErrUnsafeDelete}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &FSError{Op: "delete", Path: path, Err: err}
		}
		return nil
	})
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
	return fs.withFilesystemLock("move-if-no-target", from, to, func() error {
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
	})
}

func (fs *WorkspaceFS) Archive(path string, reason string) (string, error) {
	path, err := fs.cleanPath(path)
	if err != nil {
		return "", err
	}
	reason = safeWorkspaceArchiveName(firstNonEmptyText(reason, "recovered"))
	for i := 0; i < 100; i++ {
		archivePath := fs.archivePath(path, reason, i)
		var archived string
		err := fs.withFilesystemLock("archive", path, archivePath, func() error {
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				return nil
			} else if statErr != nil {
				return &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: statErr}
			}
			if _, statErr := os.Stat(archivePath); statErr == nil {
				return ErrPathCollision
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: statErr}
			}
			if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
				return &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: err}
			}
			err = os.Rename(path, archivePath)
			if err == nil {
				archived = archivePath
				return nil
			}
			if !errors.Is(err, syscall.EXDEV) {
				return &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: err}
			}
			if copyErr := copyThenRemove(path, archivePath); copyErr != nil {
				return &FSError{Op: "archive", Path: path, TargetPath: archivePath, Err: copyErr}
			}
			archived = archivePath
			return nil
		})
		if errors.Is(err, ErrPathCollision) {
			continue
		}
		if err != nil {
			return "", err
		}
		if archived == "" {
			return "", nil
		}
		return archived, nil
	}
	return "", &FSError{Op: "archive", Path: path, Err: errors.New("failed to allocate recovered workspace file name")}
}

func safeWorkspaceArchiveName(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('_')
	}
	if builder.Len() == 0 {
		return "recovered"
	}
	return builder.String()
}

func (fs *WorkspaceFS) EnsureParent(path string) error {
	path, err := fs.cleanPath(path)
	if err != nil {
		return err
	}
	return fs.withFilesystemLock("ensure-parent", path, "", func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return &FSError{Op: "ensure-parent", Path: path, Err: err}
		}
		return nil
	})
}

func (fs *WorkspaceFS) cleanPath(path string) (string, error) {
	if fs == nil || strings.TrimSpace(fs.Root) == "" {
		return filepath.Clean(path), nil
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(fs.Root, filepath.FromSlash(abs))
	} else {
		var err error
		abs, err = filepath.Abs(abs)
		if err != nil {
			return "", &FSError{Op: "resolve", Path: path, Err: err}
		}
	}
	rel, err := filepath.Rel(fs.Root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &FSError{Op: "resolve", Path: path, Err: ErrOutsideWorkspace}
	}
	return abs, nil
}

func (fs *WorkspaceFS) withFilesystemLock(operation string, pathA string, pathB string, fn func() error) error {
	return fs.withFilesystemLockContext(context.Background(), operation, pathA, pathB, fn)
}

func (fs *WorkspaceFS) withFilesystemLockContext(ctx context.Context, operation string, pathA string, pathB string, fn func() error) error {
	if fs == nil {
		return errors.New("workspace fs is required")
	}
	if fs.Locks == nil {
		return errors.New("workspace fs lock is required")
	}
	return fs.Locks.WithFilesystemLock(ctx, operation, pathA, pathB, fn)
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
