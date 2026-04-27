package notty

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"notty/internal/yproto"
)

func TestWorkspaceEndpointsOmitDocumentPayloads(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "sync me")

	router := server.Routes()
	for _, path := range []string{"/api/workspace", "/api/workspace/sync"} {
		payload := performJSONRequest(t, router, http.MethodGet, path, nil)
		document := findDocumentPayload(t, payload, documentID)
		if got := document["crdtState"].(string); got != "" {
			t.Fatalf("expected %s to omit CRDT state, got %d bytes", path, len(got))
		}
		if got := document["content"].(string); got != "" {
			t.Fatalf("expected %s to omit document content, got %d bytes", path, len(got))
		}
		if got := document["stateVector"].(string); got == "" {
			t.Fatalf("expected %s to retain document state vector", path)
		}
	}

	fullDocument := performJSONRequest(t, router, http.MethodGet, "/api/documents/"+documentID, nil)
	if got := fullDocument["crdtState"].(string); got == "" {
		t.Fatal("expected /api/documents/{id} to retain CRDT state for selected document bootstrap")
	}
	if got := fullDocument["content"].(string); got != "sync me" {
		t.Fatalf("expected /api/documents/{id} to retain document content, got %q", got)
	}
}

func TestDocumentSyncReturnsMissingCRDTUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")
	router := server.Routes()

	first := performJSONRequest(t, router, http.MethodPost, "/api/documents/"+documentID+"/sync", []byte(`{"stateVector":""}`))
	firstUpdate, err := base64.StdEncoding.DecodeString(first["update"].(string))
	if err != nil {
		t.Fatalf("decode first update: %v", err)
	}
	clientDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(clientDoc, firstUpdate, "initial-sync"); err != nil {
		t.Fatalf("apply initial sync: %v", err)
	}
	if got := clientDoc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("unexpected initial sync content: %q", got)
	}

	head, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document head: %v", err)
	}
	serverPeer, err := decodeCRDTState(head.CRDTState, 99)
	if err != nil {
		t.Fatalf("decode server peer: %v", err)
	}
	text := serverPeer.GetText("content")
	update := captureDocUpdate(t, serverPeer, "server-peer", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), " beta", nil)
	})
	updated, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{ActorID: "peer", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("apply server peer update: %v", err)
	}

	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(clientDoc))
	body, err := json.Marshal(DocumentSyncRequest{StateVector: stateVector})
	if err != nil {
		t.Fatalf("marshal sync request: %v", err)
	}
	second := performJSONRequest(t, router, http.MethodPost, "/api/documents/"+documentID+"/sync", body)
	secondDocument := second["document"].(map[string]any)
	if got := secondDocument["crdtState"].(string); got != "" {
		t.Fatalf("expected document sync metadata to omit CRDT state, got %d bytes", len(got))
	}
	secondUpdate, err := base64.StdEncoding.DecodeString(second["update"].(string))
	if err != nil {
		t.Fatalf("decode second update: %v", err)
	}
	if len(secondUpdate) == 0 {
		t.Fatal("expected second sync to return missing update")
	}
	if err := crdt.ApplyUpdateV1(clientDoc, secondUpdate, "delta-sync"); err != nil {
		t.Fatalf("apply delta sync: %v", err)
	}
	if got := clientDoc.GetText("content").ToString(); got != updated.Content {
		t.Fatalf("delta sync diverged: got %q want %q", got, updated.Content)
	}
}

func TestApplyUpdateWorkspaceEventOmitsFullDocumentContent(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	peerDoc, err := decodeCRDTState(document.CRDTState, 77)
	if err != nil {
		t.Fatalf("decode peer document: %v", err)
	}
	text := peerDoc.GetText("content")
	update := captureDocUpdate(t, peerDoc, "browser", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), " beta", nil)
	})
	body, err := json.Marshal(ApplyUpdateRequest{
		Update: base64.StdEncoding.EncodeToString(update),
		Meta: OperationMeta{
			ActorID:   "owner",
			ActorType: "human",
			Source:    "test",
		},
	})
	if err != nil {
		t.Fatalf("marshal apply update request: %v", err)
	}

	events, unsubscribe := server.subscribers.Subscribe()
	defer unsubscribe()
	performJSONRequest(t, server.Routes(), http.MethodPost, "/api/documents/"+documentID+"/updates", body)

	select {
	case event := <-events:
		if event.Type != "document.updated" {
			t.Fatalf("expected document.updated event, got %#v", event)
		}
		payload, ok := event.Data.(DocumentUpdateEvent)
		if !ok {
			t.Fatalf("expected document update payload, got %#v", event.Data)
		}
		if payload.Content != "" {
			t.Fatalf("expected workspace update event to omit content, got %d bytes", len(payload.Content))
		}
		if payload.Update == "" {
			t.Fatal("expected workspace update event to retain the CRDT update")
		}
	default:
		t.Fatal("expected document.updated event to be published")
	}
}

