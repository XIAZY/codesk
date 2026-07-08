package notty

import (
	"net/http"
	"testing"
)

// Suite 4.4 of the corpus (wire-half): deleting a daemon does NOT destroy the agents it hosted. Daemon
// deletion is a soft delete (status=deleted + DeletedAt), so the agent rows survive — the host is gone but
// its agents (and their run/event history) remain, ready to be re-homed on reinstall. This row pins that
// survival: after the daemon is deleted, the agent is still present in the workspace and the daemon itself
// is no longer an active host. A regression that hard-deleted the daemon (firing fk_agents_daemon's
// ON DELETE SET NULL) or cascade-deleted its agents — silently losing agent history on every reinstall —
// goes red. (Token rotation on reinstall is pinned by TestDaemonReinstallTokenRotatesDaemonToken.)
func TestContractDaemonDeleteDisconnectsAgents(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "disconnect-owner@example.com", "owner-pass", "Disconnect Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Disconnect Workspace")
	wsAPI := "/api/workspaces/" + ws.ID

	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, wsAPI+"/daemons", owner.Token, CreateDaemonRequest{Name: "Host daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, ws.ID, daemon.Token)
	var agent Agent
	authTestJSON(t, router, http.MethodPost, wsAPI+"/daemons/"+daemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "hosted-agent", Name: "Hosted Agent", Role: "hosted by the daemon", Kind: "codex",
	}, http.StatusCreated, &agent)

	// Sanity: before the delete the agent is bound to its daemon.
	if got := findWorkspaceAgent(t, router, owner.Token, ws.ID, agent.ID); got.DaemonID != daemon.Daemon.ID {
		t.Fatalf("agent should start bound to its daemon, got daemonId %q", got.DaemonID)
	}

	// Delete the daemon (soft delete).
	authTestStatus(t, router, http.MethodDelete, wsAPI+"/daemons/"+daemon.Daemon.ID, owner.Token, nil, http.StatusOK)

	// The agent survives its daemon's deletion — not cascade-removed, so its history is preserved.
	if got := findWorkspaceAgent(t, router, owner.Token, ws.ID, agent.ID); got == nil {
		t.Fatalf("agent must survive its daemon's soft deletion (agents + history are not destroyed on delete)")
	}

	// The daemon is no longer an active host: it is either gone from the daemons list or marked deleted.
	if d := findWorkspaceDaemon(t, router, owner.Token, ws.ID, daemon.Daemon.ID); d != nil && d.Status != "deleted" {
		t.Fatalf("deleted daemon must not remain an active host, got status %q", d.Status)
	}
}

// findWorkspaceDaemon returns the daemon with the given id from GET /workspace, or nil if absent.
func findWorkspaceDaemon(t *testing.T, router http.Handler, token, workspaceID, daemonID string) *Daemon {
	t.Helper()
	var payload struct {
		Daemons []Daemon `json:"daemons"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspaceID+"/workspace", token, nil, http.StatusOK, &payload)
	for i := range payload.Daemons {
		if payload.Daemons[i].ID == daemonID {
			return &payload.Daemons[i]
		}
	}
	return nil
}

// findWorkspaceAgent returns the agent with the given id from GET /workspace, or nil if absent.
func findWorkspaceAgent(t *testing.T, router http.Handler, token, workspaceID, agentID string) *Agent {
	t.Helper()
	var payload struct {
		Agents []Agent `json:"agents"`
	}
	authTestJSON(t, router, http.MethodGet, "/api/workspaces/"+workspaceID+"/workspace", token, nil, http.StatusOK, &payload)
	for i := range payload.Agents {
		if payload.Agents[i].ID == agentID {
			return &payload.Agents[i]
		}
	}
	return nil
}
