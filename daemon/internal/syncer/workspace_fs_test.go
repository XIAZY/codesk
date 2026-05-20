package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceFSWriteIfUnchangedRefusesDivergedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	fs := NewWorkspaceFS(root)
	if err := fs.WriteIfUnchanged(path, projectedHashString("other"), []byte("remote")); !errors.Is(err, ErrDivergedWorkingCopy) {
		t.Fatalf("expected diverged error, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read path: %v", err)
	}
	if string(got) != "base" {
		t.Fatalf("expected original content to remain, got %q", got)
	}
}

func TestWorkspaceFSDeleteIfUnchangedRefusesDirtyFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}
	fs := NewWorkspaceFS(root)
	if err := fs.DeleteIfUnchanged(path, projectedHashString("clean")); !errors.Is(err, ErrUnsafeDelete) {
		t.Fatalf("expected unsafe delete, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dirty file should remain: %v", err)
	}
}

func TestWorkspaceFSMoveIfNoTargetPreservesBytesAndRefusesCollision(t *testing.T) {
	root := t.TempDir()
	from := filepath.Join(root, "from.md")
	to := filepath.Join(root, "nested", "to.md")
	if err := os.WriteFile(from, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	fs := NewWorkspaceFS(root)
	if err := fs.MoveIfNoTarget(from, to); err != nil {
		t.Fatalf("move: %v", err)
	}
	got, err := os.ReadFile(to)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "bytes" {
		t.Fatalf("target content mismatch: %q", got)
	}
	if err := os.WriteFile(from, []byte("again"), 0o644); err != nil {
		t.Fatalf("write source again: %v", err)
	}
	if err := fs.MoveIfNoTarget(from, to); !errors.Is(err, ErrPathCollision) {
		t.Fatalf("expected collision, got %v", err)
	}
}

func TestWorkspaceFSArchiveUsesRenameWithinWorkspace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("recover me"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	fs := NewWorkspaceFS(root)
	archivePath, err := fs.Archive(path, "test-reason")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archivePath == "" {
		t.Fatal("expected archive path")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source should be moved away, stat err=%v", err)
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(got) != "recover me" {
		t.Fatalf("archive content mismatch: %q", got)
	}
}

func TestWorkspaceFSRejectsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	fs := NewWorkspaceFS(root)
	if _, err := fs.Read(outside); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("expected outside workspace error, got %v", err)
	}
}
