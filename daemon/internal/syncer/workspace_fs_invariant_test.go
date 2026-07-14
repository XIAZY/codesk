package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkspaceRuntimeWorkspaceFSForRootRequiresOwnedFilesystem(t *testing.T) {
	root := t.TempDir()
	otherRoot := t.TempDir()
	runtime := &workspaceRuntime{
		replica: &workspaceReplica{
			rootDir: root,
			fs:      NewWorkspaceFS(root),
		},
	}

	fs, err := runtime.workspaceFSForRoot(root)
	if err != nil {
		t.Fatalf("resolve owned filesystem: %v", err)
	}
	if fs != runtime.replica.fs {
		t.Fatal("runtime did not return its owned filesystem")
	}

	if _, err := runtime.workspaceFSForRoot(otherRoot); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("mismatched root error = %v, want workspace filesystem invariant", err)
	}

	runtime.replica.fs = nil
	if _, err := runtime.workspaceFSForRoot(root); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("missing filesystem error = %v, want workspace filesystem invariant", err)
	}
}

func TestTrackedFileWorkspaceFSRequiresMatchingOwnedFilesystem(t *testing.T) {
	root := t.TempDir()
	tracked := &trackedFile{
		DocumentPath:  "docs/note.md",
		Path:          filepath.Join(root, "docs", "note.md"),
		WorkspaceRoot: root,
		FS:            NewWorkspaceFS(root),
	}

	fs, err := tracked.workspaceFS()
	if err != nil {
		t.Fatalf("resolve tracked filesystem: %v", err)
	}
	if fs != tracked.FS {
		t.Fatal("tracked file did not return its owned filesystem")
	}

	tracked.FS = NewWorkspaceFS(t.TempDir())
	if _, err := tracked.workspaceFS(); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("mismatched root error = %v, want workspace filesystem invariant", err)
	}

	tracked.FS = NewWorkspaceFS(root)
	tracked.Owner = &workspaceReplica{fs: NewWorkspaceFS(root)}
	if _, err := tracked.workspaceFS(); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("mismatched owner error = %v, want workspace filesystem invariant", err)
	}

	tracked.FS = nil
	tracked.Owner = nil
	if _, err := tracked.workspaceFS(); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("missing filesystem error = %v, want workspace filesystem invariant", err)
	}
}

func TestMaterializeTrackedFileRequiresOwnedFilesystem(t *testing.T) {
	root := t.TempDir()
	document := &document{ID: "doc-1", Path: "docs/note.md"}
	path := filepath.Join(root, "docs", "note.md")

	if _, err := materializeTrackedFileWithFS(context.Background(), nil, document, path, nil); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("missing filesystem error = %v, want workspace filesystem invariant", err)
	}
	if _, err := materializeTrackedFileWithFS(context.Background(), nil, document, path, NewWorkspaceFS(t.TempDir())); !errors.Is(err, errWorkspaceFSInvariant) {
		t.Fatalf("mismatched filesystem error = %v, want workspace filesystem invariant", err)
	}
}
