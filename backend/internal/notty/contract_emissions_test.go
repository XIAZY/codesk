package notty

import (
	"net/http"
	"testing"
)

// Suite 3.2 of the corpus: broker emission counts. A workspace mutation must broadcast its change exactly
// once — a missing broadcast leaves other clients stale, a double broadcast is a storm — and a REFUSED
// mutation must broadcast nothing. This row pins the daemon lifecycle (create → 1 daemon.created, check-in
// → 1 daemon.updated, delete → 1 daemon.deleted, a refused delete → 0), mirroring the #95 anchor pattern
// now applied to the daemon endpoints that predate it.

// drainEventTypeCounts non-blockingly drains all buffered broker events and tallies them by type.
func drainEventTypeCounts(events <-chan EventEnvelope) map[string]int {
	counts := map[string]int{}
	for {
		select {
		case event := <-events:
			counts[event.Type]++
		default:
			return counts
		}
	}
}

func TestContractDaemonLifecycleEmitExactlyOncePerChange(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "emissions-owner@example.com", "owner-pass", "Emissions Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Emissions Workspace")

	events, unsubscribe := server.workspaceBroker(ws.ID).Subscribe()
	defer unsubscribe()

	// create → exactly one daemon.created
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Emissions daemon"}, http.StatusCreated, &daemon)
	if got := drainEventTypeCounts(events); got["daemon.created"] != 1 {
		t.Fatalf("daemon create must broadcast exactly one daemon.created, got %d (all: %v)", got["daemon.created"], got)
	}

	// check-in (status report) → exactly one daemon.updated
	authTestReportCodexRuntime(t, router, ws.ID, daemon.Token)
	if got := drainEventTypeCounts(events); got["daemon.updated"] != 1 {
		t.Fatalf("daemon check-in must broadcast exactly one daemon.updated, got %d (all: %v)", got["daemon.updated"], got)
	}

	// delete → exactly one daemon.deleted
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+ws.ID+"/daemons/"+daemon.Daemon.ID, owner.Token, nil, http.StatusOK)
	if got := drainEventTypeCounts(events); got["daemon.deleted"] != 1 {
		t.Fatalf("daemon delete must broadcast exactly one daemon.deleted, got %d (all: %v)", got["daemon.deleted"], got)
	}

	// refused: deleting the now-gone daemon changes nothing and must broadcast nothing.
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+ws.ID+"/daemons/"+daemon.Daemon.ID, owner.Token, nil, http.StatusNotFound)
	if got := drainEventTypeCounts(events); len(got) != 0 {
		t.Fatalf("a refused delete must broadcast nothing, got %v", got)
	}
}

func TestContractAgentLifecycleEmitExactlyOncePerChange(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "emissions-agent-owner@example.com", "owner-pass", "Emissions Agent Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Emissions Agent Workspace")
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Agent-emissions daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, ws.ID, daemon.Token)

	// Subscribe after the daemon is set up so only the agent events are under test.
	events, unsubscribe := server.workspaceBroker(ws.ID).Subscribe()
	defer unsubscribe()

	// create → exactly one agent.created
	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons/"+daemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "emissions-agent", Name: "Emissions Agent", Role: "Exercises emission counts", Kind: "codex",
	}, http.StatusCreated, &agent)
	if got := drainEventTypeCounts(events); got["agent.created"] != 1 {
		t.Fatalf("agent create must broadcast exactly one agent.created, got %d (all: %v)", got["agent.created"], got)
	}

	// session update → exactly one agent.updated
	authTestStatusWithHeaders(t, router, http.MethodPatch, "/api/workspaces/"+ws.ID+"/agents/"+agent.ID+"/session", daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, UpdateAgentSessionRequest{Status: "idle", SessionID: "emissions-session"}, http.StatusOK)
	if got := drainEventTypeCounts(events); got["agent.updated"] != 1 {
		t.Fatalf("agent session update must broadcast exactly one agent.updated, got %d (all: %v)", got["agent.updated"], got)
	}

	// delete → exactly one agent.deleted
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+ws.ID+"/agents/"+agent.ID, owner.Token, nil, http.StatusOK)
	if got := drainEventTypeCounts(events); got["agent.deleted"] != 1 {
		t.Fatalf("agent delete must broadcast exactly one agent.deleted, got %d (all: %v)", got["agent.deleted"], got)
	}

	// refused: deleting the now-gone agent must broadcast nothing.
	authTestStatus(t, router, http.MethodDelete, "/api/workspaces/"+ws.ID+"/agents/"+agent.ID, owner.Token, nil, http.StatusNotFound)
	if got := drainEventTypeCounts(events); len(got) != 0 {
		t.Fatalf("a refused agent delete must broadcast nothing, got %v", got)
	}
}

// TestContractPresenceUpsertEmitsOncePerCall pins presence's emission contract. Unlike the anchor/thread
// endpoints, presence has no no-op suppression (server_collaboration.go always publishes after an upsert),
// so its invariant is exactly one presence.updated per call — pinned here so a future change that either
// drops the broadcast or starts double-publishing is caught.
func TestContractPresenceUpsertEmitsOncePerCall(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "emissions-presence-owner@example.com", "owner-pass", "Emissions Presence Owner")
	ws := authTestCreateWorkspace(t, router, owner.Token, "Emissions Presence Workspace")
	doc := authTestCreateDocument(t, router, owner.Token, ws.ID, "docs/presence.md", "# Presence\n")
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Presence daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, ws.ID, daemon.Token)

	events, unsubscribe := server.workspaceBroker(ws.ID).Subscribe()
	defer unsubscribe()

	upsert := func() {
		var presence Presence
		authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+ws.ID+"/presence", daemon.Token, UpsertPresenceRequest{
			ActorID: daemon.Daemon.ID, ActorType: "daemon", DocumentID: doc.ID, Activity: "viewing",
		}, http.StatusOK, &presence)
	}

	upsert()
	if got := drainEventTypeCounts(events); got["presence.updated"] != 1 {
		t.Fatalf("first presence upsert must broadcast exactly one presence.updated, got %d (all: %v)", got["presence.updated"], got)
	}
	// A second identical upsert still broadcasts exactly once (presence is not no-op-suppressed by design).
	upsert()
	if got := drainEventTypeCounts(events); got["presence.updated"] != 1 {
		t.Fatalf("a repeated presence upsert must broadcast exactly one presence.updated per call, got %d (all: %v)", got["presence.updated"], got)
	}
}
