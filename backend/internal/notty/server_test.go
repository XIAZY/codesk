package notty

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/reearth/ygo/crdt"
	"notty/internal/yproto"
)

func TestWorkspaceEndpointsOmitDocumentPayloads(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "sync me")

	router := server.Routes()
	payload := performJSONRequest(t, router, http.MethodGet, "/api/workspace", nil)
	document := findDocumentPayload(t, payload, documentID)
	if _, ok := document["crdtState"]; ok {
		t.Fatal("expected /api/workspace document metadata to omit crdtState")
	}
	if _, ok := document["content"]; ok {
		t.Fatal("expected /api/workspace document metadata to omit content")
	}
	if got := document["stateVector"].(string); got == "" {
		t.Fatal("expected /api/workspace to retain document state vector")
	}

	byPath := performJSONRequest(t, router, http.MethodGet, "/api/documents/by-path?path=docs/spec.md", nil)
	byPathDocument := byPath["document"].(map[string]any)
	if _, ok := byPathDocument["crdtState"]; ok {
		t.Fatal("expected /api/documents/by-path document metadata to omit crdtState")
	}
	if _, ok := byPathDocument["content"]; ok {
		t.Fatal("expected /api/documents/by-path document metadata to omit content")
	}

	assertNonOK(t, router, http.MethodGet, "/api/workspace/sync", nil)
	assertNonOK(t, router, http.MethodGet, "/api/documents", nil)
	assertNonOK(t, router, http.MethodGet, "/api/documents/"+documentID, nil)
	assertNonOK(t, router, http.MethodPost, "/api/documents/"+documentID+"/sync", []byte(`{"stateVector":""}`))
	assertNonOK(t, router, http.MethodPost, "/api/documents/"+documentID+"/updates", []byte(`{"update":""}`))
}

func TestDocumentProtocolSyncReturnsMissingCRDTUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")
	room := server.rooms.ForDocument(documentID)

	clientDoc := crdt.New()
	clientConn := &DocumentConn{send: make(chan []byte, 4)}
	syncClientFromServer(t, server, room, clientConn, documentID, clientDoc)
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

	syncClientFromServer(t, server, room, clientConn, documentID, clientDoc)
	if got := clientDoc.GetText("content").ToString(); got != updated.Content {
		t.Fatalf("delta sync diverged: got %q want %q", got, updated.Content)
	}
}

func TestDocumentProtocolUpdatePublishesMetadataOnlyWorkspaceEvent(t *testing.T) {
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

	events, unsubscribe := server.subscribers.Subscribe()
	defer unsubscribe()
	source := &DocumentConn{send: make(chan []byte, 4)}
	if err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	select {
	case event := <-events:
		if event.Type != "document.updated" {
			t.Fatalf("expected document.updated event, got %#v", event)
		}
		payload, ok := event.Data.(DocumentUpdateEvent)
		if !ok {
			t.Fatalf("expected document update payload, got %#v", event.Data)
		}
		if payload.Update != "" {
			t.Fatalf("expected workspace update event to omit raw CRDT update, got %d bytes", len(payload.Update))
		}
		if payload.UpdateID == 0 || payload.StateVector == "" {
			t.Fatalf("expected workspace update event to keep metadata, got %#v", payload)
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

	_, _, err = store.UpdateAgentRun(run.ID, UpdateAgentRunRequest{
		Status:        "completed",
		DesiredStatus: "stopped",
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("complete run: %v", err)
	}

	terminalRun := findRunPayload(t, performJSONRequest(t, router, http.MethodGet, "/api/workspace", nil), run.ID)
	if got := terminalRun["prompt"].(string); len(got) >= len(longPrompt) {
		t.Fatalf("expected /api/workspace to trim terminal prompt, got %d bytes", len(got))
	}
	if got := terminalRun["systemPrompt"].(string); got != "" {
		t.Fatalf("expected /api/workspace to omit terminal system prompt, got %q", got)
	}
	if got := terminalRun["logTail"].([]any); len(got) != agentRunLogPreviewLines {
		t.Fatalf("expected /api/workspace to trim terminal log tail to %d lines, got %d", agentRunLogPreviewLines, len(got))
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

	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
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

	err = server.handleDocumentProtocolMessage(room, source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
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

func TestHandleDocumentProtocolMessageBroadcastsDeleteOnlySyncUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/backspace.md", "abc")

	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	peer, err := decodeCRDTState(document.CRDTState, 99)
	if err != nil {
		t.Fatalf("decode peer state: %v", err)
	}
	text := peer.GetText("content")

	room := server.rooms.ForDocument(documentID)
	source := &DocumentConn{send: make(chan []byte, 2)}
	peerConn := &DocumentConn{send: make(chan []byte, 2)}
	room.Add(source)
	room.Add(peerConn)
	defer room.Remove(source)
	defer room.Remove(peerConn)

	insertUpdate := captureDocUpdate(t, peer, "peer-insert", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), "d", nil)
	})
	if err := server.handleDocumentProtocolMessage(room, source, documentID, yproto.BuildSyncUpdate(insertUpdate), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("handle insert document protocol message: %v", err)
	}
	select {
	case <-peerConn.send:
	default:
		t.Fatal("expected peer connection to receive insert sync broadcast")
	}

	deleteUpdate := captureDocUpdate(t, peer, "peer-delete", func(txn *crdt.Transaction) {
		text.Delete(txn, text.Len()-1, 1)
	})
	if err := server.handleDocumentProtocolMessage(room, source, documentID, yproto.BuildSyncUpdate(deleteUpdate), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("handle delete-only document protocol message: %v", err)
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
		if !bytes.Equal(data, deleteUpdate) {
			t.Fatal("broadcast delete update did not match applied update")
		}
	default:
		t.Fatal("expected peer connection to receive delete-only sync broadcast")
	}

	current, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get updated document: %v", err)
	}
	if current.Content != "abc" {
		t.Fatalf("unexpected updated content after delete-only update: %q", current.Content)
	}
}

func TestHandleDocumentProtocolMessageIgnoresDuplicateSyncUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/duplicate.md", "alpha")

	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	peer, err := decodeCRDTState(document.CRDTState, 99)
	if err != nil {
		t.Fatalf("decode peer state: %v", err)
	}
	text := peer.GetText("content")
	update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), " bravo", nil)
	})

	room := server.rooms.ForDocument(documentID)
	source := &DocumentConn{send: make(chan []byte, 4)}
	peerConn := &DocumentConn{send: make(chan []byte, 4)}
	room.Add(source)
	room.Add(peerConn)
	defer room.Remove(source)
	defer room.Remove(peerConn)

	if err := server.handleDocumentProtocolMessage(room, source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("apply first update: %v", err)
	}
	select {
	case <-peerConn.send:
	default:
		t.Fatal("expected first update to broadcast")
	}
	afterFirst, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get updated document: %v", err)
	}

	if err := server.handleDocumentProtocolMessage(room, source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("apply duplicate update: %v", err)
	}
	select {
	case payload := <-peerConn.send:
		t.Fatalf("duplicate update should not broadcast, got %d bytes", len(payload))
	default:
	}
	afterDuplicate, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get duplicate document: %v", err)
	}
	if afterDuplicate.UpdateID != afterFirst.UpdateID {
		t.Fatalf("duplicate update changed version: before=%d after=%d", afterFirst.UpdateID, afterDuplicate.UpdateID)
	}
	if afterDuplicate.Content != afterFirst.Content {
		t.Fatalf("duplicate update changed content: before=%q after=%q", afterFirst.Content, afterDuplicate.Content)
	}
}