func TestWorkspaceEndpointsTrimHistoricalAgentRunPayloads(t *testing.T) {
	server, store := newTestServer(t)
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "payload-agent",
		Name:   "Payload Agent",
		Role:   "Test payload trimming",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	longPrompt := string(bytes.Repeat([]byte("x"), 5000))
	_, run, err := store.StartAgentRun(StartAgentRunRequest{
		AgentID: agent.ID,
		Prompt:  longPrompt,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	longLogLine := string(bytes.Repeat([]byte("l"), 5000))
	_, _, err = store.UpdateAgentRun(run.ID, UpdateAgentRunRequest{
		Status:  "running",
		LogTail: []string{"one", "two", "three", "four", "five", longLogLine},
		Error:   longLogLine,
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("update run log: %v", err)
	}

	router := server.Routes()
	workspaceRun := findRunPayload(t, performJSONRequest(t, router, http.MethodGet, "/api/workspace", nil), run.ID)
	if got := workspaceRun["prompt"].(string); len(got) >= len(longPrompt) {
		t.Fatalf("expected /api/workspace to trim prompt, got %d bytes", len(got))
	}
	if got := workspaceRun["systemPrompt"].(string); got != "" {
		t.Fatalf("expected /api/workspace to omit system prompt, got %q", got)
	}
	if got := workspaceRun["logTail"].([]any); len(got) != agentRunLogPreviewLines {
		t.Fatalf("expected /api/workspace to trim log tail to %d lines, got %d", agentRunLogPreviewLines, len(got))
	}
	if got := workspaceRun["error"].(string); len(got) > agentRunErrorLimit {
		t.Fatalf("expected /api/workspace to trim error, got %d bytes", len(got))
	}

	syncRun := findRunPayload(t, performJSONRequest(t, router, http.MethodGet, "/api/workspace/sync", nil), run.ID)
	if got := syncRun["prompt"].(string); got != longPrompt {
		t.Fatalf("expected /api/workspace/sync to keep active prompt, got %d bytes", len(got))
	}
	if got := syncRun["systemPrompt"].(string); got == "" {
		t.Fatal("expected /api/workspace/sync to keep active system prompt")
	}

	_, _, err = store.UpdateAgentRun(run.ID, UpdateAgentRunRequest{
		Status:        "completed",
		DesiredStatus: "stopped",
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("complete run: %v", err)
	}

	terminalSyncRun := findRunPayload(t, performJSONRequest(t, router, http.MethodGet, "/api/workspace/sync", nil), run.ID)
	if got := terminalSyncRun["prompt"].(string); len(got) >= len(longPrompt) {
		t.Fatalf("expected /api/workspace/sync to trim terminal prompt, got %d bytes", len(got))
	}
	if got := terminalSyncRun["systemPrompt"].(string); got != "" {
		t.Fatalf("expected /api/workspace/sync to omit terminal system prompt, got %q", got)
	}
	if got := terminalSyncRun["logTail"].([]any); len(got) != agentRunLogPreviewLines {
		t.Fatalf("expected /api/workspace/sync to trim terminal log tail to %d lines, got %d", agentRunLogPreviewLines, len(got))
	}
}

func TestThreadEndpointsRoundTrip(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo charlie")

	router := server.Routes()
	body, err := json.Marshal(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Question",
		Body:       "Please review this section.",
		Start:      0,
		End:        5,
	})
	if err != nil {
		t.Fatalf("marshal create thread request: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/threads?actor=owner&actor_type=human", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create thread status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create thread response: %v", err)
	}
	threadMap, ok := created["thread"].(map[string]any)
	if !ok {
		t.Fatalf("expected thread object in response, got %#v", created["thread"])
	}
	threadID, _ := threadMap["id"].(string)
	if threadID == "" {
		t.Fatalf("expected created thread id, got %#v", threadMap["id"])
	}

	fetched := performJSONRequest(t, router, http.MethodGet, "/api/threads/"+threadID, nil)
	gotThread, ok := fetched["thread"].(map[string]any)
	if !ok {
		t.Fatalf("expected fetched thread object, got %#v", fetched["thread"])
	}
	if gotThread["id"] != threadID {
		t.Fatalf("expected fetched thread %q, got %#v", threadID, gotThread["id"])
	}
}

func TestHandleDocumentProtocolMessageBroadcastsSyncUpdateToPeers(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")

	document, err := store.GetLiveDocument(documentID)
	if err != nil {
		t.Fatalf("get live document: %v", err)
	}

	peer, err := decodeCRDTState(document.CRDTState, 99)
	if err != nil {
		t.Fatalf("decode peer state: %v", err)
	}
	var update []byte
	text := peer.GetText("content")
	unsubscribe := peer.OnUpdate(func(next []byte, origin any) {
		if origin == "peer" {
			update = append([]byte(nil), next...)
		}
	})
	peer.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), " bravo", nil)
	}, "peer")
	unsubscribe()
	if len(update) == 0 {
		t.Fatal("expected peer update bytes")
	}

	room := server.rooms.ForDocument(documentID)
	source := &DocumentConn{send: make(chan []byte, 2)}
	peerConn := &DocumentConn{send: make(chan []byte, 1)}
	room.Add(source)
	room.Add(peerConn)
	defer room.Remove(source)
	defer room.Remove(peerConn)

	err = server.handleDocumentProtocolMessage(room, source, document, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	select {
	case payload := <-peerConn.send:
		messageType, reader, err := yproto.DecodeProtocolMessage(payload)
		if err != nil {
			t.Fatalf("decode broadcast message type: %v", err)
		}
		if messageType != yproto.MessageSync {
			t.Fatalf("unexpected broadcast top-level type: %d", messageType)
		}
		syncType, data, err := yproto.DecodeSyncMessage(reader)
		if err != nil {
			t.Fatalf("decode broadcast sync message: %v", err)
		}
		if syncType != yproto.SyncUpdate {
			t.Fatalf("unexpected broadcast sync type: %d", syncType)
		}
		if !bytes.Equal(data, update) {
			t.Fatal("broadcast update did not match applied update")
		}
	default:
		t.Fatal("expected peer connection to receive sync broadcast")
	}

	current, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get updated document: %v", err)
	}
	if current.Content != "alpha bravo" {
		t.Fatalf("unexpected updated content: %q", current.Content)
	}
}

