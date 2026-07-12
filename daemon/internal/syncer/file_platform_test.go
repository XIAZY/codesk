package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileIdentityTracksHardLinks(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original.md")
	linked := filepath.Join(dir, "linked.md")
	distinct := filepath.Join(dir, "distinct.md")
	if err := os.WriteFile(original, []byte("original"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := os.Link(original, linked); err != nil {
		t.Fatalf("create hard link: %v", err)
	}
	if err := os.WriteFile(distinct, []byte("distinct"), 0o644); err != nil {
		t.Fatalf("write distinct: %v", err)
	}

	originalIdentity := fileIdentityForPath(original)
	linkedIdentity := fileIdentityForPath(linked)
	distinctIdentity := fileIdentityForPath(distinct)
	if !originalIdentity.valid || !linkedIdentity.valid || !distinctIdentity.valid {
		t.Fatalf("expected valid identities: original=%+v linked=%+v distinct=%+v", originalIdentity, linkedIdentity, distinctIdentity)
	}
	if !sameFileIdentity(originalIdentity, linkedIdentity) {
		t.Fatalf("hard links should have the same identity: original=%+v linked=%+v", originalIdentity, linkedIdentity)
	}
	if sameFileIdentity(originalIdentity, distinctIdentity) {
		t.Fatalf("distinct files should not have the same identity: original=%+v distinct=%+v", originalIdentity, distinctIdentity)
	}
}

func TestStatFileWithIdentityReturnsMetadataAndStableIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	content := []byte("content")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	info, identity, err := statFileWithIdentity(path)
	if err != nil {
		t.Fatalf("stat file with identity: %v", err)
	}
	if info == nil || info.IsDir() || info.Size() != int64(len(content)) {
		t.Fatalf("unexpected file metadata: %+v", info)
	}
	if !identity.valid {
		t.Fatalf("expected valid identity: %+v", identity)
	}
}

func TestReplaceFileAtomicallyReplacesContentAndCleansStagingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("before"), 0o640); err != nil {
		t.Fatalf("write original: %v", err)
	}

	if err := replaceFileAtomically(path, "after", 0o640); err != nil {
		t.Fatalf("replace file: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replaced file: %v", err)
	}
	if string(content) != "after" {
		t.Fatalf("replaced content mismatch: got %q want %q", content, "after")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".doc.md.*.tmp"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("replacement left staging files: %v", matches)
	}
}

func TestReplaceFileAtomicallyCreatesDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")

	if err := replaceFileAtomically(path, "created", 0o640); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(content) != "created" {
		t.Fatalf("created content mismatch: got %q want %q", content, "created")
	}
}