func TestHandleDocumentProtocolMessageRespondsToSyncStep1(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo")

	source := &DocumentConn{send: make(chan []byte, 2)}
	err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), source, documentID, yproto.BuildSyncStep1(crdt.New(crdt.WithClientID(77))), OperationMeta{
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

	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
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
	if err := server.handleDocumentProtocolMessage(room, serverPeer, documentID, yproto.BuildSyncUpdate(serverUpdate), OperationMeta{
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
	if err := server.handleDocumentProtocolMessage(room, reconnected, documentID, yproto.BuildSyncStep1(clientDoc), OperationMeta{
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
				if err := server.handleDocumentProtocolMessage(room, reconnected, documentID, reply, OperationMeta{
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

func TestHandleDocumentProtocolMessageConcurrentSyncAndUpdates(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/race.md", "base")
	initial, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	room := server.rooms.ForDocument(documentID)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer, err := decodeCRDTState(initial.CRDTState, uint64(300+worker))
			if err != nil {
				errs <- err
				return
			}
			text := peer.GetText("content")
			conn := &DocumentConn{send: make(chan []byte, 1024)}
			for i := 0; i < 40; i++ {
				update := captureDocUpdate(t, peer, "writer", func(txn *crdt.Transaction) {
					text.Insert(txn, text.Len(), "x", nil)
				})
				if err := server.handleDocumentProtocolMessage(room, conn, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
					ActorID:   "writer",
					ActorType: "human",
					Source:    "test",
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer := crdt.New(crdt.WithClientID(crdt.ClientID(700 + worker)))
			conn := &DocumentConn{send: make(chan []byte, 1024)}
			for i := 0; i < 40; i++ {
				if err := server.handleDocumentProtocolMessage(room, conn, documentID, yproto.BuildSyncStep1(peer), OperationMeta{
					ActorID:   "syncer",
					ActorType: "human",
					Source:    "test",
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent protocol handling failed: %v", err)
		}
	}
}

func TestHandleDocumentProtocolMessageDoesNotPublishDocumentMentionMetadataChange(t *testing.T) {
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
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
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
	if err := server.handleDocumentProtocolMessage(server.rooms.ForDocument(documentID), source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "ws",
	}); err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	sawDocumentUpdate := false
	for {
		select {
		case event := <-events:
			if event.Type == "document.mentions.updated" {
				t.Fatalf("document text mentions must not publish metadata change event: %#v", event)
			}
			if event.Type == "document.updated" {
				sawDocumentUpdate = true
			}
		default:
			if !sawDocumentUpdate {
				t.Fatal("expected document update event to be published")
			}
			return
		}
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

func syncClientFromServer(t *testing.T, server *Server, room *DocumentRoom, conn *DocumentConn, documentID string, doc *crdt.Doc) {
	t.Helper()
	if err := server.handleDocumentProtocolMessage(room, conn, documentID, yproto.BuildSyncStep1(doc), OperationMeta{
		ActorID:   "client",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("start protocol sync: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case payload := <-conn.send:
			reply := applySyncPayloadToDoc(t, doc, payload, "server-sync")
			if len(reply) > 0 {
				if err := server.handleDocumentProtocolMessage(room, conn, documentID, reply, OperationMeta{
					ActorID:   "client",
					ActorType: "human",
					Source:    "test",
				}); err != nil {
					t.Fatalf("apply protocol sync reply: %v", err)
				}
			}
		default:
			t.Fatalf("expected protocol sync message %d", i)
		}
	}
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

func assertNonOK(t *testing.T, handler http.Handler, method, target string, body []byte) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		t.Fatalf("expected %s %s to be unavailable, got status 200 body=%s", method, target, recorder.Body.String())
	}
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