func TestHandleDocumentProtocolMessageRespondsToSyncStep1(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo")

	document, err := store.GetLiveDocument(documentID)
	if err != nil {
		t.Fatalf("get live document: %v", err)
	}

	source := &DocumentConn{send: make(chan []byte, 2)}
	err = server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), source, document, yproto.BuildSyncStep1(crdt.New(crdt.WithClientID(77))), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("handle sync step1: %v", err)
	}

	for i, expected := range []uint64{yproto.MessageSync, yproto.MessageSync} {
		select {
		case payload := <-source.send:
			messageType, _, err := yproto.DecodeProtocolMessage(payload)
			if err != nil {
				t.Fatalf("decode handshake message %d: %v", i, err)
			}
			if messageType != expected {
				t.Fatalf("unexpected handshake top-level type %d: %d", i, messageType)
			}
		default:
			t.Fatalf("expected handshake message %d to be queued", i)
		}
	}
}

func TestHandleDocumentProtocolMessageReconnectMergesServerAndClientEdits(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "base")

	document, err := store.GetLiveDocument(documentID)
	if err != nil {
		t.Fatalf("get live document: %v", err)
	}

	clientDoc, err := decodeCRDTState(document.CRDTState, 101)
	if err != nil {
		t.Fatalf("decode client document: %v", err)
	}
	serverPeerDoc, err := decodeCRDTState(document.CRDTState, 202)
	if err != nil {
		t.Fatalf("decode server peer document: %v", err)
	}

	serverText := serverPeerDoc.GetText("content")
	serverUpdate := captureDocUpdate(t, serverPeerDoc, "server-peer", func(txn *crdt.Transaction) {
		serverText.Insert(txn, serverText.Len(), " server", nil)
	})
	room := server.rooms.ForDocument(documentID)
	serverPeer := &DocumentConn{send: make(chan []byte, 4)}
	if err := server.handleDocumentProtocolMessage(room, serverPeer, document, yproto.BuildSyncUpdate(serverUpdate), OperationMeta{
		ActorID:   "server-peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("apply server-side edit: %v", err)
	}

	clientText := clientDoc.GetText("content")
	captureDocUpdate(t, clientDoc, "client-local", func(txn *crdt.Transaction) {
		clientText.Insert(txn, clientText.Len(), " client", nil)
	})

	reconnected := &DocumentConn{send: make(chan []byte, 4)}
	if err := server.handleDocumentProtocolMessage(room, reconnected, document, yproto.BuildSyncStep1(clientDoc), OperationMeta{
		ActorID:   "client",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("start reconnect sync: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case payload := <-reconnected.send:
			reply := applySyncPayloadToDoc(t, clientDoc, payload, "server-reconnect")
			if len(reply) > 0 {
				if err := server.handleDocumentProtocolMessage(room, reconnected, document, reply, OperationMeta{
					ActorID:   "client",
					ActorType: "human",
					Source:    "test",
				}); err != nil {
					t.Fatalf("apply client reconnect reply: %v", err)
				}
			}
		default:
			t.Fatalf("expected reconnect handshake message %d", i)
		}
	}

	current, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get converged document: %v", err)
	}
	clientContent := clientDoc.GetText("content").ToString()
	if current.Content != clientContent {
		t.Fatalf("expected server and client to converge, server=%q client=%q", current.Content, clientContent)
	}
	if !strings.Contains(current.Content, "server") || !strings.Contains(current.Content, "client") {
		t.Fatalf("expected converged content to include both edits, got %q", current.Content)
	}
}

