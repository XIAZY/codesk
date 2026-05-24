package syncer

import (
	"encoding/json"
	"testing"
)

func TestStreamUpdatedContentEventDoesNotRefreshOrWakeAgents(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"streamId": "doc_a",
		"kind":     "content",
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	event := workspaceEventEnvelope{Type: "stream.updated", Data: payload}
	if shouldRefreshForWorkspaceEvent(event) {
		t.Fatal("content stream update should not trigger broad workspace refresh")
	}
	if shouldWakeAgentWorkersForWorkspaceEvent(event) {
		t.Fatal("content stream update should not wake all agent workers")
	}
}

func TestStreamUpdatedRootEventRefreshesWithoutWakingAgents(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"streamId": "root_a",
		"kind":     "root",
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	event := workspaceEventEnvelope{Type: "stream.updated", Data: payload}
	if !shouldRefreshForWorkspaceEvent(event) {
		t.Fatal("root stream update should trigger workspace refresh")
	}
	if shouldWakeAgentWorkersForWorkspaceEvent(event) {
		t.Fatal("root stream update should not wake all agent workers directly")
	}
}
