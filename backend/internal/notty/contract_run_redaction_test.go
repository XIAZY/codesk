package notty

import (
	"net/http"
	"strings"
	"testing"
)

// Suite 3.1 of the corpus: the agent.run.updated broadcast is redacted. An agent run carries the agent's
// full systemPrompt, its full prompt, and an unbounded logTail/error — none of which may be broadcast to
// every workspace client. The broadcast path (cloneAgentRunForWorkspace → slimAgentRunPayload) strips the
// systemPrompt, summarizes the prompt to a preview, caps the logTail to the last agentRunLogPreviewLines
// (each ≤ agentRunLogLineLimit), and truncates the error to agentRunErrorLimit. This row pins that contract
// on the WS event itself, so a refactor that starts broadcasting a raw run — a real data-exposure regression
// — goes red. (The REST /workspace redaction is pinned separately by TestWorkspaceEndpointsTrimHistoricalAgentRunPayloads.)

// firstAgentRunUpdated drains buffered broker events and returns the run from the first agent.run.updated.
func firstAgentRunUpdated(t *testing.T, events <-chan EventEnvelope) *AgentRun {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event.Type != "agent.run.updated" {
				continue
			}
			run, ok := event.Data.(*AgentRun)
			if !ok {
				t.Fatalf("agent.run.updated data = %#v, want *AgentRun", event.Data)
			}
			return run
		default:
			t.Fatal("expected an agent.run.updated broadcast, got none")
			return nil
		}
	}
}

func TestContractAgentRunUpdatedBroadcastIsRedacted(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "run-redaction-owner@example.com", "owner-pass", "Run Redaction Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Run Redaction Workspace")
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Run daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, ws.ID, daemon.Token)
	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons/"+daemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "run-agent", Name: "Run Agent", Role: "Exercises run redaction", Kind: "codex",
	}, http.StatusCreated, &agent)

	events, unsubscribe := server.workspaceBroker(ws.ID).Subscribe()
	defer unsubscribe()

	// Start the run with a long prompt (human-only endpoint). The start itself broadcasts agent.run.updated.
	longPrompt := strings.Repeat("word ", 3000)
	authTestStatusWithHeaders(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/agents/"+agent.ID+"/runs", owner.Token, nil,
		StartAgentRunRequest{AgentID: agent.ID, Prompt: longPrompt}, http.StatusCreated)
	startRun := firstAgentRunUpdated(t, events)
	if startRun.SystemPrompt != "" {
		t.Fatalf("broadcast must strip systemPrompt, got %d bytes", len(startRun.SystemPrompt))
	}
	if runeCount(startRun.Prompt) > 75 { // summarizePrompt: 72-rune preview + "..."
		t.Fatalf("broadcast prompt must be summarized to a preview, got %d runes", runeCount(startRun.Prompt))
	}
	if len(startRun.Prompt) >= len(longPrompt) {
		t.Fatalf("broadcast prompt must be shorter than the full prompt")
	}

	// Update the run with an oversized logTail + error; the update also broadcasts a redacted run.
	longLine := strings.Repeat("l", 5000)
	longError := strings.Repeat("e", 5000)
	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+ws.ID+"/agent-runs/"+startRun.ID, daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentRunRequest{
		Status:  "running",
		LogTail: []string{"one", "two", "three", "four", "five", "six", "seven", longLine},
		Error:   longError,
	}, http.StatusOK)
	updRun := firstAgentRunUpdated(t, events)
	if updRun.SystemPrompt != "" {
		t.Fatalf("updated broadcast must strip systemPrompt")
	}
	if len(updRun.LogTail) > agentRunLogPreviewLines {
		t.Fatalf("broadcast logTail must be capped to %d lines, got %d", agentRunLogPreviewLines, len(updRun.LogTail))
	}
	for _, line := range updRun.LogTail {
		if runeCount(line) > agentRunLogLineLimit {
			t.Fatalf("broadcast logTail line must be capped to %d runes, got %d", agentRunLogLineLimit, runeCount(line))
		}
	}
	if runeCount(updRun.Error) > agentRunErrorLimit {
		t.Fatalf("broadcast error must be capped to %d runes, got %d", agentRunErrorLimit, runeCount(updRun.Error))
	}
}