func TestHandleDocumentProtocolMessagePublishesMentionMetadataChange(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	if _, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	document, err := store.GetLiveDocument(documentID)
	if err != nil {
		t.Fatalf("get live document: %v", err)
	}
	frontendDoc, err := decodeCRDTState(document.CRDTState, 303)
	if err != nil {
		t.Fatalf("decode frontend document: %v", err)
	}
	text := frontendDoc.GetText("content")
	update := captureDocUpdate(t, frontendDoc, "browser", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), "Ping @codex-agent.\n", nil)
	})

	events, unsubscribe := server.subscribers.Subscribe()
	defer unsubscribe()

	source := &DocumentConn{send: make(chan []byte, 4)}
	if err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), source, document, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "ws",
	}); err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	select {
	case event := <-events:
		if event.Type != "document.mentions.updated" {
			t.Fatalf("expected mention metadata event, got %#v", event)
		}
	default:
		t.Fatal("expected mention metadata change to be published")
	}
}

func applySyncPayloadToDoc(t *testing.T, doc *crdt.Doc, payload []byte, origin any) []byte {
	t.Helper()
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode sync payload: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("expected sync payload, got message type %d", messageType)
	}
	reply, _, err := yproto.ReadSyncMessage(reader, doc, origin)
	if err != nil {
		t.Fatalf("apply sync payload: %v", err)
	}
	return reply
}

func newTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return NewServer(Config{}, store), store
}

func performJSONRequest(t *testing.T, handler http.Handler, method, target string, body []byte) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d, body = %s", method, target, recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s %s response: %v", method, target, err)
	}
	return payload
}

func findRunPayload(t *testing.T, payload map[string]any, runID string) map[string]any {
	t.Helper()
	rawRuns, ok := payload["agentRuns"].([]any)
	if !ok {
		t.Fatalf("expected agentRuns array, got %#v", payload["agentRuns"])
	}
	for _, rawRun := range rawRuns {
		run, ok := rawRun.(map[string]any)
		if !ok {
			t.Fatalf("expected run object, got %#v", rawRun)
		}
		if run["id"] == runID {
			return run
		}
	}
	t.Fatalf("run %q not found in %#v", runID, rawRuns)
	return nil
}

func findDocumentPayload(t *testing.T, payload map[string]any, documentID string) map[string]any {
	t.Helper()
	rawDocuments, ok := payload["documents"].([]any)
	if !ok {
		t.Fatalf("expected documents array, got %#v", payload["documents"])
	}
	for _, rawDocument := range rawDocuments {
		document, ok := rawDocument.(map[string]any)
		if !ok {
			t.Fatalf("expected document object, got %#v", rawDocument)
		}
		if document["id"] == documentID {
			return document
		}
	}
	t.Fatalf("document %q not found in %#v", documentID, rawDocuments)
	return nil
}
