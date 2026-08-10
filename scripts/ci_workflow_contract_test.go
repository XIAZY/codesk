package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIDiffCheckUsesTrustedBranchMergeBase(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	start := strings.Index(workflow, "      - name: git diff --check against base\n")
	if start < 0 {
		t.Fatal("CI workflow has no diff-check step")
	}
	diffCheck := workflow[start:]
	for source, count := range map[string]int{
		`if [ "$GITHUB_REF" = "refs/heads/main" ]; then`: 1,
		`base="${{ github.event.before }}"`:              1,
		`git fetch --no-tags origin main`:                1,
		`base="$(git merge-base origin/main HEAD)"`:      1,
	} {
		if got := strings.Count(diffCheck, source); got != count {
			t.Errorf("CI diff-check source count for %q = %d, want %d", source, got, count)
		}
	}
	for _, source := range []string{"github.event_name", "github.event.pull_request.base.sha"} {
		if strings.Contains(diffCheck, source) {
			t.Errorf("CI diff-check retains fork-event source %q", source)
		}
	}
}
