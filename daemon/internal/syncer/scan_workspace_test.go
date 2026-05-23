package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceFSScanFullStatOnlySkipsNottyAndHonorsBudget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeScanTestFile(t, root, "docs/a.md", "alpha")
	writeScanTestFile(t, root, "docs/nested/b.md", "bravo")
	writeScanTestFile(t, root, "z.md", "zulu")
	writeScanTestFile(t, root, ".notty/secret.md", "internal")

	fs := NewWorkspaceFS(root)
	scan, err := fs.Scan(ctx, ScanOptions{
		StatOnly: true,
		Capabilities: ScanCapabilities{
			DirectoryMTimeReliable: false,
			FileKeyReliable:        true,
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, path := range []string{"docs/a.md", "docs/nested/b.md", "z.md"} {
		snap, ok := scan.Files[path]
		if !ok {
			t.Fatalf("expected scanned file %s in %#v", path, scan.Files)
		}
		if len(snap.Bytes) != 0 || isKnownProjectedHash(snap.Hash) {
			t.Fatalf("stat-only scan should not read bytes for %s", path)
		}
		if !snap.Stat.Exists || snap.Stat.Path != path {
			t.Fatalf("expected relative stat for %s, got %#v", path, snap.Stat)
		}
	}
	if _, ok := scan.Dirs["docs"]; !ok {
		t.Fatal("expected docs dir")
	}
	if _, ok := scan.Dirs["docs/nested"]; !ok {
		t.Fatal("expected nested dir")
	}
	if _, ok := scan.Files[".notty/secret.md"]; ok {
		t.Fatal("scan should skip .notty contents")
	}

	budgeted, err := fs.Scan(ctx, ScanOptions{
		StatOnly: true,
		Budget:   ScanBudget{MaxPaths: 1},
		Capabilities: ScanCapabilities{
			DirectoryMTimeReliable: false,
			FileKeyReliable:        true,
		},
	})
	if err != nil {
		t.Fatalf("budgeted scan: %v", err)
	}
	if !budgeted.Incomplete || budgeted.CursorPath == "" {
		t.Fatalf("expected budgeted scan to be incomplete with cursor, got %#v", budgeted)
	}
}

func TestWorkspaceFSScanHintsStatImmediateChildrenAndMissingPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeScanTestFile(t, root, "docs/a.md", "alpha")
	writeScanTestFile(t, root, "docs/sub/c.md", "charlie")
	writeScanTestFile(t, root, ".notty/ignored.md", "internal")

	fs := NewWorkspaceFS(root)
	scan, err := fs.Scan(ctx, ScanOptions{
		Hints: []ScanHint{
			{Kind: ScanHintDir, Path: "docs", Reason: "test"},
			{Kind: ScanHintPath, Path: "missing.md", Reason: "test"},
			{Kind: ScanHintPath, Path: ".notty/ignored.md", Reason: "test"},
		},
		StatOnly: true,
		Capabilities: ScanCapabilities{
			DirectoryMTimeReliable: true,
			FileKeyReliable:        true,
		},
	})
	if err != nil {
		t.Fatalf("hint scan: %v", err)
	}
	if _, ok := scan.Files["docs/a.md"]; !ok {
		t.Fatalf("expected docs/a.md from dir hint, got %#v", scan.Files)
	}
	if _, ok := scan.Dirs["docs/sub"]; !ok {
		t.Fatalf("expected docs/sub from dir hint, got %#v", scan.Dirs)
	}
	if _, ok := scan.Files["docs/sub/c.md"]; ok {
		t.Fatal("dir hint should only stat immediate children")
	}
	if _, ok := scan.Missing["missing.md"]; !ok {
		t.Fatalf("expected missing.md in missing set, got %#v", scan.Missing)
	}
	if _, ok := scan.Missing[".notty/ignored.md"]; ok {
		t.Fatal("scan should ignore .notty hints")
	}
}

func TestWorkspaceFSScanDirectoryCacheRequiresReliableDirectoryMTime(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeScanTestFile(t, root, "docs/a.md", "alpha")
	writeScanTestFile(t, root, "docs/b.md", "bravo")

	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	fs := NewWorkspaceFS(root)
	fs.State = state

	current, err := fs.Stat(ctx, "docs")
	if err != nil {
		t.Fatalf("stat docs: %v", err)
	}
	if err := state.StoreDirectoryScanCache(ctx, "docs", current.MTimeNS, current.CTimeNS, []string{"a.md"}); err != nil {
		t.Fatalf("store cache: %v", err)
	}

	cached, err := fs.Scan(ctx, ScanOptions{
		Hints:       []ScanHint{{Kind: ScanHintDir, Path: "docs", Reason: "cache-test"}},
		StatOnly:    true,
		UseDirCache: true,
		Capabilities: ScanCapabilities{
			DirectoryMTimeReliable: true,
		},
	})
	if err != nil {
		t.Fatalf("cached scan: %v", err)
	}
	if _, ok := cached.Files["docs/a.md"]; !ok {
		t.Fatalf("expected cached child docs/a.md, got %#v", cached.Files)
	}
	if _, ok := cached.Files["docs/b.md"]; ok {
		t.Fatal("expected reliable dir cache to avoid fresh readdir")
	}

	fresh, err := fs.Scan(ctx, ScanOptions{
		Hints:       []ScanHint{{Kind: ScanHintDir, Path: "docs", Reason: "cache-test"}},
		StatOnly:    true,
		UseDirCache: true,
		Capabilities: ScanCapabilities{
			DirectoryMTimeReliable: false,
		},
	})
	if err != nil {
		t.Fatalf("fresh scan: %v", err)
	}
	if _, ok := fresh.Files["docs/a.md"]; !ok {
		t.Fatal("fresh scan should include docs/a.md")
	}
	if _, ok := fresh.Files["docs/b.md"]; !ok {
		t.Fatal("unreliable directory mtime should force fresh readdir including docs/b.md")
	}
}

func TestWorkspaceFSScanClearsFileKeyWhenUnreliable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeScanTestFile(t, root, "doc.md", "content")
	fs := NewWorkspaceFS(root)

	raw, err := fs.Stat(ctx, "doc.md")
	if err != nil {
		t.Fatalf("stat doc: %v", err)
	}
	if raw.FileKey == "" {
		t.Skip("filesystem did not expose a file key")
	}
	scan, err := fs.Scan(ctx, ScanOptions{
		Hints:    []ScanHint{{Kind: ScanHintPath, Path: "doc.md", Reason: "test"}},
		StatOnly: true,
		Capabilities: ScanCapabilities{
			FileKeyReliable: false,
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := scan.Files["doc.md"].Stat.FileKey; got != "" {
		t.Fatalf("expected unreliable file key to be cleared, got %q", got)
	}
}

func writeScanTestFile(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
