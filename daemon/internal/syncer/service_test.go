package syncer

import (
	"path/filepath"
	"testing"
)

func TestComputeReplaceFindsInnerSpan(t *testing.T) {
	op := computeReplace("hello world", "hello brave world")
	if op != (replaceOp{Start: 6, End: 6, Text: "brave "}) {
		t.Fatalf("unexpected op: %#v", op)
	}
}

func TestComputeReplaceHandlesReplacement(t *testing.T) {
	op := computeReplace("alpha beta gamma", "alpha zeta gamma")
	if op.Start != 6 || op.End != 7 || op.Text != "z" {
		t.Fatalf("unexpected op: %#v", op)
	}
}

func TestIgnoredWorkspacePathPolicy(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		path    string
		ignored bool
	}{
		{path: filepath.Join(root, ".notty", "codex-agent.log"), ignored: true},
		{path: filepath.Join(root, ".env"), ignored: true},
		{path: filepath.Join(root, "docs", ".cache", "state.bin"), ignored: true},
		{path: filepath.Join(root, "docs", ".draft.md"), ignored: true},
		{path: filepath.Join(root, "docs", "spec.md"), ignored: false},
		{path: root, ignored: false},
		{path: filepath.Dir(root), ignored: true},
	}
	for _, tc := range cases {
		if got := isIgnoredWorkspaceAbsolutePath(root, tc.path); got != tc.ignored {
			t.Fatalf("isIgnoredWorkspaceAbsolutePath(%q) = %v, want %v", tc.path, got, tc.ignored)
		}
	}
}
