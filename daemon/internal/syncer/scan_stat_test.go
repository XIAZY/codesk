package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSameStatTupleIncludesFileKey(t *testing.T) {
	cached := FileStat{
		Kind:      FileKindFile,
		FileKey:   "dev:1",
		SizeBytes: 10,
		Mode:      0o644,
		MTimeNS:   100,
		CTimeNS:   200,
		StatValid: true,
	}
	current := cached
	caps := ScanCapabilities{FileKeyReliable: true, CTimeReliable: true}
	if !SameStatTuple(cached, current, caps) {
		t.Fatal("expected identical reliable stat tuple to match")
	}
	current.FileKey = "dev:2"
	if SameStatTuple(cached, current, caps) {
		t.Fatal("FileKey change must invalidate stat equality")
	}
	current = cached
	current.FileKey = ""
	if SameStatTuple(cached, current, caps) {
		t.Fatal("missing FileKey must invalidate stat equality")
	}
}

func TestSameStatTupleRequiresReliableFileKey(t *testing.T) {
	cached := FileStat{
		Kind:      FileKindFile,
		FileKey:   "dev:1",
		SizeBytes: 10,
		Mode:      0o644,
		MTimeNS:   100,
		CTimeNS:   200,
		StatValid: true,
	}
	if SameStatTuple(cached, cached, ScanCapabilities{FileKeyReliable: false}) {
		t.Fatal("unreliable FileKey must disable content stat short-circuiting")
	}
	current := cached
	current.CTimeNS = cached.CTimeNS + 1
	if !SameStatTuple(cached, current, ScanCapabilities{FileKeyReliable: true, CTimeReliable: false}) {
		t.Fatal("ctime difference should be ignored when ctime is unreliable")
	}
	if SameStatTuple(cached, current, ScanCapabilities{FileKeyReliable: true, CTimeReliable: true}) {
		t.Fatal("ctime difference should invalidate equality when ctime is reliable")
	}
}

func TestWorkspaceFSStatAndFileKeyReliabilityProbe(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	stat, err := fs.Stat(context.Background(), path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if !stat.Exists || !stat.StatValid || stat.Kind != FileKindFile || stat.SizeBytes != int64(len("content")) {
		t.Fatalf("unexpected file stat: %#v", stat)
	}
	if stat.FileKey == "" {
		t.Fatal("expected local filesystem to expose a FileKey")
	}
	if !fs.TestFileKeyReliability(context.Background()) {
		t.Fatal("expected FileKey reliability probe to pass on local filesystem")
	}
}

func TestWorkspaceFSDirectoryMTimeReliabilityProbe(t *testing.T) {
	fs := NewWorkspaceFS(t.TempDir())
	if !fs.TestDirectoryMTimeReliability(context.Background()) {
		t.Fatal("expected directory mtime reliability probe to pass on local filesystem")
	}
}

func TestWorkspaceFSCTimeReliabilityProbe(t *testing.T) {
	fs := NewWorkspaceFS(t.TempDir())
	if !fs.TestCTimeReliability(context.Background()) {
		t.Fatal("expected ctime reliability probe to pass on local filesystem")
	}
}
