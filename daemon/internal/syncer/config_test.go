package syncer

import "testing"

func TestLoadConfigDoesNotInventAgentID(t *testing.T) {
	t.Setenv("NOTTY_AGENT_ID", "")

	cfg := LoadConfig()
	if cfg.AgentID != "" {
		t.Fatalf("expected empty default agent id, got %q", cfg.AgentID)
	}

	t.Setenv("NOTTY_AGENT_ID", "agent_explicit")
	cfg = LoadConfig()
	if cfg.AgentID != "agent_explicit" {
		t.Fatalf("expected explicit agent id override, got %q", cfg.AgentID)
	}
}
