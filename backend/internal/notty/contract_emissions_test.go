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
