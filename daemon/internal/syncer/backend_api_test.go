package syncer

import "testing"

func TestWorkspaceWSPathScopesStreamRoutes(t *testing.T) {
	cfg := Config{WorkspaceID: "ws_1"}
	if got := cfg.workspaceWSPath("/ws/streams/root_1"); got != "/ws/workspaces/ws_1/streams/root_1" {
		t.Fatalf("unexpected stream ws path %q", got)
	}
	if got := cfg.workspaceWSPath("/ws/documents/doc_1"); got != "/ws/workspaces/ws_1/documents/doc_1" {
		t.Fatalf("unexpected document ws path %q", got)
	}
}
