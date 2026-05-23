package syncer

import (
	"context"
	"testing"
)

func TestEnsureManagedStreamSyncTracksDiscoveredContentStream(t *testing.T) {
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := &Service{
		cfg:         Config{BackendURL: "http://127.0.0.1:1", AgentID: "daemon"},
		state:       state,
		streamSyncs: map[string]*managedStreamSync{},
	}

	service.ensureManagedStreamSync(ctx, "doc_a", "content")

	service.mu.Lock()
	managed := service.streamSyncs["doc_a"]
	service.mu.Unlock()
	if managed == nil {
		t.Fatal("expected managed content stream sync")
	}
	streamID, kind := managed.sync.current()
	if streamID != "doc_a" || kind != "content" {
		t.Fatalf("unexpected managed stream sync stream=%q kind=%q", streamID, kind)
	}
	managed.cancel()
}
