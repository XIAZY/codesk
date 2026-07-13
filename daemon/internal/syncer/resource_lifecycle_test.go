package syncer

import (
	"net/http"
	"testing"
)

func newTestDocumentCache(t *testing.T, path string) (*documentCache, error) {
	t.Helper()
	cache, err := newDocumentCache(path)
	if err == nil && cache != nil {
		t.Cleanup(func() {
			if err := cache.Close(); err != nil {
				t.Errorf("close document cache: %v", err)
			}
		})
	}
	return cache, err
}

func newTestWorkspaceRuntime(t *testing.T, cfg Config, client *http.Client, rootDir, actorID, actorType string) (*workspaceRuntime, error) {
	t.Helper()
	runtime, err := newWorkspaceRuntime(cfg, client, rootDir, actorID, actorType)
	if err == nil && runtime != nil {
		t.Cleanup(func() {
			if err := runtime.Close(); err != nil {
				t.Errorf("close workspace runtime: %v", err)
			}
		})
	}
	return runtime, err
}
