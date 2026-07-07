package notty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIResponsesNeverNullCollectionFields(t *testing.T) {
	_, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "nullcheck-owner@example.com", "owner-pass", "Null Check Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Null Check Workspace")

	workspaceRecorder := authTestRequest(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/workspace", owner.Token, nil, nil)
	if workspaceRecorder.Code != http.StatusOK {
		t.Fatalf("GET /workspace: %d body=%s", workspaceRecorder.Code, workspaceRecorder.Body.String())
	}
	workspacePayload := decodeRawObject(t, workspaceRecorder.Body.Bytes())
	assertJSONFieldsNotNull(t, workspacePayload, "GET /workspace",
		"users",
		"daemons",
		"agents",
		"agentRuns",
		"threads",
		"agentEvents",
		"presences",
		"activities",
	)

	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	conn := dialDocumentWebsocketForTest(t, httpServer.URL, "/ws/workspaces/"+workspace.ID, owner.Token)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	_, snapshotMessage, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read workspace snapshot: %v", err)
	}
	snapshotPayload := decodeEventData(t, snapshotMessage, "workspace.snapshot")
	assertJSONFieldsNotNull(t, snapshotPayload, "workspace.snapshot",
		"users",
		"daemons",
		"agents",
		"agentRuns",
		"threads",
		"agentEvents",
		"presences",
		"activities",
	)
	if err := conn.Close(); err != nil {
		t.Fatalf("close snapshot websocket: %v", err)
	}

	document := authTestCreateDocument(t, router, owner.Token, workspace.ID, "empty.md", "")
	threadsRecorder := authTestRequest(t, router, http.MethodGet, "/api/workspaces/"+workspace.ID+"/documents/"+document.ID+"/threads", owner.Token, nil, nil)
	if threadsRecorder.Code != http.StatusOK {
		t.Fatalf("GET /documents/{id}/threads: %d body=%s", threadsRecorder.Code, threadsRecorder.Body.String())
	}
	threadsPayload := decodeRawObject(t, threadsRecorder.Body.Bytes())
	assertJSONFieldsNotNull(t, threadsPayload, "GET /documents/{id}/threads", "threads")

	presenceConn := dialDocumentWebsocketForTest(t, httpServer.URL, "/ws/workspaces/"+workspace.ID, owner.Token)
	defer presenceConn.Close()
	if err := presenceConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set presence websocket read deadline: %v", err)
	}
	if _, _, err := presenceConn.ReadMessage(); err != nil {
		t.Fatalf("read presence websocket snapshot: %v", err)
	}

	presenceRecorder := authTestRequest(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/presence", owner.Token, nil, UpsertPresenceRequest{
		ActorID:   "spoofed",
		ActorType: "agent",
		Mode:      "editing",
		Activity:  "checking null contract",
	})
	if presenceRecorder.Code != http.StatusOK {
		t.Fatalf("POST /presence: %d body=%s", presenceRecorder.Code, presenceRecorder.Body.String())
	}
	presencePayload := decodeRawObject(t, presenceRecorder.Body.Bytes())
	assertJSONFieldsNotNull(t, presencePayload, "POST /presence", "selection")

	_, eventMessage, err := presenceConn.ReadMessage()
	if err != nil {
		t.Fatalf("read presence event: %v", err)
	}
	presenceEventPayload := decodeEventData(t, eventMessage, "presence.updated")
	assertJSONFieldsNotNull(t, presenceEventPayload, "presence.updated", "selection")
}

func decodeRawObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode JSON object: %v body=%s", err, string(raw))
	}
	return obj
}

func decodeEventData(t *testing.T, raw []byte, wantType string) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode event envelope: %v body=%s", err, string(raw))
	}
	if envelope.Type != wantType {
		t.Fatalf("event type = %q, want %q body=%s", envelope.Type, wantType, string(raw))
	}
	return decodeRawObject(t, envelope.Data)
}

func assertJSONFieldsNotNull(t *testing.T, obj map[string]json.RawMessage, surface string, fields ...string) {
	t.Helper()
	for _, field := range fields {
		raw, ok := obj[field]
		if !ok {
			t.Errorf("%s field %q missing", surface, field)
			continue
		}
		if string(raw) == "null" {
			t.Errorf("%s field %q is JSON null; must serialize as [] or {}", surface, field)
		}
	}
}
