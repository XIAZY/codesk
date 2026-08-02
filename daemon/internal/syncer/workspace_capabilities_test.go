package syncer

import (
	"context"
	"testing"
)

func TestWorkspaceRuntimeRefreshesReverseWindowCapability(t *testing.T) {
	runtime := &workspaceRuntime{}
	if runtime.supportsWorkspaceCapability(documentTombstoneReverseWindowV1) {
		t.Fatal("missing capability defaulted to enabled")
	}
	if err := runtime.applyWorkspace(context.Background(), &workspaceResponse{
		Capabilities: []string{"unrelatedV1", documentTombstoneReverseWindowV1},
	}); err != nil {
		t.Fatalf("apply supported workspace: %v", err)
	}
	if !runtime.supportsWorkspaceCapability(documentTombstoneReverseWindowV1) {
		t.Fatal("advertised reverse-window capability was not enabled")
	}
	if err := runtime.applyWorkspace(context.Background(), &workspaceResponse{}); err != nil {
		t.Fatalf("apply downgraded workspace: %v", err)
	}
	if runtime.supportsWorkspaceCapability(documentTombstoneReverseWindowV1) {
		t.Fatal("missing capability did not disable the cached reverse-window gate")
	}
}
