package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWorkspaceFSAppendUsesCanonicalPathLease(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	fs := NewWorkspaceFS(root)
	if err := fs.Append(path, ""); err != nil {
		t.Fatalf("initialize path lease: %v", err)
	}
	const writers = 24
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fs.Append(path, "complete-record\n"); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read appended file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != writers+1 || lines[0] != "base" {
		t.Fatalf("unexpected append records: count=%d content=%q", len(lines), content)
	}
	for _, line := range lines[1:] {
		if line != "complete-record" {
			t.Fatalf("partial append record: %q", line)
		}
	}
}

func TestWorkspaceFSCreateEmptyOrReadCreatesAbsentFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "doc.md")
	snapshot, err := NewWorkspaceFS(root).CreateEmptyOrRead(path)
	if err != nil {
		t.Fatalf("create or read: %v", err)
	}
	if !snapshot.Exists || snapshot.Path != path || len(snapshot.Bytes) != 0 {
		t.Fatalf("unexpected created snapshot: %+v", snapshot)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if len(content) != 0 {
		t.Fatalf("created file should be empty, got %q", content)
	}
}

func TestWorkspaceFSCreateEmptyOrReadPreservesExistingFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	local := []byte("local edit\n")
	if err := os.WriteFile(path, local, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	snapshot, err := NewWorkspaceFS(root).CreateEmptyOrRead(path)
	if err != nil {
		t.Fatalf("create or read: %v", err)
	}
	if string(snapshot.Bytes) != string(local) {
		t.Fatalf("existing local content was not preserved: got %q want %q", snapshot.Bytes, local)
	}
}

func TestWorkspaceFSCreateEmptyOrReadPreservesFileCreatedAfterAbsentObservation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	fs := NewWorkspaceFS(root)
	before, err := fs.Read(path)
	if err != nil {
		t.Fatalf("observe absent path: %v", err)
	}
	if before.Exists {
		t.Fatalf("path unexpectedly existed: %+v", before)
	}
	local := []byte("created by local editor\n")
	if err := os.WriteFile(path, local, 0o644); err != nil {
		t.Fatalf("create local file after observation: %v", err)
	}
	after, err := fs.CreateEmptyOrRead(path)
	if err != nil {
		t.Fatalf("create or read after local create: %v", err)
	}
	if string(after.Bytes) != string(local) {
		t.Fatalf("local file was overwritten: got %q want %q", after.Bytes, local)
	}
}

func TestWorkspaceFSCreateEmptyOrReadDoesNotRemoveReplacementAfterCloseFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	replacement := filepath.Join(root, "replacement.md")
	local := []byte("local replacement\n")
	if err := os.WriteFile(replacement, local, 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	wantErr := errors.New("injected close failure")
	createThenFail := func(candidate string) error {
		file, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("create empty file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close created file: %v", err)
		}
		if err := os.Remove(candidate); err != nil {
			t.Fatalf("external remove created path: %v", err)
		}
		if err := os.Rename(replacement, candidate); err != nil {
			t.Fatalf("external replacement: %v", err)
		}
		return wantErr
	}

	if _, err := NewWorkspaceFS(root).createEmptyOrRead(path, createThenFail); !errors.Is(err, wantErr) {
		t.Fatalf("expected close failure, got %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read local replacement: %v", err)
	}
	if string(content) != string(local) {
		t.Fatalf("local replacement was removed or changed: got %q want %q", content, local)
	}
}

func TestWorkspaceFSCreateEmptyOrReadReturnsReplacementAfterCreate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	replacement := filepath.Join(root, "replacement.md")
	local := []byte("local replacement\n")
	if err := os.WriteFile(replacement, local, 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	createThenReplace := func(candidate string) error {
		file, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("create empty file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close created file: %v", err)
		}
		if err := os.Remove(candidate); err != nil {
			t.Fatalf("external remove created path: %v", err)
		}
		if err := os.Rename(replacement, candidate); err != nil {
			t.Fatalf("external replacement: %v", err)
		}
		return nil
	}

	snapshot, err := NewWorkspaceFS(root).createEmptyOrRead(path, createThenReplace)
	if err != nil {
		t.Fatalf("create empty or read: %v", err)
	}
	if string(snapshot.Bytes) != string(local) || snapshot.Hash != projectedHashBytes(local) {
		t.Fatalf("replacement was not returned: snapshot=%+v want=%q", snapshot, local)
	}
}

func TestScanWorkspaceFilesObservesCompleteContentDuringAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	oldContent := strings.Repeat("old-content\n", 4096)
	newContent := strings.Repeat("new-content\n", 4096)
	if err := os.WriteFile(path, []byte(oldContent), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	fs := NewWorkspaceFS(root)
	writerDone := make(chan error, 1)
	go func() {
		current := oldContent
		for i := 0; i < 40; i++ {
			next := newContent
			if current == newContent {
				next = oldContent
			}
			if err := fs.WriteIfUnchanged(path, projectedHashString(current), []byte(next)); err != nil {
				writerDone <- err
				return
			}
			current = next
		}
		writerDone <- nil
	}()

	for i := 0; i < 80; i++ {
		files, err := scanWorkspaceFiles(root)
		if err != nil {
			t.Fatalf("scan during replacement: %v", err)
		}
		got := files[path]
		if got != oldContent && got != newContent {
			t.Fatalf("scan observed partial content: %d bytes", len(got))
		}
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("atomic writer: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".doc.md.*.tmp"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("atomic replacement stranded staging files: %v", matches)
	}
}

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
