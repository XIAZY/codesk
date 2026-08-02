package notty

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestWorkspaceSnapshotsAdvertiseReverseWindowCapability(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	want := []string{"documentTombstoneReverseWindowV1"}

	var rest struct {
		Capabilities []string `json:"capabilities"`
	}
	authTestJSON(t, fixture.router, http.MethodGet, fixture.workspaceAPIPath("/workspace"), fixture.token, nil, http.StatusOK, &rest)
	if !reflect.DeepEqual(rest.Capabilities, want) {
		t.Fatalf("REST capabilities = %#v, want %#v", rest.Capabilities, want)
	}

	httpServer := httptest.NewServer(fixture.server.Routes())
	defer httpServer.Close()
	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath(""), fixture.token)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	var event struct {
		Type string `json:"type"`
		Data struct {
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read workspace snapshot: %v", err)
	}
	if event.Type != "workspace.snapshot" {
		t.Fatalf("event type = %q, want workspace.snapshot", event.Type)
	}
	if !reflect.DeepEqual(event.Data.Capabilities, want) {
		t.Fatalf("websocket capabilities = %#v, want %#v", event.Data.Capabilities, want)
	}
}
